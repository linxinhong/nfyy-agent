package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

func newStreamBudgetWorkflow(t *testing.T, cfg streamcfg.Config, executor Executor) *Workflow {
	t.Helper()
	registry := NewRegistry()
	if err := registry.RegisterExecutor("exec", executor); err != nil {
		t.Fatal(err)
	}
	value, err := New(Config{
		Definition: Definition{ID: "stream-budget", Version: "v1", Root: ExecutorNode("node", "exec")},
		Registry:   registry,
		Stream:     cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func collectWorkflowEvents(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	var collected []Event
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return collected
			}
			collected = append(collected, event)
		case <-deadline.C:
			t.Fatal("workflow stream did not close")
		}
	}
}

func assertSingleWorkflowTerminal(t *testing.T, events []Event) Event {
	t.Helper()
	var terminals []Event
	for _, event := range events {
		switch event.Type {
		case EventTypeRunPaused, EventTypeRunCompleted, EventTypeRunError, EventTypeRunCancelled:
			terminals = append(terminals, event)
		}
	}
	if len(terminals) != 1 {
		t.Fatalf("terminal events = %d, want 1; events=%v", len(terminals), events)
	}
	if events[len(events)-1].Type != terminals[0].Type {
		t.Fatalf("terminal event is not last: events=%v", events)
	}
	return terminals[0]
}

func TestWorkflowStreamNormalConsumerHasExactlyOneTerminal(t *testing.T) {
	value := newStreamBudgetWorkflow(t, streamcfg.Config{}, ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "done"}, nil
	}))
	events, err := value.RunStream(context.Background(), RunRequest{Input: "normal"})
	if err != nil {
		t.Fatal(err)
	}
	collected := collectWorkflowEvents(t, events)
	terminal := assertSingleWorkflowTerminal(t, collected)
	if terminal.Type != EventTypeRunCompleted || terminal.RunOutput == nil {
		t.Fatalf("terminal = %+v, want completed output", terminal)
	}
	for index, event := range collected {
		if event.Sequence != uint64(index+1) {
			t.Fatalf("event[%d] sequence = %d, want %d", index, event.Sequence, index+1)
		}
	}
}

func TestWorkflowStreamNoConsumerTimesOutWithinItemBudgetAndWorkersExit(t *testing.T) {
	cancelObserved := make(chan struct{}, 2)
	registry := NewRegistry()
	if err := registry.RegisterExecutor("exec", ExecutorFunc(func(ctx context.Context, _ NodeInput) (NodeOutput, error) {
		select {
		case <-ctx.Done():
			cancelObserved <- struct{}{}
			return NodeOutput{}, ctx.Err()
		default:
			return NodeOutput{}, nil
		}
	})); err != nil {
		t.Fatal(err)
	}
	value, err := New(Config{
		Definition: Definition{ID: "unread-parallel", Version: "v1", Root: Parallel("root",
			ExecutorNode("first", "exec"), ExecutorNode("second", "exec"),
		)},
		Registry: registry,
		Stream:   streamcfg.Config{BufferItems: 3, MaxOutputBytes: 1 << 20, DeliveryTimeout: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := value.RunStream(context.Background(), RunRequest{Input: "unread"})
	if err != nil {
		t.Fatal(err)
	}
	if got := cap(events); got != 3 {
		t.Fatalf("channel capacity = %d, want configured bound 3", got)
	}
	for range 2 {
		select {
		case <-cancelObserved:
		case <-time.After(2 * time.Second):
			t.Fatal("workflow executor did not observe transport cancellation")
		}
	}
	collected := collectWorkflowEvents(t, events)
	if len(collected) > 3 {
		t.Fatalf("buffered events = %d, exceed configured bound 3", len(collected))
	}
	terminal := assertSingleWorkflowTerminal(t, collected)
	if terminal.Type != EventTypeRunError || terminal.Error != streamcfg.ErrDeliveryTimeout.Error() {
		t.Fatalf("terminal = %+v, want recognizable delivery timeout", terminal)
	}
}

func TestWorkflowStreamLargeTerminalFailsWithinByteBudget(t *testing.T) {
	value := newStreamBudgetWorkflow(t, streamcfg.Config{
		BufferItems: 16, MaxOutputBytes: 2048, DeliveryTimeout: time.Second,
	}, ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: strings.Repeat("x", 4096)}, nil
	}))
	events, err := value.RunStream(context.Background(), RunRequest{Input: "large"})
	if err != nil {
		t.Fatal(err)
	}
	collected := collectWorkflowEvents(t, events)
	terminal := assertSingleWorkflowTerminal(t, collected)
	if terminal.Type != EventTypeRunError || terminal.Error != streamcfg.ErrOutputBudgetExceeded.Error() {
		t.Fatalf("terminal = %+v, want recognizable output budget failure", terminal)
	}
	if terminal.RunOutput != nil {
		t.Fatal("budget failure terminal embedded the oversized RunOutput")
	}
}

