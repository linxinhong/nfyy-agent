package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func waitWorkflowSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func waitWorkflowRun(t *testing.T, result <-chan error, message string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal(message)
		return nil
	}
}

func concurrentPeak(active, peak *atomic.Int32) func() {
	current := active.Add(1)
	for observed := peak.Load(); current > observed && !peak.CompareAndSwap(observed, current); observed = peak.Load() {
	}
	return func() { active.Add(-1) }
}

type observingWorkflowStore struct {
	db.WorkflowStore
	completedNode string
	recorded      chan struct{}
}

func (s *observingWorkflowStore) UpdateRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	err := s.WorkflowStore.UpdateRun(ctx, output, expected)
	if err == nil && output.Workflow != nil && output.Workflow.NodeResults[s.completedNode].Status == "completed" {
		select {
		case s.recorded <- struct{}{}:
		default:
		}
	}
	return err
}

func nestedLimitedParallel(width, depth int, executorRef string) NodeSpec {
	children := make([]NodeSpec, width)
	for index := range children {
		leaf := ExecutorNode(fmt.Sprintf("leaf-%03d", index), executorRef)
		for level := 1; level < depth; level++ {
			leaf = Sequence(fmt.Sprintf("branch-%03d-depth-%d", index, level), leaf)
		}
		children[index] = leaf
	}
	return Parallel("root", children...)
}

func TestWorkflowLimitsConcurrencyNestedSharedPool(t *testing.T) {
	for _, limit := range []int{1, 4, 32} {
		t.Run(fmt.Sprintf("K=%d", limit), func(t *testing.T) {
			registry := NewRegistry()
			var active, peak, starts atomic.Int32
			entered := make(chan struct{}, limit)
			release := make(chan struct{})
			if err := registry.RegisterExecutor("gate", ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
				defer concurrentPeak(&active, &peak)()
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
					return NodeOutput{Value: input.NodeID}, nil
				case <-ctx.Done():
					return NodeOutput{}, ctx.Err()
				}
			})); err != nil {
				t.Fatal(err)
			}
			workflow, err := New(Config{
				Definition: Definition{ID: fmt.Sprintf("nested-%d", limit), Version: "v1", Root: nestedLimitedParallel(256, 8, "gate")},
				Registry:   registry,
				Limits:     &Limits{MaxConcurrentCallbacks: limit},
			})
			if err != nil {
				t.Fatal(err)
			}
			previousHook := limitedSchedulerWorkerStarted
			limitedSchedulerWorkerStarted = func() { starts.Add(1) }
			t.Cleanup(func() { limitedSchedulerWorkerStarted = previousHook })
			finished := make(chan error, 1)
			go func() {
				_, runErr := workflow.Run(context.Background(), RunRequest{Input: "start"})
				finished <- runErr
			}()
			for range limit {
				waitWorkflowSignal(t, entered, "limited callbacks did not enter gate")
			}
			if got := peak.Load(); got > int32(limit) {
				t.Fatalf("callback peak = %d, limit=%d", got, limit)
			}
			close(release)
			if err := waitWorkflowRun(t, finished, "nested limited workflow did not finish"); err != nil {
				t.Fatal(err)
			}
			if got, want := starts.Load(), int32(limit); got != want {
				t.Fatalf("worker starts = %d, want fixed pool of %d", got, want)
			}
			if got := active.Load(); got != 0 {
				t.Fatalf("active callbacks after Run = %d", got)
			}
		})
	}
}

