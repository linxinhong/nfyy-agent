package workflow_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

func waitLimitedManaged(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func TestWorkflowLimitsConcurrencyPredicateSharesManagedResumeSlot(t *testing.T) {
	stores := sqliteManagedWorkflowStores(t, func() time.Time { return time.Now().UTC() })
	t.Cleanup(stores.close)
	scope := execution.NamespaceScope{Namespace: "workflow-limits-" + uuid.NewString()}
	ctx := execution.ContextWithNamespace(context.Background(), scope.Namespace)

	registry := workflow.NewRegistry()
	var active, peak atomic.Int32
	enter := func() func() {
		current := active.Add(1)
		for observed := peak.Load(); current > observed && !peak.CompareAndSwap(observed, current); observed = peak.Load() {
		}
		return func() { active.Add(-1) }
	}
	predicateStarted := make(chan struct{}, 1)
	releasePredicate := make(chan struct{})
	siblingStarted := make(chan struct{}, 1)
	if err := registry.RegisterPredicate("choose", workflow.PredicateFunc(func(ctx context.Context, input workflow.NodeInput) (bool, error) {
		defer enter()()
		predicateStarted <- struct{}{}
		select {
		case <-releasePredicate:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	})); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"review", "then", "else", "sibling"} {
		ref := ref
		if err := registry.RegisterExecutor(ref, workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
			defer enter()()
			if ref == "sibling" {
				siblingStarted <- struct{}{}
			}
			return workflow.NodeOutput{Value: ref}, nil
		})); err != nil {
			t.Fatal(err)
		}
	}
	review := workflow.ExecutorNode("review", "review")
	review.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementConfirmation}
	definition := workflow.Definition{
		ID: "managed-predicate-limit", Version: "v1",
		Root: workflow.Sequence("root", review, workflow.Parallel("parallel",
			workflow.Condition("condition", "choose", workflow.ExecutorNode("then", "then"), workflow.ExecutorNode("else", "else")),
			workflow.ExecutorNode("sibling", "sibling"),
		)),
	}
	limits := &workflow.Limits{MaxConcurrentCallbacks: 1}
	seed, err := workflow.New(workflow.Config{Definition: definition, Registry: registry, DB: stores.first, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := seed.Run(ctx, workflow.RunRequest{RunID: "managed-" + uuid.NewString(), UserID: "alice", Input: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != session.RunStatusPaused {
		t.Fatalf("seed status = %q, want paused", paused.Status)
	}

	runtime, err := workflow.New(workflow.Config{
		Definition: definition, Registry: registry, Limits: limits,
		ManagedResume: &runmanager.ManagedConfig{
			Store: stores.secondManaged, Scope: scope, Worker: runmanager.WorkerIdentity{WorkerID: "limits-worker"}, Lease: time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		out, resumeErr := runtime.Resume(ctx, workflow.ResumeRequest{
			RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: workflow.ResumeActionApprove, UserID: "alice",
		})
		if resumeErr == nil && out.Status != session.RunStatusCompleted {
			resumeErr = fmt.Errorf("managed resume status = %q", out.Status)
		}
		result <- resumeErr
	}()
	waitLimitedManaged(t, predicateStarted, "predicate did not enter managed resume")
	select {
	case <-siblingStarted:
		t.Fatal("sibling Executor entered while K=1 Predicate was active")
	default:
	}
	close(releasePredicate)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("managed resume did not finish")
	}
	if got := peak.Load(); got > 1 {
		t.Fatalf("managed Predicate/Executor peak = %d, want <= 1", got)
	}
}
