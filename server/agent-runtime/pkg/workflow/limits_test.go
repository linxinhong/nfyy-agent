package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func limitedTestRegistry(t *testing.T) *Registry {
	t.Helper()
	registry := NewRegistry()
	if err := registry.RegisterExecutor("noop", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: input.Value}, nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterPredicate("true", PredicateFunc(func(context.Context, NodeInput) (bool, error) {
		return true, nil
	})); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestWorkflowLimitsDefinitionExactNodeAndDepthBoundaries(t *testing.T) {
	registry := limitedTestRegistry(t)
	definition := Definition{
		ID:      "shape",
		Version: "v1",
		Root: Sequence("depth-1",
			Sequence("depth-2", ExecutorNode("depth-3-a", "noop")),
			ExecutorNode("depth-2-b", "noop"),
		),
	}

	if _, err := New(Config{Definition: definition, Registry: registry, Limits: &Limits{
		MaxDefinitionNodes: 4,
		MaxDefinitionDepth: 3,
	}}); err != nil {
		t.Fatalf("exact limits rejected: %v", err)
	}

	tests := []struct {
		name     string
		limits   Limits
		resource string
	}{
		{name: "nodes", limits: Limits{MaxDefinitionNodes: 3, MaxDefinitionDepth: 3}, resource: "nodes"},
		{name: "depth", limits: Limits{MaxDefinitionNodes: 4, MaxDefinitionDepth: 2}, resource: "depth"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(Config{Definition: definition, Registry: registry, Limits: &test.limits})
			var limitErr *DefinitionLimitError
			if !errors.Is(err, ErrDefinitionLimitExceeded) || !errors.As(err, &limitErr) || limitErr.Resource != test.resource {
				t.Fatalf("New error = %v, want %s DefinitionLimitError", err, test.resource)
			}
		})
	}
}

func TestWorkflowLimitsDefinitionExactJSONByteBoundary(t *testing.T) {
	registry := limitedTestRegistry(t)
	node := ExecutorNode("node", "noop")
	node.Config = json.RawMessage(`{ "message" : "<&>" }`)
	definition := Definition{ID: "bytes", Name: "UTF-8-界", Version: "v1", Root: node}
	raw, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := New(Config{Definition: definition, Registry: registry, Limits: &Limits{MaxDefinitionBytes: len(raw)}}); err != nil {
		t.Fatalf("exact byte limit rejected: %v", err)
	}
	_, err = New(Config{Definition: definition, Registry: registry, Limits: &Limits{MaxDefinitionBytes: len(raw) - 1}})
	var limitErr *DefinitionLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "bytes" || limitErr.Actual != len(raw) {
		t.Fatalf("New error = %#v, want exact bytes actual=%d", err, len(raw))
	}
}

