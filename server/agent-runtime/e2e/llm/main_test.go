//go:build llm

// Package llm contains end-to-end tests that call a real LLM provider.
// Run with: go test -tags=llm ./e2e/llm/...
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
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/openai"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/builtin"
)

func loadEnv(t *testing.T) (apiKey, baseURL, modelID string) {
	t.Helper()
	apiKey = os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY not set; skipping LLM e2e test")
	}
	baseURL = os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	modelID = os.Getenv("LLM_MODEL")
	if modelID == "" {
		modelID = "deepseek-chat"
	}
	return
}

func newLLMModel(t *testing.T) models.Model {
	t.Helper()
	apiKey, baseURL, modelID := loadEnv(t)
	chat := openai.NewChat(modelID, apiKey).SetBaseURL(baseURL)
	return chat
}

func TestDeepSeek_BasicRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-basic",
		Instructions: "You are a helpful assistant. Answer concisely.",
		Model:        newLLMModel(t),
		DB:           in_memory.New(),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(ctx, "Say hello in one sentence.")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("expected status %q, got %q", session.RunStatusCompleted, out.Status)
	}
	if out.SessionID == "" {
		t.Fatal("expected SessionID to be set")
	}
	if !strings.Contains(strings.ToLower(out.Content), "hello") {
		t.Fatalf("expected response to contain 'hello', got %q", out.Content)
	}
}

func TestDeepSeek_StreamRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-stream",
		Instructions: "You are a helpful assistant. Answer concisely.",
		Model:        newLLMModel(t),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	ch, err := ag.RunStream(ctx, "Count from 1 to 3 separated by commas.")
	if err != nil {
		t.Fatalf("run stream: %v", err)
	}

	var sawStarted, sawMessage, sawCompleted bool
	var content string
	for ev := range ch {
		switch ev.Type {
		case agent.EventTypeRunStarted:
			sawStarted = true
		case agent.EventTypeMessage:
			sawMessage = true
			content += ev.Content
		case agent.EventTypeRunCompleted:
			sawCompleted = true
		case agent.EventTypeRunError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}

	if !sawStarted {
		t.Fatal("expected run_started event")
	}
	if !sawMessage {
		t.Fatal("expected message event")
	}
	if !sawCompleted {
		t.Fatal("expected run_completed event")
	}
	if content == "" {
		t.Fatal("expected non-empty streamed content")
	}
}

func TestDeepSeek_ToolCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	calc, err := builtin.NewCalculatorTool()
	if err != nil {
		t.Fatalf("create calculator tool: %v", err)
	}

	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-tools",
		Instructions: "You have a calculator tool. Use it for arithmetic.",
		Model:        newLLMModel(t),
		Tools:        []tools.Tool{calc},
		ToolChoice:   &models.ToolChoice{Mode: models.ToolChoiceRequired},
		DB:           in_memory.New(),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(ctx, "Use the calculator to compute 12345 + 67890.")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("expected status %q, got %q", session.RunStatusCompleted, out.Status)
	}
	if len(out.ToolCalls) == 0 {
		t.Fatalf("expected at least one tool call, got none")
	}

	foundCalc := false
	for _, tc := range out.ToolCalls {
		if tc.Function.Name == "calculator" {
			foundCalc = true
			break
		}
	}
	if !foundCalc {
		t.Fatalf("expected calculator tool call, got %+v", out.ToolCalls)
	}

	// The result 80235 should appear either in the final content or in one of
	// the tool/assistant messages.
	foundResult := strings.Contains(out.Content, "80235")
	if !foundResult {
		for _, m := range out.Messages {
			if strings.Contains(m.Content, "80235") {
				foundResult = true
				break
			}
		}
	}
	if !foundResult {
		t.Fatalf("expected result 80235 in content or messages, content=%q", out.Content)
	}
}

type weatherResponse struct {
	City        string  `json:"city"`
	Temperature float64 `json:"temperature"`
	Unit        string  `json:"unit"`
}

func TestDeepSeek_StructuredOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ag, err := agent.New(agent.AgentConfig{
		Name:         "e2e-structured",
		Instructions: "You are a helpful assistant. Always respond with valid JSON matching the requested schema.",
		Model:        newLLMModel(t),
		OutputSchema: weatherResponse{},
		DB:           in_memory.New(),
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	out, err := ag.Run(ctx, "What is the weather like in Tokyo? Respond with JSON containing city, temperature, and unit.")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("expected status %q, got %q", session.RunStatusCompleted, out.Status)
	}
	if out.Parsed == nil {
		t.Fatal("expected Parsed structured output")
	}

	w, ok := out.Parsed.(weatherResponse)
	if !ok {
		t.Fatalf("expected weatherResponse, got %T", out.Parsed)
	}
	if w.City == "" {
		t.Fatal("expected city in structured output")
	}
}
