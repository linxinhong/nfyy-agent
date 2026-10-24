package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

type allowAll struct{}

func (allowAll) AuthorizeDiscovery(context.Context, execution.Principal, catalog.ToolDescriptor) (service.AuthorizationDecision, error) {
	return service.AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1"}, nil
}
func (allowAll) Authorize(context.Context, service.AuthorizeRequest) (service.AuthorizationDecision, error) {
	return service.AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1"}, nil
}

// counterTool implements tools.Tool directly (avoids NewFunction's reflection
// on map[string]any parameters).
type counterTool struct {
	name   string
	calls  *atomic.Int32
	result string
}

func (c *counterTool) Name() string           { return c.name }
func (c *counterTool) Description() string    { return "d" }
func (c *counterTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (c *counterTool) Call(context.Context, map[string]any) (any, error) {
	c.calls.Add(1)
	return c.result, nil
}

// TestAgent_Run_RoutesToolsThroughManagedService verifies that when an agent
// is configured with a ToolService, tool invocations flow through
// Prepare/Execute (the governed executor) and never the raw tool.
func TestAgent_Run_RoutesToolsThroughManagedService(t *testing.T) {
	var svcCalls atomic.Int32
	var rawCalls atomic.Int32

	rawTool := &counterTool{name: "mock_tool", calls: &rawCalls, result: "raw-result"}
	svcExec := &counterTool{name: "mock_tool", calls: &svcCalls, result: "svc-result"}

	b := catalog.NewBuilder()
	desc := catalog.ToolDescriptor{
		Name: "mock_tool", Description: "d", InputSchema: map[string]any{"type": "object"},
		ReadOnly: catalog.TriStateTrue, Source: catalog.Source{Kind: "local", Name: "test"},
	}
	if err := b.Add(desc, svcExec); err != nil {
		t.Fatalf("add: %v", err)
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	svc := service.New(reg, service.NewMemoryPlanStore(), allowAll{})

	m := mock.New()
	m.ToolCallRounds = 1
	m.ResponseData = &models.ModelResponse{Content: "Done"}

	ag, err := New(AgentConfig{
		Name:         "managed-tool-test",
		Model:        m,
		Tools:        []tools.Tool{rawTool},
		ToolService:  svc,
		MaxToolCalls: 5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := service.ContextWithPrincipal(context.Background(), execution.Principal{Subject: "alice", Namespace: "ns"})
	out, err := ag.Run(ctx, "call mock_tool")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if svcCalls.Load() != 1 {
		t.Fatalf("governed executor calls = %d, want 1", svcCalls.Load())
	}
	if rawCalls.Load() != 0 {
		t.Fatalf("raw tool was called directly %d times", rawCalls.Load())
	}
	if out.Content != "Done" {
		t.Fatalf("content = %q, want Done", out.Content)
	}
}
