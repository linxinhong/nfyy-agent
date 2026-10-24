package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
)

// countTool records invocations.
type countTool struct {
	name   string
	calls  atomic.Int32
	result string
}

func (t *countTool) Name() string           { return t.name }
func (t *countTool) Description() string    { return "d" }
func (t *countTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *countTool) Call(ctx context.Context, args map[string]any) (any, error) {
	t.calls.Add(1)
	if t.result != "" {
		return t.result, nil
	}
	return map[string]any{"echo": args}, nil
}

// scriptedAuthorizer returns a per-call decision via funcs.
type scriptedAuthorizer struct {
	discovery func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error)
	execute   func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error)
	execCalls atomic.Int32
}

func (a *scriptedAuthorizer) AuthorizeDiscovery(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
	return a.discovery(ctx, p, d)
}
func (a *scriptedAuthorizer) Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
	a.execCalls.Add(1)
	return a.execute(ctx, req)
}

func allow(required bool) AuthorizationDecision {
	return AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1", ApprovalRequired: required}
}

func buildService(t *testing.T, auth ToolAuthorizer, result string) (*Service, *countTool) {
	t.Helper()
	tool := &countTool{name: "t", result: result}
	b := catalog.NewBuilder()
	desc := catalog.ToolDescriptor{
		Name: "t", Description: "d", InputSchema: map[string]any{"type": "object"},
		ReadOnly: catalog.TriStateTrue, Source: catalog.Source{Kind: "local", Name: "test"},
	}
	if err := b.Add(desc, tool); err != nil {
		t.Fatalf("add: %v", err)
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	s := New(reg, NewMemoryPlanStore(), auth)
	return s, tool
}

func ctxWithPrincipal(ns string) context.Context {
	return ContextWithPrincipal(context.Background(), execution.Principal{Subject: "alice", Namespace: ns})
}

func fullPrincipal(subject string) execution.Principal {
	return execution.Principal{Subject: subject, Namespace: "ns", Issuer: "test-idp", AuthzEpoch: 7}
}

func TestServiceNilAuthorizerFailsClosed(t *testing.T) {
	s, tool := buildService(t, nil, "")
	ctx := ctxWithPrincipal("ns")
	// Discovery is filtered: nothing visible under deny-all.
	page, err := s.List(ctx, ToolQuery{})
	if err != nil || len(page.Tools) != 0 {
		t.Fatalf("list = %+v, err = %v, want empty", page, err)
	}
	if _, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("prepare err = %v, want forbidden", err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool must not run under deny-all")
	}
}

func TestServicePrepareExecuteHappyPath(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "done")
	ctx := ctxWithPrincipal("ns")

	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t", Arguments: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.Ref.ID == "" || prepared.Display == "" || prepared.Display == "t()" {
		t.Fatalf("prepared = %+v", prepared)
	}
	result, err := s.Execute(ctx, prepared.Ref)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Status != "succeeded" || result.Content != "done" {
		t.Fatalf("result = %+v", result)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want 1", tool.calls.Load())
	}
}

func TestServiceExecuteBindsFullTrustedIdentity(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "done")
	preparedFor := fullPrincipal("alice")
	prepared, err := s.Prepare(ContextWithPrincipal(context.Background(), preparedFor), PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	changed := []struct {
		name      string
		principal execution.Principal
	}{
		{name: "same_namespace_other_subject", principal: fullPrincipal("bob")},
		{name: "issuer_changed", principal: execution.Principal{Subject: "alice", Namespace: "ns", Issuer: "other-idp", AuthzEpoch: 7}},
		{name: "authz_epoch_changed", principal: execution.Principal{Subject: "alice", Namespace: "ns", Issuer: "test-idp", AuthzEpoch: 8}},
	}
	for _, test := range changed {
		t.Run(test.name, func(t *testing.T) {
			_, executeErr := s.Execute(ContextWithPrincipal(context.Background(), test.principal), prepared.Ref)
			if !errors.Is(executeErr, ErrPlanIdentityMismatch) || !errors.Is(executeErr, ErrPlanStale) {
				t.Fatalf("execute err = %v, want identity mismatch/stale", executeErr)
			}
			if tool.calls.Load() != 0 {
				t.Fatalf("tool calls = %d, want 0", tool.calls.Load())
			}
		})
	}

	result, err := s.Execute(ContextWithPrincipal(context.Background(), preparedFor), prepared.Ref)
	if err != nil || result.Status != "succeeded" {
		t.Fatalf("same identity execute = %+v, %v", result, err)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want 1", tool.calls.Load())
	}
}

