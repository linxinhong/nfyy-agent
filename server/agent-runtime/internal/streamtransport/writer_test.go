package streamtransport

import (
	"context"
	"sync"
	"testing"
	"time"

	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

func TestLogicalPayloadBytesFailsClosedForCycleAndUnsupportedValue(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	if got := LogicalPayloadBytes(cyclic, 1<<20); got >= 0 {
		t.Fatalf("cyclic value size = %d, want fail-closed result", got)
	}
	if got := LogicalPayloadBytes(func() {}, 1<<20); got >= 0 {
		t.Fatalf("function value size = %d, want fail-closed result", got)
	}
}

func TestWriterConcurrentEmitAndFinishKeepsOneLastTerminal(t *testing.T) {
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		writer, events := New(ctx, cancel, streamcfg.Config{
			BufferItems: 16, MaxOutputBytes: 1 << 20, DeliveryTimeout: time.Second,
		}, func(value string, _ int) int { return len(value) }, func(error) string { return "failure" })
		gate := make(chan struct{})
		var workers sync.WaitGroup
		for range 8 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-gate
				writer.Emit("data", Data)
			}()
		}
		workers.Add(3)
		go func() {
			defer workers.Done()
			<-gate
			writer.Emit("terminal", Terminal)
		}()
		for range 2 {
			go func() {
				defer workers.Done()
				<-gate
				writer.Finish(nil)
			}()
		}
		close(gate)
		workers.Wait()

		collected := make([]string, 0, cap(events))
		for event := range events {
			collected = append(collected, event)
		}
		terminals := 0
		for _, event := range collected {
			if event == "terminal" || event == "failure" {
				terminals++
			}
		}
		if terminals != 1 || len(collected) == 0 || (collected[len(collected)-1] != "terminal" && collected[len(collected)-1] != "failure") {
			t.Fatalf("concurrent output = %v, want exactly one last terminal", collected)
		}
	}
}

func TestWriterDataByteBoundaryPreservesFailureReserve(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writer, events := New(ctx, cancel, streamcfg.Config{
		BufferItems: 2, MaxOutputBytes: 512, DeliveryTimeout: time.Second,
	}, func(value string, _ int) int { return len(value) }, func(error) string { return "failure" })
	if !writer.Emit(string(make([]byte, 256)), Data) {
		t.Fatal("event exactly at the data byte boundary was rejected")
	}
	if writer.Emit("x", Data) {
		t.Fatal("event beyond the reserved data byte boundary was admitted")
	}
	writer.Finish(nil)
	collected := make([]string, 0, 2)
	for event := range events {
		collected = append(collected, event)
	}
	if len(collected) != 2 || collected[1] != "failure" {
		t.Fatalf("events = %v, want boundary data followed by reserved failure terminal", collected)
	}
}

func TestLogicalPayloadBytesStopsAtLimit(t *testing.T) {
	value := []any{"small", make([]byte, 4096)}
	if got := LogicalPayloadBytes(value, 512); got <= 512 {
		t.Fatalf("oversized value size = %d, want greater than limit", got)
	}
}

func TestWriterDoesNotEmitOrAccountUnmeasurableFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writer, events := New(ctx, cancel, streamcfg.Config{
		BufferItems: 2, MaxOutputBytes: 512, DeliveryTimeout: time.Second,
	}, func(value int, _ int) int {
		if value == 0 {
			return -1
		}
		return 1024
	}, func(error) int { return 0 })
	if writer.Emit(1, Data) {
		t.Fatal("over-budget value was emitted")
	}
	writer.Finish(nil)
	if writer.bytes != 0 {
		t.Fatalf("accounted bytes = %d, want 0", writer.bytes)
	}
	if _, ok := <-events; ok {
		t.Fatal("unmeasurable fallback was emitted")
	}
}
