package workflow

import (
	"context"
	"encoding/json"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
)

// AgentExecutor adapts an Agent to a workflow executor and persists the
// parent-run relationship with the child run's first durable write.
func AgentExecutor(value *agent.Agent) Executor {
	return ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		out, err := value.RunWith(ctx, session.RunRequest{Input: session.RunInput{Message: valueString(input.Value)}, UserID: input.UserID, ParentRunID: input.RunID, NodeID: input.NodeID, Metadata: input.Metadata})
		if out == nil {
			return NodeOutput{}, err
		}
		return NodeOutput{Value: out.Content, Metadata: map[string]any{"parsed": out.Parsed}, ChildRunIDs: []string{out.RunID}}, err
	})
}

// ParsedAgentExecutor adapts an Agent to a workflow executor like
// AgentExecutor, but when the agent produces structured output it passes the
// parsed value (after JSON normalization) downstream instead of the raw JSON
// string. This lets condition predicates and custom executors work with
// native maps/slices instead of having to re-parse a JSON string.
//
// If the run has no Parsed value, the executor falls back to Content, so
// behavior is identical to AgentExecutor for plain-text agents.
func ParsedAgentExecutor(value *agent.Agent) Executor {
	return ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		out, err := value.RunWith(ctx, session.RunRequest{Input: session.RunInput{Message: valueString(input.Value)}, UserID: input.UserID, ParentRunID: input.RunID, NodeID: input.NodeID, Metadata: input.Metadata})
		if out == nil {
			return NodeOutput{}, err
		}

		parsed := false
		var nextValue any = out.Content
		if out.Parsed != nil {
			normalized, normalizeErr := normalizeParsed(out.Parsed)
			if normalizeErr == nil {
				nextValue = normalized
				parsed = true
			}
		}
		return NodeOutput{Value: nextValue, Metadata: map[string]any{"parsed": parsed}, ChildRunIDs: []string{out.RunID}}, err
	})
}

// normalizeParsed round-trips a parsed value through JSON so in-memory types
// (struct, map, slice) become the same JSON-native types that would appear
// after loading a persisted run from the database. This makes downstream code
// resilient to whether the value was produced fresh or restored from storage.
func normalizeParsed(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

// TeamExecutor adapts a Team to a workflow executor.
func TeamExecutor(value *team.Team) Executor {
	return ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		out, err := value.RunWith(ctx, session.RunRequest{Input: session.RunInput{Message: valueString(input.Value)}, UserID: input.UserID, ParentRunID: input.RunID, NodeID: input.NodeID, Metadata: input.Metadata})
		if out == nil {
			return NodeOutput{}, err
		}
		return NodeOutput{Value: out.Content, ChildRunIDs: []string{out.RunID}}, err
	})
}
