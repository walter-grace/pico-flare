package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bigneek/picoflare/pkg/storage"
)

// TokenLedger tracks all token expenditure across the agent's lifetime.
type TokenLedger struct {
	mu     sync.Mutex
	r2     *storage.R2Client
	bucket string

	// Current session
	Session SessionStats

	// Lifetime (loaded from R2)
	Lifetime LifetimeStats
}

type SessionStats struct {
	StartedAt        time.Time       `json:"started_at"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	ToolCalls        int             `json:"tool_calls"`
	Messages         int             `json:"messages"`
	Iterations       int             `json:"iterations"`
	ByTool           map[string]int  `json:"by_tool"`
	ByModel          map[string]int  `json:"by_model"`
	CostUSD          float64         `json:"cost_usd"`
}

type LifetimeStats struct {
	FirstSeen        time.Time       `json:"first_seen"`
	TotalSessions    int             `json:"total_sessions"`
	PromptTokens     int64           `json:"prompt_tokens"`
	CompletionTokens int64           `json:"completion_tokens"`
	TotalToolCalls   int64           `json:"total_tool_calls"`
	TotalMessages    int64           `json:"total_messages"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	ByTool           map[string]int64 `json:"by_tool"`
	ByDay            map[string]int64 `json:"by_day"` // "20060102" -> total tokens
}

const ledgerKey = "memory/tokenomics/lifetime.json"

// Model pricing (per 1M tokens) -- approximations for OpenRouter
var modelPricing = map[string][2]float64{
	"moonshotai/kimi-k2.5":          {0.60, 2.40},
	"anthropic/claude-sonnet-4":     {3.00, 15.00},
	"anthropic/claude-3.5-sonnet":   {3.00, 15.00},
	"openai/gpt-4o":                 {2.50, 10.00},
	"openai/gpt-4o-mini":            {0.15, 0.60},
	"google/gemini-2.5-flash":       {0.15, 0.60},
	"deepseek/deepseek-chat":        {0.14, 0.28},
}

func NewTokenLedger(r2 *storage.R2Client, bucket string) *TokenLedger {
	tl := &TokenLedger{
		r2:     r2,
		bucket: bucket,
		Session: SessionStats{
			StartedAt: time.Now(),
			ByTool:    make(map[string]int),
			ByModel:   make(map[string]int),
		},
	}
	return tl
}

func (tl *TokenLedger) LoadLifetime(ctx context.Context) {
	if tl.r2 == nil {
		return
	}
	data, err := tl.r2.DownloadObject(ctx, tl.bucket, ledgerKey)
	if err != nil {
		tl.Lifetime = LifetimeStats{
			FirstSeen: time.Now(),
			ByTool:    make(map[string]int64),
			ByDay:     make(map[string]int64),
		}
		return
	}
	if err := json.Unmarshal(data, &tl.Lifetime); err != nil {
		tl.Lifetime = LifetimeStats{
			FirstSeen: time.Now(),
			ByTool:    make(map[string]int64),
			ByDay:     make(map[string]int64),
		}
	}
	if tl.Lifetime.ByTool == nil {
		tl.Lifetime.ByTool = make(map[string]int64)
	}
	if tl.Lifetime.ByDay == nil {
		tl.Lifetime.ByDay = make(map[string]int64)
	}
}

