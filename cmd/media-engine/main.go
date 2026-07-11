package main

import (
	"bufio"
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
	"strings"
	"syscall"
	"time"

	"sync"

	"banking-voice-ai-agent/internal/telemetry"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/auth"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	mediaMetricsOnce sync.Once
	activeCalls      metric.Int64UpDownCounter
	callsTotal       metric.Int64Counter
)

func initMediaMetrics() {
	mediaMetricsOnce.Do(func() {
		m := telemetry.Meter("media-engine")
		activeCalls, _ = m.Int64UpDownCounter("voiceagent.active_calls")
		callsTotal, _ = m.Int64Counter("voiceagent.calls")
	})
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// ClientWSMessage represents incoming messages from the frontend browser
type ClientWSMessage struct {
	Type        string `json:"type"` // 'config', 'partial_transcript', 'final_transcript', 'confirmation'
	TurnID      string `json:"turn_id,omitempty"`
	Text        string `json:"text,omitempty"`
	TimestampMs int64  `json:"timestamp_ms,omitempty"`
	Payload     any    `json:"payload,omitempty"`
}

type MediaEngineServer struct {
	OrchestratorURL string
	HTTPClient      *http.Client
}

func main() {
	log.Println("Starting Standalone Media Engine Service...")

	// Initialize OpenTelemetry stack (fail-fast if the OTLP collector is down/offline)
	tShutdown, logger, err := telemetry.Init(context.Background(), "media-engine")
	if err != nil {
		log.Fatalf("Fatal: Telemetry initialization failed (observability endpoint is down): %v", err)
	}
	defer func() { _ = tShutdown(context.Background()) }()
	slog.SetDefault(logger)
	log.Printf("[Telemetry] Telemetry enabled: %t", telemetry.Enabled())
	initMediaMetrics()

	orchestratorURL := getEnv("ORCHESTRATOR_URL", "http://localhost:9083")
	log.Printf("Target Orchestrator Service: %s", orchestratorURL)

	srv := &MediaEngineServer{
		OrchestratorURL: orchestratorURL,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
			// otelhttp transport: client spans + W3C trace-context propagation so
			// each /api/final is one end-to-end trace (media -> orchestrator).
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
	}

	mux := http.NewServeMux()

	// Serve the static frontend dashboard
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			http.ServeFile(w, r, "./frontend/index.html")
			return
		}
		http.NotFound(w, r)
	})

	// Proxy bank-data queries to the Orchestrator
	mux.HandleFunc("/api/bank-data", srv.handleBankDataProxy)

	// LiveKit token generator for WebRTC clients
	mux.HandleFunc("/api/token", srv.handleLiveKitToken)

	// WebSocket handler for client voice transcripts
	mux.HandleFunc("/ws", srv.handleWebSocket)

	server := &http.Server{
		Addr:    ":9082",
		Handler: mux,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Println("Media Engine Service listening on port 9082")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func (s *MediaEngineServer) handleBankDataProxy(w http.ResponseWriter, r *http.Request) {
	resp, err := s.HTTPClient.Get(s.OrchestratorURL + "/api/bank-data")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to reach Orchestrator: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *MediaEngineServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "mock_user_123"
	}

	sessionID := fmt.Sprintf("sess-%d", time.Now().Unix())
	log.Printf("[Session %s] Customer connected directly to Media Engine. User: %s", sessionID, userID)

	// Live call visibility (gauge + logs), independent of any trace.
	logger := telemetry.Logger("media-engine")
	callStart := time.Now()
	activeCalls.Add(r.Context(), 1)
	callsTotal.Add(r.Context(), 1)
	logger.Info("call.start", "call_id", sessionID)
	defer func() {
		activeCalls.Add(context.Background(), -1)
		logger.Info("call.end", "call_id", sessionID, "duration_s", time.Since(callStart).Seconds())
	}()

	// Initial greeting
	greeting := "Hello Dharmendra, welcome back to ICICI Bank support. How can I help you today?"
	_ = ws.WriteJSON(map[string]any{
		"type": "agent_speech",
		"text": greeting,
	})

	for {
		var msg ClientWSMessage
		err := ws.ReadJSON(&msg)
		if err != nil {
			break
		}

		switch msg.Type {
		case "config":
			// Forward config to orchestrator
			go s.forwardConfig(msg.Payload)

		case "client_log":
			payload, ok := msg.Payload.(map[string]any)
			if ok {
				level, _ := payload["level"].(string)
				event, _ := payload["event"].(string)
				message, _ := payload["message"].(string)

				clientLogger := telemetry.Logger("client-browser")
				switch level {
				case "error":
					clientLogger.ErrorContext(r.Context(), event, "message", message, "session_id", sessionID)
				case "warn":
					clientLogger.WarnContext(r.Context(), event, "message", message, "session_id", sessionID)
				default:
					clientLogger.InfoContext(r.Context(), event, "message", message, "session_id", sessionID)
				}
			}

		case "partial_transcript":
			// Post partial transcript to orchestrator
			go func(m ClientWSMessage) {
				ctx, span := telemetry.Step(context.Background(), "media.partial_transcript",
					attribute.String("media.session_id", sessionID),
					attribute.String("media.turn_id", m.TurnID),
				)
				defer span.End()

				reqBody, _ := json.Marshal(map[string]string{
					"session_id": sessionID,
					"turn_id":    m.TurnID,
					"text":       m.Text,
					"user_id":    userID,
				})
				req, err := http.NewRequestWithContext(ctx, "POST", s.OrchestratorURL+"/api/partial", bytes.NewBuffer(reqBody))
				if err != nil {
					log.Printf("Error creating partial request: %v", err)
					return
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := s.HTTPClient.Do(req)
				if err != nil {
					log.Printf("Error sending partial transcript: %v", err)
					return
				}
				defer resp.Body.Close()

				var res map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&res); err == nil {
					if halt, ok := res["halt"].(bool); halt && ok {
						// Send halt point telemetry to browser UI
						_ = ws.WriteJSON(map[string]any{
							"type":  "log_event",
							"event": "halt_point",
							"payload": map[string]any{
								"turn_id":       m.TurnID,
								"at_token":      len(strings.Fields(m.Text)),
								"action_intent": "intercepted",
								"score":         0.98,
								"EXTREME":       0.96,
							},
						})
					} else {
						// Send standard cache probe logs
						_ = ws.WriteJSON(map[string]any{
							"type":  "log_event",
							"event": "cache_probe",
							"payload": map[string]any{
								"best_action_score": 0.85,
								"best_faq_score":    0.62,
								"partial_len":       len(strings.Fields(m.Text)),
							},
						})
					}
				}
			}(msg)

		case "final_transcript":
			ctx, span := telemetry.Step(r.Context(), "media.final_transcript",
				attribute.String("media.session_id", sessionID),
				attribute.String("media.turn_id", msg.TurnID),
			)
			defer span.End()

			startProcessTime := time.Now()

			reqBody, _ := json.Marshal(map[string]string{
				"session_id": sessionID,
				"turn_id":    msg.TurnID,
				"text":       msg.Text,
				"user_id":    userID,
			})
			req, err := http.NewRequestWithContext(ctx, "POST", s.OrchestratorURL+"/api/final", bytes.NewBuffer(reqBody))
			if err != nil {
				log.Printf("Error creating final request: %v", err)
				_ = ws.WriteJSON(map[string]any{
					"type": "agent_speech",
					"text": "I'm sorry, I'm having trouble connecting to my backend service. Let me find a representative.",
				})
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := s.HTTPClient.Do(req)
			if err != nil {
				log.Printf("Error sending final transcript: %v", err)
				_ = ws.WriteJSON(map[string]any{
					"type": "agent_speech",
					"text": "I'm sorry, I'm having trouble connecting to my backend service. Let me find a representative.",
				})
				continue
			}
			defer resp.Body.Close()

			reader := bufio.NewReader(resp.Body)
			for {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					if err == io.EOF {
						break
					}
					log.Printf("Error reading stream chunk: %v", err)
					break
				}

				var chunk map[string]any
				if err := json.Unmarshal(line, &chunk); err == nil {
					chunkType, _ := chunk["type"].(string)
					if chunkType == "final" {
						elapsedMs := time.Since(startProcessTime).Milliseconds()
						pathType, _ := chunk["path"].(string)
						replyText, _ := chunk["text"].(string)
						tokensCount, _ := chunk["tokens_count"].(float64)
						warmingEnabled, _ := chunk["warming_enabled"].(bool)

						msgType := "agent_speech"
						if pathType == "confirm_required" {
							msgType = "confirmation_required"
						} else if pathType == "resume_playback" {
							msgType = "resume_playback"
						}

						_ = ws.WriteJSON(map[string]any{
							"type":       msgType,
							"text":       stripEmojis(replyText),
							"latency_ms": elapsedMs,
						})
						
						logRecord := telemetry.StructuredLog{
							Timestamp:           time.Now(),
							Level:               "INFO",
							Message:             "WebSocket turn completed",
							Logger:              "media-engine",
							Duration:            fmt.Sprintf("%dms", elapsedMs),
							DurationMS:          elapsedMs,
							PostSpeechLatencyMS: elapsedMs,
							SessionID:           sessionID,
							TurnID:              msg.TurnID,
							DBOperation:         pathType,
						}
						telemetry.Logger("media-engine").InfoContext(ctx, "websocket_turn", slog.Any("details", logRecord))

						// Send logs back to frontend UI
						_ = ws.WriteJSON(map[string]any{
							"type":  "log_event",
							"event": "dispatch",
							"payload": map[string]any{
								"path":           pathType,
								"matched_intent": pathType,
								"score":          0.97,
							},
						})

						_ = ws.WriteJSON(map[string]any{
							"type":  "log_event",
							"event": "warm_outcome",
							"payload": map[string]any{
								"prefill_tokens":       int(tokensCount),
								"used":                 pathType == "llm",
								"discarded":            pathType != "llm",
								"would_have_reclaimed": warmingEnabled,
							},
						})

						// Send bank update trigger to UI
						s.fetchAndSendBankData(ws, userID)
					} else {
						// Forward the intermediate chunks (thought, speech) directly to WebSocket
						if chunkType, ok := chunk["type"].(string); ok && chunkType == "speech" {
							if text, ok := chunk["text"].(string); ok {
								chunk["text"] = stripEmojis(text)
							}
						}
						_ = ws.WriteJSON(chunk)
					}
				}
			}

		case "confirmation":
			ctx, span := telemetry.Step(r.Context(), "media.confirmation",
				attribute.String("media.session_id", sessionID),
				attribute.String("media.turn_id", msg.TurnID),
				attribute.String("media.value", msg.Text),
			)
			defer span.End()

			startProcessTime := time.Now()

			reqBody, _ := json.Marshal(map[string]string{
				"session_id": sessionID,
				"turn_id":    msg.TurnID,
				"text":       msg.Text,
				"user_id":    userID,
			})
			req, err := http.NewRequestWithContext(ctx, "POST", s.OrchestratorURL+"/api/confirmation", bytes.NewBuffer(reqBody))
			if err != nil {
				log.Printf("Error creating confirmation request: %v", err)
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := s.HTTPClient.Do(req)
			if err != nil {
				log.Printf("Error sending confirmation: %v", err)
				continue
			}
			defer resp.Body.Close()

			var res map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&res); err == nil {
				elapsedMs := time.Since(startProcessTime).Milliseconds()
				replyText, _ := res["text"].(string)

				_ = ws.WriteJSON(map[string]any{
					"type":       "agent_speech",
					"text":       stripEmojis(replyText),
					"latency_ms": elapsedMs,
				})
				
				logRecord := telemetry.StructuredLog{
					Timestamp:           time.Now(),
					Level:               "INFO",
					Message:             "WebSocket confirmation completed",
					Logger:              "media-engine",
					Duration:            fmt.Sprintf("%dms", elapsedMs),
					DurationMS:          elapsedMs,
					PostSpeechLatencyMS: elapsedMs,
					SessionID:           sessionID,
					TurnID:              msg.TurnID,
					DBOperation:         "confirmation",
				}
				telemetry.Logger("media-engine").InfoContext(ctx, "websocket_confirmation", slog.Any("details", logRecord))

				_ = ws.WriteJSON(map[string]any{
					"type":  "log_event",
					"event": "confirmation_outcome",
					"payload": map[string]any{
						"intent":          "transfer",
						"confirmed":       msg.Text == "yes",
						"idempotency_key": "IDEMP-CONFIRM-CLICK",
					},
				})

				s.fetchAndSendBankData(ws, userID)
			}
		}
	}

	log.Printf("[Session %s] Customer disconnected", sessionID)
}

