package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/postgres"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/sqlite"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// approvalAuthorizer requires approval for every execution (single-tool test).
type approvalAuthorizer struct{}

func (approvalAuthorizer) AuthorizeDiscovery(context.Context, execution.Principal, catalog.ToolDescriptor) (service.AuthorizationDecision, error) {
	return service.AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1"}, nil
}
func (approvalAuthorizer) Authorize(context.Context, service.AuthorizeRequest) (service.AuthorizationDecision, error) {
	return service.AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1", ApprovalRequired: true, ApprovalKind: "human"}, nil
}

// countingService counts Prepare/Execute while delegating to the concrete
// service (which also supplies ResolveApproval via embedding).
type countingService struct {
	*service.Service
	prepares atomic.Int32
	executes atomic.Int32
	mu       sync.Mutex
	execRefs []service.PlanRef
}

func (c *countingService) Prepare(ctx context.Context, req service.PrepareRequest) (service.PreparedExecution, error) {
	c.prepares.Add(1)
	return c.Service.Prepare(ctx, req)
}
func (c *countingService) Execute(ctx context.Context, ref service.PlanRef) (service.ToolResult, error) {
	c.executes.Add(1)
	c.mu.Lock()
	c.execRefs = append(c.execRefs, ref)
	c.mu.Unlock()
	return c.Service.Execute(ctx, ref)
}

func (c *countingService) executedRefs() []service.PlanRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]service.PlanRef(nil), c.execRefs...)
}

type frozenApprovalTool struct {
	approvalTool
	mu        sync.Mutex
	args      map[string]any
	principal execution.Principal
}

func (t *frozenApprovalTool) Call(ctx context.Context, args map[string]any) (any, error) {
	if t.calls != nil {
		atomic.AddInt32(t.calls, 1)
	}
	principal, err := service.PrincipalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.args = make(map[string]any, len(args))
	for k, v := range args {
		t.args[k] = v
	}
	t.principal = principal
	t.mu.Unlock()
	return "done", nil
}

func (t *frozenApprovalTool) invocation() (map[string]any, execution.Principal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	args := make(map[string]any, len(t.args))
	for k, v := range t.args {
		args[k] = v
	}
	return args, t.principal
}

func managedApprovalService(t *testing.T, tool tools.Tool) *countingService {
	t.Helper()
	b := catalog.NewBuilder()
	desc := catalog.ToolDescriptor{
		Name: tool.Name(), Description: tool.Description(), InputSchema: map[string]any{"type": "object"},
		ReadOnly: catalog.TriStateTrue, Source: catalog.Source{Kind: "local", Name: "test"},
	}
	if err := b.Add(desc, tool); err != nil {
		t.Fatalf("add: %v", err)
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	svc := service.New(reg, service.NewMemoryPlanStore(), approvalAuthorizer{})
	svc.SetApprovalService(service.NewApprovalService(service.NewMemoryApprovalStore(), nil))
	return &countingService{Service: svc}
}

func managedApprovalAgent(t *testing.T, tool tools.Tool, svc service.ToolService, model *queueModel, store db.AgentStore) *Agent {
	t.Helper()
	ag, err := New(AgentConfig{
		Name: "managed-approval-test", ID: "agent-1", Model: model, Instructions: "sys",
		Tools: []tools.Tool{tool}, ToolService: svc, DB: store, UserID: "user-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func TestManagedApprovalPauseAndResumeUsesSamePlan(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	// One governed service shared across pause and resume (the plan/approval
	// stores are durable in a real deployment).
	counting := managedApprovalService(t, tool)

	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "ok", ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
		{Content: "done"},
	}}
	db := in_memory.New()
	ag := managedApprovalAgent(t, tool, counting, model, db)

	ctx := service.ContextWithPrincipal(context.Background(), execution.Principal{Subject: "alice", Namespace: "ns"})
	output, err := ag.Run(ctx, "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Status != session.RunStatusPaused {
		t.Fatalf("status = %q, want paused", output.Status)
	}
	if output.ActiveRequirement() == nil {
		t.Fatalf("active requirement missing")
	}
	if len(output.PendingPlans) != 1 {
		t.Fatalf("pending plans = %d, want 1", len(output.PendingPlans))
	}
	if counting.prepares.Load() != 1 {
		t.Fatalf("prepares = %d, want 1", counting.prepares.Load())
	}
	if counting.executes.Load() != 0 || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("zero side effects before approval: executes=%d calls=%d", counting.executes.Load(), calls)
	}

	// Rebuild the agent from the same DB (simulates a restart), sharing the
	// same governed service so the frozen plan/approval store persists.
	ag2 := managedApprovalAgent(t, tool, counting, &queueModel{}, db)
	resumeReq := session.ResumeRequest{
		RunID:         output.RunID,
		RequirementID: output.ActiveRequirement().ID,
		Action:        string(requirement.ActionApprove),
		ActorID:       "reviewer",
	}
	resumed, err := ag2.Resume(ctx, resumeReq)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != session.RunStatusCompleted {
		t.Fatalf("status = %q, want completed", resumed.Status)
	}
	// The same frozen plan was executed: exactly one Prepare across pause +
	// resume (no re-prepare), and the executor ran exactly once.
	if counting.prepares.Load() != 1 {
		t.Fatalf("prepares = %d after resume, want 1 (no re-prepare)", counting.prepares.Load())
	}
	if counting.executes.Load() != 1 {
		t.Fatalf("executes = %d, want 1", counting.executes.Load())
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("tool called %d times, want 1", calls)
	}
}

