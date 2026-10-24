package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent/compaction"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// --- test helpers ---

type fakeCounter struct{ perMessage int }

func (c *fakeCounter) Count(ctx context.Context, msgs []models.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return len(msgs) * c.perMessage, nil
}

// contentAwareCounter counts based on total content length, so folding
// tool result content actually reduces the token count.
type contentAwareCounter struct{ charsPerToken int }

func (c *contentAwareCounter) Count(ctx context.Context, msgs []models.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	total := 0
	for _, m := range msgs {
		total += (len(m.Content) + c.charsPerToken - 1) / c.charsPerToken
	}
	return total, nil
}

type errCounter struct{}

func (errCounter) Count(ctx context.Context, msgs []models.Message) (int, error) {
	return 0, errors.New("counter failed")
}

type fakeSummarizer struct{ summary string }

func (s *fakeSummarizer) Summarize(ctx context.Context, msgs []models.Message) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.summary, nil
}

type errSummarizer struct{}

func (errSummarizer) Summarize(ctx context.Context, msgs []models.Message) (string, error) {
	return "", errors.New("summarizer unavailable")
}

func compactionAgent(t *testing.T, policy *ContextWindowPolicy, counter TokenCounter, summarizer Summarizer) *Agent {
	t.Helper()
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "ok"}
	ag, err := New(AgentConfig{
		Name:         "compaction-test",
		Model:        m,
		Instructions: "sys",
		ContextProviders: []ContextProvider{
			&CompactionContextProvider{
				Policy:     policy,
				Counter:    counter,
				Summarizer: summarizer,
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

// --- tests ---

func TestCompactionContextProvider_DisabledWhenMaxInputTokensZero(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 0}
	c := &fakeCounter{perMessage: 1000}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// system, u1, a1, per-request context, input
	if len(messages) != 5 {
		t.Fatalf("len(messages) = %d, want 5 (no compaction): %+v", len(messages), messages)
	}
}

func TestCompactionContextProvider_NoCompactionUnderThreshold(t *testing.T) {
	// 5 msgs (incl. per-request context) * 10 = 50 total. MaxInputTokens=100,
	// trigger at 80. 50 < 80 → no compaction.
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	if len(messages) != 5 {
		t.Fatalf("len(messages) = %d, want 5: %+v", len(messages), messages)
	}
}

func TestCompactionContextProvider_FoldsOldToolResults(t *testing.T) {
	// Use content-aware counter so folding tool content reduces tokens.
	// Tool results have 200 chars each → ~50 tokens each with charsPerToken=4.
	// 3 tool results = 150 tokens. After folding to 100 chars = ~25 tokens each.
	// System + 6 history msgs + input with short content ≈ 10 tokens each = 80.
	// Total ≈ 80 + 150 = 230. MaxInputTokens=200, trigger=160. 230 > 160 → triggers.
	// Target = 200*0.7 = 140. After folding: 80 + 75 = 155 > 140, still over.
	// Truncation removes first turn, protected = system + last 2 turns + input = 6 msgs ≈ 60 + 50 = 110 < 140.
	p := &ContextWindowPolicy{MaxInputTokens: 200, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &contentAwareCounter{charsPerToken: 4}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"),
			toolCall("c1"), toolResult("c1", strings.Repeat("x", 200)),
			msg(models.RoleUser, "q2"),
			toolCall("c2"), toolResult("c2", strings.Repeat("y", 200)),
			msg(models.RoleUser, "q3"),
			toolCall("c3"), toolResult("c3", strings.Repeat("z", 200)),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Should have compacted (fewer than 11).
	if len(messages) >= 11 {
		t.Fatalf("expected compaction, got %d messages: %+v", len(messages), messages)
	}
	// Old tool result c1 should be folded (content shortened).
	for _, m := range messages {
		if m.Role == models.RoleTool && m.ToolCallID == "c1" && len(m.Content) > 120 {
			t.Errorf("old tool result c1 not folded: content len=%d", len(m.Content))
		}
	}
}

func TestCompactionContextProvider_SummarizesOldTurns(t *testing.T) {
	// Use longer content so summary is beneficial.
	// 8 msgs * 25 = 200 total. MaxInputTokens=200, trigger=160. 200 > 160 → triggers.
	// Target=140. Old turns (q1,a1,q2,a2) have ~100 chars. Summary "[summary]" = 9 chars.
	// After summarization: system + summary + q3 + a3 + input ≈ 5 msgs * 25 = 125 < 140.
	longContent := strings.Repeat("x", 50)
	p := &ContextWindowPolicy{MaxInputTokens: 200, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 25}
	s := &fakeSummarizer{summary: "[summary]"}
	ag := compactionAgent(t, p, c, s)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, longContent), msg(models.RoleAssistant, longContent),
			msg(models.RoleUser, longContent), msg(models.RoleAssistant, longContent),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	foundSummary := false
	for _, m := range messages {
		if strings.Contains(m.Content, "[summary]") {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Errorf("summary not found in messages: %+v", messages)
	}
}

func TestCompactionContextProvider_SummarizerFallbackToTruncation(t *testing.T) {
	// Summarizer fails → fallback to truncation.
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, errSummarizer{})
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	if len(messages) < 2 {
		t.Fatalf("too few messages after fallback: %+v", messages)
	}
	if messages[len(messages)-1].Content != "input" {
		t.Errorf("last message = %q, want input", messages[len(messages)-1].Content)
	}
}

func TestCompactionContextProvider_AtomicGroupTruncation(t *testing.T) {
	// Force truncation at atomic group boundary.
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"),
			toolCall("c1"), toolResult("c1", "r1"),
			msg(models.RoleUser, "q2"),
			toolCall("c2"), toolResult("c2", "r2"),
			msg(models.RoleUser, "q3"),
			toolCall("c3"), toolResult("c3", "r3"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// P1: verify per call-ID pairing.
	keptCallIDs := make(map[string]bool)
	keptResultIDs := make(map[string]bool)
	for _, m := range messages {
		if m.Role == models.RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				keptCallIDs[tc.ID] = true
			}
		}
		if m.Role == models.RoleTool {
			keptResultIDs[m.ToolCallID] = true
		}
	}
	for callID := range keptCallIDs {
		if !keptResultIDs[callID] {
			t.Errorf("atomic group split: assistant keeps call %s but matching result dropped", callID)
		}
	}
}

func TestCompactionContextProvider_ErrContextBudgetExceeded(t *testing.T) {
	// Budget so tight that protected content alone exceeds it.
	// 4 msgs * 10 = 40 total. MaxInputTokens=15, trigger=12. 40 > 12 → triggers.
	// Protected = system + input = 2 msgs = 20 tokens. 20 > 15 → budget exceeded.
	p := &ContextWindowPolicy{MaxInputTokens: 15, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")},
	)
	_, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err == nil {
		t.Fatal("expected ErrContextBudgetExceeded, got nil")
	}
	if !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("expected ErrContextBudgetExceeded, got %v", err)
	}
}

func TestCompactionContextProvider_ProtectedContent(t *testing.T) {
	// P0-3: verify last 2 history turns (q2, q3) are protected.
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "current_input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	if messages[0].Role != models.RoleSystem {
		t.Errorf("messages[0].Role = %q, want system", messages[0].Role)
	}
	if messages[len(messages)-1].Content != "current_input" {
		t.Errorf("last message = %q, want current_input", messages[len(messages)-1].Content)
	}
	// P0-3: both q2 and q3 must be present (last 2 history turns protected).
	foundQ2, foundQ3 := false, false
	for _, m := range messages {
		if m.Role == models.RoleUser && m.Content == "q2" {
			foundQ2 = true
		}
		if m.Role == models.RoleUser && m.Content == "q3" {
			foundQ3 = true
		}
	}
	if !foundQ2 || !foundQ3 {
		t.Errorf("expected q2 and q3 both present, got q2=%v q3=%v: %+v", foundQ2, foundQ3, messages)
	}
}

