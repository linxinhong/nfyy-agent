// Package dbtest contains adapter-independent storage contract tests.
package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

// RunStoreContract verifies the concurrency and append invariants every
// production-capable adapter must provide.
func RunStoreContract(t *testing.T, store db.Store) {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	s := &session.AgentSession{SessionID: id, AgentID: "contract-agent", UserID: "contract-user"}
	if err := store.CreateSession(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	first, err := store.ReadSession(ctx, id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	stale, _ := store.ReadSession(ctx, id)
	missingRevision := *stale
	missingRevision.Revision = 0
	if err := store.UpdateSession(ctx, &missingRevision); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("zero revision update: %v", err)
	}
	first.SessionState = map[string]any{"version": "new"}
	if err := store.UpdateSession(ctx, first); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := store.UpdateSession(ctx, stale); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	run := &session.RunOutput{RunID: uuid.NewString(), SessionID: id, AgentID: s.AgentID, UserID: s.UserID, ParentRunID: "parent-workflow", NodeID: "agent-node", Status: session.RunStatusRunning}
	if err := store.AppendRun(ctx, run); err != nil {
		t.Fatalf("append: %v", err)
	}
	run.Status = session.RunStatusCompleted
	run.Content = "finished"
	run.Metadata = map[string]string{"project_id": "contract-project"}
	run.Metrics = session.RunMetrics{
		DurationMS: 12, ModelCalls: 2, ToolCalls: 1, InputTokens: 7, OutputTokens: 3, TotalTokens: 10,
		OriginalContextTokens: 30, FinalContextTokens: 20, ContextCompactions: 1,
	}
	run.ToolExecutions = []session.ToolExecution{{CallID: "call-1", Name: "lookup", Status: "success", DurationMS: 4}}
	run.Parsed = map[string]any{"accepted": true}
	run.UpdatedAt = time.Now().UTC().Add(time.Second)
	run.Messages = []models.Message{{Role: models.RoleUser, Content: "first"}, {Role: models.RoleAssistant, Content: "second"}}
	if err := store.UpdateRun(ctx, run, session.RunStatusRunning); err != nil {
		t.Fatalf("update run: %v", err)
	}
	staleRun := *run
	staleRun.Status = session.RunStatusCancelled
	if err := store.UpdateRun(ctx, &staleRun, session.RunStatusRunning); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("stale run update: %v", err)
	}
	illegal := *run
	illegal.Status = session.RunStatusRunning
	if err := store.UpdateRun(ctx, &illegal, session.RunStatusCompleted); !errors.Is(err, db.ErrInvalidTransition) {
		t.Fatalf("illegal run transition: %v", err)
	}
	stored, _ := store.ReadSession(ctx, id)
	if stored.Revision != 2 || len(stored.Runs) != 1 || stored.SessionState["version"] != "new" {
		t.Fatalf("stored: %+v", stored)
	}
	stored.Runs[0].Content = "tampered"
	stored.Summary = "updated"
	if err := store.UpdateSession(ctx, stored); err != nil {
		t.Fatalf("field update: %v", err)
	}
	after, _ := store.ReadSession(ctx, id)
	if after.UpdatedAt.Before(run.UpdatedAt) {
		t.Fatalf("parent updated_at = %s, want >= run updated_at %s", after.UpdatedAt, run.UpdatedAt)
	}
	if after.Runs[0].Content == "tampered" {
		t.Fatal("session field update rewrote immutable run history")
	}
	found, err := store.GetRun(ctx, db.RunQuery{RunID: run.RunID, UserID: s.UserID})
	if err != nil || found == nil {
		t.Fatalf("get run: %+v, %v", found, err)
	}
	if len(found.Messages) != 2 || found.Messages[0].Content != "first" || found.Messages[1].Content != "second" {
		t.Fatalf("message order: %+v", found.Messages)
	}
	if found.Status != session.RunStatusCompleted || found.Content != "finished" {
		t.Fatalf("updated run: %+v", found)
	}
	if found.ParentRunID != "parent-workflow" || found.NodeID != "agent-node" {
		t.Fatalf("run relationship: %+v", found)
	}
	if found.Metadata["project_id"] != "contract-project" || found.Metrics.TotalTokens != 10 ||
		found.Metrics.OriginalContextTokens != 30 || found.Metrics.FinalContextTokens != 20 ||
		found.Metrics.ContextCompactions != 1 || len(found.ToolExecutions) != 1 || found.Parsed == nil {
		t.Fatalf("run observability round trip: %+v", found)
	}
	if hidden, _ := store.GetRun(ctx, db.RunQuery{RunID: run.RunID, UserID: "other"}); hidden != nil {
		t.Fatal("run escaped user scope")
	}
	approvalRun := &session.RunOutput{
		RunID: uuid.NewString(), SessionID: id, AgentID: s.AgentID, UserID: s.UserID,
		Status: session.RunStatusRunning,
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "approve"},
			{Role: models.RoleAssistant, ToolCalls: []models.ToolCall{{
				ID: "approval-call", Type: "function", Function: models.FunctionCall{Name: "write", Arguments: `{"value":"x"}`},
			}}},
		},
	}
	if err := store.AppendRun(ctx, approvalRun); err != nil {
		t.Fatalf("append approval run: %v", err)
	}
	approvalRun.Status = session.RunStatusPaused
	approvalRun.PendingToolCalls = cloneToolCalls(approvalRun.Messages[1].ToolCalls)
	approvalRun.NextToolIndex = 0
	approvalRun.ModelRound = 2
	approvalRequirement := requirement.Requirement{
		ID: "approval-requirement", RunID: approvalRun.RunID, AgentID: approvalRun.AgentID,
		SessionID: approvalRun.SessionID, UserID: approvalRun.UserID, Kind: requirement.KindToolApproval,
		ToolCall: approvalRun.PendingToolCalls[0], Status: requirement.StatusPending, CreatedAt: time.Now().UTC(),
	}
	approvalRun.SetActiveRequirement(approvalRequirement)
	if err := store.UpdateRun(ctx, approvalRun, session.RunStatusRunning); err != nil {
		t.Fatalf("pause approval run: %v", err)
	}
	storedApproval, err := store.GetRun(ctx, db.RunQuery{RunID: approvalRun.RunID, UserID: s.UserID})
	if err != nil || storedApproval == nil || storedApproval.ActiveRequirement() == nil {
		t.Fatalf("get paused approval run: %+v, %v", storedApproval, err)
	}
	if storedApproval.ModelRound != 2 || len(storedApproval.PendingToolCalls) != 1 ||
		storedApproval.ActiveRequirement().ToolCall.Function.Arguments != `{"value":"x"}` {
		t.Fatalf("approval continuation round trip: %+v", storedApproval)
	}
	resolvedAt := time.Now().UTC()
	storedApproval.Requirements[0].Status = requirement.StatusApproved
	storedApproval.Requirements[0].ResolvedAt = &resolvedAt
	storedApproval.Requirements[0].Resolution = &requirement.Resolution{
		ActorID: "reviewer", Action: requirement.ActionApprove, At: resolvedAt,
	}
	storedApproval.Status = session.RunStatusRunning
	if err := store.UpdateRun(ctx, storedApproval, session.RunStatusPaused); err != nil {
		t.Fatalf("claim approval run: %v", err)
	}
	claimed, err := store.GetRun(ctx, db.RunQuery{RunID: approvalRun.RunID, UserID: s.UserID})
	if err != nil || claimed == nil || claimed.Requirements[0].Resolution == nil ||
		claimed.Requirements[0].Resolution.ActorID != "reviewer" {
		t.Fatalf("approval claim round trip: %+v, %v", claimed, err)
	}
	wrongParent := &session.RunOutput{RunID: uuid.NewString(), SessionID: id, AgentID: "other-agent", UserID: s.UserID, Status: session.RunStatusCompleted}
	if err := store.AppendRun(ctx, wrongParent); err == nil {
		t.Fatal("run with wrong parent component was accepted")
	}

	teamSession := &session.TeamSession{SessionID: uuid.NewString(), TeamID: "contract-team", UserID: s.UserID}
	if err := store.CreateTeamSession(ctx, teamSession); err != nil {
		t.Fatalf("create team: %v", err)
	}
	teamState := map[string]any{"ok": true}
	if revision, err := store.UpdateTeamSessionFields(ctx, db.SessionFieldsUpdate{SessionID: teamSession.SessionID, ExpectedRevision: 1, SessionState: teamState}); err != nil || revision != 2 {
		t.Fatalf("update team: rev=%d err=%v", revision, err)
	}
	teamRun := &session.RunOutput{
		RunID: uuid.NewString(), SessionID: teamSession.SessionID, TeamID: teamSession.TeamID, UserID: teamSession.UserID,
		Status: session.RunStatusRunning, Metadata: map[string]string{"project_id": "team-project"},
	}
	if err := store.AppendRun(ctx, teamRun); err != nil {
		t.Fatalf("append team run: %v", err)
	}
	teamRun.Status = session.RunStatusCompleted
	teamRun.Metrics = session.RunMetrics{ModelCalls: 2, ToolCalls: 1, TotalTokens: 9}
	teamRun.ToolExecutions = []session.ToolExecution{{CallID: "delegate-1", Name: "delegate_to_member", Status: "success"}}
	if err := store.UpdateRun(ctx, teamRun, session.RunStatusRunning); err != nil {
		t.Fatalf("update team run: %v", err)
	}
	storedTeamRun, err := store.GetRun(ctx, db.RunQuery{RunID: teamRun.RunID, UserID: teamSession.UserID})
	if err != nil || storedTeamRun == nil || storedTeamRun.Metadata["project_id"] != "team-project" || storedTeamRun.Metrics.TotalTokens != 9 || len(storedTeamRun.ToolExecutions) != 1 {
		t.Fatalf("team observability round trip: %+v, %v", storedTeamRun, err)
	}
	workflowSession := &session.WorkflowSession{SessionID: uuid.NewString(), WorkflowID: "contract-workflow", UserID: s.UserID}
	if err := store.CreateWorkflowSession(ctx, workflowSession); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	if revision, err := store.UpdateWorkflowSessionFields(ctx, db.SessionFieldsUpdate{SessionID: workflowSession.SessionID, ExpectedRevision: 1, Summary: stringPtr("done")}); err != nil || revision != 2 {
		t.Fatalf("update workflow: rev=%d err=%v", revision, err)
	}
	workflowRun := &session.RunOutput{
		RunID: uuid.NewString(), SessionID: workflowSession.SessionID, WorkflowID: workflowSession.WorkflowID, UserID: workflowSession.UserID,
		Status:   session.RunStatusRunning,
		Workflow: &workflowschema.RunData{SchemaVersion: 1, DefinitionVersion: "v1", RootResult: &workflowschema.NodeResult{NodeID: "root", Status: workflowschema.NodeStatusRunning}},
		Metadata: map[string]string{"project_id": "workflow-project"}, Metrics: session.RunMetrics{DurationMS: 5},
	}
	if err := store.AppendRun(ctx, workflowRun); err != nil {
		t.Fatalf("append workflow run: %v", err)
	}
	workflowRun.Status = session.RunStatusPaused
	workflowRun.Workflow.Requirement = &workflowschema.Requirement{ID: "approval", NodeID: "write", Kind: workflowschema.RequirementConfirmation}
	if err := store.UpdateRun(ctx, workflowRun, session.RunStatusRunning); err != nil {
		t.Fatalf("pause workflow run: %v", err)
	}
	storedWorkflowRun, err := store.GetRun(ctx, db.RunQuery{RunID: workflowRun.RunID, UserID: workflowSession.UserID})
	if err != nil || storedWorkflowRun == nil || storedWorkflowRun.Workflow == nil || storedWorkflowRun.Workflow.Requirement == nil {
		t.Fatalf("stored workflow run: %+v, %v", storedWorkflowRun, err)
	}
	if storedWorkflowRun.Metadata["project_id"] != "workflow-project" || storedWorkflowRun.Metrics.DurationMS != 5 {
		t.Fatalf("workflow observability round trip: %+v", storedWorkflowRun)
	}

	now := time.Now().UTC()
	for _, user := range []string{"alice", "bob"} {
		if err := store.CreateSession(ctx, &session.AgentSession{SessionID: uuid.NewString(), AgentID: "scoped", UserID: user, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create scoped: %v", err)
		}
	}
	items, err := store.ListSessions(ctx, db.SessionQuery{ComponentID: "scoped", UserID: "alice", Limit: 1})
	if err != nil || len(items) != 1 || items[0].UserID != "alice" {
		t.Fatalf("scoped list: %+v, %v", items, err)
	}

	mem := &memory.UserMemory{MemoryID: uuid.NewString(), Memory: "remember", UserID: s.UserID, AgentID: s.AgentID}
	if err := store.CreateMemory(ctx, mem); err != nil {
		t.Fatalf("create memory: %v", err)
	}
	memories, err := store.GetMemories(ctx, s.UserID, s.AgentID, 1)
	if err != nil || len(memories) != 1 {
		t.Fatalf("memories: %+v, %v", memories, err)
	}
	if err := store.DeleteMemory(ctx, mem.MemoryID); err != nil {
		t.Fatalf("delete memory: %v", err)
	}
	if err := store.DeleteMemory(ctx, mem.MemoryID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("delete missing memory: %v", err)
	}

	concurrent := &session.AgentSession{SessionID: uuid.NewString(), AgentID: "concurrent", UserID: "alice"}
	if err := store.CreateSession(ctx, concurrent); err != nil {
		t.Fatalf("create concurrent: %v", err)
	}
	left, _ := store.ReadSession(ctx, concurrent.SessionID)
	right, _ := store.ReadSession(ctx, concurrent.SessionID)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, candidate := range []*session.AgentSession{left, right} {
		go func(s *session.AgentSession) {
			<-start
			s.Summary = uuid.NewString()
			results <- store.UpdateSession(ctx, s)
		}(candidate)
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, db.ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results: successes=%d conflicts=%d", successes, conflicts)
	}
}

