package llmorchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/telemetry"

	"github.com/gocql/gocql"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

type HistoryHealthStatus struct {
	Status     string `json:"status"`
	Redis      string `json:"redis"`
	Cassandra  string `json:"cassandra"`
	PendingLag int64  `json:"pending_lag"`
	Error      string `json:"error,omitempty"`
}

func StartHistoryConsumer(ctx context.Context, r *db.RedisManager, c *db.CassandraManager) {
	logger := telemetry.Logger("voice-ai-conversation-history-consumer")
	stream := "conversation_history_stream"
	group := "cassandra_history_group"
	consumer := "consumer-node-1"

	// Initialize group if not exists
	_ = r.Client.XGroupCreateMkStream(ctx, stream, group, "$").Err()

	// Start Health Check HTTP server on port 9085
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
			handleHistoryConsumerHealth(w, req, r.Client, c.Session)
		})
		srv := &http.Server{
			Addr:    ":9085",
			Handler: mux,
		}
		log.Println("History Consumer health server listening on port 9085")
		// Shutdown health check server when context is done
		go func() {
			<-ctx.Done()
			_ = srv.Shutdown(context.Background())
		}()
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[History Health Server Error] %v", err)
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				// Read new messages from stream
				entries, err := r.Client.XReadGroup(ctx, &redis.XReadGroupArgs{
					Group:    group,
					Consumer: consumer,
					Streams:  []string{stream, ">"},
					Count:    10,
					Block:    2 * time.Second,
				}).Result()

				if err != nil {
					if ctx.Err() != nil {
						return
					}
					continue
				}

				for _, streamEntry := range entries {
					for _, msg := range streamEntry.Messages {
						start := time.Now()

						// 1. Unpack values
						userID, _ := msg.Values["user_id"].(string)
						sessionID, _ := msg.Values["session_id"].(string)
						turnSeqStr, _ := msg.Values["turn_seq"].(string)
						turnSeq, _ := strconv.Atoi(turnSeqStr)
						role, _ := msg.Values["role"].(string)
						transcript, _ := msg.Values["transcript"].(string)
						intent, _ := msg.Values["intent"].(string)
						action, _ := msg.Values["action"].(string)
						result, _ := msg.Values["result"].(string)

						// 2. Write to Cassandra
						bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						writeErr := c.LogTurn(bgCtx, userID, sessionID, turnSeq, role, transcript, intent, action, result)
						cancel()

						duration := time.Since(start)
						durationMS := float64(duration.Nanoseconds()) / 1e6

						logRecord := telemetry.StructuredLog{
							Timestamp:    time.Now(),
							Level:        "INFO",
							Logger:       "voice-ai-conversation-history-consumer",
							Message:      "Processed turn write to Cassandra",
							Duration:     duration.String(),
							DurationMS:   durationMS,
							SessionID:    sessionID,
							DBSystem:     "cassandra",
							DBCollection: "conversations",
							DBOperation:  "insert",
						}

						if writeErr != nil {
							logRecord.Level = "ERROR"
							logRecord.Message = fmt.Sprintf("Failed to write to Cassandra: %v", writeErr)
							logger.ErrorContext(ctx, "history_write_failed", slog.Any("details", logRecord))
						} else {
							// Acknowledge the message in Redis Stream
							r.Client.XAck(ctx, stream, group, msg.ID)
							logger.InfoContext(ctx, "history_write_success", logRecord.SlogArgs()...)
						}
					}
				}
			}
		}
	}()
}

