package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"banking-voice-ai-agent/internal/audit"
	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/ollama"
	"banking-voice-ai-agent/internal/telemetry"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const DefaultSystemPrompt = `You are a friendly customer service agent for a retail bank. You support both English and Hindi.
SPEAKING STYLE: Speak in short, segmented sentences. Keep responses conversational and brief. When interacting in English, use standard English vocabulary, cadence, and greetings (like "Hello", "Hi"); do NOT mix Hindi words or greetings (like "Namaste") into pure English responses. However, when the user speaks Hindi or Hinglish, adjust your tone, phrasing, and vocabulary to sound like a polite, warm Indian customer service agent (using Devnagari characters and respectful terms like "Aap" or "Namaste"). Avoid technical jargon, and allow for natural pauses.
NO EMOJIS OR MARKDOWN: This text is read directly by a Text-to-Speech (TTS) engine. Emojis (e.g. 😊, 👍), emoticons, and markdown formatting (like asterisks, bullet points, or bold text) are read aloud literally by the TTS engine (e.g. "smiling face emoji"). You MUST NOT include any emojis, smileys, emoticons, asterisks, bullet points, hashtags, or markdown formatting in your responses. Use ONLY clean plain text (letters, numbers, and basic punctuation like periods, commas, and question marks).
PRONUNCIATION & NAMES: Pay absolute attention to personal names. To ensure correct pronunciation by the Text-to-Speech (TTS) engine across different language configurations:
- When the English (American) agent is speaking an Indian name (e.g. "Dharmendra"), write it with phonetically clear helper hints if necessary, or in its most standard, clean transliterated form to prevent the US voice from mangling it.
- When the Indian agent is speaking an English name, ensure it is represented clearly and correctly without local linguistic accents or phonemes that would distort the name.
Never spell names in a way that causes awkward stuttering in the audio synthesis.
LANGUAGE RULE: Detect the language of the customer's query (English or Hindi/Hinglish) and respond in the same language. If the customer greets you or queries you in English (e.g., "hello", "hi", "good morning"), you MUST respond in clean English. Never default to Hindi or Hinglish unless the customer explicitly initiates speaking in Hindi or Hinglish. When responding in Hindi or Hinglish, you MUST write your entire response using the Devnagari script (Hindi fonts/characters, e.g., 'नमस्ते धर्मेंद्र, आप कैसे हैं?'). Do NOT write Hindi or Hinglish response words using the English alphabet (Latin script).

ROLES & OUTCOMES:
1. TOOL CALLS (JSON only): If the user asks for balance, transactions, card due date, block card, or transfer, and the data has not been fetched yet in the conversation history, you MUST respond ONLY with the JSON tool call. No other text.
Supported tool_names:
- "get_balance"
- "get_transactions"
- "get_due_date"
- "block_card"
- "transfer"
- "resume_playback" (when user asks to "continue", "go on", or "resume")

EXAMPLE TOOL CALL:
If the user asks "my transactions", you respond exactly with:
{
  "tool_name": "get_transactions",
  "args": {}
}

2. CONTEXT RESPONSES (Natural speech): If the details the user is asking about (e.g. a specific transaction amount, merchant name, due date, or card status) are ALREADY visible in the conversation history, do NOT output JSON. Instead, read the history and answer the user's question directly.

3. DEFLECTOR (Natural speech): For out-of-scope queries (e.g. general knowledge, weather, news), refuse politely and offer to connect to a human agent.

CRITICAL SAFETY: Never invent or hallucinate transaction lists, balance figures, or account numbers. Only state details that are explicitly written in your conversation history.`

type ConfigPayload struct {
	WarmingEnabled   bool    `json:"warming_enabled"`
	ExtremeThreshold float64 `json:"extreme_threshold"`
	NormalThreshold  float64 `json:"normal_threshold"`
}

type ConfirmationContext struct {
	Intent      string         `json:"intent"`
	ToolName    string         `json:"tool_name"`
	Args        map[string]any `json:"args"`
	UniqueRefNo string         `json:"unique_ref_no"`
}

type OrchestratorServer struct {
	Redis             *db.RedisManager
	ContextService    string
	CacheService      string
	InferenceService  string
	ToolService       string
	HTTPClient        *http.Client

	// Local configurations
	mu               sync.RWMutex
	warmingEnabled   bool
	extremeThreshold float64
	normalThreshold  float64

	// Session states for warming cancellation
	muWarming     sync.Mutex
	warmingCancel map[string]context.CancelFunc
}

