package agent

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bigneek/picoflare/pkg/llm"
)

// SubagentTask represents a spawn task (running or completed).
type SubagentTask struct {
	ID       string
	Label    string
	Task     string
	ChatID   int64
	Status   string // "running", "completed", "failed"
	Created  int64
	Finished int64
}

// SubagentTracker records spawn tasks for status queries.
type SubagentTracker struct {
	mu     sync.RWMutex
	tasks  map[string]*SubagentTask
	nextID int
}

// NewSubagentTracker creates a new tracker.
func NewSubagentTracker() *SubagentTracker {
	return &SubagentTracker{tasks: make(map[string]*SubagentTask), nextID: 1}
}

// RecordStart registers a new spawn task. Returns the task ID.
func (t *SubagentTracker) RecordStart(label, task string, chatID int64) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := fmt.Sprintf("subagent-%d", t.nextID)
	t.nextID++
	t.tasks[id] = &SubagentTask{
		ID: id, Label: label, Task: truncateTask(task, 60), ChatID: chatID,
		Status: "running", Created: time.Now().UnixMilli(),
	}
	return id
}

// RecordComplete updates a task's status when done.
func (t *SubagentTracker) RecordComplete(taskID, status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if task, ok := t.tasks[taskID]; ok {
		task.Status = status
		task.Finished = time.Now().UnixMilli()
	}
}

// ListTasks returns all tasks, optionally filtered by chatID (0 = all).
func (t *SubagentTracker) ListTasks(chatID int64) []SubagentTask {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []SubagentTask
	for _, task := range t.tasks {
		if chatID == 0 || task.ChatID == chatID {
			out = append(out, *task)
		}
	}
	return out
}

func truncateTask(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Workspace-scoped tool names (from Code Mode). When subagent uses a different workspace,
// we replace these with versions scoped to the sub-workspace.
var workspaceToolNames = map[string]bool{
	"read_file": true, "write_file": true, "edit_file": true, "list_files": true, "shell": true,
}

// chatIDKey is the context key for the current Telegram chat ID.
type chatIDKey struct{}

// WithChatID attaches chatID to the context for tools that need it (e.g. spawn).
func WithChatID(ctx context.Context, chatID int64) context.Context {
	return context.WithValue(ctx, chatIDKey{}, chatID)
}

// ChatIDFromContext returns the chat ID from context, if set.
func ChatIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(chatIDKey{}).(int64)
	return v, ok
}


const subagentMaxIterations = 20 // Matches main agent—spawned coding tasks need room to finish

// resolveSubWorkspace validates that subPath is within mainWorkspace and returns the absolute path.
func resolveSubWorkspace(mainWorkspace, subPath string) (string, error) {
	if mainWorkspace == "" {
		return "", fmt.Errorf("main workspace not configured")
	}
	mainAbs, err := filepath.Abs(mainWorkspace)
	if err != nil {
		return "", fmt.Errorf("resolve main workspace: %w", err)
	}
	subAbs := filepath.Clean(filepath.Join(mainAbs, subPath))
	rel, err := filepath.Rel(mainAbs, subAbs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("workspace %q is outside main workspace", subPath)
	}
	return subAbs, nil
}

const subagentSyncTimeoutDefault = 3 * time.Minute  // Default timeout for sync subagent calls
const subagentSyncTimeoutMax = 10 * time.Minute     // Maximum allowed timeout

// RunSubagentLoop runs a subagent with the given task. Uses the same LLM and tools as the parent,
// but excludes spawn/subagent to avoid recursion. If workspace is non-empty, the subagent runs in
// that sub-folder (relative to mainWorkspace) with workspace-scoped tools for that path.
// timeout of 0 uses the default; otherwise capped at subagentSyncTimeoutMax.
func RunSubagentLoop(parentCtx context.Context, llmClient *llm.Client, tools []Tool, task, mainWorkspace, workspace string, timeout time.Duration) (string, error) {
	// Apply timeout for sync subagents to prevent indefinite hangs
	if timeout <= 0 {
		timeout = subagentSyncTimeoutDefault
	}
	if timeout > subagentSyncTimeoutMax {
		timeout = subagentSyncTimeoutMax
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	var subTools []Tool

	if workspace != "" && mainWorkspace != "" {
		subWorkspace, err := resolveSubWorkspace(mainWorkspace, workspace)
		if err != nil {
			return "", err
		}
		// Replace workspace-scoped tools with versions for the sub-folder
		for _, t := range tools {
			if t.Name == "spawn" || t.Name == "subagent" {
				continue
			}
			if !workspaceToolNames[t.Name] {
				subTools = append(subTools, t)
			}
		}
		subTools = append(subTools, BuildWorkspaceSubTools(subWorkspace, nil, "")...)
	} else {
		for _, t := range tools {
			if t.Name != "spawn" && t.Name != "subagent" {
				subTools = append(subTools, t)
			}
		}
	}

	toolDefs := ToLLMDefs(subTools)

	systemPrompt := `You are a subagent. Complete the given task independently using your tools.
Provide a clear, concise summary of what you did. Be direct and efficient.`
	if workspace != "" {
		systemPrompt += fmt.Sprintf("\n\nYou are working in the folder: %s (paths are relative to this).", workspace)
	}

	messages := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: task},
	}

	for i := 0; i < subagentMaxIterations; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		result, err := llmClient.Chat(ctx, messages, toolDefs)
		if err != nil {
			return "", fmt.Errorf("subagent LLM error: %w", err)
		}

		if len(result.ToolCalls) == 0 {
			return strings.TrimSpace(result.Content), nil
		}

		assistantMsg := llm.Message{
			Role:      "assistant",
			Content:   result.Content,
			ToolCalls: result.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		for _, tc := range result.ToolCalls {
			toolResult, err := ExecuteTool(ctx, subTools, tc.Function.Name, tc.Function.Arguments)
			if err != nil {
				toolResult = fmt.Sprintf("Error: %v", err)
				log.Printf("  [subagent tool error] %s: %v", tc.Function.Name, err)
			} else {
				log.Printf("  [subagent tool ok] %s", tc.Function.Name)
			}

			toolMsg := llm.Message{
				Role:       "tool",
				Content:    toolResult,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			}
			messages = append(messages, toolMsg)
		}
	}

	return "(Subagent reached iteration limit)", nil
}

