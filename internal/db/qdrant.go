package db

import (
	"banking-voice-ai-agent/internal/telemetry"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"banking-voice-ai-agent/internal/ollama"
)

type QdrantManager struct {
	BaseURL    string
	HTTPClient *http.Client
}

type QdrantMatch struct {
	ID      any            `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

type SearchResponse struct {
	Result []QdrantMatch `json:"result"`
	Status string        `json:"status"`
}

func NewQdrantManager(baseURL string) (*QdrantManager, error) {
	if baseURL == "" {
		baseURL = "http://localhost:6333"
	}
	mgr := &QdrantManager{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}

	// Verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", mgr.BaseURL+"/collections", nil)
	if err != nil {
		return nil, err
	}

	resp, err := mgr.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to qdrant: %w", err)
	}
	resp.Body.Close()

	return mgr, nil
}

// CreateCollection creates a Qdrant collection with the specified name and vector size
func (q *QdrantManager) CreateCollection(ctx context.Context, name string, dim int) error {
	url := fmt.Sprintf("%s/collections/%s", q.BaseURL, name)

	// Drop any existing collection first so a changed embedding dimension (e.g.
	// nomic 768 → bge-m3 1024) takes effect — Qdrant rejects a size mismatch.
	if delReq, derr := http.NewRequestWithContext(ctx, "DELETE", url, nil); derr == nil {
		if delResp, err := q.HTTPClient.Do(delReq); err == nil {
			delResp.Body.Close()
		}
	}

	bodyMap := map[string]any{
		"vectors": map[string]any{
			"size":     dim,
			"distance": "Cosine",
		},
		"hnsw_config": map[string]any{
			"m":            16,
			"ef_construct": 100,
		},
	}

	reqBody, err := json.Marshal(bodyMap)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := q.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		// It's fine if collection already exists, Qdrant might return bad request if we try to recreate it
		log.Printf("Collection status returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// UpsertPoint inserts vectors and payloads into Qdrant
func (q *QdrantManager) UpsertPoints(ctx context.Context, collection string, points []map[string]any) error {
	url := fmt.Sprintf("%s/collections/%s/points", q.BaseURL, collection)

	bodyMap := map[string]any{
		"points": points,
	}

	reqBody, err := json.Marshal(bodyMap)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := q.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qdrant points upsert failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// Search queries Qdrant using vector similarity
func (q *QdrantManager) Search(ctx context.Context, collection string, vector []float64, limit int) ([]QdrantMatch, error) {
	ctx, span := telemetry.Step(ctx, "qdrant.search")
	defer span.End()
	url := fmt.Sprintf("%s/collections/%s/points/search", q.BaseURL, collection)

	bodyMap := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
	}

	reqBody, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := q.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("qdrant search returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var searchRes SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchRes); err != nil {
		return nil, err
	}

	return searchRes.Result, nil
}

// SeedData sets up collections and upserts the seeded data using Ollama embeddings
func (q *QdrantManager) SeedData(ctx context.Context, ollamaClient *ollama.Client) error {
	log.Println("Initializing Qdrant Collections...")
	dim := 768 // fallback for nomic-embed-text

	// bge-m3 (multilingual, the plan default) and mxbai-embed-large are 1024-dim
	if ollamaClient.EmbedModel == "bge-m3" || ollamaClient.EmbedModel == "mxbai-embed-large" {
		dim = 1024
	}

	if err := q.CreateCollection(ctx, "action_intents", dim); err != nil {
		log.Printf("Note on creating action_intents collection: %v", err)
	}
	if err := q.CreateCollection(ctx, "faq_items", dim); err != nil {
		log.Printf("Note on creating faq_items collection: %v", err)
	}

	// Wait 1 second for Qdrant to propagate index creation
	time.Sleep(1 * time.Second)

	// Actions to seed
	type ActionSeed struct {
		Text             string
		Intent           string
		ResponseTemplate string
		BankAction       string
	}

	actions := []ActionSeed{
		// English Actions
		{Text: "what is my balance", Intent: "balance", ResponseTemplate: "Your current account balance is {{balance}}.", BankAction: "get_balance"},
		{Text: "check my balance please", Intent: "balance", ResponseTemplate: "Your current account balance is {{balance}}.", BankAction: "get_balance"},
		{Text: "how much money do i have left", Intent: "balance", ResponseTemplate: "Your current account balance is {{balance}}.", BankAction: "get_balance"},
		{Text: "show recent transactions", Intent: "transactions", ResponseTemplate: "Here are your recent transactions: {{transactions}}.", BankAction: "get_transactions"},
		{Text: "what transactions occurred on my account", Intent: "transactions", ResponseTemplate: "Here are your recent transactions: {{transactions}}.", BankAction: "get_transactions"},
		{Text: "block my credit card", Intent: "block_card", ResponseTemplate: "Your credit card has been successfully blocked.", BankAction: "block_card"},
		{Text: "freeze card", Intent: "block_card", ResponseTemplate: "Your credit card has been successfully blocked.", BankAction: "block_card"},
		{Text: "lost my credit card deactivate it", Intent: "block_card", ResponseTemplate: "Your credit card has been successfully blocked.", BankAction: "block_card"},
		{Text: "when is my credit card bill due", Intent: "due_date", ResponseTemplate: "Your credit card bill payment due date is {{due_date}}.", BankAction: "get_due_date"},
		{Text: "due date card", Intent: "due_date", ResponseTemplate: "Your credit card bill payment due date is {{due_date}}.", BankAction: "get_due_date"},
		{Text: "transfer money to friend", Intent: "transfer", ResponseTemplate: "I have transferred {{amount}} to account {{to}}. Your transaction ID is {{tx_id}}.", BankAction: "transfer"},
		{Text: "wire money to account", Intent: "transfer", ResponseTemplate: "I have transferred {{amount}} to account {{to}}. Your transaction ID is {{tx_id}}.", BankAction: "transfer"},
		{Text: "send money online", Intent: "transfer", ResponseTemplate: "I have transferred {{amount}} to account {{to}}. Your transaction ID is {{tx_id}}.", BankAction: "transfer"},

		// Hinglish / Hindi Actions
		{Text: "mera balance kitna hai", Intent: "balance", ResponseTemplate: "आपके खाते का वर्तमान बैलेंस {{balance}} है।", BankAction: "get_balance"},
		{Text: "balance check karna hai", Intent: "balance", ResponseTemplate: "आपके खाते का वर्तमान बैलेंस {{balance}} है।", BankAction: "get_balance"},
		{Text: "खाता बैलेंस चेक करें", Intent: "balance", ResponseTemplate: "आपके खाते का वर्तमान बैलेंस {{balance}} है।", BankAction: "get_balance"},
		{Text: "apne pichle transaction dikhao", Intent: "transactions", ResponseTemplate: "आपके हाल के लेनदेन (transactions) इस प्रकार हैं: {{transactions}}.", BankAction: "get_transactions"},
		{Text: "लेनदेन का इतिहास दिखाएं", Intent: "transactions", ResponseTemplate: "आपके हाल के लेनदेन (transactions) इस प्रकार हैं: {{transactions}}.", BankAction: "get_transactions"},
		{Text: "mera card block kar do", Intent: "block_card", ResponseTemplate: "आपका क्रेडिट कार्ड सफलतापूर्वक ब्लॉक कर दिया गया है।", BankAction: "block_card"},
		{Text: "क्रेडिट कार्ड ब्लॉक करें", Intent: "block_card", ResponseTemplate: "आपका क्रेडिट कार्ड सफलतापूर्वक ब्लॉक कर दिया गया है।", BankAction: "block_card"},
		{Text: "card ka bill kab due hai", Intent: "due_date", ResponseTemplate: "आपके क्रेडिट कार्ड बिल की देय तिथि {{due_date}} है।", BankAction: "get_due_date"},
		{Text: "क्रेडिट कार्ड बिल देय तिथि क्या है", Intent: "due_date", ResponseTemplate: "आपके क्रेडिट कार्ड बिल की देय तिथि {{due_date}} है।", BankAction: "get_due_date"},
		{Text: "paise transfer karne hain", Intent: "transfer", ResponseTemplate: "मैंने खाता {{to}} में {{amount}} ट्रांसफर कर दिए हैं। आपका ट्रांजैक्शन आईडी {{tx_id}} है।", BankAction: "transfer"},
		{Text: "पैसे ट्रांसफर करें", Intent: "transfer", ResponseTemplate: "मैंने खाता {{to}} में {{amount}} ट्रांसफर कर दिए हैं। आपका ट्रांजैक्शन आईडी {{tx_id}} है।", BankAction: "transfer"},
	}

	// Embed and seed actions
	var actionPoints []map[string]any
	for idx, act := range actions {
		emb, err := ollamaClient.GetEmbedding(ctx, act.Text)
		if err != nil {
			return fmt.Errorf("failed to embed action query '%s': %w", act.Text, err)
		}

		actionPoints = append(actionPoints, map[string]any{
			"id":     uint64(idx + 1),
			"vector": emb,
			"payload": map[string]any{
				"text":              act.Text,
				"intent":            act.Intent,
				"response_template": act.ResponseTemplate,
				"bank_action":       act.BankAction,
			},
		})
	}

	if err := q.UpsertPoints(ctx, "action_intents", actionPoints); err != nil {
		return fmt.Errorf("failed to seed action_intents in Qdrant: %w", err)
	}
	log.Printf("Successfully seeded %d action intents into Qdrant.", len(actions))

	// FAQs to seed
	type FAQSeed struct {
		Question string
		Answer   string
	}

	faqs := []FAQSeed{
		// English FAQs
		{
			Question: "How do I reset my net banking password?",
			Answer:   "To reset your Net Banking password, go to the login page, click 'Forgot Password', enter your User ID, and verify with the OTP sent to your registered mobile number.",
		},
		{
			Question: "What are the bank branch working hours?",
			Answer:   "Our standard branch working hours are Monday to Friday, 9:30 AM to 4:30 PM. We are closed on Sundays and the 2nd and 4th Saturdays of the month.",
		},
		{
			Question: "How do I apply for a home loan?",
			Answer:   "You can apply for a Home Loan online via our website, by visiting any bank branch, or by calling our customer care number.",
		},
		{
			Question: "What is the interest rate on savings account?",
			Answer:   "Our savings account interest rates start at 3.0% per annum for balances below 50 lakhs and 3.5% per annum for balances of 50 lakhs and above.",
		},
		{
			Question: "How do I contact customer care?",
			Answer:   "You can contact our customer care 24/7 at 1800 1080 or email support@bank.com.",
		},

		// Hinglish / Hindi FAQs
		{
			Question: "net banking password reset kaise kare",
			Answer:   "नेट बैंकिंग पासवर्ड रीसेट करने के लिए, लॉगिन पेज पर जाएं, 'Forgot Password' पर क्लिक करें, यूजर आईडी डालें, और अपने मोबाइल पर प्राप्त OTP से सत्यापित करें।",
		},
		{
			Question: "नेट बैंकिंग पासवर्ड कैसे रीसेट करें?",
			Answer:   "नेट बैंकिंग पासवर्ड रीसेट करने के लिए, लॉगिन पेज पर जाएं, 'Forgot Password' पर क्लिक करें, यूजर आईडी डालें, और अपने मोबाइल पर प्राप्त OTP से सत्यापित करें।",
		},
		{
			Question: "bank khulne ka samay kya hai",
			Answer:   "शाखा के काम के घंटे सोमवार से शुक्रवार सुबह 9:30 बजे से शाम 4:30 बजे तक हैं। रविवार और दूसरे/चौथे शनिवार को बैंक बंद रहता है।",
		},
		{
			Question: "बैंक शाखा के काम के घंटे क्या हैं?",
			Answer:   "शाखा के काम के घंटे सोमवार से शुक्रवार सुबह 9:30 बजे से शाम 4:30 बजे तक हैं। रविवार और दूसरे/चौथे शनिवार को बैंक बंद रहता है।",
		},
		{
			Question: "customer care number kya hai",
			Answer:   "आप 24/7 हमारे ग्राहक सेवा (customer care) नंबर 1800 1080 पर संपर्क कर सकते हैं या support@bank.com पर ईमेल लिख सकते हैं।",
		},
		{
			Question: "कस्टमर केयर से कैसे संपर्क करें?",
			Answer:   "आप 24/7 हमारे ग्राहक सेवा (customer care) नंबर 1800 1080 पर संपर्क कर सकते हैं या support@bank.com पर ईमेल लिख सकते हैं।",
		},
	}

	// Embed and seed FAQs
	var faqPoints []map[string]any
	for idx, faq := range faqs {
		emb, err := ollamaClient.GetEmbedding(ctx, faq.Question)
		if err != nil {
			return fmt.Errorf("failed to embed FAQ question '%s': %w", faq.Question, err)
		}

		faqPoints = append(faqPoints, map[string]any{
			"id":     uint64(idx + 1000), // separate IDs
			"vector": emb,
			"payload": map[string]any{
				"question": faq.Question,
				"answer":   faq.Answer,
			},
		})
	}

	if err := q.UpsertPoints(ctx, "faq_items", faqPoints); err != nil {
		return fmt.Errorf("failed to seed faq_items in Qdrant: %w", err)
	}
	log.Printf("Successfully seeded %d FAQ items into Qdrant.", len(faqs))

	return nil
}