func main() {
	log.Println("Starting Stateless LLM Micro-Orchestrator Server...")

	tShutdown, logger, err := telemetry.Init(context.Background(), "micro-orchestrator")
	if err != nil {
		log.Fatalf("Fatal: Telemetry initialization failed (observability endpoint is down): %v", err)
	}
	defer func() { _ = tShutdown(context.Background()) }()
	slog.SetDefault(logger)

	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	contextServiceURL := getEnv("CONTEXT_SERVICE_URL", "http://localhost:9087")
	cacheServiceURL := getEnv("CACHE_SERVICE_URL", "http://localhost:9089")
	inferenceServiceURL := getEnv("INFERENCE_SERVICE_URL", "http://localhost:9091")
	toolServiceURL := getEnv("TOOL_SERVICE_URL", "http://localhost:9088")

	redisMgr, err := db.NewRedisManager(redisAddr)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Redis: %v", err)
	}

	srv := &OrchestratorServer{
		Redis:            redisMgr,
		ContextService:   contextServiceURL,
		CacheService:     cacheServiceURL,
		InferenceService: inferenceServiceURL,
		ToolService:      toolServiceURL,
		HTTPClient: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		warmingEnabled:   true,
		extremeThreshold: 0.90,
		normalThreshold:  0.70,
		warmingCancel:    make(map[string]context.CancelFunc),
	}

	mux := http.NewServeMux()
	mux.Handle("/api/partial", otelhttp.NewHandler(http.HandlerFunc(srv.handlePartial), "partial"))
	mux.Handle("/api/final", otelhttp.NewHandler(http.HandlerFunc(srv.handleFinal), "final"))
	mux.Handle("/api/confirmation", otelhttp.NewHandler(http.HandlerFunc(srv.handleConfirmation), "confirmation"))
	mux.Handle("/api/bank-data", otelhttp.NewHandler(http.HandlerFunc(srv.handleBankData), "bank-data"))
	mux.Handle("/api/config", otelhttp.NewHandler(http.HandlerFunc(srv.handleConfig), "config"))
	mux.Handle("/healthz", otelhttp.NewHandler(http.HandlerFunc(srv.handleHealthz), "healthz"))

	server := &http.Server{
		Addr:    ":9083",
		Handler: withLogging(mux),
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down Stateless LLM Micro-Orchestrator Server...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Println("LLM Micro-Orchestrator Server listening on port 9083")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

type PartialRequest struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Text      string `json:"text"`
}

type FinalRequest struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Text      string `json:"text"`
	UserID    string `json:"user_id"`
}

type ConfirmationRequest struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Text      string `json:"text"`
	UserID    string `json:"user_id"`
}

