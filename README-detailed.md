# Deep-Dive Engineering & Deployment Manual: Voice AI Banking Agent (V2)

This document provides a detailed breakdown of the evolutionary approaches, technical implementation details, directory structures, network layouts, and latency evaluation reports for the distributed Voice AI banking support agent.

For a high-level overview and design diagrams, refer back to the main [README.md](./README.md).

---

## 🏗️ Evolution of Architectural Approaches

To reach the target latency and transaction safety SLOs, the system went through three distinct design phases. Each iteration preserved local model capabilities while introducing progressive architectural decoupling and cloud extensibility.

```
Approach 1 (Monolith) -> Approach 2 (Hybrid Cache) -> Approach 3 (Decoupled 8-Service Stack)
```

### Approach 1: Monolithic Synchronous Pipeline (V1 Legacy)
* **Topology**: 2 Core Services — **`media-engine`** and **`llm-orchestrator`**.
* **Capabilities**: Client-side browser SpeechRecognition (STT), Google Translate / system native TTS, and a local `gemma2:2b` LLM hosted on Ollama.
* **Data Flow**: The `media-engine` parsed voice into text and pushed it to `llm-orchestrator`. The orchestrator executed database checks (MongoDB ledger), LLM inference, and TTS speech generation synchronously on a single request-response thread.
* **Limitations & Trade-offs**: Real-time WebRTC audio rendering is highly sensitive to thread blockages. Running write transactions (e.g. money transfers) or local LLM generation synchronously blocked the audio frame delivery loop, resulting in critical packet drops, voice jitters, and connection resets.

### Approach 2: Hybrid Caching & Partial Decoupling (v1.5)
* **Topology**: Added **`Qdrant`** vector database and a caching layer.
* **Capabilities**: Retained local STT and fallback TTS; introduced standardized prompt formatting in the orchestrator to allow hot-swapping the local model with Cloud APIs (Gemini/OpenAI) via environment variables.
* **Data Flow**: When a user utterance was received, it checked Qdrant for semantic cache hits in parallel. A cache hit bypassed the LLM entirely, responding in `<150ms`.
* **Limitations & Trade-offs**: While read-only requests were fast, mutative banking transactions still executed synchronously on the orchestrator thread. Latency spikes on writing to MongoDB still caused connection breaks in the active WebSocket channel.

### Approach 3: Fully Decoupled Asynchronous Microservices (V2 Current)
* **Topology**: 8 Decoupled Go Services + Nginx Load Balancer (Proxying 3 replicas of the media-engine and orchestrator).
  1. **`media-engine`**: High-performance WebRTC connection gateway (LiveKit).
  2. **`llm-micro-orchestrator`**: Stateless coordinator managing multi-turn states and intent routing.
  3. **`session-context-service`**: Manages transient state and slot parameters over Redis.
  4. **`semantic-cache-service`**: Executes fast semantic checks in Qdrant, using context cancellation to immediately halt parallel LLM threads on a hit.
  5. **`llm-inference-service`**: Model-agnostic wrapper supporting local Ollama (`qwen2.5:7b-instruct`) and cloud providers (Gemini, OpenAI).
  6. **`tool-execution-service`**: Executes ledger writes (MongoDB) and validates parameters.
  7. **`conversation-history-consumer`**: Async worker persisting transcripts to Cassandra.
  8. **`audit-log-consumer`**: Async worker persisting security audit logs to Cassandra.
* **Capabilities**: Advanced high-fidelity local neural **Kokoro-82M** TTS running natively with PyTorch Metal (MPS) GPU acceleration on macOS, alongside full cloud LLM pluggability.
* **Benefits & Trade-offs**: Isolates all slow compute processes (LLM inference, MongoDB writes, Cassandra logging) from the real-time audio channel, guaranteeing zero packet drops and <300ms p99 latency targets. This comes at the cost of managing a larger distributed microservice configuration.

## ⚠️ Flaws of the Basic Flow in Banking (And How V2 Resolves Them)

While the **Basic Voice AI Flow** (`User -> STT -> LLM -> TTS -> User`) is a standard baseline for voice interaction, applying it directly to a production banking environment exposes several critical architectural and security flaws:

