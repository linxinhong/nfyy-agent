package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/sqlite"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

type observingAgentStore struct {
	db.AgentStore
	legacyAppends atomic.Int32
	legacyUpdates atomic.Int32
}

type cancellationTool struct {
	name    string
	started chan struct{}
	once    sync.Once
	calls   atomic.Int32
	block   bool
}

func (t *cancellationTool) Name() string         { return t.name }
func (*cancellationTool) Description() string    { return "managed cancellation test" }
func (*cancellationTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *cancellationTool) Call(ctx context.Context, _ map[string]any) (any, error) {
	t.calls.Add(1)
	if !t.block {
		return "unexpected", nil
	}
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*observingAgentStore) SupportsNamespaceScopes() {}

func (s *observingAgentStore) AppendRun(ctx context.Context, output *session.RunOutput) error {
	s.legacyAppends.Add(1)
	return s.AgentStore.AppendRun(ctx, output)
}

func (s *observingAgentStore) UpdateRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	s.legacyUpdates.Add(1)
	return s.AgentStore.UpdateRun(ctx, output, expected)
}

func TestManagedManagerAgentRootRunUsesOnlyClaimBoundCommits(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "managed-agent.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer store.Close()
	dispatcher := sqlite.NewLeaseDispatcher(store)
	spy := &observingAgentStore{AgentStore: store}
	model := mock.New()
	model.ResponseData = &models.ModelResponse{Content: "managed result"}
	ag, err := New(AgentConfig{ID: "agent-a", UserID: "alice", Model: model, DB: spy})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	manager, err := runmanager.NewManaged(dispatcher, time.Second, runmanager.ManagedConfig{
		Scope: execution.NamespaceScope{Namespace: "tenant-a"}, Worker: runmanager.WorkerIdentity{WorkerID: "worker-a"}, Lease: time.Second,
	})
	if err != nil {
		t.Fatalf("run.NewManaged: %v", err)
	}
	started, err := manager.StartManaged(session.RunRequest{RunID: "managed-agent-run", UserID: "alice", Input: session.RunInput{Message: "hello"}}, ag.RunWith)
	if err != nil {
		t.Fatalf("StartManaged: %v", err)
	}
	if started.RunID != "managed-agent-run" || started.Namespace != "tenant-a" {
		t.Fatalf("started run = %+v", started)
	}

	deadline := time.Now().Add(time.Second)
	for {
		snapshot, getErr := dispatcher.GetManagedRun(context.Background(), execution.NamespaceScope{Namespace: "tenant-a"}, started.RunID)
		if getErr == nil && snapshot != nil && snapshot.Run.Status == session.RunStatusCompleted {
			if snapshot.Run.Content != "managed result" || snapshot.Revision < 2 {
				t.Fatalf("managed snapshot = %+v", snapshot)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed run did not complete: snapshot=%+v err=%v", snapshot, getErr)
		}
		time.Sleep(time.Millisecond)
	}
	if got := spy.legacyAppends.Load(); got != 0 {
		t.Fatalf("managed root used legacy AppendRun %d times", got)
	}
	if got := spy.legacyUpdates.Load(); got != 0 {
		t.Fatalf("managed root used legacy UpdateRun %d times", got)
	}
}

func TestManagedAgentStopsBeforeSchedulingNextToolAfterCancellation(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "managed-cancel.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer store.Close()
	dispatcher := sqlite.NewLeaseDispatcher(store)
	owner, err := runmanager.AcquireManagedRunOwner(context.Background(), "managed-tool-run", runmanager.ManagedConfig{
		Store: dispatcher, Scope: execution.NamespaceScope{Namespace: "tenant-a"},
		Worker: runmanager.WorkerIdentity{WorkerID: "worker-a"}, Lease: time.Second,
	})
	if err != nil {
		t.Fatalf("AcquireManagedRunOwner: %v", err)
	}
	first := &cancellationTool{name: "first", started: make(chan struct{}), block: true}
	second := &cancellationTool{name: "second", started: make(chan struct{})}
	model := mock.New()
	model.ResponseData = &models.ModelResponse{ToolCalls: []models.ToolCall{
		{ID: "call-1", Type: "function", Function: models.FunctionCall{Name: first.name, Arguments: `{}`}},
		{ID: "call-2", Type: "function", Function: models.FunctionCall{Name: second.name, Arguments: `{}`}},
	}}
	ag, err := New(AgentConfig{ID: "agent-a", UserID: "alice", Model: model, DB: store, Tools: []tools.Tool{first, second}})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	type result struct {
		output *session.RunOutput
		err    error
	}
	finished := make(chan result, 1)
	go func() {
		output, runErr := ag.RunWith(owner.Context(), session.RunRequest{
			RunID: "managed-tool-run", UserID: "alice", Input: session.RunInput{Message: "use tools"},
		})
		finished <- result{output: output, err: runErr}
	}()
	select {
	case <-first.started:
	case got := <-finished:
		t.Fatalf("agent exited before first tool: %+v, %v", got.output, got.err)
	case <-time.After(time.Second):
		t.Fatal("first tool did not start")
	}
	owner.Cancel(context.Canceled)
	select {
	case got := <-finished:
		if !errors.Is(got.err, context.Canceled) || got.output == nil || got.output.Status != session.RunStatusCancelled {
			t.Fatalf("cancelled run = %+v, %v", got.output, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not stop after ownership cancellation")
	}
	if got := first.calls.Load(); got != 1 {
		t.Fatalf("first tool calls = %d", got)
	}
	if got := second.calls.Load(); got != 0 {
		t.Fatalf("second tool was scheduled after cancellation: %d", got)
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := owner.Release(releaseCtx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