func TestWorkflowStreamApprovalAndPauseTerminalsArePreserved(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterExecutor("review", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "approved"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	node := ExecutorNode("review", "review")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	value, err := New(Config{
		Definition: Definition{ID: "stream-review", Version: "v1", Root: node},
		Registry:   registry,
		Stream:     streamcfg.Config{BufferItems: 8, MaxOutputBytes: 1 << 20, DeliveryTimeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := value.RunStream(context.Background(), RunRequest{Input: "review"})
	if err != nil {
		t.Fatal(err)
	}
	// The declared bound has room for both approval events; consumption must
	// retain the requirement and its terminal pause.
	collected := collectWorkflowEvents(t, events)
	terminal := assertSingleWorkflowTerminal(t, collected)
	if terminal.Type != EventTypeRunPaused {
		t.Fatalf("terminal = %s, want run_paused", terminal.Type)
	}
	foundRequirement := false
	for _, event := range collected {
		if event.Type == EventTypeRequirementCreated {
			foundRequirement = true
		}
	}
	if !foundRequirement {
		t.Fatalf("approval requirement was dropped: events=%v", collected)
	}
}

type legacyCancellationModel struct {
	stopped chan struct{}
}

func (*legacyCancellationModel) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	return &models.ModelResponse{Content: "done"}, nil
}

func (m *legacyCancellationModel) ResponseStream(ctx context.Context, _ []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		defer close(m.stopped)
		for {
			select {
			case ch <- models.StreamEvent{Event: "content_delta", Content: "x"}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (*legacyCancellationModel) ID() string       { return "legacy-cancellation" }
func (*legacyCancellationModel) Provider() string { return "test" }

func TestLegacyWorkflowStreamNoConsumerStopsNestedProducerAndForwarder(t *testing.T) {
	model := &legacyCancellationModel{stopped: make(chan struct{})}
	nested, err := agent.New(agent.AgentConfig{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	value, err := New(Config{
		ID:     "legacy-stream-budget",
		Steps:  []Step{{Name: "nested", Agent: nested}},
		Stream: streamcfg.Config{BufferItems: 3, MaxOutputBytes: 1 << 20, DeliveryTimeout: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := value.RunLegacyStream(context.Background(), "unread")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("nested Agent producer did not exit")
	}
	collected := make([]agent.Event, 0, cap(events))
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				if len(collected) > 3 {
					t.Fatalf("buffered events = %d, exceed configured bound 3", len(collected))
				}
				terminal := collected[len(collected)-1]
				if terminal.Type != agent.EventTypeRunError || terminal.Err == nil || terminal.Err.Error() != streamcfg.ErrDeliveryTimeout.Error() {
					t.Fatalf("terminal = %+v, want delivery timeout", terminal)
				}
				return
			}
			collected = append(collected, event)
		case <-deadline.C:
			t.Fatal("legacy forwarding worker did not exit")
		}
	}
}
