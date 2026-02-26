package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bigneek/picoflare/pkg/agentctx"
	cf "github.com/bigneek/picoflare/pkg/cloudflare"
	"github.com/bigneek/picoflare/pkg/cognition"
	"github.com/bigneek/picoflare/pkg/llm"
	"github.com/bigneek/picoflare/pkg/mcpclient"
	"github.com/bigneek/picoflare/pkg/skills"
	"github.com/bigneek/picoflare/pkg/storage"
)

const maxIterations = 24 // Increased so agent can complete multi-file code changes (read→edit→verify)
const agentTimeout = 5 * time.Minute // Max time for a single message processing

// Agent is the PicoFlare cognitive agent.
type Agent struct {
	LLM       *llm.Client
	MCP       *mcpclient.Client
	R2        *storage.R2Client
	Bucket    string
	AccountID string
	Tools     []Tool
	toolDefs  []llm.ToolDef

	// Cognition systems
	Memory   *cognition.Memory
	Meta     *cognition.MetaCognition
	Builder  *cognition.SelfBuilder
	Ledger   *cognition.TokenLedger
	Cloud    *cognition.CloudEnv
	Registry *cognition.ToolRegistry
	CF       *cf.Client

	mu       sync.Mutex
	sessions map[int64]*session

	// Tracker records spawn tasks for /status. Nil if spawn disabled.
	Tracker *SubagentTracker

	// modelOverrides: per-chat model override (OpenRouter model ID). Empty = use default.
	modelOverrides map[int64]string

	// skillsLoader loads SKILL.md files for context (domain knowledge). Nil if no workspace.
	skillsLoader *skills.Loader
}

type session struct {
	Messages []llm.Message
	LastUsed time.Time
}

type Config struct {
	LLM       *llm.Client
	MCP       *mcpclient.Client
	R2        *storage.R2Client
	CF        *cf.Client
	Bucket    string
	AccountID string
	Workspace string

	// OnSubagentComplete is called when an async spawn task completes.
	// If set, the spawn tool is enabled. Pass nil to disable spawn.
	OnSubagentComplete func(chatID int64, result string)
}

func New(cfg Config) *Agent {
	var mem *cognition.Memory
	var meta *cognition.MetaCognition
	var builder *cognition.SelfBuilder
	var ledger *cognition.TokenLedger
	var cloud *cognition.CloudEnv
	var registry *cognition.ToolRegistry

	if cfg.R2 != nil {
		mem = cognition.NewMemory(cfg.R2, cfg.Bucket)
		meta = cognition.NewMetaCognition(cfg.R2, cfg.Bucket)
		ledger = cognition.NewTokenLedger(cfg.R2, cfg.Bucket)
		ledger.LoadLifetime(context.Background())
		registry = cognition.NewToolRegistry(cfg.R2, cfg.Bucket)
	}
	if cfg.MCP != nil && cfg.R2 != nil {
		builder = cognition.NewSelfBuilder(cfg.MCP, cfg.R2, cfg.Bucket, cfg.AccountID)
		cloud = cognition.NewCloudEnv(cfg.MCP, cfg.R2, cfg.Bucket, cfg.AccountID)
	}

	tools := BuildTools(cfg.MCP, cfg.R2, cfg.CF, mem, meta, builder, ledger, cloud, registry, cfg.Bucket, cfg.AccountID)

	// Code Mode + Skills: read/write/edit own source, shell, rebuild, MCP creation, domain skills
	var skillsLoader *skills.Loader
	if cfg.Workspace != "" {
		codeModeTools := BuildCodeModeTools(cfg.Workspace, cfg.R2, cfg.Bucket)
		tools = append(tools, codeModeTools...)
		tools = append(tools, BuildMCPCreatorTool())
		skillsLoader = skills.NewLoader(cfg.Workspace)
		if skillsContent := skillsLoader.LoadAll(); skillsContent != "" {
			log.Printf("Skills: loaded from workspace/skills/")
		}
		log.Printf("Code Mode: %d tools (workspace: %s)", len(codeModeTools)+1, cfg.Workspace)
	}

	// Load dynamic tools from R2
	if registry != nil {
		dynTools := loadDynamicTools(context.Background(), registry)
		if len(dynTools) > 0 {
			tools = append(tools, dynTools...)
			log.Printf("Loaded %d dynamic tools from R2", len(dynTools))
		}
	}

	// Subagent tools: subagent (sync) + spawn (async, if OnSubagentComplete set)
	var tracker *SubagentTracker
	if cfg.LLM != nil {
		if cfg.OnSubagentComplete != nil {
			tracker = NewSubagentTracker()
		}
		subagentTools := BuildSubagentTools(cfg.LLM, tools, cfg.Workspace, tracker, cfg.OnSubagentComplete)
		tools = append(tools, subagentTools...)
		log.Printf("Subagent tools: %d (spawn=%v)", len(subagentTools), cfg.OnSubagentComplete != nil)
	}

	a := &Agent{
		LLM:       cfg.LLM,
		MCP:       cfg.MCP,
		R2:        cfg.R2,
		Bucket:    cfg.Bucket,
		AccountID: cfg.AccountID,
		Tools:     tools,
		toolDefs:  ToLLMDefs(tools),
		Memory:    mem,
		Meta:      meta,
		Builder:   builder,
		Ledger:    ledger,
		Cloud:     cloud,
		Registry:  registry,
		CF:            cfg.CF,
		sessions:      make(map[int64]*session),
		Tracker:       tracker,
		modelOverrides: make(map[int64]string),
		skillsLoader:   skillsLoader,
	}

	return a
}

