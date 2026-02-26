// Package cloudflare provides a direct REST API client for Cloudflare.
// This bypasses the MCP sandbox for operations that need full HTTP control
// (Worker deployment with multipart uploads, subdomain management, etc).
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"time"
)

const baseURL = "https://api.cloudflare.com/client/v4"

type Client struct {
	AccountID string
	APIToken  string
	http      *http.Client
	Subdomain string
}

func NewClient(accountID, apiToken string) *Client {
	return &Client{
		AccountID: accountID,
		APIToken:  apiToken,
		http:      &http.Client{Timeout: 120 * time.Second},
	}
}

// apiResponse is the standard Cloudflare API v4 envelope.
type apiResponse struct {
	Success  bool              `json:"success"`
	Errors   []apiError        `json:"errors"`
	Messages []json.RawMessage `json:"messages"`
	Result   json.RawMessage   `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*apiResponse, error) {
	url := baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response (HTTP %d): %s", resp.StatusCode, string(respBody[:min(len(respBody), 500)]))
	}

	if !apiResp.Success && len(apiResp.Errors) > 0 {
		return &apiResp, fmt.Errorf("cloudflare API error: [%d] %s", apiResp.Errors[0].Code, apiResp.Errors[0].Message)
	}

	return &apiResp, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, payload interface{}) (*apiResponse, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	return c.do(ctx, method, path, body, "application/json")
}

// ---- Account / Subdomain ----

type SubdomainInfo struct {
	Subdomain string `json:"subdomain"`
}

// GetSubdomain returns the workers.dev subdomain for this account.
func (c *Client) GetSubdomain(ctx context.Context) (string, error) {
	if c.Subdomain != "" {
		return c.Subdomain, nil
	}
	resp, err := c.doJSON(ctx, "GET", fmt.Sprintf("/accounts/%s/workers/subdomain", c.AccountID), nil)
	if err != nil {
		return "", err
	}
	var info SubdomainInfo
	if err := json.Unmarshal(resp.Result, &info); err != nil {
		return "", err
	}
	c.Subdomain = info.Subdomain
	return info.Subdomain, nil
}

// RegisterSubdomain registers a workers.dev subdomain for this account.
func (c *Client) RegisterSubdomain(ctx context.Context, subdomain string) error {
	_, err := c.doJSON(ctx, "PUT", fmt.Sprintf("/accounts/%s/workers/subdomain", c.AccountID), map[string]string{
		"subdomain": subdomain,
	})
	if err == nil {
		c.Subdomain = subdomain
	}
	return err
}

// VerifyToken checks if the API token is valid and returns its status.
func (c *Client) VerifyToken(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, "GET", "/user/tokens/verify", nil, "")
	if err != nil {
		return "", err
	}
	var result struct {
		Status string `json:"status"`
	}
	json.Unmarshal(resp.Result, &result)
	return result.Status, nil
}

// ---- Workers ----

type WorkerScript struct {
	ID         string `json:"id"`
	CreatedOn  string `json:"created_on,omitempty"`
	ModifiedOn string `json:"modified_on,omitempty"`
}

// ListWorkers returns all worker scripts on the account.
func (c *Client) ListWorkers(ctx context.Context) ([]WorkerScript, error) {
	resp, err := c.doJSON(ctx, "GET", fmt.Sprintf("/accounts/%s/workers/scripts", c.AccountID), nil)
	if err != nil {
		return nil, err
	}
	var scripts []WorkerScript
	json.Unmarshal(resp.Result, &scripts)
	return scripts, nil
}

// DeployWorker uploads a Worker script using multipart form data (ES module format).
func (c *Client) DeployWorker(ctx context.Context, name, jsCode string) error {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	metaHeader := make(textproto.MIMEHeader)
	metaHeader.Set("Content-Disposition", `form-data; name="metadata"`)
	metaHeader.Set("Content-Type", "application/json")
	metaPart, _ := writer.CreatePart(metaHeader)
	metadata := map[string]interface{}{
		"main_module":        "worker.js",
		"compatibility_date": "2024-09-23",
		"compatibility_flags": []string{"nodejs_compat"},
	}
	json.NewEncoder(metaPart).Encode(metadata)

	scriptHeader := make(textproto.MIMEHeader)
	scriptHeader.Set("Content-Disposition", `form-data; name="worker.js"; filename="worker.js"`)
	scriptHeader.Set("Content-Type", "application/javascript+module")
	scriptPart, _ := writer.CreatePart(scriptHeader)
	scriptPart.Write([]byte(jsCode))

	writer.Close()

	path := fmt.Sprintf("/accounts/%s/workers/scripts/%s", c.AccountID, name)
	_, err := c.do(ctx, "PUT", path, &buf, writer.FormDataContentType())
	if err != nil {
		return fmt.Errorf("deploy worker %q: %w", name, err)
	}

	// Enable workers.dev route so the worker is accessible
	_ = c.EnableWorkerSubdomain(ctx, name, true)

	return nil
}

// DeleteWorker removes a worker script.
func (c *Client) DeleteWorker(ctx context.Context, name string) error {
	_, err := c.doJSON(ctx, "DELETE", fmt.Sprintf("/accounts/%s/workers/scripts/%s", c.AccountID, name), nil)
	return err
}

// GetWorkerURL returns the public URL for a deployed worker.
func (c *Client) GetWorkerURL(ctx context.Context, name string) string {
	sub, _ := c.GetSubdomain(ctx)
	if sub == "" {
		return fmt.Sprintf("(no workers.dev subdomain — run register_subdomain first)")
	}
	return fmt.Sprintf("https://%s.%s.workers.dev", name, sub)
}

// EnableWorkerSubdomain enables/disables the workers.dev route for a script.
func (c *Client) EnableWorkerSubdomain(ctx context.Context, name string, enabled bool) error {
	path := fmt.Sprintf("/accounts/%s/workers/scripts/%s/subdomain", c.AccountID, name)
	_, err := c.doJSON(ctx, "POST", path, map[string]bool{"enabled": enabled})
	return err
}

// ---- KV ----

type KVNamespace struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (c *Client) ListKVNamespaces(ctx context.Context) ([]KVNamespace, error) {
	resp, err := c.doJSON(ctx, "GET", fmt.Sprintf("/accounts/%s/storage/kv/namespaces", c.AccountID), nil)
	if err != nil {
		return nil, err
	}
	var ns []KVNamespace
	json.Unmarshal(resp.Result, &ns)
	return ns, nil
}

func (c *Client) CreateKVNamespace(ctx context.Context, title string) (*KVNamespace, error) {
	resp, err := c.doJSON(ctx, "POST", fmt.Sprintf("/accounts/%s/storage/kv/namespaces", c.AccountID), map[string]string{
		"title": title,
	})
	if err != nil {
		return nil, err
	}
	var ns KVNamespace
	json.Unmarshal(resp.Result, &ns)
	return &ns, nil
}

func (c *Client) KVWrite(ctx context.Context, nsID, key string, value []byte) error {
	path := fmt.Sprintf("/accounts/%s/storage/kv/namespaces/%s/values/%s", c.AccountID, nsID, key)
	_, err := c.do(ctx, "PUT", path, bytes.NewReader(value), "application/octet-stream")
	return err
}

func (c *Client) KVRead(ctx context.Context, nsID, key string) ([]byte, error) {
	url := fmt.Sprintf("%s/accounts/%s/storage/kv/namespaces/%s/values/%s", baseURL, c.AccountID, nsID, key)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ---- D1 ----

type D1Database struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

func (c *Client) ListD1Databases(ctx context.Context) ([]D1Database, error) {
	resp, err := c.doJSON(ctx, "GET", fmt.Sprintf("/accounts/%s/d1/database", c.AccountID), nil)
	if err != nil {
		return nil, err
	}
	var dbs []D1Database
	json.Unmarshal(resp.Result, &dbs)
	return dbs, nil
}

func (c *Client) CreateD1Database(ctx context.Context, name string) (*D1Database, error) {
	resp, err := c.doJSON(ctx, "POST", fmt.Sprintf("/accounts/%s/d1/database", c.AccountID), map[string]string{
		"name": name,
	})
	if err != nil {
		return nil, err
	}
	var db D1Database
	json.Unmarshal(resp.Result, &db)
	return &db, nil
}

func (c *Client) D1Query(ctx context.Context, dbID, sql string) (string, error) {
	resp, err := c.doJSON(ctx, "POST", fmt.Sprintf("/accounts/%s/d1/database/%s/query", c.AccountID, dbID), map[string]string{
		"sql": sql,
	})
	if err != nil {
		return "", err
	}
	return string(resp.Result), nil
}

// ---- R2 Buckets (management API, not S3) ----

type R2Bucket struct {
	Name         string `json:"name"`
	CreationDate string `json:"creation_date,omitempty"`
}

func (c *Client) ListR2Buckets(ctx context.Context) ([]R2Bucket, error) {
	resp, err := c.doJSON(ctx, "GET", fmt.Sprintf("/accounts/%s/r2/buckets", c.AccountID), nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Buckets []R2Bucket `json:"buckets"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		var buckets []R2Bucket
		json.Unmarshal(resp.Result, &buckets)
		return buckets, nil
	}
	return result.Buckets, nil
}