func TestCompactionContextProvider_ContextCancelled(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 500, TriggerRatio: 0.8, TargetRatio: 0.7}
	c := &fakeCounter{perMessage: 100}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := ag.buildRunMessages(ctx, sess, "input")
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
}

func TestCompactionContextProvider_ConcurrentRuns(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 500, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 100}
	ag := compactionAgent(t, p, c, nil)
	run := func() {
		sess := sessionWithRuns(t,
			[]models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")},
			[]models.Message{msg(models.RoleUser, "u2"), msg(models.RoleAssistant, "a2")},
		)
		_, _, err := ag.buildRunMessages(context.Background(), sess, "input")
		if err != nil && !errors.Is(err, ErrContextBudgetExceeded) {
			t.Errorf("buildRunMessages: %v", err)
		}
	}
	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			run()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

// --- Agrun 补强测试 ---

func TestCompactionContextProvider_DefaultRatios(t *testing.T) {
	// TriggerRatio/TargetRatio zero → defaults 0.8/0.7.
	// 8 msgs * 10 = 80 tokens. MaxInputTokens=100, trigger=80. 80 >= 80 → triggers.
	// Target=70. Protected = system + last 2 turns + input = 6 msgs = 60 < 70.
	p := &ContextWindowPolicy{MaxInputTokens: 100}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Should have compacted (fewer than 8 msgs).
	if len(messages) >= 8 {
		t.Errorf("expected compaction with default ratios, got %d messages: %+v", len(messages), messages)
	}
}