| Architectural Flaw in Basic Flow | Impact in Banking | V2 Resolution Mechanism |
| :--- | :--- | :--- |
| **No Authentication Gate** | Exposes private ledger data (balances, statements) without verification. | **Redis Session Intercept**: Every turn is validated against the `session-context-service`. Unauthenticated sessions are forced into an OTP/PIN sub-flow. |
| **Instant / Unconfirmed Writes** | User speech errors (e.g. misspelling names or amounts) trigger immediate, irreversible money transfers. | **2-Stage Write State Machine**: Mutative actions (transfers, card blocks) are staged in Redis as `StateAwaitingConfirmation`. They require positive user confirmation and a client-side idempotency token (`unique_ref_no`) before execution. |
| **Synchronous Database Writes** | Long-running database operations (MongoDB / Cassandra) block the audio loop, causing audio packet jitter and drops. | **Event Stream Offloading**: Write queries are decoupled. Transactions are verified in MongoDB, while analytical logs and transcripts are offloaded to Redis streams and consumed asynchronously by Cassandra workers. |
| **GPU Latency & Cost Spikes** | Simple or repetitive queries (greetings, ATM searches) hit the LLM, introducing high processing delays (>1.5s) and cost. | **Semantic Cache Deflection**: Orchestrator queries Qdrant vector store in parallel with speculative LLM inference. A hit $\ge 0.96$ similarity cancels the LLM thread and responds in <10ms. |
| **Prompt Injection & Scope Leaks** | Prompt injection attacks ("Ignore previous rules...") can leak internal system prompts or prompt malicious behavior. | **Regex Guardrails & Deflection**: Out-of-scope queries are blocked before LLM execution, returning static deflection responses directly. |

---

## ⚡ Core Technical Decisions & Optimizations

### 1. Multilingual Turn Supervisor
* **Unicode/Regex Language Classification**: The system intercepts the Speech-to-Text output and executes quick regex and Unicode character classification to detect Hindi/Hinglish vs. English.
* **Dynamic Template Swapping**: If Hindi/Hinglish is detected, the supervisor dynamically swaps system prompt templates, instructing the LLM to output in matching Hinglish. This maintains conversational comfort and natural flow without requiring translation middle-layers.

### 2. Semantic Caching & Load Shedding
* **Parallel Cache Probing**: When a transcribed utterance arrives, the `llm-micro-orchestrator` starts speculative LLM inference in the background while concurrently querying Qdrant for a semantic cache match.
* **Early Deflection**: If the Qdrant query returns a cache hit with a cosine similarity score $\ge 0.96$, the orchestrator immediately uses context cancellation to abort the pending LLM inference thread. This load-shedding mechanism immediately reclaims GPU cycles and delivers sub-10ms response latencies.

### 3. Transaction Safety & Saga Idempotency
* **2-Stage Write Confirmation**: Sensitive write operations (e.g., executing money transfers, blocking credit cards) require explicit user confirmation. The orchestrator transitions the session to `StateAwaitingConfirmation`.
* **Idempotent Token (`unique_ref_no`)**: When the user says "yes" to confirm, the client frontend attaches a client-side generated UUID (`unique_ref_no`).
* **Double-Charge Mitigation**: If the WebSocket drops and reconnects during the write execution, the retried transaction is matched against MongoDB's `unique_ref_no` index. The ledger returns the cached `payment_ref_no` instead of executing a duplicate charge.

### 4. Hybrid Neural TTS Pipeline
* **Kokoro-82M Integration**: Tries high-quality local neural Kokoro-82M first (accelerated via Apple Silicon MPS), falling back to Google Translate TTS and native Web Speech API.

---

## ⚙️ Ports and Endpoint Layout

* **Public Load Balancer / Gateway**: Port `9090`
* **LiveKit WebRTC Server**: Port `7880`
* **Local Ollama LLM Server**: Port `11434`
* **Qdrant Vector Database**: Port `6333`
* **MongoDB (Ledger Store)**: Port `27017`
* **Redis Cache (Transient Context)**: Port `6379`
* **Cassandra Database (Audits)**: Port `9042`
* **Session Context Service**: Port `9087`
* **LLM Inference Service**: Port `9091`
* **Semantic Cache Service**: Port `9089`
* **Tool Execution Service**: Port `9088`
* **LLM Micro-Orchestrator**: Port `9083`
* **Media Engine Service**: Port `9082`

---

## 🚀 Operations, Deployment & Container Orchestration

The v2 microservices architecture is containerized using Docker and Docker Compose to coordinate configuration, replication, and service-to-service communication.

### Directory Layout
* **`cmd/`**: Entry points for all 8 microservices and the observability CLI.
* **`internal/`**: Core logic including `contextmanager`, `mcp`, `db` managers, `audit`, and `telemetry`.
* **`native-kokoro/`**: Local Python fast-TTS service implementation.

### Single-Script Bootstrapping (`./start-app-v2.sh`)
The bootstrapper script automates local system dependency checks, Docker building, and model provisioning:
1. **Dependency Verification**: Confirms `docker`, `jq`, `bc`, and `go` versions are installed locally.
2. **TLS Certificate Provisioning**: Generates self-signed SSL/TLS certificates placed under the `certs/` directory to secure internal microservice communication.
3. **Local LLM Model Pre-pulling**: Connects to the local Ollama daemon to ensure the chat model (`qwen2.5:7b-instruct`) and the vector embedding model (`bge-m3`) are downloaded and ready before booting containers.
4. **Service Compiling & Docker Build**: Automatically compiles all Go microservices in the workspace and spins up the multi-service Docker container stack:
   ```bash
   # Build Go microservices, download model weights, and spin up containers
   ./start-app-v2.sh
   ```
   To force clean builds of all services, use the rebuild flag:
   ```bash
   ./start-app-v2.sh --force
   ```