func managedApprovalSQLiteService(t *testing.T, d *sqlite.DB, tool tools.Tool) *service.Service {
	t.Helper()
	b := catalog.NewBuilder()
	desc := catalog.ToolDescriptor{Name: tool.Name(), Description: tool.Description(), InputSchema: map[string]any{"type": "object"}, ReadOnly: catalog.TriStateTrue, Source: catalog.Source{Kind: "local", Name: "test"}}
	if err := b.Add(desc, tool); err != nil {
		t.Fatalf("add: %v", err)
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	svc := service.New(reg, sqlite.NewPlanStore(d), approvalAuthorizer{})
	svc.SetApprovalService(service.NewApprovalService(sqlite.NewApprovalStore(d), nil))
	return svc
}

func managedApprovalPostgresService(t *testing.T, d *postgres.DB, tool tools.Tool) *service.Service {
	t.Helper()
	b := catalog.NewBuilder()
	desc := catalog.ToolDescriptor{Name: tool.Name(), Description: tool.Description(), InputSchema: map[string]any{"type": "object"}, ReadOnly: catalog.TriStateTrue, Source: catalog.Source{Kind: "local", Name: "test"}}
	if err := b.Add(desc, tool); err != nil {
		t.Fatalf("add: %v", err)
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	svc := service.New(reg, postgres.NewPlanStore(d), approvalAuthorizer{})
	svc.SetApprovalService(service.NewApprovalService(postgres.NewApprovalStore(d), nil))
	return svc
}

func TestManagedApprovalSQLiteRestartUsesFrozenPlanAndIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.db")
	d, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	var calls int32
	tool := &frozenApprovalTool{approvalTool: approvalTool{kind: tools.ApprovalRequired, calls: &calls}}
	counting := &countingService{Service: managedApprovalSQLiteService(t, d, tool)}
	model := &queueModel{responses: []*models.ModelResponse{{ToolCalls: []models.ToolCall{{ID: "frozen", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: `{"value":"original"}`}}}}, {Content: "done"}}}
	principal := execution.Principal{Subject: "alice", Namespace: "ns", Issuer: "idp", AuthzEpoch: 9}
	ctx := execution.ContextWithNamespace(service.ContextWithPrincipal(context.Background(), principal), "ns")
	ag := managedApprovalAgent(t, tool, counting, model, d)
	paused, err := ag.Run(ctx, "hello")
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	ref := paused.PendingPlans["frozen"]
	if paused.Principal != principal || paused.Namespace != "ns" || ref.ID == "" || ref.Version == "" {
		t.Fatalf("frozen state = %+v", paused)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	counting.Service = managedApprovalSQLiteService(t, reopened, tool)
	ag2 := managedApprovalAgent(t, tool, counting, &queueModel{responses: []*models.ModelResponse{{Content: "done"}}}, reopened)
	resumed, err := ag2.Resume(ctx, session.ResumeRequest{RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove), ActorID: "reviewer"})
	if err != nil || resumed.Status != session.RunStatusCompleted {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	if counting.prepares.Load() != 1 || counting.executes.Load() != 1 || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("frozen plan was not reused: prepares=%d executes=%d calls=%d", counting.prepares.Load(), counting.executes.Load(), atomic.LoadInt32(&calls))
	}
	if refs := counting.executedRefs(); len(refs) != 1 || refs[0] != (service.PlanRef{ID: ref.ID, Version: ref.Version}) {
		t.Fatalf("executed refs = %+v, want %+v", refs, ref)
	}
	if args, gotPrincipal := tool.invocation(); !reflect.DeepEqual(args, map[string]any{"value": "original"}) || gotPrincipal != principal {
		t.Fatalf("tool invocation args=%+v principal=%+v", args, gotPrincipal)
	}
}