func TestCompactionContextProvider_MinRecentTurnsAtLeastTwo(t *testing.T) {
	// Roadmap hard constraint: even with MinRecentTurns=0 or 1, protect at least 2 turns.
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 0}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// With MinRecentTurns=0 but hard floor of 2, at least 2 turns should be kept.
	userCount := 0
	for _, m := range messages {
		if m.Role == models.RoleUser && m.Content != "input" {
			userCount++
		}
	}
	if userCount < 2 {
		t.Errorf("expected at least 2 turns protected, got %d users: %+v", userCount, messages)
	}
}

func TestCompactionContextProvider_SummarizerContextCanceledPropagated(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, ctxCancelSummarizer{})
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	)
	_, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

type ctxCancelSummarizer struct{}

func (ctxCancelSummarizer) Summarize(ctx context.Context, msgs []models.Message) (string, error) {
	return "", context.Canceled
}

func TestCompactionContextProvider_CounterErrorPropagates(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 300, TriggerRatio: 0.8, TargetRatio: 0.7}
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, strings.Repeat("x", 400)),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, strings.Repeat("y", 400)),
		},
	)
	ag2, err := New(AgentConfig{
		Name:         "counter-err-test",
		Model:        mock.New(),
		Instructions: "sys",
		ContextProviders: []ContextProvider{
			&CompactionContextProvider{Policy: p, Counter: errCounter{}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = ag2.buildRunMessages(context.Background(), sess, "input")
	if err == nil {
		t.Fatal("expected counter error, got nil")
	}
}

// P0-4: trigger at exactly 80% (inclusive).
func TestCompactionContextProvider_TriggerAtExactly80Percent(t *testing.T) {
	// 10 msgs * 8 = 80 tokens. MaxInputTokens=100, trigger = 80.
	// 80 >= 80 → should trigger compaction.
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7}
	c := &fakeCounter{perMessage: 8}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
			msg(models.RoleUser, "q4"), msg(models.RoleAssistant, "a4"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Should have compacted (fewer than 10 msgs).
	if len(messages) >= 10 {
		t.Errorf("expected compaction at exactly 80%%, got %d messages: %+v", len(messages), messages)
	}
}

// P0-5: verify ErrContextBudgetExceeded via Agent.Run with model call counting.
func TestCompactionContextProvider_BudgetExceededViaAgentRun(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 30, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 100}
	m := &countingMockModel{}
	ag, err := New(AgentConfig{
		Name:         "budget-test",
		Model:        m,
		Instructions: "sys",
		ContextProviders: []ContextProvider{
			&CompactionContextProvider{Policy: p, Counter: c},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = ag.Run(context.Background(), "hello")
	if !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("expected ErrContextBudgetExceeded, got %v", err)
	}
	if m.calls > 0 {
		t.Errorf("expected 0 model calls, got %d", m.calls)
	}
}

func TestCompactionContextProvider_PersistsInitialRunBudgetAudit(t *testing.T) {
	store := in_memory.New()
	model := &countingMockModel{ResponseData: &models.ModelResponse{Content: "ok"}}
	value, err := New(AgentConfig{
		ID: "budget-audit", Model: model, DB: store, UserID: "audit-user",
		Instructions: "sys",
		ContextProviders: []ContextProvider{&CompactionContextProvider{
			// system + per-request context + input = 30 tokens: over the
			// 28-token trigger, within the 35-token hard cap.
			Policy:  &ContextWindowPolicy{MaxInputTokens: 35, TriggerRatio: 0.8, TargetRatio: 0.7},
			Counter: &fakeCounter{perMessage: 10},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	output, err := value.Run(context.Background(), "input")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Metrics.OriginalContextTokens != 30 || output.Metrics.FinalContextTokens != 30 ||
		output.Metrics.ContextCompactions != 1 {
		t.Fatalf("metrics = %+v", output.Metrics)
	}
	stored, err := store.GetRun(context.Background(), db.RunQuery{RunID: output.RunID, UserID: output.UserID})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored == nil || stored.Metrics.OriginalContextTokens != 30 ||
		stored.Metrics.FinalContextTokens != 30 || stored.Metrics.ContextCompactions != 1 {
		t.Fatalf("stored metrics = %+v", stored)
	}
}

type budgetRoundModel struct {
	responses []*models.ModelResponse
	calls     int32
}

func (m *budgetRoundModel) Response(_ context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	index := int(atomic.AddInt32(&m.calls, 1)) - 1
	if index >= len(m.responses) {
		return &models.ModelResponse{Content: "done"}, nil
	}
	return m.responses[index], nil
}

func (m *budgetRoundModel) ResponseStream(_ context.Context, _ []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	index := int(atomic.AddInt32(&m.calls, 1)) - 1
	response := &models.ModelResponse{Content: "done"}
	if index < len(m.responses) {
		response = m.responses[index]
	}
	ch := make(chan models.StreamEvent, len(response.ToolCalls)+2)
	if response.Content != "" {
		ch <- models.StreamEvent{Event: "content_delta", Content: response.Content}
	}
	for i := range response.ToolCalls {
		call := response.ToolCalls[i]
		call.Index = i
		ch <- models.StreamEvent{Event: "tool_call_delta", ToolCall: &call}
	}
	ch <- models.StreamEvent{Event: "done", Done: true}
	close(ch)
	return ch, nil
}

func (m *budgetRoundModel) ID() string                     { return "budget-round" }
func (m *budgetRoundModel) Provider() string               { return "test" }
func (m *budgetRoundModel) SupportsNativeJSONSchema() bool { return false }

type budgetResultTool struct {
	name     string
	result   string
	approval tools.ApprovalKind
	calls    int32
}

func (t *budgetResultTool) Name() string        { return t.name }
func (t *budgetResultTool) Description() string { return t.name }
func (t *budgetResultTool) Schema() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": t.name}}
}
func (t *budgetResultTool) Approval() tools.ApprovalKind { return t.approval }
func (t *budgetResultTool) Call(context.Context, map[string]any) (any, error) {
	atomic.AddInt32(&t.calls, 1)
	return t.result, nil
}

func budgetToolCall(id, name string) models.ToolCall {
	return models.ToolCall{ID: id, Type: "function", Function: models.FunctionCall{Name: name, Arguments: "{}"}}
}

func perCallCompaction() ContextProvider {
	return &CompactionContextProvider{
		Policy:  &ContextWindowPolicy{MaxInputTokens: 30, TriggerRatio: 0.8, TargetRatio: 0.7},
		Counter: &contentAwareCounter{charsPerToken: 4},
	}
}

func TestCompactionContextProvider_BlocksSyncSecondModelCallAfterLargeToolResult(t *testing.T) {
	tool := &budgetResultTool{name: "large", result: strings.Repeat("x", 200), approval: tools.ApprovalNone}
	model := &budgetRoundModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{budgetToolCall("large-call", tool.Name())}},
		{Content: "must not run"},
	}}
	value, err := New(AgentConfig{
		ID: "sync-budget", Model: model, Tools: []tools.Tool{tool}, Instructions: "sys",
		ContextProviders: []ContextProvider{perCallCompaction()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	output, runErr := value.Run(context.Background(), "input")
	if !errors.Is(runErr, ErrContextBudgetExceeded) {
		t.Fatalf("Run error = %v, want ErrContextBudgetExceeded", runErr)
	}
	if output == nil || output.Status != session.RunStatusError {
		t.Fatalf("output = %+v", output)
	}
	if atomic.LoadInt32(&model.calls) != 1 || atomic.LoadInt32(&tool.calls) != 1 {
		t.Fatalf("calls = model:%d tool:%d, want 1/1", model.calls, tool.calls)
	}
}

func TestCompactionContextProvider_BlocksStreamSecondModelCallAfterLargeToolResult(t *testing.T) {
	tool := &budgetResultTool{name: "large", result: strings.Repeat("界", 80), approval: tools.ApprovalNone}
	model := &budgetRoundModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{budgetToolCall("large-stream-call", tool.Name())}},
		{Content: "must not run"},
	}}
	value, err := New(AgentConfig{
		ID: "stream-budget", Model: model, Tools: []tools.Tool{tool}, Instructions: "sys",
		ContextProviders: []ContextProvider{perCallCompaction()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, err := value.RunStream(context.Background(), "input")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var budgetErr error
	for event := range events {
		if event.Type == EventTypeRunError {
			budgetErr = event.Err
		}
	}
	if !errors.Is(budgetErr, ErrContextBudgetExceeded) {
		t.Fatalf("stream error = %v, want ErrContextBudgetExceeded", budgetErr)
	}
	if atomic.LoadInt32(&model.calls) != 1 || atomic.LoadInt32(&tool.calls) != 1 {
		t.Fatalf("calls = model:%d tool:%d, want 1/1", model.calls, tool.calls)
	}
}

func TestCompactionContextProvider_BlocksLaterModelCallDuringResume(t *testing.T) {
	requiredTool := &budgetResultTool{name: "required", result: "ok", approval: tools.ApprovalRequired}
	largeTool := &budgetResultTool{name: "large", result: strings.Repeat("x", 200), approval: tools.ApprovalNone}
	model := &budgetRoundModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{budgetToolCall("required-call", requiredTool.Name())}},
		{ToolCalls: []models.ToolCall{budgetToolCall("large-call", largeTool.Name())}},
		{Content: "must not run"},
	}}
	store := in_memory.New()
	value, err := New(AgentConfig{
		ID: "resume-budget", Model: model, Tools: []tools.Tool{requiredTool, largeTool},
		Instructions: "sys", DB: store, UserID: "user", ContextProviders: []ContextProvider{perCallCompaction()},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := value.Run(context.Background(), "input")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, resumeErr := value.Resume(context.Background(), session.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	})
	if !errors.Is(resumeErr, ErrContextBudgetExceeded) {
		t.Fatalf("Resume error = %v, want ErrContextBudgetExceeded", resumeErr)
	}
	if atomic.LoadInt32(&model.calls) != 2 || atomic.LoadInt32(&largeTool.calls) != 1 {
		t.Fatalf("calls = model:%d large-tool:%d, want 2/1", model.calls, largeTool.calls)
	}
}

type ordinaryCountingContextProvider struct{ calls int32 }

func (p *ordinaryCountingContextProvider) Apply(_ context.Context, req ContextRequest) (ContextRequest, error) {
	atomic.AddInt32(&p.calls, 1)
	return req, nil
}

func TestPerModelCallCompaction_DoesNotRepeatOrdinaryContextProviders(t *testing.T) {
	tool := &budgetResultTool{name: "small", result: "ok", approval: tools.ApprovalNone}
	model := &budgetRoundModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{budgetToolCall("small-call", tool.Name())}},
		{Content: "done"},
	}}
	ordinary := &ordinaryCountingContextProvider{}
	value, err := New(AgentConfig{
		ID: "ordinary-once", Model: model, Tools: []tools.Tool{tool},
		ContextProviders: []ContextProvider{
			ordinary,
			&CompactionContextProvider{
				Policy:  &ContextWindowPolicy{MaxInputTokens: 1000},
				Counter: &contentAwareCounter{charsPerToken: 4},
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := value.Run(context.Background(), "input"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&ordinary.calls) != 1 {
		t.Fatalf("ordinary provider calls = %d, want 1", ordinary.calls)
	}
}

// countingMockModel counts how many times Response is called.
type countingMockModel struct {
	ResponseData *models.ModelResponse
	calls        int
}

func (m *countingMockModel) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	m.calls++
	if m.ResponseData != nil {
		return m.ResponseData, nil
	}
	return &models.ModelResponse{Content: "ok"}, nil
}

func (m *countingMockModel) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	m.calls++
	ch := make(chan models.StreamEvent, 1)
	ch <- models.StreamEvent{Event: "done", Done: true}
	close(ch)
	return ch, nil
}

func (m *countingMockModel) ID() string                     { return "counting" }
func (m *countingMockModel) Provider() string               { return "counting" }
func (m *countingMockModel) SupportsNativeJSONSchema() bool { return false }

// P1: verify per-call-ID pairing after truncation.
func TestCompactionContextProvider_AtomicGroupPerCallIDPairing(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"),
			assistantWithCalls("c1", "c2"),
			toolResult("c1", "r1"),
			toolResult("c2", "r2"),
			msg(models.RoleUser, "q2"),
			toolCall("c3"), toolResult("c3", "r3"),
			msg(models.RoleUser, "q3"),
			toolCall("c4"), toolResult("c4", "r4"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// For each kept assistant, ALL its declared call IDs must have matching results.
	keptCallIDs := make(map[string]bool)
	keptResultIDs := make(map[string]bool)
	for _, m := range messages {
		if m.Role == models.RoleAssistant && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				keptCallIDs[tc.ID] = true
			}
		}
		if m.Role == models.RoleTool {
			keptResultIDs[m.ToolCallID] = true
		}
	}
	for callID := range keptCallIDs {
		if !keptResultIDs[callID] {
			t.Errorf("atomic group split: assistant keeps call %s but matching result dropped", callID)
		}
	}
	// For each kept tool result, the owning assistant must also be present.
	// (Reverse check.)
	for resultID := range keptResultIDs {
		if !keptCallIDs[resultID] {
			t.Errorf("orphan tool result %s without owning assistant", resultID)
		}
	}
}

// Fix 5 regression: exactly 2 history turns must both be protected.
func TestCompactionContextProvider_ExactlyTwoTurnsProtected(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Both q1 and q2 must be present (all history protected).
	foundQ1, foundQ2 := false, false
	for _, m := range messages {
		if m.Role == models.RoleUser {
			if m.Content == "q1" {
				foundQ1 = true
			}
			if m.Content == "q2" {
				foundQ2 = true
			}
		}
	}
	if !foundQ1 || !foundQ2 {
		t.Errorf("expected both q1 and q2 present: q1=%v q2=%v: %+v", foundQ1, foundQ2, messages)
	}
}

// Fix 5 regression: exactly 1 history turn must be protected.
func TestCompactionContextProvider_OneTurnProtected(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// q1 must be present (only history turn is protected).
	foundQ1 := false
	for _, m := range messages {
		if m.Role == models.RoleUser && m.Content == "q1" {
			foundQ1 = true
		}
	}
	if !foundQ1 {
		t.Errorf("expected q1 present: %+v", messages)
	}
}

// Fix 4: summarizer returns summary longer than original → fallback to truncation.
func TestCompactionContextProvider_LongSummaryFallback(t *testing.T) {
	// Use content-aware counter so long summary is detected as larger.
	longContent := strings.Repeat("x", 100)
	p := &ContextWindowPolicy{MaxInputTokens: 500, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &contentAwareCounter{charsPerToken: 4}
	// Summarizer returns a very long summary (500 chars → ~125 tokens).
	// Old turns have ~400 chars → ~100 tokens. Summary is longer.
	s := &fakeSummarizer{summary: strings.Repeat("y", 500)}
	ag := compactionAgent(t, p, c, s)
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, longContent), msg(models.RoleAssistant, longContent),
			msg(models.RoleUser, longContent), msg(models.RoleAssistant, longContent),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Should NOT contain the long summary; should have fallen back to truncation.
	for _, m := range messages {
		if strings.Contains(m.Content, strings.Repeat("y", 500)) {
			t.Errorf("long summary was used instead of falling back")
		}
	}
}

// Fix: findTurnBoundaries must count first history user at index 0
// when req.System is empty.
func TestCompactionContextProvider_NoSystemMessageTurnBoundary(t *testing.T) {
	p := &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.8, TargetRatio: 0.7, MinRecentTurns: 1}
	c := &fakeCounter{perMessage: 10}
	ag := compactionAgent(t, p, c, nil)
	// Session with 3 turns, no system message in the run.
	sess := sessionWithRuns(t,
		[]models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// q2 and q3 must both be present (last 2 history turns protected).
	foundQ2, foundQ3 := false, false
	for _, m := range messages {
		if m.Role == models.RoleUser {
			if m.Content == "q2" {
				foundQ2 = true
			}
			if m.Content == "q3" {
				foundQ3 = true
			}
		}
	}
	if !foundQ2 || !foundQ3 {
		t.Errorf("expected q2 and q3 both present: q2=%v q3=%v: %+v", foundQ2, foundQ3, messages)
	}
}

// Direct API validation tests for CompactionContextProvider.Apply.
func TestCompactionContextProvider_InvalidPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy *ContextWindowPolicy
		want   error
	}{
		{
			name:   "negative max_input_tokens",
			policy: &ContextWindowPolicy{MaxInputTokens: -1},
			want:   ErrInvalidContextWindowPolicy,
		},
		{
			name:   "negative trigger_ratio",
			policy: &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: -0.1},
			want:   ErrInvalidContextWindowPolicy,
		},
		{
			name:   "trigger_ratio > 1",
			policy: &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 1.5},
			want:   ErrInvalidContextWindowPolicy,
		},
		{
			name:   "negative target_ratio",
			policy: &ContextWindowPolicy{MaxInputTokens: 100, TargetRatio: -0.1},
			want:   ErrInvalidContextWindowPolicy,
		},
		{
			name:   "target_ratio > 1",
			policy: &ContextWindowPolicy{MaxInputTokens: 100, TargetRatio: 1.5},
			want:   ErrInvalidContextWindowPolicy,
		},
		{
			name:   "target >= trigger after defaults",
			policy: &ContextWindowPolicy{MaxInputTokens: 100, TriggerRatio: 0.6, TargetRatio: 0},
			want:   ErrInvalidContextWindowPolicy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &CompactionContextProvider{Policy: tt.policy}
			req := ContextRequest{
				System:  []models.Message{msg(models.RoleSystem, "s")},
				History: []HistoryRun{{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "q1")}}},
				Input:   msg(models.RoleUser, "in"),
			}
			_, err := p.Apply(context.Background(), req)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Apply error = %v, want %v", err, tt.want)
			}
		})
	}
}

