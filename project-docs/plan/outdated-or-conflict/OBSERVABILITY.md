# Observability (v2)

Traces/APM, logs, and metrics via **OpenTelemetry (OTLP)**, viewed in a
**Datadog-like local UI** (Grafana). Backends live in the **separate reusable
repo** `../observability-stack`; this app only owns instrumentation
(`internal/telemetry`).

## The split
- **`observability-stack` repo** = backends + UI (Grafana `otel-lgtm`: Collector
  + Tempo + Loki + Prometheus + Grafana). Reusable by any project.
- **This app** = OTel instrumentation exporting OTLP to `localhost:4317`. No-op
  unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set; exporters are lazy, so a down
  collector never blocks or crashes the app. When disabled, `Step` still logs to
  stderr (JSON) so every step is visible locally.

## Design: per-turn traces, not one call-long trace
A call can last minutes; a single call-length span would export only at hangup —
you'd see nothing mid-call. So **each turn is its own short trace** (exported the
moment it completes), correlated by `call_id`/`session_id`. Live visibility of an
**ongoing** call comes from **logs + metrics**, not from waiting for a trace to
close.

## What's instrumented (end-to-end)
- **Media engine:** the HTTP client to the orchestrator uses `otelhttp.NewTransport`,
  so each `POST /api/final` is a client span that **propagates** W3C trace context
  — the orchestrator's server + `turn` spans nest under it → one per-turn trace
  across services. Live `call.start` / `call.end` logs + a `voiceagent.active_calls`
  gauge (no call-length span).
- **Orchestrator / warm supervisor:** `otelhttp.NewHandler` on every `/api/*`
  endpoint; a `turn` span; dispatch counter `voiceagent.turns{dispatch=action|faq|llm}`;
  warm-supervisor events via `LogEvent` (halt_point, cache_probe, dispatch, …).
- **Every step = span + trace-correlated log** via `telemetry.Step(...)`:
  `ollama.embedding`, `ollama.chat`, `qdrant.search`, `redis.get_session`,
  `redis.save_session`, `redis.audit`, `bank.get_balance` / `.get_transactions` /
  `.get_due_date` / `.block_card` / `.transfer`, `mcp.<tool>`, `cassandra.log_turn`
  — each in **both** Tempo (traces) and Loki (logs).

## Run + view
```bash
# 1. start the reusable stack
cd ../observability-stack && docker compose up -d      # Grafana at :3000

# 2. run the app pointed at it
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
# start orchestrator + media engine as usual
```
Grafana → **Explore**: Tempo (per-turn trace waterfalls + service map), Loki
(step logs; jump log⇄trace by `trace_id`; tail `call.start`/`turn` live),
Prometheus (`active_calls` gauge, `voiceagent.turns`, latencies).

## Data & retention
Telemetry is stored on local disk in the stack's Docker volume (nothing leaves
the machine); prod would point Tempo/Loki at object storage.

## Still open
- **Keep PII out of plain `log.Printf`** (audit stream legitimately carries txn
  detail; general debug logs should not).