func (s *OrchestratorServer) handlePartial(w http.ResponseWriter, r *http.Request) {
	var req PartialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := telemetry.WithTraceContext(r.Context(), req.SessionID, req.TurnID)
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("session_id", req.SessionID),
		attribute.String("turn_id", req.TurnID),
	)

	s.muWarming.Lock()
	if oldCancel, ok := s.warmingCancel[req.SessionID]; ok {
		oldCancel()
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	s.warmingCancel[req.SessionID] = cancel
	s.muWarming.Unlock()

	s.mu.RLock()
	warmingEnabled := s.warmingEnabled
	extremeThreshold := s.extremeThreshold
	s.mu.RUnlock()

	partialLen := len(strings.Fields(req.Text))

	warmDone := make(chan struct{})

	// Branch A: speculative prefill
	go func(wCtx context.Context, done chan struct{}, text string) {
		defer close(done)
		if warmingEnabled && text != "" {
			s.LogEvent(wCtx, req.TurnID, "warm_start", map[string]any{
				"turn_id":              req.TurnID,
				"at_token":             partialLen,
				"static_prefix_reused": true,
				"text":                 text,
			})

			// POST to inference-service /chat
			chatReqBody, _ := json.Marshal(map[string]any{
				"messages": []ollama.ChatMessage{{Role: "user", Content: text}},
				"stream":   false,
			})
			req, err := http.NewRequestWithContext(wCtx, "POST", s.InferenceService+"/chat", bytes.NewBuffer(chatReqBody))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				resp, err := s.HTTPClient.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}
		} else if text != "" {
			s.LogEvent(wCtx, req.TurnID, "warm_shed", map[string]any{
				"turn_id":  req.TurnID,
				"at_token": partialLen,
				"reason":   "GPU utilization watermark exceeded / manual override",
			})
		}
	}(cancelCtx, warmDone, req.Text)

	// Branch B: cache probe
	var isHalted bool
	var matchedAction map[string]any

	if req.Text != "" {
		cacheReqBody, _ := json.Marshal(map[string]string{"text": req.Text})
		reqPost, err := http.NewRequestWithContext(ctx, "POST", s.CacheService+"/search", bytes.NewBuffer(cacheReqBody))
		if err == nil {
			reqPost.Header.Set("Content-Type", "application/json")
			s.LogEvent(ctx, req.TurnID, "cache_probe", map[string]any{
				"turn_id":     req.TurnID,
				"partial_len": partialLen,
			})

			resp, err := s.HTTPClient.Do(reqPost)
			if err == nil {
				defer resp.Body.Close()
				var cacheRes struct {
					BestActionScore float64        `json:"best_action_score"`
					MatchedAction   map[string]any `json:"matched_action"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&cacheRes); err == nil {
					s.LogEvent(ctx, req.TurnID, "cache_probe_result", map[string]any{
						"turn_id":           req.TurnID,
						"best_action_score": cacheRes.BestActionScore,
					})

					if cacheRes.BestActionScore >= extremeThreshold {
						intent, _ := cacheRes.MatchedAction["intent"].(string)
						s.LogEvent(ctx, req.TurnID, "halt_point", map[string]any{
							"turn_id":       req.TurnID,
							"at_token":      partialLen,
							"action_intent": intent,
							"score":         cacheRes.BestActionScore,
							"EXTREME":       extremeThreshold,
							"message":       "HALT FIRED: speculative LLM prefill aborted",
						})

						cancel() // Abort prefill
						isHalted = true
						matchedAction = cacheRes.MatchedAction

						s.LogEvent(ctx, req.TurnID, "warm_outcome", map[string]any{
							"turn_id":              req.TurnID,
							"prefill_tokens":       partialLen,
							"used":                 false,
							"discarded":            true,
							"would_have_reclaimed": warmingEnabled,
						})
					}
				}
			}
		}
	}

	resp := map[string]any{
		"halt":           isHalted,
		"matched_action": matchedAction,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *OrchestratorServer) handleFinal(w http.ResponseWriter, r *http.Request) {
	var req FinalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := telemetry.WithTraceContext(r.Context(), req.SessionID, req.TurnID)
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("session_id", req.SessionID),
		attribute.String("turn_id", req.TurnID),
	)

	s.muWarming.Lock()
	if cancel, ok := s.warmingCancel[req.SessionID]; ok {
		cancel()
		delete(s.warmingCancel, req.SessionID)
	}
	s.muWarming.Unlock()

	s.mu.RLock()
	warmingEnabled := s.warmingEnabled
	normalThreshold := s.normalThreshold
	s.mu.RUnlock()

	// Check if pending confirmation in Redis
	confirmKey := fmt.Sprintf("session:%s:confirm", req.SessionID)
	confirmData, err := s.Redis.Client.Get(ctx, confirmKey).Result()
	hasPendingConfirm := err == nil && confirmData != ""

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Transfer-Encoding", "chunked")

	var flusher http.Flusher
	if f, ok := w.(http.Flusher); ok {
		flusher = f
	}

	writeChunk := func(eventType string, text string) {
		chunk := map[string]any{
			"type": eventType,
			"text": text,
		}
		_ = json.NewEncoder(w).Encode(chunk)
		if flusher != nil {
			flusher.Flush()
		}
	}

	userID := req.UserID
	if userID == "" {
		userID = "mock_user_123"
	}

	var pathType, replyText string

	if hasPendingConfirm {
		// 1. Process Confirmation path
		pathType = "confirmation"
		s.Redis.Client.Del(ctx, confirmKey)

		var conf ConfirmationContext
		_ = json.Unmarshal([]byte(confirmData), &conf)

		confirmReqBody, _ := json.Marshal(map[string]any{
			"tool_name":         conf.ToolName,
			"args":              conf.Args,
			"confirmation_text": req.Text,
			"user_id":           userID,
			"session_id":        req.SessionID,
			"turn_id":           req.TurnID,
		})

		reqConfirm, err := http.NewRequestWithContext(ctx, "POST", s.ToolService+"/confirm", bytes.NewBuffer(confirmReqBody))
		if err == nil {
			reqConfirm.Header.Set("Content-Type", "application/json")
			s.LogEvent(ctx, req.TurnID, "confirmation_execute", map[string]any{
				"turn_id": req.TurnID,
				"intent":  conf.Intent,
			})

			resp, err := s.HTTPClient.Do(reqConfirm)
			if err == nil {
				defer resp.Body.Close()
				var confirmRes struct {
					Confirmed    bool   `json:"confirmed"`
					ResponseText string `json:"response_text"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&confirmRes); err == nil {
					replyText = confirmRes.ResponseText
					s.LogEvent(ctx, req.TurnID, "confirmation_outcome", map[string]any{
						"turn_id":       req.TurnID,
						"intent":        conf.Intent,
						"confirmed":     confirmRes.Confirmed,
						"unique_ref_no": conf.UniqueRefNo,
					})
				} else {
					replyText = "Transaction confirmation process failed."
				}
			} else {
				replyText = "Transaction confirmation process failed."
			}
		} else {
			replyText = "Transaction confirmation process failed."
		}

	} else {
		// 2. Standard path (context load -> cache -> LLM fallback)
		var history []ollama.ChatMessage
		loadBody, _ := json.Marshal(map[string]string{"session_id": req.SessionID})
		reqCtx, err := http.NewRequestWithContext(ctx, "POST", s.ContextService+"/load", bytes.NewBuffer(loadBody))
		if err == nil {
			reqCtx.Header.Set("Content-Type", "application/json")
			resp, err := s.HTTPClient.Do(reqCtx)
			if err == nil {
				defer resp.Body.Close()
				var loadRes struct {
					Messages []ollama.ChatMessage `json:"messages"`
				}
				if json.NewDecoder(resp.Body).Decode(&loadRes) == nil {
					history = loadRes.Messages
				}
			}
		}

		// Detect Prompt Injection or Out-of-Scope queries
		lowerQuery := strings.ToLower(req.Text)
		isPromptInjection := strings.Contains(lowerQuery, "ignore all") ||
			strings.Contains(lowerQuery, "ignore previous") ||
			strings.Contains(lowerQuery, "system prompt") ||
			strings.Contains(lowerQuery, "system instruction") ||
			strings.Contains(lowerQuery, "override")
		
		isOutOfScope := (strings.Contains(lowerQuery, "story") && !strings.Contains(lowerQuery, "history")) ||
			strings.Contains(lowerQuery, "dragon") ||
			strings.Contains(lowerQuery, "joke") ||
			strings.Contains(lowerQuery, "weather") ||
			strings.Contains(lowerQuery, "poem") ||
			strings.Contains(lowerQuery, "capital of") ||
			(strings.Contains(lowerQuery, "who is") && !strings.Contains(lowerQuery, "dharmendra"))

		if isPromptInjection || isOutOfScope {
			pathType = "deflection"
			if isHindiText(req.Text) {
				replyText = "मुझे खेद है, मैं केवल बैंकिंग से संबंधित प्रश्नों में आपकी सहायता कर सकता हूँ।"
			} else {
				replyText = "I am sorry, but I can only assist with banking related queries. I cannot help you with other topics."
			}
		} else {
			wordCount := len(strings.Fields(req.Text))
			bypassCache := len(history) > 0 && wordCount < 3

			lowerTranscript := strings.ToLower(req.Text)
			if strings.Contains(lowerTranscript, "transaction") || strings.Contains(lowerTranscript, "balance") || strings.Contains(lowerTranscript, "due") || strings.Contains(lowerTranscript, "block") {
				bypassCache = false
			}

		var cacheMatched bool
		var searchRes struct {
			BestActionScore float64        `json:"best_action_score"`
			MatchedAction   map[string]any `json:"matched_action"`
			BestFAQScore    float64        `json:"best_faq_score"`
			MatchedFAQ      map[string]any `json:"matched_faq"`
		}

		if !bypassCache {
			cacheReqBody, _ := json.Marshal(map[string]string{"text": req.Text})
			reqSearch, err := http.NewRequestWithContext(ctx, "POST", s.CacheService+"/search", bytes.NewBuffer(cacheReqBody))
			if err == nil {
				reqSearch.Header.Set("Content-Type", "application/json")
				resp, err := s.HTTPClient.Do(reqSearch)
				if err == nil {
					defer resp.Body.Close()
					if json.NewDecoder(resp.Body).Decode(&searchRes) == nil {
						cacheMatched = true
					}
				}
			}
		} else {
			s.LogEvent(ctx, req.TurnID, "cache_bypass", map[string]any{
				"turn_id": req.TurnID,
				"reason":  "short query in active conversation",
				"query":   req.Text,
			})
		}

		// A. Action Intent Match
		if cacheMatched && searchRes.BestActionScore >= normalThreshold {
			intent, _ := searchRes.MatchedAction["intent"].(string)

			s.LogEvent(ctx, req.TurnID, "dispatch", map[string]any{
				"turn_id":        req.TurnID,
				"path":           "action",
				"matched_intent": intent,
				"score":          searchRes.BestActionScore,
			})

			pathType, replyText = s.executeCommitPath(ctx, req.TurnID, req.SessionID, userID, searchRes.MatchedAction, req.Text, history, writeChunk)

		} else if cacheMatched && searchRes.BestFAQScore >= normalThreshold {
			// B. FAQ Match
			answer, _ := searchRes.MatchedFAQ["answer"].(string)
			s.LogEvent(ctx, req.TurnID, "dispatch", map[string]any{
				"turn_id":        req.TurnID,
				"path":           "faq",
				"matched_intent": "faq_item",
				"score":          searchRes.BestFAQScore,
			})

			pathType = "faq"
			replyText = answer

		} else {
			// C. LLM Fallthrough
			s.LogEvent(ctx, req.TurnID, "dispatch", map[string]any{
				"turn_id": req.TurnID,
				"path":    "llm",
			})

			if warmingEnabled {
				s.LogEvent(ctx, req.TurnID, "warm_outcome", map[string]any{
					"turn_id":        req.TurnID,
					"prefill_tokens": len(strings.Fields(req.Text)),
					"used":           true,
					"discarded":      false,
				})
			}

			// Call llm-inference-service/chat
			chatMessages := []ollama.ChatMessage{
				{Role: "system", Content: DefaultSystemPrompt},
			}
			chatMessages = append(chatMessages, history...)
			chatMessages = append(chatMessages, ollama.ChatMessage{Role: "user", Content: req.Text})

			chatReqBody, _ := json.Marshal(map[string]any{
				"messages": chatMessages,
				"stream":   true,
			})

			reqChat, err := http.NewRequestWithContext(ctx, "POST", s.InferenceService+"/chat", bytes.NewBuffer(chatReqBody))
			var fullResponse strings.Builder
			if err == nil {
				reqChat.Header.Set("Content-Type", "application/json")
				resp, err := s.HTTPClient.Do(reqChat)
				if err == nil {
					defer resp.Body.Close()
					decoder := json.NewDecoder(resp.Body)
					for {
						var chunk struct {
							Type string `json:"type"`
							Text string `json:"text"`
						}
						if err := decoder.Decode(&chunk); err != nil {
							break
						}
						if chunk.Type == "thought" {
							writeChunk("thought", chunk.Text)
						} else if chunk.Type == "speech" {
							writeChunk("speech", chunk.Text)
							fullResponse.WriteString(chunk.Text)
						}
					}
				}
			}

			rawLLMResponse := fullResponse.String()
			cleanedResponse := cleanJSONResponse(rawLLMResponse)

			if strings.HasPrefix(cleanedResponse, "{") && strings.HasSuffix(cleanedResponse, "}") {
				// We generated a tool call
				toolReqBody, _ := json.Marshal(map[string]any{
					"raw_json":   cleanedResponse,
					"user_id":    userID,
					"session_id": req.SessionID,
					"turn_id":    req.TurnID,
				})
				reqTool, err := http.NewRequestWithContext(ctx, "POST", s.ToolService+"/execute", bytes.NewBuffer(toolReqBody))
				if err == nil {
					reqTool.Header.Set("Content-Type", "application/json")
					resp, err := s.HTTPClient.Do(reqTool)
					if err == nil {
						defer resp.Body.Close()
						var toolRes audit.ToolExecutionResult
						if json.NewDecoder(resp.Body).Decode(&toolRes) == nil {
							if toolRes.Status == "confirm_required" {
								pathType, replyText = s.executeCommitPath(ctx, req.TurnID, req.SessionID, userID, toolRes.Payload, req.Text, history, writeChunk)
							} else if toolRes.Status == "success" {
								// Format response
								formatReqBody, _ := json.Marshal(map[string]any{
									"query":      req.Text,
									"mcp_result": toolRes.ResponseText,
									"stream":     false,
								})
								reqFormat, err := http.NewRequestWithContext(ctx, "POST", s.InferenceService+"/format", bytes.NewBuffer(formatReqBody))
								var formattedText string
								if err == nil {
									reqFormat.Header.Set("Content-Type", "application/json")
									respF, err := s.HTTPClient.Do(reqFormat)
									if err == nil {
										defer respF.Body.Close()
										var formatRes struct {
											Text string `json:"text"`
										}
										if json.NewDecoder(respF.Body).Decode(&formatRes) == nil {
											formattedText = formatRes.Text
										}
									}
								}
								if formattedText == "" {
									formattedText = toolRes.ResponseText
								}
								historyStr := s.SerializeHistory(history)
								replyText = s.ApplyOutputGuardrailFilter(formattedText, toolRes.ResponseText+" "+historyStr)
								pathType = "llm"
							} else if toolRes.Status == "resume_playback" {
								pathType = "resume_playback"
								replyText = ""
							}
						}
					}
				}
				} else {
					historyStr := s.SerializeHistory(history)
					replyText = s.ApplyOutputGuardrailFilter(rawLLMResponse, historyStr)
					pathType = "llm"
				}
			}
		}
	}

	// Update conversation context history and log to streams asynchronously
	go func() {
		histCtx := telemetry.WithTraceContext(context.Background(), req.SessionID, req.TurnID)

		// 1. Call context-service to append turns
		contextAppendBody, _ := json.Marshal(map[string]any{
			"session_id":        req.SessionID,
			"user_message":      req.Text,
			"assistant_message": replyText,
		})
		reqCtx, err := http.NewRequestWithContext(histCtx, "POST", s.ContextService+"/save", bytes.NewBuffer(contextAppendBody))
		if err == nil {
			reqCtx.Header.Set("Content-Type", "application/json")
			resp, err := s.HTTPClient.Do(reqCtx)
			if err == nil {
				resp.Body.Close()
			}
		}

		// 2. Publish completion events to conversation_history_stream
		s.LogConversationTurn(histCtx, userID, req.SessionID, "user", req.Text, "", "", "")
		s.LogConversationTurn(histCtx, userID, req.SessionID, "assistant", replyText, pathType, "", "")
	}()

	resp := map[string]any{
		"type":            "final",
		"path":            pathType,
		"text":            replyText,
		"tokens_count":    len(strings.Fields(req.Text)),
		"warming_enabled": warmingEnabled,
	}
	_ = json.NewEncoder(w).Encode(resp)
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *OrchestratorServer) handleConfirmation(w http.ResponseWriter, r *http.Request) {
	var req ConfirmationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := telemetry.WithTraceContext(r.Context(), req.SessionID, req.TurnID)
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("session_id", req.SessionID),
		attribute.String("turn_id", req.TurnID),
	)

	userID := req.UserID
	if userID == "" {
		userID = "mock_user_123"
	}

	confirmKey := fmt.Sprintf("session:%s:confirm", req.SessionID)
	confirmData, err := s.Redis.Client.Get(ctx, confirmKey).Result()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "No transaction is currently awaiting confirmation."})
		return
	}
	s.Redis.Client.Del(ctx, confirmKey)

	var conf ConfirmationContext
	_ = json.Unmarshal([]byte(confirmData), &conf)

	confirmReqBody, _ := json.Marshal(map[string]any{
		"tool_name":         conf.ToolName,
		"args":              conf.Args,
		"confirmation_text": req.Text,
		"user_id":           userID,
		"session_id":        req.SessionID,
		"turn_id":           req.TurnID,
	})

	var replyText string
	reqConfirm, err := http.NewRequestWithContext(ctx, "POST", s.ToolService+"/confirm", bytes.NewBuffer(confirmReqBody))
	if err == nil {
		reqConfirm.Header.Set("Content-Type", "application/json")
		s.LogEvent(ctx, req.TurnID, "confirmation_execute", map[string]any{
			"turn_id": req.TurnID,
			"intent":  conf.Intent,
		})

		resp, err := s.HTTPClient.Do(reqConfirm)
		if err == nil {
			defer resp.Body.Close()
			var confirmRes struct {
				Confirmed    bool   `json:"confirmed"`
				ResponseText string `json:"response_text"`
			}
			if json.NewDecoder(resp.Body).Decode(&confirmRes) == nil {
				replyText = confirmRes.ResponseText
				s.LogEvent(ctx, req.TurnID, "confirmation_outcome", map[string]any{
					"turn_id":       req.TurnID,
					"intent":        conf.Intent,
					"confirmed":     confirmRes.Confirmed,
					"unique_ref_no": conf.UniqueRefNo,
				})
			} else {
				replyText = "Transaction confirmation failed."
			}
		} else {
			replyText = "Transaction confirmation failed."
		}
	} else {
		replyText = "Transaction confirmation failed."
	}

	// Log confirmation outcomes asynchronously
	go func() {
		histCtx := telemetry.WithTraceContext(context.Background(), req.SessionID, req.TurnID)

		// Call context-service to append turns
		contextAppendBody, _ := json.Marshal(map[string]any{
			"session_id":        req.SessionID,
			"user_message":      req.Text,
			"assistant_message": replyText,
		})
		reqCtx, err := http.NewRequestWithContext(histCtx, "POST", s.ContextService+"/save", bytes.NewBuffer(contextAppendBody))
		if err == nil {
			reqCtx.Header.Set("Content-Type", "application/json")
			resp, err := s.HTTPClient.Do(reqCtx)
			if err == nil {
				resp.Body.Close()
			}
		}

		s.LogConversationTurn(histCtx, userID, req.SessionID, "user", req.Text, "confirmation", "", "")
		s.LogConversationTurn(histCtx, userID, req.SessionID, "assistant", replyText, "confirmation", "", "")
	}()

	resp := map[string]any{
		"text": replyText,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *OrchestratorServer) handleBankData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "mock_user_123"
	}

	reqBank, err := http.NewRequestWithContext(ctx, "GET", s.ToolService+"/bank-data?user_id="+userID, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp, err := s.HTTPClient.Do(reqBank)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *OrchestratorServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	var payload ConfigPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.warmingEnabled = payload.WarmingEnabled
	s.extremeThreshold = payload.ExtremeThreshold
	s.normalThreshold = payload.NormalThreshold
	s.mu.Unlock()

	log.Printf("[Config Update] Warming: %t, Extreme: %.2f, Normal: %.2f",
		payload.WarmingEnabled, payload.ExtremeThreshold, payload.NormalThreshold)

	w.WriteHeader(http.StatusOK)
}

