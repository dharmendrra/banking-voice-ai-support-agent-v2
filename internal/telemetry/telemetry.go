// Package telemetry wires the service into the reusable observability stack
// (../../observability-stack) via OpenTelemetry: traces, metrics, and logs all
// exported over OTLP to localhost:4317.
//
// It is a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set, so the app runs fine
// with the observability stack down. Exporters connect lazily, so a missing
// collector never blocks the app.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Enabled reports whether telemetry export is configured.
func Enabled() bool { return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" }

// verifyEndpointUp performs a fast TCP connection dial to check if the OTLP receiver is online.
func verifyEndpointUp(endpoint string) error {
	var hostPort string
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		hostPort = strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	} else {
		hostPort = u.Host
	}

	if !strings.Contains(hostPort, ":") {
		hostPort = hostPort + ":4317"
	}

	conn, err := net.DialTimeout("tcp", hostPort, 2*time.Second)
	if err != nil {
		return fmt.Errorf("observability endpoint %s is offline or unreachable: %w", hostPort, err)
	}
	conn.Close()
	return nil
}

// Init sets up global tracer/meter/logger providers exporting OTLP. It returns a
// shutdown func (flushes buffers), a structured logger, and a connection error if the endpoint is down.
func Init(ctx context.Context, serviceName string) (shutdown func(context.Context) error, logger *slog.Logger, err error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4317" // Default OTel standard endpoint
	}

	// Verify the collector is up before starting (fail-fast)
	if err := verifyEndpointUp(endpoint); err != nil {
		return nil, nil, err
	}

	// Global propagator so trace context crosses service boundaries
	// (media-engine → orchestrator) — OTel Go sets none by default.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	res, _ := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", serviceName),
		attribute.String("service.namespace", "voice-ai-agent"),
	))

	traceExp, _ := otlptracegrpc.New(ctx)
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)

	metricExp, _ := otlpmetricgrpc.New(ctx)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	var lp *sdklog.LoggerProvider
	if os.Getenv("OTEL_LOGS_EXPORTER") != "none" {
		logExp, err := otlploggrpc.New(ctx)
		if err == nil {
			lp = sdklog.NewLoggerProvider(
				sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
				sdklog.WithResource(res),
			)
			otellog.SetLoggerProvider(lp)
		}
	}

	shutdown = func(c context.Context) error {
		_ = tp.Shutdown(c)
		_ = mp.Shutdown(c)
		if lp != nil {
			_ = lp.Shutdown(c)
		}
		return nil
	}

	if os.Getenv("OTEL_LOGS_EXPORTER") == "none" {
		return shutdown, Logger(serviceName), nil
	}
	return shutdown, otelslog.NewLogger(serviceName), nil
}

// Logger returns a structured logger. When telemetry is enabled it bridges to
// OTLP (→ Loki, trace-correlated); otherwise it writes JSON to stderr with trace correlation.
func Logger(name string) *slog.Logger {
	if !Enabled() || os.Getenv("OTEL_LOGS_EXPORTER") == "none" {
		return slog.New(newTraceCorrelatingHandler(os.Stderr, name))
	}
	return otelslog.NewLogger(name)
}

type contextKey string

const (
	SessionIDKey contextKey = "session_id"
	TurnIDKey    contextKey = "turn_id"
)

// WithTraceContext returns a new context carrying the session and turn IDs.
func WithTraceContext(ctx context.Context, sessionID, turnID string) context.Context {
	ctx = context.WithValue(ctx, SessionIDKey, sessionID)
	ctx = context.WithValue(ctx, TurnIDKey, turnID)
	return ctx
}

type loggingSpan struct {
	trace.Span
	ctx   context.Context
	name  string
	attrs []attribute.KeyValue
	start time.Time
}

