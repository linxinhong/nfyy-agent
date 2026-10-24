package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
)

type limitTool struct {
	calls atomic.Int32
	call  func(context.Context) (any, error)
}

func (t *limitTool) Name() string           { return "limit-tool" }
func (t *limitTool) Description() string    { return "limit test tool" }
func (t *limitTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *limitTool) Call(ctx context.Context, _ map[string]any) (any, error) {
	t.calls.Add(1)
	return t.call(ctx)
}

func buildLimitService(t *testing.T, decision AuthorizationDecision, tool *limitTool) *Service {
	t.Helper()
	authorizer := &scriptedAuthorizer{
		discovery: func(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return decision, nil
		},
		execute: func(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
			return decision, nil
		},
	}
	builder := catalog.NewBuilder()
	if err := builder.Add(catalog.ToolDescriptor{
		Name: "limit-tool", Description: "limit test tool", InputSchema: map[string]any{"type": "object"},
		ReadOnly: catalog.TriStateTrue, Source: catalog.Source{Kind: "local", Name: "test"},
	}, tool); err != nil {
		t.Fatalf("add tool: %v", err)
	}
	registry, err := builder.Build()
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	return New(registry, NewMemoryPlanStore(), authorizer)
}

func prepareLimitTool(t *testing.T, service *Service) (context.Context, PlanRef) {
	t.Helper()
	ctx := ctxWithPrincipal("ns")
	prepared, err := service.Prepare(ctx, PrepareRequest{ToolName: "limit-tool"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return ctx, prepared.Ref
}

func TestServiceAuthorizationTimeoutUsesStrictestDeadline(t *testing.T) {
	tests := []struct {
		name            string
		authorizationMS int64
		callerTimeout   time.Duration
		wantDeadline    bool
		wantCaller      bool
	}{
		{name: "authorization_100ms_is_stricter_than_caller_1s", authorizationMS: 100, callerTimeout: time.Second, wantDeadline: true},
		{name: "caller_100ms_is_stricter_than_authorization_1s", authorizationMS: 1000, callerTimeout: 100 * time.Millisecond, wantDeadline: true, wantCaller: true},
		{name: "zero_authorization_keeps_caller_deadline", callerTimeout: 100 * time.Millisecond, wantDeadline: true, wantCaller: true},
		{name: "zero_authorization_without_caller_deadline_is_unlimited", wantDeadline: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var observed time.Time
			var hadDeadline bool
			tool := &limitTool{call: func(ctx context.Context) (any, error) {
				observed, hadDeadline = ctx.Deadline()
				return "ok", nil
			}}
			service := buildLimitService(t, AuthorizationDecision{
				Allowed: true, AuthorizationPolicyRevision: "pol-1", MaxTimeoutMS: test.authorizationMS,
			}, tool)
			baseCtx, ref := prepareLimitTool(t, service)
			executeCtx := baseCtx
			var callerDeadline time.Time
			cancel := func() {}
			if test.callerTimeout > 0 {
				executeCtx, cancel = context.WithTimeout(baseCtx, test.callerTimeout)
				callerDeadline, _ = executeCtx.Deadline()
			}
			defer cancel()
			result, err := service.Execute(executeCtx, ref)
			if err != nil || result.Status != "succeeded" {
				t.Fatalf("execute: result=%+v err=%v", result, err)
			}
			if hadDeadline != test.wantDeadline {
				t.Fatalf("executor deadline present=%v, want %v", hadDeadline, test.wantDeadline)
			}
			if test.wantCaller && !observed.Equal(callerDeadline) {
				t.Fatalf("executor deadline=%v, want caller deadline=%v", observed, callerDeadline)
			}
			if test.authorizationMS == 100 && !test.wantCaller {
				remaining := time.Until(observed)
				if remaining <= 0 || remaining > 100*time.Millisecond {
					t.Fatalf("authorization deadline remaining=%v, want (0,100ms]", remaining)
				}
			}
		})
	}
}

func TestServiceAuthorizationTimeoutCancelsCooperativeToolAndAuditsReason(t *testing.T) {
	tool := &limitTool{call: func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return "must-not-return", ctx.Err()
	}}
	service := buildLimitService(t, AuthorizationDecision{
		Allowed: true, AuthorizationPolicyRevision: "pol-1", MaxTimeoutMS: 20,
	}, tool)
	journal := run.NewMemoryExecutionJournal()
	service.SetJournal(journal, nil, "worker-1")
	ctx, ref := prepareLimitTool(t, service)
	started := time.Now()
	result, err := service.Execute(ctx, ref)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cooperative cancellation took %v", elapsed)
	}
	if !result.IsError || result.Status != "failed" || result.Content != nil || result.Error == nil || result.Error.Code != "tool_timeout" {
		t.Fatalf("timeout result = %+v", result)
	}
	events, err := journal.ClaimOutbox(ctx, execution.NamespaceScope{Namespace: "ns"}, "reader", 10)
	if err != nil {
		t.Fatalf("claim audit events: %v", err)
	}
	var found bool
	for _, event := range events {
		found = found || event.Type == "audit.tool.timed_out"
	}
	if !found {
		t.Fatalf("timeout audit reason missing: %+v", events)
	}
}

