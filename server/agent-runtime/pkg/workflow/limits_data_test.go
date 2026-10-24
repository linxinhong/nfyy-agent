package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/sqlite"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

type boundedWriteStore struct {
	db.WorkflowStore
	limit int

	mu       sync.Mutex
	writes   int
	statuses []session.RunStatus
	maxBytes int
}

type rawHistoryStore struct {
	db.WorkflowStore
	loaded *session.RunOutput

	mu            sync.Mutex
	maxRawLenSeen int
	maxRawCapSeen int
}

func (s *rawHistoryStore) GetRun(context.Context, db.RunQuery) (*session.RunOutput, error) {
	return s.loaded, nil
}

func (s *rawHistoryStore) UpdateRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	s.mu.Lock()
	for _, requirement := range output.Workflow.RequirementHistory {
		if len(requirement.InputSchema) > s.maxRawLenSeen {
			s.maxRawLenSeen = len(requirement.InputSchema)
			s.maxRawCapSeen = cap(requirement.InputSchema)
		}
	}
	s.mu.Unlock()
	return s.WorkflowStore.UpdateRun(ctx, output, expected)
}

func (s *boundedWriteStore) record(output *session.RunOutput) error {
	raw, err := json.Marshal(output)
	if err != nil {
		return err
	}
	if len(raw) > s.limit {
		return errors.New("test store observed an over-budget write")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	s.statuses = append(s.statuses, output.Status)
	if len(raw) > s.maxBytes {
		s.maxBytes = len(raw)
	}
	return nil
}

func (s *boundedWriteStore) AppendRun(ctx context.Context, output *session.RunOutput) error {
	if err := s.record(output); err != nil {
		return err
	}
	return s.WorkflowStore.AppendRun(ctx, output)
}

func (s *boundedWriteStore) UpdateRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	if err := s.record(output); err != nil {
		return err
	}
	return s.WorkflowStore.UpdateRun(ctx, output, expected)
}

func (s *boundedWriteStore) completedWrites() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, status := range s.statuses {
		if status == session.RunStatusCompleted {
			count++
		}
	}
	return count
}

