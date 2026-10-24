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

func metricTool(query string) string { return query }

func TestAgentAccumulatesUsageAndToolExecution(t *testing.T) {
	model := mock.New()
	model.ToolCallRounds = 1
	model.ResponseData = &models.ModelResponse{Content: "done", Usage: &models.MessageMetrics{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}}
	tool, err := tools.NewFunction("mock_tool", "test tool", metricTool)
	if err != nil {
		t.Fatalf("NewFunction: %v", err)
	}
	value, err := agent.New(agent.AgentConfig{Model: model, Tools: []tools.Tool{tool}, MaxToolCalls: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, runErr := value.Run(context.Background(), "use tool")
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if out.Metrics.ModelCalls != 2 || out.Metrics.ToolCalls != 1 || out.Metrics.TotalTokens != 10 || out.Metrics.DurationMS < 0 {
		t.Fatalf("metrics = %+v", out.Metrics)
	}
	if len(out.ToolExecutions) != 1 || out.ToolExecutions[0].Status != "success" || out.ToolExecutions[0].Name != "mock_tool" {
		t.Fatalf("tool executions = %+v", out.ToolExecutions)
	}
}

func TestAgentFailsWhenToolCallRoundLimitIsExhausted(t *testing.T) {
	model := mock.New()
	model.ToolCallRounds = 2
	value, err := agent.New(agent.AgentConfig{Model: model, MaxToolCalls: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, runErr := value.Run(context.Background(), "loop")
	if runErr == nil || out.Status != session.RunStatusError || out.Metrics.ModelCalls != 2 || out.Metrics.ToolCalls != 1 {
		t.Fatalf("Run = %+v, %v", out, runErr)
	}
}

func TestAgentAllowsFinalResponseAfterLastToolCallRound(t *testing.T) {
	model := mock.New()
	model.ToolCallRounds = 1
	model.ResponseData = &models.ModelResponse{Content: "done"}
	value, err := agent.New(agent.AgentConfig{Model: model, MaxToolCalls: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, runErr := value.Run(context.Background(), "loop once")
	if runErr != nil || out.Status != session.RunStatusCompleted || out.Content != "done" || out.Metrics.ModelCalls != 2 {
		t.Fatalf("Run = %+v, %v", out, runErr)
	}
}