func TestServiceCallerCancellationStopsBeforeToolCall(t *testing.T) {
	tool := &limitTool{call: func(context.Context) (any, error) { return "unexpected", nil }}
	service := buildLimitService(t, AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1"}, tool)
	ctx, ref := prepareLimitTool(t, service)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	result, err := service.Execute(cancelled, ref)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tool.calls.Load() != 0 || !result.IsError || result.Error == nil || result.Error.Code != "tool_cancelled" || result.Content != nil {
		t.Fatalf("cancelled execution: calls=%d result=%+v", tool.calls.Load(), result)
	}
}

func TestServiceAuthorizationResultBytesUsesStrictestLimit(t *testing.T) {
	tests := []struct {
		name         string
		globalBytes  int64
		authorized   int64
		resultBytes  int
		wantTooLarge bool
	}{
		{name: "authorization_1KiB_stricter_than_global_4KiB", globalBytes: 4096, authorized: 1024, resultBytes: 1500, wantTooLarge: true},
		{name: "global_1KiB_stricter_than_authorization_4KiB", globalBytes: 1024, authorized: 4096, resultBytes: 1500, wantTooLarge: true},
		{name: "zero_authorization_keeps_global_limit", globalBytes: 1024, resultBytes: 1500, wantTooLarge: true},
		{name: "zero_limits_use_global_default", resultBytes: 1500},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tool := &limitTool{call: func(context.Context) (any, error) {
				return strings.Repeat("x", test.resultBytes), nil
			}}
			service := buildLimitService(t, AuthorizationDecision{
				Allowed: true, AuthorizationPolicyRevision: "pol-1", MaxResultBytes: test.authorized,
			}, tool).SetResultLimits(ResultLimits{MaxBytes: test.globalBytes})
			ctx, ref := prepareLimitTool(t, service)
			result, err := service.Execute(ctx, ref)
			if test.wantTooLarge {
				if !errors.Is(err, ErrResultTooLarge) || result.Content != nil {
					t.Fatalf("oversized result=%+v err=%v", result, err)
				}
			} else if err != nil || result.Status != "succeeded" {
				t.Fatalf("allowed result=%+v err=%v", result, err)
			}
			if tool.calls.Load() != 1 {
				t.Fatalf("tool calls=%d, want 1", tool.calls.Load())
			}
		})
	}
}

func TestServiceResultTooLargeAuditsReasonWithoutContent(t *testing.T) {
	tool := &limitTool{call: func(context.Context) (any, error) { return strings.Repeat("secret", 300), nil }}
	service := buildLimitService(t, AuthorizationDecision{
		Allowed: true, AuthorizationPolicyRevision: "pol-1", MaxResultBytes: 1024,
	}, tool).SetResultLimits(ResultLimits{MaxBytes: 4096})
	journal := run.NewMemoryExecutionJournal()
	service.SetJournal(journal, nil, "worker-1")
	ctx, ref := prepareLimitTool(t, service)
	result, err := service.Execute(ctx, ref)
	if !errors.Is(err, ErrResultTooLarge) || result.Content != nil {
		t.Fatalf("oversized result=%+v err=%v", result, err)
	}
	events, err := journal.ClaimOutbox(ctx, execution.NamespaceScope{Namespace: "ns"}, "reader", 10)
	if err != nil {
		t.Fatalf("claim audit events: %v", err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), "secret") {
			t.Fatalf("audit payload contains tool result: %+v", event)
		}
		if event.Type == "audit.tool.result_too_large" {
			return
		}
	}
	t.Fatalf("result-too-large audit reason missing: %+v", events)
}

func TestServiceInvalidExecutionLimitsFailClosed(t *testing.T) {
	tests := []struct {
		name     string
		decision AuthorizationDecision
		global   ResultLimits
	}{
		{name: "negative_authorization_timeout", decision: AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1", MaxTimeoutMS: -1}},
		{name: "negative_authorization_result", decision: AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1", MaxResultBytes: -1}},
		{name: "overflowing_authorization_timeout", decision: AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1", MaxTimeoutMS: 1<<63 - 1}},
		{name: "negative_global_result", decision: AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1"}, global: ResultLimits{MaxBytes: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tool := &limitTool{call: func(context.Context) (any, error) { return "unexpected", nil }}
			service := buildLimitService(t, test.decision, tool).SetResultLimits(test.global)
			ctx, ref := prepareLimitTool(t, service)
			if _, err := service.Execute(ctx, ref); !errors.Is(err, ErrInvalidExecutionLimit) {
				t.Fatalf("execute err=%v, want invalid limit", err)
			}
			if tool.calls.Load() != 0 {
				t.Fatalf("tool calls=%d, want 0", tool.calls.Load())
			}
		})
	}
}
