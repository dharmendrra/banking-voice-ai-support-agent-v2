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

	"banking-voice-ai-agent/internal/audit"
	"banking-voice-ai-agent/internal/contextmanager"
	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/mcp"
	"banking-voice-ai-agent/internal/ollama"
	"banking-voice-ai-agent/internal/telemetry"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
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
	Redis          *db.RedisManager
	Qdrant         *db.QdrantManager
	Cassandra      *db.CassandraManager
	Ollama         *ollama.Client
	MCP            *mcp.BankingMCPServer
	AuditService   *audit.ToolCallAuditService
	ContextManager *contextmanager.ContextManager

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
		AuditService:     audit.NewToolCallAuditService(m, r),
		ContextManager:   contextmanager.NewContextManager(r),
		ExtremeThreshold: 0.96, // Cosine score >= 0.96 for halt
		NormalThreshold:  0.94, // Cosine score >= 0.94 for final dispatch
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
	// Redact sensitive fields from payload for stdout/telemetry logging
	redactedPayload := make(map[string]any)
	sensitiveKeys := map[string]bool{
		"partial_text":     true,
		"final_transcript": true,
		"query":            true,
		"text":             true,
		"message":          true,
		"response":         true,
		"args":             true,
		"result":           true,
		"payload":          true,
		"user_id":          true,
		"session_id":       true,
	}
	for k, v := range payload {
		if sensitiveKeys[strings.ToLower(k)] {
			redactedPayload[k] = "[REDACTED]"
		} else {
			redactedPayload[k] = v
		}
	}
	log.Printf("[EVENT] %s %v", eventName, redactedPayload)
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
	embCtx, embCancel := context.WithTimeout(ctx, 5*time.Second)
	defer embCancel()

	emb, err := s.Ollama.GetEmbedding(embCtx, partialText)
	if err != nil {
		log.Printf("Embedding error on partial: %v", err)
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
func (s *TurnSupervisor) HandleFinalTranscript(ctx context.Context, turnID string, sessionID string, userID string, finalTranscript string, intercepted bool, interceptedPayload map[string]any, onChunk func(eventType string, text string)) (path string, text string, err error) {
	startTime := time.Now()
	defer func() {
		log.Printf("[Supervisor Turn Latency] turn_id: %s, duration: %v, path: %s", turnID, time.Since(startTime), path)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Start OTel turn span
	ctx, span := telemetry.Step(ctx, "turn",
		attribute.String("turn_id", turnID),
	)
	defer span.End()

	log.Printf("[Supervisor] Handling final transcript (intercepted: %t)", intercepted)

	// Fetch conversation history from Redis via ContextManager
	history, err := s.ContextManager.GetContext(ctx, sessionID)
	if err != nil {
		log.Printf("Failed to load history from ContextManager: %v", err)
	}

	// Determine if we should bypass the cache (short/one-word answers in an active conversation are context-dependent)
	wordCount := len(strings.Fields(finalTranscript))
	bypassCache := len(history) > 0 && wordCount < 3

	// Do not bypass cache for direct command keywords
	lowerTranscript := strings.ToLower(finalTranscript)
	if strings.Contains(lowerTranscript, "transaction") || strings.Contains(lowerTranscript, "balance") || strings.Contains(lowerTranscript, "due") || strings.Contains(lowerTranscript, "block") {
		bypassCache = false
	}

	// 1. Reconcile if we had an early halt
	if !bypassCache && intercepted && interceptedPayload != nil {
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

					return s.executeCommitPath(ctx, turnID, sessionID, userID, interceptedPayload, finalTranscript, history, onChunk)
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
	var actionMatches, faqMatches []db.QdrantMatch
	if !bypassCache {
		emb, err := s.Ollama.GetEmbedding(ctx, finalTranscript)
		if err != nil {
			return "", "", fmt.Errorf("failed to embed final transcript: %w", err)
		}

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
	} else {
		s.LogEvent(ctx, turnID, "cache_bypass", map[string]any{
			"turn_id": turnID,
			"reason":  "short query in active conversation",
			"query":   finalTranscript,
		})
	}

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
		return s.executeCommitPath(ctx, turnID, sessionID, userID, payload, finalTranscript, history, onChunk)
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
		return "faq", answer, nil
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
	response, err := s.runLLMDeflector(ctx, sessionID, finalTranscript, history, onChunk)
	if err != nil {
		return "", "", err
	}

	cleanedResponse := cleanJSONResponse(response)

	// Delegate tool call verification, execution, and auditing to the ToolCallAuditService
	if strings.HasPrefix(cleanedResponse, "{") && strings.HasSuffix(cleanedResponse, "}") {
		res, err := s.AuditService.ExecuteToolCall(ctx, turnID, sessionID, userID, cleanedResponse)
		if err == nil {
			if res.Status == "confirm_required" {
				return s.executeCommitPath(ctx, turnID, sessionID, userID, res.Payload, finalTranscript, history, onChunk)
			} else if res.Status == "success" {
				// Use LLM to formulate a natural verbal response grounded in the tool output
				formattedText, err := s.formatLLMResponse(ctx, finalTranscript, res.ResponseText, onChunk)
				if err != nil {
					log.Printf("[Supervisor] Warning: LLM formatting failed for fallback tool: %v. Using raw response.", err)
					formattedText = res.ResponseText
				}
				historyStr := s.ContextManager.SerializeHistory(history)
				safeText := s.ApplyOutputGuardrailFilter(formattedText, res.ResponseText+" "+historyStr)
				return "llm", safeText, nil
			} else if res.Status == "resume_playback" {
				return "resume_playback", "", nil
			}
		} else {
			log.Printf("[Supervisor] ToolCallAuditService error: %v (cleaned response was: %s)", err, cleanedResponse)
			return "llm", "I'm sorry, I encountered a validation check error.", nil
		}
	}

	// Run output guardrail filter to block un-sourced values
	historyStr := s.ContextManager.SerializeHistory(history)
	safeText := s.ApplyOutputGuardrailFilter(response, historyStr)

	return "llm", safeText, nil
}

// executeCommitPath handles executing the action path or scheduling confirmation
func (s *TurnSupervisor) executeCommitPath(ctx context.Context, turnID string, sessionID string, userID string, actionPayload map[string]any, finalTranscript string, history []ollama.ChatMessage, onChunk func(eventType string, text string)) (string, string, error) {
	intent := actionPayload["intent"].(string)
	bankAction := actionPayload["bank_action"].(string)

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
		refNo, _ := args["unique_ref_no"].(string)
		confCtx := ConfirmationContext{
			Intent:      intent,
			ToolName:    bankAction,
			Args:        args,
			UniqueRefNo: refNo,
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

	// Use LLM to formulate a natural verbal response grounded in the tool output
	responseText, err := s.formatLLMResponse(ctx, finalTranscript, mcpRes, onChunk)
	if err != nil {
		log.Printf("[Supervisor] Warning: LLM formatting failed: %v. Falling back to raw response.", err)
		responseText = mcpRes
	}

	// Verify guardrail
	historyStr := s.ContextManager.SerializeHistory(history)
	safeText := s.ApplyOutputGuardrailFilter(responseText, mcpRes+" "+historyStr)

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
func (s *TurnSupervisor) runLLMDeflector(ctx context.Context, sessionID string, query string, history []ollama.ChatMessage, onChunk func(eventType string, text string)) (string, error) {
	// Construct role-limited prompt (§6)
	systemPrompt := DefaultSystemPrompt

	var messages []ollama.ChatMessage
	messages = append(messages, ollama.ChatMessage{Role: "system", Content: systemPrompt})

	messages = append(messages, history...)
	messages = append(messages, ollama.ChatMessage{Role: "user", Content: query})

	responseChan := make(chan ollama.ChatResponse, 10)
	var chatErr error
	go func() {
		_, chatErr = s.Ollama.Chat(ctx, messages, true, responseChan)
	}()

	var fullResponse strings.Builder
	for chunk := range responseChan {
		if chunk.Message.Thinking != "" && onChunk != nil {
			onChunk("thought", chunk.Message.Thinking)
		}
		if chunk.Message.Content != "" {
			if onChunk != nil {
				onChunk("speech", chunk.Message.Content)
			}
			fullResponse.WriteString(chunk.Message.Content)
		}
	}

	if chatErr != nil {
		log.Printf("[Supervisor] Ollama Chat error in runLLMDeflector: %v", chatErr)
		return "", chatErr
	}

	return fullResponse.String(), nil
}

// ApplyOutputGuardrailFilter validates that any numerical values in the LLM response exist in the trusted source data.
// Returns a sanitized response or a deflection if the check fails.
func (s *TurnSupervisor) ApplyOutputGuardrailFilter(responseText string, trustedSourceData string) string {
	// 1. Security Check: Block any disclosure of PIN, CVV, OTP, or Passwords
	rePinCvv := regexp.MustCompile(`(?i)\b(pin|cvv|cvc|otp|password|passcode)\b.*?(\b\d{3,6}\b)`)
	if rePinCvv.MatchString(responseText) {
		log.Printf("[SECURITY] Suppressed response due to sensitive credentials (PIN/CVV/OTP) leak risk")
		return "For security reasons, I cannot disclose or discuss PINs, CVVs, or passwords over this line."
	}

	// 2. Masking Check: Automatically mask full credit card numbers (12-19 digits)
	// Match typical credit cards: 16 digits, with optional spaces or hyphens
	reCard := regexp.MustCompile(`\b(?:\d{4}[- ]?){3}(\d{4})\b`)
	if reCard.MatchString(responseText) {
		responseText = reCard.ReplaceAllString(responseText, "XXXX-XXXX-XXXX-$1")
	}
	
	// Mask any raw card number written as a single long digit sequence (9 to 15 digits + 4 suffix)
	reLongNum := regexp.MustCompile(`\b\d{9,15}(\d{4})\b`)
	if reLongNum.MatchString(responseText) {
		responseText = reLongNum.ReplaceAllString(responseText, "******$1")
	}

	// Extract and parse all numbers from trustedSourceData as float64
	trustedFloatsList := extractAllFloats(trustedSourceData)
	trustedFloats := make(map[float64]bool)
	for _, val := range trustedFloatsList {
		trustedFloats[val] = true
	}

	// Extract and parse all numbers from responseText
	responseFloats := extractAllFloats(responseText)

	if len(responseFloats) == 0 {
		return responseText // no numbers, safe to proceed
	}

	// Check if each number is present in the trusted source data
	for _, val := range responseFloats {
		// Ignore common non-financial constants (0-10), time (24/7/365, etc.)
		intVal := int(val)
		if val == float64(intVal) && intVal >= 0 && intVal <= 10 {
			continue
		}
		if val == 24 || val == 7 || val == 30 || val == 60 || val == 365 || val == 18 {
			continue
		}

		// Check float existence in trustedFloats
		if !trustedFloats[val] {
			log.Printf("[GUARDRAIL FILTER TRIP] Suppressed response due to unverified numerical value: %f", val)
			return "I'm sorry, I don't have that specific information right now. Let me connect you with a representative who can look that up for you."
		}
	}

	return responseText
}

// extractAllFloats parses all numbers out of a text string as float64, cleaning any commas or trailing periods
func extractAllFloats(text string) []float64 {
	reNum := regexp.MustCompile(`\d[\d,.]*`)
	matches := reNum.FindAllString(text, -1)
	var results []float64
	for _, match := range matches {
		cleaned := match
		for strings.HasSuffix(cleaned, ".") || strings.HasSuffix(cleaned, ",") {
			cleaned = cleaned[:len(cleaned)-1]
		}
		cleaned = strings.ReplaceAll(cleaned, ",", "")
		if val, err := strconv.ParseFloat(cleaned, 64); err == nil {
			results = append(results, val)
		}
	}
	return results
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

// LogConversationTurn logs a single dialogue turn to Redis Stream conversation_history_stream
func (s *TurnSupervisor) LogConversationTurn(ctx context.Context, userID, sessionID, role, transcript, intent, action, result string) {
	seqKey := fmt.Sprintf("session:%s:seq", sessionID)
	seq, err := s.Redis.Client.Incr(ctx, seqKey).Result()
	if err != nil {
		log.Printf("[Redis log] Warning: failed to increment sequence for session %s: %v", sessionID, err)
		seq = 1
	}

	// Package the turn event
	payload := map[string]interface{}{
		"user_id":    userID,
		"session_id": sessionID,
		"turn_seq":   int(seq),
		"role":       role,
		"transcript": transcript,
		"intent":     intent,
		"action":     action,
		"result":     result,
		"timestamp":  time.Now().Format(time.RFC3339Nano),
	}

	// Publish to the stream (low-latency, fire-and-forget)
	err = s.Redis.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: "conversation_history_stream",
		Values: payload,
	}).Err()

	if err != nil {
		log.Printf("[Stream Error] Failed to publish turn to history stream: %v", err)
	} else {
		log.Printf("[Stream Success] Published turn %d (%s) for session %s to stream", seq, role, sessionID)
	}
}

// formatLLMResponse utilizes the LLM to write a friendly customer-facing verbal response from the raw bank data.
func (s *TurnSupervisor) formatLLMResponse(ctx context.Context, query string, mcpRes string, onChunk func(eventType string, text string)) (string, error) {
	systemPrompt := "You are a friendly customer service agent for a retail bank. Formulate a natural, conversational response based ONLY on the provided raw bank data. You MUST list all transactions provided in the raw data, detailing the merchant/description, amount, and date. Speak in friendly, conversational, short sentences. For transaction lists, use ordinals like First, Second, etc. and avoid robotic signs like plus/minus. Translate negative/positive values to friendly descriptions (e.g. 'spent 150' instead of '-150')."
	
	promptText := fmt.Sprintf("Customer query: %s\nRaw bank data: %s\nFormulate the response:", query, mcpRes)
	
	messages := []ollama.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: promptText},
	}
	
	responseChan := make(chan ollama.ChatResponse, 10)
	var chatErr error
	go func() {
		_, chatErr = s.Ollama.Chat(ctx, messages, true, responseChan)
	}()
	
	var llmResponse strings.Builder
	for chunk := range responseChan {
		if chunk.Message.Thinking != "" && onChunk != nil {
			onChunk("thought", chunk.Message.Thinking)
		}
		if chunk.Message.Content != "" {
			if onChunk != nil {
				onChunk("speech", chunk.Message.Content)
			}
			llmResponse.WriteString(chunk.Message.Content)
		}
	}

	if chatErr != nil {
		log.Printf("[Supervisor] Ollama Chat error in formatLLMResponse: %v", chatErr)
		return "", chatErr
	}
	return llmResponse.String(), nil
}

// cleanJSONResponse extracts the JSON block from an LLM output, removing any markdown code fences or conversational prefixes.
func cleanJSONResponse(input string) string {
	input = strings.TrimSpace(input)
	// Remove markdown code fences if present
	input = strings.TrimPrefix(input, "```json")
	input = strings.TrimPrefix(input, "```")
	input = strings.TrimSuffix(input, "```")
	input = strings.TrimSpace(input)

	firstBrace := strings.Index(input, "{")
	lastBrace := strings.LastIndex(input, "}")
	if firstBrace != -1 && lastBrace != -1 && lastBrace > firstBrace {
		return input[firstBrace : lastBrace+1]
	}
	return input
}
