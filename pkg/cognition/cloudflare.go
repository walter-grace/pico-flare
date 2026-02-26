// Package cognition: cloudflare.go gives the agent full programmatic
// control over the Cloudflare environment — creating, listing, and managing
// all resource types. The agent can provision per-user storage, spin up
// databases, and manage its own infrastructure.
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

// CloudEnv gives the agent full access to the Cloudflare account.
type CloudEnv struct {
	MCP       *mcpclient.Client
	R2        *storage.R2Client
	Bucket    string
	AccountID string
}

func NewCloudEnv(mcp *mcpclient.Client, r2 *storage.R2Client, bucket, accountID string) *CloudEnv {
	return &CloudEnv{MCP: mcp, R2: r2, Bucket: bucket, AccountID: accountID}
}

// --- R2 Bucket Management ---

type BucketInfo struct {
	Name      string `json:"name"`
	CreatedAt string `json:"creation_date,omitempty"`
}

func (ce *CloudEnv) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	code := `async () => {
		const resp = await cloudflare.request({
			method: "GET",
			path: "/accounts/" + accountId + "/r2/buckets"
		});
		return resp;
	}`
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return nil, err
	}
	return parseBuckets(raw), nil
}

func (ce *CloudEnv) CreateBucket(ctx context.Context, name string) error {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/r2/buckets",
			body: { name: %q }
		});
		return resp;
	}`, name)
	_, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	return err
}

func (ce *CloudEnv) DeleteBucket(ctx context.Context, name string) error {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "DELETE",
			path: "/accounts/" + accountId + "/r2/buckets/%s"
		});
		return resp;
	}`, name)
	_, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	return err
}

// --- KV Namespace Management ---

type KVNamespace struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (ce *CloudEnv) ListKVNamespaces(ctx context.Context) ([]KVNamespace, error) {
	code := `async () => {
		const resp = await cloudflare.request({
			method: "GET",
			path: "/accounts/" + accountId + "/storage/kv/namespaces"
		});
		return resp;
	}`
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return nil, err
	}
	return parseKVNamespaces(raw), nil
}

func (ce *CloudEnv) CreateKVNamespace(ctx context.Context, title string) (string, error) {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/storage/kv/namespaces",
			body: { title: %q }
		});
		return resp;
	}`, title)
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", raw), nil
}

func (ce *CloudEnv) KVWrite(ctx context.Context, namespaceID, key, value string) error {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "PUT",
			path: "/accounts/" + accountId + "/storage/kv/namespaces/%s/values/%s",
			body: %s
		});
		return resp;
	}`, namespaceID, key, jsonEscapeValue(value))
	_, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	return err
}

func (ce *CloudEnv) KVRead(ctx context.Context, namespaceID, key string) (string, error) {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "GET",
			path: "/accounts/" + accountId + "/storage/kv/namespaces/%s/values/%s"
		});
		return resp;
	}`, namespaceID, key)
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", raw), nil
}

// --- D1 Database Management ---

type D1Database struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

func (ce *CloudEnv) ListD1Databases(ctx context.Context) ([]D1Database, error) {
	code := `async () => {
		const resp = await cloudflare.request({
			method: "GET",
			path: "/accounts/" + accountId + "/d1/database"
		});
		return resp;
	}`
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return nil, err
	}
	return parseD1Databases(raw), nil
}

func (ce *CloudEnv) CreateD1Database(ctx context.Context, name string) (string, error) {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/d1/database",
			body: { name: %q }
		});
		return resp;
	}`, name)
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", raw), nil
}

