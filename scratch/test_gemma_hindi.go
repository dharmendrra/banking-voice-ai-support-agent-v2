package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

type FormatReq struct {
	Query     string `json:"query"`
	McpResult string `json:"mcp_result"`
}

type FormatRes struct {
	Text string `json:"text"`
}

func testQuery(query, mcpResult string) {
	reqBody, _ := json.Marshal(FormatReq{
		Query:     query,
		McpResult: mcpResult,
	})
	resp, err := http.Post("http://localhost:9091/format", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		fmt.Printf("Query: %s -> Error: %v\n", query, err)
		return
	}
	defer resp.Body.Close()

	var res FormatRes
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		fmt.Printf("Query: %s -> Decode error: %v\n", query, err)
		return
	}
	fmt.Printf("Query:    %s\nResponse: %s\n\n", query, res.Text)
}

func main() {
	fmt.Println("=== TESTING GEMMA4:E4B HINDI / HINGLISH PHRASING ===")
	
	testQuery(
		"mera balance check karo",
		"Your balance is 4567.89 INR.",
	)
	
	testQuery(
		"mujhe transactions dikhao",
		"Your recent transactions are: (1) -150.00 at Grocery Store on Jul 12. (2) -450.00 at Electricity Bill on Jul 8. (3) +2500.00 at Salary Credit on Jul 3.",
	)
	
	testQuery(
		"namaste, kaise ho?",
		"",
	)
}
