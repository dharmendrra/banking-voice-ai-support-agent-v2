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
	"syscall"
	"time"

	"banking-voice-ai-agent/internal/contextmanager"
	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/ollama"
	"banking-voice-ai-agent/internal/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
)

type Server struct {
	Redis          *db.RedisManager
	ContextManager *contextmanager.ContextManager
}

type LoadRequest struct {
	SessionID string `json:"session_id"`
}

type LoadResponse struct {
	SessionID string               `json:"session_id"`
	Messages  []ollama.ChatMessage `json:"messages"`
}

type SaveRequest struct {
	SessionID        string `json:"session_id"`
	UserMessage      string `json:"user_message"`
	AssistantMessage string `json:"assistant_message"`
}

type SaveResponse struct {
	SessionID string               `json:"session_id"`
	Messages  []ollama.ChatMessage `json:"messages"`
}

type PruneRequest struct {
	SessionID string `json:"session_id"`
	MaxTurns  int    `json:"max_turns"`
}

type PruneResponse struct {
	SessionID string               `json:"session_id"`
	Messages  []ollama.ChatMessage `json:"messages"`
}

func main() {
	log.Println("Starting Standalone Session Context Service...")

	// Initialize OpenTelemetry stack
	if telemetry.Enabled() {
		tShutdown, logger, err := telemetry.Init(context.Background(), "session-context-service")
		if err != nil {
			log.Printf("[Telemetry] Warning: Telemetry initialization failed: %v", err)
		} else {
			defer func() { _ = tShutdown(context.Background()) }()
			slog.SetDefault(logger)
		}
	}

	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")

	redisMgr, err := db.NewRedisManager(redisAddr)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Redis: %v", err)
	}

	ctxManager := contextmanager.NewContextManager(redisMgr)

	srv := &Server{
		Redis:          redisMgr,
		ContextManager: ctxManager,
	}

	mux := http.NewServeMux()
	mux.Handle("/load", otelhttp.NewHandler(http.HandlerFunc(srv.handleLoad), "context.load"))
	mux.Handle("/save", otelhttp.NewHandler(http.HandlerFunc(srv.handleSave), "context.save"))
	mux.Handle("/prune", otelhttp.NewHandler(http.HandlerFunc(srv.handlePrune), "context.prune"))
	mux.HandleFunc("/healthz", srv.handleHealthz)

	port := getEnv("PORT", "9087")
	server := &http.Server{
		Addr:    ":" + port,
		Handler: withLogging(mux),
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down Session Context Service...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("Session Context Service listening on port %s", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req LoadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	messages, err := s.ContextManager.GetContext(ctx, req.SessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load context: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(LoadResponse{
		SessionID: req.SessionID,
		Messages:  messages,
	})
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	messages, err := s.ContextManager.AppendAndSave(ctx, req.SessionID, req.UserMessage, req.AssistantMessage)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to save context: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(SaveResponse{
		SessionID: req.SessionID,
		Messages:  messages,
	})
}

func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req PruneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}

	maxTurns := req.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 5
	}

	ctx := r.Context()
	ctx, span := telemetry.Step(ctx, "context.prune_operation",
		attribute.String("session_id", req.SessionID),
		attribute.Int("max_turns", maxTurns),
	)
	defer span.End()

	// Load context
	messages, err := s.ContextManager.GetContext(ctx, req.SessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load context for pruning: %v", err), http.StatusInternalServerError)
		return
	}

	// Prune history
	pruned := s.ContextManager.PruneHistory(messages, maxTurns)
	prunedTurns := (len(messages) - len(pruned)) / 2
	span.SetAttributes(attribute.Int("context.pruned_turns", prunedTurns))

	// Save back to Redis
	err = s.Redis.SaveSessionContext(ctx, req.SessionID, pruned)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to save pruned context: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PruneResponse{
		SessionID: req.SessionID,
		Messages:  pruned,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	status := "healthy"
	details := make(map[string]string)

	if err := s.Redis.Client.Ping(ctx).Err(); err != nil {
		status = "unhealthy"
		details["redis"] = err.Error()
	} else {
		details["redis"] = "healthy"
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
