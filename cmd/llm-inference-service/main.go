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

	"banking-voice-ai-agent/internal/ollama"
	"banking-voice-ai-agent/internal/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
)

type LLMInferenceServer struct {
	Ollama *ollama.Client
}

func main() {
	log.Println("Starting LLM Inference Service...")

	tShutdown, logger, err := telemetry.Init(context.Background(), "llm-inference-service")
	if err != nil {
		log.Fatalf("Fatal: Telemetry initialization failed: %v", err)
	}
	defer func() { _ = tShutdown(context.Background()) }()
	slog.SetDefault(logger)

	ollamaURL := getEnv("OLLAMA_URL", "http://localhost:11434")
	chatModel := getEnv("CHAT_MODEL", "qwen2.5:7b-instruct")
	embedModel := getEnv("EMBED_MODEL", "bge-m3")

	ollamaClient := ollama.NewClient(ollamaURL, chatModel, embedModel)
	srv := &LLMInferenceServer{
		Ollama: ollamaClient,
	}

	mux := http.NewServeMux()
	mux.Handle("/embedding", otelhttp.NewHandler(http.HandlerFunc(srv.handleEmbedding), "embedding"))
	mux.Handle("/chat", otelhttp.NewHandler(http.HandlerFunc(srv.handleChat), "chat"))
	mux.Handle("/format", otelhttp.NewHandler(http.HandlerFunc(srv.handleFormat), "format"))
	mux.HandleFunc("/healthz", srv.handleHealthz)

	server := &http.Server{
		Addr:    ":9091",
		Handler: mux,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down LLM Inference Service...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Println("LLM Inference Service listening on port 9091")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

type EmbeddingReq struct {
	Text string `json:"text"`
}

type EmbeddingResp struct {
	Embedding []float64 `json:"embedding"`
}

func (s *LLMInferenceServer) handleEmbedding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req EmbeddingReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Text == "" {
		http.Error(w, "missing text parameter", http.StatusBadRequest)
		return
	}

	ctx, span := telemetry.Step(r.Context(), "llm.embedding")
	defer span.End()

	emb, err := s.Ollama.GetEmbedding(ctx, req.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(EmbeddingResp{Embedding: emb})
}

type ChatReq struct {
	Messages []ollama.ChatMessage `json:"messages"`
	Stream   bool                 `json:"stream"`
}

type ChatRespChunk struct {
	Type     string `json:"type"` // "thought" or "speech"
	Text     string `json:"text"`
	Done     bool   `json:"done,omitempty"`
	FullText string `json:"full_text,omitempty"`
}

func (s *LLMInferenceServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, span := telemetry.Step(r.Context(), "llm.chat",
		attribute.String("llm.model", s.Ollama.ChatModel),
		attribute.Int("llm.num_messages", len(req.Messages)),
	)
	defer span.End()

	if req.Stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Transfer-Encoding", "chunked")

		var flusher http.Flusher
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}

		responseChan := make(chan ollama.ChatResponse, 20)
		var chatErr error
		go func() {
			_, chatErr = s.Ollama.Chat(ctx, req.Messages, true, responseChan)
		}()

		for chunk := range responseChan {
			if chunk.Message.Thinking != "" {
				_ = json.NewEncoder(w).Encode(ChatRespChunk{Type: "thought", Text: chunk.Message.Thinking})
				if flusher != nil {
					flusher.Flush()
				}
			}
			if chunk.Message.Content != "" {
				_ = json.NewEncoder(w).Encode(ChatRespChunk{Type: "speech", Text: chunk.Message.Content})
				if flusher != nil {
					flusher.Flush()
				}
			}
		}

		if chatErr != nil {
			log.Printf("Chat streaming failed: %v", chatErr)
			_ = json.NewEncoder(w).Encode(ChatRespChunk{Type: "error", Text: chatErr.Error()})
		}
		return
	}

	// Non-streaming
	resText, err := s.Ollama.Chat(ctx, req.Messages, false, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ChatRespChunk{Type: "speech", Text: resText, FullText: resText})
}

type FormatReq struct {
	Query     string `json:"query"`
	McpResult string `json:"mcp_result"`
	Stream    bool   `json:"stream"`
}

func (s *LLMInferenceServer) handleFormat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req FormatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	systemPrompt := "You are a friendly customer service agent for a retail bank. Formulate a natural, conversational response based ONLY on the provided raw bank data. You MUST list all transactions provided in the raw data, detailing the merchant/description, amount, and date. Speak in friendly, conversational, short sentences. For transaction lists, use ordinals like First, Second, etc. and avoid robotic signs like plus/minus. Translate negative/positive values to friendly descriptions (e.g. 'spent 150' instead of '-150')."
	promptText := fmt.Sprintf("Customer query: %s\nRaw bank data: %s\nFormulate the response:", req.Query, req.McpResult)

	messages := []ollama.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: promptText},
	}

	ctx, span := telemetry.Step(r.Context(), "llm.chat",
		attribute.String("llm.model", s.Ollama.ChatModel),
		attribute.String("llm.task", "format_response"),
	)
	defer span.End()

	if req.Stream {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Transfer-Encoding", "chunked")

		var flusher http.Flusher
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}

		responseChan := make(chan ollama.ChatResponse, 20)
		var chatErr error
		go func() {
			_, chatErr = s.Ollama.Chat(ctx, messages, true, responseChan)
		}()

		for chunk := range responseChan {
			if chunk.Message.Thinking != "" {
				_ = json.NewEncoder(w).Encode(ChatRespChunk{Type: "thought", Text: chunk.Message.Thinking})
				if flusher != nil {
					flusher.Flush()
				}
			}
			if chunk.Message.Content != "" {
				_ = json.NewEncoder(w).Encode(ChatRespChunk{Type: "speech", Text: chunk.Message.Content})
				if flusher != nil {
					flusher.Flush()
				}
			}
		}

		if chatErr != nil {
			log.Printf("Format streaming failed: %v", chatErr)
			_ = json.NewEncoder(w).Encode(ChatRespChunk{Type: "error", Text: chatErr.Error()})
		}
		return
	}

	// Non-streaming
	resText, err := s.Ollama.Chat(ctx, messages, false, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ChatRespChunk{Type: "speech", Text: resText, FullText: resText})
}

func (s *LLMInferenceServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", s.Ollama.BaseURL+"/", nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Error creating Ollama request: " + err.Error()))
		return
	}

	resp, err := s.Ollama.HTTPClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Ollama unhealthy: " + err.Error()))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fmt.Sprintf("Ollama returned status code %d", resp.StatusCode)))
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
