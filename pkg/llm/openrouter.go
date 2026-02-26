// Package llm provides an OpenRouter (OpenAI-compatible) chat client
// with function-calling (tool use) support.
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const defaultEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// Message represents a chat message with optional tool calls.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is the LLM's request to invoke a tool.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef defines a tool the LLM can call.
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type Client struct {
	APIKey   string
	Model    string
	Endpoint string
	http     *http.Client

	TotalPromptTokens     int
	TotalCompletionTokens int
}

func NewClient(apiKey, model string) *Client {
	if model == "" {
		model = "moonshotai/kimi-k2.5"
	}
	return &Client{
		APIKey:   apiKey,
		Model:    model,
		Endpoint: defaultEndpoint,
		http:     &http.Client{Timeout: 600 * time.Second},
	}
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ChatResult contains the LLM response with possible tool calls.
type ChatResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
}

// Chat sends messages (with optional tools) and returns the full result.
func (c *Client) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*ChatResult, error) {
	return c.ChatWithModel(ctx, "", messages, tools)
}

// ChatWithModel sends messages using the given model. If model is empty, uses c.Model.
func (c *Client) ChatWithModel(ctx context.Context, model string, messages []Message, tools []ToolDef) (*ChatResult, error) {
	if model == "" {
		model = c.Model
	}
	req := chatRequest{
		Model:    model,
		Messages: messages,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("HTTP-Referer", "https://github.com/walter-grace/pico-flare")
	httpReq.Header.Set("X-Title", "PicoFlare")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("decode LLM response: %w\nBody: %s", err, string(respBody[:min(len(respBody), 500)]))
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("LLM error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("LLM returned no choices")
	}

	if chatResp.Usage != nil {
		c.TotalPromptTokens += chatResp.Usage.PromptTokens
		c.TotalCompletionTokens += chatResp.Usage.CompletionTokens
		log.Printf("LLM [tokens: %d in, %d out | session total: %d in, %d out]",
			chatResp.Usage.PromptTokens, chatResp.Usage.CompletionTokens,
			c.TotalPromptTokens, c.TotalCompletionTokens)
	}

	choice := chatResp.Choices[0]
	return &ChatResult{
		Content:      choice.Message.Content,
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
	}, nil
}

// SimpleChat is a convenience method for tool-free chat.
func (c *Client) SimpleChat(ctx context.Context, messages []Message) (string, error) {
	result, err := c.Chat(ctx, messages, nil)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TranscribeAudio transcribes audio using OpenRouter's multimodal API.
// audioData is the raw audio bytes. format is the audio format (ogg, wav, mp3, etc.)
func (c *Client) TranscribeAudio(ctx context.Context, audioData []byte, format string) (string, error) {
	if format == "" {
		format = "ogg"
	}

	// Base64 encode the audio
	base64Audio := base64.StdEncoding.EncodeToString(audioData)

	// Build request with audio content
	messages := []Message{
		{
			Role: "user",
			Content: fmt.Sprintf(`Transcribe this audio file word for word.

Audio data (base64, format: %s):
%s`, format, base64Audio),
		},
	}

	// Use a model that supports audio
	result, err := c.ChatWithModel(ctx, "google/gemini-2.0-flash-exp:free", messages, nil)
	if err != nil {
		return "", err
	}

	return result.Content, nil
}
