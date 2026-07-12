package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/mcp"
	"banking-voice-ai-agent/internal/ollama"
	"banking-voice-ai-agent/internal/llm-orchestrator-server"
	"banking-voice-ai-agent/internal/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type OrchestratorServer struct {
	Mongo      *db.MongoManager
	Redis      *db.RedisManager
	Qdrant     *db.QdrantManager
	Cassandra  *db.CassandraManager
	Ollama     *ollama.Client
	MCP        *mcp.BankingMCPServer
	Supervisor *llmorchestrator.TurnSupervisor

	// Session states for warming cancellation
	mu            sync.Mutex
	warmingCancel map[string]context.CancelFunc
}

func main() {
	log.Println("Starting Standalone LLM Orchestrator Server...")

	// Initialize OpenTelemetry stack (fail-fast if the OTLP collector is down/offline)
	tShutdown, logger, err := telemetry.Init(context.Background(), "llm-orchestrator-server")
	if err != nil {
		log.Fatalf("Fatal: Telemetry initialization failed (observability endpoint is down): %v", err)
	}
	defer func() { _ = tShutdown(context.Background()) }()
	slog.SetDefault(logger)
	log.Printf("[Telemetry] Telemetry enabled: %t", telemetry.Enabled())

	mongoURI := getEnv("MONGO_URI", "mongodb://localhost:27017")
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	qdrantURL := getEnv("QDRANT_URL", "http://localhost:6333")
	cassandraHostsStr := getEnv("CASSANDRA_HOSTS", "localhost")
	cassandraHosts := strings.Split(cassandraHostsStr, ",")
	ollamaURL := getEnv("OLLAMA_URL", "http://localhost:11434")
	chatModel := getEnv("CHAT_MODEL", "qwen2.5:7b-instruct")
	embedModel := getEnv("EMBED_MODEL", "bge-m3")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	redisMgr, err := db.NewRedisManager(redisAddr)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Redis: %v", err)
	}

	mongoMgr, err := db.NewMongoManager(mongoURI)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to MongoDB: %v", err)
	}

	qdrantMgr, err := db.NewQdrantManager(qdrantURL)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Qdrant: %v", err)
	}

	cassandraMgr, err := db.NewCassandraManager(cassandraHosts)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Cassandra: %v", err)
	}
	defer cassandraMgr.Close()

	ollamaClient := ollama.NewClient(ollamaURL, chatModel, embedModel)
	if err := qdrantMgr.SeedData(ctx, ollamaClient); err != nil {
		log.Printf("Qdrant seeding notice: %v", err)
	}

	mcpServer := mcp.NewBankingMCPServer(mongoMgr)
	supervisor := llmorchestrator.NewTurnSupervisor(redisMgr, qdrantMgr, ollamaClient, mcpServer, cassandraMgr)

	// Start background consumers for conversation history and audit log
	consumerCtx, consumerCancel := context.WithCancel(context.Background())
	defer consumerCancel()
	llmorchestrator.StartHistoryConsumer(consumerCtx, redisMgr, cassandraMgr)
	llmorchestrator.StartAuditConsumer(consumerCtx, redisMgr, cassandraMgr)

	srv := &OrchestratorServer{
		Mongo:         mongoMgr,
		Redis:         redisMgr,
		Qdrant:        qdrantMgr,
		Cassandra:     cassandraMgr,
		Ollama:        ollamaClient,
		MCP:           mcpServer,
		Supervisor:    supervisor,
		warmingCancel: make(map[string]context.CancelFunc),
	}

	healthCtx, healthCancel := context.WithCancel(context.Background())
	defer healthCancel()
	StartOllamaHealthCheck(healthCtx, ollamaURL)

	mux := http.NewServeMux()
	mux.Handle("/api/partial", otelhttp.NewHandler(http.HandlerFunc(srv.handlePartial), "partial"))
	mux.Handle("/api/final", otelhttp.NewHandler(http.HandlerFunc(srv.handleFinal), "final"))
	mux.Handle("/api/confirmation", otelhttp.NewHandler(http.HandlerFunc(srv.handleConfirmation), "confirmation"))
	mux.Handle("/api/bank-data", otelhttp.NewHandler(http.HandlerFunc(srv.handleBankData), "bank-data"))
	mux.Handle("/api/config", otelhttp.NewHandler(http.HandlerFunc(srv.handleConfig), "config"))
	mux.HandleFunc("/healthz", srv.handleHealthz)

	server := &http.Server{
		Addr:    ":9083",
		Handler: withLogging(mux),
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down LLM Orchestrator Server...")
		consumerCancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Println("LLM Orchestrator Server listening on port 9083")
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

type ConfigPayload struct {
	WarmingEnabled   bool    `json:"warming_enabled"`
	ExtremeThreshold float64 `json:"extreme_threshold"`
	NormalThreshold  float64 `json:"normal_threshold"`
}

func (s *OrchestratorServer) handlePartial(w http.ResponseWriter, r *http.Request) {
	var req PartialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := telemetry.WithTraceContext(r.Context(), req.SessionID, req.TurnID)

	s.mu.Lock()
	if oldCancel, ok := s.warmingCancel[req.SessionID]; ok {
		oldCancel()
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	s.warmingCancel[req.SessionID] = cancel
	s.mu.Unlock()

	warmDone := make(chan struct{})

	// Branch A: speculative prefill
	go func(wCtx context.Context, done chan struct{}, text string) {
		defer close(done)
		if s.Supervisor.IsWarmingEnabled() {
			messages := []ollama.ChatMessage{{Role: "user", Content: text}}
			_, _ = s.Ollama.Chat(wCtx, messages, false, nil)
		}
	}(cancelCtx, warmDone, req.Text)

	// Branch B: cache probe
	isHalted, matchedAction := s.Supervisor.HandleStablePartial(cancelCtx, req.TurnID, req.SessionID, req.Text, cancel, warmDone)

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

	s.mu.Lock()
	if cancel, ok := s.warmingCancel[req.SessionID]; ok {
		cancel()
		delete(s.warmingCancel, req.SessionID)
	}
	s.mu.Unlock()

	// Check if this session is awaiting transaction confirmation
	confirmKey := fmt.Sprintf("session:%s:confirm", req.SessionID)
	hasPendingConfirm, _ := s.Redis.Client.Exists(ctx, confirmKey).Result()

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

	var pathType, replyText string
	var err error
	userID := req.UserID
	if userID == "" {
		userID = "mock_user_123"
	}

	if hasPendingConfirm > 0 {
		pathType = "confirmation"
		replyText, err = s.Supervisor.HandleConfirmation(ctx, req.TurnID, req.SessionID, userID, req.Text)
	} else {
		pathType, replyText, err = s.Supervisor.HandleFinalTranscript(ctx, req.TurnID, req.SessionID, userID, req.Text, false, nil, func(eventType string, text string) {
			writeChunk(eventType, text)
		})
	}

	if err != nil {
		log.Printf("Error processing turn final: %v", err)
		replyText = "I'm sorry, I ran into an error processing your query. Please connect with customer support."
	}

	// Update conversation context history and log to Cassandra
	go func() {
		histCtx := telemetry.WithTraceContext(context.Background(), req.SessionID, req.TurnID)
		_, _ = s.Supervisor.ContextManager.AppendAndSave(histCtx, req.SessionID, req.Text, replyText)

		// Log to Cassandra durable event history store
		s.Supervisor.LogConversationTurn(histCtx, userID, req.SessionID, "user", req.Text, "", "", "")
		s.Supervisor.LogConversationTurn(histCtx, userID, req.SessionID, "assistant", replyText, pathType, "", "")
	}()

	resp := map[string]any{
		"type":            "final",
		"path":            pathType,
		"text":            replyText,
		"tokens_count":    len(strings.Fields(req.Text)),
		"warming_enabled": s.Supervisor.IsWarmingEnabled(),
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

	userID := req.UserID
	if userID == "" {
		userID = "mock_user_123"
	}
	replyText, err := s.Supervisor.HandleConfirmation(ctx, req.TurnID, req.SessionID, userID, req.Text)
	if err != nil {
		replyText = "Transaction confirmation failed."
	}
	// Log confirmation outcomes to Cassandra
	go func() {
		histCtx := telemetry.WithTraceContext(context.Background(), req.SessionID, req.TurnID)
		s.Supervisor.LogConversationTurn(histCtx, userID, req.SessionID, "user", req.Text, "confirmation", "", "")
		s.Supervisor.LogConversationTurn(histCtx, userID, req.SessionID, "assistant", replyText, "confirmation", "", "")
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

	balance, currency, err := s.Mongo.GetBalance(ctx, userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	txs, err := s.Mongo.GetTransactions(ctx, userID, 5)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	dueDate, err := s.Mongo.GetDueDate(ctx, userID, "credit")
	if err != nil {
		dueDate = "N/A"
	}

	cardStatus := "active"
	var card db.Card
	if err := s.Mongo.DB.Collection("cards").FindOne(ctx, map[string]string{"user_id": userID, "card_type": "credit"}).Decode(&card); err == nil {
		cardStatus = card.Status
	}

	var txList []map[string]any
	for _, t := range txs {
		txList = append(txList, map[string]any{
			"description": t.Description,
			"amount":      t.Amount,
			"date":        t.Date.Format("2006-01-02 15:04"),
		})
	}

	res := map[string]any{
		"balance":       balance,
		"currency":      currency,
		"card_status":   cardStatus,
		"card_due_date": dueDate,
		"transactions":  txList,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *OrchestratorServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	var payload ConfigPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.Supervisor.UpdateConfig(payload.WarmingEnabled, payload.ExtremeThreshold, payload.NormalThreshold)
	log.Printf("[Config Update] Warming: %t, Extreme: %.2f, Normal: %.2f",
		payload.WarmingEnabled, payload.ExtremeThreshold, payload.NormalThreshold)

	w.WriteHeader(http.StatusOK)
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
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

// StartOllamaHealthCheck starts a background routine that pings Ollama every 15 seconds.
func StartOllamaHealthCheck(ctx context.Context, ollamaURL string) {
	logger := telemetry.Logger("voice-ai-ollama")
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(15 * time.Second)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				start := time.Now()
				resp, err := client.Get(ollamaURL + "/")
				duration := time.Since(start)
				durationMS := float64(duration.Nanoseconds()) / 1e6

				logRecord := telemetry.StructuredLog{
					Timestamp:  time.Now(),
					Level:      "INFO",
					Logger:     "voice-ai-ollama",
					Duration:   duration.String(),
					DurationMS: durationMS,
				}

				if err != nil || resp.StatusCode != http.StatusOK {
					logRecord.Level = "ERROR"
					if err != nil {
						logRecord.Message = fmt.Sprintf("Ollama connection failed: %v", err)
					} else {
						resp.Body.Close()
						logRecord.Message = fmt.Sprintf("Ollama connection failed: status code %d", resp.StatusCode)
					}
					logger.ErrorContext(ctx, logRecord.Message, logRecord.SlogArgs()...)
				} else {
					resp.Body.Close()
					logRecord.Message = "Ollama is healthy"
					logger.InfoContext(ctx, logRecord.Message, logRecord.SlogArgs()...)
				}
			}
		}
	}()
}

// handleHealthz handles health check requests checking Redis, MongoDB, Cassandra, and Ollama.
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

	// Check MongoDB
	if err := s.Mongo.Client.Ping(ctx, nil); err != nil {
		status = "unhealthy"
		details["mongodb"] = err.Error()
	} else {
		details["mongodb"] = "healthy"
	}

	// Check Cassandra
	if err := s.Cassandra.Session.Query("SELECT release_version FROM system.local").WithContext(ctx).Exec(); err != nil {
		status = "unhealthy"
		details["cassandra"] = err.Error()
	} else {
		details["cassandra"] = "healthy"
	}

	// Check Ollama
	req, err := http.NewRequestWithContext(ctx, "GET", s.Ollama.BaseURL+"/", nil)
	if err != nil {
		status = "unhealthy"
		details["ollama"] = err.Error()
	} else {
		resp, err := s.Ollama.HTTPClient.Do(req)
		if err != nil {
			status = "unhealthy"
			details["ollama"] = err.Error()
		} else {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				status = "unhealthy"
				details["ollama"] = fmt.Sprintf("status code %d", resp.StatusCode)
			} else {
				details["ollama"] = "healthy"
			}
		}
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
