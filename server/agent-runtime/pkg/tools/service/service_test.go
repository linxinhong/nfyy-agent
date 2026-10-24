package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
)

func TestMemoryPlanStoreNamespacePartitioning(t *testing.T) {
	store := NewMemoryPlanStore()
	ctx := context.Background()

	mk := func(ns string) *ExecutionPlan {
		return &ExecutionPlan{
			ID: "plan-1", Version: "v1", ToolName: "t",
			Arguments:     json.RawMessage(`{"a":1}`),
			ArgumentsHash: "hash",
			Principal:     execution.Principal{Subject: "s", Namespace: ns},
			PreparedAt:    time.Now().UTC(),
		}
	}
	if err := store.Create(ctx, mk("ns-a")); err != nil {
		t.Fatalf("create ns-a: %v", err)
	}
	if err := store.Create(ctx, mk("ns-b")); err != nil {
		t.Fatalf("create ns-b: %v", err)
	}

	gotA, err := store.Get(ctx, execution.NamespaceScope{Namespace: "ns-a"}, "plan-1")
	if err != nil || gotA.Principal.Namespace != "ns-a" {
		t.Fatalf("ns-a get = %+v, err=%v", gotA, err)
	}
	gotB, err := store.Get(ctx, execution.NamespaceScope{Namespace: "ns-b"}, "plan-1")
	if err != nil || gotB.Principal.Namespace != "ns-b" {
		t.Fatalf("ns-b get = %+v, err=%v", gotB, err)
	}
	// Unscoped read is rejected.
	if _, err := store.Get(ctx, execution.NamespaceScope{}, "plan-1"); err == nil {
		t.Fatalf("expected unscoped read rejection")
	}
	// Unknown namespace cannot see the plan.
	if _, err := store.Get(ctx, execution.NamespaceScope{Namespace: "ns-c"}, "plan-1"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("cross-namespace read: err = %v", err)
	}
}

func TestMemoryPlanStoreRejectsInvalidNamespace(t *testing.T) {
	store := NewMemoryPlanStore()
	plan := &ExecutionPlan{
		ID: "p", ToolName: "t", Arguments: json.RawMessage(`{}`),
		Principal: execution.Principal{Namespace: "legacy"},
	}
	if err := store.Create(context.Background(), plan); err == nil {
		t.Fatalf("expected reserved-namespace rejection")
	}
}

func TestCanonicalHashStable(t *testing.T) {
	a := CanonicalHash([]byte(`{"x":1}`))
	b := CanonicalHash([]byte(`{"x":1}`))
	if a != b || a == "" {
		t.Fatalf("canonical hash unstable: %q %q", a, b)
	}
}

func TestDenyAllAuthorizerFailsClosed(t *testing.T) {
	a := DenyAllAuthorizer{}
	p := execution.Principal{Namespace: "ns"}
	if d, err := a.AuthorizeDiscovery(context.Background(), p, catalog.ToolDescriptor{}); err != nil || d.Allowed {
		t.Fatalf("discovery must fail closed: %+v, %v", d, err)
	}
	if d, err := a.Authorize(context.Background(), AuthorizeRequest{Principal: p}); err != nil || d.Allowed {
		t.Fatalf("execution must fail closed: %+v, %v", d, err)
	}
}

func TestErrorStringsStable(t *testing.T) {
	for _, err := range []error{ErrUnknownTool, ErrPlanNotFound, ErrPlanStale, ErrPlanExpired, ErrPlanNamespaceMismatch, ErrForbidden} {
		if err == nil || !strings.Contains(err.Error(), "service:") {
			t.Fatalf("sentinel %v must be stable and namespaced", err)
		}
	}
}
