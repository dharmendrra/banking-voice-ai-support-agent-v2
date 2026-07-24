# Conversation-History Storage

Where conversation history lives — and why it is **not** the banking store.

Conversation history is an **append-only, immutable, write-heavy, ever-growing**
event log (billions of records at 10M users), read mostly for audit/QA and
occasional per-user lookups, and it is **PII-laden with regulated retention**.
That profile does not match an operational document DB — so it is kept separate
from the banking MongoDB (which is the small, hot, rich-query financial
source-of-truth).

---

## Two layers, two stores

| Layer | Store | Role |
|---|---|---|
| **Working context** (last N turns for the LLM prompt) | **Redis** (`session:{id}:context`, TTL, purge on call-end) | hot, low-latency, ephemeral |
| **Durable history** (full transcripts, audit, per-user lookups) | **Cassandra** (production) | append-heavy, retained, operational reads |

The durable store does **not** serve the low-latency turn — that's Redis. So it
only needs cheap high-volume writes + occasional reads.

## Why Cassandra (production)

- **Write-optimized, append-heavy** — matches the workload; massive write
  throughput, horizontally scalable, no single write bottleneck.
- **Per-user / per-session point + range lookups at scale** — "fetch this user's
  conversation, ordered by time" is a single-partition read.
- **Per-row TTL** — retention policies (auto-expiry) fall out naturally.
- The classic "chat/message history at scale" choice.
- **ScyllaDB** is a drop-in, C\*-compatible alternative (C++ rewrite) — lower
  latency and ops cost; use it if you want better perf/economics.

### Data model (partition + clustering)

```
PRIMARY KEY ((user_id), conversation_id, turn_seq)
  partition key : user_id            -- all of a user's history co-located
  clustering    : conversation_id, turn_seq (time-ordered)
  columns       : ts, role, transcript, intent, action, result, ...
  TTL           : per retention policy
```

This gives efficient "all conversations for user U" and "conversation X in order."

## The caveat — Cassandra is the *operational* tier, not analytics

Cassandra is great for point/range reads by partition, but **poor at ad-hoc
aggregation** ("count balance intents across all users last month"). So:

- **Operational per-user/session history lookups → Cassandra.**
- **Analytics / QA aggregation → ClickHouse or a warehouse** (BigQuery/Snowflake).
- **Cheap long-term compliance archival → object storage** (S3/GCS + Parquet),
  lifecycle-tiered.

Production shape: **Orchestrator → Kafka → consumer → fan-out**: Cassandra
(operational) + ClickHouse/warehouse (analytics) + object storage (archival).
Kafka is the ingest/transport; Cassandra is one sink, not the whole story.

## Local scaffold

- **Cassandra runs locally and in production** (single node locally, `cassandra:4.1`
  in `docker-compose.yml`, keyspace `voiceagent`, table `conversations`). Working
  context stays in **Redis**; each turn is sinked to Cassandra by the orchestrator
  (`internal/history`), fire-and-forget off the live path. On connection failure it
  degrades to a **log-only mock**, so the app still runs without the container.
- Locally the orchestrator writes turns **directly** to Cassandra; in production
  the write is decoupled through **Kafka → consumer → Cassandra** (Redis Streams is
  the local Kafka stand-in). Same table either way.
- **Never** put conversation history in the banking MongoDB.

## Summary

- Working context = Redis; durable history = **Cassandra** (local + production),
  fed by the Kafka consumer.
- Cassandra = operational history lookups; pair with ClickHouse/warehouse for
  analytics and object storage for archival.
- Conversation history is a different domain from banking data — keep it out of
  the banking Mongo.
