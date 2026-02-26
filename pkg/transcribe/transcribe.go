// Package transcribe provides speech-to-text via OpenRouter API.
package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

const openRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// Request/Response types for OpenRouter API
type ChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error,omitempty"`
}

// Transcribe sends audio data to the OpenRouter API for transcription.
// apiKey is the OpenRouter API key. data is the raw audio bytes; format is the file extension (e.g. "ogg").
// Returns the transcribed text.
func Transcribe(ctx context.Context, apiKey string, data []byte, format string) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("OPENROUTER_API_KEY required for transcription")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("no audio data")
	}
	audioData := data
	if format == "" {
		format = "ogg"
	}

	// Build multipart/form-data request
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add model field
	if err := writer.WriteField("model", "google/gemini-2.5-flash-preview-05-20"); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}

	// Add file field with audio data
	part, err := writer.CreateFormFile("file", "audio."+format)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(audioData); err != nil {
		return "", fmt.Errorf("write audio data: %w", err)
	}

	// Close the writer to finalize the multipart message
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterEndpoint, &body)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)

	// Send request
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter request: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openrouter API %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if chatResp.Error != nil {
		return "", fmt.Errorf("openrouter error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("no transcription returned")
	}

	return chatResp.Choices[0].Message.Content, nil
}
