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
)

func TestWorkflowLimitsDeadlineLateSuccessDoesNotCommit(t *testing.T) {
	registry := NewRegistry()
	var downstream atomic.Int32
	if err := registry.RegisterExecutor("late", ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		<-ctx.Done()
		return NodeOutput{Value: "late-success"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("downstream", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		downstream.Add(1)
		return NodeOutput{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{Definition: Definition{ID: "late", Version: "v1", Root: Sequence("root", ExecutorNode("late", "late"), ExecutorNode("downstream", "downstream"))}, Registry: registry, Limits: &Limits{RunTimeout: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := wf.Run(context.Background(), RunRequest{Input: "go"})
	if !errors.Is(err, context.DeadlineExceeded) || out.Status != session.RunStatusCancelled {
		t.Fatalf("Run = %+v, %v", out, err)
	}
	if downstream.Load() != 0 {
		t.Fatal("downstream entered after late success")
	}
}

func TestWorkflowLimitsDeadlinePredicateAndSkipObserveCancellation(t *testing.T) {
	registry := NewRegistry()
	var child atomic.Int32
	if err := registry.RegisterPredicate("late", PredicateFunc(func(ctx context.Context, input NodeInput) (bool, error) { <-ctx.Done(); return true, nil })); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("child", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) { child.Add(1); return NodeOutput{}, nil })); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("skip", ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		<-ctx.Done()
		return NodeOutput{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	condition := Condition("condition", "late", ExecutorNode("then", "child"), ExecutorNode("else", "child"))
	wf, err := New(Config{Definition: Definition{ID: "predicate", Version: "v1", Root: condition}, Registry: registry, Limits: &Limits{RunTimeout: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Run(context.Background(), RunRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("predicate Run error=%v", err)
	}
	if child.Load() != 0 {
		t.Fatal("condition branch entered after timeout")
	}
	node := ExecutorNode("skip", "skip")
	node.Policy = NodePolicy{OnError: ErrorPolicySkip}
	skip, err := New(Config{Definition: Definition{ID: "skip", Version: "v1", Root: node}, Registry: registry, Limits: &Limits{RunTimeout: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := skip.Run(context.Background(), RunRequest{}); !errors.Is(err, context.DeadlineExceeded) || out.Status != session.RunStatusCancelled {
		t.Fatalf("skip Run=%+v,%v", out, err)
	}
}

type deadlineStore struct {
	db.WorkflowStore
	seen chan time.Time
}

func (s *deadlineStore) UpdateRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	if deadline, ok := ctx.Deadline(); ok {
		select {
		case s.seen <- deadline:
		default:
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestWorkflowLimitsDeadlinePersistenceIsBounded(t *testing.T) {
	store := &deadlineStore{WorkflowStore: in_memory.New(), seen: make(chan time.Time, 1)}
	registry := NewRegistry()
	if err := registry.RegisterExecutor("ok", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) { return NodeOutput{Value: "ok"}, nil })); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{Definition: Definition{ID: "store", Version: "v1", Root: ExecutorNode("node", "ok")}, Registry: registry, DB: store, Limits: &Limits{PersistenceTimeout: 30 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = wf.Run(context.Background(), RunRequest{Input: "go"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error=%v", err)
	}
	select {
	case deadline := <-store.seen:
		if deadline.Sub(started) > time.Second {
			t.Fatalf("persistence deadline too long: %s", deadline.Sub(started))
		}
	case <-time.After(time.Second):
		t.Fatal("store did not observe deadline")
	}
}

func TestWorkflowLimitsDeadlineCancelsRetryBackoff(t *testing.T) {
	registry := NewRegistry()
	var attempts atomic.Int32
	if err := registry.RegisterExecutor("retry", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		attempts.Add(1)
		return NodeOutput{}, errors.New("retry")
	})); err != nil {
		t.Fatal(err)
	}
	node := ExecutorNode("retry", "retry")
	node.Policy = NodePolicy{MaxAttempts: 2, Backoff: time.Second}
	wf, err := New(Config{Definition: Definition{ID: "retry-deadline", Version: "v1", Root: node}, Registry: registry, Limits: &Limits{RunTimeout: 20 * time.Millisecond, MaxConcurrentCallbacks: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Run(context.Background(), RunRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error=%v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts=%d, want no retry after cancellation", attempts.Load())
	}
}
