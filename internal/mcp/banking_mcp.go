package mcp

import (
	"banking-voice-ai-agent/internal/telemetry"

	"context"
	"encoding/json"
	"fmt"
	"log"

	"banking-voice-ai-agent/internal/db"
)

type BankingMCPServer struct {
	Mongo *db.MongoManager
}

func NewBankingMCPServer(mongoManager *db.MongoManager) *BankingMCPServer {
	return &BankingMCPServer{Mongo: mongoManager}
}

// CallTool routes and executes a tool call, returning the response as a string
func (s *BankingMCPServer) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	ctx, span := telemetry.Step(ctx, "mcp."+name)
	defer span.End()
	loggedArgs := make(map[string]any)
	for k, v := range args {
		if k == "to" {
			loggedArgs[k] = "[MASKED]"
		} else {
			loggedArgs[k] = v
		}
	}
	log.Printf("[MCP] Calling tool '%s' with arguments: %+v", name, loggedArgs)

	userID, ok := args["user_id"].(string)
	if !ok || userID == "" {
		return "", fmt.Errorf("missing or invalid parameter 'user_id'")
	}

	switch name {
	case "get_balance":
		balance, currency, err := s.Mongo.GetBalance(ctx, userID)
		if err != nil {
			return "", err
		}
		result := map[string]any{
			"user_id":  userID,
			"balance":  balance,
			"currency": currency,
			"text":     fmt.Sprintf("Your balance is %.2f %s.", balance, currency),
		}
		resBytes, _ := json.Marshal(result)
		return string(resBytes), nil

	case "get_transactions":
		var limit int64 = 3 // default
		if val, ok := args["n"]; ok {
			switch v := val.(type) {
			case float64:
				limit = int64(v)
			case int64:
				limit = v
			case int:
				limit = int64(v)
			}
		}

		txs, err := s.Mongo.GetTransactions(ctx, userID, limit)
		if err != nil {
			return "", err
		}

		// Format transactions list
		var txList []map[string]any
		var textSummary string = "Your recent transactions are: "
		if len(txs) == 0 {
			textSummary = "No transactions found."
		}

		for idx, t := range txs {
			txList = append(txList, map[string]any{
				"amount":      t.Amount,
				"description": t.Description,
				"date":        t.Date.Format("2006-01-02"),
			})
			sign := "+"
			if t.Amount < 0 {
				sign = ""
			}
			textSummary += fmt.Sprintf("(%d) %s%.2f at %s on %s. ", idx+1, sign, t.Amount, t.Description, t.Date.Format("Jan 2"))
		}

		result := map[string]any{
			"user_id":      userID,
			"transactions": txList,
			"text":         textSummary,
		}
		resBytes, _ := json.Marshal(result)
		return string(resBytes), nil

	case "get_due_date":
		cardType, ok := args["card"].(string)
		if !ok || cardType == "" {
			cardType = "credit" // default
		}

		dueDate, err := s.Mongo.GetDueDate(ctx, userID, cardType)
		if err != nil {
			return "", err
		}

		result := map[string]any{
			"user_id":  userID,
			"card":     cardType,
			"due_date": dueDate,
			"text":     fmt.Sprintf("The payment due date for your %s card is %s.", cardType, dueDate),
		}
		resBytes, _ := json.Marshal(result)
		return string(resBytes), nil

	case "block_card":
		cardType, ok := args["card"].(string)
		if !ok || cardType == "" {
			cardType = "credit" // default
		}

		success, err := s.Mongo.BlockCard(ctx, userID, cardType)
		if err != nil {
			return "", err
		}

		var text string
		if success {
			text = fmt.Sprintf("Your %s card has been successfully blocked.", cardType)
		} else {
			text = fmt.Sprintf("We could not modify the status of your %s card. Please contact customer care.", cardType)
		}

		result := map[string]any{
			"user_id": userID,
			"card":    cardType,
			"success": success,
			"text":    text,
		}
		resBytes, _ := json.Marshal(result)
		return string(resBytes), nil

	case "transfer":
		to, ok := args["to"].(string)
		if !ok || to == "" {
			return "", fmt.Errorf("missing or invalid parameter 'to'")
		}

		amountVal, ok := args["amount"]
		if !ok {
			return "", fmt.Errorf("missing parameter 'amount'")
		}
		var amount float64
		switch v := amountVal.(type) {
		case float64:
			amount = v
		case float32:
			amount = float64(v)
		case int:
			amount = float64(v)
		case int64:
			amount = float64(v)
		default:
			return "", fmt.Errorf("invalid type for parameter 'amount'")
		}

		uniqueRefNo, ok := args["unique_ref_no"].(string)
		if !ok || uniqueRefNo == "" {
			return "", fmt.Errorf("missing or invalid parameter 'unique_ref_no'")
		}

		paymentRefNo, err := s.Mongo.Transfer(ctx, userID, to, amount, uniqueRefNo)
		if err != nil {
			return "", err
		}

		result := map[string]any{
			"user_id":        userID,
			"to":             to,
			"amount":         amount,
			"payment_ref_no": paymentRefNo,
			"unique_ref_no":  uniqueRefNo,
			"text":           fmt.Sprintf("Successfully transferred %.2f to account %s. Payment Reference Number is %s.", amount, to, paymentRefNo),
		}
		resBytes, _ := json.Marshal(result)
		return string(resBytes), nil

	default:
		return "", fmt.Errorf("unknown tool name '%s'", name)
	}
}
