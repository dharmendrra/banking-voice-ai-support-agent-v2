# Architectural Blueprint: Distributed Voice AI Banking Support Agent (V2)

A production-grade, low-latency, highly decoupled Voice AI banking agent built from first principles in Go. The architecture is optimized for sub-300ms interaction latencies, strict transaction safety, and horizontal scaling. It is built to satisfy the core capabilities expected of a banking voice agent: bridging complex business requirements (real-time banking, compliance) with scalable distributed systems design.

---

## 🎨 Architectural Design Diagrams

### 1. Basic Voice AI Flow
This shows the sequential block flow of a standard Voice AI turn: capturing user microphone input, transcribing to text, generating an agent response, synthesizing to audio speech, and playing it back.

![Basic Flow](docs/images/basic_flow.jpg)

### 2. High-Level Design (HLD / HDD)
This diagram illustrates the fully decoupled 8-service architecture. Nginx serves as the single WebSocket entry point, balancing traffic to media engine replicas, which talk to the stateless micro-orchestrator. Data writes, caching, and analytics are isolated across Redis, MongoDB, Qdrant, and Cassandra.

![High-Level Design](docs/images/hdd_architecture.jpg)

### 3. Low-Level Design & Data Flow Diagram (LLD / DFD)
This diagram details the sequence of operations and real-time data flow (DFD) for a single user utterance. Audio streaming, Speech-to-Text, parallel vector cache probes, LLM generation, tool validation, and Text-to-Speech synthesis execute in a low-latency pipeline, while transaction logging is offloaded asynchronously to Cassandra.

![Low-Level Design](docs/images/lld_sequence_clean.jpg)

---

## 🔗 Deep-Dive Documentation

For granular details on our evolutionary designs, implementation details, port mappings, and performance latency reports, please check:
👉 **[Detailed Engineering](./README-detailed.md)**

---

## 🚀 Getting Started Quick Links

### Bootstrapping & Launch
```bash
# Provision databases, compile Go binaries, and start all microservice containers
./start-app-v2.sh
```
* Dashboard Control Panel: **[http://localhost:9090](http://localhost:9090)**

### Teardown & Stop
```bash
# Cleanly terminate and stop all active containers
./terminate.sh
```
