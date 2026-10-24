package agent_test

import (
	"context"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

type metadataTool struct{ seen chan map[string]string }

type metadataPreRunHook struct{ seen chan map[string]string }

func (h metadataPreRunHook) PreRun(ctx context.Context, _ *session.RunInput) error {
	metadata, _ := session.RunMetadataFromContext(ctx)
	h.seen <- metadata
	return nil
}

func (t *metadataTool) Name() string        { return "mock_tool" }
func (t *metadataTool) Description() string { return "read trusted metadata" }
func (t *metadataTool) Schema() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": t.Name(), "parameters": map[string]any{"type": "object"}}}
}
func (t *metadataTool) Call(ctx context.Context, _ map[string]any) (any, error) {
	metadata, _ := session.RunMetadataFromContext(ctx)
	t.seen <- metadata
	return "ok", nil
}

func TestAgentPropagatesTrustedMetadataToOutputHookAndTool(t *testing.T) {
	model := mock.New()
	model.ToolCallRounds = 1
	model.ResponseData = &models.ModelResponse{Content: "done"}
	tool := &metadataTool{seen: make(chan map[string]string, 1)}
	hook := metadataPreRunHook{seen: make(chan map[string]string, 1)}
	value, err := agent.New(agent.AgentConfig{Model: model, Tools: []tools.Tool{tool}, Hooks: &agent.Hooks{PreRun: hook}, MaxToolCalls: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	metadata := map[string]string{"project_id": "project-a", "actor_id": "actor-a"}
	out, runErr := value.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: "read"}, Metadata: metadata})
	if runErr != nil {
		t.Fatalf("RunWith: %v", runErr)
	}
	metadata["project_id"] = "mutated"
	seen := <-tool.seen
	hookSeen := <-hook.seen
	if out.Metadata["project_id"] != "project-a" || seen["actor_id"] != "actor-a" || hookSeen["project_id"] != "project-a" {
		t.Fatalf("metadata output=%+v tool=%+v hook=%+v", out.Metadata, seen, hookSeen)
	}
}
