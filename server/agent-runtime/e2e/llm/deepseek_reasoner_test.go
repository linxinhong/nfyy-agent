//go:build llm

package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/providers"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/builtin"
)

// newDeepSeekModel resolves a DeepSeek model through the provider registry,
// e.g. "deepseek:deepseek-reasoner" (R1 / V3 thinking) or
// "deepseek:deepseek-chat" (V3). Override with LLM_REASONER_REF.
func newDeepSeekModel(t *testing.T, defaultRef string) models.Model {
	t.Helper()
	if os.Getenv("DEEPSEEK_API_KEY") == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping LLM e2e test")
	}
	ref := defaultRef
	if override := os.Getenv("LLM_REASONER_REF"); override != "" && defaultRef == "deepseek:deepseek-reasoner" {
		ref = override
	}
	m, err := providers.NewFromEnv().Resolve(ref)
	if err != nil {
		t.Fatalf("resolve %s: %v", ref, err)
	}
	return m
}

func TestDeepSeekReasoner_RunKeepsReasoningSeparate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ag, err := agent.New(agent.AgentConfig{
		Name:  "e2e-reasoner",
		Model: newDeepSeekModel(t, "deepseek:deepseek-reasoner"),
		DB:    in_memory.New(),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	out, err := ag.Run(ctx, "What is 17 * 23? Reply with just the number.")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != session.RunStatusCompleted || !strings.Contains(out.Content, "391") {
		t.Fatalf("unexpected output: status=%s content=%q", out.Status, out.Content)
	}
	if strings.Contains(out.Content, "<think>") {
		t.Fatalf("reasoning leaked into content: %q", out.Content)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.ReasoningContent == "" {
		t.Fatal("expected reasoning_content on the final assistant message")
	}
}

func TestDeepSeekReasoner_StreamEmitsReasoningThenAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ag, err := agent.New(agent.AgentConfig{
		Name:  "e2e-reasoner-stream",
		Model: newDeepSeekModel(t, "deepseek:deepseek-reasoner"),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	events, err := ag.RunStream(ctx, "Is 97 a prime number? Answer yes or no.")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var reasoning, content strings.Builder
	var terminal agent.Event
	for ev := range events {
		switch ev.Type {
		case agent.EventTypeReasoning:
			reasoning.WriteString(ev.Content)
		case agent.EventTypeMessage:
			content.WriteString(ev.Content)
		}
		terminal = ev
	}
	if terminal.Type != agent.EventTypeRunCompleted {
		t.Fatalf("terminal event = %s err=%v", terminal.Type, terminal.Err)
	}
	if reasoning.Len() == 0 || !strings.Contains(strings.ToLower(content.String()), "yes") {
		t.Fatalf("reasoning=%d bytes content=%q", reasoning.Len(), content.String())
	}
}

// Thinking-mode tool loops require reasoning_content of the assistant
// tool-call turn to be sent back on the next request.
func TestDeepSeekReasoner_ToolLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	calc, err := builtin.NewCalculatorTool()
	if err != nil {
		t.Fatalf("create calculator tool: %v", err)
	}
	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-reasoner-tools",
		Instructions: "You have a calculator tool. Always use it for arithmetic.",
		Model:        newDeepSeekModel(t, "deepseek:deepseek-reasoner"),
		Tools:        []tools.Tool{calc},
		DB:           in_memory.New(),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	out, err := ag.Run(ctx, "Use the calculator to compute 12345 + 67890.")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != session.RunStatusCompleted || !strings.Contains(out.Content, "80235") {
		t.Fatalf("unexpected output: status=%s content=%q", out.Status, out.Content)
	}
}

func TestDeepSeekChat_CacheUsageIsReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	m := newDeepSeekModel(t, "deepseek:deepseek-chat")
	// A long shared prefix makes the second call eligible for context caching.
	prefix := strings.Repeat("This is shared context for cache testing. ", 200)
	messages := []models.Message{{Role: models.RoleSystem, Content: prefix}, {Role: models.RoleUser, Content: "Say ok."}}
	if _, err := m.Response(ctx, messages, models.ModelOptions{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	resp, err := m.Response(ctx, messages, models.ModelOptions{})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if resp.FinishReason != models.FinishReasonStop {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
	t.Logf("usage: %+v", resp.Usage)
}
