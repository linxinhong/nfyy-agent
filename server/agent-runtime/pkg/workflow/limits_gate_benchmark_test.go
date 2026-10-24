package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// BenchmarkWorkflowLimitedSchedulerSaturated reports the throughput of the
// fixed acceptance workload (width 256, depth 8) at K=1/4/32. It is a
// saturated queue sample, not an equal-concurrency comparison with legacy.
func BenchmarkWorkflowLimitedSchedulerSaturated(b *testing.B) {
	for _, limit := range []int{1, 4, 32} {
		b.Run(fmt.Sprintf("width256_depth8_K%d", limit), func(b *testing.B) {
			registry := NewRegistry()
			if err := registry.RegisterExecutor("work", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
				return NodeOutput{Value: input.NodeID}, nil
			})); err != nil {
				b.Fatal(err)
			}
			value, err := New(Config{
				Definition: Definition{ID: "saturated", Version: "v1", Root: nestedLimitedParallel(256, 8, "work")},
				Registry:   registry, Limits: &Limits{MaxConcurrentCallbacks: limit},
			})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			var elapsed time.Duration
			b.ResetTimer()
			for range b.N {
				started := time.Now()
				output, runErr := value.Run(context.Background(), RunRequest{Input: "fixed-input"})
				elapsed += time.Since(started)
				if runErr != nil {
					b.Fatal(runErr)
				}
				if got := len(output.Workflow.RootResult.Children); got != 256 {
					b.Fatalf("parallel results = %d, want 256", got)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(b.N*256)/elapsed.Seconds(), "callbacks/s")
		})
	}
}
