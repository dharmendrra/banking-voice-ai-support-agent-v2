package llmorchestrator

import (
	"context"
	"sync"

	"banking-voice-ai-agent/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	metricsOnce sync.Once
	turnCounter metric.Int64Counter
)

// recordTurn increments the per-dispatch turn counter (voiceagent.turns), labeled
// by dispatch path: "action" | "faq" | "llm".
func recordTurn(ctx context.Context, path string) {
	metricsOnce.Do(func() {
		turnCounter, _ = telemetry.Meter("orchestrator").Int64Counter("voiceagent.turns")
	})
	if turnCounter != nil {
		turnCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("dispatch", path)))
	}
}
