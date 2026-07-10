package llmorchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/mcp"
	"banking-voice-ai-agent/internal/ollama"
	"banking-voice-ai-agent/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type SessionState string

const (
	StateIdle                SessionState = "IDLE"
	StateStreaming           SessionState = "STREAMING"
	StateCacheIntercept      SessionState = "CACHE_INTERCEPT"
	StateAwaitingConfirmation SessionState = "AWAITING_CONFIRMATION"
)

// ConfirmationContext holds details for a pending write transaction
type ConfirmationContext struct {
	Intent      string         `json:"intent"`
	ToolName    string         `json:"tool_name"`
	Args        map[string]any `json:"args"`
	UniqueRefNo string         `json:"unique_ref_no"`
}

type TurnSupervisor struct {
	Redis     *db.RedisManager
	Qdrant    *db.QdrantManager
	Cassandra *db.CassandraManager
	Ollama    *ollama.Client
	MCP       *mcp.BankingMCPServer

	// Configurations
	ExtremeThreshold float64
	NormalThreshold  float64
	WarmingEnabled   bool // Load-shedding knob

	// Active turns lock
	mu sync.Mutex
}

func NewTurnSupervisor(r *db.RedisManager, q *db.QdrantManager, o *ollama.Client, m *mcp.BankingMCPServer, c *db.CassandraManager) *TurnSupervisor {
	return &TurnSupervisor{
		Redis:            r,
		Qdrant:           q,
		Cassandra:        c,
		Ollama:           o,
		MCP:              m,
		ExtremeThreshold: 0.96, // Cosine score >= 0.96 for halt
		NormalThreshold:  0.88, // Cosine score >= 0.88 for final dispatch
		WarmingEnabled:   true, // enabled by default
	}
}

func (s *TurnSupervisor) UpdateConfig(warmingEnabled bool, extreme float64, normal float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WarmingEnabled = warmingEnabled
	s.ExtremeThreshold = extreme
	s.NormalThreshold = normal
}

func (s *TurnSupervisor) IsWarmingEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.WarmingEnabled
}

func (s *TurnSupervisor) GetExtremeThreshold() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ExtremeThreshold
}

func (s *TurnSupervisor) GetNormalThreshold() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.NormalThreshold
}


// LogEvent helper to write to Go log and Redis Streams (Kafka stand-in)
func (s *TurnSupervisor) LogEvent(ctx context.Context, turnID string, eventName string, payload map[string]any) {
	log.Printf("[EVENT] %s %v", eventName, payload)
	// Write asynchronously to Redis stream
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Redis.AddAuditLog(bgCtx, turnID, eventName, payload)
	}()
}