func (s *OrchestratorServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status := "healthy"
	details := make(map[string]string)

	// Check Redis
	if err := s.Redis.Client.Ping(ctx).Err(); err != nil {
		status = "unhealthy"
		details["redis"] = err.Error()
	} else {
		details["redis"] = "healthy"
	}

	// Check context service
	if err := s.pingService(ctx, s.ContextService); err != nil {
		status = "unhealthy"
		details["context-service"] = err.Error()
	} else {
		details["context-service"] = "healthy"
	}

	// Check cache service
	if err := s.pingService(ctx, s.CacheService); err != nil {
		status = "unhealthy"
		details["cache-service"] = err.Error()
	} else {
		details["cache-service"] = "healthy"
	}

	// Check inference service
	if err := s.pingService(ctx, s.InferenceService); err != nil {
		status = "unhealthy"
		details["inference-service"] = err.Error()
	} else {
		details["inference-service"] = "healthy"
	}

	// Check tool service
	if err := s.pingService(ctx, s.ToolService); err != nil {
		status = "unhealthy"
		details["tool-service"] = err.Error()
	} else {
		details["tool-service"] = "healthy"
	}

	respBody := map[string]any{
		"status":  status,
		"details": details,
	}

	w.Header().Set("Content-Type", "application/json")
	if status == "unhealthy" {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(respBody)
}

func (s *OrchestratorServer) pingService(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status code %d", resp.StatusCode)
	}
	return nil
}