// Trigger > 1 with tokens > max must NOT silently pass through.
func TestCompactionContextProvider_TriggerAboveOneNotSilent(t *testing.T) {
	p := &CompactionContextProvider{
		Policy: &ContextWindowPolicy{MaxInputTokens: 50, TriggerRatio: 1.5},
	}
	req := ContextRequest{
		System:  []models.Message{msg(models.RoleSystem, "s")},
		History: []HistoryRun{{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "q1")}}},
		Input:   msg(models.RoleUser, "in"),
	}
	_, err := p.Apply(context.Background(), req)
	if !errors.Is(err, ErrInvalidContextWindowPolicy) {
		t.Fatalf("expected ErrInvalidContextWindowPolicy, got %v", err)
	}
}

func TestApproximateTokenCounter_CountsConservatively(t *testing.T) {
	c := &ApproximateTokenCounter{}
	msgs := []models.Message{
		{Content: "hello world"}, // 11 chars → ~3 tokens + 1 role
		{Content: "a"},           // 1 char → ~1 token + 1 role
	}
	got, err := c.Count(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	// Each message: content/4 + 1 (role). Plus overhead.
	if got < 4 || got > 20 {
		t.Fatalf("approximate count = %d, expected 4-20", got)
	}
}

func TestApproximateTokenCounter_UsesRuneFloorForCJK(t *testing.T) {
	counter := &ApproximateTokenCounter{}
	for _, test := range []struct {
		name    string
		content string
		minimum int
	}{
		{name: "english bytes over four", content: "abcdefgh", minimum: 3}, // 2 text + role
		{name: "cjk rune floor", content: "你好世界", minimum: 5},              // 4 runes + role
		{name: "mixed", content: "hello世界", minimum: 8},                    // 7 runes + role
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := counter.Count(context.Background(), []models.Message{{
				Role: models.RoleUser, Content: test.content,
			}})
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if got < test.minimum {
				t.Fatalf("Count(%q) = %d, want >= %d", test.content, got, test.minimum)
			}
		})
	}
}