// HandleStablePartial executes Branch A (speculative prefill) and Branch B (cache probe) in parallel.
// If Branch B matches with extreme confidence, it halts Branch A.
func (s *TurnSupervisor) HandleStablePartial(ctx context.Context, turnID string, sessionID string, partialText string, cancelWarm context.CancelFunc, warmDoneChan <-chan struct{}) (bool, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	partialLen := len(strings.Fields(partialText))
	if partialLen == 0 {
		return false, nil
	}

	// 1. Branch A: speculative prefill (Ollama runs in background).
	// Locally we log that we extend the warm state.
	if s.WarmingEnabled {
		s.LogEvent(ctx, turnID, "warm_start", map[string]any{
			"turn_id":               turnID,
			"at_token":              partialLen,
			"static_prefix_reused":  true, // System prompt prefix matches
			"text":                  partialText,
		})
	} else {
		// Log that warming is shed due to pressure knob
		s.LogEvent(ctx, turnID, "warm_shed", map[string]any{
			"turn_id":  turnID,
			"at_token": partialLen,
			"reason":   "GPU utilization watermark exceeded / manual override",
		})
	}

	// 2. Branch B: cache probe (Embed text and search Qdrant).
	embCtx, embCancel := context.WithTimeout(ctx, 1*time.Second)
	defer embCancel()

	emb, err := s.Ollama.GetEmbedding(embCtx, partialText)
	if err != nil {
		log.Printf("Embedding error on partial '%s': %v", partialText, err)
		return false, nil
	}

	// Query both indexes in parallel
	var actionMatches, faqMatches []db.QdrantMatch
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if res, err := s.Qdrant.Search(ctx, "action_intents", emb, 1); err == nil {
			actionMatches = res
		}
	}()

	go func() {
		defer wg.Done()
		if res, err := s.Qdrant.Search(ctx, "faq_items", emb, 1); err == nil {
			faqMatches = res
		}
	}()

	wg.Wait()

	var bestActionScore float64 = 0.0
	var bestFAQScore float64 = 0.0
	var matchedAction map[string]any

	if len(actionMatches) > 0 {
		bestActionScore = actionMatches[0].Score
		matchedAction = actionMatches[0].Payload
	}
	if len(faqMatches) > 0 {
		bestFAQScore = faqMatches[0].Score
	}

	s.LogEvent(ctx, turnID, "cache_probe", map[string]any{
		"turn_id":           turnID,
		"partial_len":       partialLen,
		"best_action_score": bestActionScore,
		"best_faq_score":    bestFAQScore,
	})

	// Check for extreme-confidence action match
	if bestActionScore >= s.ExtremeThreshold {
		intent := matchedAction["intent"].(string)

		s.LogEvent(ctx, turnID, "halt_point", map[string]any{
			"turn_id":       turnID,
			"at_token":      partialLen,
			"action_intent": intent,
			"score":         bestActionScore,
			"EXTREME":       s.ExtremeThreshold,
			"message":       "HALT FIRED: speculative LLM prefill aborted",
		})

		// Halt Branch A
		if cancelWarm != nil {
			cancelWarm()
		}

		s.LogEvent(ctx, turnID, "warm_outcome", map[string]any{
			"turn_id":              turnID,
			"prefill_tokens":       partialLen,
			"used":                 false,
			"discarded":            true,
			"would_have_reclaimed": s.WarmingEnabled,
		})

		return true, matchedAction
	}

	return false, nil
}

