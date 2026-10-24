package agent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// --- test tools ---

type approvalTool struct {
	tools.Tool
	kind  tools.ApprovalKind
	calls *int32
}

func (t approvalTool) Approval() tools.ApprovalKind { return t.kind }
func (t approvalTool) Name() string                 { return "approval_tool" }
func (t approvalTool) Description() string          { return "a tool" }
func (t approvalTool) Schema() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": "approval_tool"}}
}
func (t approvalTool) Call(ctx context.Context, args map[string]any) (any, error) {
	if t.calls != nil {
		atomic.AddInt32(t.calls, 1)
	}
	return "done", nil
}

// queueModel returns configured responses in order.
type queueModel struct {
	responses []*models.ModelResponse
	mu        sync.Mutex
	idx       int
	calls     int32
}

func (m *queueModel) Response(ctx context.Context, msgs []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	atomic.AddInt32(&m.calls, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idx >= len(m.responses) {
		return &models.ModelResponse{Content: "done"}, nil
	}
	resp := m.responses[m.idx]
	m.idx++
	return resp, nil
}

func (m *queueModel) ResponseStream(ctx context.Context, msgs []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	atomic.AddInt32(&m.calls, 1)
	ch := make(chan models.StreamEvent, 1)
	ch <- models.StreamEvent{Event: "done", Done: true}
	close(ch)
	return ch, nil
}

func (m *queueModel) ID() string                     { return "queue" }
func (m *queueModel) Provider() string               { return "queue" }
func (m *queueModel) SupportsNativeJSONSchema() bool { return false }

// --- helpers ---

func newApprovalAgent(t *testing.T, tool tools.Tool, model *queueModel, db *in_memory.DB) *Agent {
	t.Helper()
	ag, err := New(AgentConfig{
		Name:         "approval-test",
		ID:           "agent-1",
		Model:        model,
		Instructions: "sys",
		Tools:        []tools.Tool{tool},
		DB:           db,
		UserID:       "user-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func newResumeAgent(t *testing.T, tool tools.Tool, model *queueModel, db *in_memory.DB) *Agent {
	t.Helper()
	if model == nil {
		model = &queueModel{}
	}
	ag, err := New(AgentConfig{
		Name:         "approval-test",
		ID:           "agent-1",
		Model:        model,
		Instructions: "sys",
		Tools:        []tools.Tool{tool},
		DB:           db,
		UserID:       "user-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

// --- tests ---

func TestAgent_Run_PausesForRequiredTool(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "ok", ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
	}}
	db := in_memory.New()
	ag := newApprovalAgent(t, tool, model, db)

	output, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Status != session.RunStatusPaused {
		t.Fatalf("status = %q, want paused", output.Status)
	}
	if output.ActiveRequirement() == nil || output.ActiveRequirement().Status != requirement.StatusPending {
		t.Fatalf("requirement = %+v", output.ActiveRequirement())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("tool called %d times before approval, want 0", calls)
	}
}

func TestAgent_Resume_Approve(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "ok", ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
		{Content: "done"},
	}}
	db := in_memory.New()
	ag := newApprovalAgent(t, tool, model, db)

	output, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Rebuild agent from same DB.
	ag2 := newResumeAgent(t, tool, &queueModel{}, db)

	resumeReq := session.ResumeRequest{
		RunID:         output.RunID,
		RequirementID: output.ActiveRequirement().ID,
		Action:        string(requirement.ActionApprove),
	}
	resumed, err := ag2.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != session.RunStatusCompleted {
		t.Fatalf("status = %q, want completed", resumed.Status)
	}
	if resumed.Requirements[0].Status != requirement.StatusApproved {
		t.Fatalf("requirement status = %q, want approved", resumed.Requirements[0].Status)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("tool called %d times after approve, want 1", calls)
	}
	if resumed.Metrics.ModelCalls != 2 {
		t.Fatalf("model calls = %d, want 2 across run+resume", resumed.Metrics.ModelCalls)
	}
}

func TestAgent_Resume_Idempotent(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "ok", ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
		{Content: "done"},
	}}
	db := in_memory.New()
	ag := newApprovalAgent(t, tool, model, db)

	output, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	ag2 := newResumeAgent(t, tool, &queueModel{}, db)
	resumeReq := session.ResumeRequest{
		RunID:         output.RunID,
		RequirementID: output.ActiveRequirement().ID,
		Action:        string(requirement.ActionApprove),
	}
	// First approve.
	_, err = ag2.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	// Second approve: idempotent.
	resumed2, err := ag2.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if resumed2.Requirements[0].Status != requirement.StatusApproved {
		t.Fatalf("second resume status = %q, want approved", resumed2.Requirements[0].Status)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("tool called %d times after idempotent resume, want 1", calls)
	}
}

