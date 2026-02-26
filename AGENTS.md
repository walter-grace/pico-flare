# PicoFlare Agents: Features & How to Harness Them

This guide explains how PicoFlare's agent system works and how to get the most out of it.

---

## Architecture Overview

```
Telegram → Bot → Agent (LLM + Tools) → Cloudflare / R2 / MCP
                    │
                    ├── Subagents (spawn) → parallel workers
                    ├── Code Mode → read/write/shell in workspace
                    └── Tools → cf_api, http_request, memory, etc.
```

- **Agent**: One LLM-backed loop per chat. Sessions persist conversation history.
- **Subagents**: Child agents that run tasks in parallel. Use `spawn` (async) or `subagent` (sync).
- **Tools**: 49+ tools (Cloudflare API, R2, memory, shell, MCP, etc.).

---

## Running the agent in the terminal (pico-flare agent)

You can run the same agent interactively in the terminal:

```bash
./picoflare              # or: ./picoflare agent
```

This starts **pico-flare agent** in interactive mode. You get a `>` prompt; type a message and the agent replies using the same tools (Cloudflare MCP, R2, Code Mode, etc.). Set `OPENROUTER_API_KEY` in `.env`; optionally `CLOUDFLARE_ACCOUNT_ID` and `CLOUDFLARE_API_TOKEN` for Cloudflare features.

**MCP fallback:** If the Cloudflare MCP server is unavailable at startup, the agent does not exit. It falls back to the Cloudflare REST API so you still get Workers, R2, KV, D1, and Vectorize tools. You’ll see a log line like: `MCP unavailable (...), using Cloudflare REST API for Cloudflare operations`.

---

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/start` | Welcome + Spawn 2 / Custom Subagents buttons |
| `/spawn` | Quick: spawn 2 subagents (workers/ + pkg/agent/) |
| `/custom` | Enter custom tasks (one per line), then `/go` |
| `/go` | Spawn collected custom tasks |
| `/cancel` | Cancel custom spawn |
| `/status` | Show running/completed subagent tasks |
| `/model` | Show or set LLM model for this chat |
| `/reboot` | Restart the bot (graceful shutdown; requires systemd/supervisor) |

---

## /model: Switching LLMs Per Chat

Each chat can use a different OpenRouter model. Useful for:
- **Heavy reasoning** → `anthropic/claude-3-opus` or `anthropic/claude-3-5-sonnet`
- **Fast replies** → `moonshotai/kimi-k2.5` (default) or `openai/gpt-4o-mini`
- **Coding** → `anthropic/claude-3-5-sonnet` or `openai/gpt-4o`

### Usage

```
/model                    → show current model
/model anthropic/claude-3-5-sonnet   → set model
/model default            → reset to OPENROUTER_MODEL from .env
```

### Popular OpenRouter Model IDs

| Model | Use case |
|-------|----------|
| `moonshotai/kimi-k2.5` | Default, good balance |
| `anthropic/claude-3-5-sonnet` | Strong coding, agents |
| `anthropic/claude-3-opus` | Max capability |
| `openai/gpt-4o` | Fast, capable |
| `openai/gpt-4o-mini` | Cheap, quick |

Browse all: [openrouter.ai/models](https://openrouter.ai/models)

---

## Subagents: Parallel Task Execution

### How It Works

1. **Main agent** receives your prompt (e.g. "Spawn two subagents: A lists workers/, B lists pkg/agent/").
2. **LLM** decides to use the `spawn` tool for each task.
3. **Spawn** starts a goroutine that runs `RunSubagentLoop` — a full agent loop with the same tools.
4. **Subagent** runs in a sub-workspace (optional) and reports back via `OnSubagentComplete`.
5. **You** get separate Telegram messages when each completes.

### Harnessing Subagents

- **Parallel work**: Use `/spawn` or ask "Spawn 3 subagents to do X, Y, Z in parallel."
- **Different workspaces**: Pass `workspace: "frontend"` so the subagent runs in that folder.
- **Custom tasks**: `/custom` → enter tasks (one per line) → `/go`.
- **Status**: `/status` shows running and completed tasks.

### Sync vs Async

- **`subagent`** (sync): Blocks until done. Use when you need the result in the same turn.
- **`spawn`** (async): Returns immediately; result arrives later via Telegram. Use for parallel work.

---

## Tools + Skills

- **Tools** — Executable capabilities (read_file, spawn, create_tool, etc.). The agent calls them.
- **Skills** — Domain knowledge from `workspace/skills/*/SKILL.md`. Injected into context to shape how the agent behaves. Add `skills/nextjs-specialist/SKILL.md` for Next.js expertise, etc.

Both work together: skills tell the agent *how* to think; tools let it *do* things.

---

## Code Mode: Self-Modification

When `Workspace` is set (PicoFlare repo root), the agent has:

- `read_file`, `write_file`, `edit_file`, `list_files` — full access to its own source
- `shell` — run commands (with safety checks)
- `create_mcp_worker` — scaffold and deploy MCP servers

The agent can **rewrite itself**, add tools, and deploy Workers. Subagents inherit Code Mode and can run in sub-folders via `workspace`.

---

## Memory & Cognition

- **Memory**: Episodic (experiences), semantic (facts), procedural (skills). Stored in R2 + Vectorize.
- **Meta**: Goals, reflections, self-improvement notes.
- **Ledger**: Token usage and cost tracking per model.

---

## Best Practices

1. **Use /model for the task**: Heavy coding → Claude. Quick Q&A → Kimi or GPT-4o-mini.
2. **Spawn for parallelism**: Don't ask the main agent to do 5 things sequentially — spawn 5 subagents.
3. **Custom tasks for batch work**: `/custom` + one task per line + `/go`.
4. **Check /status** when waiting on spawns to see what's running.
