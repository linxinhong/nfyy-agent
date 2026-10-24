package workflow

import (
	"context"
	"fmt"
	"testing"
)

func benchmarkLimitedSchedulerWorkflow(b *testing.B, limits *Limits) *Workflow {
	b.Helper()
	registry := NewRegistry()
	children := make([]NodeSpec, 4)
	for index := range children {
		ref := fmt.Sprintf("scheduler-%d", index)
		if err := registry.RegisterExecutor(ref, ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
			return NodeOutput{Value: input.NodeID}, nil
		})); err != nil {
			b.Fatal(err)
		}
		children[index] = ExecutorNode(fmt.Sprintf("node-%d", index), ref)
	}
	value, err := New(Config{
		Definition: Definition{ID: "scheduler-benchmark", Version: "v1", Root: Parallel("root", children...)},
		Registry:   registry,
		Limits:     limits,
	})
	if err != nil {
		b.Fatal(err)
	}
	return value
}

// BenchmarkWorkflowLimitedScheduler compares equal, unsaturated four-leaf
// parallel work. Saturated queue behavior is intentionally measured by the
// concurrency tests rather than presented as an equal-throughput comparison.
func BenchmarkWorkflowLimitedScheduler(b *testing.B) {
	for _, mode := range []struct {
		name   string
		limits *Limits
	}{
		{name: "legacy", limits: nil},
		{name: "limited_K4", limits: &Limits{MaxConcurrentCallbacks: 4}},
	} {
		b.Run(mode.name, func(b *testing.B) {
			workflow := benchmarkLimitedSchedulerWorkflow(b, mode.limits)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				output, err := workflow.Run(context.Background(), RunRequest{Input: "fixed-input"})
				if err != nil {
					b.Fatal(err)
				}
				if got := len(output.Workflow.RootResult.Children); got != 4 {
					b.Fatalf("parallel results = %d, want 4", got)
				}
			}
		})
	}
}