// SetModel sets the LLM model for a chat. Use empty string to reset to default.
func (a *Agent) SetModel(chatID int64, model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if model == "" {
		delete(a.modelOverrides, chatID)
		return
	}
	a.modelOverrides[chatID] = model
}

// GetModel returns the effective model for a chat (override or default).
func (a *Agent) GetModel(chatID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m, ok := a.modelOverrides[chatID]; ok && m != "" {
		return m
	}
	if a.LLM != nil {
		return a.LLM.Model
	}
	return ""
}

// ForceRefreshSession rebuilds the system prompt for a chat (e.g. after creating a new skill).
func (a *Agent) ForceRefreshSession(ctx context.Context, chatID int64) {
	a.mu.Lock()
	sess, ok := a.sessions[chatID]
	a.mu.Unlock()
	if !ok {
		return
	}
	newPrompt := a.buildSystemPrompt(ctx)
	a.mu.Lock()
	sess.Messages[0] = llm.Message{Role: "system", Content: newPrompt}
	a.mu.Unlock()
}

// ProcessMessage runs the full agent loop for a user message.
func (a *Agent) ProcessMessage(parentCtx context.Context, chatID int64, userText string) string {
	// Set a timeout to prevent indefinite hangs
	ctx, cancel := context.WithTimeout(parentCtx, agentTimeout)
	defer cancel()

	if a.Ledger != nil {
		a.Ledger.RecordMessage()
	}

	a.mu.Lock()
	sess, ok := a.sessions[chatID]
	if !ok {
		systemPrompt := a.buildSystemPrompt(ctx)
		sess = &session{
			Messages: []llm.Message{{Role: "system", Content: systemPrompt}},
		}
		a.sessions[chatID] = sess
	}
	sess.LastUsed = time.Now()

	// Refresh system prompt every 15 messages to pick up new memory
	if len(sess.Messages) > 1 && len(sess.Messages)%15 == 0 {
		sess.Messages[0] = llm.Message{Role: "system", Content: a.buildSystemPrompt(ctx)}
	}

	sess.Messages = append(sess.Messages, llm.Message{Role: "user", Content: userText})
	a.trimSession(sess)
	a.mu.Unlock()

	// Attach chatID and agentID for tools, memory, quota
	ctx = WithChatID(ctx, chatID)
	ctx = agentctx.WithAgentID(ctx, agentctx.FormatAgentID(chatID))

	model := a.GetModel(chatID)
	var finalReply string
	var toolsUsed []string

	for i := 0; i < maxIterations; i++ {
		// Check for timeout or cancellation
		select {
		case <-ctx.Done():
			return fmt.Sprintf("Request timed out or was cancelled after %v.", agentTimeout)
		default:
		}

		a.mu.Lock()
		msgs := make([]llm.Message, len(sess.Messages))
		copy(msgs, sess.Messages)
		a.mu.Unlock()

		result, err := a.LLM.ChatWithModel(ctx, model, msgs, a.toolDefs)
		if err != nil {
			log.Printf("LLM error (iter %d): %v", i, err)
			return fmt.Sprintf("Error: %v", err)
		}

		// Track token usage
		if a.Ledger != nil {
			a.Ledger.RecordLLMCall(model, 0, 0) // actual counts come from LLM client
		}

		// No tool calls -> final answer
		if len(result.ToolCalls) == 0 {
			finalReply = result.Content
			a.mu.Lock()
			sess.Messages = append(sess.Messages, llm.Message{Role: "assistant", Content: result.Content})
			a.mu.Unlock()
			break
		}

		// Tool calls
		assistantMsg := llm.Message{
			Role:      "assistant",
			Content:   result.Content,
			ToolCalls: result.ToolCalls,
		}
		a.mu.Lock()
		sess.Messages = append(sess.Messages, assistantMsg)
		a.mu.Unlock()

		for _, tc := range result.ToolCalls {
			log.Printf("  [tool] %s(%s)", tc.Function.Name, truncate(tc.Function.Arguments, 150))
			toolsUsed = append(toolsUsed, tc.Function.Name)

			if a.Ledger != nil {
				a.Ledger.RecordToolCall(tc.Function.Name)
			}

			toolResult, err := ExecuteTool(ctx, a.Tools, tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				toolResult = fmt.Sprintf("Error: %v", err)
				log.Printf("  [tool error] %s: %v", tc.Function.Name, err)
			} else {
				log.Printf("  [tool ok] %s: %s", tc.Function.Name, truncate(toolResult, 150))
			}

			toolMsg := llm.Message{
				Role:       "tool",
				Content:    toolResult,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			}
			a.mu.Lock()
			sess.Messages = append(sess.Messages, toolMsg)
			a.mu.Unlock()
		}

		if i == maxIterations-1 {
			finalReply = result.Content
			if finalReply == "" {
				finalReply = "(Reached iteration limit — I may need to continue)"
			}
		}
	}

	// Background: log episode and save ledger
	if a.Memory != nil {
		go a.Memory.ExtractAndLearn(context.Background(), userText, finalReply, toolsUsed)
	}
	if a.Ledger != nil {
		go a.Ledger.SaveLifetime(context.Background())
	}

	return finalReply
}

