package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	cf "github.com/bigneek/picoflare/pkg/cloudflare"
	"github.com/bigneek/picoflare/pkg/cognition"
	"github.com/bigneek/picoflare/pkg/llm"
	"github.com/bigneek/picoflare/pkg/mcpclient"
	"github.com/bigneek/picoflare/pkg/storage"
)

// Tool represents an executable tool the agent can invoke.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
	Execute     func(ctx context.Context, args map[string]interface{}) (string, error)
}

// BuildTools creates the full PicoFlare tool set.
func BuildTools(
	mcp *mcpclient.Client,
	r2 *storage.R2Client,
	cfClient *cf.Client,
	mem *cognition.Memory,
	meta *cognition.MetaCognition,
	builder *cognition.SelfBuilder,
	ledger *cognition.TokenLedger,
	cloud *cognition.CloudEnv,
	registry *cognition.ToolRegistry,
	bucket, accountID string,
) []Tool {
	var tools []Tool

	// ── Cloudflare API tools ──

	if mcp != nil {
		tools = append(tools, Tool{
			Name:        "cf_search",
			Description: "Search the Cloudflare API spec for endpoints. Use plain English like 'list workers', 'DNS records', 'R2 buckets'. Returns matching API paths.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string", "description": "What to search for"},
				},
				"required": []string{"query"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				query, _ := args["query"].(string)
				code := fmt.Sprintf(`async () => {
					const q = %q.toLowerCase();
					const results = [];
					for (const [path, methods] of Object.entries(spec.paths)) {
						for (const [method, op] of Object.entries(methods)) {
							if (typeof op !== 'object' || !op) continue;
							const haystack = (path + ' ' + (op.summary || '') + ' ' + (op.tags || []).join(' ')).toLowerCase();
							if (haystack.includes(q)) {
								results.push({ method: method.toUpperCase(), path, summary: op.summary });
							}
						}
					}
					return results.slice(0, 15);
				}`, query)
				out, err := mcp.Search(ctx, code)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%v", out), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "cf_execute",
			Description: "Execute arbitrary JavaScript with Cloudflare API access. Has `cloudflare.request({method, path, body, headers})`, `accountId`, `FormData`, `Blob`. Use for any Cloudflare operation. Return result from async IIFE.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"code": map[string]interface{}{"type": "string", "description": "JavaScript async function body"},
				},
				"required": []string{"code"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				code, _ := args["code"].(string)
				if !strings.Contains(code, "async") {
					code = fmt.Sprintf(`async () => { %s }`, code)
				}
				out, err := mcp.Execute(ctx, code, accountID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%v", out), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "cf_api",
			Description: "Call any Cloudflare REST API endpoint directly. Simpler than cf_execute — just specify method, path, and optional JSON body. The path is relative to /accounts/{account_id}/. Example: method=GET, path=workers/scripts to list workers.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"method": map[string]interface{}{"type": "string", "description": "HTTP method: GET, POST, PUT, DELETE, PATCH"},
					"path":   map[string]interface{}{"type": "string", "description": "API path relative to /accounts/{account_id}/ (e.g. 'workers/scripts', 'r2/buckets', 'storage/kv/namespaces')"},
					"body":   map[string]interface{}{"type": "string", "description": "JSON body for POST/PUT requests (optional)"},
				},
				"required": []string{"method", "path"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				method, _ := args["method"].(string)
				path, _ := args["path"].(string)
				bodyStr, _ := args["body"].(string)

				var bodyJS string
				if bodyStr != "" {
					bodyJS = fmt.Sprintf(", body: %s", bodyStr)
				}

				code := fmt.Sprintf(`async () => {
					const resp = await cloudflare.request({
						method: %q,
						path: "/accounts/" + accountId + "/%s"%s
					});
					return resp;
				}`, method, path, bodyJS)

				out, err := mcp.Execute(ctx, code, accountID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%v", out), nil
			},
		})
	}

	// ── HTTP Request (runs from bot process, bypasses Workers Free 403) ──
	tools = append(tools, Tool{
		Name:        "http_request",
		Description: "Make HTTP requests from the bot (your machine). Use this to call Workers on workers.dev — cf_execute gets 403 on the free plan because it runs in Cloudflare. This runs locally so it works. Use for: testing deployed Workers, calling Worker APIs, fetching from your fib3d/voice-handler etc.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url":     map[string]interface{}{"type": "string", "description": "Full URL (e.g. https://fib3d.nico-zahniser.workers.dev/)"},
				"method":  map[string]interface{}{"type": "string", "description": "GET, POST, PUT, etc (default GET)"},
				"body":    map[string]interface{}{"type": "string", "description": "Request body for POST/PUT (optional)"},
				"headers": map[string]interface{}{"type": "object", "description": "JSON object of headers, e.g. {\"Content-Type\":\"application/json\"}"},
			},
			"required": []string{"url"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			url, _ := args["url"].(string)
			method, _ := args["method"].(string)
			if method == "" {
				method = "GET"
			}
			bodyStr, _ := args["body"].(string)

			var body io.Reader
			if bodyStr != "" {
				body = bytes.NewReader([]byte(bodyStr))
			}

			req, err := http.NewRequestWithContext(ctx, method, url, body)
			if err != nil {
				return "", fmt.Errorf("create request: %w", err)
			}

			if h := args["headers"]; h != nil {
				var headers map[string]interface{}
				switch v := h.(type) {
				case map[string]interface{}:
					headers = v
				case string:
					_ = json.Unmarshal([]byte(v), &headers)
				}
				for k, v := range headers {
					if s, ok := v.(string); ok {
						req.Header.Set(k, s)
					}
				}
			}
			if body != nil && req.Header.Get("Content-Type") == "" {
				req.Header.Set("Content-Type", "application/json")
			}

			client := &http.Client{Timeout: 60 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				return "", fmt.Errorf("request failed: %w", err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return "", fmt.Errorf("read response: %w", err)
			}

			result := string(respBody)
			if len(result) > 15000 {
				result = result[:15000] + "\n...(truncated)"
			}
			return fmt.Sprintf("Status: %d\n%s", resp.StatusCode, result), nil
		},
	})

	// ── R2 Storage tools ──

	if r2 != nil && bucket != "" {
		tools = append(tools, Tool{
			Name:        "r2_write",
			Description: "Write data to R2 storage. Use for files, configs, or any persistent data.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"key":     map[string]interface{}{"type": "string", "description": "Object key (e.g. 'data/config.json')"},
					"content": map[string]interface{}{"type": "string", "description": "Content to write"},
				},
				"required": []string{"key", "content"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				key, _ := args["key"].(string)
				content, _ := args["content"].(string)
				if err := r2.UploadObject(ctx, bucket, key, []byte(content)); err != nil {
					return "", err
				}
				return fmt.Sprintf("Written %d bytes to r2://%s/%s", len(content), bucket, key), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "r2_read",
			Description: "Read data from R2 storage.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"key": map[string]interface{}{"type": "string", "description": "Object key to read"},
				},
				"required": []string{"key"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				key, _ := args["key"].(string)
				data, err := r2.DownloadObject(ctx, bucket, key)
				if err != nil {
					return "", err
				}
				result := string(data)
				if len(result) > 8000 {
					result = result[:8000] + "\n...(truncated)"
				}
				return result, nil
			},
		})
	}

	// ── Cognitive Memory tools ──

	if mem != nil {
		tools = append(tools, Tool{
			Name:        "learn_fact",
			Description: "Store a fact in semantic memory. Use for user preferences, project details, domain knowledge — anything worth remembering permanently.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"category": map[string]interface{}{
						"type": "string",
						"description": "Category: user, system, domain, preference, project",
						"enum":        []string{"user", "system", "domain", "preference", "project"},
					},
					"content":    map[string]interface{}{"type": "string", "description": "The fact to remember"},
					"confidence": map[string]interface{}{"type": "number", "description": "Confidence 0.0-1.0 (default 0.8)"},
				},
				"required": []string{"category", "content"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				category, _ := args["category"].(string)
				content, _ := args["content"].(string)
				confidence := 0.8
				if c, ok := args["confidence"].(float64); ok {
					confidence = c
				}
				err := mem.LearnFact(ctx, cognition.Fact{
					Category:   category,
					Content:    content,
					Confidence: confidence,
					Source:      "agent",
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Learned [%s]: %s (confidence: %.0f%%)", category, content, confidence*100), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "recall_facts",
			Description: "Retrieve facts from semantic memory, optionally filtered by category.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"category": map[string]interface{}{
						"type": "string",
						"description": "Filter by category (empty = all)",
					},
				},
				"required": []string{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				category, _ := args["category"].(string)
				facts := mem.QueryFacts(ctx, category)
				if len(facts) == 0 {
					return "No facts stored yet.", nil
				}
				var lines []string
				for _, f := range facts {
					lines = append(lines, fmt.Sprintf("- [%s] %s (%.0f%%)", f.Category, f.Content, f.Confidence*100))
				}
				return strings.Join(lines, "\n"), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "save_episode",
			Description: "Log a notable event, insight, or experience to episodic memory.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"type":    map[string]interface{}{"type": "string", "description": "Event type: conversation, tool_use, error, insight, goal"},
					"summary": map[string]interface{}{"type": "string", "description": "Brief description of what happened"},
					"detail":  map[string]interface{}{"type": "string", "description": "Detailed information (optional)"},
				},
				"required": []string{"type", "summary"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				epType, _ := args["type"].(string)
				summary, _ := args["summary"].(string)
				detail, _ := args["detail"].(string)
				err := mem.SaveEpisode(ctx, cognition.Episode{
					Type:    epType,
					Summary: summary,
					Detail:  detail,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Episode logged [%s]: %s", epType, summary), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "learn_procedure",
			Description: "Store a reusable procedure/skill. Useful for remembering how to accomplish recurring tasks (e.g. 'deploy a worker', 'create DNS record').",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":        map[string]interface{}{"type": "string", "description": "Procedure name"},
					"description": map[string]interface{}{"type": "string", "description": "What this procedure does"},
					"steps":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Ordered steps"},
					"code":        map[string]interface{}{"type": "string", "description": "JS code for cf_execute (optional)"},
				},
				"required": []string{"name", "description", "steps"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				desc, _ := args["description"].(string)
				code, _ := args["code"].(string)

				var steps []string
				if rawSteps, ok := args["steps"].([]interface{}); ok {
					for _, s := range rawSteps {
						if str, ok := s.(string); ok {
							steps = append(steps, str)
						}
					}
				}

				err := mem.SaveProcedure(ctx, cognition.Procedure{
					Name:        name,
					Description: desc,
					Steps:       steps,
					Code:        code,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Procedure learned: %s (%d steps)", name, len(steps)), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "recall_memory",
			Description: "Read cognitive memory context: facts, recent episodes, and learned procedures. Use to recall what you know.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"budget": map[string]interface{}{
						"type": "string",
						"description": "How much context: 'small' (1000 chars), 'medium' (4000), 'large' (8000)",
						"enum": []string{"small", "medium", "large"},
					},
				},
				"required": []string{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				budgetSize, _ := args["budget"].(string)
				budget := cognition.DefaultBudget
				switch budgetSize {
				case "small":
					budget.MaxTotalChars = 1000
				case "large":
					budget.MaxTotalChars = 8000
				default:
					budget.MaxTotalChars = 4000
				}
				return mem.BuildContext(ctx, budget), nil
			},
		})
	}

	// Self-Build tools (R2-based worker tracking, kept for history)
	_ = builder

	// ── Meta-cognition tools ──

	if meta != nil {
		tools = append(tools, Tool{
			Name:        "set_goal",
			Description: "Set or update a goal. Use for tracking what you're working toward.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"description": map[string]interface{}{"type": "string", "description": "Goal description"},
					"priority":    map[string]interface{}{"type": "integer", "description": "Priority 1 (highest) to 5 (lowest)"},
					"status":      map[string]interface{}{"type": "string", "description": "active, completed, blocked, abandoned"},
				},
				"required": []string{"description"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				desc, _ := args["description"].(string)
				priority := 3
				if p, ok := args["priority"].(float64); ok {
					priority = int(p)
				}
				status := "active"
				if s, ok := args["status"].(string); ok {
					status = s
				}
				err := meta.SaveGoal(ctx, cognition.Goal{
					Description: desc,
					Priority:    priority,
					Status:      status,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Goal set [P%d]: %s (%s)", priority, desc, status), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "self_reflect",
			Description: "Record a self-reflection: what happened, how it went, what to improve. Use after complex tasks or when you notice patterns.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"observation": map[string]interface{}{"type": "string", "description": "What you observed"},
					"assessment":  map[string]interface{}{"type": "string", "description": "How it went"},
					"improvement": map[string]interface{}{"type": "string", "description": "What to do better next time"},
				},
				"required": []string{"observation", "assessment", "improvement"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				obs, _ := args["observation"].(string)
				assess, _ := args["assessment"].(string)
				improve, _ := args["improvement"].(string)
				err := meta.SaveReflection(ctx, cognition.Reflection{
					Observation: obs,
					Assessment:  assess,
					Improvement: improve,
				})
				if err != nil {
					return "", err
				}
				return "Reflection saved.", nil
			},
		})
	}

	// ── Tokenomics tool ──

	if ledger != nil {
		tools = append(tools, Tool{
			Name:        "tokenomics",
			Description: "View token usage, costs, and efficiency metrics for this session and lifetime.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				return ledger.Report(), nil
			},
		})
	}

	// ── Full Cloudflare Environment tools (direct REST API) ──

	if cfClient != nil {
		tools = append(tools, Tool{
			Name:        "cf_inventory",
			Description: "Full inventory of all Cloudflare resources: workers.dev subdomain, Workers, KV, D1, R2 buckets, Vectorize indexes.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				inv := cfClient.TakeInventory(ctx)
				data, _ := json.MarshalIndent(inv, "", "  ")
				return inv.Summary() + "\n\n" + string(data), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "cf_verify_token",
			Description: "Verify the Cloudflare API token is valid and check its permissions.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				status, err := cfClient.VerifyToken(ctx)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Token status: %s", status), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "cf_get_subdomain",
			Description: "Get the workers.dev subdomain for this account. Workers are accessible at <name>.<subdomain>.workers.dev.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				sub, err := cfClient.GetSubdomain(ctx)
				if err != nil {
					return "", fmt.Errorf("get subdomain: %w (may need to register one)", err)
				}
				return fmt.Sprintf("Subdomain: %s.workers.dev", sub), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "cf_register_subdomain",
			Description: "Register a workers.dev subdomain for this Cloudflare account. Required before Workers can be accessed via <name>.<subdomain>.workers.dev URLs.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"subdomain": map[string]interface{}{"type": "string", "description": "Desired subdomain (lowercase, alphanumeric)"},
				},
				"required": []string{"subdomain"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				sub, _ := args["subdomain"].(string)
				if err := cfClient.RegisterSubdomain(ctx, sub); err != nil {
					return "", err
				}
				return fmt.Sprintf("Subdomain registered: %s.workers.dev", sub), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "deploy_worker",
			Description: "Deploy a Cloudflare Worker (ES module JS). Creates a live HTTP endpoint. Use for APIs, webhooks, MCP servers, web UIs.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Worker name (lowercase, hyphens ok)"},
					"code": map[string]interface{}{"type": "string", "description": "JavaScript (ES module) Worker code"},
				},
				"required": []string{"name", "code"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				code, _ := args["code"].(string)
				if err := cfClient.DeployWorker(ctx, name, code); err != nil {
					return "", err
				}
				url := cfClient.GetWorkerURL(ctx, name)
				return fmt.Sprintf("Worker %q deployed.\nURL: %s", name, url), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "delete_worker",
			Description: "Delete a deployed Cloudflare Worker.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Worker name to delete"},
				},
				"required": []string{"name"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				if err := cfClient.DeleteWorker(ctx, name); err != nil {
					return "", err
				}
				return fmt.Sprintf("Worker %q deleted.", name), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "list_workers",
			Description: "List all Cloudflare Workers on the account.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				workers, err := cfClient.ListWorkers(ctx)
				if err != nil {
					return "", err
				}
				if len(workers) == 0 {
					return "No workers deployed.", nil
				}
				var lines []string
				for _, w := range workers {
					url := cfClient.GetWorkerURL(ctx, w.ID)
					lines = append(lines, fmt.Sprintf("- %s → %s (modified: %s)", w.ID, url, w.ModifiedOn))
				}
				return strings.Join(lines, "\n"), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "create_bucket",
			Description: "Create an R2 storage bucket.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Bucket name (lowercase, hyphens ok)"},
				},
				"required": []string{"name"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				if err := cfClient.CreateR2Bucket(ctx, name); err != nil {
					return "", err
				}
				return fmt.Sprintf("R2 bucket %q created.", name), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "list_buckets",
			Description: "List all R2 storage buckets.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				buckets, err := cfClient.ListR2Buckets(ctx)
				if err != nil {
					return "", err
				}
				if len(buckets) == 0 {
					return "No R2 buckets.", nil
				}
				var lines []string
				for _, b := range buckets {
					lines = append(lines, fmt.Sprintf("- %s (created: %s)", b.Name, b.CreationDate))
				}
				return strings.Join(lines, "\n"), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "create_kv",
			Description: "Create a Workers KV namespace for edge key-value storage.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title": map[string]interface{}{"type": "string", "description": "KV namespace title"},
				},
				"required": []string{"title"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				title, _ := args["title"].(string)
				ns, err := cfClient.CreateKVNamespace(ctx, title)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("KV namespace %q created (ID: %s)", ns.Title, ns.ID), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "kv_write",
			Description: "Write a value to a KV namespace.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"namespace_id": map[string]interface{}{"type": "string", "description": "KV namespace ID"},
					"key":          map[string]interface{}{"type": "string", "description": "Key"},
					"value":        map[string]interface{}{"type": "string", "description": "Value to store"},
				},
				"required": []string{"namespace_id", "key", "value"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				nsID, _ := args["namespace_id"].(string)
				key, _ := args["key"].(string)
				value, _ := args["value"].(string)
				if err := cfClient.KVWrite(ctx, nsID, key, []byte(value)); err != nil {
					return "", err
				}
				return fmt.Sprintf("Written %q to KV %s", key, nsID), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "kv_read",
			Description: "Read a value from a KV namespace.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"namespace_id": map[string]interface{}{"type": "string", "description": "KV namespace ID"},
					"key":          map[string]interface{}{"type": "string", "description": "Key to read"},
				},
				"required": []string{"namespace_id", "key"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				nsID, _ := args["namespace_id"].(string)
				key, _ := args["key"].(string)
				data, err := cfClient.KVRead(ctx, nsID, key)
				if err != nil {
					return "", err
				}
				result := string(data)
				if len(result) > 8000 {
					result = result[:8000] + "\n...(truncated)"
				}
				return result, nil
			},
		})

		tools = append(tools, Tool{
			Name:        "create_database",
			Description: "Create a D1 (SQLite at the edge) database.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Database name"},
				},
				"required": []string{"name"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				db, err := cfClient.CreateD1Database(ctx, name)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("D1 database %q created (UUID: %s)", db.Name, db.UUID), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "query_database",
			Description: "Run SQL against a D1 database.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"database_id": map[string]interface{}{"type": "string", "description": "D1 database UUID"},
					"sql":         map[string]interface{}{"type": "string", "description": "SQL query"},
				},
				"required": []string{"database_id", "sql"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				dbID, _ := args["database_id"].(string)
				sql, _ := args["sql"].(string)
				return cfClient.D1Query(ctx, dbID, sql)
			},
		})

		tools = append(tools, Tool{
			Name:        "create_vectorize_index",
			Description: "Create a Vectorize vector database index for semantic search.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":       map[string]interface{}{"type": "string", "description": "Index name"},
					"dimensions": map[string]interface{}{"type": "integer", "description": "Vector dimensions (e.g. 768)"},
					"metric":     map[string]interface{}{"type": "string", "description": "Distance metric", "enum": []string{"cosine", "euclidean", "dot-product"}},
				},
				"required": []string{"name", "dimensions"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				dims := 768
				if d, ok := args["dimensions"].(float64); ok {
					dims = int(d)
				}
				metric, _ := args["metric"].(string)
				if err := cfClient.CreateVectorizeIndex(ctx, name, dims, metric); err != nil {
					return "", err
				}
				return fmt.Sprintf("Vectorize index %q created (%d dims, %s)", name, dims, metric), nil
			},
		})
	}

	// ── MCP-based Cloudflare tools (used when direct API token unavailable) ──

	if cfClient == nil && cloud != nil {
		tools = append(tools, Tool{
			Name:        "cf_inventory",
			Description: "Full inventory of all Cloudflare resources: Workers, KV, D1, R2 buckets, Vectorize, and users.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				inv := cloud.TakeInventory(ctx)
				data, _ := json.MarshalIndent(inv, "", "  ")
				return inv.Summary() + "\n\n" + string(data), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "cf_get_subdomain",
			Description: "Get the workers.dev subdomain for this account. Workers are accessible at <name>.<subdomain>.workers.dev.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				sub, err := cloud.GetSubdomain(ctx)
				if err != nil {
					return "", fmt.Errorf("get subdomain: %w", err)
				}
				return fmt.Sprintf("Subdomain: %s.workers.dev", sub), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "cf_register_subdomain",
			Description: "Register a workers.dev subdomain. Required before Workers can be accessed via URL.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"subdomain": map[string]interface{}{"type": "string", "description": "Desired subdomain (lowercase, alphanumeric)"},
				},
				"required": []string{"subdomain"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				sub, _ := args["subdomain"].(string)
				if err := cloud.RegisterSubdomain(ctx, sub); err != nil {
					return "", err
				}
				return fmt.Sprintf("Subdomain registered: %s.workers.dev", sub), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "deploy_worker",
			Description: "Deploy a Cloudflare Worker. Write JS code and it becomes a live HTTP endpoint. Auto-enables workers.dev route.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Worker name (lowercase, hyphens ok)"},
					"code": map[string]interface{}{"type": "string", "description": "JavaScript Worker code (ES module or service worker format)"},
				},
				"required": []string{"name", "code"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				code, _ := args["code"].(string)
				result, err := cloud.DeployWorker(ctx, name, code)
				if err != nil {
					return "", err
				}
				return result, nil
			},
		})

		tools = append(tools, Tool{
			Name:        "delete_worker",
			Description: "Delete a deployed Cloudflare Worker.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Worker name to delete"},
				},
				"required": []string{"name"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				if err := cloud.DeleteWorker(ctx, name); err != nil {
					return "", err
				}
				return fmt.Sprintf("Worker %q deleted.", name), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "list_workers",
			Description: "List all Cloudflare Workers on the account.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				workers, err := cloud.ListWorkers(ctx)
				if err != nil {
					return "", err
				}
				if len(workers) == 0 {
					return "No workers deployed.", nil
				}
				var lines []string
				for _, w := range workers {
					url := cloud.GetWorkerURL(ctx, w.ID)
					lines = append(lines, fmt.Sprintf("- %s → %s", w.ID, url))
				}
				return strings.Join(lines, "\n"), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "create_bucket",
			Description: "Create an R2 storage bucket.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Bucket name"},
				},
				"required": []string{"name"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				if err := cloud.CreateBucket(ctx, name); err != nil {
					return "", err
				}
				return fmt.Sprintf("R2 bucket %q created.", name), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "list_buckets",
			Description: "List all R2 storage buckets.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				buckets, err := cloud.ListBuckets(ctx)
				if err != nil {
					return "", err
				}
				if len(buckets) == 0 {
					return "No R2 buckets.", nil
				}
				var lines []string
				for _, b := range buckets {
					lines = append(lines, fmt.Sprintf("- %s", b.Name))
				}
				return strings.Join(lines, "\n"), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "create_kv",
			Description: "Create a Workers KV namespace.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title": map[string]interface{}{"type": "string", "description": "KV namespace title"},
				},
				"required": []string{"title"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				title, _ := args["title"].(string)
				result, err := cloud.CreateKVNamespace(ctx, title)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("KV namespace %q created: %s", title, result), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "kv_write",
			Description: "Write a value to a KV namespace.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"namespace_id": map[string]interface{}{"type": "string", "description": "KV namespace ID"},
					"key":          map[string]interface{}{"type": "string", "description": "Key"},
					"value":        map[string]interface{}{"type": "string", "description": "Value"},
				},
				"required": []string{"namespace_id", "key", "value"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				nsID, _ := args["namespace_id"].(string)
				key, _ := args["key"].(string)
				value, _ := args["value"].(string)
				if err := cloud.KVWrite(ctx, nsID, key, value); err != nil {
					return "", err
				}
				return fmt.Sprintf("Written %q to KV %s", key, nsID), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "kv_read",
			Description: "Read a value from a KV namespace.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"namespace_id": map[string]interface{}{"type": "string", "description": "KV namespace ID"},
					"key":          map[string]interface{}{"type": "string", "description": "Key"},
				},
				"required": []string{"namespace_id", "key"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				nsID, _ := args["namespace_id"].(string)
				key, _ := args["key"].(string)
				return cloud.KVRead(ctx, nsID, key)
			},
		})

		tools = append(tools, Tool{
			Name:        "create_database",
			Description: "Create a D1 (SQLite at the edge) database.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Database name"},
				},
				"required": []string{"name"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				result, err := cloud.CreateD1Database(ctx, name)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("D1 database %q created: %s", name, result), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "query_database",
			Description: "Run SQL against a D1 database.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"database_id": map[string]interface{}{"type": "string", "description": "D1 database UUID"},
					"sql":         map[string]interface{}{"type": "string", "description": "SQL query"},
				},
				"required": []string{"database_id", "sql"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				dbID, _ := args["database_id"].(string)
				sql, _ := args["sql"].(string)
				return cloud.D1Query(ctx, dbID, sql)
			},
		})

		tools = append(tools, Tool{
			Name:        "create_vectorize_index",
			Description: "Create a Vectorize vector database index.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":       map[string]interface{}{"type": "string", "description": "Index name"},
					"dimensions": map[string]interface{}{"type": "integer", "description": "Vector dimensions (e.g. 768)"},
					"metric":     map[string]interface{}{"type": "string", "description": "Distance metric", "enum": []string{"cosine", "euclidean", "dot-product"}},
				},
				"required": []string{"name", "dimensions"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				dims := 768
				if d, ok := args["dimensions"].(float64); ok {
					dims = int(d)
				}
				metric, _ := args["metric"].(string)
				if err := cloud.CreateVectorizeIndex(ctx, name, dims, metric); err != nil {
					return "", err
				}
				return fmt.Sprintf("Vectorize index %q created (%d dims, %s)", name, dims, metric), nil
			},
		})
	}

	// ── Per-User Storage tools (R2-based) ──

	if cloud != nil {
		tools = append(tools, Tool{
			Name:        "provision_user",
			Description: "Provision dedicated R2 storage for a user. Use when a new user needs persistent storage.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"user_id":  map[string]interface{}{"type": "string", "description": "User identifier (e.g. Telegram ID)"},
					"username": map[string]interface{}{"type": "string", "description": "Display name"},
				},
				"required": []string{"user_id", "username"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				userID, _ := args["user_id"].(string)
				username, _ := args["username"].(string)
				us, err := cloud.ProvisionUserStorage(ctx, userID, username)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("User %s provisioned:\n- R2 prefix: %s\n- Created: %s",
					us.Username, us.R2Prefix, us.CreatedAt.Format(time.RFC3339)), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "user_store",
			Description: "Write data to a user's personal R2 space.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"user_id": map[string]interface{}{"type": "string", "description": "User ID"},
					"key":     map[string]interface{}{"type": "string", "description": "File key"},
					"content": map[string]interface{}{"type": "string", "description": "Content"},
				},
				"required": []string{"user_id", "key", "content"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				userID, _ := args["user_id"].(string)
				key, _ := args["key"].(string)
				content, _ := args["content"].(string)
				if err := cloud.UserR2Write(ctx, userID, key, []byte(content)); err != nil {
					return "", err
				}
				return fmt.Sprintf("Stored %d bytes for user %s at %s", len(content), userID, key), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "user_retrieve",
			Description: "Read data from a user's personal R2 space.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"user_id": map[string]interface{}{"type": "string", "description": "User ID"},
					"key":     map[string]interface{}{"type": "string", "description": "File key"},
				},
				"required": []string{"user_id", "key"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				userID, _ := args["user_id"].(string)
				key, _ := args["key"].(string)
				data, err := cloud.UserR2Read(ctx, userID, key)
				if err != nil {
					return "", err
				}
				result := string(data)
				if len(result) > 8000 {
					result = result[:8000] + "\n...(truncated)"
				}
				return result, nil
			},
		})
	}

	// ── Self-Evolution tools ──

	if registry != nil {
		tools = append(tools, Tool{
			Name:        "create_tool",
			Description: "Create a new tool for yourself. HTTP tools call an endpoint. JS tools store Cloudflare Code Mode scripts. After creating a tool, it becomes immediately available. Use this to extend your own capabilities.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":        map[string]interface{}{"type": "string", "description": "Tool name (lowercase, underscores ok)"},
					"description": map[string]interface{}{"type": "string", "description": "What the tool does"},
					"type":        map[string]interface{}{"type": "string", "description": "Tool type: 'http' (calls URL) or 'js' (Cloudflare Code Mode script)", "enum": []string{"http", "js"}},
					"endpoint":    map[string]interface{}{"type": "string", "description": "For http tools: the URL to call"},
					"method":      map[string]interface{}{"type": "string", "description": "HTTP method (default POST)"},
					"js_code":     map[string]interface{}{"type": "string", "description": "For js tools: the JavaScript code"},
				},
				"required": []string{"name", "description", "type"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				desc, _ := args["description"].(string)
				toolType, _ := args["type"].(string)
				endpoint, _ := args["endpoint"].(string)
				method, _ := args["method"].(string)
				jsCode, _ := args["js_code"].(string)

				dt := cognition.DynTool{
					Name:        "dyn_" + name,
					Description: desc,
					Type:        toolType,
					Endpoint:    endpoint,
					Method:      method,
					JSCode:      jsCode,
				}
				if err := registry.RegisterTool(ctx, dt); err != nil {
					return "", err
				}
				return fmt.Sprintf("Tool %q created and registered. It will be available on next message.", "dyn_"+name), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "list_my_tools",
			Description: "List all dynamic tools you've created for yourself.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				dynTools, _ := registry.LoadTools(ctx)
				if len(dynTools) == 0 {
					return "No dynamic tools created yet.", nil
				}
				var lines []string
				for _, dt := range dynTools {
					status := "enabled"
					if !dt.Enabled {
						status = "disabled"
					}
					lines = append(lines, fmt.Sprintf("- **%s** [%s] %s: %s (used %dx)",
						dt.Name, dt.Type, status, dt.Description, dt.Uses))
				}
				return strings.Join(lines, "\n"), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "remove_tool",
			Description: "Disable a dynamic tool you created.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string", "description": "Tool name to disable"},
				},
				"required": []string{"name"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				return "", registry.RemoveTool(ctx, name)
			},
		})

		tools = append(tools, Tool{
			Name:        "evolve_prompt",
			Description: "Add or update a section in your own system prompt. Use this to give yourself new instructions, personality traits, knowledge, or behavioral rules that persist across conversations.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":     map[string]interface{}{"type": "string", "description": "Section name (e.g. 'Spanish mode', 'Code style preferences')"},
					"content":  map[string]interface{}{"type": "string", "description": "The prompt content to add"},
					"priority": map[string]interface{}{"type": "integer", "description": "Order in prompt (1=first, 10=last, default 5)"},
				},
				"required": []string{"name", "content"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				content, _ := args["content"].(string)
				priority := 5
				if p, ok := args["priority"].(float64); ok {
					priority = int(p)
				}
				err := registry.SavePromptPatch(ctx, cognition.PromptPatch{
					Name:     name,
					Content:  content,
					Priority: priority,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Prompt section %q saved. Will take effect on next system prompt refresh.", name), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "design_feature",
			Description: "Design a new feature for yourself. Describe what it does, what Worker code it needs, and what tool it creates. Use this to plan before building.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":        map[string]interface{}{"type": "string", "description": "Feature name"},
					"description": map[string]interface{}{"type": "string", "description": "What this feature does"},
					"status":      map[string]interface{}{"type": "string", "description": "idea, designed, implemented, deployed", "enum": []string{"idea", "designed", "implemented", "deployed"}},
					"worker_code": map[string]interface{}{"type": "string", "description": "Worker JavaScript code (if ready)"},
				},
				"required": []string{"name", "description"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				name, _ := args["name"].(string)
				desc, _ := args["description"].(string)
				status, _ := args["status"].(string)
				code, _ := args["worker_code"].(string)
				if status == "" {
					status = "idea"
				}
				err := registry.SaveFeature(ctx, cognition.Feature{
					Name:        name,
					Description: desc,
					Status:      status,
					WorkerCode:  code,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Feature %q saved [%s]", name, status), nil
			},
		})

		tools = append(tools, Tool{
			Name:        "list_features",
			Description: "List all features in the feature store (ideas, designs, implementations).",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				features, _ := registry.LoadFeatures(ctx)
				if len(features) == 0 {
					return "No features in the store yet.", nil
				}
				var lines []string
				for _, f := range features {
					line := fmt.Sprintf("- **%s** [%s]: %s", f.Name, f.Status, f.Description)
					if f.WorkerName != "" {
						line += fmt.Sprintf(" (worker: %s)", f.WorkerName)
					}
					lines = append(lines, line)
				}
				return strings.Join(lines, "\n"), nil
			},
		})
	}

	return tools
}

// ToLLMDefs converts tools to OpenAI function-calling format.
func ToLLMDefs(tools []Tool) []llm.ToolDef {
	defs := make([]llm.ToolDef, len(tools))
	for i, t := range tools {
		defs[i] = llm.ToolDef{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
	}
	return defs
}

// ExecuteTool runs a tool by name with the given JSON arguments.
func ExecuteTool(ctx context.Context, tools []Tool, name string, argsJSON string) (string, error) {
	for _, t := range tools {
		if t.Name == name {
			var args map[string]interface{}
			if argsJSON == "" {
				argsJSON = "{}"
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return "", fmt.Errorf("parse tool args: %w", err)
			}
			result, err := t.Execute(ctx, args)
			if err != nil {
				return "", err
			}
			return result, nil
		}
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

func init() {
	// Ensure time is available
	_ = time.Now()
}
