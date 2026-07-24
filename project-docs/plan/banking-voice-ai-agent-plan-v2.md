# Voice AI Banking Agent — Plan v2: Parallel Warm with Mid-Flight Cache Interception

A local-first, concurrent voice banking support agent built with **Go**,
**Qdrant**, **Redis**, and **Ollama**, architected to preserve the state
boundaries needed to scale to **10M total users / 1M concurrent** in production.

This plan's defining choice is its **latency engine**: the LLM is warmed **in
parallel** while the customer is still speaking, and that warm-up is **halted
mid-utterance** the instant the semantic cache matches with *extreme* confidence.
The intent is to make cache **misses** feel instant (the model is already warm at
end-of-utterance) while still bailing out of the LLM early on the **hits**.

> **Scaffold, not production-accurate.** Local model choices are not certified for
> regulated banking accuracy. Security/compliance (auth, PII, audit) is scoped in
> §9 but not fully specified here.

> **Build scope (for implementers).** Build the local scaffold as **one replica
> per component** (one media engine, one orchestrator, one STT/TTS, Qdrant, Redis,
> Mongo, banking MCP) — **no redundancy, no horizontal scaling, no failover.**
> This is *not* a monolith and *not* an "only-one-orchestrator" design: it's the
> same distributed system at **minimum multiplicity**. Instance *count* is a
> production sizing parameter; the Go services stay **stateless** (all state in
> Redis) so 1 → N is deployment config, not a rewrite. Do **NOT** implement the
> production topology: no L4/L7 load balancers, no Envoy/API gateway, no service
> mesh, no Apache Kafka, **no redundant/scaled instances (multiple media
> engines / orchestrators / LLM workers), and no cross-instance failover.**
> `docs/production-architecture.drawio.svg` and `docs/FAILOVER.md` are
> **production reference only** — they show what the scaffold maps to at scale,
> not what to build now. Where those docs name a production component (e.g.
> Kafka), the scaffold uses the local stand-in named in this plan (e.g. Redis
> Streams).