func TestWorkflowLimitsPayloadHugeInputStopsBeforeExecutionAndStorage(t *testing.T) {
	base := in_memory.New()
	store := &boundedWriteStore{WorkflowStore: base, limit: DefaultMaxRunEnvelopeBytes}
	registry := NewRegistry()
	var calls atomic.Int32
	if err := registry.RegisterExecutor("never", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		calls.Add(1)
		return NodeOutput{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{
		Definition: Definition{ID: "huge-input", Version: "v1", Root: ExecutorNode("node", "never")},
		Registry:   registry, DB: store, Limits: &Limits{MaxInputBytes: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"payload": strings.Repeat("x", 8<<20)}
	_, err = wf.Run(context.Background(), RunRequest{Input: input})
	if !errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("Run error = %v, want payload limit", err)
	}
	if calls.Load() != 0 || store.writes != 0 {
		t.Fatalf("calls=%d writes=%d, want both zero", calls.Load(), store.writes)
	}
	if got := len(input["payload"].(string)); got != 8<<20 {
		t.Fatalf("input was modified: length=%d", got)
	}
}

func TestWorkflowLimitsPayloadOversizedNodeOutputDoesNotReachDownstreamOrSuccess(t *testing.T) {
	if err := validateNodeOutput(NodeOutput{Value: strings.Repeat("x", 4096)}, 256); !errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("direct node output validation = %v, want payload limit", err)
	}
	base := in_memory.New()
	store := &boundedWriteStore{WorkflowStore: base, limit: 16 << 10}
	registry := NewRegistry()
	var downstream atomic.Int32
	if err := registry.RegisterExecutor("large", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: strings.Repeat("x", 4096)}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("downstream", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		downstream.Add(1)
		return NodeOutput{Value: "wrong"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{
		Definition: Definition{ID: "large-output", Version: "v1", Root: Sequence("root", ExecutorNode("large", "large"), ExecutorNode("downstream", "downstream"))},
		Registry:   registry, DB: store, Limits: &Limits{MaxNodeOutputBytes: 256, MaxRunEnvelopeBytes: store.limit},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := wf.Run(context.Background(), RunRequest{RunID: "large-output-run", Input: "input"})
	if !errors.Is(err, ErrPayloadLimitExceeded) || out.Status != session.RunStatusError {
		t.Fatalf("Run = status %s error %v, want auditable payload error", out.Status, err)
	}
	if downstream.Load() != 0 {
		t.Fatalf("downstream calls = %d, want 0", downstream.Load())
	}
	if store.completedWrites() != 0 {
		t.Fatal("store observed a successful write")
	}
	stored, getErr := base.GetRun(context.Background(), db.RunQuery{RunID: out.RunID})
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Status != session.RunStatusError || !strings.Contains(stored.Error, "payload limit exceeded") {
		t.Fatalf("stored terminal = status %s error %q", stored.Status, stored.Error)
	}
}

func TestWorkflowLimitsPayloadParallelAggregateCannotBypassNodeBudget(t *testing.T) {
	registry := NewRegistry()
	children := make([]NodeSpec, 8)
	for i := range children {
		name := string(rune('a' + i))
		if err := registry.RegisterExecutor(name, ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
			return NodeOutput{Value: strings.Repeat(name, 24)}, nil
		})); err != nil {
			t.Fatal(err)
		}
		children[i] = ExecutorNode("node-"+name, name)
	}
	wf, err := New(Config{
		Definition: Definition{ID: "aggregate", Version: "v1", Root: Parallel("root", children...)},
		Registry:   registry, Limits: &Limits{MaxNodeOutputBytes: 200, MaxConcurrentCallbacks: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := wf.Run(context.Background(), RunRequest{Input: "input"})
	if !errors.Is(err, ErrPayloadLimitExceeded) || out.Status != session.RunStatusError {
		t.Fatalf("parallel Run = status %s error %v", out.Status, err)
	}
}

func TestWorkflowLimitsPayloadCumulativeStateDoesNotReachDownstream(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"first", "second"} {
		name := name
		if err := registry.RegisterExecutor(name, ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
			return NodeOutput{Value: name, StatePatch: map[string]any{name: strings.Repeat("x", 64)}}, nil
		})); err != nil {
			t.Fatal(err)
		}
	}
	var downstream atomic.Int32
	if err := registry.RegisterExecutor("downstream", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		downstream.Add(1)
		return NodeOutput{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{
		Definition: Definition{ID: "state-total", Version: "v1", Root: Sequence("root", ExecutorNode("first", "first"), ExecutorNode("second", "second"), ExecutorNode("downstream", "downstream"))},
		Registry:   registry, Limits: &Limits{MaxStateBytes: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := wf.Run(context.Background(), RunRequest{Input: "input"})
	if !errors.Is(err, ErrPayloadLimitExceeded) || out.Status != session.RunStatusError {
		t.Fatalf("Run = status %s error %v", out.Status, err)
	}
	if downstream.Load() != 0 {
		t.Fatalf("downstream calls = %d, want 0", downstream.Load())
	}
	if _, exists := out.Workflow.State["second"]; exists {
		t.Fatalf("rejected state patch was admitted: %#v", out.Workflow.State)
	}
}

func TestWorkflowLimitsPayloadPreservesNativeNumericStateTypes(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterExecutor("write", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "ok", StatePatch: map[string]any{"count": int64(7)}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("read", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		if _, ok := input.State["count"].(int64); !ok {
			return NodeOutput{}, errors.New("numeric state type changed")
		}
		return NodeOutput{Value: input.State["count"]}, nil
	})); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{
		Definition: Definition{ID: "numeric-state", Version: "v1", Root: Sequence("root", ExecutorNode("write", "write"), ExecutorNode("read", "read"))},
		Registry:   registry, Limits: &Limits{},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := wf.Run(context.Background(), RunRequest{Input: "input"})
	if err != nil || out.Status != session.RunStatusCompleted {
		t.Fatalf("Run = status %s error %v", out.Status, err)
	}
}

func TestWorkflowLimitsPayloadRejectsHistoricalOversizeWithoutOverwrite(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	var calls atomic.Int32
	if err := registry.RegisterExecutor("review", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		calls.Add(1)
		return NodeOutput{Value: "done"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	node := ExecutorNode("review", "review")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	wf, err := New(Config{
		Definition: Definition{ID: "history", Version: "v1", Root: node}, Registry: registry, DB: store,
		Limits: &Limits{MaxStateBytes: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := wf.Run(context.Background(), RunRequest{RunID: "history-run", Input: "input"})
	if err != nil {
		t.Fatal(err)
	}
	corrupt := cloneRunOutput(paused)
	corrupt.Workflow.State = map[string]any{"too_large": strings.Repeat("x", 1024)}
	if err := store.UpdateRun(context.Background(), corrupt, session.RunStatusPaused); err != nil {
		t.Fatal(err)
	}
	before, _ := store.GetRun(context.Background(), db.RunQuery{RunID: paused.RunID})
	_, err = wf.Resume(context.Background(), ResumeRequest{RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: ResumeActionApprove})
	if !errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("Resume error = %v, want payload limit", err)
	}
	after, _ := store.GetRun(context.Background(), db.RunQuery{RunID: paused.RunID})
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if !reflect.DeepEqual(beforeJSON, afterJSON) || calls.Load() != 0 {
		t.Fatalf("oversized history changed: executor calls=%d", calls.Load())
	}
}

func TestWorkflowLimitsPayloadSQLiteReopenPreservesProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := registry.RegisterExecutor("ok", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "ok", StatePatch: map[string]any{"saved": true}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterExecutor("large", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: strings.Repeat("x", 4096)}, nil
	})); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{
		Definition: Definition{ID: "sqlite-progress", Version: "v1", Root: Sequence("root", ExecutorNode("first", "ok"), ExecutorNode("second", "large"))},
		Registry:   registry, DB: store, Limits: &Limits{MaxNodeOutputBytes: 256},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := wf.Run(context.Background(), RunRequest{RunID: "sqlite-progress-run", Input: "input"})
	if !errors.Is(runErr, ErrPayloadLimitExceeded) || out.Status != session.RunStatusError {
		t.Fatalf("Run = status %s error %v", out.Status, runErr)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.GetRun(context.Background(), db.RunQuery{RunID: out.RunID})
	if err != nil {
		t.Fatal(err)
	}
	first, ok := stored.Workflow.NodeResults["first"]
	if !ok || first.Status != workflowschema.NodeStatusCompleted || stored.Workflow.State["saved"] != true {
		t.Fatalf("reopened progress = result %+v state %#v", first, stored.Workflow.State)
	}
}

func TestWorkflowLimitsPayloadResumeNormalizesWhitespacePaddedInternalRaw(t *testing.T) {
	base := in_memory.New()
	registry := NewRegistry()
	if err := registry.RegisterExecutor("review", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "done"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	node := ExecutorNode("review", "review")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation, InputSchema: json.RawMessage(`{}`)}
	config := Config{
		Definition: Definition{ID: "raw-history", Version: "v1", Root: node}, Registry: registry, DB: base,
		Limits: &Limits{MaxRunEnvelopeBytes: 16 << 10},
	}
	wf, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := wf.Run(context.Background(), RunRequest{RunID: "raw-history-run", Input: "input"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := base.GetRun(context.Background(), db.RunQuery{RunID: paused.RunID})
	if err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat(" ", 8<<20)
	loaded.Workflow.Requirement.InputSchema = json.RawMessage(padding + `{}` + padding)
	loaded.Workflow.RequirementHistory[0].InputSchema = json.RawMessage(padding + `{}` + padding)
	store := &rawHistoryStore{WorkflowStore: base, loaded: loaded}
	config.DB = store
	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	out, err := restarted.Resume(context.Background(), ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: ResumeActionApprove,
	})
	if err != nil || out.Status != session.RunStatusCompleted {
		t.Fatalf("Resume = status %s error %v", out.Status, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.maxRawLenSeen != len(`{}`) || store.maxRawCapSeen != len(`{}`) {
		t.Fatalf("persisted raw length/capacity = %d/%d, want 2/2", store.maxRawLenSeen, store.maxRawCapSeen)
	}
}
