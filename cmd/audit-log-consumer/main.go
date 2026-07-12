package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/llm-micro-orchestrator"
	"banking-voice-ai-agent/internal/telemetry"
)

func main() {
	log.Println("Starting Audit Log Consumer...")

	tShutdown, logger, err := telemetry.Init(context.Background(), "voice-ai-audit-consumer")
	if err != nil {
		log.Fatalf("Fatal: Telemetry initialization failed: %v", err)
	}
	defer func() { _ = tShutdown(context.Background()) }()
	slog.SetDefault(logger)

	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	cassandraHostsStr := getEnv("CASSANDRA_HOSTS", "localhost")
	cassandraHosts := strings.Split(cassandraHostsStr, ",")

	redisMgr, err := db.NewRedisManager(redisAddr)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Redis: %v", err)
	}

	cassandraMgr, err := db.NewCassandraManager(cassandraHosts)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Cassandra: %v", err)
	}
	defer cassandraMgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	llmorchestrator.StartAuditConsumer(ctx, redisMgr, cassandraMgr)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down Audit Log Consumer...")
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
