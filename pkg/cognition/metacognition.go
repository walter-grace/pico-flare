package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bigneek/picoflare/pkg/storage"
)

// MetaCognition tracks the agent's self-awareness: goals, capabilities,
// performance metrics, and self-improvement directives.
type MetaCognition struct {
	r2     *storage.R2Client
	bucket string
}

func NewMetaCognition(r2 *storage.R2Client, bucket string) *MetaCognition {
	return &MetaCognition{r2: r2, bucket: bucket}
}

// --- Goals ---

type Goal struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Status      string    `json:"status"` // "active", "completed", "blocked", "abandoned"
	Priority    int       `json:"priority"` // 1 (highest) - 5 (lowest)
	SubGoals    []string  `json:"sub_goals,omitempty"`
	Progress    string    `json:"progress,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const goalsKey = "memory/meta/goals.json"

func (mc *MetaCognition) LoadGoals(ctx context.Context) ([]Goal, error) {
	data, err := mc.r2.DownloadObject(ctx, mc.bucket, goalsKey)
	if err != nil {
		return nil, nil
	}
	var goals []Goal
	json.Unmarshal(data, &goals)
	return goals, nil
}

func (mc *MetaCognition) SaveGoal(ctx context.Context, goal Goal) error {
	goals, _ := mc.LoadGoals(ctx)
	if goal.ID == "" {
		goal.ID = fmt.Sprintf("goal-%d", time.Now().UnixNano())
	}
	if goal.CreatedAt.IsZero() {
		goal.CreatedAt = time.Now()
	}
	goal.UpdatedAt = time.Now()

	found := false
	for i, g := range goals {
		if g.ID == goal.ID {
			goals[i] = goal
			found = true
			break
		}
	}
	if !found {
		goals = append(goals, goal)
	}

	data, _ := json.Marshal(goals)
	return mc.r2.UploadObject(ctx, mc.bucket, goalsKey, data)
}

// --- Self-Reflection ---

type Reflection struct {
	Timestamp     time.Time `json:"timestamp"`
	Observation   string    `json:"observation"`
	Assessment    string    `json:"assessment"`
	Improvement   string    `json:"improvement"`
	TokensSpent   int       `json:"tokens_spent"`
	ToolsUsed     int       `json:"tools_used"`
}

const reflectionsKey = "memory/meta/reflections.jsonl"

func (mc *MetaCognition) SaveReflection(ctx context.Context, r Reflection) error {
	r.Timestamp = time.Now()
	data, _ := json.Marshal(r)
	existing, _ := mc.r2.DownloadObject(ctx, mc.bucket, reflectionsKey)
	return mc.r2.UploadObject(ctx, mc.bucket, reflectionsKey, append(existing, append(data, '\n')...))
}

func (mc *MetaCognition) LoadRecentReflections(ctx context.Context, max int) []Reflection {
	data, err := mc.r2.DownloadObject(ctx, mc.bucket, reflectionsKey)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	var refs []Reflection
	for i := len(lines) - 1; i >= 0 && len(refs) < max; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var r Reflection
		if json.Unmarshal([]byte(line), &r) == nil {
			refs = append(refs, r)
		}
	}
	return refs
}

// --- Capability Inventory ---

type Capability struct {
	Name        string `json:"name"`
	Category    string `json:"category"` // "cloudflare", "memory", "self", "communication"
	Description string `json:"description"`
	Reliability string `json:"reliability"` // "high", "medium", "low", "untested"
}

func (mc *MetaCognition) GetCapabilities() []Capability {
	return []Capability{
		{Name: "cf_search", Category: "cloudflare", Description: "Search Cloudflare API spec", Reliability: "high"},
		{Name: "cf_execute", Category: "cloudflare", Description: "Execute Cloudflare API calls", Reliability: "high"},
		{Name: "r2_read", Category: "storage", Description: "Read from R2 object storage", Reliability: "high"},
		{Name: "r2_write", Category: "storage", Description: "Write to R2 object storage", Reliability: "high"},
		{Name: "memory_save", Category: "memory", Description: "Save to episodic/daily memory", Reliability: "high"},
		{Name: "memory_read", Category: "memory", Description: "Read cognitive memory", Reliability: "high"},
		{Name: "learn_fact", Category: "memory", Description: "Store semantic facts", Reliability: "high"},
		{Name: "learn_procedure", Category: "memory", Description: "Store reusable procedures", Reliability: "medium"},
		{Name: "deploy_worker", Category: "self", Description: "Deploy Cloudflare Workers", Reliability: "medium"},
		{Name: "create_kv", Category: "self", Description: "Create KV namespaces", Reliability: "medium"},
		{Name: "create_d1", Category: "self", Description: "Create D1 databases", Reliability: "medium"},
		{Name: "self_reflect", Category: "meta", Description: "Analyze own performance", Reliability: "high"},
		{Name: "set_goal", Category: "meta", Description: "Set and track goals", Reliability: "high"},
		{Name: "tokenomics", Category: "meta", Description: "Track token expenditure", Reliability: "high"},
	}
}

// BuildMetaContext returns a context string for the system prompt.
func (mc *MetaCognition) BuildMetaContext(ctx context.Context) string {
	var sb strings.Builder

	// Active goals
	goals, _ := mc.LoadGoals(ctx)
	activeGoals := 0
	for _, g := range goals {
		if g.Status == "active" {
			activeGoals++
		}
	}
	if activeGoals > 0 {
		sb.WriteString("### Active Goals\n")
		for _, g := range goals {
			if g.Status == "active" {
				sb.WriteString(fmt.Sprintf("- [P%d] %s", g.Priority, g.Description))
				if g.Progress != "" {
					sb.WriteString(fmt.Sprintf(" (%s)", g.Progress))
				}
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")
	}

	// Recent reflections (last 3)
	refs := mc.LoadRecentReflections(ctx, 3)
	if len(refs) > 0 {
		sb.WriteString("### Recent Self-Reflections\n")
		for _, r := range refs {
			sb.WriteString(fmt.Sprintf("- %s: %s → %s\n",
				r.Timestamp.Format("Jan 2"), r.Observation, r.Improvement))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
