package workflow

import (
	"context"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestParsedAgentExecutorPassesNormalizedValue(t *testing.T) {
	model := mock.New()
	model.ResponseData = &models.ModelResponse{
		Content: `{"score":42}`,
		Parsed:  map[string]any{"score": float64(42)},
	}
	agentValue, err := agent.New(agent.AgentConfig{
		ID:           "parsed-agent",
		Model:        model,
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"score": map[string]any{"type": "integer"}}},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	exec := ParsedAgentExecutor(agentValue)
	out, err := exec.Execute(context.Background(), NodeInput{Value: "go"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	m, ok := out.Value.(map[string]any)
	if !ok {
		t.Fatalf("value type = %T, want map[string]any", out.Value)
	}
	if m["score"] != float64(42) {
		t.Errorf("score = %v, want 42", m["score"])
	}
	if out.Metadata["parsed"] != true {
		t.Errorf("metadata parsed = %v, want true", out.Metadata["parsed"])
	}
}

func TestParsedAgentExecutorFallsBackToContent(t *testing.T) {
	model := mock.New()
	model.ResponseData = &models.ModelResponse{Content: "plain text"}
	agentValue, err := agent.New(agent.AgentConfig{ID: "text-agent", Model: model})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	exec := ParsedAgentExecutor(agentValue)
	out, err := exec.Execute(context.Background(), NodeInput{Value: "go"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Value != "plain text" {
		t.Errorf("value = %v, want plain text", out.Value)
	}
	if out.Metadata["parsed"] != false {
		t.Errorf("metadata parsed = %v, want false", out.Metadata["parsed"])
	}
}

func TestParsedAgentExecutorFeedsConditionPredicate(t *testing.T) {
	store := in_memory.New()
	model := mock.New()
	model.ResponseData = &models.ModelResponse{
		Content: `{"approved":true}`,
		Parsed:  map[string]any{"approved": true},
	}
	decider, err := agent.New(agent.AgentConfig{
		ID:           "decider",
		Model:        model,
		DB:           store,
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"approved": map[string]any{"type": "boolean"}}},
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	registry := NewRegistry()
	registry.RegisterExecutor("decider", ParsedAgentExecutor(decider))
	registry.RegisterExecutor("yes", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "yes-branch"}, nil
	}))
	registry.RegisterExecutor("no", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "no-branch"}, nil
	}))
	registry.RegisterPredicate("approved", PredicateFunc(func(_ context.Context, input NodeInput) (bool, error) {
		m, ok := input.Value.(map[string]any)
		return ok && m["approved"] == true, nil
	}))

	wf, err := New(Config{
		Definition: Definition{
			ID:      "routing",
			Version: "v1",
			Root: Sequence("root",
				ExecutorNode("decide", "decider"),
				Condition("route", "approved",
					ExecutorNode("then", "yes"),
					ExecutorNode("else", "no"),
				),
			),
		},
		Registry: registry,
		DB:       store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := wf.Run(context.Background(), RunRequest{Input: "go", UserID: "alice"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("status = %v, want completed", out.Status)
	}
	if out.Content != "yes-branch" {
		t.Errorf("output = %q, want yes-branch", out.Content)
	}
}