func TestServiceExecuteRejectsMissingIdentityBinding(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "")
	ctx := ctxWithPrincipal("ns")
	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	store := s.plans.(*memoryPlanStore)
	store.mu.Lock()
	store.plans[planKey(execution.NamespaceScope{Namespace: "ns"}, prepared.Ref.ID)].IdentityBindingHash = ""
	store.mu.Unlock()

	if _, err := s.Execute(ctx, prepared.Ref); !errors.Is(err, ErrPlanIdentityMismatch) {
		t.Fatalf("execute err = %v, want identity mismatch", err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool calls = %d, want 0", tool.calls.Load())
	}
}

func TestServiceIdentityBindingAllowsAuditOnlyEmptySubject(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "done")
	principal := execution.Principal{Namespace: "ns", Issuer: "audit-source"}
	ctx := ContextWithPrincipal(context.Background(), principal)
	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := s.Execute(ctx, prepared.Ref); err != nil {
		t.Fatalf("execute same audit-only identity: %v", err)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want 1", tool.calls.Load())
	}
}

func TestServiceExecuteRequiresFreshAuthorizationRevision(t *testing.T) {
	tests := []struct {
		name      string
		prepared  string
		fresh     string
		wantStale bool
	}{
		{name: "same_version", prepared: "pol-1", fresh: "pol-1"},
		{name: "both_unversioned", prepared: "", fresh: ""},
		{name: "version_changed", prepared: "pol-1", fresh: "pol-2", wantStale: true},
		{name: "version_removed", prepared: "pol-1", fresh: "", wantStale: true},
		{name: "version_introduced", prepared: "", fresh: "pol-2", wantStale: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			revision := test.prepared
			authorizer := &scriptedAuthorizer{
				discovery: func(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
					return AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: revision}, nil
				},
				execute: func(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
					return AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: revision}, nil
				},
			}
			s, tool := buildService(t, authorizer, "done")
			ctx := ctxWithPrincipal("ns")
			prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			revision = test.fresh
			_, executeErr := s.Execute(ctx, prepared.Ref)
			if test.wantStale {
				if !errors.Is(executeErr, ErrPlanAuthorizationStale) || !errors.Is(executeErr, ErrPlanStale) {
					t.Fatalf("execute err = %v, want authorization stale", executeErr)
				}
				if tool.calls.Load() != 0 {
					t.Fatalf("tool calls = %d, want 0", tool.calls.Load())
				}
				return
			}
			if executeErr != nil {
				t.Fatalf("execute: %v", executeErr)
			}
			if tool.calls.Load() != 1 {
				t.Fatalf("tool calls = %d, want 1", tool.calls.Load())
			}
		})
	}
}

func TestServiceExecuteRejectsChangedApprovalPosture(t *testing.T) {
	tests := []struct {
		name         string
		preparedReq  bool
		preparedKind string
		freshReq     bool
		freshKind    string
	}{
		{name: "kind_changed", preparedReq: true, preparedKind: "manager", freshReq: true, freshKind: "security"},
		{name: "approval_added", preparedReq: false, freshReq: true, freshKind: "manager"},
		{name: "approval_removed", preparedReq: true, preparedKind: "manager", freshReq: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			required, kind := test.preparedReq, test.preparedKind
			authorizer := &scriptedAuthorizer{
				discovery: func(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
					return AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1", ApprovalRequired: required, ApprovalKind: kind}, nil
				},
				execute: func(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
					return AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1", ApprovalRequired: required, ApprovalKind: kind}, nil
				},
			}
			s, tool := buildService(t, authorizer, "done")
			approvals := NewApprovalService(NewMemoryApprovalStore(), nil)
			s.SetApprovalService(approvals)
			ctx := ctxWithPrincipal("ns")
			prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if test.preparedReq {
				plan, getErr := s.plans.Get(ctx, execution.NamespaceScope{Namespace: "ns"}, prepared.Ref.ID)
				if getErr != nil {
					t.Fatalf("get plan: %v", getErr)
				}
				if _, resolveErr := approvals.Resolve(ctx, execution.NamespaceScope{Namespace: "ns"}, plan, "approver", "approve", "nonce", time.Hour); resolveErr != nil {
					t.Fatalf("resolve: %v", resolveErr)
				}
			}
			required, kind = test.freshReq, test.freshKind
			if _, err := s.Execute(ctx, prepared.Ref); !errors.Is(err, ErrPlanApprovalStale) || !errors.Is(err, ErrPlanStale) {
				t.Fatalf("execute err = %v, want approval stale", err)
			}
			if tool.calls.Load() != 0 {
				t.Fatalf("tool calls = %d, want 0", tool.calls.Load())
			}
		})
	}
}

