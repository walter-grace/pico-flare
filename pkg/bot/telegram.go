// Package bot provides a Telegram bot that wraps the PicoFlare agent,
// exposing Cloudflare infrastructure capabilities via chat.
package bot

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/bigneek/picoflare/pkg/agent"
	cf "github.com/bigneek/picoflare/pkg/cloudflare"
	"github.com/bigneek/picoflare/pkg/llm"
	"github.com/bigneek/picoflare/pkg/mcpclient"
	"github.com/bigneek/picoflare/pkg/storage"
	"github.com/bigneek/picoflare/pkg/transcribe"
)

// spawnParallelPrompt is the prompt sent when user taps "Spawn 2 Subagents" or /spawn.
const spawnParallelPrompt = `Spawn two subagents in parallel:
1. Subagent A: List the files in workers/ and tell me what's there (workspace: "workers")
2. Subagent B: List the files in pkg/agent/ and tell me what's there (workspace: "pkg/agent")

Use spawn for both. I'll get two separate messages when each completes.`

// customSpawnState holds tasks the user is collecting for a custom spawn.
type customSpawnState struct {
	Tasks []string
}

// Bot wraps the Telegram bot and the PicoFlare agent.
type Bot struct {
	tg            *telego.Bot
	agent         *agent.Agent
	openRouterKey string // For voice transcription via OpenRouter

	customSpawnMu  sync.Mutex
	customSpawnMap map[int64]*customSpawnState

	runCancel context.CancelFunc // set in Run(); calling it triggers graceful shutdown (for /reboot)
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
	LLMAPIKey      string
	LLMModel       string
	Workspace      string
	OpenAIApiKey   string // For voice note transcription (Whisper)
}

// New creates a new Bot from the given config.
func New(cfg Config) (*Bot, error) {
	tg, err := telego.NewBot(cfg.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("telegram bot init: %w", err)
	}

	var mcp *mcpclient.Client
	if cfg.APIToken != "" && cfg.AccountID != "" {
		mcp = mcpclient.NewClient("https://mcp.cloudflare.com/mcp", cfg.APIToken, cfg.AccountID)
		if err := mcp.Initialize(context.Background()); err != nil {
			log.Printf("MCP unavailable (%v), using Cloudflare REST API when available", err)
			mcp = nil
		}
	}

	var r2 *storage.R2Client
	if cfg.AccountID != "" && cfg.R2AccessKey != "" && cfg.R2SecretKey != "" {
		r2Client, err := storage.NewR2Client(cfg.AccountID, cfg.R2AccessKey, cfg.R2SecretKey)
		if err != nil {
			log.Printf("R2 client init failed (non-fatal): %v", err)
		} else {
			r2 = r2Client
		}
	}

	var llmClient *llm.Client
	if cfg.LLMAPIKey != "" {
		llmClient = llm.NewClient(cfg.LLMAPIKey, cfg.LLMModel)
		log.Printf("LLM: OpenRouter (%s)", llmClient.Model)
	}

	var cfClient *cf.Client
	if cfg.AccountID != "" && cfg.APIToken != "" {
		candidate := cf.NewClient(cfg.AccountID, cfg.APIToken)
		if status, err := candidate.VerifyToken(context.Background()); err == nil {
			cfClient = candidate
			log.Printf("Cloudflare REST API: token %s", status)
			if sub, err := cfClient.GetSubdomain(context.Background()); err == nil {
				log.Printf("Cloudflare Workers subdomain: %s.workers.dev", sub)
			} else {
				log.Printf("Cloudflare Workers subdomain: not registered yet")
			}
		} else {
			log.Printf("Cloudflare REST API: token invalid (%v) — using MCP for Cloudflare operations", err)
			log.Printf("  To enable direct API access (Worker deployment, etc), create a token at:")
			log.Printf("  https://dash.cloudflare.com/profile/api-tokens")
			log.Printf("  Permissions needed: Workers Scripts Edit, Workers KV Edit, Workers R2 Edit, D1 Edit")
		}
	}

	b := &Bot{tg: tg, agent: nil}
	ag := agent.New(agent.Config{
		LLM:       llmClient,
		MCP:       mcp,
		R2:        r2,
		CF:        cfClient,
		Bucket:    cfg.R2Bucket,
		AccountID: cfg.AccountID,
		Workspace: cfg.Workspace,
		OnSubagentComplete: func(chatID int64, result string) {
			b.sendFormattedReply(context.Background(), tu.ID(chatID), result)
		},
	})
	b.agent = ag
	b.openRouterKey = cfg.LLMAPIKey
	b.customSpawnMap = make(map[int64]*customSpawnState)
	if cfg.LLMAPIKey != "" {
		log.Printf("Voice notes: OpenRouter transcription enabled")
	}
	return b, nil
}