func (tl *TokenLedger) SaveLifetime(ctx context.Context) {
	if tl.r2 == nil {
		return
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()

	data, err := json.MarshalIndent(tl.Lifetime, "", "  ")
	if err != nil {
		return
	}
	if err := tl.r2.UploadObject(ctx, tl.bucket, ledgerKey, data); err != nil {
		log.Printf("tokenomics: save lifetime failed: %v", err)
	}
}

// RecordLLMCall logs token usage for a single LLM API call.
func (tl *TokenLedger) RecordLLMCall(model string, promptTokens, completionTokens int) {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	tl.Session.PromptTokens += promptTokens
	tl.Session.CompletionTokens += completionTokens
	tl.Session.Iterations++
	tl.Session.ByModel[model] += promptTokens + completionTokens

	cost := tl.estimateCost(model, promptTokens, completionTokens)
	tl.Session.CostUSD += cost

	tl.Lifetime.PromptTokens += int64(promptTokens)
	tl.Lifetime.CompletionTokens += int64(completionTokens)
	tl.Lifetime.TotalCostUSD += cost

	today := time.Now().Format("20060102")
	tl.Lifetime.ByDay[today] += int64(promptTokens + completionTokens)
}

// RecordToolCall logs a tool invocation.
func (tl *TokenLedger) RecordToolCall(toolName string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	tl.Session.ToolCalls++
	tl.Session.ByTool[toolName]++
	tl.Lifetime.TotalToolCalls++
	tl.Lifetime.ByTool[toolName]++
}

// RecordMessage logs a user message.
func (tl *TokenLedger) RecordMessage() {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	tl.Session.Messages++
	tl.Lifetime.TotalMessages++
}

// FlushSession saves session data to lifetime and persists.
func (tl *TokenLedger) FlushSession(ctx context.Context) {
	tl.mu.Lock()
	tl.Lifetime.TotalSessions++
	tl.mu.Unlock()
	tl.SaveLifetime(ctx)
}

func (tl *TokenLedger) estimateCost(model string, prompt, completion int) float64 {
	pricing, ok := modelPricing[model]
	if !ok {
		// Default to cheap model pricing
		pricing = [2]float64{0.50, 2.00}
	}
	return (float64(prompt)*pricing[0] + float64(completion)*pricing[1]) / 1_000_000
}

// Report returns a human-readable tokenomics report.
func (tl *TokenLedger) Report() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("## Tokenomics Report\n\n")

	sb.WriteString("### This Session\n")
	sb.WriteString(fmt.Sprintf("- Messages: %d\n", tl.Session.Messages))
	sb.WriteString(fmt.Sprintf("- LLM iterations: %d\n", tl.Session.Iterations))
	sb.WriteString(fmt.Sprintf("- Tokens: %d in / %d out (%d total)\n",
		tl.Session.PromptTokens, tl.Session.CompletionTokens,
		tl.Session.PromptTokens+tl.Session.CompletionTokens))
	sb.WriteString(fmt.Sprintf("- Tool calls: %d\n", tl.Session.ToolCalls))
	sb.WriteString(fmt.Sprintf("- Estimated cost: $%.6f\n", tl.Session.CostUSD))

	if len(tl.Session.ByTool) > 0 {
		sb.WriteString("- Tools used: ")
		var parts []string
		for k, v := range tl.Session.ByTool {
			parts = append(parts, fmt.Sprintf("%s(%d)", k, v))
		}
		sb.WriteString(strings.Join(parts, ", ") + "\n")
	}

	sb.WriteString("\n### Lifetime\n")
	sb.WriteString(fmt.Sprintf("- Since: %s\n", tl.Lifetime.FirstSeen.Format("2006-01-02")))
	sb.WriteString(fmt.Sprintf("- Sessions: %d\n", tl.Lifetime.TotalSessions))
	sb.WriteString(fmt.Sprintf("- Messages: %d\n", tl.Lifetime.TotalMessages))
	sb.WriteString(fmt.Sprintf("- Tokens: %d in / %d out\n", tl.Lifetime.PromptTokens, tl.Lifetime.CompletionTokens))
	sb.WriteString(fmt.Sprintf("- Tool calls: %d\n", tl.Lifetime.TotalToolCalls))
	sb.WriteString(fmt.Sprintf("- Total cost: $%.6f\n", tl.Lifetime.TotalCostUSD))

	return sb.String()
}

// ContextCost estimates token count for a string (rough: 1 token ~ 4 chars).
func ContextCost(s string) int {
	return len(s) / 4
}