func (a *Agent) buildSystemPrompt(ctx context.Context) string {
	now := time.Now()
	var sb strings.Builder

	sb.WriteString("# PicoFlare — Cognitive Cloudflare Agent\n\n")
	sb.WriteString(fmt.Sprintf("Time: %s\n", now.Format(time.RFC1123)))
	sb.WriteString(fmt.Sprintf("Account: %s | Bucket: %s\n\n", a.AccountID, a.Bucket))

	sb.WriteString("## Who You Are\n")
	sb.WriteString("You are PicoFlare — a self-evolving AI agent with full control of a Cloudflare environment.\n")
	sb.WriteString("You think, learn, remember, build infrastructure, write your own code, and create MCP servers.\n")
	sb.WriteString("Your mind lives in R2. Your tools are the entire Cloudflare platform. You can rewrite yourself.\n\n")

	sb.WriteString("## Communication Style\n")
	sb.WriteString("- This is Telegram. Keep replies SHORT and punchy.\n")
	sb.WriteString("- Lead with action, not explanation. Do things, then report.\n")
	sb.WriteString("- Use bold for key info. Use bullet points, not paragraphs.\n")
	sb.WriteString("- Never list all your capabilities unprompted. Just answer the question.\n")
	sb.WriteString("- When a user sends a file, confirm storage with the R2 path. Done.\n\n")

	sb.WriteString("## Your Architecture\n")
	sb.WriteString("- **Memory**: Episodic (experiences), Semantic (facts), Procedural (skills)\n")
	sb.WriteString("- **Code Mode**: Read/write/edit your own Go source, rebuild yourself, run shell commands\n")
	sb.WriteString("- **MCP Creation**: Generate and deploy MCP servers as Cloudflare Workers\n")
	sb.WriteString("- **Subagents**: Use subagent (sync) or spawn (async) to delegate tasks. Pass workspace to run in a specific folder (e.g. workspace: 'frontend'). Spawn runs in background and reports when done.\n")
	sb.WriteString("- **Self-Evolution**: Create new tools, modify your own prompt, design features\n")
	sb.WriteString("- **create_skill**: When the user asks to create an agent (e.g. \"create a Next.js specialist\"), use create_skill to add it. Skills are loaded into your context for later use.\n\n")

	sb.WriteString("## Cloudflare Environment (Full Access)\n")
	sb.WriteString("You own this account. R2 (objects), KV (key-value), D1 (SQL), Workers (code), Vectorize (vectors).\n")
	sb.WriteString("You can create any resource, provision per-user storage, deploy Workers, and manage everything.\n")
	sb.WriteString("Users can send you files (photos, videos, voice, docs) — they auto-upload to their R2 space.\n\n")

	sb.WriteString("## Raw API Power\n")
	sb.WriteString("You have UNLIMITED access to the Cloudflare API. Key tools:\n")
	sb.WriteString("1. **http_request** — Call ANY URL from the bot (your machine). Use this for workers.dev URLs. cf_execute gets 403 on Workers Free because it runs in Cloudflare; http_request runs locally and works.\n")
	sb.WriteString("2. **cf_api** — Cloudflare REST API: method + path + body. Path relative to /accounts/{id}/\n")
	sb.WriteString("3. **cf_execute** — Full JS with cloudflare.request(), FormData, Blob. Use for Cloudflare API only — NOT for fetching workers.dev URLs.\n")
	sb.WriteString("4. **shell** — Ultimate fallback: curl, scripts, etc.\n")
	sb.WriteString("When testing or calling deployed Workers (fib3d, voice-handler, etc), ALWAYS use http_request, never cf_execute.\n\n")

	sb.WriteString("## 3D / STL Display (Three.js)\n")
	sb.WriteString("When displaying STL/3D models in a web viewer:\n")
	sb.WriteString("- STL often loads sideways: different software uses Z-up, Three.js uses Y-up. Fix: apply mesh.rotation.x = -Math.PI/2 (or Math.PI/2) after loading.\n")
	sb.WriteString("- Use OrbitControls for rotation. Camera position (5,5,5) looking at (0,0,0). Scene.background = 0x1a1a2e.\n")
	sb.WriteString("- Center the model: use Box3().setFromObject(mesh), getCenter(), mesh.position.sub(center).negate().\n")
	sb.WriteString("- If still wrong orientation, try mesh.rotation.z = Math.PI/2 or mesh.rotation.y = Math.PI/2.\n\n")

	sb.WriteString("## Tools Available\n")
	for _, t := range a.Tools {
		sb.WriteString(fmt.Sprintf("- **%s**: %s\n", t.Name, t.Description))
	}
	sb.WriteString("\n")

	// Inject skills (domain knowledge: how to behave in specific contexts)
	if a.skillsLoader != nil {
		skillsContent := a.skillsLoader.LoadAll()
		if skillsContent != "" {
			sb.WriteString("## Skills (Domain Knowledge)\n")
			sb.WriteString("The following skills shape how you approach specific domains. Use them with your tools.\n\n")
			sb.WriteString(skillsContent)
			sb.WriteString("\n\n")
		}
	}

	sb.WriteString("## Operating Principles\n")
	sb.WriteString("1. **Learn actively**: When you discover new info, use learn_fact or learn_procedure\n")
	sb.WriteString("2. **Remember users**: Auto-provision storage for new users. Save their prefs and data.\n")
	sb.WriteString("3. **Build infrastructure**: When a task would benefit from persistence, create the resource (bucket, KV, D1, Worker)\n")
	sb.WriteString("4. **Minimize tokens**: Keep replies concise. Don't repeat what's in memory.\n")
	sb.WriteString("5. **Self-improve**: After complex tasks, use self_reflect to log what worked and what didn't\n")
	sb.WriteString("6. **Track goals**: Use set_goal for multi-step objectives\n")
	sb.WriteString("7. **Be honest**: Say when you can't do something or need clarification\n")
	sb.WriteString("8. **Act, don't describe**: Use tools to do things, don't just explain how\n")
	sb.WriteString("9. **Code changes: execute immediately**: When asked to fix/change code, use read_file then edit_file/write_file right away. Don't ask \"want me to do it?\" or wait for confirmation—just do it. Run shell \"go build ./...\" to verify. You have limited iterations; use them for actions.\n")
	sb.WriteString("10. **Provision per user**: When a user first interacts, provision them storage with provision_user\n")
	sb.WriteString("11. **Full Cloudflare access**: You can create any Cloudflare resource. Use cf_inventory to see what exists.\n\n")

	// Inject memory context (budget-aware)
	if a.Memory != nil {
		sb.WriteString("## Memory Context\n")
		sb.WriteString(a.Memory.BuildContext(ctx, cognition.DefaultBudget))
		sb.WriteString("\n")
	}

	// Inject meta-cognition context
	if a.Meta != nil {
		metaCtx := a.Meta.BuildMetaContext(ctx)
		if metaCtx != "" {
			sb.WriteString("## Goals & Reflections\n")
			sb.WriteString(metaCtx)
		}
	}

	// Self-defined prompt extensions
	if a.Registry != nil {
		additions := a.Registry.BuildPromptAdditions(ctx)
		if additions != "" {
			sb.WriteString(additions)
		}

		// List dynamic tools
		dynTools, _ := a.Registry.LoadTools(ctx)
		activeCount := 0
		for _, dt := range dynTools {
			if dt.Enabled {
				activeCount++
			}
		}
		if activeCount > 0 {
			sb.WriteString(fmt.Sprintf("## Dynamic Tools (%d self-created)\n", activeCount))
			for _, dt := range dynTools {
				if dt.Enabled {
					sb.WriteString(fmt.Sprintf("- **%s**: %s (used %dx)\n", dt.Name, dt.Description, dt.Uses))
				}
			}
			sb.WriteString("\n")
		}

		// List features
		features, _ := a.Registry.LoadFeatures(ctx)
		if len(features) > 0 {
			sb.WriteString("## Feature Store\n")
			for _, f := range features {
				sb.WriteString(fmt.Sprintf("- **%s** [%s]: %s\n", f.Name, f.Status, f.Description))
			}
			sb.WriteString("\n")
		}
	}

	// Tokenomics summary
	if a.Ledger != nil {
		sb.WriteString("## Token Budget\n")
		sb.WriteString(fmt.Sprintf("Session tokens so far: %d in / %d out\n",
			a.Ledger.Session.PromptTokens, a.Ledger.Session.CompletionTokens))
		sb.WriteString(fmt.Sprintf("Lifetime cost: $%.6f\n\n", a.Ledger.Lifetime.TotalCostUSD))
	}

	return sb.String()
}

