package workflow

import (
	"context"
	"encoding/json"
	"testing"
)

func benchmarkDefinitionSnapshotWorkflow(b *testing.B, limits *Limits) *Workflow {
	b.Helper()
	registry := NewRegistry()
	if err := registry.RegisterExecutor("noop", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: input.Value}, nil
	})); err != nil {
		b.Fatal(err)
	}
	node := ExecutorNode("node", "noop")
	node.Config = json.RawMessage(`{"mode":"fixed"}`)
	value, err := New(Config{
		Definition: Definition{ID: "definition-snapshot", Version: "v1", Root: node},
		Registry:   registry,
		Limits:     limits,
	})
	if err != nil {
		b.Fatal(err)
	}
	return value
}

// BenchmarkWorkflowDefinitionSnapshot compares one unsaturated executor with
// identical work. It intentionally does not exercise parallel scheduling,
// which belongs to WF-002 and may have a finite K.
func BenchmarkWorkflowDefinitionSnapshot(b *testing.B) {
	for _, mode := range []struct {
		name   string
		limits *Limits
	}{
		{name: "legacy", limits: nil},
		{name: "limited", limits: &Limits{}},
	} {
		b.Run(mode.name, func(b *testing.B) {
			value := benchmarkDefinitionSnapshotWorkflow(b, mode.limits)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				output, err := value.Run(context.Background(), RunRequest{Input: "fixed-input"})
				if err != nil {
					b.Fatal(err)
				}
				if output.Content != "fixed-input" {
					b.Fatalf("output = %q", output.Content)
				}
			}
		})
	}
}