// Run starts the bot. If TELEGRAM_WEBHOOK_URL is set, uses webhook mode (for Cloudflare);
// otherwise uses long-polling.
func (b *Bot) Run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	b.runCancel = cancel

	me, err := b.tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe failed: %w", err)
	}
	log.Printf("PicoFlare bot online: @%s (id: %d)", me.Username, me.ID)
	log.Printf("Tools: %d registered", len(b.agent.Tools))

	// Set bot commands for menu
	_ = b.tg.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "start", Description: "Welcome & subagent options"},
			{Command: "spawn", Description: "Quick: spawn 2 subagents"},
			{Command: "custom", Description: "Custom: enter your own tasks"},
			{Command: "go", Description: "Spawn your custom tasks"},
			{Command: "cancel", Description: "Cancel custom spawn"},
			{Command: "status", Description: "Show running subagents"},
			{Command: "model", Description: "Set or show LLM model"},
			{Command: "voicenote", Description: "Save a voice message as a note"},
		},
	})

	// Webhook mode: requires WebhookURL in config (set via TELEGRAM_WEBHOOK_URL)
	webhookURL := os.Getenv("TELEGRAM_WEBHOOK_URL")
	webhookPath := os.Getenv("TELEGRAM_WEBHOOK_PATH")
	if webhookPath == "" {
		webhookPath = "/bot"
	}
	webhookListen := os.Getenv("TELEGRAM_WEBHOOK_LISTEN")
	if webhookListen == "" {
		webhookListen = ":8080"
	}

	if webhookURL != "" {
		return b.runWebhook(ctx, webhookURL, webhookPath, webhookListen)
	}
	return b.runLongPolling(ctx)
}

func (b *Bot) runLongPolling(ctx context.Context) error {
	updates, err := b.tg.UpdatesViaLongPolling(ctx, nil)
	if err != nil {
		return fmt.Errorf("long polling: %w", err)
	}
	return b.processUpdates(ctx, updates)
}

func (b *Bot) runWebhook(ctx context.Context, webhookURL, path, listenAddr string) error {
	mux := http.NewServeMux()
	secretToken := b.tg.SecretToken()
	updates, err := b.tg.UpdatesViaWebhook(ctx,
		telego.WebhookHTTPServeMux(mux, path, secretToken),
		telego.WithWebhookSet(ctx, &telego.SetWebhookParams{
			URL:         webhookURL,
			SecretToken: secretToken,
		}),
	)
	if err != nil {
		return fmt.Errorf("webhook setup: %w", err)
	}
	log.Printf("Webhook set: %s (path %s)", webhookURL, path)

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Webhook server error: %v", err)
		}
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	log.Printf("Webhook server listening on %s", listenAddr)
	return b.processUpdates(ctx, updates)
}

