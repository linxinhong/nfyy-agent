package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func cancelElsewhere(t *testing.T, store *in_memory.DB, runID string) {
	t.Helper()
	run, err := store.GetRun(context.Background(), db.RunQuery{RunID: runID})
	if err != nil || run == nil {
		t.Errorf("GetRun: %v", err)
		return
	}
	run.Status = session.RunStatusCancelled
	run.Error = context.Canceled.Error()
	if err := store.UpdateRun(context.Background(), run, session.RunStatusRunning); err != nil {
		t.Errorf("cancel elsewhere: %v", err)
	}
}

func TestStructuredWorkflowHeartbeatStopsRunCancelledElsewhere(t *testing.T) {
	store := in_memory.New()
	entered := make(chan struct{})
	registry := NewRegistry()
	registry.RegisterExecutor("block", ExecutorFunc(func(ctx context.Context, _ NodeInput) (NodeOutput, error) {
		close(entered)
		<-ctx.Done()
		return NodeOutput{}, ctx.Err()
	}))
	wf, err := New(Config{Definition: Definition{ID: "lease", Version: "v1", Root: ExecutorNode("block", "block")},
		Registry: registry, DB: store, RunLeaseTimeout: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-entered
		cancelElsewhere(t, store, "wf-lease")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := wf.Run(ctx, RunRequest{RunID: "wf-lease", Input: "x"})
	if !errors.Is(err, agent.ErrRunCancelledExternally) || out.Status != session.RunStatusCancelled || out.LeaseTimeout != 40*time.Millisecond {
		t.Fatalf("out = status %s lease %s, err = %v", out.Status, out.LeaseTimeout, err)
	}
}

func TestStructuredWorkflowCheckpointConflictStopsLaterNodes(t *testing.T) {
	store := in_memory.New()
	var laterRan atomic.Bool
	registry := NewRegistry()
	registry.RegisterExecutor("cancel", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		cancelElsewhere(t, store, "wf-ckpt")
		return NodeOutput{Value: "done"}, nil
	}))
	registry.RegisterExecutor("later", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		laterRan.Store(true)
		return NodeOutput{}, nil
	}))
	wf, err := New(Config{Definition: Definition{ID: "ckpt", Version: "v1", Root: Sequence("root", ExecutorNode("first", "cancel"), ExecutorNode("second", "later"))},
		Registry: registry, DB: store, RunLeaseTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	out, err := wf.Run(context.Background(), RunRequest{RunID: "wf-ckpt", Input: "x"})
	if !errors.Is(err, agent.ErrRunCancelledExternally) || out.Status != session.RunStatusCancelled || laterRan.Load() {
		t.Fatalf("status = %s, later ran = %v, err = %v", out.Status, laterRan.Load(), err)
	}
	stored, _ := store.GetRun(context.Background(), db.RunQuery{RunID: "wf-ckpt"})
	if stored.Status != session.RunStatusCancelled {
		t.Fatalf("stored status = %s", stored.Status)
	}
}

// blockingStepModel blocks until its context ends.
type blockingStepModel struct{ entered chan struct{} }

func (b *blockingStepModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (b *blockingStepModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not streamed")
}
func (*blockingStepModel) ID() string       { return "blocking" }
func (*blockingStepModel) Provider() string { return "test" }

func TestSequentialWorkflowHeartbeatStopsRunCancelledElsewhere(t *testing.T) {
	store := in_memory.New()
	model := &blockingStepModel{entered: make(chan struct{})}
	step, err := agent.New(agent.AgentConfig{ID: "step", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{ID: "seq-lease", Steps: []Step{{Name: "only", Agent: step}}, DB: store, RunLeaseTimeout: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-model.entered
		cancelElsewhere(t, store, "seq-run")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := wf.RunWith(ctx, session.RunRequest{RunID: "seq-run", Input: session.RunInput{Message: "x"}})
	if !errors.Is(err, agent.ErrRunCancelledExternally) || out.Status != session.RunStatusCancelled {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
}
