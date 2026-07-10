package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/mcp"
	"github.com/redis/go-redis/v9"
)

type ToolExecutionResult struct {
	Status       string         `json:"status"` // "success", "confirm_required", "error"
	ResponseText string         `json:"response_text"`
	Intent       string         `json:"intent,omitempty"`
	Payload      map[string]any `json:"payload,omitempty"`
}

type ToolCallAuditService struct {
	MCP   *mcp.BankingMCPServer
	Redis *db.RedisManager
}

func NewToolCallAuditService(mcpServer *mcp.BankingMCPServer, redisManager *db.RedisManager) *ToolCallAuditService {
	return &ToolCallAuditService{
		MCP:   mcpServer,
		Redis: redisManager,
	}
}

// ExecuteToolCall runs structural checks, identity injection, compliance routing, and audit logging.
func (s *ToolCallAuditService) ExecuteToolCall(ctx context.Context, turnID, sessionID, userID, rawJSON string) (ToolExecutionResult, error) {
	// Clean markdown JSON wrappers if present
	cleanedResponse := rawJSON
	if strings.Contains(cleanedResponse, "```json") {
		parts := strings.Split(cleanedResponse, "```json")
		if len(parts) > 1 {
			cleanedResponse = strings.Split(parts[1], "```")[0]
		}
	} else if strings.Contains(cleanedResponse, "```") {
		parts := strings.Split(cleanedResponse, "```")
		if len(parts) > 1 {
			cleanedResponse = parts[1]
		}
	}
	cleanedResponse = strings.TrimSpace(cleanedResponse)

	// Unmarshal
	var req struct {
		ToolName string         `json:"tool_name"`
		Args     map[string]any `json:"args"`
	}
	if err := json.Unmarshal([]byte(cleanedResponse), &req); err != nil {
		return ToolExecutionResult{Status: "error"}, fmt.Errorf("structural check failed: invalid json format: %w", err)
	}

	if req.ToolName == "" {
		return ToolExecutionResult{Status: "error"}, fmt.Errorf("structural check failed: tool_name is empty")
	}

	// 1. Structural Check: Verify the requested tool is supported
	var intent, responseTemplate string
	switch req.ToolName {
	case "get_balance":
		intent = "balance"
		responseTemplate = "Your current account balance is {{balance}}."
	case "get_transactions":
		intent = "transactions"
		responseTemplate = "Here are your recent transactions: {{transactions}}."
	case "get_due_date":
		intent = "due_date"
		responseTemplate = "Your credit card bill payment due date is {{due_date}}."
	case "block_card":
		intent = "block_card"
		responseTemplate = "Your credit card has been successfully blocked."
	case "transfer":
		intent = "transfer"
		responseTemplate = "I have transferred {{amount}} to account {{to}}. Your transaction ID is {{tx_id}}."
	case "resume_playback":
		return ToolExecutionResult{
			Status:       "resume_playback",
			ResponseText: "resume",
		}, nil
	default:
		log.Printf("[Security] Structural Check Failed: Unsupported tool name requested by LLM: %s", req.ToolName)
		return ToolExecutionResult{
			Status:       "error",
			ResponseText: "I'm sorry, I cannot perform that action.",
		}, fmt.Errorf("structural check failed: unsupported tool '%s'", req.ToolName)
	}

	// 2. Identity Injection: Inject the authenticated session user_id
	if req.Args == nil {
		req.Args = make(map[string]any)
	}
	req.Args["user_id"] = userID
	log.Printf("[Security] Identity Injection Complete: Injected user_id '%s' into tool args", userID)

	// 3. Compliance Check: Ensure write actions route to confirmation dialog
	payload := map[string]any{
		"intent":            intent,
		"bank_action":       req.ToolName,
		"response_template": responseTemplate,
	}
	for k, v := range req.Args {
		payload[k] = v
	}

	if req.ToolName == "transfer" || req.ToolName == "block_card" {
		log.Printf("[Security] Compliance Check: Routing write action to confirmation context")
		return ToolExecutionResult{
			Status:  "confirm_required",
			Intent:  intent,
			Payload: payload,
		}, nil
	}

	// Execute read-only tool call via Banking MCP Server
	log.Printf("[Supervisor] Executing read-only tool call on behalf of LLM: %s", req.ToolName)
	mcpRes, err := s.MCP.CallTool(ctx, req.ToolName, req.Args)
	if err != nil {
		return ToolExecutionResult{Status: "error"}, err
	}

	// Write Audit Log to Redis Streams
	s.WriteAuditLog(ctx, turnID, sessionID, userID, req.ToolName, req.Args, mcpRes)

	var mcpData map[string]any
	_ = json.Unmarshal([]byte(mcpRes), &mcpData)

	return ToolExecutionResult{
		Status:       "success",
		ResponseText: mcpRes,
		Payload:      mcpData,
	}, nil
}

func (s *ToolCallAuditService) WriteAuditLog(ctx context.Context, turnID, sessionID, userID, toolName string, args map[string]any, result string) {
	argsBytes, _ := json.Marshal(args)
	event := map[string]interface{}{
		"timestamp":  time.Now().Format(time.RFC3339),
		"turn_id":    turnID,
		"session_id": sessionID,
		"user_id":    userID,
		"action":     toolName,
		"args":       string(argsBytes),
		"result":     result,
	}

	err := s.Redis.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: "audit_log_stream",
		Values: event,
	}).Err()
	if err != nil {
		log.Printf("[Audit] Failed to write audit event to stream: %v", err)
	} else {
		log.Printf("[Audit] Successfully wrote audit event for tool '%s' to stream", toolName)
	}
}