func (c *Client) CreateR2Bucket(ctx context.Context, name string) error {
	_, err := c.doJSON(ctx, "POST", fmt.Sprintf("/accounts/%s/r2/buckets", c.AccountID), map[string]string{
		"name": name,
	})
	return err
}

// ---- Vectorize ----

type VectorizeIndex struct {
	Name       string `json:"name"`
	Dimensions int    `json:"dimensions,omitempty"`
}

func (c *Client) ListVectorizeIndexes(ctx context.Context) ([]VectorizeIndex, error) {
	resp, err := c.doJSON(ctx, "GET", fmt.Sprintf("/accounts/%s/vectorize/v2/indexes", c.AccountID), nil)
	if err != nil {
		return nil, err
	}
	var indexes []VectorizeIndex
	json.Unmarshal(resp.Result, &indexes)
	return indexes, nil
}

func (c *Client) CreateVectorizeIndex(ctx context.Context, name string, dimensions int, metric string) error {
	if metric == "" {
		metric = "cosine"
	}
	_, err := c.doJSON(ctx, "POST", fmt.Sprintf("/accounts/%s/vectorize/v2/indexes", c.AccountID), map[string]interface{}{
		"name":        name,
		"description": "PicoFlare managed index",
		"config":      map[string]interface{}{"dimensions": dimensions, "metric": metric},
	})
	return err
}

