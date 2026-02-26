// Package agentctx provides context keys for per-agent isolation.
package agentctx

import (
	"context"
	"fmt"
)

type agentIDKey struct{}

// WithAgentID attaches agentID to the context for per-agent memory, FS, quota.
func WithAgentID(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, agentIDKey{}, agentID)
}

// AgentIDFromContext returns the agent ID from context, if set.
func AgentIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(agentIDKey{}).(string)
	return v, ok
}

// FormatAgentID converts chatID to a stable agent ID string.
func FormatAgentID(chatID int64) string {
	return fmt.Sprintf("chat-%d", chatID)
}
