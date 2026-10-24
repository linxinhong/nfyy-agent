package deepseekmock_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/e2e/deepseekmock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/providers"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/builtin"
)

// official resolves a model on the official DeepSeek provider, pointed at
// the emulator.
func official(t *testing.T, srv *deepseekmock.Server, model string) models.Model {
	t.Helper()
	p := providers.DeepSeek()
	p.BaseURL = srv.BaseURL()
	reg := providers.NewRegistry(p)
	reg.SetAPIKey("deepseek", "sk-mock")
	m, err := reg.Resolve("deepseek:" + model)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// hosted resolves a DeepSeek model on a third-party host (SiliconFlow).
func hosted(t *testing.T, srv *deepseekmock.Server, model string) models.Model {
	t.Helper()
	p := providers.SiliconFlow()
	p.BaseURL = srv.BaseURL()
	reg := providers.NewRegistry(p)
	reg.SetAPIKey("siliconflow", "sk-mock")
	m, err := reg.Resolve("siliconflow:" + model)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// calculatorBrain asks for the calculator once per user turn, then answers
// with the tool's result, reasoning before each step.
func calculatorBrain(expression string) func(*deepseekmock.Request) deepseekmock.Reply {
	return func(req *deepseekmock.Request) deepseekmock.Reply {
		results := req.ToolResults()
		if len(req.Tools) == 0 || len(results) > 0 {
			answer := "no tools"
			if len(results) > 0 {
				answer = "The answer is " + deepseekmock.Arg(results[len(results)-1].Text(), "result") + "."
			}
			return deepseekmock.Reply{Reasoning: "The calculator returned the value; report it.", Content: answer}
		}
		param := deepseekmock.FirstParam(req.Tools[0])
		args, _ := json.Marshal(map[string]string{param: expression})
		return deepseekmock.Reply{
			Reasoning: "I must not do arithmetic myself; call the calculator.",
			ToolCalls: []deepseekmock.Call{{Name: req.Tools[0].Function.Name, Arguments: string(args)}},
		}
	}
}

func calculator(t *testing.T) tools.Tool {
	t.Helper()
	calc, err := builtin.NewCalculatorTool()
	if err != nil {
		t.Fatal(err)
	}
	return calc
}

func requireNoRejections(t *testing.T, srv *deepseekmock.Server) {
	t.Helper()
	for i, e := range srv.Exchanges() {
		if e.Status == 400 || e.Status == 422 || e.Status == 401 {
			t.Fatalf("request %d rejected by the API (%d): %s", i, e.Status, e.Error)
		}
	}
}

func drain(t *testing.T, events <-chan agent.Event) (content, reasoning string, terminal agent.Event) {
	t.Helper()
	var c, r strings.Builder
	for ev := range events {
		switch ev.Type {
		case agent.EventTypeMessage:
			c.WriteString(ev.Content)
		case agent.EventTypeReasoning:
			r.WriteString(ev.Content)
		case agent.EventTypeRunCompleted, agent.EventTypeRunError, agent.EventTypeRunPaused:
			terminal = ev
		}
	}
	return c.String(), r.String(), terminal
}

// --- V3.2 thinking mode ---------------------------------------------------

func TestV32ReasonerToolLoopReplaysReasoning(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32, KeepAlives: 2, Brain: calculatorBrain("12345 + 67890")})
			defer srv.Close()
			ag, err := agent.New(agent.AgentConfig{Model: official(t, srv, "deepseek-reasoner"), Tools: []tools.Tool{calculator(t)}})
			if err != nil {
				t.Fatal(err)
			}
			var content, reasoning string
			if stream {
				events, err := ag.RunStream(context.Background(), "What is 12345 + 67890?")
				if err != nil {
					t.Fatal(err)
				}
				var terminal agent.Event
				content, reasoning, terminal = drain(t, events)
				if terminal.Type != agent.EventTypeRunCompleted {
					t.Fatalf("terminal = %s: %v", terminal.Type, terminal.Err)
				}
			} else {
				out, err := ag.Run(context.Background(), "What is 12345 + 67890?")
				if err != nil {
					t.Fatal(err)
				}
				content = out.Content
				reasoning = out.Messages[len(out.Messages)-1].ReasoningContent
			}
			requireNoRejections(t, srv)
			if !strings.Contains(content, "80235") || reasoning == "" {
				t.Fatalf("content = %q reasoning = %q", content, reasoning)
			}
			reqs := srv.Accepted()
			if len(reqs) != 2 {
				t.Fatalf("requests = %d", len(reqs))
			}
			second := reqs[1].Messages
			var replayed bool
			for _, m := range second {
				replayed = replayed || (len(m.ToolCalls) > 0 && m.ReasoningContent != nil && strings.Contains(*m.ReasoningContent, "call the calculator"))
			}
			if !replayed {
				t.Fatalf("tool-call reasoning not replayed: %s", reqs[1].Raw)
			}
		})
	}
}