func (ce *CloudEnv) D1Query(ctx context.Context, databaseID, sql string) (string, error) {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/d1/database/%s/query",
			body: { sql: %s }
		});
		return resp;
	}`, databaseID, jsonEscapeValue(sql))
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", raw), nil
}

// --- Workers Management ---

type WorkerInfo struct {
	ID         string `json:"id"`
	CreatedOn  string `json:"created_on,omitempty"`
	ModifiedOn string `json:"modified_on,omitempty"`
}

func (ce *CloudEnv) ListWorkers(ctx context.Context) ([]WorkerInfo, error) {
	code := `async () => {
		const resp = await cloudflare.request({
			method: "GET",
			path: "/accounts/" + accountId + "/workers/scripts"
		});
		return resp;
	}`
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return nil, err
	}
	return parseWorkers(raw), nil
}

// DeployWorker uploads a Worker script. Tries ES module multipart first, falls
// back to service-worker format (plain JS body) if FormData isn't available.
func (ce *CloudEnv) DeployWorker(ctx context.Context, name, jsCode string) (string, error) {
	// Primary: ES module via multipart FormData (works if sandbox supports it)
	code := fmt.Sprintf(`async () => {
		const scriptContent = %s;
		const workerName = %q;
		try {
			const metadata = {
				main_module: "worker.js",
				compatibility_date: "2024-09-23",
				compatibility_flags: ["nodejs_compat"]
			};
			const form = new FormData();
			form.append("metadata", new Blob([JSON.stringify(metadata)], {type: "application/json"}));
			form.append("worker.js", new Blob([scriptContent], {type: "application/javascript+module"}), "worker.js");
			const resp = await cloudflare.request({
				method: "PUT",
				path: "/accounts/" + accountId + "/workers/scripts/" + workerName,
				body: form
			});
			// Enable workers.dev route
			try {
				await cloudflare.request({
					method: "POST",
					path: "/accounts/" + accountId + "/workers/scripts/" + workerName + "/subdomain",
					body: { enabled: true }
				});
			} catch(e) {}
			return resp;
		} catch(e) {
			// Fallback: service-worker format (plain JS, no FormData needed)
			const resp = await cloudflare.request({
				method: "PUT",
				path: "/accounts/" + accountId + "/workers/scripts/" + workerName,
				body: scriptContent,
				headers: { "Content-Type": "application/javascript" }
			});
			try {
				await cloudflare.request({
					method: "POST",
					path: "/accounts/" + accountId + "/workers/scripts/" + workerName + "/subdomain",
					body: { enabled: true }
				});
			} catch(e2) {}
			return resp;
		}
	}`, jsonEscapeValue(jsCode), name)
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return "", fmt.Errorf("deploy worker %q: %w", name, err)
	}

	// Get the workers.dev URL
	url := ce.GetWorkerURL(ctx, name)
	return fmt.Sprintf("Deployed. URL: %s\nResult: %v", url, raw), nil
}

func (ce *CloudEnv) DeleteWorker(ctx context.Context, name string) error {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "DELETE",
			path: "/accounts/" + accountId + "/workers/scripts/%s"
		});
		return resp;
	}`, name)
	_, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	return err
}

// --- Workers.dev Subdomain Management ---

func (ce *CloudEnv) GetSubdomain(ctx context.Context) (string, error) {
	code := `async () => {
		const resp = await cloudflare.request({
			method: "GET",
			path: "/accounts/" + accountId + "/workers/subdomain"
		});
		return resp;
	}`
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return "", err
	}
	str, _ := raw.(string)
	var wrapper struct {
		Result struct {
			Subdomain string `json:"subdomain"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(str), &wrapper) == nil && wrapper.Result.Subdomain != "" {
		return wrapper.Result.Subdomain, nil
	}
	return str, nil
}

func (ce *CloudEnv) RegisterSubdomain(ctx context.Context, subdomain string) error {
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "PUT",
			path: "/accounts/" + accountId + "/workers/subdomain",
			body: { subdomain: %q }
		});
		return resp;
	}`, subdomain)
	_, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	return err
}

func (ce *CloudEnv) GetWorkerURL(ctx context.Context, name string) string {
	sub, err := ce.GetSubdomain(ctx)
	if err != nil || sub == "" {
		return "(no workers.dev subdomain — use cf_register_subdomain)"
	}
	return fmt.Sprintf("https://%s.%s.workers.dev", name, sub)
}

// --- Vectorize Index Management ---

type VectorizeIndex struct {
	Name       string `json:"name"`
	Dimensions int    `json:"config_dimensions,omitempty"`
}

func (ce *CloudEnv) ListVectorizeIndexes(ctx context.Context) ([]VectorizeIndex, error) {
	code := `async () => {
		const resp = await cloudflare.request({
			method: "GET",
			path: "/accounts/" + accountId + "/vectorize/v2/indexes"
		});
		return resp;
	}`
	raw, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	if err != nil {
		return nil, err
	}
	return parseVectorizeIndexes(raw), nil
}

func (ce *CloudEnv) CreateVectorizeIndex(ctx context.Context, name string, dimensions int, metric string) error {
	if metric == "" {
		metric = "cosine"
	}
	code := fmt.Sprintf(`async () => {
		const resp = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/vectorize/v2/indexes",
			body: {
				name: %q,
				description: "PicoFlare managed index",
				config: { dimensions: %d, metric: %q }
			}
		});
		return resp;
	}`, name, dimensions, metric)
	_, err := ce.MCP.Execute(ctx, code, ce.AccountID)
	return err
}

// --- Per-User Storage Provisioning ---