func (s *OrchestratorServer) executeCommitPath(ctx context.Context, turnID string, sessionID string, userID string, actionPayload map[string]any, finalTranscript string, history []ollama.ChatMessage, writeChunk func(eventType string, text string)) (string, string) {
	intent, _ := actionPayload["intent"].(string)
	bankAction, _ := actionPayload["bank_action"].(string)

	if intent == "transfer" || intent == "block_card" {
		args := map[string]any{"user_id": userID}

		if intent == "transfer" {
			args["to"] = extractAccountNo(finalTranscript)
			args["amount"] = extractAmount(finalTranscript)
			args["unique_ref_no"] = fmt.Sprintf("REF-%s-%d", sessionID, time.Now().UnixNano())
		} else if intent == "block_card" {
			args["card"] = "credit"
			if strings.Contains(strings.ToLower(finalTranscript), "debit") {
				args["card"] = "debit"
			}
		}

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
			amountVal, _ := args["amount"].(float64)
			amountStr := fmt.Sprintf("%.2f", amountVal)
			if amountVal <= 0 {
				if isHindi {
					promptText = "पैसे ट्रांसफर करने के लिए, कृपया एक मान्य राशि और गंतव्य खाता संख्या बताएं। उदाहरण के लिए: 987654 खाते में 500 रुपये भेजें।"
				} else {
					promptText = "To transfer money, please specify a valid amount and destination account number. For example: transfer 500 to account 987654."
				}
				_ = s.Redis.Client.Del(ctx, fmt.Sprintf("session:%s:confirm", sessionID))
				return "text", promptText
			}

			if isHindi {
				promptText = fmt.Sprintf("कृपया पुष्टि करें: क्या आप खाता %s में %s रुपये ट्रांसफर करना चाहते हैं? (हाँ/ना)", args["to"], amountStr)
			} else {
				promptText = fmt.Sprintf("Please confirm: Do you want to transfer %s INR to account %s? (yes/no)", amountStr, args["to"])
			}
		} else {
			cardType, _ := args["card"].(string)
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

		return "confirm_required", promptText
	}

	// Read-only action execution
	args := map[string]any{
		"tool_name":  bankAction,
		"user_id":    userID,
		"session_id": sessionID,
		"turn_id":    turnID,
	}
	execReqBody, _ := json.Marshal(args)
	reqExec, err := http.NewRequestWithContext(ctx, "POST", s.ToolService+"/execute", bytes.NewBuffer(execReqBody))
	var mcpResText string
	if err == nil {
		reqExec.Header.Set("Content-Type", "application/json")
		resp, err := s.HTTPClient.Do(reqExec)
		if err == nil {
			defer resp.Body.Close()
			var toolRes audit.ToolExecutionResult
			if json.NewDecoder(resp.Body).Decode(&toolRes) == nil {
				mcpResText = toolRes.ResponseText
			}
		}
	}
	if mcpResText == "" {
		return "text", "I'm sorry, I encountered an issue retrieving your bank details. Let me connect you with a representative."
	}

	// Format response
	formatReqBody, _ := json.Marshal(map[string]any{
		"query":      finalTranscript,
		"mcp_result": mcpResText,
		"stream":     false,
	})
	reqFormat, err := http.NewRequestWithContext(ctx, "POST", s.InferenceService+"/format", bytes.NewBuffer(formatReqBody))
	var formattedText string
	if err == nil {
		reqFormat.Header.Set("Content-Type", "application/json")
		resp, err := s.HTTPClient.Do(reqFormat)
		if err == nil {
			defer resp.Body.Close()
			var formatRes struct {
				Text string `json:"text"`
			}
			if json.NewDecoder(resp.Body).Decode(&formatRes) == nil {
				formattedText = formatRes.Text
			}
		}
	}
	if formattedText == "" {
		formattedText = mcpResText
	}

	historyStr := s.SerializeHistory(history)
	safeText := s.ApplyOutputGuardrailFilter(formattedText, mcpResText+" "+historyStr)

	return "text", safeText
}