func TestAgent_Resume_Reject(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "ok", ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
		{Content: "done"},
	}}
	db := in_memory.New()
	ag := newApprovalAgent(t, tool, model, db)

	output, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	ag2 := newResumeAgent(t, tool, &queueModel{}, db)
	resumeReq := session.ResumeRequest{
		RunID:         output.RunID,
		RequirementID: output.ActiveRequirement().ID,
		Action:        string(requirement.ActionReject),
	}
	resumed, err := ag2.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Requirements[0].Status != requirement.StatusRejected {
		t.Fatalf("requirement status = %q, want rejected", resumed.Requirements[0].Status)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("tool called %d times after reject, want 0", calls)
	}
}

func TestAgent_Run_NoApprovalNeeded(t *testing.T) {
	tool := approvalTool{kind: tools.ApprovalNone}
	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "ok", ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
		{Content: "done"},
	}}
	db := in_memory.New()
	ag := newApprovalAgent(t, tool, model, db)

	output, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Status != session.RunStatusCompleted {
		t.Fatalf("status = %q, want completed", output.Status)
	}
}

func TestAgent_Resume_WrongUser(t *testing.T) {
	tool := approvalTool{kind: tools.ApprovalRequired}
	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "ok", ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
	}}
	db := in_memory.New()
	ag := newApprovalAgent(t, tool, model, db)

	output, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Different user's agent.
	ag2, _ := New(AgentConfig{
		Name: "approval-test", ID: "agent-1", Model: &queueModel{},
		Instructions: "sys", DB: db, UserID: "user-2",
	})
	resumeReq := session.ResumeRequest{
		RunID:         output.RunID,
		RequirementID: output.ActiveRequirement().ID,
		Action:        string(requirement.ActionApprove),
	}
	_, err = ag2.Resume(context.Background(), resumeReq)
	if err == nil {
		t.Fatal("expected error for wrong user")
	}
}

func TestAgent_Resume_InvalidAction(t *testing.T) {
	tool := approvalTool{kind: tools.ApprovalRequired}
	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "ok", ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
	}}
	db := in_memory.New()
	ag := newApprovalAgent(t, tool, model, db)

	output, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	ag2 := newResumeAgent(t, tool, &queueModel{}, db)
	resumeReq := session.ResumeRequest{
		RunID:         output.RunID,
		RequirementID: output.ActiveRequirement().ID,
		Action:        "invalid",
	}
	_, err = ag2.Resume(context.Background(), resumeReq)
	if err == nil {
		t.Fatal("expected error for invalid action")
	}
}

