// Package bot provides a Telegram bot that exposes PicoFlare's Cloudflare
// capabilities: MCP search/execute, R2 storage, and Vectorize memory.
package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/bigneek/picoflare/pkg/mcpclient"
	"github.com/bigneek/picoflare/pkg/memory"
	"github.com/bigneek/picoflare/pkg/storage"
)

// Bot wraps the Telegram bot and PicoFlare services.
type Bot struct {
	tg        *telego.Bot
	mcp       *mcpclient.Client
	r2        *storage.R2Client
	vectorize *memory.Client
	accountID string
	bucket    string
}

// Config holds everything needed to start the bot.
type Config struct {
	TelegramToken  string
	AccountID      string
	APIToken       string
	R2AccessKey    string
	R2SecretKey    string
	R2Bucket       string
	VectorizeIndex string
}

// New creates a new Bot from the given config.
func New(cfg Config) (*Bot, error) {
	tg, err := telego.NewBot(cfg.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("telegram bot init: %w", err)
	}

	b := &Bot{
		tg:        tg,
		accountID: cfg.AccountID,
		bucket:    cfg.R2Bucket,
	}

	if cfg.APIToken != "" && cfg.AccountID != "" {
		b.mcp = mcpclient.NewClient("https://mcp.cloudflare.com/mcp", cfg.APIToken, cfg.AccountID)
		b.vectorize = memory.NewClient(cfg.AccountID, cfg.APIToken)
	}

	if cfg.AccountID != "" && cfg.R2AccessKey != "" && cfg.R2SecretKey != "" {
		r2, err := storage.NewR2Client(cfg.AccountID, cfg.R2AccessKey, cfg.R2SecretKey)
		if err != nil {
			log.Printf("R2 client init failed (non-fatal): %v", err)
		} else {
			b.r2 = r2
		}
	}

	return b, nil
}

// Run starts the bot with long-polling and blocks until interrupted.
func (b *Bot) Run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	me, err := b.tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe failed: %w", err)
	}
	log.Printf("PicoFlare bot online: @%s (id: %d)", me.Username, me.ID)

	updates, err := b.tg.UpdatesViaLongPolling(ctx, nil)
	if err != nil {
		return fmt.Errorf("long polling: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down bot...")
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message != nil {
				go b.handleMessage(ctx, update.Message)
			}
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *telego.Message) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	log.Printf("[%s] %s", msg.From.Username, text)

	// Send typing indicator
	_ = b.tg.SendChatAction(ctx, tu.ChatAction(msg.Chat.ChatID(), telego.ChatActionTyping))

	var reply string

	switch {
	case text == "/start":
		reply = "PicoFlare bot ready.\n\n" +
			"Commands:\n" +
			"/status - Show connected services\n" +
			"/search <query> - Search Cloudflare API spec\n" +
			"/execute <code> - Execute Cloudflare API call\n" +
			"/r2put <key> <data> - Upload to R2\n" +
			"/r2get <key> - Download from R2\n" +
			"/remember <text> - Store in Vectorize memory\n" +
			"/recall <text> - Search Vectorize memory\n\n" +
			"Or just send a message and I'll search the Cloudflare API for you."

	case text == "/status":
		reply = b.statusReport()

	case strings.HasPrefix(text, "/search "):
		query := strings.TrimPrefix(text, "/search ")
		reply = b.handleSearch(ctx, query)

	case strings.HasPrefix(text, "/execute "):
		code := strings.TrimPrefix(text, "/execute ")
		reply = b.handleExecute(ctx, code)

	case strings.HasPrefix(text, "/r2put "):
		reply = b.handleR2Put(ctx, strings.TrimPrefix(text, "/r2put "))

	case strings.HasPrefix(text, "/r2get "):
		key := strings.TrimPrefix(text, "/r2get ")
		reply = b.handleR2Get(ctx, strings.TrimSpace(key))

	case strings.HasPrefix(text, "/remember "):
		content := strings.TrimPrefix(text, "/remember ")
		reply = b.handleRemember(ctx, content)

	case strings.HasPrefix(text, "/recall "):
		query := strings.TrimPrefix(text, "/recall ")
		reply = b.handleRecall(ctx, query)

	default:
		reply = b.handleSearch(ctx, text)
	}

	if len(reply) > 4096 {
		reply = reply[:4090] + "\n..."
	}

	_, err := b.tg.SendMessage(ctx, tu.Message(msg.Chat.ChatID(), reply))
	if err != nil {
		log.Printf("Send failed: %v", err)
	}
}

