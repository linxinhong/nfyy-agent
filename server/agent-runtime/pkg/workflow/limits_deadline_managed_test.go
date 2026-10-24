package workflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

// blockingProgressManagedStore lets Resume perform its initial paused-to-
// running commit, then blocks its first per-node durable-progress commit.
type blockingProgressManagedStore struct {
	runmanager.ManagedStore
	commits atomic.Int32
	seen    chan time.Time
}

func (s *blockingProgressManagedStore) CommitManagedRun(ctx context.Context, scope execution.NamespaceScope, claim *runmanager.Claim, commit runmanager.ManagedRunCommit) (uint64, error) {
	if s.commits.Add(1) == 2 {
		if deadline, ok := ctx.Deadline(); ok {
			select {
			case s.seen <- deadline:
			default:
			}
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return s.ManagedStore.CommitManagedRun(ctx, scope, claim, commit)
}

func TestWorkflowLimitsDeadlineManagedProgressIsBounded(t *testing.T) {
	stores := sqliteManagedWorkflowStores(t, func() time.Time { return time.Now().UTC() })
	t.Cleanup(stores.close)
	scope := execution.NamespaceScope{Namespace: "workflow-managed-deadline-" + uuid.NewString()}
	ctx := execution.ContextWithNamespace(context.Background(), scope.Namespace)

	registry := workflow.NewRegistry()
	if err := registry.RegisterExecutor("write", workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
		return workflow.NodeOutput{Value: "written"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	review := workflow.ExecutorNode("review", "write")
	review.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementConfirmation}
	definition := workflow.Definition{ID: "managed-progress-deadline", Version: "v1", Root: workflow.Sequence("root", review, workflow.ExecutorNode("after", "write"))}
	seed, err := workflow.New(workflow.Config{Definition: definition, Registry: registry, DB: stores.first})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := seed.Run(ctx, workflow.RunRequest{RunID: "run-" + uuid.NewString(), UserID: "alice", Input: "go"})
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("seed Run = %+v, %v", paused, err)
	}

	blocked := &blockingProgressManagedStore{ManagedStore: stores.secondManaged, seen: make(chan time.Time, 1)}
	runtime, err := workflow.New(workflow.Config{
		Definition: definition, Registry: registry,
		Limits:        &workflow.Limits{PersistenceTimeout: 30 * time.Millisecond},
		ManagedResume: &runmanager.ManagedConfig{Store: blocked, Scope: scope, Worker: runmanager.WorkerIdentity{WorkerID: "deadline-worker"}, Lease: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = runtime.Resume(ctx, workflow.ResumeRequest{RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: workflow.ResumeActionApprove, UserID: "alice"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("managed Resume error = %v, want persistence deadline", err)
	}
	select {
	case deadline := <-blocked.seen:
		if deadline.Sub(started) > time.Second {
			t.Fatalf("managed progress deadline too long: %s", deadline.Sub(started))
		}
	case <-time.After(time.Second):
		t.Fatal("managed progress commit did not observe a deadline")
	}
}