func TestAgent_ApprovalOf(t *testing.T) {
	tests := []struct {
		name string
		tool tools.Tool
		want tools.ApprovalKind
	}{
		{"none", approvalTool{kind: tools.ApprovalNone}, tools.ApprovalNone},
		{"required", approvalTool{kind: tools.ApprovalRequired}, tools.ApprovalRequired},
		{"audit", approvalTool{kind: tools.ApprovalAudit}, tools.ApprovalAudit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tools.ApprovalOf(tt.tool)
			if got != tt.want {
				t.Errorf("ApprovalOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// namedTool creates a tool with a specific name and approval kind.
func namedTool(name string, kind tools.ApprovalKind, calls *int32) tools.Tool {
	return &namedApprovalTool{name: name, kind: kind, calls: calls}
}

type namedApprovalTool struct {
	name  string
	kind  tools.ApprovalKind
	calls *int32
}

func (t *namedApprovalTool) Name() string        { return t.name }
func (t *namedApprovalTool) Description() string { return t.name }
func (t *namedApprovalTool) Schema() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": t.name}}
}
func (t *namedApprovalTool) Approval() tools.ApprovalKind { return t.kind }
func (t *namedApprovalTool) Call(ctx context.Context, args map[string]any) (any, error) {
	if t.calls != nil {
		atomic.AddInt32(t.calls, 1)
	}
	return "done", nil
}

// Mixed batch: [none, required1, audit, required2, none] with multiple tools.
func TestAgent_MixedBatchSequentialApproval(t *testing.T) {
	var noneCalls, req1Calls, auditCalls, req2Calls, none2Calls int32

	none1 := namedTool("none1", tools.ApprovalNone, &noneCalls)
	req1 := namedTool("req1", tools.ApprovalRequired, &req1Calls)
	audit := namedTool("audit1", tools.ApprovalAudit, &auditCalls)
	req2 := namedTool("req2", tools.ApprovalRequired, &req2Calls)
	none2 := namedTool("none2", tools.ApprovalNone, &none2Calls)

	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "batch", ToolCalls: []models.ToolCall{
			{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "none1", Arguments: "{}"}},
			{ID: "c2", Type: "function", Function: models.FunctionCall{Name: "req1", Arguments: "{}"}},
			{ID: "c3", Type: "function", Function: models.FunctionCall{Name: "audit1", Arguments: "{}"}},
			{ID: "c4", Type: "function", Function: models.FunctionCall{Name: "req2", Arguments: "{}"}},
			{ID: "c5", Type: "function", Function: models.FunctionCall{Name: "none2", Arguments: "{}"}},
		}},
		{Content: "done"},
	}}
	db := in_memory.New()
	ag, err := New(AgentConfig{
		Name: "approval-test", ID: "agent-1", Model: model,
		Instructions: "sys",
		Tools:        []tools.Tool{none1, req1, audit, req2, none2},
		DB:           db, UserID: "user-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Phase 1: Run pauses at first required tool (req1).
	// Roadmap: no tool should execute before approval when batch contains required.
	output, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Status != session.RunStatusPaused {
		t.Fatalf("status = %q, want paused", output.Status)
	}
	// ALL tools should be at 0 before any approval.
	if atomic.LoadInt32(&noneCalls) != 0 {
		t.Fatalf("none1 called %d times before approval, want 0", noneCalls)
	}
	if atomic.LoadInt32(&req1Calls) != 0 {
		t.Fatalf("req1 called %d times before approval, want 0", req1Calls)
	}
	if atomic.LoadInt32(&auditCalls) != 0 {
		t.Fatalf("audit1 called %d times before approval, want 0", auditCalls)
	}

	// Phase 2: Approve req1. Should execute none1→req1→audit1, then pause at req2.
	ag2, _ := New(AgentConfig{
		Name: "approval-test", ID: "agent-1", Model: &queueModel{},
		Instructions: "sys", Tools: []tools.Tool{none1, req1, audit, req2, none2},
		DB: db, UserID: "user-1",
	})
	resumeReq := session.ResumeRequest{
		RunID:         output.RunID,
		RequirementID: output.ActiveRequirement().ID,
		Action:        string(requirement.ActionApprove),
	}
	resumed, err := ag2.Resume(context.Background(), resumeReq)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != session.RunStatusPaused {
		t.Fatalf("status after req1 approve = %q, want paused at req2", resumed.Status)
	}
	if atomic.LoadInt32(&noneCalls) != 1 {
		t.Fatalf("none1 called %d times after req1 approve, want 1", noneCalls)
	}
	if atomic.LoadInt32(&req1Calls) != 1 {
		t.Fatalf("req1 called %d times after approve, want 1", req1Calls)
	}
	if atomic.LoadInt32(&auditCalls) != 1 {
		t.Fatalf("audit1 called %d times, want 1", auditCalls)
	}
	if atomic.LoadInt32(&req2Calls) != 0 {
		t.Fatalf("req2 called %d times before 2nd approval, want 0", req2Calls)
	}

	// Phase 3: Approve req2. Should execute req2→none2, then complete.
	ag3, _ := New(AgentConfig{
		Name: "approval-test", ID: "agent-1", Model: &queueModel{},
		Instructions: "sys", Tools: []tools.Tool{none1, req1, audit, req2, none2},
		DB: db, UserID: "user-1",
	})
	resumeReq2 := session.ResumeRequest{
		RunID:         output.RunID,
		RequirementID: resumed.ActiveRequirement().ID,
		Action:        string(requirement.ActionApprove),
	}
	final, err := ag3.Resume(context.Background(), resumeReq2)
	if err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	if final.Status != session.RunStatusCompleted {
		t.Fatalf("final status = %q, want completed", final.Status)
	}
	if atomic.LoadInt32(&req2Calls) != 1 {
		t.Fatalf("req2 called %d times after approve, want 1", req2Calls)
	}
	if atomic.LoadInt32(&none2Calls) != 1 {
		t.Fatalf("none2 called %d times, want 1", none2Calls)
	}
}

func TestAgent_Run_AuditToolExecutesAndRecordsResolvedRequirement(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalAudit, calls: &calls}
	model := &queueModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{{ID: "audit-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}}},
		{Content: "done"},
	}}
	output, err := newApprovalAgent(t, tool, model, in_memory.New()).Run(context.Background(), "audit")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Status != session.RunStatusCompleted || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("output=%+v calls=%d", output, calls)
	}
	if len(output.Requirements) != 1 || output.Requirements[0].Kind != requirement.KindToolAudit ||
		output.Requirements[0].Status != requirement.StatusApproved || output.Requirements[0].Resolution == nil {
		t.Fatalf("audit requirement = %+v", output.Requirements)
	}
}