func (b *Bot) processUpdates(ctx context.Context, updates <-chan telego.Update) error {
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
			if update.CallbackQuery != nil {
				go b.handleCallbackQuery(ctx, update.CallbackQuery)
			}
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *telego.Message) {
	text := strings.TrimSpace(msg.Text)
	if text == "" && msg.Caption != "" {
		text = strings.TrimSpace(msg.Caption)
	}

	// /start: welcome with inline keyboard to enable subagents
	if text == "/start" {
		b.sendWelcome(ctx, msg.Chat.ChatID())
		return
	}

	// /spawn: quick trigger for parallel subagents
	if text == "/spawn" {
		b.processAgentPrompt(ctx, msg.Chat.ID, msg.Chat.ChatID(), msg.From, spawnParallelPrompt)
		return
	}

	// /custom: enter custom spawn mode (same as tapping Custom Subagents)
	if text == "/custom" {
		b.startCustomSpawn(ctx, msg.Chat.ID, msg.Chat.ChatID())
		return
	}

	// /go: execute custom spawn if we have tasks
	if text == "/go" {
		b.customSpawnMu.Lock()
		state := b.customSpawnMap[msg.Chat.ID]
		b.customSpawnMu.Unlock()
		if state != nil && len(state.Tasks) > 0 {
			b.executeCustomSpawn(ctx, msg.Chat.ID, msg.Chat.ChatID(), msg.From)
			return
		}
	}

	// /cancel: exit custom spawn mode
	if text == "/cancel" {
		b.cancelCustomSpawn(msg.Chat.ID)
		b.sendFormattedReply(ctx, msg.Chat.ChatID(), "Cancelled.")
		return
	}

	// /status: show running subagents
	if text == "/status" {
		b.sendStatus(ctx, msg.Chat.ID, msg.Chat.ChatID())
		return
	}



	// /model: set or show LLM model for this chat
	if text == "/model" || strings.HasPrefix(text, "/model ") {
		b.handleModel(ctx, msg.Chat.ID, msg.Chat.ChatID(), strings.TrimSpace(strings.TrimPrefix(text, "/model")))
		return
	}

	// /createagent: create a new agent/skill for later use
	if text == "/createagent" || strings.HasPrefix(text, "/createagent ") {
		b.handleCreateAgent(ctx, msg.Chat.ID, msg.Chat.ChatID(), msg.From, strings.TrimSpace(strings.TrimPrefix(text, "/createagent")))
		return
	}

	// /voicenote: save a voice message as a note (reply to a voice message with this)
	if text == "/voicenote" {
		b.handleVoiceNote(ctx, msg.Chat.ChatID(), msg.From, msg.ReplyToMessage)
		return
	}

	// /reboot: trigger graceful shutdown so systemd/supervisor can restart the bot
	if text == "/reboot" {
		b.handleReboot(ctx, msg.Chat.ChatID())
		return
	}

	// Custom spawn mode: user sent tasks (one per line)
	if handled, tasks := b.getAndSetCustomTasks(msg.Chat.ID, text); handled {
		if len(tasks) == 0 {
			b.sendFormattedReply(ctx, msg.Chat.ChatID(), "No tasks found. Send at least one task per line.")
			return
		}
		// Show tasks and Spawn/Cancel buttons
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("📋 <b>%d tasks</b>:\n", len(tasks)))
		for i, t := range tasks {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, t))
		}
		sb.WriteString("\nSend <b>/go</b> to spawn, or <b>/cancel</b> to abort.")
		btnGo := tu.InlineKeyboardButton("🚀 Spawn").WithCallbackData("custom_spawn_go")
		btnCancel := tu.InlineKeyboardButton("❌ Cancel").WithCallbackData("custom_spawn_cancel")
		markup := tu.InlineKeyboard(tu.InlineKeyboardRow(btnGo, btnCancel))
		params := tu.Message(msg.Chat.ChatID(), sb.String()).WithParseMode(telego.ModeHTML).WithReplyMarkup(markup)
		_, _ = b.tg.SendMessage(ctx, params)
		return
	}

	// Handle voice messages: transcribe via OpenRouter (single download)
	if msg.Voice != nil {
		voiceText := b.handleVoiceMessage(ctx, msg)
		if voiceText != "" {
			if text != "" {
				text = text + "\n" + voiceText
			} else {
				text = voiceText
			}
		}
	} else {
		// Handle other file uploads: download, upload to R2, tell the agent
		fileDesc := b.handleFileUpload(ctx, msg)
		if fileDesc != "" {
			if text != "" {
				text = text + "\n" + fileDesc
			} else {
				text = fileDesc
			}
		}
	}

	if text == "" {
		b.sendFormattedReply(ctx, msg.Chat.ChatID(), "I didn't receive any text. Send a message or file (photo, voice, document).")
		return
	}

	log.Printf("[%s] %s", msg.From.Username, text)

	// Send a visible placeholder while the agent thinks
	thinkMsg, err := b.tg.SendMessage(ctx, tu.Message(msg.Chat.ChatID(), "💭 Thinking..."))
	if err != nil {
		log.Printf("Failed to send thinking placeholder: %v", err)
	}

	// Also keep the native typing indicator running
	typingCtx, stopTyping := context.WithCancel(ctx)
	go b.keepTyping(typingCtx, msg.Chat.ChatID())

	userCtx := fmt.Sprintf("[User: %s (id: %d)", msg.From.Username, msg.From.ID)
	if msg.From.FirstName != "" {
		userCtx += fmt.Sprintf(", name: %s", msg.From.FirstName)
	}
	userCtx += "] " + text

	reply := b.agent.ProcessMessage(ctx, msg.Chat.ID, userCtx)
	stopTyping()

	if reply == "" {
		reply = "(no response)"
	}

	// Delete the "Thinking..." placeholder
	if thinkMsg != nil {
		_ = b.tg.DeleteMessage(ctx, &telego.DeleteMessageParams{
			ChatID:    msg.Chat.ChatID(),
			MessageID: thinkMsg.MessageID,
		})
	}

	b.sendFormattedReply(ctx, msg.Chat.ChatID(), reply)
}

