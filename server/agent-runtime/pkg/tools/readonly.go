package tools

import "time"

// ReadOnlyAware is an optional interface for tools to declare they have no
// side effects. Read-only, approval-free tools may run concurrently within
// one model round when the agent enables MaxParallelTools. The declaration
// is trusted, so only hosts (never remote MCP hints) should make it.
type ReadOnlyAware interface {
	ReadOnly() bool
}

// IsReadOnly reports whether tool declares itself read-only.
func IsReadOnly(tool Tool) bool {
	aware, ok := tool.(ReadOnlyAware)
	return ok && aware.ReadOnly()
}

// WithReadOnly marks tool as side-effect free without changing its schema
// or behavior. Approval declarations of the wrapped tool are preserved.
func WithReadOnly(tool Tool) Tool {
	return &readOnlyTool{Tool: tool}
}

type readOnlyTool struct {
	Tool
}

func (t *readOnlyTool) ReadOnly() bool                 { return true }
func (t *readOnlyTool) Approval() ApprovalKind         { return ApprovalOf(t.Tool) }
func (t *readOnlyTool) ApprovalTimeout() time.Duration { return ApprovalTimeoutOf(t.Tool) }

// IdempotentAware is an optional interface for tools whose repeated
// execution with the same arguments has the same effect as one execution.
// When a crashed run is recovered, idempotent tool calls whose outcome is
// unknown are executed again; others are reported to the model as
// interrupted instead of risking a duplicate side effect.
type IdempotentAware interface {
	Idempotent() bool
}

// IsIdempotent reports whether tool may be safely re-executed: it declares
// itself idempotent or read-only.
func IsIdempotent(tool Tool) bool {
	if aware, ok := tool.(IdempotentAware); ok && aware.Idempotent() {
		return true
	}
	return IsReadOnly(tool)
}

// WithIdempotent marks tool as safe to re-execute without changing its
// schema or behavior. Read-only and approval declarations are preserved.
func WithIdempotent(tool Tool) Tool {
	return &idempotentTool{Tool: tool}
}

type idempotentTool struct {
	Tool
}

func (t *idempotentTool) Idempotent() bool               { return true }
func (t *idempotentTool) ReadOnly() bool                 { return IsReadOnly(t.Tool) }
func (t *idempotentTool) Approval() ApprovalKind         { return ApprovalOf(t.Tool) }
func (t *idempotentTool) ApprovalTimeout() time.Duration { return ApprovalTimeoutOf(t.Tool) }
