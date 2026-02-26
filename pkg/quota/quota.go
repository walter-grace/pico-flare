// Package quota provides per-agent usage tracking and limits.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bigneek/picoflare/pkg/storage"
)

// Usage holds per-agent usage stats.
type Usage struct {
	AgentID          string    `json:"agent_id"`
	Messages         int64     `json:"messages"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	ToolCalls        int64     `json:"tool_calls"`
	StorageBytes     int64     `json:"storage_bytes"`
	LastUsed         time.Time `json:"last_used"`
	CreatedAt        time.Time `json:"created_at"`
}

// Limits defines per-agent quotas. Zero = unlimited.
type Limits struct {
	MaxMessages         int64
	MaxPromptTokens     int64
	MaxCompletionTokens int64
	MaxToolCalls        int64
	MaxStorageBytes     int64
}

// Manager tracks and enforces per-agent quotas.
type Manager struct {
	r2     *storage.R2Client
	bucket string
	limits Limits
	mu     sync.Mutex
	cache  map[string]*Usage
}

// NewManager creates a quota manager.
func NewManager(r2 *storage.R2Client, bucket string, limits Limits) *Manager {
	return &Manager{
		r2:     r2,
		bucket: bucket,
		limits: limits,
		cache:  make(map[string]*Usage),
	}
}

func (m *Manager) key(agentID string) string {
	return fmt.Sprintf("agents/%s/quota.json", agentID)
}

// Load loads usage for an agent from R2.
func (m *Manager) Load(ctx context.Context, agentID string) (*Usage, error) {
	if m.r2 == nil {
		return &Usage{AgentID: agentID, CreatedAt: time.Now()}, nil
	}
	data, err := m.r2.DownloadObject(ctx, m.bucket, m.key(agentID))
	if err != nil {
		return &Usage{AgentID: agentID, CreatedAt: time.Now()}, nil
	}
	var u Usage
	if err := json.Unmarshal(data, &u); err != nil {
		return &Usage{AgentID: agentID, CreatedAt: time.Now()}, nil
	}
	return &u, nil
}

// Save persists usage to R2.
func (m *Manager) Save(ctx context.Context, u *Usage) error {
	if m.r2 == nil {
		return nil
	}
	u.LastUsed = time.Now()
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return m.r2.UploadObject(ctx, m.bucket, m.key(u.AgentID), data)
}

// Check returns an error if the agent would exceed limits after the given delta.
func (m *Manager) Check(ctx context.Context, agentID string, delta Usage) error {
	u, err := m.Load(ctx, agentID)
	if err != nil {
		return err
	}
	if m.limits.MaxMessages > 0 && u.Messages+delta.Messages > m.limits.MaxMessages {
		return fmt.Errorf("quota exceeded: messages (%d/%d)", u.Messages+delta.Messages, m.limits.MaxMessages)
	}
	if m.limits.MaxPromptTokens > 0 && u.PromptTokens+delta.PromptTokens > m.limits.MaxPromptTokens {
		return fmt.Errorf("quota exceeded: prompt tokens (%d/%d)", u.PromptTokens+delta.PromptTokens, m.limits.MaxPromptTokens)
	}
	if m.limits.MaxCompletionTokens > 0 && u.CompletionTokens+delta.CompletionTokens > m.limits.MaxCompletionTokens {
		return fmt.Errorf("quota exceeded: completion tokens (%d/%d)", u.CompletionTokens+delta.CompletionTokens, m.limits.MaxCompletionTokens)
	}
	if m.limits.MaxToolCalls > 0 && u.ToolCalls+delta.ToolCalls > m.limits.MaxToolCalls {
		return fmt.Errorf("quota exceeded: tool calls (%d/%d)", u.ToolCalls+delta.ToolCalls, m.limits.MaxToolCalls)
	}
	if m.limits.MaxStorageBytes > 0 && u.StorageBytes+delta.StorageBytes > m.limits.MaxStorageBytes {
		return fmt.Errorf("quota exceeded: storage (%d/%d bytes)", u.StorageBytes+delta.StorageBytes, m.limits.MaxStorageBytes)
	}
	return nil
}

// Record adds usage and persists.
func (m *Manager) Record(ctx context.Context, agentID string, delta Usage) error {
	u, err := m.Load(ctx, agentID)
	if err != nil {
		return err
	}
	u.Messages += delta.Messages
	u.PromptTokens += delta.PromptTokens
	u.CompletionTokens += delta.CompletionTokens
	u.ToolCalls += delta.ToolCalls
	u.StorageBytes += delta.StorageBytes
	u.LastUsed = time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	return m.Save(ctx, u)
}