// BuildSubagentTools creates the subagent and spawn tools.
// onComplete is called when a spawn task completes (async). Pass nil to disable spawn.
// tracker records spawn tasks for /status. Pass nil to disable tracking.
func BuildSubagentTools(llmClient *llm.Client, tools []Tool, mainWorkspace string, tracker *SubagentTracker, onComplete func(chatID int64, result string)) []Tool {
	var result []Tool

	// subagent: synchronous — runs task in same goroutine, returns result
	result = append(result, Tool{
		Name:        "subagent",
		Description: "Delegate a task to a subagent. Use workspace to run in a specific folder (e.g. 'frontend', 'pkg/agent'). Returns the subagent's result directly.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"task": map[string]interface{}{
					"type":        "string",
					"description": "The task for the subagent to complete",
				},
				"label": map[string]interface{}{
					"type":        "string",
					"description": "Optional short label for the task (for display)",
				},
				"workspace": map[string]interface{}{
					"type":        "string",
					"description": "Optional sub-folder to work in (relative to repo root, e.g. 'frontend', 'workers/fib3d'). Paths in read_file/write_file are relative to this.",
				},
				"timeout": map[string]interface{}{
					"type":        "number",
					"description": "Optional timeout in seconds (default: 180, max: 600). Increase for long tasks like multi-file code analysis.",
				},
			},
			"required": []string{"task"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			task, ok := args["task"].(string)
			if !ok || task == "" {
				return "", fmt.Errorf("task is required")
			}
			label, _ := args["label"].(string)
			workspace, _ := args["workspace"].(string)
			workspace = strings.TrimSpace(workspace)

			var timeout time.Duration
			if t, ok := args["timeout"].(float64); ok && t > 0 {
				timeout = time.Duration(t) * time.Second
			}

			res, err := RunSubagentLoop(ctx, llmClient, tools, task, mainWorkspace, workspace, timeout)
			if err != nil {
				return "", err
			}
			if label != "" {
				return fmt.Sprintf("Subagent '%s' completed:\n%s", label, res), nil
			}
			return fmt.Sprintf("Subagent completed:\n%s", res), nil
		},
	})

	// spawn: asynchronous — runs in background, reports result via onComplete
	if onComplete != nil {
		result = append(result, Tool{
			Name:        "spawn",
			Description: "Spawn a subagent to handle a task in the background. Use workspace to run in a specific folder. The subagent reports back when done. You can continue with other work while it runs.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task": map[string]interface{}{
						"type":        "string",
						"description": "The task for the subagent to complete",
					},
					"label": map[string]interface{}{
						"type":        "string",
						"description": "Optional short label for the task (for display)",
					},
					"workspace": map[string]interface{}{
						"type":        "string",
						"description": "Optional sub-folder to work in (e.g. 'frontend', 'workers/fib3d'). Paths are relative to this.",
					},
					"timeout": map[string]interface{}{
						"type":        "number",
						"description": "Optional timeout in seconds (default: 300, max: 600). Increase for long background tasks.",
					},
				},
				"required": []string{"task"},
			},
			Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
				task, ok := args["task"].(string)
				if !ok || task == "" {
					return "", fmt.Errorf("task is required")
				}
				label, _ := args["label"].(string)
				workspace, _ := args["workspace"].(string)
				workspace = strings.TrimSpace(workspace)

				var timeout time.Duration = 5 * time.Minute // default for spawn
				if t, ok := args["timeout"].(float64); ok && t > 0 {
					timeout = time.Duration(t) * time.Second
					if timeout > subagentSyncTimeoutMax {
						timeout = subagentSyncTimeoutMax
					}
				}

				chatID, ok := ChatIDFromContext(ctx)
				if !ok {
					return "", fmt.Errorf("spawn requires chat context (use subagent for sync delegation)")
				}

				// Track for /status
				var taskID string
				if tracker != nil {
					taskID = tracker.RecordStart(label, task, chatID)
				}

				// Capture for goroutine
				cb := onComplete
				cid := chatID
				taskCopy := task
				labelCopy := label
				workspaceCopy := workspace
				timeoutCopy := timeout

				go func() {
					bgCtx, cancel := context.WithTimeout(context.Background(), timeoutCopy)
					defer cancel()

					res, err := RunSubagentLoop(bgCtx, llmClient, tools, taskCopy, mainWorkspace, workspaceCopy, 0) // 0 = use default for nested calls
					status := "completed"
					if err != nil {
						res = fmt.Sprintf("Error: %v", err)
						status = "failed"
					}
					if tracker != nil && taskID != "" {
						tracker.RecordComplete(taskID, status)
					}
					if labelCopy != "" {
						res = fmt.Sprintf("**%s**\n\n%s", labelCopy, res)
					}
					cb(cid, "📋 Subagent completed:\n\n"+res)
				}()

				if label != "" {
					return fmt.Sprintf("Spawned subagent '%s' for task. Will report when done.", label), nil
				}
				return "Spawned subagent. Will report when done.", nil
			},
		})
	}

	return result
}
