package contextmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/ollama"
	"banking-voice-ai-agent/internal/telemetry"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
)

// ContextManager handles loading, appending, pruning, and saving conversation context.
type ContextManager struct {
	Redis *db.RedisManager
}

// NewContextManager creates a new instance of ContextManager.
func NewContextManager(r *db.RedisManager) *ContextManager {
	return &ContextManager{
		Redis: r,
	}
}

// GetContext retrieves the conversation history, applies pruning to the last 5 turns, and returns it.
func (cm *ContextManager) GetContext(ctx context.Context, sessionID string) ([]ollama.ChatMessage, error) {
	ctx, span := telemetry.Step(ctx, "context_manager.get_context",
		attribute.String("session_id", sessionID),
	)
	defer span.End()

	key := fmt.Sprintf("session:%s:context", sessionID)
	data, err := cm.Redis.Client.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil || strings.Contains(err.Error(), "nil") {
			return []ollama.ChatMessage{}, nil
		}
		return nil, err
	}

	var messages []ollama.ChatMessage
	if err := json.Unmarshal([]byte(data), &messages); err != nil {
		return nil, err
	}

	// Apply rolling 5-turn pruning (max 10 messages)
	return cm.PruneHistory(messages, 5), nil
}

// AppendAndSave adds new user/assistant turns, redacts sensitive PII, and commits context to Redis.
func (cm *ContextManager) AppendAndSave(ctx context.Context, sessionID string, userMsg, assistantMsg string) ([]ollama.ChatMessage, error) {
	ctx, span := telemetry.Step(ctx, "context_manager.save_context",
		attribute.String("session_id", sessionID),
	)
	defer span.End()

	history, err := cm.GetContext(ctx, sessionID)
	if err != nil {
		history = []ollama.ChatMessage{}
	}

	// Clean/Redact inputs
	cleanUser := cm.RedactPII(userMsg)
	cleanAssistant := cm.RedactPII(assistantMsg)

	if cleanUser != "" {
		history = append(history, ollama.ChatMessage{Role: "user", Content: cleanUser})
	}
	if cleanAssistant != "" {
		history = append(history, ollama.ChatMessage{Role: "assistant", Content: cleanAssistant})
	}

	// Prune again before saving to ensure strict boundaries
	history = cm.PruneHistory(history, 5)

	key := fmt.Sprintf("session:%s:context", sessionID)
	data, err := json.Marshal(history)
	if err != nil {
		return nil, err
	}

	err = cm.Redis.Client.Set(ctx, key, data, 1*time.Hour).Err()
	if err != nil {
		return nil, err
	}

	return history, nil
}

// PruneHistory keeps only the rolling last N turns (1 turn = 1 user + 1 assistant message).
// It ensures that the conversation always begins with a user message.
func (cm *ContextManager) PruneHistory(messages []ollama.ChatMessage, maxTurns int) []ollama.ChatMessage {
	maxMessages := maxTurns * 2
	if len(messages) <= maxMessages {
		return messages
	}

	pruned := messages[len(messages)-maxMessages:]
	// Auto-align so it starts with a user message
	for len(pruned) > 0 && pruned[0].Role != "user" {
		pruned = pruned[1:]
	}
	return pruned
}

// RedactPII filters out card numbers, CVVs, and PINs from stored logs and LLM contexts.
func (cm *ContextManager) RedactPII(text string) string {
	// Redact 15 or 16-digit card numbers (Visa, Mastercard, Amex formats)
	cardRegex := regexp.MustCompile(`\b\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}\b|\b\d{4}[- ]?\d{6}[- ]?\d{5}\b`)
	text = cardRegex.ReplaceAllString(text, "[CARD_REDACTED]")

	// Redact CVV / PIN parameters
	cvvRegex := regexp.MustCompile(`(?i)\b(cvv|cvc|pin)\b\s*[:=]\s*\d{3,4}\b`)
	text = cvvRegex.ReplaceAllString(text, "$1: [REDACTED]")

	return text
}

// SerializeHistory concatenates all messages in the history into a single space-separated text block.
// This is used as trusted source data in the output guardrail filters.
func (cm *ContextManager) SerializeHistory(messages []ollama.ChatMessage) string {
	var builder strings.Builder
	for _, msg := range messages {
		builder.WriteString(msg.Content)
		builder.WriteString(" ")
	}
	return builder.String()
}