func TestManagedApprovalPostgresRestartUsesFrozenPlanAndIdentity(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		if os.Getenv("POSTGRES_TEST_FAIL_ON_SKIP") != "" {
			t.Fatal("POSTGRES_TEST_DSN is required (POSTGRES_TEST_FAIL_ON_SKIP=1)")
		}
		t.Skip("POSTGRES_TEST_DSN is not set")
	}
	d, err := postgres.New(context.Background(), dsn, postgres.Options{AutoMigrate: true})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	var calls int32
	tool := &frozenApprovalTool{approvalTool: approvalTool{kind: tools.ApprovalRequired, calls: &calls}}
	counting := &countingService{Service: managedApprovalPostgresService(t, d, tool)}
	principal := execution.Principal{Subject: "alice", Namespace: "pg-approval-" + filepath.Base(t.TempDir()), Issuer: "idp", AuthzEpoch: 9}
	ctx := execution.ContextWithNamespace(service.ContextWithPrincipal(context.Background(), principal), principal.Namespace)
	ag := managedApprovalAgent(t, tool, counting, &queueModel{responses: []*models.ModelResponse{{ToolCalls: []models.ToolCall{{ID: "frozen", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: `{"value":"original"}`}}}}, {Content: "done"}}}, d)
	paused, err := ag.Run(ctx, "hello")
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	ref := paused.PendingPlans["frozen"]
	if err := d.Close(); err != nil {
		t.Fatalf("close postgres: %v", err)
	}
	reopened, err := postgres.New(context.Background(), dsn, postgres.Options{AutoMigrate: true})
	if err != nil {
		t.Fatalf("reopen postgres: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	counting.Service = managedApprovalPostgresService(t, reopened, tool)
	ag2 := managedApprovalAgent(t, tool, counting, &queueModel{responses: []*models.ModelResponse{{Content: "done"}}}, reopened)
	resumed, err := ag2.Resume(ctx, session.ResumeRequest{RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove), ActorID: "reviewer"})
	if err != nil || resumed.Status != session.RunStatusCompleted {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	if counting.prepares.Load() != 1 || counting.executes.Load() != 1 || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("frozen plan was not reused: prepares=%d executes=%d calls=%d", counting.prepares.Load(), counting.executes.Load(), atomic.LoadInt32(&calls))
	}
	if refs := counting.executedRefs(); len(refs) != 1 || refs[0] != (service.PlanRef{ID: ref.ID, Version: ref.Version}) {
		t.Fatalf("executed refs = %+v, want %+v", refs, ref)
	}
	if args, gotPrincipal := tool.invocation(); !reflect.DeepEqual(args, map[string]any{"value": "original"}) || gotPrincipal != principal {
		t.Fatalf("tool invocation args=%+v principal=%+v", args, gotPrincipal)
	}
}

func TestManagedApprovalHistoricalMissingFrozenPlanFailsBeforeSideEffect(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	counting := managedApprovalService(t, tool)
	store := in_memory.New()
	ctx := service.ContextWithPrincipal(context.Background(), execution.Principal{Subject: "alice", Namespace: "ns"})
	ag := managedApprovalAgent(t, tool, counting, &queueModel{responses: []*models.ModelResponse{{ToolCalls: []models.ToolCall{{ID: "legacy", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}}}}, store)
	paused, err := ag.Run(ctx, "hello")
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	stored, err := store.GetRun(ctx, db.RunQuery{RunID: paused.RunID, UserID: paused.UserID})
	if err != nil || stored == nil {
		t.Fatalf("load paused = %+v, %v", stored, err)
	}
	stored.PendingPlans = nil // legacy envelope predating frozen-plan persistence
	if err := store.UpdateRun(ctx, stored, session.RunStatusPaused); err != nil {
		t.Fatalf("write historical fixture: %v", err)
	}
	_, err = ag.Resume(ctx, session.ResumeRequest{RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove), ActorID: "reviewer"})
	if !errors.Is(err, ErrFrozenApprovalStateRequired) {
		t.Fatalf("resume historical state = %v", err)
	}
	if counting.prepares.Load() != 1 || counting.executes.Load() != 0 || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("historical resume had side effects: prepares=%d executes=%d calls=%d", counting.prepares.Load(), counting.executes.Load(), atomic.LoadInt32(&calls))
	}
}