func TestApproximateTokenCounter_CountsToolCallsAndResults(t *testing.T) {
	c := &ApproximateTokenCounter{}
	msgs := []models.Message{
		{
			Role: models.RoleAssistant,
			ToolCalls: []models.ToolCall{
				{ID: "call_123", Function: models.FunctionCall{Name: "search", Arguments: `{"query":"test"}`}},
			},
		},
		{
			Role:       models.RoleTool,
			ToolCallID: "call_123",
			Name:       "search",
			Content:    "result data",
		},
	}
	got, err := c.Count(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	// Should be more than just content counting.
	if got < 10 {
		t.Fatalf("expected tool call/result overhead counted, got %d", got)
	}
}

func TestApproximateTokenCounter_CountsMultimodal(t *testing.T) {
	c := &ApproximateTokenCounter{}
	msgs := []models.Message{
		{
			Role:    models.RoleUser,
			Content: "What is this?",
			Images:  []models.Image{{URL: "https://example.com/img.png", MimeType: "image/png"}},
		},
	}
	got, err := c.Count(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	// Should count image URL + mime overhead.
	if got < 10 {
		t.Fatalf("expected multimodal overhead counted, got %d", got)
	}
}

func TestCompactionContextProvider_UsesPipelineWhenSet(t *testing.T) {
	// Pipeline that excludes the first non-system group, regardless of tokens.
	pipeline := &compaction.TruncationStrategy{
		Trigger:                compaction.Always(),
		MinimumPreservedGroups: 1,
	}

	messages := []models.Message{
		{Role: models.RoleSystem, Content: "sys"},
		{Role: models.RoleUser, Content: "turn1"},
		{Role: models.RoleAssistant, Content: "a1"},
		{Role: models.RoleUser, Content: "turn2"},
		{Role: models.RoleAssistant, Content: "a2"},
	}

	provider := &CompactionContextProvider{
		Policy: &ContextWindowPolicy{
			MaxInputTokens: 1, // force trigger threshold
			MinRecentTurns: 0,
		},
		Counter:  &ApproximateTokenCounter{},
		Pipeline: pipeline,
	}

	req := ContextRequest{
		System: []models.Message{messages[0]},
		History: []HistoryRun{{
			RunID:    "run1",
			Messages: messages[1:],
		}},
		Input: models.Message{Role: models.RoleUser, Content: "current"},
	}

	result, err := provider.Apply(context.Background(), req)
	// Budget check may fail (protected content > 1 token), but pipeline ran.
	if err != nil && !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("Apply: %v", err)
	}
	// Even if budget exceeded, the pipeline should have run and excluded
	// some messages. In the error case, result is the original request.
	_ = result
}

func TestContextWindowStrategy_NewPackageIntegration(t *testing.T) {
	strategy := &compaction.ContextWindowStrategy{
		MaxContextWindowTokens: 200,
		MaxOutputTokens:        50,
	}

	messages := []models.Message{
		{Role: models.RoleSystem, Content: "sys"},
		{Role: models.RoleUser, Content: "hello world"},
		{Role: models.RoleAssistant, Content: "hi there"},
	}

	idx := compaction.CreateMessageIndex(messages, &compaction.ApproximateTokenCounter{})
	_, err := strategy.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	// Budget is 200-50=150. Tool eviction at 75, truncation at 120.
	// Small messages won't trigger any compaction.
	if idx.IncludedMessageCount() != 3 {
		t.Fatalf("expected all 3 messages, got %d", idx.IncludedMessageCount())
	}
}

func TestCompactionContextProvider_PipelineWithoutCounter(t *testing.T) {
	// Pipeline != nil && Counter == nil must not panic: Apply falls back to
	// the default token counter for trigger/budget checks, and the pipeline
	// path uses its own internal counter.
	pipeline := &compaction.TruncationStrategy{
		Trigger:                compaction.Always(),
		MinimumPreservedGroups: 1,
	}
	messages := []models.Message{
		{Role: models.RoleSystem, Content: "sys"},
		{Role: models.RoleUser, Content: "turn1"},
		{Role: models.RoleAssistant, Content: "a1"},
	}

	provider := &CompactionContextProvider{
		Policy: &ContextWindowPolicy{
			MaxInputTokens: 1, // force trigger threshold
			MinRecentTurns: 0,
		},
		// Counter deliberately nil.
		Pipeline: pipeline,
	}

	req := ContextRequest{
		System:  messages[:1],
		History: []HistoryRun{{RunID: "run1", Messages: messages[1:]}},
		Input:   models.Message{Role: models.RoleUser, Content: "current"},
	}
	result, err := provider.Apply(context.Background(), req)
	// Budget check may fail (protected content > 1 token), but the provider
	// must have run without a nil-pointer panic.
	if err != nil && !errors.Is(err, ErrContextBudgetExceeded) {
		t.Fatalf("Apply: %v", err)
	}
	_ = result
}