// sendWelcome sends the welcome message with inline keyboard for subagents.
func (b *Bot) sendWelcome(ctx context.Context, chatID telego.ChatID) {
	text := "👋 <b>PicoFlare</b> — Cloudflare agent with subagents.\n\n" +
		"• <b>Spawn 2</b> — Quick: workers/ + pkg/agent/\n" +
		"• <b>Custom</b> — Enter your own tasks (one per line)"

	btn1 := tu.InlineKeyboardButton("🚀 Spawn 2").WithCallbackData("spawn_parallel")
	btn2 := tu.InlineKeyboardButton("✏️ Custom Subagents").WithCallbackData("custom_spawn")
	markup := tu.InlineKeyboard(tu.InlineKeyboardRow(btn1, btn2))

	params := tu.Message(chatID, text).WithParseMode(telego.ModeHTML).WithReplyMarkup(markup)
	_, err := b.tg.SendMessage(ctx, params)
	if err != nil {
		log.Printf("Send welcome failed: %v", err)
	}
}

// handleCallbackQuery handles inline button clicks.
func (b *Bot) handleCallbackQuery(ctx context.Context, q *telego.CallbackQuery) {
	// Answer callback to remove loading state
	_ = b.tg.AnswerCallbackQuery(ctx, tu.CallbackQuery(q.ID))

	if !q.Message.IsAccessible() {
		return
	}
	chat := q.Message.GetChat()
	chatID := chat.ChatID()

	switch q.Data {
	case "spawn_parallel":
		b.processAgentPrompt(ctx, chat.ID, chatID, &q.From, spawnParallelPrompt)
	case "custom_spawn":
		b.startCustomSpawn(ctx, chat.ID, chatID)
	case "custom_spawn_go":
		b.executeCustomSpawn(ctx, chat.ID, chatID, &q.From)
	case "custom_spawn_cancel":
		b.cancelCustomSpawn(chat.ID)
		b.sendFormattedReply(ctx, chatID, "Cancelled.")
	default:
		// Unknown callback, ignore
	}
}

// startCustomSpawn enters custom spawn mode and asks for tasks.
func (b *Bot) startCustomSpawn(ctx context.Context, chatIDInt int64, chatID telego.ChatID) {
	b.customSpawnMu.Lock()
	b.customSpawnMap[chatIDInt] = &customSpawnState{Tasks: nil}
	b.customSpawnMu.Unlock()

	text := "✏️ <b>Custom Subagents</b>\n\n" +
		"Send your tasks, <b>one per line</b>.\n\n" +
		"Example:\n" +
		"<code>List files in workers/</code>\n" +
		"<code>Review pkg/agent/ for bugs</code>\n" +
		"<code>Count lines in main.go</code>\n\n" +
		"Then send <b>/go</b> to spawn, or <b>/cancel</b> to abort."
	params := tu.Message(chatID, text).WithParseMode(telego.ModeHTML)
	_, _ = b.tg.SendMessage(ctx, params)
}