func (s *OrchestratorServer) LogConversationTurn(ctx context.Context, userID, sessionID, role, transcript, intent, action, result string) {
	seqKey := fmt.Sprintf("session:%s:seq", sessionID)
	seq, err := s.Redis.Client.Incr(ctx, seqKey).Result()
	if err != nil {
		log.Printf("[Redis log] Warning: failed to increment sequence for session %s: %v", sessionID, err)
		seq = 1
	}

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

func (s *OrchestratorServer) LogEvent(ctx context.Context, turnID string, eventName string, payload map[string]any) {
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
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Redis.AddAuditLog(bgCtx, turnID, eventName, payload)
	}()
}

func (s *OrchestratorServer) ApplyOutputGuardrailFilter(responseText string, trustedSourceData string) string {
	rePinCvv := regexp.MustCompile(`(?i)\b(pin|cvv|cvc|otp|password|passcode)\b.*?(\b\d{3,6}\b)`)
	if rePinCvv.MatchString(responseText) {
		log.Printf("[SECURITY] Suppressed response due to sensitive credentials (PIN/CVV/OTP) leak risk")
		return "For security reasons, I cannot disclose or discuss PINs, CVVs, or passwords over this line."
	}

	reCard := regexp.MustCompile(`\b(?:\d{4}[- ]?){3}(\d{4})\b`)
	if reCard.MatchString(responseText) {
		responseText = reCard.ReplaceAllString(responseText, "XXXX-XXXX-XXXX-$1")
	}

	reLongNum := regexp.MustCompile(`\b\d{9,15}(\d{4})\b`)
	if reLongNum.MatchString(responseText) {
		responseText = reLongNum.ReplaceAllString(responseText, "******$1")
	}

	trustedFloatsList := extractAllFloats(trustedSourceData)
	trustedFloats := make(map[float64]bool)
	for _, val := range trustedFloatsList {
		trustedFloats[val] = true
	}

	responseFloats := extractAllFloats(responseText)

	if len(responseFloats) == 0 {
		return responseText
	}

	for _, val := range responseFloats {
		intVal := int(val)
		if val == float64(intVal) && intVal >= 0 && intVal <= 10 {
			continue
		}
		if val == 24 || val == 7 || val == 30 || val == 60 || val == 365 || val == 18 {
			continue
		}

		if !trustedFloats[val] {
			log.Printf("[GUARDRAIL FILTER TRIP] Suppressed response due to unverified numerical value: %f", val)
			return "I'm sorry, I don't have that specific information right now. Let me connect you with a representative who can look that up for you."
		}
	}

	return responseText
}

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