// HandleFinalTranscript executes the final dispatch logic: action > FAQ > LLM.
func (s *TurnSupervisor) HandleFinalTranscript(ctx context.Context, turnID string, sessionID string, userID string, finalTranscript string, intercepted bool, interceptedPayload map[string]any) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Start OTel turn span
	ctx, span := telemetry.Tracer("orchestrator").Start(ctx, "turn",
		trace.WithAttributes(
			attribute.String("turn_id", turnID),
			attribute.String("session_id", sessionID),
			attribute.String("user_id", userID),
		))
	defer span.End()

	log.Printf("[Supervisor] Handling final transcript: '%s' (intercepted: %t)", finalTranscript, intercepted)

	// Fetch conversation history from Redis
	history, err := s.Redis.GetSessionContext(ctx, sessionID)
	if err != nil {
		log.Printf("Failed to load history from Redis: %v", err)
	}

	// 1. Reconcile if we had an early halt
	if intercepted && interceptedPayload != nil {
		// Embed final and verify score >= NORMAL
		emb, err := s.Ollama.GetEmbedding(ctx, finalTranscript)
		if err == nil {
			actionMatches, err := s.Qdrant.Search(ctx, "action_intents", emb, 1)
			if err == nil && len(actionMatches) > 0 {
				finalScore := actionMatches[0].Score
				if finalScore >= s.NormalThreshold {
					// Confirmed match
					s.LogEvent(ctx, turnID, "final_reconcile", map[string]any{
						"turn_id":   turnID,
						"state":     "intercept",
						"diverged":  false,
						"intent":    interceptedPayload["intent"],
						"score":     finalScore,
						"NORMAL":    s.NormalThreshold,
					})

					return s.executeCommitPath(ctx, turnID, sessionID, userID, interceptedPayload, finalTranscript, history)
				}
			}
		}

		// Diverged! The final transcript did not match the intercepted intent
		s.LogEvent(ctx, turnID, "final_reconcile", map[string]any{
			"turn_id":  turnID,
			"state":    "intercept",
			"diverged": true,
			"message":  "FINAL Diverged: restarting cold prefill on LLM",
		})
		// Proceed to standard dispatch with final transcript
	} else {
		s.LogEvent(ctx, turnID, "final_reconcile", map[string]any{
			"turn_id":  turnID,
			"state":    "no_intercept",
			"diverged": false,
		})
	}

	// 2. Standard Dispatch Flow: embed final and search collections
	emb, err := s.Ollama.GetEmbedding(ctx, finalTranscript)
	if err != nil {
		return "", "", fmt.Errorf("failed to embed final transcript: %w", err)
	}

	var actionMatches, faqMatches []db.QdrantMatch
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if res, err := s.Qdrant.Search(ctx, "action_intents", emb, 1); err == nil {
			actionMatches = res
		}
	}()

	go func() {
		defer wg.Done()
		if res, err := s.Qdrant.Search(ctx, "faq_items", emb, 1); err == nil {
			faqMatches = res
		}
	}()

	wg.Wait()

	// Precedence 1: Action Intent Match >= NORMAL
	if len(actionMatches) > 0 && actionMatches[0].Score >= s.NormalThreshold {
		payload := actionMatches[0].Payload
		intent := payload["intent"].(string)

		s.LogEvent(ctx, turnID, "dispatch", map[string]any{
			"turn_id":        turnID,
			"path":           "action",
			"matched_intent": intent,
			"score":          actionMatches[0].Score,
		})

		recordTurn(ctx, "action")
		return s.executeCommitPath(ctx, turnID, sessionID, userID, payload, finalTranscript, history)
	}

	// Precedence 2: FAQ Match >= NORMAL
	if len(faqMatches) > 0 && faqMatches[0].Score >= s.NormalThreshold {
		payload := faqMatches[0].Payload
		answer := payload["answer"].(string)

		s.LogEvent(ctx, turnID, "dispatch", map[string]any{
			"turn_id":        turnID,
			"path":           "faq",
			"matched_intent": "faq_item",
			"score":          faqMatches[0].Score,
		})

		recordTurn(ctx, "faq")
		// FAQs are static and safe. No private info is contained, so it passes guardrails.
		return "text", answer, nil
	}

	// Precedence 3: LLM Fallthrough (MISS)
	s.LogEvent(ctx, turnID, "dispatch", map[string]any{
		"turn_id": turnID,
		"path":    "llm",
	})

	recordTurn(ctx, "llm")

	if s.WarmingEnabled {
		s.LogEvent(ctx, turnID, "warm_outcome", map[string]any{
			"turn_id":        turnID,
			"prefill_tokens": len(strings.Fields(finalTranscript)),
			"used":           true,
			"discarded":      false,
		})
	}

	// Fallback to Ollama safe-deflector prompt
	response, err := s.runLLMDeflector(ctx, sessionID, finalTranscript, history)
	if err != nil {
		return "", "", err
	}

	// Run output guardrail filter to block un-sourced values
	safeText := s.ApplyOutputGuardrailFilter(response, "")

	return "text", safeText, nil
}