// executeCustomSpawn runs the agent with the collected tasks.
func (b *Bot) executeCustomSpawn(ctx context.Context, chatIDInt int64, chatID telego.ChatID, from *telego.User) {
	b.customSpawnMu.Lock()
	state := b.customSpawnMap[chatIDInt]
	delete(b.customSpawnMap, chatIDInt)
	b.customSpawnMu.Unlock()

	if state == nil || len(state.Tasks) == 0 {
		b.sendFormattedReply(ctx, chatID, "No tasks. Send your tasks first (one per line), then /go.")
		return
	}

	// Build prompt: Spawn N subagents with these tasks
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Spawn %d subagents in parallel:\n", len(state.Tasks)))
	for i, t := range state.Tasks {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, t))
	}
	sb.WriteString("\nUse spawn for all. I'll get separate messages when each completes.")
	b.processAgentPrompt(ctx, chatIDInt, chatID, from, sb.String())
}

// cancelCustomSpawn clears custom spawn state.
func (b *Bot) cancelCustomSpawn(chatIDInt int64) {
	b.customSpawnMu.Lock()
	delete(b.customSpawnMap, chatIDInt)
	b.customSpawnMu.Unlock()
}

// sendStatus reports running/completed subagent tasks for this chat.
func (b *Bot) sendStatus(ctx context.Context, chatIDInt int64, chatID telego.ChatID) {
	if b.agent.Tracker == nil {
		b.sendFormattedReply(ctx, chatID, "Subagent tracking is disabled.")
		return
	}
	tasks := b.agent.Tracker.ListTasks(chatIDInt)
	if len(tasks) == 0 {
		b.sendFormattedReply(ctx, chatID, "No subagent tasks.")
		return
	}
	var sb strings.Builder
	sb.WriteString("📋 <b>Subagent tasks</b>\n\n")
	running := 0
	for _, t := range tasks {
		icon := "✅"
		if t.Status == "running" {
			icon = "🔄"
			running++
		} else if t.Status == "failed" {
			icon = "❌"
		}
		label := t.Label
		if label == "" {
			label = t.Task
		}
		sb.WriteString(fmt.Sprintf("%s <b>%s</b> — %s\n", icon, label, t.Status))
	}
	if running > 0 {
		sb.WriteString(fmt.Sprintf("\n%d running.", running))
	}
	b.sendFormattedReply(ctx, chatID, sb.String())
}

// handleCreateAgent handles /createagent [description]. Creates a new skill via the agent.
func (b *Bot) handleCreateAgent(ctx context.Context, chatIDInt int64, chatID telego.ChatID, from *telego.User, desc string) {
	if desc == "" {
		b.sendFormattedReply(ctx, chatID, "🤖 <b>Create Agent</b>\n\nDescribe the agent you want. Example:\n<code>/createagent Next.js specialist for App Router and Server Components</code>\n\nI'll create a skill you can use later.")
		return
	}
	prompt := fmt.Sprintf("Create a new agent/skill for: %s\n\nUse the create_skill tool. Choose a kebab-case name (e.g. nextjs-specialist), write a clear description, and provide detailed instructions in the content so this agent type knows how to behave. Create it now.", desc)
	b.processAgentPrompt(ctx, chatIDInt, chatID, from, prompt)
	// Refresh session so the new skill is loaded into context
	b.agent.ForceRefreshSession(ctx, chatIDInt)
	b.sendFormattedReply(ctx, chatID, "✅ Session refreshed — your new agent skill is now in context.")
}

