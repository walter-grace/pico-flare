// Package memory provides Cloudflare Vectorize REST API client for RAG memory.
// Base URL: https://api.cloudflare.com/client/v4/accounts/{account_id}/vectorize/v2/indexes/{index_name}
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const baseURL = "https://api.cloudflare.com/client/v4/accounts"

// Client is a Vectorize REST API client.
type Client struct {
	BaseURL  string
	APIToken string
	http     *http.Client
}

// NewClient creates a new Vectorize client for the given account ID and API token.
func NewClient(accountID, apiToken string) *Client {
	return &Client{
		BaseURL:  fmt.Sprintf("%s/%s/vectorize/v2/indexes", baseURL, accountID),
		APIToken: apiToken,
		http:     &http.Client{},
	}
}

// VectorMatch represents a single result from a vector query.
type VectorMatch struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// InsertVector upserts a vector into the given index.
// Uses POST to .../upsert with JSON { "vectors": [{ "id", "values", "metadata" }] }.
func (c *Client) InsertVector(ctx context.Context, indexName, id string, vector []float64, metadata map[string]string) error {
	if metadata == nil {
		metadata = make(map[string]string)
	}
	body := map[string]interface{}{
		"vectors": []map[string]interface{}{
			{
				"id":       id,
				"values":   vector,
				"metadata": metadata,
			},
		},
	}
	return c.post(ctx, indexName, "upsert", body)
}

// QueryVector queries the index with the given vector and returns top K matches.
func (c *Client) QueryVector(ctx context.Context, indexName string, queryVector []float64, topK int) ([]VectorMatch, error) {
	body := map[string]interface{}{
		"vector":         queryVector,
		"topK":           topK,
		"returnValues":   false,
		"returnMetadata": "none",
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/%s/query", c.BaseURL, indexName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vectorize query failed: %s: %s", resp.Status, string(b))
	}

	var result struct {
		Matches []VectorMatch `json:"matches"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Matches, nil
}

func (c *Client) post(ctx context.Context, indexName, path string, body interface{}) error {
	reqBody, err := json.Marshal(body)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/%s/%s", c.BaseURL, indexName, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vectorize %s failed: %s: %s", path, resp.Status, string(b))
	}
	return nil
}