func extractAmount(text string) float64 {
	re := regexp.MustCompile(`(?i)(?:transfer|send|wire)?\s*(\d+(?:\.\d+)?)\s*(?:inr|rupees|dollars|bucks)?`)
	matches := re.FindStringSubmatch(text)
	if len(matches) > 1 {
		val, err := strconv.ParseFloat(matches[1], 64)
		if err == nil {
			return val
		}
	}
	re2 := regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	numMatch := re2.FindString(text)
	if numMatch != "" {
		val, _ := strconv.ParseFloat(numMatch, 64)
		return val
	}
	return 0.0
}

func extractAccountNo(text string) string {
	re := regexp.MustCompile(`\b\d{5,12}\b`)
	match := re.FindString(text)
	if match != "" {
		return match
	}
	return "9876543210"
}

func isHindiText(text string) bool {
	for _, r := range text {
		if r >= 0x0900 && r <= 0x097F {
			return true
		}
	}
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

func (s *OrchestratorServer) SerializeHistory(messages []ollama.ChatMessage) string {
	var builder strings.Builder
	for _, msg := range messages {
		builder.WriteString(msg.Content)
		builder.WriteString(" ")
	}
	return builder.String()
}

func cleanJSONResponse(input string) string {
	input = strings.TrimSpace(input)
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

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		duration := time.Since(start)
		logRecord := telemetry.StructuredLog{
			Timestamp:   time.Now(),
			Level:       "INFO",
			Message:     "HTTP request completed",
			Logger:      "http",
			Duration:    duration.String(),
			DurationMS:  float64(duration.Nanoseconds()) / 1e6,
			DBSystem:    r.Method,
			DBOperation: r.URL.Path,
		}
		telemetry.Logger("http").InfoContext(r.Context(), "http_request", logRecord.SlogArgs()...)
	})
}
