package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/bigneek/picoflare/pkg/mcpclient"
	"github.com/bigneek/picoflare/pkg/storage"
)

// SelfBuilder allows the agent to create, deploy, and manage its own
// Cloudflare Workers — effectively extending its own capabilities at runtime.
type SelfBuilder struct {
	mcp       *mcpclient.Client
	r2        *storage.R2Client
	bucket    string
	accountID string
}

// DeployedWorker tracks a worker the agent created.
type DeployedWorker struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Code        string    `json:"code"`
	Route       string    `json:"route,omitempty"`
	DeployedAt  time.Time `json:"deployed_at"`
	Status      string    `json:"status"` // "active", "failed", "deleted"
	URL         string    `json:"url,omitempty"`
}

const workersIndexKey = "memory/workers/index.json"

func NewSelfBuilder(mcp *mcpclient.Client, r2 *storage.R2Client, bucket, accountID string) *SelfBuilder {
	return &SelfBuilder{
		mcp:       mcp,
		r2:        r2,
		bucket:    bucket,
		accountID: accountID,
	}
}

// DeployWorker creates and deploys a Cloudflare Worker using Code Mode MCP.
func (sb *SelfBuilder) DeployWorker(ctx context.Context, name, description, workerCode string) (*DeployedWorker, error) {
	if sb.mcp == nil {
		return nil, fmt.Errorf("MCP not configured")
	}

	// Use cf_execute to deploy the Worker script via the Cloudflare API
	deployJS := fmt.Sprintf(`async () => {
		const scriptContent = %s;
		const formData = new FormData();

		const metadata = {
			main_module: "worker.js",
			compatibility_date: "2024-01-01"
		};
		formData.append("metadata", new Blob([JSON.stringify(metadata)], {type: "application/json"}));
		formData.append("worker.js", new Blob([scriptContent], {type: "application/javascript+module"}), "worker.js");

		const response = await cloudflare.request({
			method: "PUT",
			path: "/accounts/" + accountId + "/workers/scripts/%s",
			body: formData,
			headers: {}
		});
		return response;
	}`, jsonEscape(workerCode), name)

	result, err := sb.mcp.Execute(ctx, deployJS, sb.accountID)
	if err != nil {
		return nil, fmt.Errorf("deploy worker %q: %w", name, err)
	}

	worker := &DeployedWorker{
		Name:        name,
		Description: description,
		Code:        workerCode,
		DeployedAt:  time.Now(),
		Status:      "active",
		URL:         fmt.Sprintf("https://%s.%s.workers.dev", name, sb.accountID),
	}

	log.Printf("selfbuild: deployed worker %q: %v", name, result)

	// Track in R2
	if err := sb.trackWorker(ctx, worker); err != nil {
		log.Printf("selfbuild: track worker failed: %v", err)
	}

	// Store code in R2 for recovery
	codeKey := fmt.Sprintf("memory/workers/%s/worker.js", name)
	_ = sb.r2.UploadObject(ctx, sb.bucket, codeKey, []byte(workerCode))

	return worker, nil
}

// ListWorkers returns all workers the agent has deployed.
func (sb *SelfBuilder) ListWorkers(ctx context.Context) ([]DeployedWorker, error) {
	data, err := sb.r2.DownloadObject(ctx, sb.bucket, workersIndexKey)
	if err != nil {
		return nil, nil
	}
	var workers []DeployedWorker
	if err := json.Unmarshal(data, &workers); err != nil {
		return nil, nil
	}
	return workers, nil
}

// DeleteWorker removes a worker from Cloudflare.
func (sb *SelfBuilder) DeleteWorker(ctx context.Context, name string) error {
	deleteJS := fmt.Sprintf(`async () => {
		const response = await cloudflare.request({
			method: "DELETE",
			path: "/accounts/" + accountId + "/workers/scripts/%s"
		});
		return response;
	}`, name)

	_, err := sb.mcp.Execute(ctx, deleteJS, sb.accountID)
	if err != nil {
		return fmt.Errorf("delete worker %q: %w", name, err)
	}

	// Update tracking
	workers, _ := sb.ListWorkers(ctx)
	for i, w := range workers {
		if w.Name == name {
			workers[i].Status = "deleted"
			break
		}
	}
	data, _ := json.Marshal(workers)
	_ = sb.r2.UploadObject(ctx, sb.bucket, workersIndexKey, data)

	return nil
}

// GetWorkerCode retrieves the source code of a deployed worker.
func (sb *SelfBuilder) GetWorkerCode(ctx context.Context, name string) (string, error) {
	codeKey := fmt.Sprintf("memory/workers/%s/worker.js", name)
	data, err := sb.r2.DownloadObject(ctx, sb.bucket, codeKey)
	if err != nil {
		return "", fmt.Errorf("worker code not found: %w", err)
	}
	return string(data), nil
}

// CreateKVNamespace creates a KV namespace for Workers to use.
func (sb *SelfBuilder) CreateKVNamespace(ctx context.Context, title string) (string, error) {
	createJS := fmt.Sprintf(`async () => {
		const response = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/storage/kv/namespaces",
			body: { title: %q }
		});
		return response;
	}`, title)

	result, err := sb.mcp.Execute(ctx, createJS, sb.accountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", result), nil
}

// CreateD1Database creates a D1 (SQLite) database.
func (sb *SelfBuilder) CreateD1Database(ctx context.Context, name string) (string, error) {
	createJS := fmt.Sprintf(`async () => {
		const response = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/d1/database",
			body: { name: %q }
		});
		return response;
	}`, name)

	result, err := sb.mcp.Execute(ctx, createJS, sb.accountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", result), nil
}

func (sb *SelfBuilder) trackWorker(ctx context.Context, worker *DeployedWorker) error {
	workers, _ := sb.ListWorkers(ctx)
	found := false
	for i, w := range workers {
		if w.Name == worker.Name {
			workers[i] = *worker
			found = true
			break
		}
	}
	if !found {
		workers = append(workers, *worker)
	}
	data, err := json.Marshal(workers)
	if err != nil {
		return err
	}
	return sb.r2.UploadObject(ctx, sb.bucket, workersIndexKey, data)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
