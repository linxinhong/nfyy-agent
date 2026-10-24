package runtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// goldenJourney executes the managed tool journey end to end with no real
// LLM or database: identity injection -> catalog -> prepare -> approval ->
// execute -> audit requested/outcome.
func TestGoldenJourneyManagedToolExecution(t *testing.T) {
	// 1. Catalog: one local tool + one MCP-adapted tool.
	local := &journeyTool{name: "local-eval", result: "local-ok"}
	b := catalog.NewBuilder()
	if err := b.Add(catalog.ToolDescriptor{
		Name: "local-eval", Description: "evaluates locally",
		InputSchema: map[string]any{"type": "object"},
		ReadOnly:    catalog.TriStateTrue,
		Source:      catalog.Source{Kind: "local", Name: "builtin"},
	}, local); err != nil {
		t.Fatalf("add local: %v", err)
	}
	ro := false
	mcpDef := mcp.ToolDef{
		Name: "mcp-fetch", Description: "fetches",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &ro}, // remote claim: not readonly
	}
	remote := &journeyTool{name: "mcp-fetch", result: "remote-ok"}
	if err := b.Add(catalog.FromMCPDef(mcpDef, catalog.Source{Kind: "mcp", Name: "srv", Version: "1"}, "catrev-1"), remote); err != nil {
		t.Fatalf("add mcp: %v", err)
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}

	// 2. Authorizer: allow all, but require approval for the MCP tool.
	auth := &journeyAuthorizer{}
	plans := service.NewMemoryPlanStore()
	approvals := service.NewApprovalService(service.NewMemoryApprovalStore(), nil)
	svc := service.New(reg, plans, auth)
	svc.SetApprovalService(approvals)

	// 3. Audit ledger + exporter.
	journal := run.NewMemoryExecutionJournal()
	ledger := run.NewTransactionalAuditLedger(journal)
	exporter := run.NewMemoryAuditExporter()

	// 4. Identity injection (host layer) + prepare.
	ctx := service.ContextWithPrincipal(context.Background(),
		execution.Principal{Subject: "alice", Namespace: "ns-golden", Issuer: "idp"})

	localPlan, err := svc.Prepare(ctx, service.PrepareRequest{ToolName: "local-eval", Arguments: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatalf("prepare local: %v", err)
	}
	if localPlan.ApprovalRequired {
		t.Fatalf("readonly local tool must not require approval")
	}

	// 5. Audit requested before execution.
	attempt := &run.Attempt{AttemptID: "att-1", RunID: "run-1", State: run.AttemptClaimed}
	if err := ledger.Requested(ctx, execution.NamespaceScope{Namespace: "ns-golden"}, attempt,
		run.AuditEvent{EventID: "ev-1", AttemptID: "att-1", RunID: "run-1", Namespace: "ns-golden", Subject: "alice", ToolName: "local-eval", Phase: "tool"}); err != nil {
		t.Fatalf("audit requested: %v", err)
	}

	// 6. Execute local tool.
	result, err := svc.Execute(ctx, localPlan.Ref)
	if err != nil || result.Content != "local-ok" {
		t.Fatalf("execute local = %+v, err=%v", result, err)
	}

	// 7. MCP tool requires approval: prepare -> 202-ish, grant, execute.
	mcpPlan, err := svc.Prepare(ctx, service.PrepareRequest{ToolName: "mcp-fetch", Arguments: json.RawMessage(`{"url":"x"}`)})
	if err != nil || !mcpPlan.ApprovalRequired {
		t.Fatalf("prepare mcp = %+v, err=%v (must require approval for remote hint)", mcpPlan, err)
	}
	// The frozen plan is required for the grant.
	plan, err := plans.Get(ctx, execution.NamespaceScope{Namespace: "ns-golden"}, mcpPlan.Ref.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if _, err := approvals.Resolve(ctx, execution.NamespaceScope{Namespace: "ns-golden"}, plan, "approver", "approve", "nonce-1", time.Hour); err != nil {
		t.Fatalf("approve: %v", err)
	}
	mcpResult, err := svc.Execute(ctx, mcpPlan.Ref)
	if err != nil || mcpResult.Content != "remote-ok" {
		t.Fatalf("execute mcp = %+v, err=%v", mcpResult, err)
	}

	// 8. Audit outcome + export; no sensitive bodies in the audit trail.
	if err := journal.AppendOutcome(ctx, execution.NamespaceScope{Namespace: "ns-golden"}, "att-1", run.AttemptInvoking, nil); err != nil {
		t.Fatalf("to invoking: %v", err)
	}
	if err := ledger.Outcome(ctx, execution.NamespaceScope{Namespace: "ns-golden"}, "att-1", run.AuditOutcomeSucceeded, "ok",
		run.AuditEvent{EventID: "ev-2", AttemptID: "att-1", RunID: "run-1", Phase: "tool"}); err != nil {
		t.Fatalf("audit outcome: %v", err)
	}
	exported, failed, err := run.ExportAndAck(ctx, journal, execution.NamespaceScope{Namespace: "ns-golden"}, "worker-1", exporter, 10)
	if err != nil || failed != 0 || exported != 2 {
		t.Fatalf("export = %d ok, %d failed, %v", exported, failed, err)
	}
	if len(exporter.Delivered()) != 2 {
		t.Fatalf("delivered = %d, want 2 (requested + outcome)", len(exporter.Delivered()))
	}

}

// journeyTool implements tools.Tool.
type journeyTool struct {
	name   string
	result string
}

func (t *journeyTool) Name() string           { return t.name }
func (t *journeyTool) Description() string    { return "d" }
func (t *journeyTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *journeyTool) Call(ctx context.Context, args map[string]any) (any, error) {
	return t.result, nil
}

// journeyAuthorizer allows discovery; requires approval when the tool is
// not ReadOnly.
type journeyAuthorizer struct{}

func (journeyAuthorizer) AuthorizeDiscovery(ctx context.Context, p execution.Principal, d catalog.ToolDescriptor) (service.AuthorizationDecision, error) {
	return service.AuthorizationDecision{Allowed: true}, nil
}
func (journeyAuthorizer) Authorize(ctx context.Context, req service.AuthorizeRequest) (service.AuthorizationDecision, error) {
	required := req.Descriptor.ReadOnly != catalog.TriStateTrue
	return service.AuthorizationDecision{Allowed: true, ApprovalRequired: required, AuthorizationPolicyRevision: "pol-1"}, nil
}
