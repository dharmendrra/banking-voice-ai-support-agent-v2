package ollama

import (
	"banking-voice-ai-agent/internal/telemetry"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
)

type Client struct {
	BaseURL    string
	ChatModel  string
	EmbedModel string
	HTTPClient *http.Client
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string         `json:"model"`
	Messages []ChatMessage  `json:"messages"`
	Stream   bool           `json:"stream"`
	Think    *bool          `json:"think,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

type ChatResponse struct {
	Message ChatMessage `json:"message"`
	Done    bool        `json:"done"`
}

type EmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type EmbedResponse struct {
	Embedding []float64 `json:"embedding"`
}

func NewClient(baseURL, chatModel, embedModel string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &Client{
		BaseURL:    baseURL,
		ChatModel:  chatModel,
		EmbedModel: embedModel,
		HTTPClient: &http.Client{
			Timeout: 0, // No global timeout, rely on Context timeout/cancellation
		},
	}
}

// GetEmbedding gets the text embedding vector from Ollama
func (c *Client) GetEmbedding(ctx context.Context, text string) ([]float64, error) {
	ctx, span := telemetry.Step(ctx, "ollama.embedding", attribute.String("ollama.model", c.EmbedModel))
	defer span.End()
	reqBody, err := json.Marshal(EmbedRequest{
		Model:  c.EmbedModel,
		Prompt: text,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/embeddings", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embeddings returned status %d: %s", resp.StatusCode, string(body))
	}

	var res EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	return res.Embedding, nil
}

// Chat calls chat endpoint. If stream is true, returns a channel of strings and an error.
// If stream is false, it returns the single completion string.
// Context cancellation will abort the Ollama request mid-flight.
func (c *Client) Chat(ctx context.Context, messages []ChatMessage, stream bool, streamChan chan<- string) (string, error) {
	ctx, span := telemetry.Step(ctx, "ollama.chat",
		attribute.String("ollama.model", c.ChatModel),
		attribute.Int("ollama.num_messages", len(messages)),
	)
	defer span.End()
	thinkVal := false
	reqBody, err := json.Marshal(ChatRequest{
		Model:    c.ChatModel,
		Messages: messages,
		Stream:   stream,
		Think:    &thinkVal,
		Options: map[string]any{
			"num_predict": 1024, // Limit generation length for speed (expanded for thinking models)
			"temperature": 0.0,  // Low temp for reliable banking deflections
		},
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/chat", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama chat returned status %d: %s", resp.StatusCode, string(body))
	}

	if !stream {
		defer resp.Body.Close()
		var res ChatResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return "", err
		}
		return res.Message.Content, nil
	}

	// Read stream response chunk-by-chunk
	go func() {
		defer resp.Body.Close()
		defer close(streamChan)

		decoder := json.NewDecoder(resp.Body)
		for {
			select {
			case <-ctx.Done():
				// Request canceled, stop reading
				return
			default:
				var chunk ChatResponse
				if err := decoder.Decode(&chunk); err != nil {
					if err == io.EOF {
						return
					}
					log.Printf("[Ollama] Stream decoding error: %v", err)
					return
				}
				if chunk.Message.Content != "" {
					select {
					case streamChan <- chunk.Message.Content:
					case <-ctx.Done():
						return
					}
				}
				if chunk.Done {
					return
				}
			}
		}
	}()

	return "", nil
}
