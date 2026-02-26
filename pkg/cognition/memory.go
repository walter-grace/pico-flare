// Package cognition implements PicoFlare's cognitive memory architecture.
//
// Memory is organized into four layers, all persisted in R2:
//
//	Working   - Current conversation context (in-process, not persisted per-message)
//	Episodic  - Timestamped experiences: "what happened" (R2: memory/episodes/)
//	Semantic  - Facts and knowledge: "what I know" (R2: memory/knowledge/)
//	Procedural - Learned skills and procedures: "how to do things" (R2: memory/procedures/)
//
// A cortex coordinates retrieval across layers, injecting only what's relevant
// into the LLM context window — minimizing token spend.
package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/bigneek/picoflare/pkg/agentctx"
	"github.com/bigneek/picoflare/pkg/storage"
)

// Memory is the full cognitive memory system backed by R2.
type Memory struct {
	r2     *storage.R2Client
	bucket string
}

func NewMemory(r2 *storage.R2Client, bucket string) *Memory {
	return &Memory{r2: r2, bucket: bucket}
}

// --- Episodic Memory: timestamped experiences ---

type Episode struct {
	ID        string            `json:"id"`
	Timestamp time.Time         `json:"timestamp"`
	Type      string            `json:"type"` // "conversation", "tool_use", "error", "insight", "goal"
	Summary   string            `json:"summary"`
	Detail    string            `json:"detail,omitempty"`
	Tags      []string          `json:"tags,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

func (m *Memory) prefix(ctx context.Context) string {
	if id, ok := agentctx.AgentIDFromContext(ctx); ok && id != "" {
		return fmt.Sprintf("agents/%s/", id)
	}
	return ""
}

func (m *Memory) SaveEpisode(ctx context.Context, ep Episode) error {
	if ep.ID == "" {
		ep.ID = fmt.Sprintf("ep-%d", time.Now().UnixNano())
	}
	if ep.Timestamp.IsZero() {
		ep.Timestamp = time.Now()
	}

	data, err := json.Marshal(ep)
	if err != nil {
		return err
	}

	p := m.prefix(ctx)
	// Store individually for random access
	key := p + fmt.Sprintf("memory/episodes/%s/%s.json",
		ep.Timestamp.Format("20060102"), ep.ID)
	if err := m.r2.UploadObject(ctx, m.bucket, key, data); err != nil {
		return err
	}

	// Also append to daily log for sequential access
	logKey := p + fmt.Sprintf("memory/episodes/%s/log.jsonl", ep.Timestamp.Format("20060102"))
	existing, _ := m.r2.DownloadObject(ctx, m.bucket, logKey)
	line := string(data) + "\n"
	return m.r2.UploadObject(ctx, m.bucket, logKey, append(existing, []byte(line)...))
}

func (m *Memory) LoadTodayEpisodes(ctx context.Context) ([]Episode, error) {
	return m.LoadEpisodesForDate(ctx, time.Now())
}

func (m *Memory) LoadEpisodesForDate(ctx context.Context, date time.Time) ([]Episode, error) {
	key := fmt.Sprintf("memory/episodes/%s/log.jsonl", date.Format("20060102"))
	data, err := m.r2.DownloadObject(ctx, m.bucket, key)
	if err != nil {
		return nil, nil // no episodes for this date
	}

	var episodes []Episode
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ep Episode
		if err := json.Unmarshal([]byte(line), &ep); err != nil {
			continue
		}
		episodes = append(episodes, ep)
	}
	return episodes, nil
}

func (m *Memory) LoadRecentEpisodes(ctx context.Context, days int, maxCount int) []Episode {
	var all []Episode
	now := time.Now()
	for i := 0; i < days; i++ {
		d := now.AddDate(0, 0, -i)
		eps, _ := m.LoadEpisodesForDate(ctx, d)
		all = append(all, eps...)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp.After(all[j].Timestamp)
	})
	if len(all) > maxCount {
		all = all[:maxCount]
	}
	return all
}

// --- Semantic Memory: facts and knowledge ---

type Fact struct {
	ID         string    `json:"id"`
	Category   string    `json:"category"` // "user", "system", "domain", "preference", "project"
	Content    string    `json:"content"`
	Confidence float64   `json:"confidence"` // 0.0-1.0
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type KnowledgeBase struct {
	Facts     []Fact    `json:"facts"`
	UpdatedAt time.Time `json:"updated_at"`
}

const knowledgeKey = "memory/knowledge/facts.json"

func (m *Memory) knowledgeKey(ctx context.Context) string {
	return m.prefix(ctx) + knowledgeKey
}

func (m *Memory) LoadKnowledge(ctx context.Context) (*KnowledgeBase, error) {
	data, err := m.r2.DownloadObject(ctx, m.bucket, m.knowledgeKey(ctx))
	if err != nil {
		return &KnowledgeBase{}, nil
	}
	var kb KnowledgeBase
	if err := json.Unmarshal(data, &kb); err != nil {
		return &KnowledgeBase{}, nil
	}
	return &kb, nil
}

func (m *Memory) SaveKnowledge(ctx context.Context, kb *KnowledgeBase) error {
	kb.UpdatedAt = time.Now()
	data, err := json.Marshal(kb)
	if err != nil {
		return err
	}
	return m.r2.UploadObject(ctx, m.bucket, m.knowledgeKey(ctx), data)
}

// LearnFact adds or updates a fact in the knowledge base.
func (m *Memory) LearnFact(ctx context.Context, fact Fact) error {
	kb, err := m.LoadKnowledge(ctx)
	if err != nil {
		return err
	}

	if fact.ID == "" {
		fact.ID = fmt.Sprintf("fact-%d", time.Now().UnixNano())
	}
	if fact.CreatedAt.IsZero() {
		fact.CreatedAt = time.Now()
	}
	fact.UpdatedAt = time.Now()
	if fact.Confidence == 0 {
		fact.Confidence = 0.8
	}

	// Update existing or append
	found := false
	for i, f := range kb.Facts {
		if f.ID == fact.ID || (f.Category == fact.Category && f.Content == fact.Content) {
			kb.Facts[i] = fact
			found = true
			break
		}
	}
	if !found {
		kb.Facts = append(kb.Facts, fact)
	}

	return m.SaveKnowledge(ctx, kb)
}

func (m *Memory) QueryFacts(ctx context.Context, category string) []Fact {
	kb, err := m.LoadKnowledge(ctx)
	if err != nil {
		return nil
	}
	if category == "" {
		return kb.Facts
	}
	var results []Fact
	for _, f := range kb.Facts {
		if f.Category == category {
			results = append(results, f)
		}
	}
	return results
}

// --- Procedural Memory: learned skills and procedures ---

type Procedure struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Steps       []string  `json:"steps"`
	Code        string    `json:"code,omitempty"`    // JS code for cf_execute
	Trigger     string    `json:"trigger,omitempty"` // when to auto-invoke
	Uses        int       `json:"uses"`
	LastUsed    time.Time `json:"last_used"`
	CreatedAt   time.Time `json:"created_at"`
}

const proceduresKey = "memory/procedures/index.json"

func (m *Memory) proceduresKey(ctx context.Context) string {
	return m.prefix(ctx) + proceduresKey
}

func (m *Memory) LoadProcedures(ctx context.Context) ([]Procedure, error) {
	data, err := m.r2.DownloadObject(ctx, m.bucket, m.proceduresKey(ctx))
	if err != nil {
		return nil, nil
	}
	var procs []Procedure
	if err := json.Unmarshal(data, &procs); err != nil {
		return nil, nil
	}
	return procs, nil
}

func (m *Memory) SaveProcedure(ctx context.Context, proc Procedure) error {
	procs, _ := m.LoadProcedures(ctx)

	if proc.ID == "" {
		proc.ID = fmt.Sprintf("proc-%d", time.Now().UnixNano())
	}
	if proc.CreatedAt.IsZero() {
		proc.CreatedAt = time.Now()
	}

	found := false
	for i, p := range procs {
		if p.ID == proc.ID || p.Name == proc.Name {
			procs[i] = proc
			found = true
			break
		}
	}
	if !found {
		procs = append(procs, proc)
	}

	data, err := json.Marshal(procs)
	if err != nil {
		return err
	}
	return m.r2.UploadObject(ctx, m.bucket, m.proceduresKey(ctx), data)
}

func (m *Memory) RecordProcedureUse(ctx context.Context, name string) {
	procs, _ := m.LoadProcedures(ctx)
	for i, p := range procs {
		if p.Name == name {
			procs[i].Uses++
			procs[i].LastUsed = time.Now()
			data, _ := json.Marshal(procs)
			_ = m.r2.UploadObject(ctx, m.bucket, m.proceduresKey(ctx), data)
			return
		}
	}
}

// --- Cortex: smart retrieval across memory layers ---

// ContextBudget controls how many tokens to allocate per memory layer.
type ContextBudget struct {
	MaxTotalChars  int // rough proxy for tokens (1 token ~ 4 chars)
	EpisodicPct    int // % of budget for episodes
	SemanticPct    int // % of budget for facts
	ProceduralPct  int // % of budget for procedures
}

var DefaultBudget = ContextBudget{
	MaxTotalChars: 6000, // ~1500 tokens
	EpisodicPct:   30,
	SemanticPct:   50,
	ProceduralPct: 20,
}

// BuildContext assembles a memory context string optimized for the token budget.
// It pulls from all layers and formats them for the system prompt.
func (m *Memory) BuildContext(ctx context.Context, budget ContextBudget) string {
	if m.r2 == nil {
		return "(No memory backend connected)\n"
	}

	var sections []string
	remaining := budget.MaxTotalChars

	// Semantic: facts (highest priority -- they define identity)
	semanticBudget := budget.MaxTotalChars * budget.SemanticPct / 100
	facts := m.QueryFacts(ctx, "")
	if len(facts) > 0 {
		var factLines []string
		charCount := 0
		// Sort by confidence descending
		sort.Slice(facts, func(i, j int) bool {
			return facts[i].Confidence > facts[j].Confidence
		})
		for _, f := range facts {
			line := fmt.Sprintf("- [%s] %s (confidence: %.0f%%)", f.Category, f.Content, f.Confidence*100)
			if charCount+len(line) > semanticBudget {
				break
			}
			factLines = append(factLines, line)
			charCount += len(line)
		}
		if len(factLines) > 0 {
			section := "### Known Facts\n" + strings.Join(factLines, "\n")
			sections = append(sections, section)
			remaining -= len(section)
		}
	}

	// Episodic: recent experiences
	episodicBudget := budget.MaxTotalChars * budget.EpisodicPct / 100
	if episodicBudget > remaining {
		episodicBudget = remaining
	}
	episodes := m.LoadRecentEpisodes(ctx, 3, 20)
	if len(episodes) > 0 {
		var epLines []string
		charCount := 0
		for _, ep := range episodes {
			line := fmt.Sprintf("- %s [%s] %s", ep.Timestamp.Format("Jan 2 15:04"), ep.Type, ep.Summary)
			if charCount+len(line) > episodicBudget {
				break
			}
			epLines = append(epLines, line)
			charCount += len(line)
		}
		if len(epLines) > 0 {
			section := "### Recent Activity\n" + strings.Join(epLines, "\n")
			sections = append(sections, section)
			remaining -= len(section)
		}
	}

	// Procedural: known skills
	proceduralBudget := budget.MaxTotalChars * budget.ProceduralPct / 100
	if proceduralBudget > remaining {
		proceduralBudget = remaining
	}
	procs, _ := m.LoadProcedures(ctx)
	if len(procs) > 0 {
		sort.Slice(procs, func(i, j int) bool { return procs[i].Uses > procs[j].Uses })
		var procLines []string
		charCount := 0
		for _, p := range procs {
			line := fmt.Sprintf("- **%s**: %s (used %dx)", p.Name, p.Description, p.Uses)
			if charCount+len(line) > proceduralBudget {
				break
			}
			procLines = append(procLines, line)
			charCount += len(line)
		}
		if len(procLines) > 0 {
			section := "### Learned Procedures\n" + strings.Join(procLines, "\n")
			sections = append(sections, section)
		}
	}

	if len(sections) == 0 {
		return "(Memory is empty. I'll learn as we interact.)\n"
	}

	return strings.Join(sections, "\n\n") + "\n"
}

// --- Background learning: extract and store knowledge from conversations ---

// ExtractAndLearn analyzes a conversation turn and extracts learnable information.
// Called after each agent response to build memory over time.
func (m *Memory) ExtractAndLearn(ctx context.Context, userMsg, agentReply string, toolsUsed []string) {
	// Log the interaction as an episode
	ep := Episode{
		Timestamp: time.Now(),
		Type:      "conversation",
		Summary:   truncateStr(userMsg, 200),
		Tags:      toolsUsed,
	}
	if len(toolsUsed) > 0 {
		ep.Type = "tool_use"
		ep.Detail = fmt.Sprintf("Used: %s", strings.Join(toolsUsed, ", "))
	}
	if err := m.SaveEpisode(ctx, ep); err != nil {
		log.Printf("cognition: save episode failed: %v", err)
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
