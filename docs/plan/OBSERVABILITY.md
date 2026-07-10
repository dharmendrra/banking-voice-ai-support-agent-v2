# Observability

Traces/APM, logs, and metrics via **OpenTelemetry (OTLP)**, viewed in a
**Datadog-like local UI** (Grafana). The backends live in a **separate reusable
repo** (`../observability-stack`) — this app only owns instrumentation.

## The split
- **`observability-stack` repo** = the backends + UI (Grafana `otel-lgtm`:
  Collector + Tempo + Loki + Prometheus + Grafana). Reusable by any project.
- **This app** = OTel instrumentation (`internal/telemetry`) that exports OTLP to
  `localhost:4317`. No-op unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set, so the app
  runs fine with the stack down.

## What's instrumented (orchestrator)
- **Traces:** every `/query` is a server span (otelhttp) with a child `turn` span;
  MCP/Ollama/Cassandra calls nest under it → trace waterfall in Tempo.
- **Metrics:** `voiceagent.turns` counter, labeled `dispatch=action|faq|llm`.
- **Logs:** structured slog bridged to OTLP → Loki, auto-correlated to traces by
  `trace_id`.

## Run + view
```bash
# 1. start the stack (separate repo)
cd ../observability-stack && docker compose up -d      # Grafana at :3000

# 2. run the app pointed at it
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
# start orchestrator (+ media engine, mcp) as usual
```
Grafana → **Explore**: Tempo (traces/APM + service map), Loki (logs, jump to
trace by `trace_id`), Prometheus (metrics / dashboards).

## Data & retention
All telemetry stored on local disk in the stack's Docker volume (nothing leaves
the machine). Each backend has its own retention; prod would use object storage.

## Still open
- Instrument the **media engine** + **MCP** too (only the orchestrator is wired),
  so a trace spans STT → dispatch → bank → TTS end-to-end.
- **Keep PII out of plain debug logs** (audit stream legitimately carries txn
  detail; general `log.Printf` should not).
