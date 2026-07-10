package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

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

	orchestratorURL := getEnv("ORCHESTRATOR_URL", "http://localhost:9083")
	log.Printf("Target Orchestrator Service: %s", orchestratorURL)

	srv := &MediaEngineServer{
		OrchestratorURL: orchestratorURL,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
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

	sessionID := fmt.Sprintf("sess-%d", time.Now().Unix())
	log.Printf("[Session %s] Customer connected directly to Media Engine", sessionID)

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

		case "partial_transcript":
			// Post partial transcript to orchestrator
			go func(m ClientWSMessage) {
				reqBody, _ := json.Marshal(map[string]string{
					"session_id": sessionID,
					"turn_id":     m.TurnID,
					"text":        m.Text,
				})
				resp, err := s.HTTPClient.Post(s.OrchestratorURL+"/api/partial", "application/json", bytes.NewBuffer(reqBody))
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
			startProcessTime := time.Now()

			reqBody, _ := json.Marshal(map[string]string{
				"session_id": sessionID,
				"turn_id":     msg.TurnID,
				"text":        msg.Text,
			})
			resp, err := s.HTTPClient.Post(s.OrchestratorURL+"/api/final", "application/json", bytes.NewBuffer(reqBody))
			if err != nil {
				log.Printf("Error sending final transcript: %v", err)
				_ = ws.WriteJSON(map[string]any{
					"type": "agent_speech",
					"text": "I'm sorry, I'm having trouble connecting to my backend service. Let me find a representative.",
				})
				continue
			}
			defer resp.Body.Close()

			var res map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&res); err == nil {
				elapsedMs := time.Since(startProcessTime).Milliseconds()

				pathType, _ := res["path"].(string)
				replyText, _ := res["text"].(string)
				tokensCount, _ := res["tokens_count"].(float64)
				warmingEnabled, _ := res["warming_enabled"].(bool)

				msgType := "agent_speech"
				if pathType == "confirm_required" {
					msgType = "confirmation_required"
				}

				_ = ws.WriteJSON(map[string]any{
					"type":       msgType,
					"text":       replyText,
					"latency_ms": elapsedMs,
				})

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
				s.fetchAndSendBankData(ws)
			}

		case "confirmation":
			startProcessTime := time.Now()

			reqBody, _ := json.Marshal(map[string]string{
				"session_id": sessionID,
				"turn_id":     msg.TurnID,
				"text":        msg.Text,
			})
			resp, err := s.HTTPClient.Post(s.OrchestratorURL+"/api/confirmation", "application/json", bytes.NewBuffer(reqBody))
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
					"text":       replyText,
					"latency_ms": elapsedMs,
				})

				_ = ws.WriteJSON(map[string]any{
					"type":  "log_event",
					"event": "confirmation_outcome",
					"payload": map[string]any{
						"intent":          "transfer",
						"confirmed":       msg.Text == "yes",
						"idempotency_key": "IDEMP-CONFIRM-CLICK",
					},
				})

				s.fetchAndSendBankData(ws)
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

func (s *MediaEngineServer) fetchAndSendBankData(ws *websocket.Conn) {
	resp, err := s.HTTPClient.Get(s.OrchestratorURL + "/api/bank-data")
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

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