// The emulator rejects a tool loop that drops reasoning; this guards the
// emulator itself so the test above cannot pass vacuously.
func TestEmulatorRejectsMissingReasoningInThinkingToolLoop(t *testing.T) {
	srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32, Brain: calculatorBrain("1+1")})
	defer srv.Close()
	m := official(t, srv, "deepseek-reasoner")
	messages := []models.Message{
		{Role: models.RoleUser, Content: "1+1?"},
		{Role: models.RoleAssistant, ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "calculator", Arguments: `{"expression":"1+1"}`}}}},
		{Role: models.RoleTool, ToolCallID: "c1", Content: `{"result":2}`},
	}
	_, err := m.Response(context.Background(), messages, models.ModelOptions{})
	var perr *models.ProviderError
	if !errors.As(err, &perr) || perr.StatusCode != 400 || !strings.Contains(perr.Message, "reasoning_content") || models.IsRetryable(err) {
		t.Fatalf("err = %v, want a non-retryable 400 about reasoning_content", err)
	}
}

func TestV32ChatThinkingToggleRequiresReplayToo(t *testing.T) {
	srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32, Brain: calculatorBrain("6 * 7")})
	defer srv.Close()
	ag, err := agent.New(agent.AgentConfig{Model: official(t, srv, "deepseek-chat"), Tools: []tools.Tool{calculator(t)}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.Run(models.WithReasoningEffort(context.Background(), "high"), "6 * 7?")
	if err != nil {
		t.Fatal(err)
	}
	requireNoRejections(t, srv)
	if !strings.Contains(out.Content, "42") || srv.Accepted()[0].Thinking == nil {
		t.Fatalf("content = %q, thinking switch sent = %v", out.Content, srv.Accepted()[0].Thinking != nil)
	}
}

func TestV32ChatMaxTokensIsClampedToModelLimit(t *testing.T) {
	srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32})
	defer srv.Close()
	maxTokens := 20000
	ag, err := agent.New(agent.AgentConfig{Model: official(t, srv, "deepseek-chat"), MaxTokens: &maxTokens})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := *srv.Accepted()[0].MaxTokens; got != 8192 {
		t.Fatalf("max_tokens = %d, want 8192", got)
	}
}

// usageLog records UsageGuard records, which carry cache hits and cost.
type usageLog struct {
	mu      sync.Mutex
	records []agent.UsageRecord
}

func (u *usageLog) Admit(context.Context, agent.UsageScope) error { return nil }
func (u *usageLog) Record(_ context.Context, _ agent.UsageScope, r agent.UsageRecord) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.records = append(u.records, r)
}

// changingMemory returns a different memory every call, like a real store
// that learns between turns.
type changingMemory struct {
	mu sync.Mutex
	n  int
}

func (m *changingMemory) CreateMemory(context.Context, string, string, string) (*memory.UserMemory, error) {
	return nil, nil
}
func (m *changingMemory) GetMemories(context.Context, string, string, int) ([]*memory.UserMemory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return []*memory.UserMemory{{Memory: fmt.Sprintf("fact #%d about the user", m.n)}}, nil
}

