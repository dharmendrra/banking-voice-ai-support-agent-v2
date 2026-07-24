# Observability & Microservices Decoupling Master Plan (2026-07-12)

This master plan coordinates all modular plans designed today to achieve high-precision observability, active service monitoring, decoupled write buffers, and low-latency streaming audio playback.

---

## 🗺️ Sub-Plans Index

| Sub-Plan | Purpose & Scope | Target File |
| :--- | :--- | :--- |
| **APM & Unified Namespace** | Sets up `voice-ai-agent` namespace telemetry, Nginx ingress proxy logs, and custom OTel resource metrics. | [apm_plan.md](file:///Users/dharmendra/golang-projects/banking-voice-ai-support-agent-v2/project-docs/plan/done/apm_plan.md) |
| **Background Model Health** | Establishes background pull-based probes and logging for STT, TTS, and Ollama. | [background_health_plan.md](file:///Users/dharmendra/golang-projects/banking-voice-ai-support-agent-v2/project-docs/plan/done/background_health_plan.md) |
| **History Consumer** | Decouples Cassandra database writes from the live orchestrator thread using Redis Streams. | [history_consumer_plan.md](file:///Users/dharmendra/golang-projects/banking-voice-ai-support-agent-v2/project-docs/plan/done/history_consumer_plan.md) |
| **Audit Log Consumer** | Handles async secure compliance tool audit logs, ensuring zero latency impact on the user call path. | [audit_consumer_plan.md](file:///Users/dharmendra/golang-projects/banking-voice-ai-support-agent-v2/project-docs/plan/done/audit_consumer_plan.md) |
| **8-Service Decoupling** | Refactors the orchestrator monolith into stateless components for context, cache, inference, and tools. | [decoupled_orchestrator_plan.md](file:///Users/dharmendra/golang-projects/banking-voice-ai-support-agent-v2/project-docs/plan/done/decoupled_orchestrator_plan.md) |
| **Decoupled Telemetry Specs** | Outlines explicit OTel spans, attributes, and ClickHouse queries for all decoupled components. | [decoupled_telemetry_plan.md](file:///Users/dharmendra/golang-projects/banking-voice-ai-support-agent-v2/project-docs/plan/done/decoupled_telemetry_plan.md) |
| **Streaming & Chunked TTS** | Implements sentence-by-sentence streaming playback with user barge-in / resume states. | [streaming_tts_plan.md](file:///Users/dharmendra/golang-projects/banking-voice-ai-support-agent-v2/project-docs/plan/done/streaming_tts_plan.md) |

---

## 🚀 Coordinated Implementation Phases

We will execute this architecture refactoring in five structured, testable phases:

### Phase 1: Foundation & APM Namespace
1. Modify `internal/telemetry/telemetry.go` to inject `"service.namespace" = "voice-ai-agent"` into the OTel Resource configuration.
2. Verify that both the `media-engine` and `llm-orchestrator-server` start reporting metrics under the same namespace group in SigNoz.

### Phase 2: Dependency Health Probes (`/healthz`)
1. Create `/healthz` endpoints in `llm-orchestrator-server` and `media-engine`.
2. Configure Docker Healthchecks in `docker-compose.yml` to automatically verify container status.
3. Set up the browser STT heartbeat WebSocket event (`stt_health_heartbeat`) logging to `"voice-ai-stt"`.
4. Establish the backend background loops verifying Kokoro TTS (`voice-ai-tts`) and Ollama (`voice-ai-ollama`).

### Phase 3: Decoupled Consumers (History & Audit)
1. Modify `LogConversationTurn` and `WriteAuditLog` to publish events asynchronously to Redis Streams (`conversation_history_stream` and `audit_log_stream`).
2. Build the history consumer (`voice-ai-conversation-history-consumer`) and the audit log consumer (`voice-ai-audit-consumer`) background workers.
3. Expose pull-based `/healthz` HTTP ports (ports `9085` and `9086`) on the consumers to report backlog lag.

### Phase 4: High-Performance Streaming TTS & Barge-in
1. Refactor `frontend/index.html` to process incoming `speech` text chunks on-the-fly, building sentences dynamically and queueing them in `audioQueue`.
2. Update the barge-in event handler to store remaining sentences in `suspendedQueue` on interruption, instead of discarding them.
3. Implement the `resume_playback` WebSocket event handler to restore `suspendedQueue` and continue playing when the user says "resume" or "go on".

### Phase 5: Complete 8-Service Decoupling
1. Refactor the `llm-orchestrator-server` codebase to split out:
   * **`session-context-service`** (Redis memory manager).
   * **`semantic-cache-service`** (Qdrant search gateway).
   * **`llm-inference-service`** (Ollama call gateway).
   * **`tool-execution-service`** (MCP and security checks).
2. Wire gRPC protocols between all internal services to keep network hop overhead under 1.5ms.
3. Verify the end-to-end trace latency budget is under **`106ms`** (pre-audio rendering).
