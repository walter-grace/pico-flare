# PicoFlare

Cloudflare-native AI agent with Code Mode MCP, R2 storage, and Vectorize memory.

## What it does

- **Cloudflare MCP** – Calls the [Cloudflare MCP](https://github.com/cloudflare/mcp) via Streamable HTTP (JSON-RPC). Two tools: `search` (query the OpenAPI spec) and `execute` (run API calls via Code Mode). ~1k tokens instead of ~244k.
- **R2 Storage** – S3-compatible object storage via Cloudflare R2.
- **Vectorize** – Vector memory for RAG (semantic search over stored knowledge).
- **Telegram** – Bot channel (coming soon).

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
./picoflare           # default: test MCP, R2, Vectorize
./picoflare mcp-test  # create R2 bucket + Vectorize index via MCP
```

## Project Structure

```
PicoFlare/
├── main.go                  # Entry point, mcp-test command
├── pkg/
│   ├── mcpclient/client.go  # Cloudflare MCP client (Streamable HTTP)
│   ├── storage/r2.go        # R2 object storage (S3-compatible)
│   └── memory/vectorize.go  # Vectorize REST client (RAG memory)
├── skills/
│   └── mcp-builder/         # Skill: create MCP servers on Workers
├── AGENT_DESIGN.md          # Architecture + token-aware design
├── .env.example             # Template (no secrets)
└── .gitignore
```

## Design

See [AGENT_DESIGN.md](AGENT_DESIGN.md) for the full architecture: MCP Builder skill, token tracking, and optimization principles.

## License

MIT
