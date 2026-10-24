package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
)

func approvalPlan(t *testing.T, ns string) (*ExecutionPlan, error) {
	t.Helper()
	return &ExecutionPlan{
		ID: "plan-1", Version: "v1", ToolName: "t",
		Arguments:                   json.RawMessage(`{}`),
		ArgumentsHash:               "arg-hash",
		RegistryRevision:            "reg-1",
		DescriptorPolicyRevision:    "dp-1",
		AuthorizationPolicyRevision: "ap-1",
		Principal:                   execution.Principal{Subject: "requester", Namespace: ns},
		Approval:                    ApprovalSpec{Required: true},
	}, nil
}

func TestApprovalResolveAndGrant(t *testing.T) {
	store := NewMemoryApprovalStore()
	svc := NewApprovalService(store, nil)
	plan, _ := approvalPlan(t, "ns")

	grant, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "approve", "nonce-1", time.Hour)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if grant.Decision != "approve" || grant.Approver != "approver" || grant.Requester != "requester" {
		t.Fatalf("grant = %+v", grant)
	}
	got, err := svc.GrantFor(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan)
	if err != nil {
		t.Fatalf("grantfor: %v", err)
	}
	if got.Nonce != "nonce-1" {
		t.Fatalf("nonce = %q", got.Nonce)
	}
}

func TestApprovalDeniesSelfApproval(t *testing.T) {
	svc := NewApprovalService(NewMemoryApprovalStore(), nil)
	plan, _ := approvalPlan(t, "ns")
	_, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "requester", "approve", "n", time.Hour)
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("err = %v, want self-approval rejection", err)
	}
}

func TestApprovalDenyPreventsExecution(t *testing.T) {
	svc := NewApprovalService(NewMemoryApprovalStore(), nil)
	plan, _ := approvalPlan(t, "ns")
	if _, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "deny", "n", time.Hour); err != nil {
		t.Fatalf("resolve deny: %v", err)
	}
	if _, err := svc.GrantFor(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("denied plan must not be granted: %v", err)
	}
}

func TestApprovalIdempotentSameDecision(t *testing.T) {
	store := NewMemoryApprovalStore()
	svc := NewApprovalService(store, nil)
	plan, _ := approvalPlan(t, "ns")
	if _, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "approve", "n", time.Hour); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Same decision + same nonce: idempotent, no error.
	if _, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "approve", "n", time.Hour); err != nil {
		t.Fatalf("idempotent re-resolve: %v", err)
	}
}

func TestApprovalDifferentDecisionConflicts(t *testing.T) {
	store := NewMemoryApprovalStore()
	svc := NewApprovalService(store, nil)
	plan, _ := approvalPlan(t, "ns")
	if _, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "approve", "n1", time.Hour); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "deny", "n2", time.Hour); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
}

func TestApprovalGrantExpired(t *testing.T) {
	store := NewMemoryApprovalStore()
	svc := NewApprovalService(store, nil)
	base := time.Now().UTC()
	svc.SetClock(func() time.Time { return base })
	plan, _ := approvalPlan(t, "ns")
	if _, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "approve", "n", time.Hour); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	svc.SetClock(func() time.Time { return base.Add(2 * time.Hour) })
	if _, err := svc.GrantFor(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("err = %v, want expired", err)
	}
}

func TestApprovalGrantStaleOnRevisionChange(t *testing.T) {
	store := NewMemoryApprovalStore()
	svc := NewApprovalService(store, nil)
	plan, _ := approvalPlan(t, "ns")
	if _, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "approve", "n", time.Hour); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Catalog changed after approval: registry revision differs.
	plan.RegistryRevision = "reg-2"
	if _, err := svc.GrantFor(context.Background(), execution.NamespaceScope{Namespace: "ns"}, plan); !errors.Is(err, ErrGrantStale) {
		t.Fatalf("err = %v, want stale", err)
	}
}

func TestApprovalNamespaceIsolation(t *testing.T) {
	store := NewMemoryApprovalStore()
	svc := NewApprovalService(store, nil)
	plan, _ := approvalPlan(t, "ns-a")
	if _, err := svc.Resolve(context.Background(), execution.NamespaceScope{Namespace: "ns-a"}, plan, "approver", "approve", "n", time.Hour); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := svc.GrantFor(context.Background(), execution.NamespaceScope{Namespace: "ns-b"}, plan); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("cross-namespace grant must not be visible: %v", err)
	}
}

// End-to-end: approval-required plan executes only after a valid grant.
func TestServiceExecuteAfterApproval(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(true), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(true), nil
		},
	}, "approved-done")
	approvals := NewApprovalService(NewMemoryApprovalStore(), nil)
	s.SetApprovalService(approvals)
	ctx := ctxWithPrincipal("ns")

	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Without a grant, execution refuses.
	if _, err := s.Execute(ctx, prepared.Ref); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("pre-grant execute err = %v", err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool ran before approval")
	}
	// Grant then execute.
	plan, err := s.plans.(*memoryPlanStore).Get(ctx, execution.NamespaceScope{Namespace: "ns"}, prepared.Ref.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if _, err := approvals.Resolve(ctx, execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "approve", "nonce", time.Hour); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	result, err := s.Execute(ctx, prepared.Ref)
	if err != nil {
		t.Fatalf("post-grant execute: %v", err)
	}
	if result.Content != "approved-done" || tool.calls.Load() != 1 {
		t.Fatalf("result = %+v, calls = %d", result, tool.calls.Load())
	}
}
