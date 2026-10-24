package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/internal/metrictest"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

func TestAgentMetrics(t *testing.T) {
	metrictest.Install()
	v := func(name string, kv ...attribute.KeyValue) float64 { return metrictest.Value(t, name, kv...) }
	model := &usageModel{id: "metric-model", cost: 0.002, scriptedModel: scriptedModel{responses: []*models.ModelResponse{
		{Content: "ok", Usage: &models.MessageMetrics{InputTokens: 100, CachedInputTokens: 80, OutputTokens: 10}},
	}}}
	beforeRuns := v("agent.runs", attribute.String("status", "completed"), attribute.String("agent_id", "metrics"))
	beforeCached := v("agent.model.tokens", attribute.String("model_id", "metric-model"), attribute.String("kind", "cached"))
	beforeCost := v("agent.model.cost", attribute.String("model_id", "metric-model"))
	beforeLock := v("agent.session_lock.wait", attribute.String("outcome", "acquired"))

	ag, err := New(AgentConfig{ID: "metrics", Model: model, DB: in_memory.New(), UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ag.Run(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: "q2"}, SessionID: first.SessionID}); err != nil {
		t.Fatal(err)
	}
	if got := v("agent.runs", attribute.String("status", "completed"), attribute.String("agent_id", "metrics")) - beforeRuns; got != 2 {
		t.Fatalf("agent.runs completed delta = %v", got)
	}
	if got := v("agent.model.tokens", attribute.String("model_id", "metric-model"), attribute.String("kind", "cached")) - beforeCached; got != 80 {
		t.Fatalf("cached tokens delta = %v", got)
	}
	// Only the first call reports usage.
	if got := v("agent.model.cost", attribute.String("model_id", "metric-model")) - beforeCost; got < 0.0019 || got > 0.0021 {
		t.Fatalf("cost delta = %v", got)
	}
	if got := v("agent.session_lock.wait", attribute.String("outcome", "acquired")) - beforeLock; got != 1 {
		t.Fatalf("session lock acquisitions delta = %v (only the run on an existing session locks)", got)
	}
}

func TestAgentFailureMetrics(t *testing.T) {
	metrictest.Install()
	v := func(name string, kv ...attribute.KeyValue) float64 { return metrictest.Value(t, name, kv...) }

	beforeQuota := v("agent.quota.rejected", attribute.String("reason", "run_tokens"))
	call := &models.ModelResponse{ToolCalls: []models.ToolCall{newToolCall("c", "mock_tool", `{}`)}, Usage: &models.MessageMetrics{InputTokens: 200}}
	var calls atomic.Int32
	ag, _ := New(AgentConfig{Model: &scriptedModel{responses: []*models.ModelResponse{call, call}}, MaxRunTokens: 100,
		Tools: []tools.Tool{&counterTool{name: "mock_tool", calls: &calls}}})
	if _, err := ag.Run(context.Background(), "q"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v", err)
	}
	if got := v("agent.quota.rejected", attribute.String("reason", "run_tokens")) - beforeQuota; got != 1 {
		t.Fatalf("quota rejections delta = %v", got)
	}

	beforeCircuit := v("agent.model.circuit_opened", attribute.String("model_id", "failing"))
	beforeRetries := v("agent.model.retries")
	cb, _ := New(AgentConfig{Model: &failingModel{}, FallbackModels: []models.Model{&scriptedModel{}},
		RetryConfig:    RetryConfig{MaxRetries: 1, RetryDelay: time.Millisecond},
		CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: 2, Cooldown: time.Minute}})
	for i := 0; i < 3; i++ {
		if _, err := cb.Run(context.Background(), "q"); err != nil {
			t.Fatal(err)
		}
	}
	if got := v("agent.model.circuit_opened", attribute.String("model_id", "failing")) - beforeCircuit; got != 1 {
		t.Fatalf("circuit opened delta = %v", got)
	}
	if got := v("agent.model.retries") - beforeRetries; got != 2 {
		t.Fatalf("retries delta = %v", got)
	}

	beforeExternal := v("agent.run.cancelled_externally", attribute.String("detection", "checkpoint"))
	store := in_memory.New()
	tool := &cancelElsewhereTool{store: store, runID: "metric-ext"}
	ext, _ := New(AgentConfig{Model: &scriptedModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{newToolCall("c1", "cancel_elsewhere", `{}`)}},
	}}, DB: store, UserID: "u", Tools: []tools.Tool{tool}, RunLeaseTimeout: -1})
	if _, err := ext.RunWith(context.Background(), session.RunRequest{RunID: "metric-ext", Input: session.RunInput{Message: "q"}}); !errors.Is(err, ErrRunCancelledExternally) {
		t.Fatalf("err = %v", err)
	}
	if got := v("agent.run.cancelled_externally", attribute.String("detection", "checkpoint")) - beforeExternal; got != 1 {
		t.Fatalf("external cancels delta = %v", got)
	}
}
