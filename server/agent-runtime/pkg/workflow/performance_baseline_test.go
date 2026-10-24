package workflow

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type workflowCyclicBarrier struct {
	mu      sync.Mutex
	width   int
	arrived int
	gate    chan struct{}
}

func newWorkflowCyclicBarrier(width int) *workflowCyclicBarrier {
	return &workflowCyclicBarrier{width: width, gate: make(chan struct{})}
}

func (b *workflowCyclicBarrier) wait(ctx context.Context) error {
	b.mu.Lock()
	gate := b.gate
	b.arrived++
	if b.arrived == b.width {
		close(gate)
		b.arrived = 0
		b.gate = make(chan struct{})
	}
	b.mu.Unlock()
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func baselineParallelWorkflow(tb testing.TB, width int, active, peak *atomic.Int64) *Workflow {
	tb.Helper()
	registry := NewRegistry()
	barrier := newWorkflowCyclicBarrier(width)
	children := make([]NodeSpec, width)
	for index := 0; index < width; index++ {
		ref := fmt.Sprintf("executor-%03d", index)
		if err := registry.RegisterExecutor(ref, ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
			current := active.Add(1)
			for observed := peak.Load(); current > observed && !peak.CompareAndSwap(observed, current); observed = peak.Load() {
			}
			defer active.Add(-1)
			if err := barrier.wait(ctx); err != nil {
				return NodeOutput{}, err
			}
			return NodeOutput{Value: input.NodeID}, nil
		})); err != nil {
			tb.Fatalf("RegisterExecutor: %v", err)
		}
		children[index] = ExecutorNode(fmt.Sprintf("node-%03d", index), ref)
	}
	value, err := New(Config{
		Definition: Definition{ID: fmt.Sprintf("parallel-%03d", width), Version: "baseline-v1", Root: Parallel("root", children...)},
		Registry:   registry,
	})
	if err != nil {
		tb.Fatalf("workflow.New: %v", err)
	}
	return value
}

// BenchmarkWorkflowParallelNodes is the within-one-Run parallelism baseline;
// it is deliberately distinct from the Agent cross-Run benchmark.
func BenchmarkWorkflowParallelNodes(b *testing.B) {
	for _, width := range []int{1, 32, 256} {
		b.Run(fmt.Sprintf("nodes_%03d", width), func(b *testing.B) {
			var active atomic.Int64
			var peak atomic.Int64
			value := baselineParallelWorkflow(b, width, &active, &peak)
			b.ReportAllocs()
			b.ResetTimer()
			started := time.Now()
			for i := 0; i < b.N; i++ {
				output, err := value.Run(context.Background(), RunRequest{Input: "fixed-input"})
				if err != nil {
					b.Fatal(err)
				}
				if got := len(output.Workflow.RootResult.Children); got != width {
					b.Fatalf("parallel results = %d, want %d", got, width)
				}
			}
			elapsed := time.Since(started)
			b.StopTimer()
			b.ReportMetric(float64(b.N*width)/elapsed.Seconds(), "nodes/s")
			b.ReportMetric(float64(peak.Load()), "active-nodes-peak")
			b.ReportMetric(float64(runtime.NumGoroutine()), "goroutines-end")
		})
	}
}