func handleHistoryConsumerHealth(w http.ResponseWriter, r *http.Request, redisClient *redis.Client, cassandraSession *gocql.Session) {
	w.Header().Set("Content-Type", "application/json")
	status := HistoryHealthStatus{
		Status:    "healthy",
		Redis:     "up",
		Cassandra: "up",
	}
	hasError := false

	// 1. Check Redis Connection
	if err := redisClient.Ping(r.Context()).Err(); err != nil {
		status.Redis = "down"
		status.Status = "unhealthy"
		status.Error = "Redis ping failed: " + err.Error()
		hasError = true
	}

	// 2. Check Cassandra Connection
	if cassandraSession == nil || cassandraSession.Closed() {
		status.Cassandra = "down"
		status.Status = "unhealthy"
		status.Error = "Cassandra session is closed or nil"
		hasError = true
	}

	// 3. Check Redis Stream Pending Backlog (Lag)
	if !hasError {
		pending, err := redisClient.XPending(r.Context(), "conversation_history_stream", "cassandra_history_group").Result()
		if err != nil {
			status.Status = "unhealthy"
			status.Error = "Failed to query Redis stream backlog: " + err.Error()
			hasError = true
		} else {
			status.PendingLag = pending.Count
			// If message backlog is too high, mark as degraded or unhealthy
			if pending.Count > 100 {
				status.Status = "degraded"
				status.Error = fmt.Sprintf("High backlog lag: %d unacknowledged messages", pending.Count)
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(status)
				return
			}
		}
	}

	if hasError {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(status)
}

type AuditHealthStatus struct {
	Status     string `json:"status"`
	Redis      string `json:"redis"`
	Cassandra  string `json:"cassandra"`
	PendingLag int64  `json:"pending_lag"`
	Error      string `json:"error,omitempty"`
}

func StartAuditConsumer(ctx context.Context, r *db.RedisManager, c *db.CassandraManager) {
	logger := telemetry.Logger("voice-ai-audit-consumer")
	stream := "audit_log_stream"
	group := "cassandra_audit_group"
	consumer := "consumer-node-1"

	// Initialize group if not exists
	_ = r.Client.XGroupCreateMkStream(ctx, stream, group, "$").Err()

	// Start Health Check HTTP server on port 9086
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
			handleAuditConsumerHealth(w, req, r.Client, c.Session)
		})
		srv := &http.Server{
			Addr:    ":9086",
			Handler: mux,
		}
		log.Println("Audit Consumer health server listening on port 9086")
		// Shutdown health check server when context is done
		go func() {
			<-ctx.Done()
			_ = srv.Shutdown(context.Background())
		}()
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("[Audit Health Server Error] %v", err)
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				entries, err := r.Client.XReadGroup(ctx, &redis.XReadGroupArgs{
					Group:    group,
					Consumer: consumer,
					Streams:  []string{stream, ">"},
					Count:    10,
					Block:    2 * time.Second,
				}).Result()

				if err != nil {
					if ctx.Err() != nil {
						return
					}
					continue
				}

				for _, streamEntry := range entries {
					for _, msg := range streamEntry.Messages {
						start := time.Now()

						// 1. Unpack values
						turnID, _ := msg.Values["turn_id"].(string)
						sessionID, _ := msg.Values["session_id"].(string)
						userID, _ := msg.Values["user_id"].(string)
						action, _ := msg.Values["action"].(string)
						args, _ := msg.Values["args"].(string)
						result, _ := msg.Values["result"].(string)
						traceParent, _ := msg.Values["traceparent"].(string)

						// 2. Extract Context from traceparent
						var tpCtx = context.Background()
						if traceParent != "" {
							tpCtx = otel.GetTextMapPropagator().Extract(tpCtx, propagation.MapCarrier{"traceparent": traceParent})
						}

						// 3. Start OTel span for async database write
						spanCtx, span := otel.Tracer("audit-consumer").Start(tpCtx, "cassandra.write_audit")

						// 4. Write to Cassandra Audit table
						writeErr := c.LogAuditEvent(spanCtx, userID, sessionID, turnID, action, args, result)
						span.End()

						duration := time.Since(start)
						durationMS := float64(duration.Nanoseconds()) / 1e6

						logRecord := telemetry.StructuredLog{
							Timestamp:    time.Now(),
							Level:        "INFO",
							Logger:       "voice-ai-audit-consumer",
							Message:      "Processed audit log write to Cassandra",
							Duration:     duration.String(),
							DurationMS:   durationMS,
							SessionID:    sessionID,
							TurnID:       turnID,
							DBSystem:     "cassandra",
							DBCollection: "audit_logs",
							DBOperation:  "insert",
						}

						if writeErr != nil {
							logRecord.Level = "ERROR"
							logRecord.Message = fmt.Sprintf("Failed to write audit to Cassandra: %v", writeErr)
							logger.ErrorContext(spanCtx, "audit_write_failed", slog.Any("details", logRecord))
						} else {
							// Acknowledge the message in Redis Stream
							r.Client.XAck(ctx, stream, group, msg.ID)
							logger.InfoContext(spanCtx, "audit_write_success", logRecord.SlogArgs()...)
						}
					}
				}
			}
		}
	}()
}

func handleAuditConsumerHealth(w http.ResponseWriter, r *http.Request, redisClient *redis.Client, cassandraSession *gocql.Session) {
	w.Header().Set("Content-Type", "application/json")
	status := AuditHealthStatus{
		Status:    "healthy",
		Redis:     "up",
		Cassandra: "up",
	}
	hasError := false

	// 1. Check Redis Connection
	if err := redisClient.Ping(r.Context()).Err(); err != nil {
		status.Redis = "down"
		status.Status = "unhealthy"
		status.Error = "Redis ping failed: " + err.Error()
		hasError = true
	}

	// 2. Check Cassandra Connection
	if cassandraSession == nil || cassandraSession.Closed() {
		status.Cassandra = "down"
		status.Status = "unhealthy"
		status.Error = "Cassandra session is closed or nil"
		hasError = true
	}

	// 3. Check Redis Stream Pending Backlog (Lag)
	if !hasError {
		pending, err := redisClient.XPending(r.Context(), "audit_log_stream", "cassandra_audit_group").Result()
		if err != nil {
			status.Status = "unhealthy"
			status.Error = "Failed to query Redis stream backlog: " + err.Error()
			hasError = true
		} else {
			status.PendingLag = pending.Count
			// If message backlog is too high, mark as degraded or unhealthy
			if pending.Count > 100 {
				status.Status = "degraded"
				status.Error = fmt.Sprintf("High backlog lag: %d unacknowledged messages", pending.Count)
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(status)
				return
			}
		}
	}

	if hasError {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(status)
}