// handleVoiceNote handles /voicenote. Saves a voice message when replied-to. Reply to a voice message with /voicenote.
func (b *Bot) handleVoiceNote(ctx context.Context, chatID telego.ChatID, from *telego.User, replyTo *telego.Message) {
	if replyTo == nil || replyTo.Voice == nil {
		b.sendFormattedReply(ctx, chatID, "🎤 <b>Save Voice Note</b>\n\nReply to a voice message with <code>/voicenote</code> to save it. I'll transcribe and store the audio + transcript.")
		return
	}

	// Download voice
	file, err := b.tg.GetFile(ctx, &telego.GetFileParams{FileID: replyTo.Voice.FileID})
	if err != nil {
		log.Printf("voicenote GetFile failed: %v", err)
		b.sendFormattedReply(ctx, chatID, fmt.Sprintf("Couldn't get the voice file: %v", err))
		return
	}
	fileURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.tg.Token(), file.FilePath)
	resp, err := http.Get(fileURL)
	if err != nil {
		log.Printf("voicenote download failed: %v", err)
		b.sendFormattedReply(ctx, chatID, fmt.Sprintf("Couldn't download: %v", err))
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		b.sendFormattedReply(ctx, chatID, "Couldn't read voice data.")
		return
	}

	ts := time.Now().Format("20060102_150405")
	userID := from.ID

	// Store audio in R2
	audioKey := fmt.Sprintf("users/%d/files/voice_%s.ogg", userID, ts)
	if b.agent.R2 != nil {
		if err := b.agent.R2.UploadObject(ctx, b.agent.Bucket, audioKey, data); err != nil {
			log.Printf("voicenote R2 upload failed: %v", err)
		}
	}

	// Transcribe
	var transcript string
	if b.openRouterKey != "" {
		text, err := transcribe.Transcribe(ctx, b.openRouterKey, data, "ogg")
		if err != nil {
			log.Printf("voicenote transcribe failed: %v", err)
			transcript = "(transcription failed)"
		} else {
			transcript = text
		}
	} else {
		transcript = "(transcription not configured)"
	}

	// Store transcript in R2 under notes/
	noteKey := fmt.Sprintf("users/%d/notes/voice_%s.txt", userID, ts)
	if b.agent.R2 != nil && transcript != "" {
		noteBody := []byte(transcript)
		_ = b.agent.R2.UploadObject(ctx, b.agent.Bucket, noteKey, noteBody)
	}

	preview := transcript
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	b.sendFormattedReply(ctx, chatID, fmt.Sprintf("✅ <b>Voice note saved</b>\n\n%s", escapeHTML(preview)))
}

// handleReboot handles /reboot. Triggers graceful shutdown so systemd/supervisor restarts the bot.
func (b *Bot) handleReboot(ctx context.Context, chatID telego.ChatID) {
	b.sendFormattedReply(ctx, chatID, "🔄 Rebooting...")
	if b.runCancel != nil {
		b.runCancel()
	}
}

// handleModel handles /model [model_id|default]. Empty = show current.
func (b *Bot) handleModel(ctx context.Context, chatIDInt int64, chatID telego.ChatID, arg string) {
	if b.agent.LLM == nil {
		b.sendFormattedReply(ctx, chatID, "LLM not configured.")
		return
	}
	if arg == "" {
		// Show current model
		model := b.agent.GetModel(chatIDInt)
		b.sendFormattedReply(ctx, chatID, fmt.Sprintf("🤖 <b>Model</b>: <code>%s</code>\n\nUse /model &lt;id&gt; to change, /model default to reset.", model))
		return
	}
	if strings.EqualFold(arg, "default") {
		b.agent.SetModel(chatIDInt, "")
		model := b.agent.GetModel(chatIDInt)
		b.sendFormattedReply(ctx, chatID, fmt.Sprintf("Model reset to default: <code>%s</code>", model))
		return
	}
	b.agent.SetModel(chatIDInt, arg)
	b.sendFormattedReply(ctx, chatID, fmt.Sprintf("Model set to <code>%s</code>. Next messages will use this model.", arg))
}

// getAndSetCustomTasks stores the user's message as tasks and returns true if we handled it.
// If state already has tasks, new lines are appended (add more).
func (b *Bot) getAndSetCustomTasks(chatIDInt int64, text string) (handled bool, tasks []string) {
	b.customSpawnMu.Lock()
	defer b.customSpawnMu.Unlock()

	state := b.customSpawnMap[chatIDInt]
	if state == nil {
		return false, nil
	}

	// Parse tasks: split by newline, trim, skip empty
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			tasks = append(tasks, line)
		}
	}

	// Append to existing or set new
	if len(state.Tasks) > 0 {
		tasks = append(state.Tasks, tasks...)
	}
	if len(tasks) == 0 {
		return true, nil
	}

	b.customSpawnMap[chatIDInt] = &customSpawnState{Tasks: tasks}
	return true, tasks
}

