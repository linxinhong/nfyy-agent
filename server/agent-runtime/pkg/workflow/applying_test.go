package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

// applyingWorkflow builds a one-step workflow and returns it with the store.
func applyingWorkflow(t *testing.T, executor Executor) (*Workflow, db.WorkflowStore) {
	t.Helper()
	store := in_memory.New()
	registry := NewRegistry()
	if err := registry.RegisterExecutor("step1", executor); err != nil {
		t.Fatalf("register executor: %v", err)
	}
	wf, err := New(Config{
		Definition: Definition{
			ID: "wf-applying", Version: "v1", Root: ExecutorNode("step1", "step1"),
		},
		Registry: registry,
		DB:       store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return wf, store
}

// applyingStoredRun persists a run whose step crashed mid-execution: the
// node is recorded as applying (side effects may have happened) and the run
// is still in running status — the durable state left by a crash.
func applyingStoredRun(t *testing.T, store db.WorkflowStore, wf *Workflow) *session.RunOutput {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.CreateWorkflowSession(ctx, &session.WorkflowSession{
		SessionID:    "sess-applying",
		WorkflowID:   wf.ID(),
		UserID:       "alice",
		SessionState: map[string]any{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("CreateWorkflowSession: %v", err)
	}
	stored := &session.RunOutput{
		RunID: "run-applying", WorkflowID: wf.ID(), SessionID: "sess-applying", UserID: "alice",
		Input: session.RunInput{Message: "go"}, Status: session.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
		Workflow: &workflowschema.RunData{
			SchemaVersion:     1,
			DefinitionVersion: "v1",
			Definition:        &wf.cfg.Definition,
			Input:             "go",
			State:             map[string]any{},
			NodeResults: map[string]NodeResult{
				"step1": {NodeID: "step1", NodeType: NodeTypeStep, Status: workflowschema.NodeStatusApplying, StartedAt: now},
			},
		},
	}
	if err := store.AppendRun(ctx, stored); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}
	return stored
}

// TestResumeApplyingFailsClosedWithoutRetry: resuming a run that crashed
// while a step was applying must fail closed for any action other than
// retry_applying, and must not re-run the step.
func TestResumeApplyingFailsClosedWithoutRetry(t *testing.T) {
	var sideEffects atomic.Int32
	executor := ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		sideEffects.Add(1)
		return NodeOutput{Value: "ok"}, nil
	})
	wf, _ := applyingWorkflow(t, executor)
	applyingStoredRun(t, wf.cfg.DB, wf)

	_, err := wf.Resume(context.Background(), ResumeRequest{
		RunID: "run-applying", UserID: "alice", Action: ResumeActionApprove,
	})
	if !errors.Is(err, ErrApplyingUnknown) {
		t.Fatalf("Resume error = %v, want ErrApplyingUnknown", err)
	}
	if got := sideEffects.Load(); got != 0 {
		t.Fatalf("side effects = %d, want 0 (fail-closed must not re-run the step)", got)
	}
}

// TestResumeApplyingRetryRerunsStep: an explicit retry_applying resume
// clears the claim and re-runs the step to completion.
func TestResumeApplyingRetryRerunsStep(t *testing.T) {
	var sideEffects atomic.Int32
	executor := ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		sideEffects.Add(1)
		return NodeOutput{Value: "ok"}, nil
	})
	wf, _ := applyingWorkflow(t, executor)
	applyingStoredRun(t, wf.cfg.DB, wf)

	out, err := wf.Resume(context.Background(), ResumeRequest{
		RunID: "run-applying", UserID: "alice", Action: ResumeActionRetryApplying,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	if got := sideEffects.Load(); got != 1 {
		t.Fatalf("side effects = %d, want 1 (retry ran the step exactly once)", got)
	}
	if out.Content != "ok" {
		t.Fatalf("content = %q, want ok", out.Content)
	}
	// The applying claim must be gone after recovery.
	if findApplyingNodeID(out) != "" {
		t.Fatalf("applying claim still present after recovery: %+v", out.Workflow.NodeResults)
	}
}

// TestStepClaimsApplyingBeforeExecute: the applying claim is durably
// recorded before the executor runs, so a crash during Execute leaves the
// "outcome unknown" marker behind.
func TestStepClaimsApplyingBeforeExecute(t *testing.T) {
	store := in_memory.New()
	var claimSeen atomic.Bool
	executor := ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		run, err := store.GetRun(ctx, db.RunQuery{RunID: "run-claim", UserID: "alice"})
		if err == nil && run != nil && run.Workflow != nil {
			if r, ok := run.Workflow.NodeResults["step1"]; ok && r.Status == workflowschema.NodeStatusApplying {
				claimSeen.Store(true)
			}
		}
		return NodeOutput{Value: "ok"}, nil
	})
	registry := NewRegistry()
	if err := registry.RegisterExecutor("step1", executor); err != nil {
		t.Fatalf("register executor: %v", err)
	}
	wf, err := New(Config{
		Definition: Definition{
			ID: "wf-claim", Version: "v1", Root: ExecutorNode("step1", "step1"),
		},
		Registry: registry,
		DB:       store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := wf.Run(context.Background(), RunRequest{RunID: "run-claim", UserID: "alice", Input: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	if !claimSeen.Load() {
		t.Fatalf("applying claim was not persisted before executor ran")
	}
}
