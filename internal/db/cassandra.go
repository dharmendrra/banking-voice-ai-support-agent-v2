package db

import (
	"banking-voice-ai-agent/internal/telemetry"

	"context"
	"fmt"
	"log"
	"time"

	"github.com/gocql/gocql"
	"go.opentelemetry.io/otel/attribute"
)

type CassandraManager struct {
	Session *gocql.Session
}

func NewCassandraManager(hosts []string) (*CassandraManager, error) {
	cluster := gocql.NewCluster(hosts...)
	cluster.Timeout = 5 * time.Second
	cluster.Keyspace = "system"

	var session *gocql.Session
	var err error

	// Retry connection loop since Cassandra/ScyllaDB container takes time to start
	for i := 0; i < 15; i++ {
		session, err = cluster.CreateSession()
		if err == nil {
			break
		}
		log.Printf("Waiting for Cassandra/ScyllaDB to start... (Attempt %d/15, error: %v)", i+1, err)
		time.Sleep(5 * time.Second)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to connect to cassandra after retries: %w", err)
	}
	defer session.Close()

	// Create keyspace if not exists
	err = session.Query(`
		CREATE KEYSPACE IF NOT EXISTS banking_audit 
		WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}
	`).Exec()
	if err != nil {
		return nil, fmt.Errorf("failed to create keyspace: %w", err)
	}

	// Reconnect using the specific keyspace
	cluster.Keyspace = "banking_audit"
	realSession, err := cluster.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create keyspace session: %w", err)
	}

	// Create table if not exists matching clustering partition key: PRIMARY KEY ((user_id), conversation_id, turn_seq)
	err = realSession.Query(`
		CREATE TABLE IF NOT EXISTS conversation_history (
			user_id text,
			conversation_id text,
			turn_seq int,
			ts timestamp,
			role text,
			transcript text,
			intent text,
			action text,
			result text,
			PRIMARY KEY ((user_id), conversation_id, turn_seq)
		) WITH CLUSTERING ORDER BY (conversation_id ASC, turn_seq ASC)
	`).Exec()
	if err != nil {
		realSession.Close()
		return nil, fmt.Errorf("failed to create conversation_history table: %w", err)
	}

	// Create table if not exists for audit logs
	err = realSession.Query(`
		CREATE TABLE IF NOT EXISTS audit_logs (
			user_id text,
			session_id text,
			turn_id text,
			ts timestamp,
			action text,
			args text,
			result text,
			PRIMARY KEY ((user_id), session_id, turn_id)
		) WITH CLUSTERING ORDER BY (session_id ASC, turn_id ASC)
	`).Exec()
	if err != nil {
		realSession.Close()
		return nil, fmt.Errorf("failed to create audit_logs table: %w", err)
	}

	log.Println("Cassandra/ScyllaDB Keyspace and Tables initialized successfully.")
	return &CassandraManager{Session: realSession}, nil
}

func (c *CassandraManager) Close() {
	if c.Session != nil {
		c.Session.Close()
	}
}

func (c *CassandraManager) LogTurn(ctx context.Context, userID, conversationID string, seq int, role, transcript, intent, action, result string) error {
	ctx, span := telemetry.Step(ctx, "cassandra.log_turn",
		attribute.String("db.operation", "insert_turn"),
		attribute.String("db.role", role),
		attribute.String("db.intent", intent),
		attribute.String("db.action", action),
	)
	defer span.End()
	query := `INSERT INTO conversation_history (user_id, conversation_id, turn_seq, ts, role, transcript, intent, action, result) 
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	return c.Session.Query(query, userID, conversationID, seq, time.Now(), role, transcript, intent, action, result).WithContext(ctx).Exec()
}

func (c *CassandraManager) LogAuditEvent(ctx context.Context, userID, sessionID, turnID, action, args, result string) error {
	ctx, span := telemetry.Step(ctx, "cassandra.log_audit",
		attribute.String("db.operation", "insert_audit"),
		attribute.String("db.action", action),
	)
	defer span.End()
	query := `INSERT INTO audit_logs (user_id, session_id, turn_id, ts, action, args, result) 
	          VALUES (?, ?, ?, ?, ?, ?, ?)`
	return c.Session.Query(query, userID, sessionID, turnID, time.Now(), action, args, result).WithContext(ctx).Exec()
}
