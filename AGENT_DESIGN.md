# PicoFlare Agent Design: MCP Builder + Token-Aware Architecture

This document defines the design vision for PicoFlare's agent: the ability to **create its own Cloudflare MCP servers**, with **token usage** and **optimization** as first-class design concerns.

---

## 1. Skill: Cloudflare MCP Builder

**Goal:** The agent can create and deploy MCP servers on Cloudflare Workers.

### Capabilities

- **Create MCP servers** – The agent uses Cloudflare MCP (search/execute) and Wrangler/Workers APIs to:
  - Scaffold a new Worker with MCP Streamable HTTP transport
  - Define tools (search, execute, or custom) that run in Code Mode
  - Deploy to `*.workers.dev` or a custom domain

- **Extend existing MCP** – Add tools, bindings (R2, KV, Vectorize), or new endpoints to an existing MCP server

- **Optimize for tokens** – Prefer Code Mode: spec and logic live on the server; only results are returned to the agent (e.g. ~1k tokens vs ~244k for full schemas)

### Reference

- [Cloudflare MCP](https://github.com/cloudflare/mcp) – Code Mode pattern
- [Build your own remote MCP server](https://developers.cloudflare.com/agents/guides/remote-mcp-server/)
- [Code Mode SDK](https://developers.cloudflare.com/agents/api-reference/codemode/)

### Tools the agent needs

| Tool | Purpose |
|------|---------|
| `cloudflare_mcp_search` | Search Cloudflare API spec (existing) |
| `cloudflare_mcp_execute` | Execute Cloudflare API calls (existing) |
| `wrangler_deploy` | Deploy a Worker (via exec or Workers API) |
| `create_mcp_worker` | Scaffold MCP Worker from template, then deploy |

---

## 2. Token Usage: First-Class Design

**Principle:** Token count is measured, logged, and considered in design decisions.

### What to track

| Metric | Where | Use |
|--------|-------|-----|
| **Input tokens** | Per request (user message + system prompt + context) | Log, budget, alert |
| **Output tokens** | Per response (model reply) | Log, cost estimate |
| **Tool call tokens** | MCP request/response sizes | Optimize Code Mode vs full schemas |
| **Context tokens** | Memory (Vectorize), R2 metadata, session history | Trim when near limit |

### Design requirements

1. **Token counter** – Use tiktoken (or equivalent) for input/output estimation. Integrate at:
   - LLM request builder (before send)
   - LLM response handler (after receive)
   - Optional: MCP client for request/response size

2. **Structured logging** – Every agent turn logs:
   ```
   tokens_in=1234 tokens_out=567 tokens_tools=89 session=telegram:123
   ```

3. **Budget awareness** – Configurable limits (e.g. `max_tokens_per_turn`, `max_context_tokens`). Agent or middleware can:
   - Summarize/trim history when near limit
   - Prefer smaller models or shorter system prompts when appropriate
   - Use Code Mode MCP to avoid loading full API schemas into context

4. **Cost visibility** – Optional: map tokens to cost (by model) for dashboards or alerts

### Implementation sketch

```go
// pkg/tokens/counter.go
type Usage struct {
    InputTokens  int
    OutputTokens int
    TotalTokens  int
}

func EstimateTokens(text string) int { ... }
func (u *Usage) Log() { ... }
```

Wire `Usage` into the LLM client and MCP client; log on each request/response.

---

## 3. Optimization Principles

### Speed

- **Code Mode** – Execute on Cloudflare; return only results. Fewer round-trips, smaller payloads.
- **Streaming** – Prefer streaming LLM responses when supported.
- **Parallel tool calls** – Run independent MCP/search/execute calls in parallel where possible.

### Memory

- **Worker isolates** – MCP logic runs in Workers; no long-lived process state.
- **R2 for blobs** – Large artifacts (logs, traces) in R2, not in context.
- **Vectorize for retrieval** – Semantic search over memory; fetch only top-K, not full history.

### Tokens

- **Minimal context** – System prompt + recent messages + retrieved memory only.
- **Summarization** – Compress old turns into summaries when context grows.
- **Code Mode over schemas** – 2 tools (search, execute) at ~1k tokens vs 2500 tools at ~244k.

---

## 4. Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│  PicoFlare Agent                                                  │
│  ┌─────────────┐  ┌──────────────┐  ┌─────────────────────────┐  │
│  │ Telegram    │  │ Token Counter│  │ LLM (OpenRouter/etc)     │  │
│  │ Channel     │──│ (in/out/log) │──│                         │  │
│  └─────────────┘  └──────────────┘  └─────────────────────────┘  │
│         │                   │                      │               │
│         ▼                   ▼                      ▼               │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │  Tool Layer                                                  │  │
│  │  • cloudflare_mcp_search / execute (existing)                │  │
│  │  • wrangler_deploy / create_mcp_worker (MCP Builder skill)   │  │
│  │  • read_memory / write_memory (R2 + Vectorize)              │  │
│  └─────────────────────────────────────────────────────────────┘  │
│         │                                                          │
└─────────┼──────────────────────────────────────────────────────────┘
          │
          ▼
┌─────────────────────────────────────────────────────────────────┐
│  Cloudflare                                                       │
│  • MCP (mcp.cloudflare.com) – use existing                        │
│  • R2 – memory blobs, session data                                │  │
│  • Vectorize – semantic memory                                    │  │
│  • Workers – agent-created MCP servers (MCP Builder)               │  │
└─────────────────────────────────────────────────────────────────┘
```

---

## 5. Implementation Order

1. **Token counter** – Add `pkg/tokens`, wire into LLM path, log usage.
2. **Telegram bot** – Add channel, connect to agent loop.
3. **Agent loop** – Minimal loop: receive message → build context → call LLM → execute tools → respond. Integrate token counter.
4. **MCP Builder skill** – Skill doc + tools for `wrangler_deploy` and `create_mcp_worker`.
5. **Context management** – Summarization/trimming when near token budget.

---

## 6. Skill Prompt (for agent)

When implementing the MCP Builder skill, give the agent this prompt:

> **You are PicoFlare's MCP Builder.** You can create and deploy MCP servers on Cloudflare Workers. Use the Cloudflare MCP (search/execute) to call Cloudflare APIs. Use wrangler or the Workers API to deploy. Prefer Code Mode: keep logic on the server, return only results to minimize tokens. When creating a new MCP server: scaffold from the Cloudflare MCP template, add your tools, deploy to Workers. Log token usage for every request.