func (s *loggingSpan) End(options ...trace.SpanEndOption) {
	duration := time.Since(s.start)
	s.Span.SetAttributes(attribute.String("duration", duration.String()))
	s.Span.End(options...)

	spanContext := trace.SpanContextFromContext(s.ctx)

	logRecord := StructuredLog{
		Timestamp:           time.Now(),
		Level:               "INFO",
		Message:             s.name,
		Logger:              "app",
		Duration:            duration.String(),
		DurationMS:          float64(duration.Nanoseconds()) / 1e6,
		PostSpeechLatencyMS: 0,
	}

	if s.name == "media.final_transcript" || s.name == "media.confirmation" {
		logRecord.PostSpeechLatencyMS = float64(duration.Nanoseconds()) / 1e6
	}

	if spanContext.IsValid() {
		logRecord.TraceID = spanContext.TraceID().String()
		logRecord.SpanID = spanContext.SpanID().String()
	}

	// Unpack attributes into strictly typed struct fields
	for _, attr := range s.attrs {
		key := string(attr.Key)
		val := attr.Value.AsInterface()

		switch key {
		case "db.system":
			logRecord.DBSystem, _ = val.(string)
		case "db.collection":
			logRecord.DBCollection, _ = val.(string)
		case "db.operation":
			logRecord.DBOperation, _ = val.(string)
		case "db.limit":
			if i, ok := val.(int64); ok {
				logRecord.DBLimit = i
			}
		case "redis.key_type":
			logRecord.RedisKeyType, _ = val.(string)
		case "redis.operation":
			logRecord.RedisOperation, _ = val.(string)
		case "redis.stream":
			logRecord.RedisStream, _ = val.(string)
		case "redis.event_type":
			logRecord.RedisEventType, _ = val.(string)
		case "qdrant.collection":
			logRecord.QdrantCollection, _ = val.(string)
		case "mcp.tool":
			logRecord.MCPTool, _ = val.(string)
		case "ollama.model":
			logRecord.OllamaModel, _ = val.(string)
		case "ollama.num_messages":
			if i, ok := val.(int); ok {
				logRecord.OllamaNumMessages = i
			}
		case "session_id":
			logRecord.SessionID, _ = val.(string)
		case "turn_id":
			logRecord.TurnID, _ = val.(string)
		}
	}

	Logger("app").InfoContext(s.ctx, s.name, logRecord.SlogArgs()...)
}

// Step starts a span AND returns a trace.Span. When End() is called on the span,
// it emits a trace-correlated log line containing the final duration.
func Step(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	// Propagate session and turn IDs from context if available
	if sess, ok := ctx.Value(SessionIDKey).(string); ok && sess != "" {
		attrs = append(attrs, attribute.String("session_id", sess))
	}
	if turn, ok := ctx.Value(TurnIDKey).(string); ok && turn != "" {
		attrs = append(attrs, attribute.String("turn_id", turn))
	}

	var spanOpts []trace.SpanStartOption
	if len(attrs) > 0 {
		spanOpts = append(spanOpts, trace.WithAttributes(attrs...))
	}
	ctx, span := otel.Tracer("app").Start(ctx, name, spanOpts...)

	lSpan := &loggingSpan{
		Span:  span,
		ctx:   ctx,
		name:  name,
		attrs: attrs,
		start: time.Now(),
	}

	return ctx, lSpan
}

// Tracer returns a named tracer from the global provider.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// Meter returns a named meter from the global provider.
func Meter(name string) metric.Meter { return otel.Meter(name) }

// traceCorrelatingHandler intercepts log records and injects active OTel trace/span IDs
type traceCorrelatingHandler struct {
	slog.Handler
	name string
}

func newTraceCorrelatingHandler(w *os.File, name string) *traceCorrelatingHandler {
	return &traceCorrelatingHandler{
		Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}),
		name:    name,
	}
}

func (h *traceCorrelatingHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(slog.String("logger", h.name))
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// RecordError records an error on the span inside the given context.
func RecordError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
