// Package service defines the managed tool execution contract: the
// ToolService is the single governed entry point for all tool invocation
// (Agent managed mode, Team, Workflow, Skill adapter, MCP server, internal
// REST Tools API). Executors are held privately by the catalog registry;
// everything else talks to ToolService.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
)

// PlanRef is the opaque client-side handle to a server-persisted
// ExecutionPlan. Clients never receive or echo the full plan.
type PlanRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// PrepareRequest asks to freeze one tool invocation.
type PrepareRequest struct {
	ToolName       string
	Arguments      json.RawMessage
	IdempotencyKey string
}

// PreparedExecution is the server-side result of Prepare: an opaque ref
// plus a display summary. No raw arguments or revisions leak to clients.
type PreparedExecution struct {
	Ref      PlanRef
	ToolName string
	// ApprovalRequired reports whether execution needs approval first.
	ApprovalRequired bool
	// Display is a safe, non-sensitive summary (e.g. "tool(args-hash)").
	Display string
}

// ApprovalSpec captures the approval posture frozen into a plan.
type ApprovalSpec struct {
	Required     bool
	Approver     string
	ApprovalKind string
	// Granted is set by the approval flow (RC-06) once a valid grant
	// exists; until then Required plans refuse to execute.
	Granted bool
}

// ExecutionPlan is the server-persisted, authority-bearing record of one
// tool invocation. All identity, revision, and argument data is frozen at
// Prepare time; Execute re-validates against the trusted principal.
type ExecutionPlan struct {
	ID        string
	Version   string
	ToolName  string
	Arguments json.RawMessage
	// ArgumentsHash is the canonical-JSON hash of Arguments.
	ArgumentsHash string

	RegistryRevision            string
	DescriptorPolicyRevision    string
	AuthorizationPolicyRevision string
	AuthorizationHash           string
	IdentityBindingHash         string

	Principal      execution.Principal
	Context        execution.ExecutionContext
	IdempotencyKey string
	Approval       ApprovalSpec

	PreparedAt time.Time
	ExpiresAt  time.Time
}

// ToolResult is the result of an execution.
type ToolResult struct {
	ExecutionID string     `json:"execution_id,omitempty"`
	Status      string     `json:"status"`
	Content     any        `json:"content,omitempty"`
	IsError     bool       `json:"is_error,omitempty"`
	Error       *ToolError `json:"error,omitempty"`
	Replay      bool       `json:"replay,omitempty"`
	StartedAt   time.Time  `json:"started_at,omitempty"`
	FinishedAt  time.Time  `json:"finished_at,omitempty"`
}

// ToolError is a stable, structured tool error. The raw upstream body is
// never written to logs; Body is a truncated field for trusted callers.
type ToolError struct {
	Code              string
	HTTPStatus        int
	Retryable         bool
	RetryAfter        string
	UpstreamRequestID string
	Message           string
}

// Sentinel errors.
var (
	ErrUnknownTool  = errors.New("service: unknown tool")
	ErrPlanNotFound = errors.New("service: plan not found")
	ErrPlanStale    = errors.New("service: plan revision is stale")
	// ErrPlanIdentityMismatch rejects both legacy plans without an identity
	// binding and plans presented by a different trusted principal. It wraps
	// ErrPlanStale so existing callers that re-prepare stale plans keep working.
	ErrPlanIdentityMismatch = fmt.Errorf("%w: identity binding mismatch", ErrPlanStale)
	// ErrPlanAuthorizationStale distinguishes a fresh authorizer decision made
	// under a different (or newly versioned) policy from other stale-plan causes.
	ErrPlanAuthorizationStale = fmt.Errorf("%w: authorization policy changed", ErrPlanStale)
	// ErrPlanApprovalStale reports a change in approval requiredness or kind.
	ErrPlanApprovalStale     = fmt.Errorf("%w: approval policy changed", ErrPlanStale)
	ErrPlanExpired           = errors.New("service: plan expired")
	ErrPlanNamespaceMismatch = errors.New("service: plan namespace mismatch")
	ErrForbidden             = errors.New("service: forbidden")
)

// ToolQuery filters a tool listing.
type ToolQuery struct {
	Namespace string
	Cursor    string
	Limit     int
}

// ToolPage is a page of descriptors.
type ToolPage struct {
	Tools      []catalog.ToolDescriptor
	NextCursor string
}

// ToolService is the single governed execution entry point.
type ToolService interface {
	List(ctx context.Context, query ToolQuery) (ToolPage, error)
	Describe(ctx context.Context, name string) (catalog.ToolDescriptor, error)
	Prepare(ctx context.Context, req PrepareRequest) (PreparedExecution, error)
	Execute(ctx context.Context, ref PlanRef) (ToolResult, error)
}

// PlanStore persists ExecutionPlans partitioned by namespace. Migration
// 2026080902 provides durable adapters; the in-memory implementation is the
// single-process contract reference.
type PlanStore interface {
	Create(ctx context.Context, plan *ExecutionPlan) error
	Get(ctx context.Context, scope execution.NamespaceScope, id string) (*ExecutionPlan, error)
}

// CanonicalHash returns the sha256 of canonical JSON bytes (deterministic
// encoding is the caller's responsibility).
func CanonicalHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// memoryPlanStore is the reference in-memory PlanStore.
type memoryPlanStore struct {
	mu    sync.RWMutex
	plans map[string]*ExecutionPlan // key: namespace\x00id
}

// NewMemoryPlanStore creates an in-memory PlanStore.
func NewMemoryPlanStore() PlanStore {
	return &memoryPlanStore{plans: make(map[string]*ExecutionPlan)}
}

func planKey(scope execution.NamespaceScope, id string) string {
	ns := scope.Namespace
	if ns == "" {
		ns = execution.ReservedNamespace
	}
	return ns + "\x00" + id
}

func (s *memoryPlanStore) Create(ctx context.Context, plan *ExecutionPlan) error {
	if plan == nil || plan.ID == "" {
		return errors.New("service: plan id is required")
	}
	if err := execution.ValidateNamespace(plan.Principal.Namespace); err != nil {
		return fmt.Errorf("service: plan namespace: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := planKey(execution.NamespaceScope{Namespace: plan.Principal.Namespace}, plan.ID)
	if _, exists := s.plans[key]; exists {
		return errors.New("service: plan already exists")
	}
	copy := *plan
	s.plans[key] = &copy
	return nil
}

func (s *memoryPlanStore) Get(ctx context.Context, scope execution.NamespaceScope, id string) (*ExecutionPlan, error) {
	if scope.Namespace == "" {
		return nil, errors.New("service: namespace scope is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	plan, ok := s.plans[planKey(scope, id)]
	if !ok {
		return nil, ErrPlanNotFound
	}
	copy := *plan
	return &copy, nil
}
