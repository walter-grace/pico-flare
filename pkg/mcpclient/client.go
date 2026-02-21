// Package mcpclient provides a client for Cloudflare's Code Mode MCP server.
// See https://github.com/cloudflare/mcp
package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// SystemPrompt instructs the LLM to use Cloudflare Code Mode: search() and execute()
	// with JavaScript strings. The LLM chains commands in a single JS payload.
	SystemPrompt = `You are PicoFlare. You manage Cloudflare via Code Mode. Use the search() tool with JS to search the OpenAPI spec, and the execute() tool with JS to make authenticated calls via the cloudflare.request() object. Chain your commands in a single JS payload.`
)

// Client connects to Cloudflare's MCP endpoint via Streamable HTTP with Bearer token auth.
type Client struct {
	Endpoint    string
	APIToken    string
	AccountID   string
	http        *http.Client
	mu          sync.Mutex
	requestID   int
	initialized bool
}

// NewClient creates a new MCP client for the given endpoint and API token.
func NewClient(endpoint, apiToken, accountID string) *Client {
	return &Client{
		Endpoint:  endpoint,
		APIToken:  apiToken,
		AccountID: accountID,
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
		requestID: 1,
	}
}

// jsonRPCRequest represents a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"` // omit for notifications
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// jsonRPCResponse represents a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// post sends a JSON-RPC request and returns the response.
func (c *Client) post(ctx context.Context, req *jsonRPCRequest) (*jsonRPCResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIToken)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("MCP HTTP %d: %s - %s", resp.StatusCode, resp.Status, string(body))
	}

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("read MCP response: %w", readErr)
	}

	// 202 Accepted with empty body (e.g. notifications) - return synthetic success
	if resp.StatusCode == http.StatusAccepted && len(body) == 0 {
		return &jsonRPCResponse{JSONRPC: "2.0", Result: json.RawMessage(`{}`)}, nil
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode MCP response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return &rpcResp, nil
}

// Initialize performs the MCP initialize handshake.
// Cloudflare MCP is stateless and tools work without it; we run init for compatibility.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.initialized {
		return nil
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  "initialize",
		Params: map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    "picoflare",
				"version": "0.1.0",
			},
		},
	}

	_, err := c.post(ctx, req)
	if err != nil {
		return err
	}

	// Send initialized notification (omit id for notifications)
	notif := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}
	_, _ = c.post(ctx, notif) // 202 Accepted with empty body is OK

	c.initialized = true
	return nil
}

func (c *Client) nextID() int {
	id := c.requestID
	c.requestID++
	return id
}

// Search runs the search tool with the given JavaScript code.
// See https://github.com/cloudflare/mcp
func (c *Client) Search(ctx context.Context, code string) (interface{}, error) {
	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		if err := c.Initialize(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  "tools/call",
		Params: map[string]interface{}{
			"name": "search",
			"arguments": map[string]interface{}{
				"code": code,
			},
		},
	}

	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return resp.Result, nil // return raw if not standard format
	}
	if len(result.Content) > 0 && result.Content[0].Text != "" {
		return result.Content[0].Text, nil
	}
	return resp.Result, nil
}

// Execute runs the execute tool with the given JavaScript code.
// Pass accountID for user tokens; empty for account tokens (auto-detected).
func (c *Client) Execute(ctx context.Context, code string, accountID string) (interface{}, error) {
	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		if err := c.Initialize(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()

	args := map[string]interface{}{"code": code}
	if accountID != "" {
		args["account_id"] = accountID
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  "tools/call",
		Params: map[string]interface{}{
			"name":      "execute",
			"arguments": args,
		},
	}

	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return resp.Result, nil
	}
	if len(result.Content) > 0 && result.Content[0].Text != "" {
		return result.Content[0].Text, nil
	}
	return resp.Result, nil
}

// SendLLMRequest sends a user message to the LLM via the MCP endpoint.
// This is a stub that returns a placeholder until full LLM integration is wired.
func (c *Client) SendLLMRequest(ctx context.Context, userMessage string) (string, error) {
	_ = userMessage
	return "PicoFlare ready (MCP integration pending)", nil
}
