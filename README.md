# PicoFlare

Cloudflare-native AI agent with Code Mode MCP, R2 storage, and Vectorize memory.

## What makes PicoFlare different: LLM-defined agent scaling

**Describe an agent, get it.** PicoFlare scales agents differently: instead of hand-coding each specialist or wiring up complex orchestration, you tell the LLM what kind of agent you want and it creates it for you.

- **`/createagent`** – Describe an agent in plain language (e.g. "Next.js specialist for App Router" or "Python data science expert for pandas"). The LLM writes a skill to `workspace/skills/<name>/SKILL.md` and it’s loaded into context for future use.
- **Agent self-creation** – The agent itself can create new agents. Ask it to "create a Rust async specialist" and it uses the `create_skill` tool to add that capability. No code changes, no redeploys.
- **Skills as domain knowledge** – Each skill is markdown with YAML frontmatter, injected into the system prompt. New agents become first-class capabilities the main agent can draw on.

This is the core differentiator: **agent scaling via natural language**, not pipelines or hardcoded roles.

## What it does

- **Cloudflare MCP** – Calls the [Cloudflare MCP](https://github.com/cloudflare/mcp) via Streamable HTTP (JSON-RPC). Two tools: `search` (query the OpenAPI spec) and `execute` (run API calls via Code Mode). ~1k tokens instead of ~244k.
- **R2 Storage** – S3-compatible object storage via Cloudflare R2.
- **Vectorize** – Vector memory for RAG (semantic search over stored knowledge).
- **Telegram** – Bot channel with `/createagent` and voice notes.

## Setup

```bash
cp .env.example .env
# Fill in your Cloudflare credentials and Telegram token
```

| Variable | Source |
|----------|--------|
| `CLOUDFLARE_ACCOUNT_ID` | [Cloudflare Dashboard](https://dash.cloudflare.com) |
| `CLOUDFLARE_API_TOKEN` | [API Tokens](https://dash.cloudflare.com/profile/api-tokens) – needs R2 Edit |
| `R2_ACCESS_KEY_ID` / `R2_SECRET_ACCESS_KEY` | R2 API Token from dashboard |
| `TELEGRAM_BOT_TOKEN` | [@BotFather](https://t.me/BotFather) |

## Build & Run

```bash
go build -o picoflare .
./picoflare              # default: run pico-flare agent (interactive)
./picoflare agent        # run pico-flare agent (interactive)
./picoflare bot          # Telegram bot (TELEGRAM_BOT_TOKEN required)
./picoflare mcp-test     # create R2 bucket + Vectorize index via MCP
./picoflare help         # show usage
```

When the MCP server is unavailable, **pico-flare agent** falls back to the Cloudflare REST API so you still get Workers, R2, KV, D1, and Vectorize tools.

### Webhook mode (Cloudflare deployment)

Set `TELEGRAM_WEBHOOK_URL` to run behind Cloudflare Tunnel or a reverse proxy:

```bash
TELEGRAM_WEBHOOK_URL=https://picoflare.example.com/bot ./picoflare bot
```

See [DEPLOY_CLOUDFLARE.md](DEPLOY_CLOUDFLARE.md) for full instructions.

## Project Structure

```
PicoFlare/
├── main.go                  # Entry point, bot / mcp-test commands
├── pkg/
│   ├── agent/               # Agent loop, Code Mode tools, create_skill
│   ├── bot/                 # Telegram handlers, /createagent
│   ├── mcpclient/client.go  # Cloudflare MCP client (Streamable HTTP)
│   ├── skills/loader.go     # Load SKILL.md from workspace/skills/*
│   ├── storage/r2.go        # R2 object storage (S3-compatible)
│   └── memory/vectorize.go  # Vectorize REST client (RAG memory)
├── skills/
│   └── mcp-builder/         # Built-in skill: create MCP servers on Workers
├── workspace/skills/        # LLM-created agents (via /createagent)
├── AGENT_DESIGN.md          # Architecture + token-aware design
├── .env.example             # Template (no secrets)
└── .gitignore
```

## Design

See [AGENT_DESIGN.md](AGENT_DESIGN.md) for the full architecture: MCP Builder skill, token tracking, and optimization principles. The agent scaling model (LLM-defined skills via `/createagent` and `create_skill`) is documented there as well.

## License

MIT
