package workflow

import (
	"context"
	"testing"
	"time"

	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

func TestWorkflowLimitsGateNoConsumerCancelsAndCollectsWorkers(t *testing.T) {
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
		Definition: Definition{ID: "limited-unread", Version: "v1", Root: Parallel("root",
			ExecutorNode("first", "exec"), ExecutorNode("second", "exec"),
		)},
		Registry: registry,
		Limits:   &Limits{MaxConcurrentCallbacks: 2},
		Stream:   streamcfg.Config{BufferItems: 3, MaxOutputBytes: 1 << 20, DeliveryTimeout: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := value.RunStream(context.Background(), RunRequest{Input: "unread"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-cancelObserved:
		case <-time.After(2 * time.Second):
			t.Fatal("limited worker did not observe no-consumer cancellation")
		}
	}
	collected := collectWorkflowEvents(t, events)
	if len(collected) > 3 {
		t.Fatalf("buffered events = %d, exceed configured bound 3", len(collected))
	}
	terminal := assertSingleWorkflowTerminal(t, collected)
	if terminal.Type != EventTypeRunError || terminal.Error != streamcfg.ErrDeliveryTimeout.Error() {
		t.Fatalf("terminal = %+v, want delivery timeout", terminal)
	}
}