// executeCommitPath handles executing the action path or scheduling confirmation
func (s *TurnSupervisor) executeCommitPath(ctx context.Context, turnID string, sessionID string, userID string, actionPayload map[string]any, finalTranscript string, history []ollama.ChatMessage) (string, string, error) {
	intent := actionPayload["intent"].(string)
	bankAction := actionPayload["bank_action"].(string)
	responseTemplate := actionPayload["response_template"].(string)

	// Check if this action requires confirmation (mutating action)
	if intent == "transfer" || intent == "block_card" {
		// Generate parameters from transcript (simple parsing for scaffold)
		args := map[string]any{"user_id": userID}

		if intent == "transfer" {
			args["to"] = extractAccountNo(finalTranscript)
			args["amount"] = extractAmount(finalTranscript)
			// Generate unique ref number
			args["unique_ref_no"] = fmt.Sprintf("REF-%s-%d", sessionID, time.Now().UnixNano())
		} else if intent == "block_card" {
			args["card"] = "credit" // default
			if strings.Contains(strings.ToLower(finalTranscript), "debit") {
				args["card"] = "debit"
			}
		}

		// Store confirmation context in Redis (simulating session state/saga checkpoint)
		confCtx := ConfirmationContext{
			Intent:      intent,
			ToolName:    bankAction,
			Args:        args,
			UniqueRefNo: args["unique_ref_no"].(string),
		}

		confBytes, _ := json.Marshal(confCtx)
		_ = s.Redis.Client.Set(ctx, fmt.Sprintf("session:%s:confirm", sessionID), confBytes, 15*time.Minute).Err()

		var promptText string
		isHindi := isHindiText(finalTranscript)

		if intent == "transfer" {
			amountStr := fmt.Sprintf("%.2f", args["amount"])
			if args["amount"].(float64) <= 0 {
				if isHindi {
					promptText = "पैसे ट्रांसफर करने के लिए, कृपया एक मान्य राशि और गंतव्य खाता संख्या बताएं। उदाहरण के लिए: 987654 खाते में 500 रुपये भेजें।"
				} else {
					promptText = "To transfer money, please specify a valid amount and destination account number. For example: transfer 500 to account 987654."
				}
				// Clear the state since args were invalid
				_ = s.Redis.Client.Del(ctx, fmt.Sprintf("session:%s:confirm", sessionID))
				return "text", promptText, nil
			}

			if isHindi {
				promptText = fmt.Sprintf("कृपया पुष्टि करें: क्या आप खाता %s में %s रुपये ट्रांसफर करना चाहते हैं? (हाँ/ना)", args["to"], amountStr)
			} else {
				promptText = fmt.Sprintf("Please confirm: Do you want to transfer %s INR to account %s? (yes/no)", amountStr, args["to"])
			}
		} else {
			cardType := args["card"].(string)
			if isHindi {
				if cardType == "credit" {
					cardType = "क्रेडिट"
				} else {
					cardType = "डेबिट"
				}
				promptText = fmt.Sprintf("कृपया पुष्टि करें: क्या आप अपने %s कार्ड को ब्लॉक करना चाहते हैं? (हाँ/ना)", cardType)
			} else {
				promptText = fmt.Sprintf("Please confirm: Do you want to block your %s card? (yes/no)", cardType)
			}
		}

		return "confirm_required", promptText, nil
	}

	// Read-only action, execute immediately
	args := map[string]any{"user_id": userID}
	mcpRes, err := s.MCP.CallTool(ctx, bankAction, args)
	if err != nil {
		return "text", "I'm sorry, I encountered an issue retrieving your bank details. Let me connect you with a representative.", nil
	}

	// Parse tool output to inject into template
	var mcpData map[string]any
	_ = json.Unmarshal([]byte(mcpRes), &mcpData)

	responseText := responseTemplate
	if intent == "balance" {
		balanceStr := fmt.Sprintf("%.2f %s", mcpData["balance"].(float64), mcpData["currency"].(string))
		responseText = strings.ReplaceAll(responseText, "{{balance}}", balanceStr)
	} else if intent == "transactions" {
		responseText = strings.ReplaceAll(responseText, "{{transactions}}", mcpData["text"].(string))
	} else if intent == "due_date" {
		responseText = strings.ReplaceAll(responseText, "{{due_date}}", mcpData["due_date"].(string))
	}

	// Verify guardrail
	safeText := s.ApplyOutputGuardrailFilter(responseText, mcpRes)

	return "text", safeText, nil
}