func cloneToolCalls(calls []models.ToolCall) []models.ToolCall {
	cloned := make([]models.ToolCall, len(calls))
	copy(cloned, calls)
	return cloned
}

func stringPtr(value string) *string { return &value }

// WorkflowResumeContract verifies durable pause/resume through a storage adapter.
func WorkflowResumeContract(t *testing.T, store db.Store) {
	t.Helper()
	registry := workflow.NewRegistry()
	registry.RegisterExecutor("contract-executor", workflow.ExecutorFunc(func(_ context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
		return workflow.NodeOutput{Value: input.Value}, nil
	}))
	node := workflow.ExecutorNode("contract-node", "contract-executor")
	node.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementConfirmation}
	wf, err := workflow.New(workflow.Config{
		Definition: workflow.Definition{ID: "contract-resume-" + uuid.NewString(), Version: "v1", Root: node},
		Registry:   registry,
		DB:         store,
	})
	if err != nil {
		t.Fatalf("new workflow: %v", err)
	}
	paused, err := wf.Run(context.Background(), workflow.RunRequest{Input: "value", UserID: "contract-user"})
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("pause workflow: %+v, %v", paused, err)
	}
	completed, err := wf.Resume(context.Background(), workflow.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: workflow.ResumeActionApprove, UserID: paused.UserID,
	})
	if err != nil || completed.Status != session.RunStatusCompleted || completed.RunID != paused.RunID {
		t.Fatalf("resume workflow: %+v, %v", completed, err)
	}
}
