# Deploy PicoFlare with Cloudflare

Run PicoFlare entirely on Cloudflare infrastructure: MCP, R2, Vectorize, and the Telegram bot via webhook + Cloudflare Tunnel.

## Architecture

```
Telegram → Cloudflare edge (Tunnel) → Go app (webhook) → MCP / R2 / Vectorize
```

- **MCP, R2, Vectorize** – Already Cloudflare-native
- **Bot** – Uses webhook mode instead of long-polling; exposed via Cloudflare Tunnel

## Prerequisites

- Go 1.25+
- [cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/install-and-setup/installation/) (Cloudflare Tunnel CLI)
- A domain on Cloudflare (or use `trycloudflare.com` for quick testing)

## 1. Build

```bash
go build -o picoflare .
```

## 2. Run with Cloudflare Tunnel (quick test)

Use a temporary tunnel URL for development:

```bash
# Terminal 1: Start the tunnel (gives you a random *.trycloudflare.com URL)
cloudflared tunnel --url http://localhost:8080

# Terminal 2: Run the bot with webhook
TELEGRAM_WEBHOOK_URL=https://YOUR-TUNNEL-URL.trycloudflare.com/bot ./picoflare bot
```

Replace `YOUR-TUNNEL-URL` with the URL printed by cloudflared (e.g. `abc123-xyz.trycloudflare.com`).

## 3. Run with a named tunnel (production)

For a stable URL with your own domain:

```bash
# Create a tunnel (one-time)
cloudflared tunnel create picoflare

# Configure the tunnel to route to your app
cloudflared tunnel route dns picoflare picoflare.yourdomain.com

# Create config file: ~/.cloudflared/config.yml
# ingress:
#   - hostname: picoflare.yourdomain.com
#     service: http://localhost:8080
#   - service: http_status:404

# Run tunnel
cloudflared tunnel run picoflare
```

Then run the bot:

```bash
TELEGRAM_WEBHOOK_URL=https://picoflare.yourdomain.com/bot ./picoflare bot
```

## 4. Environment variables for webhook mode

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `TELEGRAM_WEBHOOK_URL` | Yes (for webhook) | - | Full URL Telegram POSTs to (e.g. `https://picoflare.example.com/bot`) |
| `TELEGRAM_WEBHOOK_PATH` | No | `/bot` | Path component (must match URL path) |
| `TELEGRAM_WEBHOOK_LISTEN` | No | `:8080` | Local address to listen on |

If `TELEGRAM_WEBHOOK_URL` is **not** set, the bot uses long-polling (no tunnel needed).

## 5. Process manager (systemd example)

```ini
[Unit]
Description=PicoFlare Telegram Bot
After=network.target

[Service]
Type=simple
User=youruser
WorkingDirectory=/opt/picoflare
Environment="TELEGRAM_WEBHOOK_URL=https://picoflare.yourdomain.com/bot"
EnvironmentFile=/opt/picoflare/.env
ExecStart=/opt/picoflare/picoflare bot
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Run cloudflared as a separate service (or use `cloudflared service install` for a persistent tunnel).

## 6. Switching back to long-polling

Remove `TELEGRAM_WEBHOOK_URL` from your environment. The bot will automatically use long-polling:

```bash
./picoflare bot   # long-polling (no webhook)
```

## Troubleshooting

- **Webhook not receiving updates** – Ensure the tunnel URL is HTTPS and publicly reachable. Test with `curl -X POST https://your-url/bot` (Telegram sends JSON).
- **"Conflict" from Telegram** – Another webhook may be set. Use `DeleteWebhook` via the Bot API or wait for the previous instance to stop.
- **Tunnel drops** – For production, use a named tunnel with `cloudflared service install` so it restarts automatically.