// UserStorage represents a user's allocated resources.
type UserStorage struct {
	UserID      string    `json:"user_id"`
	Username    string    `json:"username"`
	R2Prefix    string    `json:"r2_prefix"`
	KVNamespace string    `json:"kv_namespace_id,omitempty"`
	D1Database  string    `json:"d1_database_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

const userStorageIndex = "memory/users/index.json"

func (ce *CloudEnv) ProvisionUserStorage(ctx context.Context, userID, username string) (*UserStorage, error) {
	// Check if already provisioned
	users, _ := ce.LoadUserStorage(ctx)
	for _, u := range users {
		if u.UserID == userID {
			return &u, nil
		}
	}

	us := UserStorage{
		UserID:   userID,
		Username: username,
		R2Prefix: fmt.Sprintf("users/%s/", userID),
		CreatedAt: time.Now(),
	}

	users = append(users, us)
	data, _ := json.Marshal(users)
	if err := ce.R2.UploadObject(ctx, ce.Bucket, userStorageIndex, data); err != nil {
		return nil, err
	}

	// Create user's welcome file
	welcome := fmt.Sprintf("# Storage for %s\nProvisioned: %s\n", username, time.Now().Format(time.RFC3339))
	_ = ce.R2.UploadObject(ctx, ce.Bucket, us.R2Prefix+"README.md", []byte(welcome))

	log.Printf("cloudenv: provisioned storage for user %s (%s)", username, userID)
	return &us, nil
}

func (ce *CloudEnv) LoadUserStorage(ctx context.Context) ([]UserStorage, error) {
	data, err := ce.R2.DownloadObject(ctx, ce.Bucket, userStorageIndex)
	if err != nil {
		return nil, nil
	}
	var users []UserStorage
	json.Unmarshal(data, &users)
	return users, nil
}

// UserR2Write writes data into a user's R2 space.
func (ce *CloudEnv) UserR2Write(ctx context.Context, userID, key string, data []byte) error {
	fullKey := fmt.Sprintf("users/%s/%s", userID, key)
	return ce.R2.UploadObject(ctx, ce.Bucket, fullKey, data)
}

// UserR2Read reads data from a user's R2 space.
func (ce *CloudEnv) UserR2Read(ctx context.Context, userID, key string) ([]byte, error) {
	fullKey := fmt.Sprintf("users/%s/%s", userID, key)
	return ce.R2.DownloadObject(ctx, ce.Bucket, fullKey)
}

// --- Resource Inventory ---

// Inventory returns a full snapshot of what the agent controls.
type ResourceInventory struct {
	Buckets   []BucketInfo     `json:"buckets,omitempty"`
	KV        []KVNamespace    `json:"kv_namespaces,omitempty"`
	D1        []D1Database     `json:"d1_databases,omitempty"`
	Workers   []WorkerInfo     `json:"workers,omitempty"`
	Vectorize []VectorizeIndex `json:"vectorize_indexes,omitempty"`
	Users     []UserStorage    `json:"users,omitempty"`
}

func (ce *CloudEnv) TakeInventory(ctx context.Context) *ResourceInventory {
	inv := &ResourceInventory{}

	inv.Buckets, _ = ce.ListBuckets(ctx)
	inv.KV, _ = ce.ListKVNamespaces(ctx)
	inv.D1, _ = ce.ListD1Databases(ctx)
	inv.Workers, _ = ce.ListWorkers(ctx)
	inv.Vectorize, _ = ce.ListVectorizeIndexes(ctx)
	inv.Users, _ = ce.LoadUserStorage(ctx)

	return inv
}

func (inv *ResourceInventory) Summary() string {
	return fmt.Sprintf("Cloudflare Resources: %d buckets, %d KV, %d D1, %d workers, %d vectorize, %d users",
		len(inv.Buckets), len(inv.KV), len(inv.D1), len(inv.Workers), len(inv.Vectorize), len(inv.Users))
}

// --- Parsing helpers ---

func parseBuckets(raw interface{}) []BucketInfo {
	return parseJSON[[]BucketInfo](raw, "buckets")
}

func parseKVNamespaces(raw interface{}) []KVNamespace {
	return parseJSON[[]KVNamespace](raw, "result")
}

func parseD1Databases(raw interface{}) []D1Database {
	return parseJSON[[]D1Database](raw, "result")
}

func parseWorkers(raw interface{}) []WorkerInfo {
	return parseJSON[[]WorkerInfo](raw, "result")
}

func parseVectorizeIndexes(raw interface{}) []VectorizeIndex {
	return parseJSON[[]VectorizeIndex](raw, "result")
}

func parseJSON[T any](raw interface{}, field string) T {
	var zero T
	str, ok := raw.(string)
	if !ok {
		return zero
	}

	// Try to parse the outer wrapper
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(str), &wrapper); err == nil {
		if data, ok := wrapper[field]; ok {
			var result T
			if json.Unmarshal(data, &result) == nil {
				return result
			}
		}
	}

	// Try direct parse
	var result T
	if json.Unmarshal([]byte(str), &result) == nil {
		return result
	}
	return zero
}

func jsonEscapeValue(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
