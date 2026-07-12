package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"banking-voice-ai-agent/internal/audit"
	"banking-voice-ai-agent/internal/db"
	"banking-voice-ai-agent/internal/mcp"
	"banking-voice-ai-agent/internal/telemetry"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
)

type ToolExecutionServer struct {
	Mongo        *db.MongoManager
	Redis        *db.RedisManager
	MCP          *mcp.BankingMCPServer
	AuditService *audit.ToolCallAuditService
}

func main() {
	log.Println("Starting Tool Execution Service...")

	tShutdown, logger, err := telemetry.Init(context.Background(), "tool-execution-service")
	if err != nil {
		log.Fatalf("Fatal: Telemetry initialization failed: %v", err)
	}
	defer func() { _ = tShutdown(context.Background()) }()
	slog.SetDefault(logger)

	mongoURI := getEnv("MONGO_URI", "mongodb://localhost:27017")
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")

	mongoMgr, err := db.NewMongoManager(mongoURI)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to MongoDB: %v", err)
	}

	redisMgr, err := db.NewRedisManager(redisAddr)
	if err != nil {
		log.Fatalf("Fatal: failed to connect to Redis: %v", err)
	}

	mcpServer := mcp.NewBankingMCPServer(mongoMgr)
	auditService := audit.NewToolCallAuditService(mcpServer, redisMgr)

	srv := &ToolExecutionServer{
		Mongo:        mongoMgr,
		Redis:        redisMgr,
		MCP:          mcpServer,
		AuditService: auditService,
	}

	mux := http.NewServeMux()
	mux.Handle("/execute", otelhttp.NewHandler(http.HandlerFunc(srv.handleExecute), "execute"))
	mux.Handle("/confirm", otelhttp.NewHandler(http.HandlerFunc(srv.handleConfirm), "confirm"))
	mux.Handle("/bank-data", otelhttp.NewHandler(http.HandlerFunc(srv.handleBankData), "bank-data"))
	mux.HandleFunc("/healthz", srv.handleHealthz)

	server := &http.Server{
		Addr:    ":9088",
		Handler: mux,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down Tool Execution Service...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Println("Tool Execution Service listening on port 9088")
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

type ExecuteRequest struct {
	ToolName  string         `json:"tool_name,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	UserID    string         `json:"user_id"`
	SessionID string         `json:"session_id"`
	TurnID    string         `json:"turn_id"`
	RawJSON   string         `json:"raw_json,omitempty"`
}

func (s *ToolExecutionServer) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	ctx, span := telemetry.Step(ctx, "tool.execute",
		attribute.String("session_id", req.SessionID),
		attribute.String("turn_id", req.TurnID),
	)
	defer span.End()

	if req.RawJSON != "" {
		// Run security checks, compliance routing and execution via AuditService
		res, err := s.AuditService.ExecuteToolCall(ctx, req.TurnID, req.SessionID, req.UserID, req.RawJSON)
		if err != nil {
			log.Printf("ExecuteToolCall error: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
		return
	}

	// Directly execute a specific tool (used for read-only actions and confirmation paths)
	span.SetAttributes(attribute.String("mcp.tool", req.ToolName))
	if req.Args == nil {
		req.Args = make(map[string]any)
	}
	req.Args["user_id"] = req.UserID

	mcpRes, err := s.MCP.CallTool(ctx, req.ToolName, req.Args)
	if err != nil {
		log.Printf("CallTool error for %s: %v", req.ToolName, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Write Audit Log
	s.AuditService.WriteAuditLog(ctx, req.TurnID, req.SessionID, req.UserID, req.ToolName, req.Args, mcpRes)

	var mcpData map[string]any
	_ = json.Unmarshal([]byte(mcpRes), &mcpData)

	resp := audit.ToolExecutionResult{
		Status:       "success",
		ResponseText: mcpRes,
		Payload:      mcpData,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

type ConfirmRequest struct {
	ToolName         string         `json:"tool_name"`
	Args             map[string]any `json:"args"`
	ConfirmationText string         `json:"confirmation_text"`
	UserID           string         `json:"user_id"`
	SessionID        string         `json:"session_id"`
	TurnID           string         `json:"turn_id"`
}

type ConfirmResponse struct {
	Confirmed    bool   `json:"confirmed"`
	ResponseText string `json:"response_text"`
}

func (s *ToolExecutionServer) handleConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	ctx, span := telemetry.Step(ctx, "tool.confirm",
		attribute.String("session_id", req.SessionID),
		attribute.String("turn_id", req.TurnID),
	)
	defer span.End()

	confirmNormalized := strings.ToLower(strings.TrimSpace(req.ConfirmationText))
	isConfirmed := strings.Contains(confirmNormalized, "yes") ||
		strings.Contains(confirmNormalized, "confirm") ||
		strings.Contains(confirmNormalized, "correct") ||
		strings.Contains(confirmNormalized, "sure") ||
		strings.Contains(confirmNormalized, "yup") ||
		strings.Contains(confirmNormalized, "ok") ||
		strings.Contains(confirmNormalized, "sahi") ||
		strings.Contains(confirmNormalized, "thik") ||
		strings.Contains(confirmNormalized, "haan") ||
		strings.Contains(confirmNormalized, "han") ||
		strings.Contains(confirmNormalized, "हाँ") ||
		strings.Contains(confirmNormalized, "हा")

	isHindi := isHindiText(req.ConfirmationText)

	if !isConfirmed {
		if isHindi {
			_ = json.NewEncoder(w).Encode(ConfirmResponse{Confirmed: false, ResponseText: "लेनदेन (transaction) रद्द कर दिया गया है।"})
		} else {
			_ = json.NewEncoder(w).Encode(ConfirmResponse{Confirmed: false, ResponseText: "Transaction cancelled."})
		}
		return
	}

	// Execute mutating action
	mcpRes, err := s.MCP.CallTool(ctx, req.ToolName, req.Args)
	if err != nil {
		log.Printf("MCP tool call error during confirmation: %v", err)
		var errMsg string
		if isHindi {
			errMsg = "मुझे खेद है, मैं इस समय ट्रांजैक्शन पूरा नहीं कर सका। कृपया फिर से प्रयास करें या ग्राहक सेवा प्रतिनिधि से बात करें।"
		} else {
			errMsg = "I'm sorry, I could not complete the transaction at this moment. Please try again or speak with a representative."
		}
		http.Error(w, errMsg, http.StatusInternalServerError)
		return
	}

	// Write Audit Log
	s.AuditService.WriteAuditLog(ctx, req.TurnID, req.SessionID, req.UserID, req.ToolName, req.Args, mcpRes)

	var mcpData map[string]any
	_ = json.Unmarshal([]byte(mcpRes), &mcpData)

	responseText := mcpRes
	if mText, ok := mcpData["text"].(string); ok {
		responseText = mText
	}

	if isHindi {
		// Specific Hindi confirmation formats
		if strings.Contains(req.ToolName, "transfer") {
			amountVal := req.Args["amount"].(float64)
			toAcc := req.Args["to"].(string)
			paymentRefNo, _ := mcpData["payment_ref_no"].(string)
			responseText = fmt.Sprintf("खाता %s में %.2f रुपये सफलतापूर्वक ट्रांसफर कर दिए गए हैं। भुगतान संदर्भ संख्या (Payment Reference Number) %s है।", toAcc, amountVal, paymentRefNo)
		} else if strings.Contains(req.ToolName, "block") {
			cardType, _ := req.Args["card"].(string)
			if cardType == "credit" {
				cardType = "क्रेडिट"
			} else {
				cardType = "डेबिट"
			}
			responseText = fmt.Sprintf("आपका %s कार्ड सफलतापूर्वक ब्लॉक कर दिया गया है।", cardType)
		}
	}

	_ = json.NewEncoder(w).Encode(ConfirmResponse{
		Confirmed:    true,
		ResponseText: responseText,
	})
}

func (s *ToolExecutionServer) handleBankData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "mock_user_123"
	}

	balance, currency, err := s.Mongo.GetBalance(ctx, userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	txs, err := s.Mongo.GetTransactions(ctx, userID, 5)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	dueDate, err := s.Mongo.GetDueDate(ctx, userID, "credit")
	if err != nil {
		dueDate = "N/A"
	}

	cardStatus := "active"
	var card db.Card
	if err := s.Mongo.DB.Collection("cards").FindOne(ctx, map[string]string{"user_id": userID, "card_type": "credit"}).Decode(&card); err == nil {
		cardStatus = card.Status
	}

	var txList []map[string]any
	for _, t := range txs {
		txList = append(txList, map[string]any{
			"description": t.Description,
			"amount":      t.Amount,
			"date":        t.Date.Format("2006-01-02 15:04"),
		})
	}

	res := map[string]any{
		"balance":       balance,
		"currency":      currency,
		"card_status":   cardStatus,
		"card_due_date": dueDate,
		"transactions":  txList,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *ToolExecutionServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.Mongo.Client.Ping(ctx, nil); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("MongoDB unhealthy: " + err.Error()))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func isHindiText(text string) bool {
	for _, r := range text {
		if r >= 0x0900 && r <= 0x097F {
			return true
		}
	}
	textLower := strings.ToLower(text)
	hinglishSpecific := []string{
		"kitna", "hai", "karna", "paise", "bhejna", "bhejo", "kar do",
		"pichle", "lenden", "khate", "rupay", "rupaya", "kab", "kya",
		"sahi", "thik", "haan", "han", "nahi", "nahin", "mera", "mere",
	}
	for _, kw := range hinglishSpecific {
		if strings.Contains(textLower, kw) {
			return true
		}
	}
	return false
}