// ---- Pages / Full Inventory ----

type Inventory struct {
	Subdomain string           `json:"subdomain"`
	Workers   []WorkerScript   `json:"workers"`
	KV        []KVNamespace    `json:"kv_namespaces"`
	D1        []D1Database     `json:"d1_databases"`
	R2        []R2Bucket       `json:"r2_buckets"`
	Vectorize []VectorizeIndex `json:"vectorize_indexes"`
}

func (c *Client) TakeInventory(ctx context.Context) *Inventory {
	inv := &Inventory{}
	inv.Subdomain, _ = c.GetSubdomain(ctx)
	inv.Workers, _ = c.ListWorkers(ctx)
	inv.KV, _ = c.ListKVNamespaces(ctx)
	inv.D1, _ = c.ListD1Databases(ctx)
	inv.R2, _ = c.ListR2Buckets(ctx)
	inv.Vectorize, _ = c.ListVectorizeIndexes(ctx)
	return inv
}

func (inv *Inventory) Summary() string {
	sub := inv.Subdomain
	if sub == "" {
		sub = "(none)"
	}
	return fmt.Sprintf("Subdomain: %s.workers.dev | %d workers, %d KV, %d D1, %d R2, %d vectorize",
		sub, len(inv.Workers), len(inv.KV), len(inv.D1), len(inv.R2), len(inv.Vectorize))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
