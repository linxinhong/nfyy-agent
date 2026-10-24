package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	executionpkg "github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestWorkflow_TenantExecutionRequiresNamespaceStore(t *testing.T) {
	w, err := New(WorkflowConfig{
		ID: "tenant-gate", DB: in_memory.New(),
		Steps: []Step{{Name: "step", Function: func(_ context.Context, input StepInput) (StepOutput, error) {
			return StepOutput{Content: input.Input}, nil
		}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := executionpkg.ContextWithNamespace(context.Background(), "tenant")
	if _, err := w.RunWith(ctx, session.RunRequest{Input: session.RunInput{Message: "blocked"}}); !errors.Is(err, dbpkg.ErrNamespaceStoreRequired) {
		t.Fatalf("RunWith tenant adapter gate = %v", err)
	}
	if _, err := w.Run(ctx, "blocked"); !errors.Is(err, dbpkg.ErrNamespaceStoreRequired) {
		t.Fatalf("Run tenant adapter gate = %v", err)
	}
}

func TestWorkflow_TwoStep_Functions(t *testing.T) {
	ctx := context.Background()
	store := in_memory.New()

	step1 := Step{
		Name: "step1",
		Function: func(_ context.Context, input StepInput) (StepOutput, error) {
			return StepOutput{Content: "step1(" + input.Input + ")"}, nil
		},
	}
	step2 := Step{
		Name: "step2",
		Function: func(_ context.Context, input StepInput) (StepOutput, error) {
			if input.PreviousOutput == "" {
				t.Errorf("expected previous output to be set")
			}
			return StepOutput{Content: "step2(" + input.Input + ")"}, nil
		},
	}

	w, err := New(WorkflowConfig{
		ID:    "test-workflow",
		Name:  "Test Workflow",
		Steps: []Step{step1, step2},
		DB:    store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := w.RunWith(ctx, session.RunRequest{RunID: "workflow-run-1", Input: session.RunInput{Message: "hello"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "step2(step1(hello))" {
		t.Errorf("unexpected output: got %q, want %q", out.Content, "step2(step1(hello))")
	}
	if out.WorkflowID != "test-workflow" {
		t.Errorf("unexpected workflow id: %s", out.WorkflowID)
	}
	if out.RunID != "workflow-run-1" {
		t.Fatalf("unexpected run id: %q", out.RunID)
	}
	persisted, err := store.ReadWorkflowSession(ctx, out.SessionID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if len(persisted.Runs) != 2 || persisted.Runs[0].RunID == persisted.Runs[1].RunID {
		t.Fatalf("step run history = %+v, want two distinct runs", persisted.Runs)
	}
	for _, run := range persisted.Runs {
		if run.Status != session.RunStatusCompleted {
			t.Fatalf("step run status = %q, want completed", run.Status)
		}
	}
}

func TestWorkflow_AgentStep(t *testing.T) {
	ctx := context.Background()
	db := in_memory.New()

	model := mock.New()
	model.ResponseData = &models.ModelResponse{Content: "agent output"}

	ag, err := agent.New(agent.AgentConfig{
		ID:    "test-agent",
		Model: model,
		DB:    db,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	step1 := Step{
		Name:  "agent-step",
		Agent: ag,
	}
	step2 := Step{
		Name: "function-step",
		Function: func(_ context.Context, input StepInput) (StepOutput, error) {
			return StepOutput{Content: "function got: " + input.Input}, nil
		},
	}

	w, err := New(WorkflowConfig{
		ID:    "agent-workflow",
		Steps: []Step{step1, step2},
		DB:    db,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := w.RunWith(ctx, session.RunRequest{RunID: "legacy-root", Input: session.RunInput{Message: "hello"}, Metadata: map[string]string{"project_id": "project-a"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "function got: agent output" {
		t.Errorf("unexpected output: got %q, want %q", out.Content, "function got: agent output")
	}
	agentSessions, err := db.ListSessions(ctx, dbpkg.SessionQuery{ComponentID: "test-agent"})
	if err != nil || len(agentSessions) != 1 || len(agentSessions[0].Runs) != 1 {
		t.Fatalf("agent child runs = %+v, %v", agentSessions, err)
	}
	child := agentSessions[0].Runs[0]
	if child.ParentRunID != out.RunID || child.NodeID != "agent-step" || child.Metadata["project_id"] != "project-a" {
		t.Fatalf("agent child run = %+v", child)
	}
}

func TestWorkflow_SessionState(t *testing.T) {
	ctx := context.Background()
	db := in_memory.New()

	w, err := New(WorkflowConfig{
		ID:    "state-workflow",
		Steps: []Step{{Name: "noop", Function: func(_ context.Context, _ StepInput) (StepOutput, error) { return StepOutput{Content: "ok"}, nil }}},
		DB:    db,
		SessionState: map[string]any{
			"initial": "one",
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := w.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "ok" {
		t.Errorf("unexpected output: %s", out.Content)
	}

	sessionID := out.SessionID
	state, err := w.GetSessionState(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSessionState: %v", err)
	}
	if state["initial"] != "one" {
		t.Errorf("expected initial state to be preserved")
	}

	if err := w.UpdateSessionState(ctx, sessionID, map[string]any{"updated": "two"}); err != nil {
		t.Fatalf("UpdateSessionState: %v", err)
	}

	state, err = w.GetSessionState(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSessionState after update: %v", err)
	}
	if state["updated"] != "two" {
		t.Errorf("expected updated state to be set")
	}
	if state["initial"] != "one" {
		t.Errorf("expected initial state to be preserved")
	}
}

func TestWorkflow_Stream(t *testing.T) {
	ctx := context.Background()
	db := in_memory.New()

	model := mock.New()
	model.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: "agent "},
		{Event: "content_delta", Content: "output"},
	}

	ag, err := agent.New(agent.AgentConfig{
		ID:    "stream-agent",
		Model: model,
		DB:    db,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	w, err := New(WorkflowConfig{
		ID:    "stream-workflow",
		Steps: []Step{{Name: "stream-step", Agent: ag}},
		DB:    db,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stream, err := w.RunLegacyStream(ctx, "hello")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var sawStarted, sawCompleted bool
	for ev := range stream {
		switch ev.Type {
		case agent.EventTypeRunStarted:
			sawStarted = true
		case agent.EventTypeRunCompleted:
			sawCompleted = true
		}
	}
	if !sawStarted {
		t.Errorf("expected run_started event")
	}
	if !sawCompleted {
		t.Errorf("expected run_completed event")
	}
}

func TestWorkflow_StreamStepRunsRemainDistinct(t *testing.T) {
	ctx := context.Background()
	store := in_memory.New()
	w, err := New(WorkflowConfig{
		ID: "stream-steps",
		DB: store,
		Steps: []Step{
			{Name: "one", Function: func(context.Context, StepInput) (StepOutput, error) { return StepOutput{Content: "one"}, nil }},
			{Name: "two", Function: func(context.Context, StepInput) (StepOutput, error) { return StepOutput{Content: "two"}, nil }},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, err := w.RunStreamWith(ctx, session.RunRequest{RunID: "stream-workflow-run", Input: session.RunInput{Message: "go"}})
	if err != nil {
		t.Fatalf("RunStreamWith: %v", err)
	}
	var runIDs []string
	for event := range events {
		if event.Type == EventTypeStepCompleted {
			if event.RunOutput == nil || event.RunOutput.Status != session.RunStatusCompleted {
				t.Fatalf("step event output = %+v", event.RunOutput)
			}
			runIDs = append(runIDs, event.RunOutput.RunID)
		}
	}
	if len(runIDs) != 2 || runIDs[0] == runIDs[1] {
		t.Fatalf("step run ids = %v, want two distinct ids", runIDs)
	}
}