func TestWorkflowLimitsDefinitionJSONMeasurementMatchesEncoding(t *testing.T) {
	deadline := time.Date(2026, 9, 7, 12, 34, 56, 123456789, time.FixedZone("offset", 8*60*60))
	thenNode := ExecutorNode("then-\n-<&-\u2028", "noop")
	thenNode.Name = string([]byte{'x', 0xff, 'y'})
	thenNode.Config = json.RawMessage(" { \n \"html\" : \"<>&\u2028\u2029\" } \t")
	thenNode.Policy = NodePolicy{Timeout: -time.Second, MaxAttempts: 2, Backoff: time.Millisecond, OnError: ErrorPolicySkip}
	thenNode.HumanReview = &HumanReview{
		Kind: RequirementConfirmation, Prompt: "approve\t<&", InputSchema: json.RawMessage(` { "type" : "boolean" } `), Deadline: &deadline,
	}
	definition := Definition{
		ID: "measure", Name: "界<&\n", Version: "v1",
		Root: Condition("condition", "true", thenNode, ExecutorNode("else", "noop")),
	}
	want, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	got, err := limitedDefinitionJSONSize(definition, len(want))
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if got != len(want) {
		t.Fatalf("measured bytes = %d, encoded bytes = %d\nencoded=%s", got, len(want), want)
	}
	if _, err := limitedDefinitionJSONSize(definition, len(want)-1); !errors.Is(err, ErrDefinitionLimitExceeded) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestWorkflowLimitsDefinitionCompactsLargeRawMessageWithoutSourceSizedCopy(t *testing.T) {
	registry := limitedTestRegistry(t)
	spaces := strings.Repeat(" ", 4<<20)
	node := ExecutorNode("node", "noop")
	node.Config = json.RawMessage(spaces + `{ "ok" : true }` + spaces)
	definition := Definition{ID: "raw-space", Version: "v1", Root: node}
	want, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{Definition: definition, Registry: registry, Limits: &Limits{MaxDefinitionBytes: len(want)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := wf.cfg.Definition.Root.Config
	if string(got) != `{"ok":true}` {
		t.Fatalf("normalized config = %q", got)
	}
	if cap(got) != len(got) {
		t.Fatalf("normalized config capacity = %d, want bounded %d", cap(got), len(got))
	}
}

func TestWorkflowLimitsDefinitionRejectNegativeConfiguration(t *testing.T) {
	registry := limitedTestRegistry(t)
	definition := Definition{ID: "negative", Version: "v1", Root: ExecutorNode("node", "noop")}
	tests := []struct {
		name   string
		limits Limits
		field  string
	}{
		{name: "nodes", limits: Limits{MaxDefinitionNodes: -1}, field: "MaxDefinitionNodes"},
		{name: "depth", limits: Limits{MaxDefinitionDepth: -1}, field: "MaxDefinitionDepth"},
		{name: "bytes", limits: Limits{MaxDefinitionBytes: -1}, field: "MaxDefinitionBytes"},
		{name: "callbacks", limits: Limits{MaxConcurrentCallbacks: -1}, field: "MaxConcurrentCallbacks"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(Config{Definition: definition, Registry: registry, Limits: &test.limits})
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("New error = %v, want field %s", err, test.field)
			}
		})
	}
	if _, err := New(Config{Steps: []Step{{Name: "legacy"}}, Limits: &Limits{}}); err == nil {
		t.Fatal("New accepted Limits without a structured Definition")
	}
}

func TestWorkflowLimitsDefinitionMalformedShapesFailWithinBudget(t *testing.T) {
	registry := limitedTestRegistry(t)
	tests := []struct {
		name       string
		definition func() Definition
		want       string
	}{
		{
			name: "cycle through then",
			definition: func() Definition {
				leaf := ExecutorNode("else", "noop")
				cycle := NodeSpec{ID: "cycle", Type: NodeTypeCondition, PredicateRef: "true", Else: &leaf}
				cycle.Then = &cycle
				return Definition{ID: "cycle", Version: "v1", Root: cycle}
			},
			want: "duplicate node id",
		},
		{
			name: "unknown type",
			definition: func() Definition {
				return Definition{ID: "unknown", Version: "v1", Root: NodeSpec{ID: "node", Type: NodeType("mystery")}}
			},
			want: "unsupported node type",
		},
		{
			name: "duplicate id",
			definition: func() Definition {
				return Definition{ID: "duplicate", Version: "v1", Root: Sequence("root", ExecutorNode("same", "noop"), ExecutorNode("same", "noop"))}
			},
			want: "duplicate node id",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(Config{
				Definition: test.definition(), Registry: registry,
				Limits: &Limits{MaxDefinitionNodes: 8, MaxDefinitionDepth: 8, MaxDefinitionBytes: 4096},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWorkflowLimitsDefinitionRejectsHiddenSerializedSubtrees(t *testing.T) {
	registry := limitedTestRegistry(t)
	cycle := ExecutorNode("cycle", "noop")
	cycle.Then = &cycle
	withChildren := ExecutorNode("hidden-children", "noop")
	withChildren.Children = make([]NodeSpec, DefaultMaxDefinitionNodes+1)

	tests := []struct {
		name string
		node NodeSpec
	}{
		{name: "step then cycle", node: cycle},
		{name: "step children", node: withChildren},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(Config{
				Definition: Definition{ID: "hidden", Version: "v1", Root: test.node},
				Registry:   registry, Limits: &Limits{},
			})
			if err == nil || !strings.Contains(err.Error(), "fields for another node type") {
				t.Fatalf("New error = %v", err)
			}
		})
	}
}

func TestWorkflowLimitsDefinitionBuildFreezesCallerDefinition(t *testing.T) {
	registry := limitedTestRegistry(t)
	if err := registry.RegisterExecutor("mutated", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "mutated"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	node := ExecutorNode("original-node", "noop")
	node.Config = json.RawMessage(`{"stable":true}`)
	definition := Definition{ID: "immutable", Name: "original", Version: "v1", Root: Sequence("root", node)}
	limits := &Limits{}
	wf, err := New(Config{Definition: definition, Registry: registry, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}

	definition.Name = "mutated"
	definition.Root.Children[0].ID = "mutated-node"
	definition.Root.Children[0].ExecutorRef = "mutated"
	definition.Root.Children[0].Config[2] = 'X'
	limits.MaxDefinitionNodes = -1

	out, err := wf.Run(context.Background(), RunRequest{Input: "original"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "original" || out.Workflow.Definition.Name != "original" || out.Workflow.RootResult.Children[0].NodeID != "original-node" {
		t.Fatalf("caller mutation reached workflow: output=%q definition=%+v result=%+v", out.Content, out.Workflow.Definition, out.Workflow.RootResult)
	}
	if got := string(out.Workflow.Definition.Root.Children[0].Config); got != `{"stable":true}` {
		t.Fatalf("frozen config = %q", got)
	}
}

func TestWorkflowLimitsDefinitionRunSnapshotAndExecutorConfigAreIsolated(t *testing.T) {
	registry := NewRegistry()
	var calls atomic.Int32
	if err := registry.RegisterExecutor("stable", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		if got := string(input.Config); got != `{"stable":true}` {
			return NodeOutput{}, fmt.Errorf("executor config = %q", got)
		}
		calls.Add(1)
		input.Config[2] = 'X'
		return NodeOutput{Value: input.NodeID}, nil
	})); err != nil {
		t.Fatal(err)
	}
	node := ExecutorNode("stable-node", "stable")
	node.Config = json.RawMessage(`{"stable":true}`)
	wf, err := New(Config{
		Definition: Definition{ID: "run-isolation", Version: "v1", Root: Sequence("root", node)},
		Registry:   registry, Limits: &Limits{},
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := wf.Run(context.Background(), RunRequest{Input: "first"})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := string(first.Workflow.Definition.Root.Children[0].Config); got != `{"stable":true}` {
		t.Fatalf("executor mutated run snapshot config = %q", got)
	}
	first.Workflow.Definition.Root.Children[0].ID = "mutated-node"
	first.Workflow.Definition.Root.Children[0].ExecutorRef = "missing"
	first.Workflow.Definition.Root.Children[0].Config[2] = 'Y'

	second, err := wf.Run(context.Background(), RunRequest{Input: "second"})
	if err != nil {
		t.Fatalf("second Run after output mutation: %v", err)
	}
	child := second.Workflow.RootResult.Children[0]
	if child.NodeID != "stable-node" || child.Output != "stable-node" {
		t.Fatalf("second result = %+v", child)
	}
	if calls.Load() != 2 {
		t.Fatalf("executor calls = %d, want 2", calls.Load())
	}
}

func TestWorkflowLimitsDefinitionFrozenSnapshotAppliesToRunAndRunEvents(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "Run"
		if stream {
			name = "RunEvents"
		}
		t.Run(name, func(t *testing.T) {
			registry := NewRegistry()
			var calls atomic.Int32
			if err := registry.RegisterExecutor("count", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
				calls.Add(1)
				return NodeOutput{Value: "called"}, nil
			})); err != nil {
				t.Fatal(err)
			}
			definition := Definition{ID: "start-check", Version: "v1", Root: Sequence("root", ExecutorNode("node", "count"))}
			wf, err := New(Config{
				Definition: definition,
				Registry:   registry,
				Limits:     &Limits{MaxDefinitionBytes: 512},
			})
			if err != nil {
				t.Fatal(err)
			}
			definition.Root.Children[0].ID = "mutated"
			definition.Root.Children[0].ExecutorRef = "missing"
			if !stream {
				out, runErr := wf.Run(context.Background(), RunRequest{Input: "value"})
				if runErr != nil || out.Status != session.RunStatusCompleted {
					t.Fatalf("Run = %+v, %v", out, runErr)
				}
			} else {
				events, err := wf.RunEvents(context.Background(), RunRequest{Input: "value"})
				if err != nil {
					t.Fatalf("RunEvents: %v", err)
				}
				var terminal Event
				for event := range events {
					terminal = event
				}
				if terminal.Type != EventTypeRunCompleted {
					t.Fatalf("terminal event = %+v, want completed", terminal)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("executor calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestWorkflowLimitsDefinitionOversizedStoredDefinitionDoesNotExecuteOrOverwrite(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	var calls atomic.Int32
	if err := registry.RegisterExecutor("reviewed", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		calls.Add(1)
		return NodeOutput{Value: "executed"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	node := ExecutorNode("reviewed", "reviewed")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	wf, err := New(Config{
		Definition: Definition{ID: "stored-limit", Version: "v1", Root: node},
		Registry:   registry,
		DB:         store,
		Limits:     &Limits{MaxDefinitionBytes: 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := wf.Run(context.Background(), RunRequest{RunID: "stored-limit-run", Input: "value"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	corrupt := cloneRunOutput(paused)
	corrupt.Workflow.Definition.Name = strings.Repeat("x", 513)
	if err := store.UpdateRun(context.Background(), corrupt, session.RunStatusPaused); err != nil {
		t.Fatalf("corrupt stored definition: %v", err)
	}

	_, err = wf.Resume(context.Background(), ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: ResumeActionApprove,
	})
	if !errors.Is(err, ErrDefinitionLimitExceeded) {
		t.Fatalf("Resume error = %v, want definition limit", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("executor calls = %d, want 0", calls.Load())
	}
	events, eventsErr := wf.ResumeEvents(context.Background(), ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: ResumeActionApprove,
	})
	if eventsErr != nil {
		t.Fatalf("ResumeEvents: %v", eventsErr)
	}
	var terminal Event
	for event := range events {
		terminal = event
	}
	if terminal.Type != EventTypeRunError || !strings.Contains(terminal.Error, "definition bytes limit exceeded") {
		t.Fatalf("ResumeEvents terminal = %+v, want definition limit", terminal)
	}
	if calls.Load() != 0 {
		t.Fatalf("executor calls after ResumeEvents = %d, want 0", calls.Load())
	}
	persisted, getErr := store.GetRun(context.Background(), db.RunQuery{RunID: paused.RunID})
	if getErr != nil {
		t.Fatal(getErr)
	}
	if persisted.Status != session.RunStatusPaused || persisted.Workflow.Requirement == nil || persisted.Workflow.Requirement.Resolution != nil {
		t.Fatalf("stored progress was overwritten: status=%q requirement=%+v", persisted.Status, persisted.Workflow.Requirement)
	}
}

func TestWorkflowLimitsDefinitionDefaultsAreFinite(t *testing.T) {
	registry := limitedTestRegistry(t)
	definition := Definition{ID: "defaults", Version: "v1", Root: ExecutorNode("node", "noop")}
	wf, err := New(Config{Definition: definition, Registry: registry, Limits: &Limits{}})
	if err != nil {
		t.Fatal(err)
	}
	if wf.cfg.Limits == nil {
		t.Fatal("resolved limits are nil")
	}
	want := Limits{
		MaxDefinitionNodes:     DefaultMaxDefinitionNodes,
		MaxDefinitionDepth:     DefaultMaxDefinitionDepth,
		MaxDefinitionBytes:     DefaultMaxDefinitionBytes,
		MaxConcurrentCallbacks: DefaultMaxConcurrentCallbacks,
		PersistenceTimeout:     DefaultPersistenceTimeout,
		MaxInputBytes:          DefaultMaxInputBytes,
		MaxNodeOutputBytes:     DefaultMaxNodeOutputBytes,
		MaxStateBytes:          DefaultMaxStateBytes,
		MaxRunEnvelopeBytes:    DefaultMaxRunEnvelopeBytes,
	}
	if *wf.cfg.Limits != want {
		t.Fatalf("resolved limits = %+v, want %+v", *wf.cfg.Limits, want)
	}
}

func TestWorkflowLimitsDefinitionLargeRejectedBeforeEncoding(t *testing.T) {
	registry := limitedTestRegistry(t)
	definition := Definition{
		ID:      "large",
		Version: "v1",
		Root:    ExecutorNode("node", "noop"),
	}
	definition.Root.Name = strings.Repeat("x", 2<<20)
	_, err := New(Config{Definition: definition, Registry: registry, Limits: &Limits{MaxDefinitionBytes: 1024}})
	if !errors.Is(err, ErrDefinitionLimitExceeded) {
		t.Fatalf("New error = %v, want definition limit", err)
	}
}

func TestWorkflowLimitsDefinitionErrorStringIsStable(t *testing.T) {
	err := (&DefinitionLimitError{Resource: "nodes", Limit: 4, Actual: 5}).Error()
	want := "workflow: definition nodes limit exceeded: limit=4 actual=5"
	if err != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if !errors.Is(&DefinitionLimitError{Resource: "nodes", Limit: 4, Actual: 5}, ErrDefinitionLimitExceeded) {
		t.Fatal("DefinitionLimitError does not unwrap to sentinel")
	}
}
