package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// TestAgent_ManagedGoldenJourney exercises the full governed path without a
// real LLM: trusted identity injection -> governed tool execution via
// ToolService -> transactional audit (requested + outcome) -> outbox export.
func TestAgent_ManagedGoldenJourney(t *testing.T) {
	var calls atomic.Int32
	exec := &counterTool{name: "mock_tool", calls: &calls, result: "answer"}

	b := catalog.NewBuilder()
	if err := b.Add(catalog.ToolDescriptor{
		Name: "mock_tool", Description: "d", InputSchema: map[string]any{"type": "object"},
		ReadOnly: catalog.TriStateTrue, Source: catalog.Source{Kind: "local", Name: "test"},
	}, exec); err != nil {
		t.Fatalf("add: %v", err)
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	svc := service.New(reg, service.NewMemoryPlanStore(), allowAll{})
	journal := run.NewMemoryExecutionJournal()
	exporter := run.NewMemoryAuditExporter()
	svc.SetJournal(journal, exporter, "worker-1")

	m := mock.New()
	m.ToolCallRounds = 1
	m.ResponseData = &models.ModelResponse{Content: "Done"}

	ag, err := New(AgentConfig{
		Name:         "golden",
		Model:        m,
		Tools:        []tools.Tool{exec},
		ToolService:  svc,
		MaxToolCalls: 5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := service.ContextWithPrincipal(context.Background(), execution.Principal{Subject: "alice", Namespace: "ns"})
	out, err := ag.Run(ctx, "use the tool")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "Done" {
		t.Fatalf("content = %q", out.Content)
	}
	if calls.Load() != 1 {
		t.Fatalf("governed executor calls = %d, want 1", calls.Load())
	}
	if delivered := exporter.Delivered(); len(delivered) != 2 {
		t.Fatalf("audit delivered = %d, want 2 (requested + outcome)", len(delivered))
	}
}
