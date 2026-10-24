package tools

import "time"

// ApprovalKind declares whether a tool needs approval before execution.
type ApprovalKind string

const (
	ApprovalNone     ApprovalKind = "none"
	ApprovalRequired ApprovalKind = "required"
	ApprovalAudit    ApprovalKind = "audit"
)

// ApprovalAware is an optional interface tools can implement to declare
// their approval requirements. Tools that do not implement it default to
// ApprovalNone (no approval needed).
type ApprovalAware interface {
	Approval() ApprovalKind
}

// ApprovalDeadlineAware optionally supplies a relative deadline for approval.
type ApprovalDeadlineAware interface {
	ApprovalTimeout() time.Duration
}

// ApprovalOf returns the approval kind for a tool, defaulting to ApprovalNone
// for tools that do not implement ApprovalAware.
func ApprovalOf(tool Tool) ApprovalKind {
	if a, ok := tool.(ApprovalAware); ok {
		return a.Approval()
	}
	return ApprovalNone
}

// ApprovalTimeoutOf returns the configured approval timeout.
func ApprovalTimeoutOf(tool Tool) time.Duration {
	if aware, ok := tool.(ApprovalDeadlineAware); ok {
		return aware.ApprovalTimeout()
	}
	return 0
}

// ApprovalOption configures an approval wrapper.
type ApprovalOption func(*approvalTool)

// WithApprovalTimeout adds a relative deadline to approval requirements.
func WithApprovalTimeout(timeout time.Duration) ApprovalOption {
	return func(tool *approvalTool) { tool.timeout = timeout }
}

// WithApproval decorates a Tool without changing its schema or call behavior.
func WithApproval(tool Tool, kind ApprovalKind, opts ...ApprovalOption) Tool {
	wrapped := &approvalTool{Tool: tool, kind: kind}
	for _, opt := range opts {
		if opt != nil {
			opt(wrapped)
		}
	}
	return wrapped
}

type approvalTool struct {
	Tool
	kind    ApprovalKind
	timeout time.Duration
}

func (t *approvalTool) Approval() ApprovalKind         { return t.kind }
func (t *approvalTool) ApprovalTimeout() time.Duration { return t.timeout }
func (t *approvalTool) ReadOnly() bool                 { return IsReadOnly(t.Tool) }
