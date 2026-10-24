// Package builtin provides ready-made tools for Agno agents.
package builtin

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// RunContext carries agent-runtime dependencies into tool calls via context.
type RunContext struct {
	Session     *session.AgentSession
	Knowledge   knowledge.Knowledge
	Memory      memory.Manager
	AgentID     string
	UserID      string
	RunID       string
	SessionID   string
	ParentRunID string
	NodeID      string
	Metadata    map[string]string
}

type runCtxKey struct{}

// ContextWithRunContext attaches a RunContext to a context.Context.
func ContextWithRunContext(ctx context.Context, rc *RunContext) context.Context {
	if rc == nil {
		return context.WithValue(ctx, runCtxKey{}, (*RunContext)(nil))
	}
	clone := *rc
	clone.Metadata, _ = session.CloneRunMetadata(rc.Metadata)
	return context.WithValue(ctx, runCtxKey{}, &clone)
}

// RunContextFromContext retrieves a RunContext previously attached with
// ContextWithRunContext.
func RunContextFromContext(ctx context.Context) (*RunContext, bool) {
	rc, ok := ctx.Value(runCtxKey{}).(*RunContext)
	if !ok || rc == nil {
		return nil, false
	}
	clone := *rc
	clone.Metadata, _ = session.CloneRunMetadata(rc.Metadata)
	return &clone, true
}
