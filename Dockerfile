FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o media-engine cmd/media-engine/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o llm-orchestrator-server cmd/llm-orchestrator-server/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o session-context-service cmd/session-context-service/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o semantic-cache-service cmd/semantic-cache-service/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o llm-inference-service cmd/llm-inference-service/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o tool-execution-service cmd/tool-execution-service/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o conversation-history-consumer cmd/conversation-history-consumer/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -o audit-log-consumer cmd/audit-log-consumer/main.go

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/media-engine .
COPY --from=builder /app/llm-orchestrator-server .
COPY --from=builder /app/session-context-service .
COPY --from=builder /app/semantic-cache-service .
COPY --from=builder /app/llm-inference-service .
COPY --from=builder /app/tool-execution-service .
COPY --from=builder /app/conversation-history-consumer .
COPY --from=builder /app/audit-log-consumer .
COPY --from=builder /app/frontend ./frontend

# Expose default ports
EXPOSE 8080 8081

CMD ["./media-engine"]