func TestManagedApprovalHistoricalIdentityFieldsFailBeforeSideEffect(t *testing.T) {
	principal := execution.Principal{Subject: "alice", Namespace: "ns", Issuer: "idp", AuthzEpoch: 4}
	for _, tc := range []struct {
		name    string
		corrupt func(*session.RunOutput)
	}{
		{name: "missing_principal", corrupt: func(run *session.RunOutput) { run.Principal = execution.Principal{} }},
		{name: "changed_principal", corrupt: func(run *session.RunOutput) { run.Principal.Subject = "mallory" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
			counting := managedApprovalService(t, tool)
			store, err := sqlite.New(filepath.Join(t.TempDir(), "historical.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := execution.ContextWithNamespace(service.ContextWithPrincipal(context.Background(), principal), principal.Namespace)
			ag := managedApprovalAgent(t, tool, counting, &queueModel{responses: []*models.ModelResponse{{ToolCalls: []models.ToolCall{{ID: "legacy", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}}}}, store)
			paused, err := ag.Run(ctx, "hello")
			if err != nil || paused.Status != session.RunStatusPaused {
				t.Fatalf("pause = %+v, %v", paused, err)
			}
			stored, err := store.GetRun(ctx, db.RunQuery{RunID: paused.RunID, UserID: paused.UserID})
			if err != nil || stored == nil {
				t.Fatalf("load paused = %+v, %v", stored, err)
			}
			tc.corrupt(stored)
			if err := store.UpdateRun(ctx, stored, session.RunStatusPaused); err != nil {
				t.Fatalf("write historical fixture: %v", err)
			}
			_, err = ag.Resume(ctx, session.ResumeRequest{RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove), ActorID: "reviewer"})
			if !errors.Is(err, ErrFrozenApprovalStateRequired) {
				t.Fatalf("resume historical state = %v", err)
			}
			if counting.prepares.Load() != 1 || counting.executes.Load() != 0 || atomic.LoadInt32(&calls) != 0 {
				t.Fatalf("historical resume had side effects: prepares=%d executes=%d calls=%d", counting.prepares.Load(), counting.executes.Load(), atomic.LoadInt32(&calls))
			}
		})
	}
}

func TestManagedApprovalWrongNamespaceFailsBeforeSideEffect(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	counting := managedApprovalService(t, tool)
	store, err := sqlite.New(filepath.Join(t.TempDir(), "namespace.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	principal := execution.Principal{Subject: "alice", Namespace: "ns-a"}
	ctx := execution.ContextWithNamespace(service.ContextWithPrincipal(context.Background(), principal), principal.Namespace)
	ag := managedApprovalAgent(t, tool, counting, &queueModel{responses: []*models.ModelResponse{{ToolCalls: []models.ToolCall{{ID: "ns", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}}}}, store)
	paused, err := ag.Run(ctx, "hello")
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	wrong := execution.ContextWithNamespace(service.ContextWithPrincipal(context.Background(), execution.Principal{Subject: "alice", Namespace: "ns-b"}), "ns-b")
	_, err = ag.Resume(wrong, session.ResumeRequest{RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove), ActorID: "reviewer"})
	if !errors.Is(err, ErrResumeNotFound) {
		t.Fatalf("wrong namespace resume = %v", err)
	}
	if counting.prepares.Load() != 1 || counting.executes.Load() != 0 || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("wrong namespace had side effects: prepares=%d executes=%d calls=%d", counting.prepares.Load(), counting.executes.Load(), atomic.LoadInt32(&calls))
	}
}

func TestManagedApprovalConflictingTrustedNamespacesFailBeforePrepare(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	counting := managedApprovalService(t, tool)
	ctx := execution.ContextWithNamespace(service.ContextWithPrincipal(context.Background(), execution.Principal{Subject: "alice", Namespace: "ns-a"}), "ns-b")
	store, err := sqlite.New(filepath.Join(t.TempDir(), "conflict.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ag := managedApprovalAgent(t, tool, counting, &queueModel{responses: []*models.ModelResponse{{ToolCalls: []models.ToolCall{{ID: "conflict", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}}}}, store)
	if _, err := ag.Run(ctx, "hello"); !errors.Is(err, ErrTrustedIdentityNamespaceMismatch) {
		t.Fatalf("run conflicting trusted contexts = %v", err)
	}
	if counting.prepares.Load() != 0 || counting.executes.Load() != 0 || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("conflicting contexts had side effects: prepares=%d executes=%d calls=%d", counting.prepares.Load(), counting.executes.Load(), atomic.LoadInt32(&calls))
	}
}
