package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

	mongoURI := getEnv("MONGO_URI", "mongodb://localhost:27017")
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	qdrantURL := getEnv("QDRANT_URL", "http://localhost:6333")
	cassandraHostsStr := getEnv("CASSANDRA_HOSTS", "localhost")
	cassandraHosts := strings.Split(cassandraHostsStr, ",")
	ollamaURL := getEnv("OLLAMA_URL", "http://localhost:11434")
	chatModel := getEnv("CHAT_MODEL", "gemma2:2b")
	embedModel := getEnv("EMBED_MODEL", "nomic-embed-text:latest")

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

	mux := http.NewServeMux()
	mux.HandleFunc("/api/partial", srv.handlePartial)
	mux.HandleFunc("/api/final", srv.handleFinal)
	mux.HandleFunc("/api/confirmation", srv.handleConfirmation)
	mux.HandleFunc("/api/bank-data", srv.handleBankData)
	mux.HandleFunc("/api/config", srv.handleConfig)

	server := &http.Server{
		Addr:    ":9083",
		Handler: mux,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down LLM Orchestrator Server...")
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
}

type ConfirmationRequest struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Text      string `json:"text"`
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

	s.mu.Lock()
	if oldCancel, ok := s.warmingCancel[req.SessionID]; ok {
		oldCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	}(ctx, warmDone, req.Text)

	// Branch B: cache probe
	isHalted, matchedAction := s.Supervisor.HandleStablePartial(context.Background(), req.TurnID, req.SessionID, req.Text, cancel, warmDone)

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

	s.mu.Lock()
	if cancel, ok := s.warmingCancel[req.SessionID]; ok {
		cancel()
		delete(s.warmingCancel, req.SessionID)
	}
	s.mu.Unlock()

	// Check if this session is awaiting transaction confirmation
	confirmKey := fmt.Sprintf("session:%s:confirm", req.SessionID)
	hasPendingConfirm, _ := s.Redis.Client.Exists(r.Context(), confirmKey).Result()

	var pathType, replyText string
	var err error
	userID := "mock_user_123"

	if hasPendingConfirm > 0 {
		pathType = "confirmation"
		replyText, err = s.Supervisor.HandleConfirmation(r.Context(), req.TurnID, req.SessionID, userID, req.Text)
	} else {
		pathType, replyText, err = s.Supervisor.HandleFinalTranscript(r.Context(), req.TurnID, req.SessionID, userID, req.Text, false, nil)
	}

	if err != nil {
		log.Printf("Error processing turn final: %v", err)
		replyText = "I'm sorry, I ran into an error processing your query. Please connect with customer support."
	}

	// Update conversation context history and log to Cassandra
	go func() {
		histCtx := context.Background()
		hist, _ := s.Redis.GetSessionContext(histCtx, req.SessionID)
		hist = append(hist, ollama.ChatMessage{Role: "user", Content: req.Text})
		hist = append(hist, ollama.ChatMessage{Role: "assistant", Content: replyText})
		_ = s.Redis.SaveSessionContext(histCtx, req.SessionID, hist)

		// Log to Cassandra durable event history store
		s.Supervisor.LogConversationTurn(histCtx, userID, req.SessionID, "user", req.Text, "", "", "")
		s.Supervisor.LogConversationTurn(histCtx, userID, req.SessionID, "assistant", replyText, pathType, "", "")
	}()

	resp := map[string]any{
		"path":            pathType,
		"text":            replyText,
		"tokens_count":    len(strings.Fields(req.Text)),
		"warming_enabled": s.Supervisor.IsWarmingEnabled(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *OrchestratorServer) handleConfirmation(w http.ResponseWriter, r *http.Request) {
	var req ConfirmationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	userID := "mock_user_123"
	replyText, err := s.Supervisor.HandleConfirmation(r.Context(), req.TurnID, req.SessionID, userID, req.Text)
	if err != nil {
		replyText = "Transaction confirmation failed."
	}
	// Log confirmation outcomes to Cassandra
	go func() {
		histCtx := context.Background()
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
	userID := "mock_user_123"

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
