package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type AuditEvent struct {
	TurnID    string
	Event     string
	Payload   map[string]any
	Timestamp string
}

func main() {
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	fmt.Printf("\033[1;36m================================================================\033[0m\n")
	fmt.Printf("\033[1;36m 📊 Banking Voice AI Agent v2 - Observability & Metrics CLI\033[0m\n")
	fmt.Printf("\033[1;36m================================================================\033[0m\n")
	fmt.Printf("Connecting to Redis telemetry stream at %s...\n\n", redisAddr)

	client := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		log.Fatalf("Fatal: failed to connect to Redis: %v", err)
	}

	// Fetch all events from the audit stream
	entries, err := client.XRange(ctx, "audit_log_stream", "-", "+").Result()
	if err != nil {
		log.Fatalf("Fatal: failed to read audit stream: %v", err)
	}

	if len(entries) == 0 {
		fmt.Println("\033[1;33mNo telemetry logs found in the stream. Run a few voice turns first!\033[0m")
		return
	}

	var events []AuditEvent
	for _, entry := range entries {
		var turnID, eventName, payloadStr, ts string

		if val, ok := entry.Values["turn_id"].(string); ok {
			turnID = val
		}
		if val, ok := entry.Values["event"].(string); ok {
			eventName = val
		}
		if val, ok := entry.Values["payload"].(string); ok {
			payloadStr = val
		}
		if val, ok := entry.Values["timestamp"].(string); ok {
			ts = val
		}

		var payload map[string]any
		_ = json.Unmarshal([]byte(payloadStr), &payload)

		events = append(events, AuditEvent{
			TurnID:    turnID,
			Event:     eventName,
			Payload:   payload,
			Timestamp: ts,
		})
	}

	// Calculate metrics
	var (
		totalTurns         = 0
		cacheHits          = 0
		cacheMisses        = 0
		totalPrefillTokens = 0
		wastedPrefill      = 0
		haltsTriggered     = 0
		haltsSucceeded     = 0 // non-diverged
	)

	// Map to track turn outcomes
	turnCacheHit := make(map[string]bool)
	turnPrefillTokens := make(map[string]int)

	for _, ev := range events {
		switch ev.Event {
		case "dispatch":
			totalTurns++
			path, _ := ev.Payload["path"].(string)
			if path == "action" || path == "faq" {
				cacheHits++
				turnCacheHit[ev.TurnID] = true
			} else if path == "llm" {
				cacheMisses++
				turnCacheHit[ev.TurnID] = false
			}

		case "warm_outcome":
			tokens := 0
			if tVal, ok := ev.Payload["prefill_tokens"]; ok {
				switch v := tVal.(type) {
				case float64:
					tokens = int(v)
				case int:
					tokens = v
				case string:
					tokens, _ = strconv.Atoi(v)
				}
			}
			turnPrefillTokens[ev.TurnID] = tokens
			totalPrefillTokens += tokens

		case "halt_point":
			haltsTriggered++

		case "final_reconcile":
			// If we intercepted and did not diverge
			diverged, _ := ev.Payload["diverged"].(bool)
			state, _ := ev.Payload["state"].(string)
			if state == "intercept" {
				if !diverged {
					haltsSucceeded++
				}
			}
		}
	}

	// Calculate wasted prefill (tokens spent on turns that ended as cache hits)
	for turnID, isHit := range turnCacheHit {
		tokens := turnPrefillTokens[turnID]
		if isHit {
			wastedPrefill += tokens
		}
	}

	// Formatting outputs
	var cacheHitRate float64
	if totalTurns > 0 {
		cacheHitRate = (float64(cacheHits) / float64(totalTurns)) * 100
	}

	var wastedPrefillRatio float64
	if totalPrefillTokens > 0 {
		wastedPrefillRatio = (float64(wastedPrefill) / float64(totalPrefillTokens)) * 100
	}

	var haltPrecision float64
	if haltsTriggered > 0 {
		haltPrecision = (float64(haltsSucceeded) / float64(haltsTriggered)) * 100
	}

	fmt.Printf("\033[1;32m📋 SUMMARY METRICS REPORT:\033[0m\n")
	fmt.Printf("----------------------------------------------------------------\n")
	fmt.Printf("Total Dialog Turns Logged:  %d\n", totalTurns)
	fmt.Printf("  └─ Cache Hits (Action/FAQ):  %d\n", cacheHits)
	fmt.Printf("  └─ Cache Misses (LLM):       %d\n", cacheMisses)
	fmt.Printf("Cache Hit Rate:             \033[1;33m%.2f%%\033[0m\n\n", cacheHitRate)

	fmt.Printf("Total Prefill Tokens Spent: %d\n", totalPrefillTokens)
	fmt.Printf("Wasted Prefill (on Hits):   %d\n", wastedPrefill)
	fmt.Printf("Wasted-Prefill Ratio:       \033[1;33m%.2f%%\033[0m\n\n", wastedPrefillRatio)

	fmt.Printf("Halt Points Triggered:      %d\n", haltsTriggered)
	fmt.Printf("Halt Reconcile Successes:   %d\n", haltsSucceeded)
	fmt.Printf("Early-Halt Precision:       \033[1;33m%.2f%%\033[0m\n", haltPrecision)
	fmt.Printf("----------------------------------------------------------------\n")
	fmt.Println()

	// Print last 5 events
	fmt.Printf("\033[1;35m🕒 LAST 5 TELEMETRY EVENTS IN STREAM:\033[0m\n")
	fmt.Printf("----------------------------------------------------------------\n")
	startIdx := 0
	if len(events) > 5 {
		startIdx = len(events) - 5
	}
	for i := startIdx; i < len(events); i++ {
		ev := events[i]
		payloadBytes, _ := json.Marshal(ev.Payload)
		// Clean up output path names
		timeParts := strings.Split(ev.Timestamp, "T")
		timeStr := ev.Timestamp
		if len(timeParts) > 1 {
			timeStr = strings.Split(timeParts[1], ".")[0]
		}
		fmt.Printf("[%s] \033[1;34m%-15s\033[0m (Turn: %s) -> %s\n", timeStr, ev.Event, ev.TurnID, string(payloadBytes))
	}
	fmt.Printf("----------------------------------------------------------------\n")
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
