// Package db defines the storage interface for sessions, memories and related data.
package db

import (
	"context"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// ErrConflict reports that a session changed after the caller read it.
var ErrConflict = errors.New("session revision conflict")

// ErrNotFound reports that a requested storage record does not exist.
var ErrNotFound = errors.New("storage record not found")

// ErrInvalidTransition reports a run lifecycle transition that is not allowed.
var ErrInvalidTransition = errors.New("invalid run status transition")

// ErrNamespaceStoreRequired means a tenant-scoped request reached an adapter
// that cannot prove namespace-aware persistence.
var ErrNamespaceStoreRequired = errors.New("namespace-scoped storage is required")

// SessionQuery scopes and paginates a session list before records are loaded.
// ComponentID is an agent, team, or workflow identifier depending on the
// store method. An empty field does not filter that dimension. Namespace, when
// set, restricts results to that physical partition.
type SessionQuery struct {
	ComponentID string
	Namespace   string
	UserID      string
	Limit       int
	Offset      int
}

// RunQuery scopes a direct run lookup. An empty UserID does not apply a
// tenant filter; a non-empty Namespace restricts to that partition.
type RunQuery struct {
	RunID     string
	Namespace string
	UserID    string
}

// SessionFieldsUpdate applies mutable session fields with optimistic locking.
// Nil fields are left unchanged.
type SessionFieldsUpdate struct {
	SessionID string
	// Namespace scopes this write before the session is loaded. An empty value
	// means the legacy partition for backward-compatible callers.
	Namespace        string
	ExpectedRevision uint64
	Metadata         map[string]any
	SessionState     map[string]any
	Summary          *string
}

// NamespaceFor resolves a storage namespace. A trusted context scope wins
// over a record/query field and prevents a caller from switching partitions.
// Old calls with neither scope nor namespace stay in the explicit legacy
// partition; empty never means all namespaces.
func NamespaceFor(ctx context.Context, requested string) (string, error) {
	if namespace, ok := execution.NamespaceFromContext(ctx); ok {
		if namespace == execution.ReservedNamespace {
			if requested != "" && requested != namespace {
				return "", ErrNotFound
			}
			return namespace, nil
		}
		if err := execution.ValidateNamespace(namespace); err != nil {
			return "", err
		}
		if requested != "" && requested != namespace {
			return "", ErrNotFound
		}
		return namespace, nil
	}
	if requested == "" {
		return execution.ReservedNamespace, nil
	}
	return requested, nil
}

// ScopedAgentSessionStore is an optional capability used by tenant HTTP
// routes. It keeps DB source compatibility while allowing those routes to
// fail closed when an adapter cannot prove namespace-aware CRUD.
type ScopedAgentSessionStore interface {
	CreateSessionScoped(context.Context, *session.AgentSession) error
	ReadSessionScoped(context.Context, string, string) (*session.AgentSession, error)
	UpdateSessionFieldsScoped(context.Context, SessionFieldsUpdate) (uint64, error)
	DeleteSessionScoped(context.Context, string, string) error
	ListSessionsScoped(context.Context, SessionQuery) ([]*session.AgentSession, error)
}

// ScopedRunStore is the optional capability for namespace-aware run lookup.
type ScopedRunStore interface {
	GetRunScoped(context.Context, RunQuery) (*session.RunOutput, error)
}

// NamespaceScopedStore is an optional capability marker for runtime paths
// whose existing interfaces carry scope through context rather than method
// parameters. Tenant HTTP routes must reject adapters without this marker.
type NamespaceScopedStore interface{ SupportsNamespaceScopes() }

// AgentSessionStore persists agent sessions.
type AgentSessionStore interface {
	CreateSession(ctx context.Context, s *session.AgentSession) error
	ReadSession(ctx context.Context, sessionID string) (*session.AgentSession, error)
	UpdateSession(ctx context.Context, s *session.AgentSession) error
	UpdateSessionFields(ctx context.Context, update SessionFieldsUpdate) (uint64, error)
	DeleteSession(ctx context.Context, sessionID string) error
	ListSessions(ctx context.Context, query SessionQuery) ([]*session.AgentSession, error)
}

// TeamSessionStore persists team sessions.
type TeamSessionStore interface {
	CreateTeamSession(ctx context.Context, s *session.TeamSession) error
	ReadTeamSession(ctx context.Context, sessionID string) (*session.TeamSession, error)
	UpdateTeamSession(ctx context.Context, s *session.TeamSession) error
	UpdateTeamSessionFields(ctx context.Context, update SessionFieldsUpdate) (uint64, error)
	DeleteTeamSession(ctx context.Context, sessionID string) error
	ListTeamSessions(ctx context.Context, query SessionQuery) ([]*session.TeamSession, error)
}

// WorkflowSessionStore persists workflow sessions.
type WorkflowSessionStore interface {
	CreateWorkflowSession(ctx context.Context, s *session.WorkflowSession) error
	ReadWorkflowSession(ctx context.Context, sessionID string) (*session.WorkflowSession, error)
	UpdateWorkflowSession(ctx context.Context, s *session.WorkflowSession) error
	UpdateWorkflowSessionFields(ctx context.Context, update SessionFieldsUpdate) (uint64, error)
	DeleteWorkflowSession(ctx context.Context, sessionID string) error
	ListWorkflowSessions(ctx context.Context, query SessionQuery) ([]*session.WorkflowSession, error)
}

// RunStore persists run outputs across all session kinds.
type RunStore interface {
	GetRun(ctx context.Context, query RunQuery) (*session.RunOutput, error)
	AppendRun(ctx context.Context, run *session.RunOutput) error
	UpdateRun(ctx context.Context, run *session.RunOutput, expectedStatus session.RunStatus) error
}

// MemoryStore persists user memories.
type MemoryStore interface {
	CreateMemory(ctx context.Context, m *memory.UserMemory) error
	ReadMemory(ctx context.Context, memoryID string) (*memory.UserMemory, error)
	UpdateMemory(ctx context.Context, m *memory.UserMemory) error
	GetMemories(ctx context.Context, userID, agentID string, limit int) ([]*memory.UserMemory, error)
	DeleteMemory(ctx context.Context, memoryID string) error
}

// Lifecycle exposes storage health and cleanup.
type Lifecycle interface {
	Ping(ctx context.Context) error
	Close() error
}

type AgentStore interface {
	AgentSessionStore
	RunStore
}
type TeamStore interface {
	TeamSessionStore
	RunStore
}
type WorkflowStore interface {
	WorkflowSessionStore
	RunStore
}

// Store is the full AgentOS persistence seam.
type Store interface {
	AgentStore
	TeamStore
	WorkflowStore
	MemoryStore
	Lifecycle
}

// DB is retained for source compatibility. New callers should depend on the
// narrowest capability interface they need.
type DB = Store

// RecentSessionReader is an optional capability that reads an agent session
// with only its most recent maxRuns runs (oldest first). Long-lived sessions
// otherwise load every run payload on each turn, which is O(N) per request.
// Adapters without it fall back to ReadSession.
type RecentSessionReader interface {
	ReadSessionRecent(ctx context.Context, sessionID string, maxRuns int) (*session.AgentSession, error)
}

// RunLeaseStore is an optional capability for crash recovery and
// cross-process cancellation of agent runs that are not owned by a managed
// dispatcher lease.
type RunLeaseStore interface {
	// TouchRun refreshes the heartbeat (updated_at) of a running run. It
	// returns ErrConflict when the run is no longer running, e.g. it was
	// cancelled by another process, and ErrNotFound when it does not exist.
	TouchRun(ctx context.Context, query RunQuery) error
	// ExpireStaleRuns moves at most limit running runs, across all
	// namespaces, that carry a LeaseTimeout and whose last heartbeat is older
	// than now minus that timeout, to the error status with reason, and
	// returns their run IDs. Runs without a LeaseTimeout, paused runs, and
	// pending runs are never touched.
	ExpireStaleRuns(ctx context.Context, now time.Time, reason string, limit int) ([]string, error)
}

// SessionRetentionStore is an optional capability for retention cleanup.
type SessionRetentionStore interface {
	// DeleteIdleSessions deletes, across all namespaces and session kinds,
	// at most limit sessions last updated before cutoff that have no
	// pending, running, or paused run, together with their runs, and
	// returns how many sessions it deleted.
	DeleteIdleSessions(ctx context.Context, cutoff time.Time, limit int) (int, error)
}