func TestWorkflowLimitsConcurrencyCancelledQueuedNodesDoNotEnter(t *testing.T) {
	for iteration := 0; iteration < 2; iteration++ {
		registry := NewRegistry()
		var entries atomic.Int32
		started := make(chan struct{}, 1)
		if err := registry.RegisterExecutor("block", ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
			entries.Add(1)
			started <- struct{}{}
			<-ctx.Done()
			return NodeOutput{}, ctx.Err()
		})); err != nil {
			t.Fatal(err)
		}
		children := make([]NodeSpec, 256)
		for index := range children {
			children[index] = ExecutorNode(fmt.Sprintf("queued-%03d", index), "block")
		}
		workflow, err := New(Config{
			Definition: Definition{ID: fmt.Sprintf("cancel-%d", iteration), Version: "v1", Root: Parallel("root", children...)},
			Registry:   registry, Limits: &Limits{MaxConcurrentCallbacks: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan error, 1)
		go func() {
			_, runErr := workflow.Run(ctx, RunRequest{Input: "cancel"})
			finished <- runErr
		}()
		waitWorkflowSignal(t, started, "first queued callback did not start")
		cancel()
		if err := waitWorkflowRun(t, finished, "cancelled workflow did not collect workers"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
		if got := entries.Load(); got != 1 {
			t.Fatalf("callbacks entered after cancellation = %d, want 1", got)
		}
	}
}

func TestWorkflowLimitsConcurrencyRetryReleasesSharedSlot(t *testing.T) {
	registry := NewRegistry()
	var active, peak, attempts atomic.Int32
	firstAttempt := make(chan struct{}, 1)
	otherStarted := make(chan struct{}, 1)
	if err := registry.RegisterExecutor("retry", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		defer concurrentPeak(&active, &peak)()
		if attempts.Add(1) == 1 {
			firstAttempt <- struct{}{}
			return NodeOutput{}, errors.New("try again")
		}
		return NodeOutput{Value: "retried"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("other", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		defer concurrentPeak(&active, &peak)()
		otherStarted <- struct{}{}
		return NodeOutput{Value: "other"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	retrying := ExecutorNode("retry", "retry")
	retrying.Policy = NodePolicy{MaxAttempts: 2, Backoff: time.Second}
	workflow, err := New(Config{
		Definition: Definition{ID: "retry-slot", Version: "v1", Root: Parallel("root", retrying, ExecutorNode("other", "other"))},
		Registry:   registry, Limits: &Limits{MaxConcurrentCallbacks: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, runErr := workflow.Run(context.Background(), RunRequest{Input: "retry"})
		finished <- runErr
	}()
	waitWorkflowSignal(t, firstAttempt, "first retry attempt did not run")
	waitWorkflowSignal(t, otherStarted, "other ready callback did not run during retry backoff")
	if err := waitWorkflowRun(t, finished, "retrying workflow did not finish"); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("retry attempts = %d, want 2", got)
	}
	if got := peak.Load(); got > 1 {
		t.Fatalf("callback peak = %d, want <= 1", got)
	}
}

func TestWorkflowLimitsConcurrencyErrorCancelsRunningSibling(t *testing.T) {
	registry := NewRegistry()
	leftStarted := make(chan struct{}, 1)
	rightStarted := make(chan struct{}, 1)
	rightCancelled := make(chan struct{}, 1)
	releaseLeft := make(chan struct{})
	if err := registry.RegisterExecutor("left", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		leftStarted <- struct{}{}
		<-releaseLeft
		return NodeOutput{}, errors.New("left failed")
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("right", ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		rightStarted <- struct{}{}
		<-ctx.Done()
		rightCancelled <- struct{}{}
		return NodeOutput{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	workflow, err := New(Config{
		Definition: Definition{ID: "error-cancels", Version: "v1", Root: Parallel("root", ExecutorNode("left", "left"), ExecutorNode("right", "right"))},
		Registry:   registry, Limits: &Limits{MaxConcurrentCallbacks: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, runErr := workflow.Run(context.Background(), RunRequest{Input: "error"})
		finished <- runErr
	}()
	waitWorkflowSignal(t, leftStarted, "left callback did not start")
	waitWorkflowSignal(t, rightStarted, "right callback did not start")
	close(releaseLeft)
	waitWorkflowSignal(t, rightCancelled, "error branch did not cancel sibling callback")
	if err := waitWorkflowRun(t, finished, "error workflow did not collect sibling"); err == nil || err.Error() != "left failed" {
		t.Fatalf("Run error = %v, want left failed", err)
	}
}

func TestWorkflowLimitsConcurrencyPreservesOrderAndDurableProgress(t *testing.T) {
	store := &observingWorkflowStore{WorkflowStore: in_memory.New(), completedNode: "second-node", recorded: make(chan struct{}, 1)}
	registry := NewRegistry()
	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	secondDone := make(chan struct{}, 1)
	if err := registry.RegisterExecutor("first", ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		firstStarted <- struct{}{}
		select {
		case <-releaseFirst:
			return NodeOutput{Value: "first", StatePatch: map[string]any{"first": true}}, nil
		case <-ctx.Done():
			return NodeOutput{}, ctx.Err()
		}
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("second", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		secondDone <- struct{}{}
		return NodeOutput{Value: "second", StatePatch: map[string]any{"second": true}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	workflow, err := New(Config{
		Definition: Definition{ID: "ordered-progress", Version: "v1", Root: Parallel("root", ExecutorNode("first-node", "first"), ExecutorNode("second-node", "second"))},
		Registry:   registry, DB: store, Limits: &Limits{MaxConcurrentCallbacks: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	type runResult struct {
		output *session.RunOutput
		err    error
	}
	finished := make(chan runResult, 1)
	go func() {
		output, runErr := workflow.Run(context.Background(), RunRequest{RunID: "ordered-progress", Input: "value"})
		finished <- runResult{output: output, err: runErr}
	}()
	waitWorkflowSignal(t, firstStarted, "first callback did not start")
	waitWorkflowSignal(t, secondDone, "second callback did not finish")
	waitWorkflowSignal(t, store.recorded, "second callback result was not durably recorded")
	stored, err := store.GetRun(context.Background(), db.RunQuery{RunID: "ordered-progress"})
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Workflow.NodeResults["second-node"].Status; got != "completed" {
		t.Fatalf("durable second status = %q, want completed", got)
	}
	close(releaseFirst)
	var result runResult
	select {
	case result = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("ordered workflow did not finish")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	outputs, ok := result.output.Workflow.Output.([]map[string]any)
	if !ok || len(outputs) != 2 || outputs[0]["node_id"] != "first-node" || outputs[1]["node_id"] != "second-node" {
		t.Fatalf("ordered parallel output = %#v", result.output.Workflow.Output)
	}
}

func TestWorkflowLimitsConcurrencyEventEntrypoints(t *testing.T) {
	registry := NewRegistry()
	var calls atomic.Int32
	if err := registry.RegisterExecutor("run", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		calls.Add(1)
		return NodeOutput{Value: "ok"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	streamed, err := New(Config{
		Definition: Definition{ID: "limited-events", Version: "v1", Root: Parallel("root", ExecutorNode("left", "run"), ExecutorNode("right", "run"))},
		Registry:   registry, Limits: &Limits{MaxConcurrentCallbacks: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := streamed.RunEvents(context.Background(), RunRequest{Input: "events"})
	if err != nil {
		t.Fatal(err)
	}
	terminal := EventType("")
	for event := range events {
		if event.Type == EventTypeRunCompleted || event.Type == EventTypeRunError || event.Type == EventTypeRunCancelled {
			terminal = event.Type
		}
	}
	if terminal != EventTypeRunCompleted || calls.Load() != 2 {
		t.Fatalf("RunEvents terminal=%q calls=%d", terminal, calls.Load())
	}

	store := in_memory.New()
	review := ExecutorNode("review", "run")
	review.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	resumable, err := New(Config{
		Definition: Definition{ID: "limited-resume-events", Version: "v1", Root: review},
		Registry:   registry, DB: store, Limits: &Limits{MaxConcurrentCallbacks: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := resumable.Run(context.Background(), RunRequest{Input: "resume"})
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("seed Run = %+v, %v", paused, err)
	}
	resumeEvents, err := resumable.ResumeEvents(context.Background(), ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: ResumeActionApprove,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal = ""
	for event := range resumeEvents {
		if event.Type == EventTypeRunCompleted || event.Type == EventTypeRunError || event.Type == EventTypeRunCancelled {
			terminal = event.Type
		}
	}
	if terminal != EventTypeRunCompleted {
		t.Fatalf("ResumeEvents terminal=%q", terminal)
	}
}
