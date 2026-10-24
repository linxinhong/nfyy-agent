package service

import (
	"context"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

// Approver is the optional managed-approval capability a ToolService exposes
// (RC-06). An agent or host resolves an approval-required frozen plan by
// creating the grant that lets Execute proceed.
type Approver interface {
	// ResolveApproval records an approval decision for a frozen plan. The
	// trusted principal (and its namespace) is read from ctx.
	ResolveApproval(ctx context.Context, planID string, approver, decision string) error
}

// ResolveApproval implements Approver. It loads the frozen plan under the
// principal's namespace and records the decision via the ApprovalService.
func (s *Service) ResolveApproval(ctx context.Context, planID string, approver, decision string) error {
	principal, err := PrincipalFromContext(ctx)
	if err != nil {
		return err
	}
	scope := execution.NamespaceScope{Namespace: principal.Namespace}
	plan, err := s.plans.Get(ctx, scope, planID)
	if err != nil {
		return err
	}
	if s.approvals == nil {
		return ErrApprovalRequired
	}
	ttl := time.Until(plan.ExpiresAt)
	if ttl <= 0 {
		return ErrPlanExpired
	}
	_, err = s.approvals.Resolve(ctx, scope, plan, approver, decision, "resume", ttl)
	return err
}

var _ Approver = (*Service)(nil)
