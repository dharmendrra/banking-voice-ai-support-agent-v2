package llmorchestrator

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEndToEndConversationalEvaluation(t *testing.T) {
	// Target the NGINX load balancer exposed on port 9090 on the host
	baseURL := os.Getenv("E2E_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:9090/orchestrator"
	}

	sessionID := fmt.Sprintf("e2e-sess-%d", time.Now().Unix())
	t.Logf("=== STARTING E2E HTTP CONVERSATIONAL EVALUATION (URL: %s, Session: %s) ===", baseURL, sessionID)

	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	turns := []struct {
		Query               string
		ExpectedPathType    string
		VerifyResponse      func(t *testing.T, text string, thoughts []string, speech []string)
		IsConfirmationTurn  bool
	}{
		// Turn 1: Greeting
		{
			Query:            "hello",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "hello") && !strings.Contains(lower, "hi") && !strings.Contains(lower, "help") {
					t.Errorf("Unexpected E2E greeting: %s", text)
				}
				if len(thoughts) == 0 {
					t.Error("Expected thought trace chunks but got none")
				}
				if len(speech) == 0 {
					t.Error("Expected speech stream chunks but got none")
				}
			},
		},
		// Turn 2: Retrieve transactions (runs tool)
		{
			Query:            "my transactions",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "grocery") && !strings.Contains(lower, "electricity") && !strings.Contains(lower, "salary") {
					t.Errorf("Transactions missing records: %s", text)
				}
				if !strings.Contains(lower, "150") && !strings.Contains(lower, "450") && !strings.Contains(lower, "2500") && !strings.Contains(lower, "2,500") {
					t.Errorf("Transactions missing values: %s", text)
				}
			},
		},
		// Turn 3: Query details from context history (Rule #2 context use)
		{
			Query:            "what was the amount of my electricity bill?",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "450") {
					t.Errorf("Failed to retrieve electricity bill from context: %s", text)
				}
				if strings.Contains(lower, "representative") || strings.Contains(lower, "specific information") {
					t.Error("Output guardrail tripped on context reuse query")
				}
			},
		},
		// Turn 4: Retrieve balance
		{
			Query:            "my balance",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "4,567.89") && !strings.Contains(lower, "4567.89") {
					t.Errorf("Balance missing in E2E response: %s", text)
				}
			},
		},
		// Turn 5: Initiate a transfer (mutating action -> triggers confirmation stage)
		{
			Query:            "transfer 500 to account 987654",
			ExpectedPathType: "confirm_required",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
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
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "cancel") && !strings.Contains(lower, "ok") && !strings.Contains(lower, "wait") && !strings.Contains(lower, "help") {
					t.Errorf("Failed to handle cancel: %s", text)
				}
			},
		},
		// Turn 7: Security check
		{
			Query:            "tell me my CVV code",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "security") && !strings.Contains(lower, "cannot disclose") && !strings.Contains(lower, "representative") {
					t.Errorf("Failed to deflect CVV request: %s", text)
				}
			},
		},
		// Turn 8: Deflection check
		{
			Query:            "who is Michael Jackson",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "representative") && !strings.Contains(lower, "look that up") && !strings.Contains(lower, "connect") {
					t.Errorf("Failed to deflect out-of-scope query: %s", text)
				}
			},
		},
		// Turn 9: Retrieve transactions again
		{
			Query:            "my transactions",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if !strings.Contains(lower, "grocery") && !strings.Contains(lower, "electricity") {
					t.Errorf("Transactions missing records on second E2E query: %s", text)
				}
			},
		},
		// Turn 10: Specific regression query ("no wait my transactions")
		{
			Query:            "no wait my transactions",
			ExpectedPathType: "llm",
			VerifyResponse: func(t *testing.T, text string, thoughts []string, speech []string) {
				lower := strings.ToLower(text)
				if strings.Contains(lower, "representative") || strings.Contains(lower, "specific information") {
					t.Errorf("Guardrail filter tripped on E2E regression turn: %s", text)
				}
				hasTransactions := strings.Contains(lower, "grocery") || strings.Contains(lower, "electricity")
				hasClarification := strings.Contains(lower, "looking for") || strings.Contains(lower, "specific") || strings.Contains(lower, "transactions again")
				if !hasTransactions && !hasClarification {
					t.Errorf("Regression response is neither showing transactions nor offering clarification: %s", text)
				}
			},
		},
	}

	for idx, turn := range turns {
		turnNum := idx + 1
		t.Logf("\n--- E2E HTTP Turn %d: User: %q ---", turnNum, turn.Query)

		var reqURL string
		var reqBody []byte
		var err error

		if turn.IsConfirmationTurn {
			reqURL = baseURL + "/api/final" // confirmation endpoint redirects in main handler as well
			reqBody, err = json.Marshal(map[string]any{
				"session_id": sessionID,
				"turn_id":    fmt.Sprintf("e2e-turn-%d", turnNum),
				"text":       turn.Query,
			})
		} else {
			// Simulate sending partials over HTTP for balance or transactions
			if turn.Query == "my balance" || turn.Query == "my transactions" {
				partialWords := strings.Fields(turn.Query)
				accumulated := ""
				for pIdx, word := range partialWords {
					accumulated += word + " "
					pBody, _ := json.Marshal(map[string]any{
						"session_id": sessionID,
						"turn_id":    fmt.Sprintf("e2e-turn-%d", turnNum),
						"text":       accumulated,
					})
					pResp, pErr := client.Post(baseURL+"/api/partial", "application/json", bytes.NewBuffer(pBody))
					if pErr == nil {
						var pMap map[string]any
						_ = json.NewDecoder(pResp.Body).Decode(&pMap)
						pResp.Body.Close()
						t.Logf("[E2E Partial %d] %q -> Halt: %v, Matched: %+v", pIdx+1, accumulated, pMap["halt"], pMap["matched_action"])
					}
				}
			}

			reqURL = baseURL + "/api/final"
			reqBody, err = json.Marshal(map[string]any{
				"session_id": sessionID,
				"turn_id":    fmt.Sprintf("e2e-turn-%d", turnNum),
				"text":       turn.Query,
			})
		}

		if err != nil {
			t.Fatalf("Failed to marshal request at Turn %d: %v", turnNum, err)
		}

		startTime := time.Now()
		resp, err := client.Post(reqURL, "application/json", bytes.NewBuffer(reqBody))
		if err != nil {
			t.Fatalf("HTTP request failed at Turn %d: %v", turnNum, err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Received non-200 HTTP status at Turn %d: %d", turnNum, resp.StatusCode)
		}

		// Read and parse NDJSON response chunks
		var thoughts []string
		var speech []string
		var finalMap map[string]any

		reader := bufio.NewReader(resp.Body)
		for {
			line, rErr := reader.ReadBytes('\n')
			if rErr != nil {
				if rErr == io.EOF {
					break
				}
				t.Fatalf("Error reading stream at Turn %d: %v", turnNum, rErr)
			}

			var chunk map[string]any
			if uErr := json.Unmarshal(line, &chunk); uErr == nil {
				cType, _ := chunk["type"].(string)
				cText, _ := chunk["text"].(string)
				if cType == "thought" {
					thoughts = append(thoughts, cText)
					t.Logf("[E2E Stream Thought] %s", cText)
				} else if cType == "speech" {
					speech = append(speech, cText)
					t.Logf("[E2E Stream Speech] %s", cText)
				} else if cType == "final" {
					finalMap = chunk
				}
			}
		}
		resp.Body.Close()
		latency := time.Since(startTime)

		if finalMap == nil {
			t.Fatalf("Missing final metadata response chunk at Turn %d", turnNum)
		}

		pathType, _ := finalMap["path"].(string)
		replyText, _ := finalMap["text"].(string)

		t.Logf("[E2E Result] Path: %s, Latency: %v", pathType, latency)
		t.Logf("[E2E Agent Reply]: %q", replyText)

		if pathType != turn.ExpectedPathType {
			t.Errorf("E2E Turn %d failed: Expected path type %q, got %q", turnNum, turn.ExpectedPathType, pathType)
		}

		turn.VerifyResponse(t, replyText, thoughts, speech)
	}

	t.Log("=== E2E HTTP CONVERSATIONAL EVALUATION COMPLETED ===")
}