func (b *Bot) statusReport() string {
	var lines []string
	lines = append(lines, "PicoFlare Status:")
	lines = append(lines, fmt.Sprintf("  Account: %s", b.accountID))

	if b.mcp != nil {
		lines = append(lines, "  MCP: connected")
	} else {
		lines = append(lines, "  MCP: not configured")
	}

	if b.r2 != nil {
		lines = append(lines, fmt.Sprintf("  R2: connected (bucket: %s)", b.bucket))
	} else {
		lines = append(lines, "  R2: not configured")
	}

	if b.vectorize != nil {
		lines = append(lines, "  Vectorize: connected")
	} else {
		lines = append(lines, "  Vectorize: not configured")
	}

	return strings.Join(lines, "\n")
}

func (b *Bot) handleSearch(ctx context.Context, query string) string {
	if b.mcp == nil {
		return "MCP not configured. Set CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID."
	}

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
		return results.slice(0, 10);
	}`, query)

	out, err := b.mcp.Search(ctx, code)
	if err != nil {
		return fmt.Sprintf("Search error: %v", err)
	}
	return fmt.Sprintf("Search results for %q:\n\n%v", query, out)
}

func (b *Bot) handleExecute(ctx context.Context, code string) string {
	if b.mcp == nil {
		return "MCP not configured."
	}

	wrappedCode := code
	if !strings.Contains(code, "async") {
		wrappedCode = fmt.Sprintf(`async () => {
			const response = await cloudflare.request(%s);
			return response;
		}`, code)
	}

	out, err := b.mcp.Execute(ctx, wrappedCode, b.accountID)
	if err != nil {
		return fmt.Sprintf("Execute error: %v", err)
	}
	return fmt.Sprintf("%v", out)
}

func (b *Bot) handleR2Put(ctx context.Context, args string) string {
	if b.r2 == nil {
		return "R2 not configured."
	}
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "Usage: /r2put <key> <data>"
	}
	key := strings.TrimSpace(parts[0])
	data := parts[1]

	if err := b.r2.UploadObject(ctx, b.bucket, key, []byte(data)); err != nil {
		return fmt.Sprintf("R2 upload failed: %v", err)
	}
	return fmt.Sprintf("Uploaded to R2: %s/%s (%d bytes)", b.bucket, key, len(data))
}

func (b *Bot) handleR2Get(ctx context.Context, key string) string {
	if b.r2 == nil {
		return "R2 not configured."
	}
	data, err := b.r2.DownloadObject(ctx, b.bucket, key)
	if err != nil {
		return fmt.Sprintf("R2 download failed: %v", err)
	}
	if len(data) > 3000 {
		return fmt.Sprintf("R2 %s/%s (%d bytes):\n\n%s\n...(truncated)", b.bucket, key, len(data), string(data[:3000]))
	}
	return fmt.Sprintf("R2 %s/%s:\n\n%s", b.bucket, key, string(data))
}

func (b *Bot) handleRemember(ctx context.Context, content string) string {
	if b.vectorize == nil {
		return "Vectorize not configured."
	}
	// Simple hash-based ID from content + timestamp
	id := fmt.Sprintf("mem-%d", time.Now().UnixNano())
	// Use a simple fixed-length vector as placeholder until embedding model is wired
	vector := simpleHash(content, 768)

	err := b.vectorize.InsertVector(ctx, "picoflare-memory", id, vector, map[string]string{
		"content":   content,
		"timestamp": time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Sprintf("Memory store failed: %v", err)
	}

	// Also persist to R2 for full-text retrieval
	if b.r2 != nil {
		key := fmt.Sprintf("memory/%s.txt", id)
		_ = b.r2.UploadObject(ctx, b.bucket, key, []byte(content))
	}

	return fmt.Sprintf("Remembered (id: %s): %s", id, truncate(content, 100))
}

func (b *Bot) handleRecall(ctx context.Context, query string) string {
	if b.vectorize == nil {
		return "Vectorize not configured."
	}

	vector := simpleHash(query, 768)
	matches, err := b.vectorize.QueryVector(ctx, "picoflare-memory", vector, 5)
	if err != nil {
		return fmt.Sprintf("Memory recall failed: %v", err)
	}
	if len(matches) == 0 {
		return "No memories found."
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Top %d memories:", len(matches)))
	for i, m := range matches {
		lines = append(lines, fmt.Sprintf("  %d. %s (score: %.4f)", i+1, m.ID, m.Score))
	}
	return strings.Join(lines, "\n")
}

// simpleHash creates a deterministic float64 vector from text.
// Placeholder until a real embedding model (e.g. Workers AI) is wired in.
func simpleHash(text string, dims int) []float64 {
	v := make([]float64, dims)
	for i, c := range text {
		v[i%dims] += float64(c) / 1000.0
	}
	// Normalize
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	if sum > 0 {
		norm := 1.0 / sqrt(sum)
		for i := range v {
			v[i] *= norm
		}
	}
	return v
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 20; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
