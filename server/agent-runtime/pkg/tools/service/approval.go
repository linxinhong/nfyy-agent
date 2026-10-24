package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

// ApprovalGrant binds one approval decision to a frozen ExecutionPlan:
// namespace, plan id/version, tool, arguments hash, the three execution
// revisions, requester, approver, decision, nonce, and expiry. A grant is
// the only thing that lets an approval-required plan execute.
type ApprovalGrant struct {
	GrantID   string
	Namespace string

	PlanID        string
	PlanVersion   string
	ToolName      string
	ArgumentsHash string

	RegistryRevision            string
	DescriptorPolicyRevision    string
	AuthorizationPolicyRevision string

	Requester string
	Approver  string
	Decision  string // "approve" | "deny"
	Nonce     string

	IssuedAt  time.Time
	ExpiresAt time.Time
}

// ApprovalStore persists decisions per plan, partitioned by namespace.
type ApprovalStore interface {
	// Resolve records a decision. Re-resolving with the same decision is
	// idempotent (returns created=false); a different decision for the same
	// plan conflicts.
	Resolve(ctx context.Context, scope execution.NamespaceScope, grant *ApprovalGrant) (bool, error)
	// Get returns the recorded decision for a plan, or nil when none.
	Get(ctx context.Context, scope execution.NamespaceScope, planID string) (*ApprovalGrant, error)
}

// ApprovalAuthorizer decides whether an actor may approve for a requester.
// The default policy forbids self-approval; hosts may supply their own.
type ApprovalAuthorizer interface {
	// CanApprove reports whether approver may decide a plan requested by
	// requester.
	CanApprove(ctx context.Context, approver, requester string) (bool, error)
}

// DefaultApprovalPolicy forbids requester self-approval (RC-06 step 4).
type DefaultApprovalPolicy struct{}

func (DefaultApprovalPolicy) CanApprove(ctx context.Context, approver, requester string) (bool, error) {
	return approver != requester, nil
}

// ErrSelfApproval is returned when the requester tries to approve their
// own plan under the default policy.
var ErrSelfApproval = errors.New("service: requester cannot approve their own plan")

// ErrApprovalConflict is returned when the same plan receives a different
// decision.
var ErrApprovalConflict = errors.New("service: conflicting approval decision")

// ErrGrantExpired is returned when a grant is past its expiry.
var ErrGrantExpired = errors.New("service: approval grant expired")

// ErrGrantStale is returned when a grant no longer matches the plan's
// revisions (catalog/policy changed after approval).
var ErrGrantStale = errors.New("service: approval grant is stale")

// memoryApprovalStore is the reference in-memory ApprovalStore.
type memoryApprovalStore struct {
	mu     sync.RWMutex
	grants map[string]*ApprovalGrant // key: namespace\x00planID
}

// NewMemoryApprovalStore creates an in-memory ApprovalStore.
func NewMemoryApprovalStore() ApprovalStore {
	return &memoryApprovalStore{grants: make(map[string]*ApprovalGrant)}
}

func (s *memoryApprovalStore) Resolve(ctx context.Context, scope execution.NamespaceScope, grant *ApprovalGrant) (bool, error) {
	if grant == nil || grant.PlanID == "" {
		return false, errors.New("service: grant plan id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := planKey(scope, grant.PlanID)
	if existing, ok := s.grants[key]; ok {
		if existing.Decision == grant.Decision && existing.Nonce == grant.Nonce {
			return false, nil // idempotent
		}
		return false, ErrApprovalConflict
	}
	copy := *grant
	s.grants[key] = &copy
	return true, nil
}

func (s *memoryApprovalStore) Get(ctx context.Context, scope execution.NamespaceScope, planID string) (*ApprovalGrant, error) {
	if scope.Namespace == "" {
		return nil, errors.New("service: namespace scope is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	grant, ok := s.grants[planKey(scope, planID)]
	if !ok {
		return nil, nil
	}
	copy := *grant
	return &copy, nil
}

// ApprovalService coordinates plan approval decisions.
type ApprovalService struct {
	store      ApprovalStore
	authorizer ApprovalAuthorizer
	clock      Clock
}

// NewApprovalService creates an ApprovalService. A nil authorizer uses the
// default no-self-approval policy.
func NewApprovalService(store ApprovalStore, authorizer ApprovalAuthorizer) *ApprovalService {
	if authorizer == nil {
		authorizer = DefaultApprovalPolicy{}
	}
	return &ApprovalService{store: store, authorizer: authorizer, clock: Now}
}

// SetClock injects a clock for grant-expiry tests.
func (s *ApprovalService) SetClock(clock Clock) *ApprovalService {
	if clock != nil {
		s.clock = clock
	}
	return s
}

// Resolve records an approval or denial for a plan. approve produces a
// valid grant; deny records a refusal (and no grant is ever produced for
// that plan).
func (s *ApprovalService) Resolve(ctx context.Context, scope execution.NamespaceScope, plan *ExecutionPlan, approver, decision, nonce string, ttl time.Duration) (*ApprovalGrant, error) {
	if decision != "approve" && decision != "deny" {
		return nil, fmt.Errorf("service: unknown approval decision %q", decision)
	}
	allowed, err := s.authorizer.CanApprove(ctx, approver, plan.Principal.Subject)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrSelfApproval
	}
	now := s.clock()
	grant := &ApprovalGrant{
		GrantID:                     fmt.Sprintf("grant-%d", now.UnixNano()),
		Namespace:                   scope.Namespace,
		PlanID:                      plan.ID,
		PlanVersion:                 plan.Version,
		ToolName:                    plan.ToolName,
		ArgumentsHash:               plan.ArgumentsHash,
		RegistryRevision:            plan.RegistryRevision,
		DescriptorPolicyRevision:    plan.DescriptorPolicyRevision,
		AuthorizationPolicyRevision: plan.AuthorizationPolicyRevision,
		Requester:                   plan.Principal.Subject,
		Approver:                    approver,
		Decision:                    decision,
		Nonce:                       nonce,
		IssuedAt:                    now,
		ExpiresAt:                   now.Add(ttl),
	}
	created, err := s.store.Resolve(ctx, scope, grant)
	if err != nil {
		return nil, err
	}
	if !created {
		// Idempotent same-decision re-resolve: return the stored grant.
		return s.store.Get(ctx, scope, plan.ID)
	}
	return grant, nil
}

// GrantFor validates that an approval-required plan has a live, matching
// grant. It re-checks revisions so catalog/policy changes invalidate old
// approvals (RC-06 step 3).
func (s *ApprovalService) GrantFor(ctx context.Context, scope execution.NamespaceScope, plan *ExecutionPlan) (*ApprovalGrant, error) {
	grant, err := s.store.Get(ctx, scope, plan.ID)
	if err != nil {
		return nil, err
	}
	if grant == nil || grant.Decision != "approve" {
		return nil, ErrApprovalRequired
	}
	if grant.PlanVersion != plan.Version ||
		grant.ArgumentsHash != plan.ArgumentsHash ||
		grant.RegistryRevision != plan.RegistryRevision ||
		grant.DescriptorPolicyRevision != plan.DescriptorPolicyRevision ||
		grant.AuthorizationPolicyRevision != plan.AuthorizationPolicyRevision {
		return nil, ErrGrantStale
	}
	if !s.clock().Before(grant.ExpiresAt) {
		return nil, ErrGrantExpired
	}
	return grant, nil
}