> **The trade this design makes.** Parallel warming spends GPU prefill on turns
> that turn out to be cache hits. The mid-flight halt *reduces* that waste (it
> stops as soon as it's confident) but does not eliminate it. This design buys
> **low miss-latency with no filler crutch** at the cost of **more GPU per hit**.
> Adopt it only where miss-latency matters more than GPU spend, or where GPU
> headroom is abundant. The honest cost table is §7.

---

## 1. Hardware Profile (dev target)

Reference machine: **Apple Silicon M3 Pro (12-core CPU / 18-core GPU), 48 GB
unified memory** — Metal-accelerated Ollama, enough headroom to run the LLM,
embeddings, STT, and TTS concurrently with Qdrant, Redis, and the Go engine.

> **Constrained fallback (2020 Intel MacBook Pro, quad-core i5, 16 GB):**
> CPU-only, no LLM GPU acceleration. There, parallel warming is *not* worthwhile
> (no idle GPU to warm into) — disable it and run the sequential fallback (§8).
> Downgrade models to `llama3.2:3b-instruct`, `nomic-embed-text` (768-dim; set
> the Qdrant vector size to 768), faster-whisper `base`/`small`.

---

## 2. Architecture Overview

```
                        +----------------------------+
                        |  Client Application (UI)   |
                        +----------------------------+
                                      │  ▲
                     Bi-directional   │  │ Synth audio
                     Audio Stream     ▼  │
                        +----------------------------+
                        | Go Orchestrator / Media    |
                        |   Engine (warm supervisor) |
                        +----------------------------+
             │ PCM        │ stable          │ warm/decode      │ session
             ▼            ▼ transcript      ▼                  ▼
   +------------------+ +----------------+ +---------------+ +-----------------+
   | Speech Pipeline  | | Qdrant         | | Ollama        | | Redis           |
   | STT│VAD│TTS       | | (Semantic      | | (LLM +        | | (session state, |
   |                  | |  Cache: intent | |  embeddings)  | |  async streams) |
   |                  | |  + FAQ index)  | |               | |                 |
   +------------------+ +----------------+ +---------------+ +-----------------+
```

- **Media Connection Gateway (LiveKit):** Manages incoming client WebRTC connections, audio encoding/decoding, and WebRTC audio track streaming.
- **Go Media Engine:** Decoupled microservice that connects to the LiveKit server to receive audio track streams, runs energy-based VAD (Voice Activity Detection), triggers barge-in control frames, and calls local Whisper/Piper containers.
- **Go Orchestrator:** Decoupled backend service on port 8081. Runs the §4 warm-supervisor state machine, queries the Qdrant semantic cache, manages Redis session state, and streams LLM deflector tokens back to the Media Engine. Completely stateless (all state in Redis).
- **Qdrant — Semantic Cache Store:** two **user-independent** vector collections (§5), using `bge-m3` embeddings (multilingual, 1024-dim).
- **Session Cache (Redis):** session state + async/durable task streams. Never holds customer financial data (§9).
- **Core Banking System (MCP / MongoDB):** Holds the transactional ledger, customer accounts, and card statuses.
- **Durable Historical Logs (Cassandra):** Receives historical conversation audio logs and transcript records asynchronously on session closure.
- **Inference Engines:** Ollama (hosted on host macOS for Metal acceleration) for LLM deflections; Whisper/Piper (hosted in Docker) for speech-to-text and text-to-speech.

**Audio format contract:** 16 kHz mono 16-bit PCM (or Opus decoded at the edge),
fixed at the WebSocket boundary so STT/VAD/TTS agree.

### Data & Media Services Stack (`docker-compose.yml`)

To orchestrate your state, media gateway, and database layers locally, spin up this configured stack optimizing resource parameters.

```yaml
# `version:` is obsolete in modern Docker Compose (Compose Spec) and is omitted.

services:
  qdrant:
    image: qdrant/qdrant:latest
    container_name: local_qdrant
    ports:
      - "6333:6333"
      - "6334:6334"
    volumes:
      - qdrant_storage:/qdrant/storage
    deploy:
      resources:
        limits:
          memory: 2g
    healthcheck:
      test: ["CMD", "sh", "-c", "bash -c ':> /dev/tcp/127.0.0.1/6333'"]
      interval: 10s
      timeout: 3s
      retries: 5

  redis:
    image: redis:7.2-alpine
    container_name: local_redis
    ports:
      - "6379:6379"
    command: redis-server --appendonly yes --maxmemory 1gb --maxmemory-policy allkeys-lru
    volumes:
      - redis_data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 3s
      retries: 5

  mongodb:
    image: mongo:6.0
    container_name: local_mongodb
    ports:
      - "27017:27017"
    volumes:
      - mongo_data:/data/db
    deploy:
      resources:
        limits:
          memory: 2g
    healthcheck:
      test: ["CMD", "mongosh", "--eval", "db.adminCommand('ping')"]
      interval: 10s
      timeout: 3s
      retries: 5

  cassandra:
    image: cassandra:4.1
    container_name: local_cassandra
    ports:
      - "9042:9042"
    volumes:
      - cassandra_data:/var/lib/cassandra
    deploy:
      resources:
        limits:
          memory: 2g
    healthcheck:
      test: ["CMD", "sh", "-c", "bash -c ':> /dev/tcp/127.0.0.1/9042'"]
      interval: 15s
      timeout: 5s
      retries: 5

  livekit:
    image: livekit/livekit-server:latest
    container_name: local_livekit
    command: --dev --config /etc/livekit.yaml
    ports:
      - "7880:7880"
      - "7881:7881"
      - "7882:7882/udp"
    volumes:
      - ./livekit.yaml:/etc/livekit.yaml
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:7880/healthz"]
      interval: 10s
      timeout: 3s
      retries: 5

  whisper-stt:
    image: fedirz/faster-whisper-server:latest-cpu
    container_name: local_whisper
    ports:
      - "8001:8000"
    environment:
      - WHISPER_MODEL=tiny
    deploy:
      resources:
        limits:
          memory: 2g
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8000/healthz"]
      interval: 10s
      timeout: 3s
      retries: 5

  piper-tts:
    image: rhasspy/piper:latest
    container_name: local_piper
    entrypoint: ["sh", "-c", "/app/piper --model /app/en_US-lessac-medium.onnx --output_raw | nc -l -p 8000"]
    ports:
      - "8002:8000"
    deploy:
      resources:
        limits:
          memory: 1g

volumes:
  qdrant_storage:
  redis_data:
  mongo_data:
  cassandra_data:
```

> The `llm-orchestrator-server` should `depends_on` both services with
> `condition: service_healthy` so it doesn't issue gRPC/RESP calls before Qdrant
> and Redis are ready.

---

## 3. The core idea

Two things run concurrently from the first **stable** STT token:

- **Branch A — LLM warm:** speculatively prefill the query tokens into the KV
  cache (on top of a pre-warmed static prefix), so the model is ready to decode
  the instant it's needed.
- **Branch B — cache probe:** on each stable partial (and the final), embed the
  text and query both semantic-cache indexes.

A supervisor watches Branch B. The **moment** it produces an *extreme-confidence*
match, it **halts Branch A** (aborts the prefill, reclaims the GPU) and commits
the turn to the cache path. If no extreme match appears, Branch A was warming all
along and the miss is served fast.

The subtlety that makes this safe: **the mid-flight halt stops *compute* only.**
It never *executes a bank action* on a partial — action execution waits for the
final transcript (and, for money-movement, explicit confirmation). Halting early
is free to get wrong (worst case: re-warm); executing early is not, so we don't.

---

## 4. The per-turn state machine

Two thresholds, `EXTREME > NORMAL` (e.g. `EXTREME = 0.97`, `NORMAL = 0.94` cosine):

```
TURN_START
  • Prewarm STATIC prefix (system prompt + conversation history) into KV cache.
    (The shared system prompt is prefix-cached once and reused across all turns.)
  • Open STT stream + VAD.

STREAMING  — for each STABLE partial token:
  A) extend LLM KV cache with the new tokens        (Branch A: speculative prefill)
  B) embed partial + query action & FAQ indexes     (Branch B: cache probe)
  evaluate Branch B:
     • best ACTION match ≥ EXTREME  → HALT Branch A, enter CACHE_INTERCEPT
     • else                         → keep both branches running

END_OF_UTTERANCE (VAD final → authoritative FINAL transcript):
  if state == CACHE_INTERCEPT:
     • re-confirm FINAL still matches ≥ NORMAL
       - read-only action  → call Banking MCP tool → template → TTS
       - write/money-move  → confirmation sub-dialog, THEN MCP tool (unique_ref_no)
       - divergence (FINAL no longer matches) → un-halt: cold-prefill + decode
  else:  # never hit EXTREME during speech
     • final cache lookup, precedence action > FAQ > LLM
       - action match ≥ NORMAL → call Banking MCP tool; abort Branch A
       - FAQ match ≥ NORMAL    → canned answer TEXT → TTS (no audio store); abort Branch A
       - no match (MISS)       → Branch A already warm → DECODE → TTS  ← overlap reclaimed
```

### Why an *extreme* threshold for the early halt

Halting Branch A is only cheap if you rarely have to un-halt it. If you halt on a
merely-`NORMAL` early match and the completed utterance diverges, you've discarded
the warm KV cache and must **cold-prefill at end-of-utterance** — the worst
latency case. So the early-halt bar is deliberately high (`EXTREME`): halt only
when the partial is so close to a known intent that completion almost certainly
won't change the verdict. Moderate matches keep *both* branches alive and defer
to the final-transcript decision.

### What "halted in the middle" cancels

| Halt target | Cancel on extreme match? | Why |
|---|---|---|
| Branch A prefill/decode | **Yes, immediately** | pure compute; safe to abort, reclaims GPU |
| Bank action execution | **No — waits for FINAL** | acting on a partial is unsafe (esp. money-movement) |
| TTS output | Gated on the committed path | never speak LLM tokens once committed to cache, and vice-versa |

Cancellation is behind an **interface** (`LLMWarmer.Cancel(reason)`) so the halt
logic is identical regardless of what the runtime supports:

```
type LLMWarmer interface { StartWarm(ctx, tokens); Cancel(reason) }
// local (Ollama): Cancel() LOGS the halt point; the prefill continues and its
//                 result is discarded (no GPU reclaim — see below).
// prod (vLLM/TGI/custom): Cancel() aborts the request and reclaims the GPU;
//                 the static-prefix KV cache is retained (shared, reused next turn).
```

> **Cancellation is a swappable backend detail, not a go/no-go.** Ollama's API
> does not cleanly cancel an in-flight prefill / reclaim GPU. **The local build
> proceeds anyway:** on `CACHE_INTERCEPT` it calls `Cancel()`, which **logs the
> halt point** and discards the (still-completing) prefill result. This validates
> the **interception logic** — *where and when* the halt fires — but **not** the
> GPU/latency saving, which is only realized in production behind a cancel-capable
> runtime. Locally you still pay the warm cost on hits; you just don't reclaim it
> (fine on a single-user M3 Pro). See §11 for the logging that proves the halt
> decisions.

---

## 5. Dispatch precedence & the semantic cache

Two Qdrant collections, both **user-independent** (shared across all customers,
no per-user vectors), pinned to the **same embedding model / dimensions**
(`bge-m3`, 1024). **There is no `user_id` payload filter** — there is
no per-user data to isolate; personalization happens later via the authenticated
bank call. **This is not a knowledge base and there is no RAG.**

- **Action-intent index (takes precedence):** points are canonical utterances
  with payload `{ intent, response_template, bank_action }` — routes only, no user
  data, no answers — where `bank_action` names a **Banking MCP tool** + arg
  template (see §5a). *"What's my balance?"* / *"what's left in my account?"* map
  to the same **balance** intent.
- **Informational / FAQ index (fallback):** canonical questions with `{ answer }`,
  **seeded from the bank's public documentation** (help-centre / FAQ pages).
  Informational only — no user data. *Rate/fee figures are time-varying and go
  stale; don't treat seeded numbers as authoritative (see §6).*

**Query both in parallel; dispatch `action > FAQ > LLM`.** Actionable intents
**must win** over informational ones — *"what's my balance"* must fetch the
balance, never return the FAQ *"how to check your balance"* (a real anti-pattern
in production bank assistants). Ordering is not a latency lever (ANN lookups are
sub-millisecond); the two indexes stay separate so actions take precedence.

Action execution: skip the LLM, take the **authenticated user identity from
session context** (never from the query text), **call the mapped Banking MCP
tool** (`bank_action` → tool + args, `user_id` injected) with
`context.WithTimeout`, fill the template, stream to TTS. Read-only intents route
on match; **write / money-movement intents require explicit confirmation and
disambiguation** (never a similarity threshold alone) and pass a client
`unique_ref_no` (the bank dedupes on it).

### 5a. Banking Backend (Local)

The "bank system" is a **local Banking MCP server backed by MongoDB** — the source
of truth for all user-specific data and transactions, called via **MCP tool
calls**. The agent never persists this data. (Local stand-in for the production
**Tool Gateway / Tool Call service**.)

- **MCP tools** (each takes an injected `user_id` from the authenticated session,
  never from the query): `get_balance(user_id)`, `get_transactions(user_id, n)`,
  `get_due_date(user_id, card)`, `block_card(user_id, card)` [write],
  `transfer(user_id, to, amount, unique_ref_no)` [**write / money-movement**].
- **MongoDB collections:** `users`, `accounts`, `transactions`, `cards`.
- **Payment reference (`unique_ref_no`):** `transfer` requires a client-generated
  `unique_ref_no` (ICICI `UniqueRefNo`, HDFC/Axis `ReqRefId`, UPI `txnId`); the
  bank **dedupes on it** — a repeat returns "Duplicate transaction". The scaffold
  simulates this with a **unique index** on `unique_ref_no`. Reconcile an
  indeterminate outcome by re-submitting the *same* reference; never a new one
  (see `docs/FAILOVER.md` §B3).
- **Identity:** no auth locally (out of scope) — a **fixed mock `user_id`** per
  session is injected into every MCP call.
- Informational content (FAQs, bank docs) does **not** use this path — it is
  seeded into the Qdrant FAQ index (§5 / Phase 2). Only user-specific/transactional
  data uses the Banking MCP server.

### 5b. Local Build Pieces (the edges the flow connects to)

- **Client / UI:** minimal **browser page** — `getUserMedia` → PCM/Opus →
  **WebSocket** → Go engine; plays returned TTS frames.
- **Transport:** **WebSocket**.
- **Identity:** fixed **mock `user_id`** per session (no auth).
- **STT mode:** **streaming with stable partials** — v2 warms the LLM on the
  stable partial tokens as the customer speaks (this is the whole premise; unlike
  a final-only setup, v2 *requires* streaming STT).
- **LLM system prompt:** author the **deflector/guardrail prompt** per §6 /
  `docs/HALLUCINATION_GUARDRAILS.md`.
- **Cache seed:** ~5 action intents (balance, transactions, transfer, card block,
  due date) + ~5–10 FAQs from bank public docs; embed + upsert via a bootstrap
  script.

### 5c. Dialog Design & Language Support

- **Follow-ups:** The LLM is used to resolve context-dependent user utterances (e.g. "show recent ones" referring to transactions) into a standalone request that re-enters the dispatch path. Correct banking facts are always fetched dynamically via the action path; the LLM itself never states or invents the fact.
- **Slots:** For transaction arguments, the orchestrator performs deterministic slot-filling utilizing localized templated prompts (e.g., extracting source/target account and transfer amount). The LLM is strictly kept off the execution/money path.
- **Confirmation:** The system uses deterministic, localized templated confirmation dialogue before executing any write transactions (such as transfer or card block), preventing accidental executions.
- **Language:** Dual-language support for **English + Hindi** — employing multilingual STT, EN+HI TTS voices (e.g. Piper configured with localized phonetic files), and real-time language detection.
- **FAQ:** The Qdrant FAQ index stores localized Q&A records in both English and Hindi to handle customer queries in either language.

#### Core Dialog & Localization Decisions

*   **Q:** How should the agent handle follow-ups that depend on prior turns — e.g. *"and last month's?"* after *"what were my transactions?"* (per-utterance intent matching won't catch these on its own)?
    *   **A:** **LLM handles follow-ups** to resolve context-dependent utterances into a standalone request that re-enters the dispatch path (action path fetches data; LLM never states the fact).
*   **Q:** When an action needs parameters the user didn't fully give (e.g. *"transfer money"* with no amount/recipient), how are they collected?
    *   **A:** **Deterministic slots** are filled utilizing localized templated prompts, keeping the LLM off the execution/money path.
*   **Q:** How should a money-movement confirmation be done before executing a transfer?
    *   **A:** A **templated confirm** dialogue (localized) is used.
*   **Q:** The bank is India-based. What language(s) should the scaffold support (STT/TTS/seed content)?
    *   **A:** **Multilingual** (English + Hindi) support is required.

---

## 6. LLM guardrails (no-RAG safe-deflector)

There is no RAG. Correct facts come only from deterministic sources — the
**action/API path** (account data) and the **curated FAQ index** (top
informational answers). The LLM fallthrough is therefore **not a
question-answerer**; it is a **contained safe-deflector**. Consequence:
informational coverage = exactly what is curated in the FAQ index; the long tail
**escalates to a human**. Safer and simpler, more escalations. Revisit (add RAG)
only if measured informational-escalation volume is too high.

Guardrail set (containment):

1. **Trusted-source rule** — the LLM may only relay facts that came from a bank
   API result or a curated FAQ entry; never assert a rate/fee/balance/procedure
   on its own.
2. **Output filter (backstop)** — before TTS, block any specific value (balance,
   rate, fee, date, account number) that did not originate from a trusted source;
   an un-sourced value → suppress → escalate.
3. **Public/private separation** — never speak an account-specific value except
   from the API path.
4. **Scope / advice refusal** — financial advice or out-of-scope → refuse + offer
   a human.
5. **Escalate-to-human** as the default for anything factual or uncertain.
6. **System-prompt role-limit** — constrain the LLM to conversational glue
   (greetings, clarifications, "let me connect you"); forbid factual bank claims.

A better model lowers hallucination *rate* and refuses more reliably, but cannot
*know* bank-specific or account facts — so the fix is containment, not model
quality.

---

## 7. Honest cost / latency characterization

Compared against a **sequential baseline** (no warming: decide on the final
transcript, mask miss-latency with a pre-rendered filler clip — `$0` LLM on hits,
higher masked latency on misses):

| Turn outcome | Sequential baseline | This design (warm + mid-flight halt) |
|---|---|---|
| **Hit, caught early** (extreme match mid-speech) | `$0` LLM | prefill spent up to the halt point only, then reclaimed — small waste |
| **Hit, caught only at final** | `$0` LLM | *full* query prefill wasted |
| **Miss** | cold prefill after VAD, **masked by filler** | warm → decode immediately, **no filler** — real latency win |
| **False early halt** (extreme, then diverges) | n/a | worst case: cold re-prefill at final — pay the penalty |

Takeaways: the win is entirely on **miss** turns; the cost is wasted prefill on
**hit** turns, shrunk (not removed) by the mid-flight halt in proportion to how
*early* the extreme match fires. At 1M concurrent that wasted prefill is real
money — so warming must be **load-sheddable** (§8).

---

## 8. Load-shedding: warming is a knob

Warming must be able to turn off under pressure and degrade to the sequential
baseline (§7):

- **GPU-pressure backpressure:** above a utilization watermark, disable Branch A;
  the turn falls back to "decide on final, mask miss with filler." No correctness
  change — only the latency profile degrades.
- **Per-tenant / per-route policy:** enable warming only where miss-latency
  matters.
- **Keyword pre-gate (optional):** a cheap deterministic keyword spotter on the
  first 1–2 words *may* suppress warming when a strong transactional keyword
  appears (likely hit). Use it **only as a warm/don't-warm hint** — never as the
  dispatch mechanism (dispatch stays semantic on the final transcript, so
  paraphrases without keywords still hit).

---

## 9. Cross-cutting constraints

- **Stateless Go engine:** no global vars / in-memory session maps; all
  operational state in Redis so instances scale horizontally behind a gateway.
- **The agent never persists customer financial data:** bank data (balances,
  transactions, PII) is fetched **live per turn**, held transiently in memory only
  long enough to speak it, and **never written to Qdrant or Redis**. Caches hold
  only user-independent content. The bank system is the source of truth.
- **Non-blocking execution:** pass immutable payloads over channels; enforce
  `context.WithTimeout` on every remote hop (Qdrant, Redis, Ollama, bank API);
  `sync.Pool` buffers must be zeroed and never carry cross-request data.
- **Security & compliance (to be specified):** caller identity verification before
  any account access; PII/PCI handling (encryption in transit + at rest,
  transcript redaction, TTL/purge of session + raw audio); immutable audit log of
  what the agent said/did.

### Operational behavior *(defaults — provisional)*

- **Escalation (no human locally):** speak a localized "let me connect you to an
  agent" message, emit an **escalation event to the async stream** (feeds the
  escalation-rate eval metric), and park/end the session.
- **Live-path failure** (MCP timeout, MongoDB down, Ollama error): speak a
  localized "I'm having trouble accessing that right now" and **escalate/end**.
  Retry idempotent **reads** once; **never** retry writes (`docs/FAILOVER.md`).
  Log the failure.
- **Conversation context / session lifetime:** rolling **last ~5 turns** in Redis
  for follow-up resolution + the LLM prompt; session **TTL ~30 min**; **purge on
  disconnect**.

---

## 10. Implementation phases

- **Phase 0 — Speech I/O Gateway:** Integrate LiveKit WebRTC audio track ingestion; hook up the client mic; connect VAD to the LiveKit track; link Whisper STT (port 8001) and Piper TTS (port 8002) container API routes; implement barge-in interrupts.
- **Phase 1 — Prefix KV cache manager:** prewarm the shared system prefix; measure reuse hit-rate and prefill cost.
- **Phase 2 — Semantic cache store:** two Qdrant collections (action + FAQ), HNSW `EfConstruct: 100`, `M: 16`, 1024-dim; content pipeline to author intents/FAQs.
- **Phase 3 — Speculative Ollama client & Cassandra logging:** incremental prefill; `Cancel()` log-only no-op locally. Create Cassandra keyspace and tables for `conversations` history; implement async sinking of transcripts and meta logs from Redis Streams to Cassandra.
- **Phase 4 — Warm supervisor:** the §4 dual-branch state machine — extreme-halt, final-transcript dispatch with `action > FAQ > LLM` precedence, false-halt cold-restart path, guardrails (§6).
- **Phase 5 — Load-shedding knob:** GPU watermark → disable warming → sequential
  fallback (§8).
- **Phase 6 — Evals (prove correctness, gate regressions):** versioned golden sets
  + a replay harness + CI gate — see `docs/EVALS.md`. **Release-blocking:**
  money-movement safety (confirmation, never-on-partial, duplicate-ref rejected,
  never blind re-send) and guardrail/hallucination red-team. **Trend-gated:**
  intent-dispatch accuracy (also **tunes `EXTREME`/`NORMAL`/`0.94` offline**), STT
  WER on numbers/names/amounts, e2e task success + latency, and the v2
  interception suite (early-halt precision from the §11 logs). Green money-movement
  + guardrail suites = "the scaffold works safely," not just "it runs."

---

### Structured log events (the local build's primary deliverable)

Because local Ollama can't reclaim mid-flight (§4), **logging is how v2 proves its
logic locally.** Emit one structured event per decision point, correlated by
`turn_id`:

- `warm_start` — `{turn_id, at_token, static_prefix_reused}`
- `cache_probe` — per stable partial: `{turn_id, partial_len, best_action_score, best_faq_score}`
- `halt_point` — extreme match mid-stream: `{turn_id, at_token, action_intent, score, EXTREME}` — **"HALT would fire here"** (locally: prefill continues + discarded; prod: cancelled)
- `dispatch` — `{turn_id, path: action|faq|llm, matched_intent?, score?}`
- `final_reconcile` — `{turn_id, state: intercept|no_intercept, diverged?}`
- `warm_outcome` — `{turn_id, prefill_tokens, used|discarded, would_have_reclaimed}`

From these you read directly: *when* the halt fires (`at_token` vs. utterance
length = how much prefill a real cancel would save), whether it was a **cache-hit
(match)** or **cache-miss**, and the false-halt rate. **Cache-hit vs. cache-miss
is exactly `dispatch.path ∈ {action, faq}` vs. `= llm`.**

### Metrics (some only fully meaningful in production)

- **Perceived latency** end-of-speech → first TTS audio, bucketed by
  {hit-early, hit-final, miss, false-halt}.
- **Wasted-prefill ratio** — prefill tokens spent on turns that ended as cache
  hits ÷ total prefill. Locally this is the *waste*; the *saving* a real cancel
  would recover is `Σ(prefill_tokens after halt_point)` — computable from the logs
  above, realized only in production.
- **Early-halt precision** — of turns halted on `EXTREME`, how many the final
  transcript confirmed (low → `EXTREME` too loose, false-halt cold-restarts hurt).
- **Cache hit-rate** and **intent-type distribution** (validate the assumption
  that transactional traffic dominates; don't hardcode it).
- **GPU utilization** vs. the load-shed watermark (production).

Tune `EXTREME` / `NORMAL` and the watermark from these, not from guesses. If the
wasted-prefill ratio is high and miss-latency isn't actually hurting UX, the
sequential baseline is the correct architecture and this design should be shelved.
