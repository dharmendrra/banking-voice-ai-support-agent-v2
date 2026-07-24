package telemetry

import (
	"log/slog"
	"time"
)

// StructuredLog represents the JSON structure of logs emitted
// to Loki/slog containing trace details, latency metrics, and DB metadata.
type StructuredLog struct {
	// Base slog Fields
	Timestamp time.Time `json:"time"`
	Level     string    `json:"level"`
	Message   string    `json:"msg"`
	Logger    string    `json:"logger"` // e.g. "app", "media-engine"

	// OpenTelemetry Trace Context
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`

	// Execution Metrics
	Duration             string  `json:"duration,omitempty"`              // e.g. "3.51ms", "2.14s"
	DurationMS           float64 `json:"duration_ms,omitempty"`           // Raw milliseconds for aggregation
	PostSpeechLatencyMS  float64 `json:"post_speech_latency_ms,omitempty"` // User end-of-speech to agent response latency
	TTFTMs               float64 `json:"ttft_ms,omitempty"`               // Client-perceived Time to First Token
	TTSPlaybackStartMs  float64 `json:"tts_playback_start_ms,omitempty"` // Time for TTS to start playing after token arrival
	E2ELatencyMs        float64 `json:"e2e_latency_ms,omitempty"`        // Total acoustic end to TTS play latency

	// Context Metadata
	SessionID string `json:"session_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`

	// MongoDB Trace Attributes
	DBSystem     string `json:"db.system,omitempty"`     // e.g. "mongodb"
	DBCollection string `json:"db.collection,omitempty"` // e.g. "transactions"
	DBOperation  string `json:"db.operation,omitempty"`  // e.g. "find_many"
	DBLimit      int64  `json:"db.limit,omitempty"`

	// Redis Cache Trace Attributes
	RedisKeyType   string `json:"redis.key_type,omitempty"`   // e.g. "session_context"
	RedisOperation string `json:"redis.operation,omitempty"`  // e.g. "get", "set"
	RedisStream    string `json:"redis.stream,omitempty"`     // e.g. "audit_log_stream"
	RedisEventType string `json:"redis.event_type,omitempty"` // e.g. "dispatch", "warm_outcome"

	// Qdrant Vector DB Trace Attributes
	QdrantCollection string `json:"qdrant.collection,omitempty"` // e.g. "action_intents", "faq_items"

	// MCP Tool Execution Trace Attributes
	MCPTool string `json:"mcp.tool,omitempty"` // e.g. "get_balance", "transfer"

	// Ollama LLM Trace Attributes
	OllamaModel       string `json:"ollama.model,omitempty"`
	OllamaNumMessages int    `json:"ollama.num_messages,omitempty"`
}

// LogValue implements slog.LogValuer to ensure correct JSON tag naming
// and omitempty behavior even under reflection-based OTel slog exporters.
func (l StructuredLog) LogValue() slog.Value {
	var attrs []slog.Attr

	if !l.Timestamp.IsZero() {
		attrs = append(attrs, slog.Time("time", l.Timestamp))
	}
	if l.Level != "" {
		attrs = append(attrs, slog.String("level", l.Level))
	}
	if l.Message != "" {
		attrs = append(attrs, slog.String("msg", l.Message))
	}
	if l.Logger != "" {
		attrs = append(attrs, slog.String("logger", l.Logger))
	}
	if l.TraceID != "" {
		attrs = append(attrs, slog.String("trace_id", l.TraceID))
	}
	if l.SpanID != "" {
		attrs = append(attrs, slog.String("span_id", l.SpanID))
	}
	if l.Duration != "" {
		attrs = append(attrs, slog.String("duration", l.Duration))
	}
	if l.DurationMS != 0 {
		attrs = append(attrs, slog.Float64("duration_ms", l.DurationMS))
	}
	if l.PostSpeechLatencyMS != 0 {
		attrs = append(attrs, slog.Float64("post_speech_latency_ms", l.PostSpeechLatencyMS))
	}
	if l.TTFTMs != 0 {
		attrs = append(attrs, slog.Float64("ttft_ms", l.TTFTMs))
	}
	if l.TTSPlaybackStartMs != 0 {
		attrs = append(attrs, slog.Float64("tts_playback_start_ms", l.TTSPlaybackStartMs))
	}
	if l.E2ELatencyMs != 0 {
		attrs = append(attrs, slog.Float64("e2e_latency_ms", l.E2ELatencyMs))
	}
	if l.SessionID != "" {
		attrs = append(attrs, slog.String("session_id", l.SessionID))
	}
	if l.TurnID != "" {
		attrs = append(attrs, slog.String("turn_id", l.TurnID))
	}
	if l.DBSystem != "" {
		attrs = append(attrs, slog.String("db.system", l.DBSystem))
	}
	if l.DBCollection != "" {
		attrs = append(attrs, slog.String("db.collection", l.DBCollection))
	}
	if l.DBOperation != "" {
		attrs = append(attrs, slog.String("db.operation", l.DBOperation))
	}
	if l.DBLimit != 0 {
		attrs = append(attrs, slog.Int64("db.limit", l.DBLimit))
	}
	if l.RedisKeyType != "" {
		attrs = append(attrs, slog.String("redis.key_type", l.RedisKeyType))
	}
	if l.RedisOperation != "" {
		attrs = append(attrs, slog.String("redis.operation", l.RedisOperation))
	}
	if l.RedisStream != "" {
		attrs = append(attrs, slog.String("redis.stream", l.RedisStream))
	}
	if l.RedisEventType != "" {
		attrs = append(attrs, slog.String("redis.event_type", l.RedisEventType))
	}
	if l.QdrantCollection != "" {
		attrs = append(attrs, slog.String("qdrant.collection", l.QdrantCollection))
	}
	if l.MCPTool != "" {
		attrs = append(attrs, slog.String("mcp.tool", l.MCPTool))
	}
	if l.OllamaModel != "" {
		attrs = append(attrs, slog.String("ollama.model", l.OllamaModel))
	}
	if l.OllamaNumMessages != 0 {
		attrs = append(attrs, slog.Int("ollama.num_messages", l.OllamaNumMessages))
	}

	return slog.GroupValue(attrs...)
}