func TestAgent_Run_UnknownApprovalPolicyFailsClosed(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalKind("future-policy"), calls: &calls}
	model := &queueModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{{ID: "unknown-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}}},
	}}
	output, err := newApprovalAgent(t, tool, model, in_memory.New()).Run(context.Background(), "unknown")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Status != session.RunStatusPaused || output.ActiveRequirement() == nil {
		t.Fatalf("output = %+v", output)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("unknown-policy tool executed %d times", calls)
	}
}

func TestAgent_Run_RequiredApprovalWithoutStoreFailsBeforeToolExecution(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	model := &queueModel{responses: []*models.ModelResponse{{
		ToolCalls: []models.ToolCall{{ID: "durable-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}},
	}}}
	value, err := New(AgentConfig{ID: "agent-1", Model: model, Tools: []tools.Tool{tool}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	output, runErr := value.Run(context.Background(), "requires store")
	if !errors.Is(runErr, ErrApprovalStoreRequired) {
		t.Fatalf("Run error = %v, want ErrApprovalStoreRequired", runErr)
	}
	if output == nil || output.Status != session.RunStatusError || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("output=%+v calls=%d", output, calls)
	}
}

func TestAgent_Resume_ApprovalWithoutTimeoutGetsDefaultDeadline(t *testing.T) {
	var calls int32
	base := approvalTool{kind: tools.ApprovalNone, calls: &calls}
	// No WithApprovalTimeout: the safety default must apply.
	tool := tools.WithApproval(base, tools.ApprovalRequired)
	store := in_memory.New()
	model := &queueModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{{ID: "no-timeout-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}}},
	}}
	first, err := newApprovalAgent(t, tool, model, store).Run(context.Background(), "safety default")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	req := first.ActiveRequirement()
	if req == nil {
		t.Fatalf("no active requirement")
	}
	if req.Deadline == nil {
		t.Fatalf("deadline = nil, want safety-net default %v", defaultApprovalTimeout)
	}
	remaining := time.Until(*req.Deadline)
	if remaining > 2*defaultApprovalTimeout || remaining < defaultApprovalTimeout-time.Hour {
		t.Fatalf("deadline %v not within default window %v", *req.Deadline, defaultApprovalTimeout)
	}
}

func TestAgent_Resume_ExpiredApprovalRejectsWithoutToolSideEffect(t *testing.T) {
	var calls int32
	base := approvalTool{kind: tools.ApprovalNone, calls: &calls}
	tool := tools.WithApproval(base, tools.ApprovalRequired, tools.WithApprovalTimeout(time.Nanosecond))
	store := in_memory.New()
	model := &queueModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{{ID: "expired-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}}},
	}}
	first, err := newApprovalAgent(t, tool, model, store).Run(context.Background(), "expire")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	time.Sleep(time.Millisecond)
	final, err := newResumeAgent(t, tool, &queueModel{}, store).Resume(context.Background(), session.ResumeRequest{
		RunID: first.RunID, RequirementID: first.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("expired tool executed %d times", calls)
	}
	if final.Requirements[0].Status != requirement.StatusExpired ||
		final.Requirements[0].Resolution.Action != requirement.ActionExpire {
		t.Fatalf("requirement = %+v", final.Requirements[0])
	}
	repeated, err := newResumeAgent(t, tool, &queueModel{}, store).Resume(context.Background(), session.ResumeRequest{
		RunID: first.RunID, RequirementID: first.ActiveRequirement().ID, Action: string(requirement.ActionExpire),
	})
	if err != nil || repeated.Requirements[0].Status != requirement.StatusExpired {
		t.Fatalf("idempotent expired Resume: output=%+v err=%v", repeated, err)
	}
}

func TestAgent_Resume_ApprovedRequirementRemainsIdempotentAfterDeadlinePasses(t *testing.T) {
	var calls int32
	base := approvalTool{kind: tools.ApprovalNone, calls: &calls}
	tool := tools.WithApproval(base, tools.ApprovalRequired, tools.WithApprovalTimeout(time.Hour))
	store := in_memory.New()
	first, err := newApprovalAgent(t, tool, &queueModel{responses: []*models.ModelResponse{{
		ToolCalls: []models.ToolCall{{ID: "approved-before-deadline", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}},
	}}}, store).Run(context.Background(), "approve")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	request := session.ResumeRequest{
		RunID: first.RunID, RequirementID: first.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	}
	resumer := newResumeAgent(t, tool, &queueModel{}, store)
	if _, err := resumer.Resume(context.Background(), request); err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	stored, err := store.GetRun(context.Background(), db.RunQuery{RunID: first.RunID, UserID: "user-1"})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	stored.Requirements[0].Deadline = &past
	if err := store.UpdateRun(context.Background(), stored, session.RunStatusCompleted); err != nil {
		t.Fatalf("move deadline: %v", err)
	}
	repeated, err := resumer.Resume(context.Background(), request)
	if err != nil {
		t.Fatalf("idempotent Resume after deadline: %v", err)
	}
	if repeated.Requirements[0].Status != requirement.StatusApproved ||
		repeated.Requirements[0].Resolution.Action != requirement.ActionApprove {
		t.Fatalf("requirement changed after deadline: %+v", repeated.Requirements[0])
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", calls)
	}
}

type blockingApprovalTool struct {
	started chan struct{}
	release chan struct{}
	calls   int32
}

func (t *blockingApprovalTool) Name() string                 { return "blocking" }
func (t *blockingApprovalTool) Description() string          { return "blocking" }
func (t *blockingApprovalTool) Approval() tools.ApprovalKind { return tools.ApprovalRequired }
func (t *blockingApprovalTool) Schema() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": t.Name()}}
}
func (t *blockingApprovalTool) Call(ctx context.Context, _ map[string]any) (any, error) {
	atomic.AddInt32(&t.calls, 1)
	close(t.started)
	select {
	case <-t.release:
		return "done", nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestAgent_Resume_ConcurrentClaimsHaveOneExecutor(t *testing.T) {
	tool := &blockingApprovalTool{started: make(chan struct{}), release: make(chan struct{})}
	store := in_memory.New()
	model := &queueModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{{ID: "blocking-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}}},
	}}
	first, err := newApprovalAgent(t, tool, model, store).Run(context.Background(), "concurrent")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	request := session.ResumeRequest{
		RunID: first.RunID, RequirementID: first.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	}
	resumer := newResumeAgent(t, tool, &queueModel{}, store)
	firstDone := make(chan error, 1)
	go func() {
		_, resumeErr := resumer.Resume(context.Background(), request)
		firstDone <- resumeErr
	}()
	<-tool.started
	_, secondErr := resumer.Resume(context.Background(), request)
	if !errors.Is(secondErr, ErrResumeConflict) {
		t.Fatalf("second Resume error = %v, want ErrResumeConflict", secondErr)
	}
	close(tool.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	if got := atomic.LoadInt32(&tool.calls); got != 1 {
		t.Fatalf("tool calls = %d, want 1", got)
	}
}

type recordingModel struct {
	mu       sync.Mutex
	messages []models.Message
}

func (m *recordingModel) Response(_ context.Context, messages []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	m.mu.Lock()
	m.messages = cloneMessages(messages)
	m.mu.Unlock()
	return &models.ModelResponse{Content: "resumed"}, nil
}
func (m *recordingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("unused")
}
func (m *recordingModel) ID() string       { return "recording" }
func (m *recordingModel) Provider() string { return "test" }

func TestAgent_Resume_RebuildsExactSessionContextWithoutDuplicatingCurrentRun(t *testing.T) {
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	store := in_memory.New()
	source := &queueModel{responses: []*models.ModelResponse{
		{Content: "first answer"},
		{ToolCalls: []models.ToolCall{{ID: "context-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}}},
	}}
	original := newApprovalAgent(t, tool, source, store)
	first, err := original.Run(context.Background(), "first input")
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	paused, err := original.RunWith(context.Background(), session.RunRequest{
		Input: session.RunInput{Message: "second input"}, SessionID: first.SessionID,
	})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	recorder := &recordingModel{}
	rebuilt, err := New(AgentConfig{
		ID: "agent-1", Name: "approval-test", Instructions: "sys", Model: recorder,
		Tools: []tools.Tool{tool}, DB: store, UserID: "user-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := rebuilt.Resume(context.Background(), session.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	counts := map[string]int{}
	for _, message := range recorder.messages {
		counts[message.Content]++
	}
	for _, content := range []string{"first input", "first answer", "second input", "done"} {
		if counts[content] != 1 {
			t.Fatalf("content %q count = %d, messages=%+v", content, counts[content], recorder.messages)
		}
	}
}

func TestAgent_ResumeStream_EmitsResumedThenPausedForNextRequiredTool(t *testing.T) {
	var firstCalls, secondCalls int32
	firstTool := namedTool("first_required", tools.ApprovalRequired, &firstCalls)
	secondTool := namedTool("second_required", tools.ApprovalRequired, &secondCalls)
	store := in_memory.New()
	model := &queueModel{responses: []*models.ModelResponse{{
		ToolCalls: []models.ToolCall{
			{ID: "first-call", Type: "function", Function: models.FunctionCall{Name: firstTool.Name(), Arguments: "{}"}},
			{ID: "second-call", Type: "function", Function: models.FunctionCall{Name: secondTool.Name(), Arguments: "{}"}},
		},
	}}}
	value, err := New(AgentConfig{
		ID: "agent-1", Name: "approval-test", Model: model,
		Tools: []tools.Tool{firstTool, secondTool}, DB: store, UserID: "user-1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := value.Run(context.Background(), "stream")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := value.ResumeStream(context.Background(), session.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	})
	if err != nil {
		t.Fatalf("ResumeStream: %v", err)
	}
	var eventTypes []EventType
	var final *session.RunOutput
	for event := range events {
		eventTypes = append(eventTypes, event.Type)
		final = event.RunOutput
	}
	// The approved first tool now reports live progress before the run
	// pauses again at the second required tool.
	want := []EventType{EventTypeRunResumed, EventTypeToolStarted, EventTypeToolResult, EventTypeRunPaused}
	if !slices.Equal(eventTypes, want) {
		t.Fatalf("events = %v, want %v", eventTypes, want)
	}
	if final == nil || final.Status != session.RunStatusPaused || final.ActiveRequirement().ToolCall.ID != "second-call" {
		t.Fatalf("final = %+v", final)
	}
	if atomic.LoadInt32(&firstCalls) != 1 || atomic.LoadInt32(&secondCalls) != 0 {
		t.Fatalf("calls = first:%d second:%d", firstCalls, secondCalls)
	}
}

func TestAgent_Resume_AppliesCompactionBeforeNextModelCall(t *testing.T) {
	var toolCalls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &toolCalls}
	model := &queueModel{responses: []*models.ModelResponse{{
		ToolCalls: []models.ToolCall{{ID: "budget-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}},
	}}}
	store := in_memory.New()
	value, err := New(AgentConfig{
		ID: "agent-1", Name: "approval-test", Instructions: "sys", Model: model,
		Tools: []tools.Tool{tool}, DB: store, UserID: "user-1",
		ContextProviders: []ContextProvider{&CompactionContextProvider{
			// Initial run: system + context + input = 30 < 32 trigger. Resume
			// adds the tool call and result: 50 > the 40-token cap.
			Policy:  &ContextWindowPolicy{MaxInputTokens: 40, TriggerRatio: 0.8, TargetRatio: 0.7},
			Counter: &fakeCounter{perMessage: 10},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := value.Run(context.Background(), "protected current input")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&model.calls); got != 1 {
		t.Fatalf("model calls before Resume = %d, want 1", got)
	}
	_, err = value.Resume(context.Background(), session.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	})
	if !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("Resume error = %v, want ErrContextBudgetExceeded", err)
	}
	if got := atomic.LoadInt32(&model.calls); got != 1 {
		t.Fatalf("model called despite resume budget failure: %d", got)
	}
	if atomic.LoadInt32(&toolCalls) != 1 {
		t.Fatalf("approved tool calls = %d, want 1", toolCalls)
	}
	stored, getErr := store.GetRun(context.Background(), db.RunQuery{RunID: paused.RunID, UserID: "user-1"})
	if getErr != nil {
		t.Fatalf("GetRun: %v", getErr)
	}
	if stored == nil || stored.Metrics.OriginalContextTokens != 50 ||
		stored.Metrics.FinalContextTokens != 50 || stored.Metrics.ContextCompactions != 1 {
		t.Fatalf("persisted resume budget audit = %+v", stored)
	}
}

type resumeContextRecorder struct {
	mu       sync.Mutex
	requests []ContextRequest
}

func (p *resumeContextRecorder) Apply(_ context.Context, request ContextRequest) (ContextRequest, error) {
	p.mu.Lock()
	p.requests = append(p.requests, ContextRequest{
		System: cloneMessages(request.System), History: cloneHistoryRuns(request.History),
		Input: request.Input, Current: cloneMessages(request.Current),
	})
	p.mu.Unlock()
	return request, nil
}

func cloneHistoryRuns(runs []HistoryRun) []HistoryRun {
	cloned := make([]HistoryRun, len(runs))
	for i := range runs {
		cloned[i] = HistoryRun{RunID: runs[i].RunID, Messages: cloneMessages(runs[i].Messages)}
	}
	return cloned
}

func TestAgent_Resume_ContextProviderReceivesProtectedCurrentRunOnce(t *testing.T) {
	tool := approvalTool{kind: tools.ApprovalRequired}
	model := &queueModel{responses: []*models.ModelResponse{{
		ToolCalls: []models.ToolCall{{ID: "provider-call", Type: "function", Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"}}},
	}}}
	store := in_memory.New()
	recorder := &resumeContextRecorder{}
	value, err := New(AgentConfig{
		ID: "agent-1", Model: model, Instructions: "sys", Tools: []tools.Tool{tool},
		DB: store, UserID: "user-1", ContextProviders: []ContextProvider{recorder},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := value.Run(context.Background(), "current input")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := value.Resume(context.Background(), session.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.requests) != 2 {
		t.Fatalf("provider calls = %d, want initial+resume", len(recorder.requests))
	}
	if len(recorder.requests[0].Current) != 0 {
		t.Fatalf("initial Current = %+v, want empty compatibility path", recorder.requests[0].Current)
	}
	resumeRequest := recorder.requests[1]
	if len(resumeRequest.Current) != 3 {
		t.Fatalf("resume Current = %+v, want user+assistant+tool", resumeRequest.Current)
	}
	if resumeRequest.Input.Content != "current input" || resumeRequest.Current[0].Content != "current input" {
		t.Fatalf("Input alias/current = %+v / %+v", resumeRequest.Input, resumeRequest.Current)
	}
	for _, history := range resumeRequest.History {
		if history.RunID == paused.RunID {
			t.Fatalf("current run duplicated in History: %+v", resumeRequest.History)
		}
	}
}