// HandleConfirmation processes the user response to a pending transaction confirmation
func (s *TurnSupervisor) HandleConfirmation(ctx context.Context, turnID string, sessionID string, userID string, confirmationText string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	confirmKey := fmt.Sprintf("session:%s:confirm", sessionID)
	data, err := s.Redis.Client.Get(ctx, confirmKey).Result()
	if err != nil {
		return "No transaction is currently awaiting confirmation.", nil
	}
	defer s.Redis.Client.Del(ctx, confirmKey) // clean up state

	var conf ConfirmationContext
	if err := json.Unmarshal([]byte(data), &conf); err != nil {
		return "Failed to parse confirmation context.", err
	}

	confirmNormalized := strings.ToLower(strings.TrimSpace(confirmationText))
	isConfirmed := strings.Contains(confirmNormalized, "yes") ||
		strings.Contains(confirmNormalized, "confirm") ||
		strings.Contains(confirmNormalized, "correct") ||
		strings.Contains(confirmNormalized, "sure") ||
		strings.Contains(confirmNormalized, "yup") ||
		strings.Contains(confirmNormalized, "ok") ||
		strings.Contains(confirmNormalized, "sahi") ||
		strings.Contains(confirmNormalized, "thik") ||
		strings.Contains(confirmNormalized, "haan") ||
		strings.Contains(confirmNormalized, "han") ||
		strings.Contains(confirmNormalized, "हाँ") ||
		strings.Contains(confirmNormalized, "हा")

	isHindi := isHindiText(confirmationText)

	if !isConfirmed {
		s.LogEvent(ctx, turnID, "confirmation_outcome", map[string]any{
			"turn_id":       turnID,
			"intent":        conf.Intent,
			"confirmed":     false,
			"unique_ref_no": conf.UniqueRefNo,
		})
		if isHindi {
			return "लेनदेन (transaction) रद्द कर दिया गया है।", nil
		}
		return "Transaction cancelled.", nil
	}

	s.LogEvent(ctx, turnID, "confirmation_outcome", map[string]any{
		"turn_id":       turnID,
		"intent":        conf.Intent,
		"confirmed":     true,
		"unique_ref_no": conf.UniqueRefNo,
	})

	// Execute mutating action
	mcpRes, err := s.MCP.CallTool(ctx, conf.ToolName, conf.Args)
	if err != nil {
		log.Printf("MCP tool call error during confirmation: %v", err)
		if isHindi {
			return "मुझे खेद है, मैं इस समय ट्रांजैक्शन पूरा नहीं कर सका। कृपया फिर से प्रयास करें या ग्राहक सेवा प्रतिनिधि से बात करें।", nil
		}
		return "I'm sorry, I could not complete the transaction at this moment. Please try again or speak with a representative.", nil
	}

	var mcpData map[string]any
	_ = json.Unmarshal([]byte(mcpRes), &mcpData)

	if isHindi {
		if conf.Intent == "transfer" {
			amountVal := conf.Args["amount"].(float64)
			toAcc := conf.Args["to"].(string)
			paymentRefNo := mcpData["payment_ref_no"].(string)
			return fmt.Sprintf("खाता %s में %.2f रुपये सफलतापूर्वक ट्रांसफर कर दिए गए हैं। भुगतान संदर्भ संख्या (Payment Reference Number) %s है।", toAcc, amountVal, paymentRefNo), nil
		} else if conf.Intent == "block_card" {
			cardType := conf.Args["card"].(string)
			if cardType == "credit" {
				cardType = "क्रेडिट"
			} else {
				cardType = "डेबिट"
			}
			return fmt.Sprintf("आपका %s कार्ड सफलतापूर्वक ब्लॉक कर दिया गया है।", cardType), nil
		}
	}

	return mcpData["text"].(string), nil
}

// runLLMDeflector executes the LLM fallthrough in a deflector role
func (s *TurnSupervisor) runLLMDeflector(ctx context.Context, sessionID string, query string, history []ollama.ChatMessage) (string, error) {
	// Construct role-limited prompt (§6)
	systemPrompt := `You are a multilingual AI assistant for a retail bank. You support both English and Hindi.
LANGUAGE RULE: Detect the language of the customer's query (English or Hindi/Hinglish) and respond in the same language. For example, if the query is in Hindi, reply in Hindi/Hinglish.
ROLE LIMITATION: You are ONLY allowed to act as conversational glue. You can greet customers, clarify their intent, or offer to transfer them to a human representative.
CRITICAL SAFETY RULE: You have NO access to bank products, interest rates, fee structures, or account details. You must NEVER state any interest rates, card details, balance figures, transaction details, or payment procedures on your own.
If the customer asks a factual bank question that you do not have in your trusted conversation history, you MUST politely refuse to answer and offer to connect them to a human representative.
Never make up any figures, percentages, dates, phone numbers, or balances.
If the query is out of scope, state clearly that you cannot assist with that and offer a human agent.`

	var messages []ollama.ChatMessage
	messages = append(messages, ollama.ChatMessage{Role: "system", Content: systemPrompt})

	// Add recent N turns of history (up to last 4 messages to stay quick and keep cache hit high)
	maxHistory := 4
	if len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
	}
	messages = append(messages, history...)
	messages = append(messages, ollama.ChatMessage{Role: "user", Content: query})

	responseChan := make(chan string, 10)
	go func() {
		_, _ = s.Ollama.Chat(ctx, messages, true, responseChan)
	}()

	var fullResponse strings.Builder
	for token := range responseChan {
		fullResponse.WriteString(token)
	}

	return fullResponse.String(), nil
}