// Multi-turn sessions: earlier reasoning is not resent, and the stable
// prefix layout turns into prompt-cache hits that are billed cheaper.
func TestV32MultiTurnPrefixCacheHitsAndCost(t *testing.T) {
	instructions := strings.Repeat("You are a meticulous support agent for an accounting product. ", 40)
	run := func(legacy bool) []agent.UsageRecord {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32, Brain: func(*deepseekmock.Request) deepseekmock.Reply {
			return deepseekmock.Reply{Reasoning: "think about it", Content: strings.Repeat("A detailed answer. ", 20)}
		}})
		defer srv.Close()
		log := &usageLog{}
		ag, err := agent.New(agent.AgentConfig{
			Model: official(t, srv, "deepseek-reasoner"), Instructions: instructions, DB: in_memory.New(), UserID: "u",
			Memory: &changingMemory{}, UsageGuard: log, LegacySystemContext: legacy,
		})
		if err != nil {
			t.Fatal(err)
		}
		first, err := ag.Run(context.Background(), "turn 1")
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"turn 2", "turn 3"} {
			if _, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: q}, SessionID: first.SessionID}); err != nil {
				t.Fatal(err)
			}
		}
		requireNoRejections(t, srv)
		for _, req := range srv.Accepted()[1:] {
			for i, m := range req.Messages[:req.LastUser()] {
				if m.ReasoningContent != nil {
					t.Fatalf("earlier turn reasoning resent at messages[%d]", i)
				}
			}
		}
		return log.records
	}

	stable := run(false)
	legacy := run(true)
	if len(stable) != 3 || len(legacy) != 3 {
		t.Fatalf("records = %d / %d", len(stable), len(legacy))
	}
	if legacy[2].CachedInputTokens != 0 {
		t.Fatalf("legacy layout unexpectedly hit the cache: %+v", legacy[2])
	}
	if stable[2].CachedInputTokens < stable[2].InputTokens/2 {
		t.Fatalf("stable layout cache hits = %d of %d prompt tokens", stable[2].CachedInputTokens, stable[2].InputTokens)
	}
	if stable[2].CostUSD >= legacy[2].CostUSD {
		t.Fatalf("cache hits must be billed cheaper: stable $%.8f vs legacy $%.8f", stable[2].CostUSD, legacy[2].CostUSD)
	}
	t.Logf("turn 3: stable %d/%d cached ($%.6f) vs legacy %d/%d ($%.6f)",
		stable[2].CachedInputTokens, stable[2].InputTokens, stable[2].CostUSD,
		legacy[2].CachedInputTokens, legacy[2].InputTokens, legacy[2].CostUSD)
}

// --- R1 deployments ---------------------------------------------------------

func TestR1HostedThinkTagsAreSplitAndNeverReplayed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prefilled bool
		stream    bool
	}{{"tags", false, false}, {"tags-stream", false, true}, {"prefilled", true, false}, {"prefilled-stream", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeR1, ThinkTags: true, PrefilledThink: tc.prefilled, R1Tools: true,
				Brain: calculatorBrain("3 * 14")})
			defer srv.Close()
			ag, err := agent.New(agent.AgentConfig{Model: hosted(t, srv, "deepseek-ai/DeepSeek-R1"), Tools: []tools.Tool{calculator(t)},
				DB: in_memory.New(), UserID: "u"})
			if err != nil {
				t.Fatal(err)
			}
			var content, reasoning, sessionID string
			if tc.stream {
				events, err := ag.RunStream(context.Background(), "3 * 14?")
				if err != nil {
					t.Fatal(err)
				}
				var terminal agent.Event
				content, reasoning, terminal = drain(t, events)
				if terminal.Type != agent.EventTypeRunCompleted {
					t.Fatalf("terminal = %s: %v", terminal.Type, terminal.Err)
				}
				sessionID = terminal.RunOutput.SessionID
			} else {
				out, err := ag.Run(context.Background(), "3 * 14?")
				if err != nil {
					t.Fatal(err)
				}
				content, sessionID = out.Content, out.SessionID
				reasoning = out.Messages[len(out.Messages)-1].ReasoningContent
			}
			if strings.Contains(content, "think>") || !strings.Contains(content, "42") || !strings.Contains(reasoning, "report it") {
				t.Fatalf("content = %q reasoning = %q", content, reasoning)
			}
			// A follow-up turn must not send any reasoning back.
			if _, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: "thanks"}, SessionID: sessionID}); err != nil {
				t.Fatal(err)
			}
			requireNoRejections(t, srv)
		})
	}
}

