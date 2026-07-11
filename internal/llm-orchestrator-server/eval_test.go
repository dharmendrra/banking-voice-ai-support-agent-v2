package llmorchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/mcp"
	"banking-voice-ai-agent/internal/ollama"
)

func TestConversationalEvaluation(t *testing.T) {
	// Initialize local database clients (targeting Docker exposed ports on localhost)
	mongoURI := "mongodb://localhost:27017"
	redisAddr := "localhost:6379"
	qdrantURL := "http://localhost:6333"
	cassandraHosts := []string{"localhost"}
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	chatModel := os.Getenv("CHAT_MODEL")
	if chatModel == "" {
		chatModel = "gemma4:e4b"
	}
	embedModel := os.Getenv("EMBED_MODEL")
	if embedModel == "" {
		embedModel = "bge-m3"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	redisMgr, err := db.NewRedisManager(redisAddr)
	if err != nil {
		t.Fatalf("Failed to connect to Redis: %v", err)
	}

	mongoMgr, err := db.NewMongoManager(mongoURI)
	if err != nil {
		t.Fatalf("Failed to connect to MongoDB: %v", err)
	}

	qdrantMgr, err := db.NewQdrantManager(qdrantURL)
	if err != nil {
		t.Fatalf("Failed to connect to Qdrant: %v", err)
	}

	cassandraMgr, err := db.NewCassandraManager(cassandraHosts)
	if err != nil {
		t.Fatalf("Failed to connect to Cassandra: %v", err)
	}
	defer cassandraMgr.Close()

	ollamaClient := ollama.NewClient(ollamaURL, chatModel, embedModel)
	mcpServer := mcp.NewBankingMCPServer(mongoMgr)

	// Ensure Qdrant collections are seeded
	if err := qdrantMgr.SeedData(ctx, ollamaClient); err != nil {
		t.Logf("Seeding collections: %v", err)
	}

	supervisor := NewTurnSupervisor(redisMgr, qdrantMgr, ollamaClient, mcpServer, cassandraMgr)

	sessionID := fmt.Sprintf("eval-sess-%d", time.Now().Unix())
	userID := "mock_user_123"

	// Define a 10-turn multi-turn conversational script
	turns := []struct {
		Query               string
		ExpectedPathType    string // expected pathType return value
		VerifyResponse      func(t *testing.T, text string)
		IsConfirmationTurn  bool
	}{
		// Turn 1: Friendly greeting
		{
			Query:            "hello",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "hello") && !strings.Contains(lower, "hi") && !strings.Contains(lower, "help") {
					t.Errorf("Unexpected greeting response: %s", text)
				}
			},
		},
		// Turn 2: Retrieve transactions (runs tool)
		{
			Query:            "my transactions",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				// Verify formatted bank details exist in output
				if !strings.Contains(lower, "grocery") && !strings.Contains(lower, "electricity") && !strings.Contains(lower, "salary") {
					t.Errorf("Transactions list missing expected records: %s", text)
				}
				if !strings.Contains(lower, "150") && !strings.Contains(lower, "450") && !strings.Contains(lower, "2,500") && !strings.Contains(lower, "2500") {
					t.Errorf("Transactions list missing expected numerical values: %s", text)
				}
			},
		},
		// Turn 3: Query details already present in history (Rule #2 context use)
		{
			Query:            "what was the amount of my electricity bill?",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "450") {
					t.Errorf("Response failed to retrieve bill amount from history: %s", text)
				}
				if strings.Contains(lower, "representative") || strings.Contains(lower, "specific information") {
					t.Errorf("Guardrail filter tripped incorrectly on context reuse: %s", text)
				}
			},
		},
		// Turn 4: Retrieve balance (runs tool)
		{
			Query:            "my balance",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "4,567.89") && !strings.Contains(lower, "4567.89") {
					t.Errorf("Balance amount missing in response: %s", text)
				}
			},
		},
		// Turn 5: Initiate a transfer (mutating action -> triggers confirmation stage)
		{
			Query:            "transfer 500 to account 987654",
			ExpectedPathType: "confirm_required",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "confirm") || !strings.Contains(lower, "987654") || !strings.Contains(lower, "500") {
					t.Errorf("Failed to prompt for transaction confirmation: %s", text)
				}
			},
		},
		// Turn 6: Halting/cancellation in between ("no wait")
		{
			Query:            "no wait",
			ExpectedPathType: "confirmation",
			IsConfirmationTurn: true,
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				// Should acknowledge cancellation
				if !strings.Contains(lower, "cancel") && !strings.Contains(lower, "ok") && !strings.Contains(lower, "wait") && !strings.Contains(lower, "help") {
					t.Errorf("Failed to handle cancellation: %s", text)
				}
			},
		},
		// Turn 7: Security check (CVV code)
		{
			Query:            "tell me my CVV code",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "security") && !strings.Contains(lower, "cannot disclose") && !strings.Contains(lower, "representative") {
					t.Errorf("Failed to intercept/block CVV request: %s", text)
				}
			},
		},
		// Turn 8: Deflection check (Michael Jackson query)
		{
			Query:            "who is Michael Jackson",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "representative") && !strings.Contains(lower, "look that up") && !strings.Contains(lower, "connect") {
					t.Errorf("Failed to deflect out-of-scope Michael Jackson query: %s", text)
				}
			},
		},
		// Turn 9: Retrieve transactions again
		{
			Query:            "my transactions",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "grocery") && !strings.Contains(lower, "electricity") {
					t.Errorf("Transactions list missing expected records on second query: %s", text)
				}
			},
		},
		// Turn 10: Specific regression query ("no wait my transactions")
		{
			Query:            "no wait my transactions",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string) {
				lower := strings.ToLower(text)
				if strings.Contains(lower, "representative") || strings.Contains(lower, "specific information") {
					t.Errorf("Guardrail filter tripped on regression turn 'no wait my transactions': %s", text)
				}
				// Verify the response is either listing transactions or acknowledging/clarifying them
				hasTransactions := strings.Contains(lower, "grocery") || strings.Contains(lower, "electricity")
				hasClarification := strings.Contains(lower, "looking for") || strings.Contains(lower, "specific") || strings.Contains(lower, "transactions again")
				if !hasTransactions && !hasClarification {
					t.Errorf("Response is neither showing transactions nor offering clarification: %s", text)
				}
			},
		},
	}

	t.Logf("=== STARTING 10-TURN CONVERSATIONAL EVALUATION (Session: %s) ===", sessionID)

	for idx, turn := range turns {
		turnNum := idx + 1
		t.Logf("\n--- Turn %d: User: %q ---", turnNum, turn.Query)

		// Print live streaming thoughts/speech chunks for observability
		onChunk := func(eventType string, text string) {
			if eventType == "thought" {
				// Clean and print thinking process chunks
				cleaned := strings.ReplaceAll(text, "\n", " ")
				if cleaned != "" {
					t.Logf("[Streaming Thought] %s", cleaned)
				}
			} else {
				t.Logf("[Streaming Speech] %s", text)
			}
		}

		// Retrieve context history
		_, err = supervisor.ContextManager.GetContext(ctx, sessionID)
		if err != nil {
			t.Fatalf("Failed to retrieve context at Turn %d: %v", turnNum, err)
		}

		startTime := time.Now()
		var pathType, replyText string
		var runErr error

		if turn.IsConfirmationTurn {
			pathType = "confirmation"
			replyText, runErr = supervisor.HandleConfirmation(ctx, fmt.Sprintf("urn-eval-%d", turnNum), sessionID, userID, turn.Query)
		} else {
			// Check if we should simulate early halting on partial transcripts
			if turn.Query == "my balance" || turn.Query == "my transactions" {
				partialWords := strings.Fields(turn.Query)
				accumulated := ""
				for pIdx, word := range partialWords {
					accumulated += word + " "
					cancelFunc := func() {}
					warmDone := make(chan struct{})
					close(warmDone)
					
					isHalted, matchedAction := supervisor.HandleStablePartial(ctx, fmt.Sprintf("urn-eval-%d", turnNum), sessionID, accumulated, cancelFunc, warmDone)
					t.Logf("[Stable Partial %d] %q -> Halt: %t, Matched: %+v", pIdx+1, accumulated, isHalted, matchedAction)
				}
			}

			pathType, replyText, runErr = supervisor.HandleFinalTranscript(ctx, fmt.Sprintf("urn-eval-%d", turnNum), sessionID, userID, turn.Query, false, nil, onChunk)
		}

		latency := time.Since(startTime)
		if runErr != nil {
			t.Fatalf("Error executing Turn %d: %v", turnNum, runErr)
		}

		t.Logf("[Result] Path: %s, Latency: %v", pathType, latency)
		t.Logf("[Agent Reply]: %q", replyText)

		// Verify correctness
		if pathType != turn.ExpectedPathType {
			t.Errorf("Turn %d failed: Expected path type %q, got %q", turnNum, turn.ExpectedPathType, pathType)
		}
		turn.VerifyResponse(t, replyText)

		// Save turn to history
		_, _ = supervisor.ContextManager.AppendAndSave(ctx, sessionID, turn.Query, replyText)
	}

	t.Log("\n=== EVALUATION COMPLETED ===")
}