// ApplyOutputGuardrailFilter validates that any numerical values in the LLM response exist in the trusted source data.
// Returns a sanitized response or a deflection if the check fails.
func (s *TurnSupervisor) ApplyOutputGuardrailFilter(responseText string, trustedSourceData string) string {
	// Extract numbers from responseText
	re := regexp.MustCompile(`\b\d+(?:[\.,]\d+)?\b`)
	numbers := re.FindAllString(responseText, -1)

	if len(numbers) == 0 {
		return responseText // no numbers, safe to proceed
	}

	// Check if each number is present in the trusted source data
	for _, num := range numbers {
		// Ignore common tiny counters or standard greetings if they occur
		if num == "1" || num == "2" || num == "3" || num == "24" || num == "7" {
			continue
		}
		if !strings.Contains(trustedSourceData, num) {
			log.Printf("[GUARDRAIL FILTER TRIP] Suppressed response due to unverified value '%s'. Response was: '%s'", num, responseText)
			return "I'm sorry, I don't have that specific information right now. Let me connect you with a representative who can look that up for you."
		}
	}

	return responseText
}

// Helper parsing functions
func extractAmount(text string) float64 {
	re := regexp.MustCompile(`(?i)(?:transfer|send|wire)?\s*(\d+(?:\.\d+)?)\s*(?:inr|rupees|dollars|bucks)?`)
	matches := re.FindStringSubmatch(text)
	if len(matches) > 1 {
		val, err := strconv.ParseFloat(matches[1], 64)
		if err == nil {
			return val
		}
	}
	// fallback matching any number
	re2 := regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	numMatch := re2.FindString(text)
	if numMatch != "" {
		val, _ := strconv.ParseFloat(numMatch, 64)
		return val
	}
	return 0.0
}

func extractAccountNo(text string) string {
	re := regexp.MustCompile(`\b\d{5,12}\b`) // matches 5-12 digit numbers
	match := re.FindString(text)
	if match != "" {
		return match
	}
	return "9876543210" // default fallback destination account for demo
}

// isHindiText detects if the user utterance contains Devanagari characters or Hinglish keywords
func isHindiText(text string) bool {
	// Check for Devanagari characters
	for _, r := range text {
		if r >= 0x0900 && r <= 0x097F {
			return true
		}
	}
	// Check for Hinglish vocabulary
	textLower := strings.ToLower(text)
	hinglishSpecific := []string{
		"kitna", "hai", "karna", "paise", "bhejna", "bhejo", "kar do",
		"pichle", "lenden", "khate", "rupay", "rupaya", "kab", "kya",
		"sahi", "thik", "haan", "han", "nahi", "nahin", "mera", "mere",
	}
	for _, kw := range hinglishSpecific {
		if strings.Contains(textLower, kw) {
			return true
		}
	}
	return false
}

// LogConversationTurn logs a single dialogue turn to Cassandra/ScyllaDB asynchronously
func (s *TurnSupervisor) LogConversationTurn(ctx context.Context, userID, sessionID, role, transcript, intent, action, result string) {
	if s.Cassandra == nil {
		return
	}
	seqKey := fmt.Sprintf("session:%s:seq", sessionID)
	seq, err := s.Redis.Client.Incr(ctx, seqKey).Result()
	if err != nil {
		log.Printf("[Cassandra log] Warning: failed to increment sequence for session %s: %v", sessionID, err)
		seq = 1
	}

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := s.Cassandra.LogTurn(bgCtx, userID, sessionID, int(seq), role, transcript, intent, action, result)
		if err != nil {
			log.Printf("[Cassandra Error] Failed to write conversation history: %v", err)
		} else {
			log.Printf("[Cassandra Success] Recorded turn %d (%s) for session %s", seq, role, sessionID)
		}
	}()
}