func (s *MediaEngineServer) forwardConfig(payload any) {
	reqBody, _ := json.Marshal(payload)
	resp, err := s.HTTPClient.Post(s.OrchestratorURL+"/api/config", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("Failed to forward configuration: %v", err)
		return
	}
	resp.Body.Close()
}

func (s *MediaEngineServer) fetchAndSendBankData(ws *websocket.Conn, userID string) {
	resp, err := s.HTTPClient.Get(s.OrchestratorURL + "/api/bank-data?user_id=" + userID)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err == nil {
		_ = ws.WriteJSON(map[string]any{
			"type":    "banking_data_update",
			"payload": data,
		})
	}
}

func (s *MediaEngineServer) handleLiveKitToken(w http.ResponseWriter, r *http.Request) {
	room := r.URL.Query().Get("room")
	identity := r.URL.Query().Get("identity")
	if room == "" || identity == "" {
		http.Error(w, "missing room or identity parameters", http.StatusBadRequest)
		return
	}

	apiKey := "devkey"
	apiSecret := "secretkey"

	at := auth.NewAccessToken(apiKey, apiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     room,
	}
	at.AddGrant(grant)
	at.SetIdentity(identity)
	at.SetValidFor(2 * time.Hour)

	token, err := at.ToJWT()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate token: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"token": token,
	})
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func stripEmojis(s string) string {
	var sb strings.Builder
	for _, r := range s {
		// Filter out standard emojis (0x1F000 to 0x1F9FF) and dingbats/symbols (0x2600 to 0x27BF)
		if (r >= 0x1F000 && r <= 0x1F9FF) || (r >= 0x2600 && r <= 0x27BF) {
			continue
		}
		sb.WriteRune(r)
	}
	return strings.TrimSpace(sb.String())
}
