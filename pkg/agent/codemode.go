package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bigneek/picoflare/pkg/agentctx"
	"github.com/bigneek/picoflare/pkg/agentfs"
	"github.com/bigneek/picoflare/pkg/storage"
)

var dangerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)rm\s+-rf\s+/`),
	regexp.MustCompile(`(?i)sudo\s+`),
	regexp.MustCompile(`(?i)chmod\s+777`),
	regexp.MustCompile(`(?i)>\s*/dev/`),
	regexp.MustCompile(`(?i)curl.*\|\s*sh`),
	regexp.MustCompile(`(?i)wget.*\|\s*sh`),
	regexp.MustCompile(`(?i)eval\s*\(`),
	regexp.MustCompile(`(?i)git\s+push.*--force`),
	regexp.MustCompile(`(?i)docker\s+run`),
	regexp.MustCompile(`(?i)kill\s+-9\s+1\b`),
}

// BuildCodeModeTools gives the agent full access to its own source code
// and the ability to run shell commands, rebuild itself, and generate MCP servers.
// When r2 and bucket are set, uses per-agent R2 workspace when agentID is in context.
func BuildCodeModeTools(workspace string, r2 *storage.R2Client, bucket string) []Tool {
	var tools []Tool

	tools = append(tools, Tool{
		Name:        "read_file",
		Description: "Read a file from the PicoFlare workspace. Use to inspect your own source code, configs, or any project file.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "File path relative to workspace (e.g. 'pkg/agent/agent.go', 'main.go')"},
			},
			"required": []string{"path"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			path, _ := args["path"].(string)
			if agentID, ok := agentctx.AgentIDFromContext(ctx); ok && r2 != nil && bucket != "" {
				fs := agentfs.New(r2, bucket, agentID)
				data, err := fs.ReadFile(ctx, path)
				if err != nil {
					return "", fmt.Errorf("read %s: %w", path, err)
				}
				content := string(data)
				if len(content) > 12000 {
					content = content[:12000] + fmt.Sprintf("\n...(truncated, %d total bytes)", len(data))
				}
				return content, nil
			}
			absPath, err := resolvePath(path, workspace)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(absPath)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", path, err)
			}
			content := string(data)
			if len(content) > 12000 {
				content = content[:12000] + fmt.Sprintf("\n...(truncated, %d total bytes)", len(data))
			}
			return content, nil
		},
	})

	tools = append(tools, Tool{
		Name:        "write_file",
		Description: "Write or create a file in the PicoFlare workspace. Creates directories if needed. Use to add new Go source files, packages, or configs.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    map[string]interface{}{"type": "string", "description": "File path relative to workspace"},
				"content": map[string]interface{}{"type": "string", "description": "Full file content to write"},
			},
			"required": []string{"path", "content"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			absPath, err := resolvePath(path, workspace)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
				return "", fmt.Errorf("mkdir: %w", err)
			}
			if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
				return "", fmt.Errorf("write %s: %w", path, err)
			}
			return fmt.Sprintf("Written %s (%d bytes)", path, len(content)), nil
		},
	})

	tools = append(tools, Tool{
		Name:        "edit_file",
		Description: "Edit a file by replacing exact text. old_text must appear exactly once. Use for surgical code changes without rewriting the whole file.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":     map[string]interface{}{"type": "string", "description": "File path relative to workspace"},
				"old_text": map[string]interface{}{"type": "string", "description": "Exact text to find (must be unique)"},
				"new_text": map[string]interface{}{"type": "string", "description": "Replacement text"},
			},
			"required": []string{"path", "old_text", "new_text"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			path, _ := args["path"].(string)
			oldText, _ := args["old_text"].(string)
			newText, _ := args["new_text"].(string)
			if agentID, ok := agentctx.AgentIDFromContext(ctx); ok && r2 != nil && bucket != "" {
				fs := agentfs.New(r2, bucket, agentID)
				data, err := fs.ReadFile(ctx, path)
				if err != nil {
					return "", fmt.Errorf("read %s: %w", path, err)
				}
				content := string(data)
				count := strings.Count(content, oldText)
				if count == 0 {
					return "", fmt.Errorf("old_text not found in %s", path)
				}
				if count > 1 {
					return "", fmt.Errorf("old_text appears %d times in %s — must be unique", count, path)
				}
				newContent := strings.Replace(content, oldText, newText, 1)
				if err := fs.WriteFile(ctx, path, []byte(newContent)); err != nil {
					return "", fmt.Errorf("write %s: %w", path, err)
				}
				return fmt.Sprintf("Edited %s: replaced %d chars with %d chars", path, len(oldText), len(newText)), nil
			}
			absPath, err := resolvePath(path, workspace)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(absPath)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", path, err)
			}
			content := string(data)
			count := strings.Count(content, oldText)
			if count == 0 {
				return "", fmt.Errorf("old_text not found in %s", path)
			}
			if count > 1 {
				return "", fmt.Errorf("old_text appears %d times in %s — must be unique", count, path)
			}
			newContent := strings.Replace(content, oldText, newText, 1)
			if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
				return "", fmt.Errorf("write %s: %w", path, err)
			}
			return fmt.Sprintf("Edited %s: replaced %d chars with %d chars", path, len(oldText), len(newText)), nil
		},
	})

	tools = append(tools, Tool{
		Name:        "list_files",
		Description: "List files and directories in the workspace. Use to explore your own project structure.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{"type": "string", "description": "Directory relative to workspace (empty = root)"},
			},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			path, _ := args["path"].(string)
			if path == "" {
				path = "."
			}
			if agentID, ok := agentctx.AgentIDFromContext(ctx); ok && r2 != nil && bucket != "" {
				fs := agentfs.New(r2, bucket, agentID)
				entries, err := fs.ListDir(ctx, path)
				if err != nil {
					return "", fmt.Errorf("list %s: %w", path, err)
				}
				return strings.Join(entries, "\n"), nil
			}
			absPath, err := resolvePath(path, workspace)
			if err != nil {
				return "", err
			}
			entries, err := os.ReadDir(absPath)
			if err != nil {
				return "", fmt.Errorf("list %s: %w", path, err)
			}
			var lines []string
			for _, e := range entries {
				info, _ := e.Info()
				suffix := ""
				if e.IsDir() {
					suffix = "/"
				}
				size := ""
				if info != nil && !e.IsDir() {
					size = fmt.Sprintf(" (%d bytes)", info.Size())
				}
				lines = append(lines, e.Name()+suffix+size)
			}
			return strings.Join(lines, "\n"), nil
		},
	})

	tools = append(tools, Tool{
		Name:        "shell",
		Description: "Run a shell command in the workspace. Use for 'go build', 'go test', 'go vet', 'git' ops, or system inspection. Dangerous commands are blocked.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "Shell command to run"},
				"cwd":     map[string]interface{}{"type": "string", "description": "Working directory relative to workspace (default: root)"},
			},
			"required": []string{"command"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			command, _ := args["command"].(string)
			cwd, _ := args["cwd"].(string)
			if err := guardCommand(command); err != nil {
				return "", err
			}
			workDir := workspace
			if cwd != "" {
				resolved, err := resolvePath(cwd, workspace)
				if err != nil {
					return "", err
				}
				workDir = resolved
			}
			cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
			cmd.Dir = workDir
			output, err := cmd.CombinedOutput()
			result := string(output)
			if len(result) > 10000 {
				result = result[:10000] + fmt.Sprintf("\n...(truncated, %d total)", len(output))
			}
			if err != nil {
				return fmt.Sprintf("Exit error: %v\n\n%s", err, result), nil
			}
			if result == "" {
				result = "(no output)"
			}
			return result, nil
		},
	})

	tools = append(tools, Tool{
		Name:        "self_rebuild",
		Description: "Rebuild PicoFlare from source after code changes. Runs 'go build'. Use after editing Go source files to compile changes.",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			cmdCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cmdCtx, "go", "build", "-o", "picoflare", ".")
			cmd.Dir = workspace
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Sprintf("Build FAILED:\n%s\n%v", string(output), err), nil
			}
			return "Build SUCCESS. Binary updated.\nBot restart needed to load new compiled code.", nil
		},
	})

	// create_skill: Create a new agent/skill for later use. Skills are loaded into context.
	tools = append(tools, Tool{
		Name:        "create_skill",
		Description: "Create a new agent/skill for later use. Use when the user asks to create an agent (e.g. 'Next.js specialist', 'Python DevOps expert'). Writes to workspace/skills/<name>/SKILL.md. The skill will be loaded into your context on next message. Use kebab-case for name (e.g. nextjs-specialist).",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":        map[string]interface{}{"type": "string", "description": "Skill name in kebab-case (e.g. nextjs-specialist)"},
				"description": map[string]interface{}{"type": "string", "description": "Short description for the skill (used in frontmatter)"},
				"content":     map[string]interface{}{"type": "string", "description": "Markdown body: instructions, guidelines, workflows for this agent type. No frontmatter—it's added automatically."},
			},
			"required": []string{"name", "description", "content"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			name, _ := args["name"].(string)
			desc, _ := args["description"].(string)
			content, _ := args["content"].(string)
			name = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
			if name == "" {
				return "", fmt.Errorf("name is required")
			}
			fullContent := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s", name, desc, strings.TrimSpace(content))
			path := filepath.Join("skills", name, "SKILL.md")
			if agentID, ok := agentctx.AgentIDFromContext(ctx); ok && r2 != nil && bucket != "" {
				fs := agentfs.New(r2, bucket, agentID)
				if err := fs.WriteFile(ctx, path, []byte(fullContent)); err != nil {
					return "", fmt.Errorf("write %s: %w", path, err)
				}
				return fmt.Sprintf("Skill %q created at %s. It will be loaded into context on your next message.", name, path), nil
			}
			absPath, err := resolvePath(path, workspace)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
				return "", fmt.Errorf("mkdir: %w", err)
			}
			if err := os.WriteFile(absPath, []byte(fullContent), 0644); err != nil {
				return "", fmt.Errorf("write %s: %w", path, err)
			}
			return fmt.Sprintf("Skill %q created at %s. It will be loaded into context on your next message.", name, path), nil
		},
	})

	return tools
}

// BuildWorkspaceSubTools returns read_file, write_file, edit_file, list_files, shell for a sub-workspace.
// Used when a subagent runs in a specific folder. Excludes self_rebuild and create_skill (main workspace only).
func BuildWorkspaceSubTools(workspace string, r2 *storage.R2Client, bucket string) []Tool {
	all := BuildCodeModeTools(workspace, r2, bucket)
	var out []Tool
	for _, t := range all {
		if t.Name != "self_rebuild" && t.Name != "create_skill" {
			out = append(out, t)
		}
	}
	return out
}

// BuildMCPCreatorTool generates complete MCP server Worker code (JSON-RPC 2.0)
// that the agent can deploy to Cloudflare. Any MCP client can then connect and
// use the tools the server exposes.
func BuildMCPCreatorTool() Tool {
	return Tool{
		Name: "create_mcp_server",
		Description: `Generate a Cloudflare Worker that IS an MCP server (JSON-RPC 2.0).
Exposes tools any MCP client can call. Use this to create APIs, data processors,
webhooks, or any new capability as a live MCP endpoint. Returns JS code — deploy with deploy_worker.`,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":        map[string]interface{}{"type": "string", "description": "MCP server name (becomes Worker name)"},
				"description": map[string]interface{}{"type": "string", "description": "What this MCP server does"},
				"tools": map[string]interface{}{
					"type": "array",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"name":        map[string]interface{}{"type": "string"},
							"description": map[string]interface{}{"type": "string"},
							"handler":     map[string]interface{}{"type": "string", "description": "JavaScript code that sets the `result` variable"},
						},
					},
					"description": "Tools the MCP server exposes",
				},
			},
			"required": []string{"name", "description", "tools"},
		},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			name, _ := args["name"].(string)
			desc, _ := args["description"].(string)
			var specs []mcpToolSpec
			if rawTools, ok := args["tools"].([]interface{}); ok {
				for _, rt := range rawTools {
					if t, ok := rt.(map[string]interface{}); ok {
						s := mcpToolSpec{}
						s.Name, _ = t["name"].(string)
						s.Description, _ = t["description"].(string)
						s.Handler, _ = t["handler"].(string)
						specs = append(specs, s)
					}
				}
			}
			code := generateMCPWorkerCode(name, desc, specs)
			return fmt.Sprintf("MCP Server code for %q (%d tools, %d bytes). Deploy with deploy_worker.\n\n%s",
				name, len(specs), len(code), code), nil
		},
	}
}

type mcpToolSpec struct{ Name, Description, Handler string }

func generateMCPWorkerCode(name, desc string, tools []mcpToolSpec) string {
	var handlers, list strings.Builder
	for i, t := range tools {
		h := t.Handler
		if h == "" {
			h = `result = { message: "Not implemented" };`
		}
		if i > 0 {
			list.WriteString(",\n              ")
			handlers.WriteString("\n          ")
		}
		list.WriteString(fmt.Sprintf(
			`{ name: %q, description: %q, inputSchema: { type: "object", properties: {} } }`,
			t.Name, t.Description))
		handlers.WriteString(fmt.Sprintf("case %q:\n            %s\n            break;", t.Name, h))
	}

	return fmt.Sprintf(`// %s — MCP Server (JSON-RPC 2.0)
// %s — Auto-generated by PicoFlare
export default {
  async fetch(request, env) {
    if (request.method === "OPTIONS")
      return new Response(null, { headers: {
        "Access-Control-Allow-Origin": "*",
        "Access-Control-Allow-Methods": "POST",
        "Access-Control-Allow-Headers": "Content-Type, Authorization"
      }});
    if (request.method !== "POST")
      return Response.json({ error: "POST required" }, { status: 405 });
    const { jsonrpc, id, method, params } = await request.json();
    if (jsonrpc !== "2.0")
      return Response.json({ jsonrpc: "2.0", id, error: { code: -32600, message: "Invalid JSON-RPC" } });
    switch (method) {
      case "initialize":
        return Response.json({ jsonrpc: "2.0", id, result: {
          protocolVersion: "2024-11-05",
          serverInfo: { name: %q, version: "1.0.0" },
          capabilities: { tools: { listChanged: false } }
        }});
      case "notifications/initialized":
        return new Response(null, { status: 202 });
      case "tools/list":
        return Response.json({ jsonrpc: "2.0", id, result: { tools: [
              %s
        ]}});
      case "tools/call": {
        const toolName = params?.name;
        const toolArgs = params?.arguments || {};
        let result;
        switch (toolName) {
          %s
          default:
            return Response.json({ jsonrpc: "2.0", id,
              error: { code: -32601, message: "Unknown tool: " + toolName } });
        }
        return Response.json({ jsonrpc: "2.0", id,
          result: { content: [{ type: "text", text: JSON.stringify(result) }] } });
      }
      default:
        return Response.json({ jsonrpc: "2.0", id,
          error: { code: -32601, message: "Method not found: " + method } });
    }
  }
};
`, name, desc, name, list.String(), handlers.String())
}

func resolvePath(path, workspace string) (string, error) {
	absWS, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath = filepath.Clean(filepath.Join(absWS, path))
	}
	if _, err := os.Lstat(absPath); err == nil {
		if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
			absPath = resolved
		}
	}
	wsReal, _ := filepath.EvalSymlinks(absWS)
	if wsReal == "" {
		wsReal = absWS
	}
	rel, err := filepath.Rel(wsReal, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("access denied: path %q is outside workspace", path)
	}
	return absPath, nil
}

func guardCommand(command string) error {
	for _, p := range dangerPatterns {
		if p.MatchString(strings.ToLower(command)) {
			return fmt.Errorf("command blocked by safety guard")
		}
	}
	return nil
}