func TestEmulatorR1RejectsReplayedReasoning(t *testing.T) {
	srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeR1})
	defer srv.Close()
	// Forcing replay on an R1 host reproduces the 400 the default avoids.
	p := providers.SiliconFlow()
	p.BaseURL = srv.BaseURL()
	reg := providers.NewRegistry(p)
	reg.SetAPIKey("siliconflow", "sk-mock")
	model, err := reg.Model("siliconflow", "deepseek-ai/DeepSeek-R1")
	if err != nil {
		t.Fatal(err)
	}
	model.Compat = &providers.OpenAICompletionsCompat{ReasoningReplay: providers.ReasoningReplayAll}
	chat, err := model.Chat(providers.ChatOptions{APIKey: "sk-mock"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = chat.Response(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "hi"},
		{Role: models.RoleAssistant, Content: "hello", ReasoningContent: "greet"},
		{Role: models.RoleUser, Content: "again"},
	}, models.ModelOptions{})
	if err == nil || !strings.Contains(err.Error(), "reasoning_content") {
		t.Fatalf("err = %v, want R1 to reject replayed reasoning", err)
	}
}

func TestOriginalR1WithoutToolSupportFailsClearly(t *testing.T) {
	srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeR1})
	defer srv.Close()
	ag, err := agent.New(agent.AgentConfig{Model: hosted(t, srv, "deepseek-ai/DeepSeek-R1"), Tools: []tools.Tool{calculator(t)}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ag.Run(context.Background(), "1+1?")
	var perr *models.ProviderError
	if !errors.As(err, &perr) || perr.StatusCode != 400 || !strings.Contains(perr.Message, "Function Calling") {
		t.Fatalf("err = %v", err)
	}
	if n := len(srv.Exchanges()); n != 1 {
		t.Fatalf("a 400 must not be retried: %d requests", n)
	}
}

// --- Faults -----------------------------------------------------------------

func retrying(t *testing.T, srv *deepseekmock.Server, model string, extra ...func(*agent.AgentConfig)) *agent.Agent {
	t.Helper()
	cfg := agent.AgentConfig{Model: official(t, srv, model), RetryConfig: agent.RetryConfig{MaxRetries: 3, RetryDelay: time.Millisecond}}
	for _, f := range extra {
		f(&cfg)
	}
	ag, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

func TestFaultsAreRetriedOrSurfacedByType(t *testing.T) {
	t.Run("503 then success", func(t *testing.T) {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32})
		defer srv.Close()
		srv.Fail(deepseekmock.Fault{Status: 503, Message: "Server Overloaded"})
		if _, err := retrying(t, srv, "deepseek-chat").Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("429 honors Retry-After", func(t *testing.T) {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32})
		defer srv.Close()
		srv.Fail(deepseekmock.Fault{Status: 429, Message: "Rate Limit Reached", RetryAfter: "1"})
		started := time.Now()
		if _, err := retrying(t, srv, "deepseek-chat").Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(started); waited < 900*time.Millisecond {
			t.Fatalf("retried after %s, before Retry-After", waited)
		}
	})
	t.Run("insufficient_system_resource is retried", func(t *testing.T) {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32})
		defer srv.Close()
		srv.Fail(deepseekmock.Fault{FinishReason: "insufficient_system_resource"})
		out, err := retrying(t, srv, "deepseek-reasoner").Run(context.Background(), "hi")
		if err != nil || out.Content != "ok" {
			t.Fatalf("out = %+v err = %v", out, err)
		}
	})
	t.Run("402 insufficient balance is not retried", func(t *testing.T) {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32})
		defer srv.Close()
		srv.Fail(deepseekmock.Fault{Status: 402, Message: "Insufficient Balance"})
		_, err := retrying(t, srv, "deepseek-chat").Run(context.Background(), "hi")
		if err == nil || !strings.Contains(err.Error(), "Insufficient Balance") || len(srv.Exchanges()) != 1 {
			t.Fatalf("err = %v, requests = %d", err, len(srv.Exchanges()))
		}
	})
	t.Run("truncated output fails the run", func(t *testing.T) {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32})
		defer srv.Close()
		srv.Fail(deepseekmock.Fault{FinishReason: "length"})
		_, err := retrying(t, srv, "deepseek-chat").Run(context.Background(), "hi")
		if !errors.Is(err, agent.ErrOutputTruncated) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stream dropped before output is retried", func(t *testing.T) {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32, KeepAlives: 1})
		defer srv.Close()
		srv.Fail(deepseekmock.Fault{DropAfter: -1})
		events, err := retrying(t, srv, "deepseek-reasoner").RunStream(context.Background(), "hi")
		if err != nil {
			t.Fatal(err)
		}
		if content, _, terminal := drain(t, events); terminal.Type != agent.EventTypeRunCompleted || content != "ok" {
			t.Fatalf("terminal = %s %v content = %q", terminal.Type, terminal.Err, content)
		}
	})
	t.Run("stream dropped mid-answer is surfaced", func(t *testing.T) {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32, Brain: func(*deepseekmock.Request) deepseekmock.Reply {
			return deepseekmock.Reply{Reasoning: "long thought", Content: "a long answer that gets cut"}
		}})
		defer srv.Close()
		srv.Fail(deepseekmock.Fault{DropAfter: 4})
		events, err := retrying(t, srv, "deepseek-reasoner").RunStream(context.Background(), "hi")
		if err != nil {
			t.Fatal(err)
		}
		_, _, terminal := drain(t, events)
		if terminal.Type != agent.EventTypeRunError || !errors.Is(terminal.Err, models.ErrStreamInterrupted) {
			t.Fatalf("terminal = %s %v", terminal.Type, terminal.Err)
		}
		if n := len(srv.Exchanges()); n != 1 {
			t.Fatalf("a stream that already produced output must not be retried: %d requests", n)
		}
	})
	t.Run("fallback after primary outage", func(t *testing.T) {
		srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32})
		defer srv.Close()
		srv.Fail(deepseekmock.Fault{Status: 503}, deepseekmock.Fault{Status: 503})
		ag := retrying(t, srv, "deepseek-reasoner", func(c *agent.AgentConfig) {
			c.RetryConfig.MaxRetries = 1
			c.FallbackModels = []models.Model{official(t, srv, "deepseek-chat")}
		})
		if _, err := ag.Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
		reqs := srv.Accepted()
		if reqs[len(reqs)-1].Model != "deepseek-chat" {
			t.Fatalf("served by %s, want the fallback", reqs[len(reqs)-1].Model)
		}
	})
}