// SlogArgs converts the StructuredLog into a slice of key-value slog.Attr pairs,
// flattening the struct so attributes are logged at the root level.
func (l StructuredLog) SlogArgs() []any {
	var args []any

	if l.TraceID != "" {
		args = append(args, slog.String("trace_id", l.TraceID))
	}
	if l.SpanID != "" {
		args = append(args, slog.String("span_id", l.SpanID))
	}
	if l.Duration != "" {
		args = append(args, slog.String("duration", l.Duration))
	}
	if l.DurationMS != 0 {
		args = append(args, slog.Float64("duration_ms", l.DurationMS))
	}
	if l.PostSpeechLatencyMS != 0 {
		args = append(args, slog.Float64("post_speech_latency_ms", l.PostSpeechLatencyMS))
	}
	if l.TTFTMs != 0 {
		args = append(args, slog.Float64("ttft_ms", l.TTFTMs))
	}
	if l.TTSPlaybackStartMs != 0 {
		args = append(args, slog.Float64("tts_playback_start_ms", l.TTSPlaybackStartMs))
	}
	if l.E2ELatencyMs != 0 {
		args = append(args, slog.Float64("e2e_latency_ms", l.E2ELatencyMs))
	}
	if l.SessionID != "" {
		args = append(args, slog.String("session_id", l.SessionID))
	}
	if l.TurnID != "" {
		args = append(args, slog.String("turn_id", l.TurnID))
	}
	if l.DBSystem != "" {
		args = append(args, slog.String("db.system", l.DBSystem))
	}
	if l.DBCollection != "" {
		args = append(args, slog.String("db.collection", l.DBCollection))
	}
	if l.DBOperation != "" {
		args = append(args, slog.String("db.operation", l.DBOperation))
	}
	if l.DBLimit != 0 {
		args = append(args, slog.Int64("db.limit", l.DBLimit))
	}
	if l.RedisKeyType != "" {
		args = append(args, slog.String("redis.key_type", l.RedisKeyType))
	}
	if l.RedisOperation != "" {
		args = append(args, slog.String("redis.operation", l.RedisOperation))
	}
	if l.RedisStream != "" {
		args = append(args, slog.String("redis.stream", l.RedisStream))
	}
	if l.RedisEventType != "" {
		args = append(args, slog.String("redis.event_type", l.RedisEventType))
	}
	if l.QdrantCollection != "" {
		args = append(args, slog.String("qdrant.collection", l.QdrantCollection))
	}
	if l.MCPTool != "" {
		args = append(args, slog.String("mcp.tool", l.MCPTool))
	}
	if l.OllamaModel != "" {
		args = append(args, slog.String("ollama.model", l.OllamaModel))
	}
	if l.OllamaNumMessages != 0 {
		args = append(args, slog.Int("ollama.num_messages", l.OllamaNumMessages))
	}

	return args
}
