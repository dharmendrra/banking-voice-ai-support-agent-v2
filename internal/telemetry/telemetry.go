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
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Enabled reports whether telemetry export is configured.
func Enabled() bool { return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" }

// Init sets up global tracer/meter/logger providers exporting OTLP. It returns a
// shutdown func (flushes buffers) and a structured logger. If telemetry is
// disabled it returns a plain stderr slog logger and a no-op shutdown.
func Init(ctx context.Context, serviceName string) (shutdown func(context.Context) error, logger *slog.Logger) {
	if !Enabled() {
		return func(context.Context) error { return nil },
			slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}

	// Global propagator so trace context crosses service boundaries
	// (media-engine → orchestrator) — OTel Go sets none by default.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	res, _ := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", serviceName)))

	traceExp, _ := otlptracegrpc.New(ctx)
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)

	metricExp, _ := otlpmetricgrpc.New(ctx)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	logExp, _ := otlploggrpc.New(ctx)
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	otellog.SetLoggerProvider(lp)

	shutdown = func(c context.Context) error {
		_ = tp.Shutdown(c)
		_ = mp.Shutdown(c)
		return lp.Shutdown(c)
	}
	// slog bridged to OTLP -> Loki, auto-correlated with traces by trace_id.
	return shutdown, otelslog.NewLogger(serviceName)
}

// Logger returns a structured logger. When telemetry is enabled it bridges to
// OTLP (→ Loki, trace-correlated); otherwise it writes JSON to stderr.
func Logger(name string) *slog.Logger {
	if !Enabled() {
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return otelslog.NewLogger(name)
}

// Tracer returns a named tracer from the global provider.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// Meter returns a named meter from the global provider.
func Meter(name string) metric.Meter { return otel.Meter(name) }