// --- Team -------------------------------------------------------------------

func TestTeamWithReasonerCoordinatorAndChatMember(t *testing.T) {
	srv := deepseekmock.New(deepseekmock.Config{Mode: deepseekmock.ModeV32, Brain: func(req *deepseekmock.Request) deepseekmock.Reply {
		if req.Model == "deepseek-chat" { // the member
			return deepseekmock.Reply{Content: "Paris is the capital of France."}
		}
		if len(req.ToolResults()) == 0 {
			return deepseekmock.Reply{Reasoning: "The geography expert should answer.", ToolCalls: []deepseekmock.Call{{
				Name: "delegate_to_member", Arguments: `{"member_id":"geo","task":"What is the capital of France?"}`}}}
		}
		return deepseekmock.Reply{Reasoning: "Relay the member's answer.", Content: "Team answer: " + req.ToolResults()[0].Text()}
	}})
	defer srv.Close()
	member, err := agent.New(agent.AgentConfig{ID: "geo", Model: official(t, srv, "deepseek-chat")})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := team.New(team.TeamConfig{Model: official(t, srv, "deepseek-reasoner"), Members: []*agent.Agent{member}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		var content string
		if stream {
			events, err := tm.RunStream(context.Background(), "capital of France?")
			if err != nil {
				t.Fatal(err)
			}
			for ev := range events {
				if ev.Type == agent.EventTypeRunCompleted && ev.RunOutput != nil && ev.RunOutput.TeamID != "" {
					content = ev.RunOutput.Content
				}
				if ev.Type == agent.EventTypeRunError {
					t.Fatalf("stream error: %v", ev.Err)
				}
			}
		} else {
			out, err := tm.Run(context.Background(), "capital of France?")
			if err != nil {
				t.Fatal(err)
			}
			content = out.Content
		}
		if !strings.Contains(content, "Paris") {
			t.Fatalf("stream=%v content = %q", stream, content)
		}
	}
	requireNoRejections(t, srv)
}
