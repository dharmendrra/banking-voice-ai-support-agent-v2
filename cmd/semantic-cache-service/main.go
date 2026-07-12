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
	"sync"
	"syscall"
	"time"

	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
)

type SemanticCacheServer struct {
	Qdrant            *db.QdrantManager
	InferenceService  string
	HTTPClient        *http.Client
}

func main() {
	log.Println("Starting Semantic Cache Service...")

	tShutdown, logger, err := telemetry.Init(context.Background(), "semantic-cache-service")
	if err != nil {
		log.Fatalf("Fatal: Telemetry initialization failed: %v", err)
	}
	defer func() { _ = tShutdown(context.Background()) }()
	slog.SetDefault(logger)

	qdrantURL := getEnv("QDRANT_URL", "http://localhost:6333")
	inferenceServiceURL := getEnv("INFERENCE_SERVICE_URL", "http://localhost:9091")

	qdrantMgr, err := db.NewQdrantManager(qdrantURL)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Qdrant: %v", err)
	}

	srv := &SemanticCacheServer{
		Qdrant:           qdrantMgr,
		InferenceService: inferenceServiceURL,
		HTTPClient:       &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			Timeout:   10 * time.Second,
		},
	}

	mux := http.NewServeMux()
	mux.Handle("/search", otelhttp.NewHandler(http.HandlerFunc(srv.handleSearch), "search"))
	mux.HandleFunc("/healthz", srv.handleHealthz)

	server := &http.Server{
		Addr:    ":9089",
		Handler: mux,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down Semantic Cache Service...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Println("Semantic Cache Service listening on port 9089")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

type SearchRequest struct {
	Text string `json:"text"`
}

type SearchResponse struct {
	BestActionScore float64        `json:"best_action_score"`
	MatchedAction   map[string]any `json:"matched_action,omitempty"`
	BestFAQScore    float64        `json:"best_faq_score"`
	MatchedFAQ      map[string]any `json:"matched_faq,omitempty"`
}

type EmbeddingRequest struct {
	Text string `json:"text"`
}

type EmbeddingResponse struct {
	Embedding []float64 `json:"embedding"`
}

func (s *SemanticCacheServer) getEmbedding(ctx context.Context, text string) ([]float64, error) {
	reqBody, err := json.Marshal(EmbeddingRequest{Text: text})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.InferenceService+"/embedding", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("inference service embedding error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var embResp EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embResp); err != nil {
		return nil, err
	}

	return embResp.Embedding, nil
}

func (s *SemanticCacheServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Text == "" {
		http.Error(w, "missing text parameter", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	ctx, span := telemetry.Step(ctx, "cache.match")
	defer span.End()

	// 1. Get Embedding
	emb, err := s.getEmbedding(ctx, req.Text)
	if err != nil {
		log.Printf("Failed to get embedding: %v", err)
		http.Error(w, fmt.Sprintf("failed to get embedding: %v", err), http.StatusInternalServerError)
		return
	}

	// 2. Query both indexes in parallel
	var actionMatches, faqMatches []db.QdrantMatch
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		actCtx, actSpan := telemetry.Step(ctx, "qdrant.search", attribute.String("qdrant.collection", "action_intents"))
		defer actSpan.End()
		if res, err := s.Qdrant.Search(actCtx, "action_intents", emb, 1); err == nil {
			actionMatches = res
		} else {
			log.Printf("Qdrant action_intents search error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		faqCtx, faqSpan := telemetry.Step(ctx, "qdrant.search", attribute.String("qdrant.collection", "faq_items"))
		defer faqSpan.End()
		if res, err := s.Qdrant.Search(faqCtx, "faq_items", emb, 1); err == nil {
			faqMatches = res
		} else {
			log.Printf("Qdrant faq_items search error: %v", err)
		}
	}()

	wg.Wait()

	var bestActionScore float64 = 0.0
	var bestFAQScore float64 = 0.0
	var matchedAction map[string]any
	var matchedFAQ map[string]any

	if len(actionMatches) > 0 {
		bestActionScore = actionMatches[0].Score
		matchedAction = actionMatches[0].Payload
	}
	if len(faqMatches) > 0 {
		bestFAQScore = faqMatches[0].Score
		matchedFAQ = faqMatches[0].Payload
	}

	resp := SearchResponse{
		BestActionScore: bestActionScore,
		MatchedAction:   matchedAction,
		BestFAQScore:    bestFAQScore,
		MatchedFAQ:      matchedFAQ,
	}

	var outcome string
	if bestActionScore >= 0.94 || bestFAQScore >= 0.94 {
		outcome = "hit"
	} else {
		outcome = "miss"
	}
	span.SetAttributes(attribute.String("cache.outcome", outcome))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *SemanticCacheServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	// Perform a simple search/probe or collections query to verify Qdrant connection
	req, err := http.NewRequestWithContext(ctx, "GET", s.Qdrant.BaseURL+"/collections", nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Error creating Qdrant ping request: " + err.Error()))
		return
	}

	resp, err := s.Qdrant.HTTPClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Qdrant unhealthy: " + err.Error()))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fmt.Sprintf("Qdrant returned status %d", resp.StatusCode)))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
