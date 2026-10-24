package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

type controlledStreamModel struct {
	events  int
	payload string
	sent    chan int
	started chan struct{}
	stopped chan struct{}
}

func (*controlledStreamModel) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	return &models.ModelResponse{Content: "done"}, nil
}

func (m *controlledStreamModel) ResponseStream(ctx context.Context, _ []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	if m.started != nil {
		close(m.started)
	}
	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		defer close(m.stopped)
		for index := 0; index < m.events; index++ {
			select {
			case ch <- models.StreamEvent{Event: "content_delta", Content: m.payload}:
				if m.sent != nil {
					m.sent <- index + 1
				}
			case <-ctx.Done():
				return
			}
		}
		select {
		case ch <- models.StreamEvent{Event: "done", Done: true}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func (*controlledStreamModel) ID() string       { return "controlled-stream" }
func (*controlledStreamModel) Provider() string { return "test" }

func newControlledStreamAgent(t *testing.T, model *controlledStreamModel, cfg streamcfg.Config) *agent.Agent {
	t.Helper()
	value, err := agent.New(agent.AgentConfig{Model: model, MaxConcurrentRuns: 1, Stream: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func collectAgentEvents(t *testing.T, events <-chan agent.Event) []agent.Event {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	var collected []agent.Event
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return collected
			}
			collected = append(collected, event)
		case <-deadline.C:
			t.Fatal("stream did not close")
		}
	}
}

func assertSingleAgentTerminal(t *testing.T, events []agent.Event) agent.Event {
	t.Helper()
	var terminals []agent.Event
	for _, event := range events {
		switch event.Type {
		case agent.EventTypeRunCompleted, agent.EventTypeRunPaused, agent.EventTypeRunError:
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

func TestAgentStreamNormalConsumerHasExactlyOneTerminal(t *testing.T) {
	model := &controlledStreamModel{events: 8, payload: "x", stopped: make(chan struct{})}
	value := newControlledStreamAgent(t, model, streamcfg.Config{})
	events, err := value.RunStream(context.Background(), "normal")
	if err != nil {
		t.Fatal(err)
	}
	collected := collectAgentEvents(t, events)
	terminal := assertSingleAgentTerminal(t, collected)
	if terminal.Type != agent.EventTypeRunCompleted {
		t.Fatalf("terminal = %s, want %s", terminal.Type, agent.EventTypeRunCompleted)
	}
	if got, want := len(collected), model.events+2; got != want {
		t.Fatalf("events = %d, want %d", got, want)
	}
}

func TestAgentStreamControlledSlowConsumerPreservesEvents(t *testing.T) {
	model := &controlledStreamModel{
		events:  8,
		payload: "x",
		sent:    make(chan int, 8),
		stopped: make(chan struct{}),
	}
	value := newControlledStreamAgent(t, model, streamcfg.Config{
		BufferItems: 4, MaxOutputBytes: 1 << 20, DeliveryTimeout: time.Second,
	})
	events, err := value.RunStream(context.Background(), "slow")
	if err != nil {
		t.Fatal(err)
	}
	if first := <-events; first.Type != agent.EventTypeRunStarted {
		t.Fatalf("first event = %s", first.Type)
	}
	// Three model events can be handed to the Agent while only two data slots
	// remain. Waiting on this producer signal deterministically establishes
	// backpressure before consumption resumes.
	for want := 1; want <= 3; want++ {
		select {
		case got := <-model.sent:
			if got != want {
				t.Fatalf("model event = %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("model did not reach the controlled backpressure point")
		}
	}
	collected := append([]agent.Event{{Type: agent.EventTypeRunStarted}}, collectAgentEvents(t, events)...)
	terminal := assertSingleAgentTerminal(t, collected)
	if terminal.Type != agent.EventTypeRunCompleted || len(collected) != model.events+2 {
		t.Fatalf("slow stream lost events: count=%d terminal=%s", len(collected), terminal.Type)
	}
}

func TestAgentStreamNoConsumerTimesOutWithinItemBudgetAndStopsProducer(t *testing.T) {
	model := &controlledStreamModel{events: 128, payload: "x", stopped: make(chan struct{})}
	value := newControlledStreamAgent(t, model, streamcfg.Config{
		BufferItems: 4, MaxOutputBytes: 1 << 20, DeliveryTimeout: 20 * time.Millisecond,
	})
	events, err := value.RunStream(context.Background(), "unread")
	if err != nil {
		t.Fatal(err)
	}
	if got := cap(events); got != 4 {
		t.Fatalf("channel capacity = %d, want configured bound 4", got)
	}
	select {
	case <-model.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("cooperative model producer did not exit")
	}
	collected := collectAgentEvents(t, events)
	if len(collected) > 4 {
		t.Fatalf("buffered events = %d, exceed configured bound 4", len(collected))
	}
	terminal := assertSingleAgentTerminal(t, collected)
	if !errors.Is(terminal.Err, streamcfg.ErrDeliveryTimeout) {
		t.Fatalf("terminal error = %v, want ErrDeliveryTimeout", terminal.Err)
	}
	if _, err := value.Run(context.Background(), "slot-released"); err != nil {
		t.Fatalf("stream worker did not release Agent run slot: %v", err)
	}
}

func TestAgentStreamLargeSingleEventFailsWithinByteBudget(t *testing.T) {
	model := &controlledStreamModel{events: 1, payload: strings.Repeat("x", 1024), stopped: make(chan struct{})}
	value := newControlledStreamAgent(t, model, streamcfg.Config{
		BufferItems: 4, MaxOutputBytes: 512, DeliveryTimeout: time.Second,
	})
	events, err := value.RunStream(context.Background(), "large")
	if err != nil {
		t.Fatal(err)
	}
	collected := collectAgentEvents(t, events)
	if len(collected) > 4 {
		t.Fatalf("buffered events = %d, exceed configured bound 4", len(collected))
	}
	terminal := assertSingleAgentTerminal(t, collected)
	if !errors.Is(terminal.Err, streamcfg.ErrOutputBudgetExceeded) {
		t.Fatalf("terminal error = %v, want ErrOutputBudgetExceeded", terminal.Err)
	}
	for _, event := range collected {
		if len(event.Content) >= 1024 {
			t.Fatal("over-budget payload was emitted")
		}
	}
}

func TestAgentStreamCumulativeLogicalByteBudget(t *testing.T) {
	model := &controlledStreamModel{events: 16, payload: "x", stopped: make(chan struct{})}
	value := newControlledStreamAgent(t, model, streamcfg.Config{
		BufferItems: 16, MaxOutputBytes: 512, DeliveryTimeout: time.Second,
	})
	events, err := value.RunStream(context.Background(), "cumulative")
	if err != nil {
		t.Fatal(err)
	}
	collected := collectAgentEvents(t, events)
	terminal := assertSingleAgentTerminal(t, collected)
	if !errors.Is(terminal.Err, streamcfg.ErrOutputBudgetExceeded) {
		t.Fatalf("terminal error = %v, want ErrOutputBudgetExceeded", terminal.Err)
	}
	messages := 0
	for _, event := range collected {
		if event.Type == agent.EventTypeMessage {
			messages++
		}
	}
	if messages >= model.events {
		t.Fatalf("cumulative budget admitted every model event: %d", messages)
	}
}

func TestAgentStreamCancellationStopsProducerAndIsTerminal(t *testing.T) {
	model := &controlledStreamModel{events: 128, payload: "x", started: make(chan struct{}), stopped: make(chan struct{})}
	value := newControlledStreamAgent(t, model, streamcfg.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	events, err := value.RunStream(ctx, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	if first := <-events; first.Type != agent.EventTypeRunStarted {
		t.Fatalf("first event = %s", first.Type)
	}
	select {
	case <-model.started:
	case <-time.After(2 * time.Second):
		t.Fatal("model producer did not start")
	}
	cancel()
	collected := append([]agent.Event{{Type: agent.EventTypeRunStarted}}, collectAgentEvents(t, events)...)
	terminal := assertSingleAgentTerminal(t, collected)
	if !errors.Is(terminal.Err, context.Canceled) {
		t.Fatalf("terminal error = %v, want context.Canceled", terminal.Err)
	}
	select {
	case <-model.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("cooperative model producer did not exit after cancellation")
	}
}
