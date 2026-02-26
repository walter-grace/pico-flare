// Package cognition: evolution.go enables PicoFlare to create new tools,
// evolve its own prompt, and wire deployed Workers as callable capabilities
// — all at runtime, persisted in R2.
//
// This is what makes PicoFlare self-evolving: the agent can literally
// define new features for itself, deploy them, and start using them
// without a rebuild.
package cognition

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bigneek/picoflare/pkg/storage"
)

// --- Dynamic Tool Registry ---

// DynTool is a tool definition stored in R2 that the agent created for itself.
type DynTool struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Type        string            `json:"type"` // "http", "js", "composite"
	Endpoint    string            `json:"endpoint,omitempty"`
	JSCode      string            `json:"js_code,omitempty"`
	Method      string            `json:"method,omitempty"` // GET, POST
	Headers     map[string]string `json:"headers,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	CreatedBy   string            `json:"created_by"` // "agent" or "user"
	Enabled     bool              `json:"enabled"`
	Uses        int               `json:"uses"`
}

const dynToolsKey = "memory/evolution/tools.json"

// ToolRegistry manages dynamic tools.
type ToolRegistry struct {
	r2     *storage.R2Client
	bucket string
	http   *http.Client
}

func NewToolRegistry(r2 *storage.R2Client, bucket string) *ToolRegistry {
	return &ToolRegistry{
		r2:     r2,
		bucket: bucket,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (tr *ToolRegistry) LoadTools(ctx context.Context) ([]DynTool, error) {
	data, err := tr.r2.DownloadObject(ctx, tr.bucket, dynToolsKey)
	if err != nil {
		return nil, nil
	}
	var tools []DynTool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, nil
	}
	return tools, nil
}

func (tr *ToolRegistry) SaveTools(ctx context.Context, tools []DynTool) error {
	data, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		return err
	}
	return tr.r2.UploadObject(ctx, tr.bucket, dynToolsKey, data)
}

// RegisterTool creates a new dynamic tool.
func (tr *ToolRegistry) RegisterTool(ctx context.Context, tool DynTool) error {
	tools, _ := tr.LoadTools(ctx)

	if tool.CreatedAt.IsZero() {
		tool.CreatedAt = time.Now()
	}
	if tool.CreatedBy == "" {
		tool.CreatedBy = "agent"
	}
	tool.Enabled = true

	// Update or append
	found := false
	for i, t := range tools {
		if t.Name == tool.Name {
			tools[i] = tool
			found = true
			break
		}
	}
	if !found {
		tools = append(tools, tool)
	}

	log.Printf("evolution: registered dynamic tool %q (%s)", tool.Name, tool.Type)
	return tr.SaveTools(ctx, tools)
}

// RemoveTool disables a dynamic tool.
func (tr *ToolRegistry) RemoveTool(ctx context.Context, name string) error {
	tools, _ := tr.LoadTools(ctx)
	for i, t := range tools {
		if t.Name == name {
			tools[i].Enabled = false
			return tr.SaveTools(ctx, tools)
		}
	}
	return fmt.Errorf("tool %q not found", name)
}

// IncrementUse tracks usage of a dynamic tool.
func (tr *ToolRegistry) IncrementUse(ctx context.Context, name string) {
	tools, _ := tr.LoadTools(ctx)
	for i, t := range tools {
		if t.Name == name {
			tools[i].Uses++
			_ = tr.SaveTools(ctx, tools)
			return
		}
	}
}

// CallHTTPTool invokes an HTTP-type dynamic tool.
func (tr *ToolRegistry) CallHTTPTool(ctx context.Context, tool DynTool, input map[string]interface{}) (string, error) {
	method := tool.Method
	if method == "" {
		method = "POST"
	}

	var body io.Reader
	if method == "POST" || method == "PUT" {
		data, _ := json.Marshal(input)
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, tool.Endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range tool.Headers {
		req.Header.Set(k, v)
	}

	resp, err := tr.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	result := string(respBody)
	if len(result) > 8000 {
		result = result[:8000] + "\n...(truncated)"
	}
	return result, nil
}

// --- Worker-as-Tool Bridge ---

// RegisterWorkerAsTool takes a deployed Worker and creates a dynamic tool for it.
func (tr *ToolRegistry) RegisterWorkerAsTool(ctx context.Context, workerName, description, url string, inputSchema map[string]interface{}) error {
	tool := DynTool{
		Name:        "worker_" + workerName,
		Description: description,
		Type:        "http",
		Endpoint:    url,
		Method:      "POST",
		InputSchema: inputSchema,
		CreatedBy:   "agent",
	}
	return tr.RegisterTool(ctx, tool)
}

// --- Self-Prompt Evolution ---

// PromptPatch is a named section the agent has added to its own system prompt.
type PromptPatch struct {
	Name      string    `json:"name"`
	Content   string    `json:"content"`
	Priority  int       `json:"priority"` // lower = inserted earlier
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const promptPatchesKey = "memory/evolution/prompt_patches.json"

func (tr *ToolRegistry) LoadPromptPatches(ctx context.Context) ([]PromptPatch, error) {
	data, err := tr.r2.DownloadObject(ctx, tr.bucket, promptPatchesKey)
	if err != nil {
		return nil, nil
	}
	var patches []PromptPatch
	json.Unmarshal(data, &patches)
	return patches, nil
}

func (tr *ToolRegistry) SavePromptPatch(ctx context.Context, patch PromptPatch) error {
	patches, _ := tr.LoadPromptPatches(ctx)

	if patch.CreatedAt.IsZero() {
		patch.CreatedAt = time.Now()
	}
	patch.UpdatedAt = time.Now()
	patch.Enabled = true

	found := false
	for i, p := range patches {
		if p.Name == patch.Name {
			patches[i] = patch
			found = true
			break
		}
	}
	if !found {
		patches = append(patches, patch)
	}

	data, _ := json.MarshalIndent(patches, "", "  ")
	return tr.r2.UploadObject(ctx, tr.bucket, promptPatchesKey, data)
}

func (tr *ToolRegistry) RemovePromptPatch(ctx context.Context, name string) error {
	patches, _ := tr.LoadPromptPatches(ctx)
	for i, p := range patches {
		if p.Name == name {
			patches[i].Enabled = false
			data, _ := json.MarshalIndent(patches, "", "  ")
			return tr.r2.UploadObject(ctx, tr.bucket, promptPatchesKey, data)
		}
	}
	return fmt.Errorf("patch %q not found", name)
}

// BuildPromptAdditions returns all active prompt patches concatenated.
func (tr *ToolRegistry) BuildPromptAdditions(ctx context.Context) string {
	patches, _ := tr.LoadPromptPatches(ctx)
	if len(patches) == 0 {
		return ""
	}

	var active []PromptPatch
	for _, p := range patches {
		if p.Enabled {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Self-Defined Extensions\n")
	for _, p := range active {
		sb.WriteString(fmt.Sprintf("### %s\n%s\n\n", p.Name, p.Content))
	}
	return sb.String()
}

// --- Feature Store ---

// Feature is a complete feature spec the agent designed and can implement.
type Feature struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Status      string   `json:"status"` // "idea", "designed", "implemented", "deployed"
	WorkerName  string   `json:"worker_name,omitempty"`
	WorkerCode  string   `json:"worker_code,omitempty"`
	ToolName    string   `json:"tool_name,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const featuresKey = "memory/evolution/features.json"

func (tr *ToolRegistry) LoadFeatures(ctx context.Context) ([]Feature, error) {
	data, err := tr.r2.DownloadObject(ctx, tr.bucket, featuresKey)
	if err != nil {
		return nil, nil
	}
	var features []Feature
	json.Unmarshal(data, &features)
	return features, nil
}

func (tr *ToolRegistry) SaveFeature(ctx context.Context, feature Feature) error {
	features, _ := tr.LoadFeatures(ctx)

	if feature.CreatedAt.IsZero() {
		feature.CreatedAt = time.Now()
	}
	feature.UpdatedAt = time.Now()

	found := false
	for i, f := range features {
		if f.Name == feature.Name {
			features[i] = feature
			found = true
			break
		}
	}
	if !found {
		features = append(features, feature)
	}

	data, _ := json.MarshalIndent(features, "", "  ")
	return tr.r2.UploadObject(ctx, tr.bucket, featuresKey, data)
}