// processAgentPrompt runs the agent with the given prompt and sends the reply.
func (b *Bot) processAgentPrompt(ctx context.Context, chatIDInt int64, chatID telego.ChatID, from *telego.User, prompt string) {
	userCtx := fmt.Sprintf("[User: %s (id: %d)", from.Username, from.ID)
	if from.FirstName != "" {
		userCtx += fmt.Sprintf(", name: %s", from.FirstName)
	}
	userCtx += "] " + prompt

	thinkMsg, _ := b.tg.SendMessage(ctx, tu.Message(chatID, "💭 Thinking..."))
	typingCtx, stopTyping := context.WithCancel(ctx)
	go b.keepTyping(typingCtx, chatID)

	reply := b.agent.ProcessMessage(ctx, chatIDInt, userCtx)
	stopTyping()

	if thinkMsg != nil {
		_ = b.tg.DeleteMessage(ctx, &telego.DeleteMessageParams{
			ChatID: chatID, MessageID: thinkMsg.MessageID,
		})
	}
	if reply == "" {
		reply = "(no response)"
	}
	b.sendFormattedReply(ctx, chatID, reply)
}

// sendFormattedReply splits a reply into code-block-aware chunks, converts each
// to Telegram HTML, and falls back to plain text if Telegram rejects the HTML.
func (b *Bot) sendFormattedReply(ctx context.Context, chatID telego.ChatID, reply string) {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		reply = "(no response)"
	}
	chunks := splitMarkdownChunks(reply, 3500)
	if len(chunks) == 0 {
		b.sendPlainChunks(ctx, chatID, "(no response)")
		return
	}

	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		html := markdownToTelegramHTML(chunk)
		if strings.TrimSpace(html) == "" {
			b.sendPlainChunks(ctx, chatID, chunk)
			continue
		}

		if len(html) > 4096 {
			log.Printf("HTML chunk too long (%d), sending plain text", len(html))
			b.sendPlainChunks(ctx, chatID, chunk)
			continue
		}

		params := tu.Message(chatID, html).WithParseMode(telego.ModeHTML)
		_, err := b.tg.SendMessage(ctx, params)
		if err != nil {
			log.Printf("HTML send failed (%v), falling back to plain text", err)
			b.sendPlainChunks(ctx, chatID, chunk)
		}
	}
}

func (b *Bot) sendPlainChunks(ctx context.Context, chatID telego.ChatID, text string) {
	for len(text) > 0 {
		chunk := text
		if len(chunk) > 4090 {
			cut := strings.LastIndex(chunk[:4090], "\n")
			if cut < 2000 {
				cut = 4090
			}
			chunk = text[:cut]
			text = text[cut:]
		} else {
			text = ""
		}
		_, err := b.tg.SendMessage(ctx, tu.Message(chatID, chunk))
		if err != nil {
			log.Printf("Plain send failed: %v", err)
			return
		}
	}
}