### Termination & Teardown (`./terminate.sh`)
To cleanly stop all microservices, release network ports, and spin down local container services, execute:
```bash
./terminate.sh
```

---

## 📊 Observability, Distributed Tracing & APM

To monitor the performance of real-time voice hops, the system implements a strict observability system:
* **OpenTelemetry Instrumentation**: Go services utilize the OpenTelemetry SDK (`internal/telemetry`) to trace execution contexts across service boundaries (e.g. from the load balancer down to MongoDB queries and Qdrant cache searches).
* **Distributed Traces & APM**: Tracing spans record critical metadata, such as transaction IDs, session states, and database latency. Spans are exported to an OTLP-compliant collector.
* **Observability Monitoring CLI**: A dedicated utility located at `cmd/observability-cli` lets developers query and verify transaction trace statistics, measuring trace counts, query error boundaries, and rate limits in real-time.

---

## 🤖 Multi-Model Evaluation & Cloud Hot-Swapping

To support diverse model choices and evaluate accuracy-versus-latency trade-offs, the system features a model-agnostic inference layer:
* **Tested Local Models**: Fully compatible with Ollama local model inference:
  - `qwen2.5:7b-instruct` (provides balanced speed and structured prompt compliance).
  - `gemma2:2b` (ultralight, minimal footprint for fast turn-taking dialogues).
  - `gemma4:e4b` (precise prompt instruction compliance).
  - `gemma4:31b-mxfp8` (benchmarked high-parameter reasoning using block-quantized 8-bit formats optimized for Apple Silicon MPS execution).
  - `qwen3.6:35b` (evaluated for complex multilingual structural parsing and transaction routing under higher parameter constraints).
* **Cloud API Hot-Swapping**: The `llm-inference-service` exposes pluggable environment config slots. By supplying external cloud credentials (such as Google Gemini or OpenAI API keys) in the environment, the service seamlessly delegates inference, allowing the interviewer to compare cloud capabilities vs. local models with zero orchestrator code modifications.

---

## 🧪 E2E Integration Testing & Evaluation Framework

To guarantee compliance and safety in production banking environments, the codebase includes automated end-to-end (E2E) integration tests and conversational evaluations:

### 1. Go E2E Integration Tests
* **Execution**: Run with the E2E flag:
  ```bash
  ./run-evals.sh --e2e
  ```
  This triggers `go test -v ./internal/llm-micro-orchestrator/...` in the test runner.
* **Scope**: Validates orchestration loops, multi-turn Redis transaction states, mock MCP tools execution, and state-machine transitions (e.g. confirming card-block and money-transfer prompts).

### 2. LLM-as-a-Judge Conversational Evaluations
* **Execution**: Triggered automatically by the evaluation runner:
  ```bash
  ./run-evals.sh
  ```
* **Process**:
  1. The runner resets the MongoDB database to its seed state by hitting the tool service's `/reset` endpoint.
  2. The Python evaluator (`tests/evals/run_evals.py`) runs 6 representative banking conversation paths defined in the golden dataset (`tests/data/golden_dataset.json`).
  3. The conversation logs are processed through a validator model (the **LLM Judge**) which grades response accuracy, parameter capturing, compliance, and hallucination scores.
  4. The metrics are written to `tests/results/eval_results.json` and formatted as a Markdown report (`tests/results/eval_results.md`).

### 📈 Current SLO Latency & Evaluation Benchmarks

| Test Case ID | Scenario Name | Result | Latency (p50) | Compliance |
| :--- | :--- | :--- | :--- | :--- |
| `tc_greeting_flow_01` | Greeting and Introduction Flow | 🟢 PASS | `289.4ms` | Verified |
| `tc_balance_inquiry_01` | Account Balance Inquiry | 🟢 PASS | `129.7ms` | Verified |
| `tc_transactions_list_01` | Transaction Statement Check | 🟢 PASS | `134.0ms` | Verified |
| `tc_money_transfer_01` | Transfer Confirmation Dialog | 🟢 PASS | `129.7ms` | Verified |
| `tc_card_block_01` | Card Block Confirmation Dialog | 🟢 PASS | `125.4ms` | Verified |
| `tc_out_of_scope_01` | Prompt Injection / Deflection | 🟢 PASS | `4.5ms` | Verified |

* **Overall Pass Rate**: `100.0%`
* **p50 Latency**: `129.7ms`
* **Compliance Auditor Judge**: Verified under `qwen2.5:7b-instruct` / `gemma4:e4b` judge auditing.