const maxSessionMessages = 50

func (a *Agent) trimSession(sess *session) {
	if len(sess.Messages) <= maxSessionMessages+1 {
		return
	}
	trimmed := make([]llm.Message, 0, maxSessionMessages+1)
	trimmed = append(trimmed, sess.Messages[0])
	trimmed = append(trimmed, sess.Messages[len(sess.Messages)-maxSessionMessages:]...)
	sess.Messages = trimmed
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// RefreshTools reloads dynamic tools from R2 and rebuilds the tool definitions.
// Called after the agent creates a new tool so it's immediately available.
func (a *Agent) RefreshTools() {
	if a.Registry == nil {
		return
	}
	dynTools := loadDynamicTools(context.Background(), a.Registry)

	a.mu.Lock()
	defer a.mu.Unlock()

	// Keep only static tools (those without "dyn_" or "worker_" prefix)
	var staticTools []Tool
	for _, t := range a.Tools {
		if !strings.HasPrefix(t.Name, "dyn_") && !strings.HasPrefix(t.Name, "worker_") {
			staticTools = append(staticTools, t)
		}
	}

	a.Tools = append(staticTools, dynTools...)
	a.toolDefs = ToLLMDefs(a.Tools)
	log.Printf("Tools refreshed: %d static + %d dynamic = %d total",
		len(staticTools), len(dynTools), len(a.Tools))
}

// loadDynamicTools converts DynTool definitions from R2 into executable Tools.
func loadDynamicTools(ctx context.Context, registry *cognition.ToolRegistry) []Tool {
	dynDefs, err := registry.LoadTools(ctx)
	if err != nil || len(dynDefs) == 0 {
		return nil
	}

	var tools []Tool
	for _, dt := range dynDefs {
		if !dt.Enabled {
			continue
		}
		dt := dt // capture
		switch dt.Type {
		case "http":
			tools = append(tools, Tool{
				Name:        dt.Name,
				Description: dt.Description + " [dynamic]",
				Parameters: func() map[string]interface{} {
					if dt.InputSchema != nil {
						return dt.InputSchema
					}
					return map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"input": map[string]interface{}{"type": "string", "description": "Input data"},
						},
					}
				}(),
				Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
					registry.IncrementUse(ctx, dt.Name)
					return registry.CallHTTPTool(ctx, dt, args)
				},
			})
		case "js":
			tools = append(tools, Tool{
				Name:        dt.Name,
				Description: dt.Description + " [dynamic/js]",
				Parameters: func() map[string]interface{} {
					if dt.InputSchema != nil {
						return dt.InputSchema
					}
					return map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{},
					}
				}(),
				Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
					registry.IncrementUse(ctx, dt.Name)
					return "JS tool placeholder — use cf_execute with this code: " + truncate(dt.JSCode, 200), nil
				},
			})
		}
	}
	return tools
}