// keepTyping sends "typing..." to Telegram every 3 seconds until ctx is cancelled.
// Telegram's typing indicator expires after ~5s, so we refresh before it fades.
func (b *Bot) keepTyping(ctx context.Context, chatID telego.ChatID) {
	send := func() {
		err := b.tg.SendChatAction(ctx, tu.ChatAction(chatID, telego.ChatActionTyping))
		if err != nil && ctx.Err() == nil {
			log.Printf("typing indicator error: %v", err)
		}
	}
	send()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// handleFileUpload detects file attachments, downloads them from Telegram,
// uploads to the user's R2 space, and returns a description for the agent.
func (b *Bot) handleFileUpload(ctx context.Context, msg *telego.Message) string {
	var fileID, fileName, fileType string

	switch {
	case msg.Document != nil:
		fileID = msg.Document.FileID
		fileName = msg.Document.FileName
		fileType = "document"
	case msg.Photo != nil && len(msg.Photo) > 0:
		best := msg.Photo[len(msg.Photo)-1]
		fileID = best.FileID
		fileName = fmt.Sprintf("photo_%d.jpg", msg.Date)
		fileType = "photo"
	case msg.Voice != nil:
		fileID = msg.Voice.FileID
		fileName = fmt.Sprintf("voice_%d.ogg", msg.Date)
		fileType = "voice"
	case msg.Audio != nil:
		fileID = msg.Audio.FileID
		fileName = msg.Audio.FileName
		if fileName == "" {
			fileName = fmt.Sprintf("audio_%d.mp3", msg.Date)
		}
		fileType = "audio"
	case msg.Video != nil:
		fileID = msg.Video.FileID
		fileName = msg.Video.FileName
		if fileName == "" {
			fileName = fmt.Sprintf("video_%d.mp4", msg.Date)
		}
		fileType = "video"
	case msg.VideoNote != nil:
		fileID = msg.VideoNote.FileID
		fileName = fmt.Sprintf("videonote_%d.mp4", msg.Date)
		fileType = "video_note"
	case msg.Sticker != nil:
		fileID = msg.Sticker.FileID
		fileName = fmt.Sprintf("sticker_%d.webp", msg.Date)
		fileType = "sticker"
	default:
		return ""
	}

	if fileID == "" {
		return ""
	}

	// Get file info from Telegram
	file, err := b.tg.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		log.Printf("GetFile failed: %v", err)
		return fmt.Sprintf("[User sent a %s but I couldn't download it: %v]", fileType, err)
	}

	// Download from Telegram
	fileURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.tg.Token(), file.FilePath)
	resp, err := http.Get(fileURL)
	if err != nil {
		log.Printf("Download file failed: %v", err)
		return fmt.Sprintf("[User sent a %s but download failed: %v]", fileType, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("[User sent a %s but read failed: %v]", fileType, err)
	}

	userID := fmt.Sprintf("%d", msg.From.ID)
	r2Key := fmt.Sprintf("users/%s/files/%s", userID, fileName)

	// Voice notes: handled by handleVoiceMessage (download, transcribe, upload)
	if fileType == "voice" {
		return ""
	}

	// Upload to R2 user space (non-voice files)

	if b.agent.R2 != nil {
		if err := b.agent.R2.UploadObject(ctx, b.agent.Bucket, r2Key, data); err != nil {
			log.Printf("R2 upload failed: %v", err)
			return fmt.Sprintf("[User sent %s %q (%d bytes) but R2 upload failed: %v]", fileType, fileName, len(data), err)
		}
		log.Printf("File uploaded: %s -> r2://%s/%s (%d bytes)", fileType, b.agent.Bucket, r2Key, len(data))
		return fmt.Sprintf("[User uploaded %s: %q (%d bytes) -> stored at r2://%s/%s]",
			fileType, fileName, len(data), b.agent.Bucket, r2Key)
	}

	return fmt.Sprintf("[User sent %s: %q (%d bytes) but R2 not configured]", fileType, fileName, len(data))
}

// handleVoiceMessage transcribes a voice message using OpenRouter API.
func (b *Bot) handleVoiceMessage(ctx context.Context, msg *telego.Message) string {
	if msg.Voice == nil {
		return ""
	}

	// Download voice file from Telegram
	file, err := b.tg.GetFile(ctx, &telego.GetFileParams{FileID: msg.Voice.FileID})
	if err != nil {
		log.Printf("GetFile failed: %v", err)
		return fmt.Sprintf("[Voice download failed: %v]", err)
	}

	fileURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.tg.Token(), file.FilePath)
	resp, err := http.Get(fileURL)
	if err != nil {
		log.Printf("Download voice failed: %v", err)
		return fmt.Sprintf("[Voice download failed: %v]", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("[Voice read failed: %v]", err)
	}

	// Store voice file in R2
	if b.agent.R2 != nil {
		fileName := fmt.Sprintf("voice_%d.ogg", msg.Date)
		r2Key := fmt.Sprintf("users/%d/files/%s", msg.From.ID, fileName)
		_ = b.agent.R2.UploadObject(ctx, b.agent.Bucket, r2Key, data)
	}

	// Transcribe via OpenRouter only
	if b.openRouterKey == "" {
		return "[Voice transcription failed: no API key configured]"
	}

	text, err := transcribe.Transcribe(ctx, b.openRouterKey, data, "ogg")
	if err != nil {
		return fmt.Sprintf("[Voice transcription failed: %v]", err)
	}
	return fmt.Sprintf("[Voice transcribed]: %s", text)
}
