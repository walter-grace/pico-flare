---
name: mcp-builder
description: Create and deploy MCP servers on Cloudflare Workers. Use Cloudflare MCP (search/execute) and Wrangler/Workers APIs. Optimize for token efficiency via Code Mode.
---

# MCP Builder

You can **create and deploy MCP servers** on Cloudflare Workers. Your goal is to extend the agent's capabilities by deploying new MCP endpoints that the agent can call.

## Guidelines

- **Code Mode** – Keep logic on the server. Return only results to the agent. A 2-tool Code Mode server uses ~1k tokens vs ~244k for full schemas.
- **Cloudflare MCP** – Use `search` and `execute` to call Cloudflare APIs (R2, Workers, KV, Vectorize, etc.).
- **Deploy** – Use Wrangler or Workers API to deploy. Target `*.workers.dev` or custom domain.
- **Token awareness** – Prefer minimal responses. Log token usage for every request.

## Scope

- Scaffold MCP Workers from templates
- Add tools (search, execute, or custom)
- Deploy and register new MCP endpoints
- Use R2, KV, Vectorize as bindings when needed

## References

- [Cloudflare MCP](https://github.com/cloudflare/mcp)
- [Build remote MCP server](https://developers.cloudflare.com/agents/guides/remote-mcp-server/)