func TestServicePrepareFrozenArguments(t *testing.T) {
	s, _ := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "")
	ctx := ctxWithPrincipal("ns")

	raw := json.RawMessage(`{"b":2,"a":1}`)
	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t", Arguments: raw})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Mutating the caller's raw arguments after Prepare must not affect the
	// frozen plan.
	raw[0] = '{'
	raw[1] = '}'
	result, err := s.Execute(ctx, prepared.Ref)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	content, _ := result.Content.(map[string]any)
	echo, _ := content["echo"].(map[string]any)
	if echo["a"] != float64(1) || echo["b"] != float64(2) {
		t.Fatalf("frozen arguments lost: %+v", echo)
	}
}

func TestServiceApprovalRequiredBlocksExecution(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(true), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(true), nil
		},
	}, "")
	ctx := ctxWithPrincipal("ns")

	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !prepared.ApprovalRequired {
		t.Fatalf("prepare must report approval required")
	}
	_, err = s.Execute(ctx, prepared.Ref)
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("execute err = %v, want approval required", err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool must not run before approval")
	}
}

func TestServiceFreshAuthorizationOnExecute(t *testing.T) {
	var revoked atomic.Bool
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			if revoked.Load() {
				return AuthorizationDecision{Allowed: false, ReasonCode: "revoked"}, nil
			}
			return allow(false), nil
		},
	}, "")
	ctx := ctxWithPrincipal("ns")

	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Permission revoked between Prepare and Execute (authz epoch change).
	revoked.Store(true)
	if _, err := s.Execute(ctx, prepared.Ref); !errors.Is(err, ErrForbidden) {
		t.Fatalf("execute err = %v, want forbidden after revocation", err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool must not run after revocation")
	}
}

func TestServiceCrossNamespaceExecuteRejected(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "")
	prepared, err := s.Prepare(ctxWithPrincipal("ns-a"), PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// A principal from ns-b cannot execute ns-a's plan.
	if _, err := s.Execute(ctxWithPrincipal("ns-b"), prepared.Ref); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("cross-namespace execute err = %v, want plan not found", err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool must not run cross-namespace")
	}
}

func TestServiceExpiredPlanRejected(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "")
	base := time.Now().UTC()
	s.SetClock(func() time.Time { return base })
	prepared, err := s.Prepare(ctxWithPrincipal("ns"), PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Advance past the plan TTL.
	s.SetClock(func() time.Time { return base.Add(planTTL + time.Minute) })
	if _, err := s.Execute(ctxWithPrincipal("ns"), prepared.Ref); !errors.Is(err, ErrPlanExpired) {
		t.Fatalf("execute err = %v, want expired", err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool must not run for expired plan")
	}
}

func TestServiceResultSizeLimit(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	s, _ := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, big)
	s.SetResultLimits(ResultLimits{MaxBytes: 1024})
	ctx := ctxWithPrincipal("ns")
	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := s.Execute(ctx, prepared.Ref); !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("execute err = %v, want result-too-large", err)
	}
}

func TestServiceAuthorizeCalledPerRequest(t *testing.T) {
	auth := &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}
	s, _ := buildService(t, auth, "x")
	ctx := ctxWithPrincipal("ns")
	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t", Arguments: json.RawMessage(`{"k":1}`)})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := s.Execute(ctx, prepared.Ref); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// One authorize for Prepare, one fresh authorize for Execute.
	if auth.execCalls.Load() != 2 {
		t.Fatalf("authorize calls = %d, want 2", auth.execCalls.Load())
	}
}

func TestServiceUnknownTool(t *testing.T) {
	s, _ := buildService(t, &scriptedAuthorizer{
		discovery: func(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "")
	if _, err := s.Prepare(ctxWithPrincipal("ns"), PrepareRequest{ToolName: "missing"}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want unknown tool", err)
	}
}

func TestServiceIdentityRequired(t *testing.T) {
	s, _ := buildService(t, nil, "")
	if _, err := s.Prepare(context.Background(), PrepareRequest{ToolName: "t"}); !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("err = %v, want identity required", err)
	}
}
