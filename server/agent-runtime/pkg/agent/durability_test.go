package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/vectordb"
)

// gatedModel blocks calls whose input contains gateOn until release is
// closed, and records every request.
type gatedModel struct {
	scriptedModel
	gateOn  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *gatedModel) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	if m.gateOn != "" && messages[len(messages)-1].Content == m.gateOn {
		m.once.Do(func() { close(m.entered) })
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.scriptedModel.Response(ctx, messages, opts)
}

func newGatedModel(gateOn string) *gatedModel {
	return &gatedModel{gateOn: gateOn, entered: make(chan struct{}), release: make(chan struct{})}
}

func TestConcurrentRunsOnSameSessionAreSerialized(t *testing.T) {
	m := newGatedModel("first")
	ag, err := New(AgentConfig{Model: m, DB: in_memory.New(), UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	seed, err := ag.Run(context.Background(), "seed")
	if err != nil {
		t.Fatal(err)
	}
	req := func(input string) session.RunRequest {
		return session.RunRequest{Input: session.RunInput{Message: input}, SessionID: seed.SessionID}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := ag.RunWith(context.Background(), req("first")); err != nil {
			t.Error(err)
		}
	}()
	<-m.entered
	go func() {
		defer wg.Done()
		if _, err := ag.RunWith(context.Background(), req("second")); err != nil {
			t.Error(err)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	m.mu.Lock()
	calls := len(m.received)
	m.mu.Unlock()
	if calls != 1 { // only the seed run reached the model
		t.Fatalf("second run reached the model while the session was busy: calls=%d", calls)
	}
	close(m.release)
	wg.Wait()

	last := m.received[len(m.received)-1]
	var sawFirst bool
	for _, msg := range last {
		sawFirst = sawFirst || msg.Content == "first"
	}
	if !sawFirst {
		t.Fatalf("second run did not see the first run's transcript: %+v", last)
	}
}

func TestSessionLockTimeoutReturnsBusy(t *testing.T) {
	m := newGatedModel("first")
	ag, err := New(AgentConfig{Model: m, DB: in_memory.New(), UserID: "u", SessionLockTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	seed, err := ag.Run(context.Background(), "seed")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: "first"}, SessionID: seed.SessionID})
	}()
	<-m.entered
	_, err = ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: "second"}, SessionID: seed.SessionID})
	close(m.release)
	<-done
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("err = %v, want ErrSessionBusy", err)
	}
}

func TestSessionLockIsReentrantWithinOneCallChain(t *testing.T) {
	ag, err := New(AgentConfig{Model: &scriptedModel{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, unlock, err := ag.lockSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	nestedCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, nestedUnlock, err := ag.lockSession(nestedCtx, "s1"); err != nil {
		t.Fatalf("nested lock on the same session deadlocked: %v", err)
	} else {
		nestedUnlock()
	}
}

type staticMemory struct{ text atomic.Value }

func (m *staticMemory) CreateMemory(context.Context, string, string, string) (*memory.UserMemory, error) {
	return nil, nil
}
func (m *staticMemory) GetMemories(context.Context, string, string, int) ([]*memory.UserMemory, error) {
	return []*memory.UserMemory{{Memory: m.text.Load().(string)}}, nil
}

func TestPerRequestContextFollowsStableHistoryPrefix(t *testing.T) {
	m := &scriptedModel{}
	mem := &staticMemory{}
	mem.text.Store("likes tea")
	ag, err := New(AgentConfig{Model: m, DB: in_memory.New(), UserID: "u", Instructions: "sys", Memory: mem})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ag.Run(context.Background(), "q1")
	if err != nil {
		t.Fatal(err)
	}
	mem.text.Store("likes coffee") // per-request context changes between turns
	if _, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: "q2"}, SessionID: first.SessionID}); err != nil {
		t.Fatal(err)
	}
	r1, r2 := m.received[0], m.received[1]
	// r1: [sys, ctx, q1]; r2: [sys, q1, done, ctx, q2]
	if len(r1) != 3 || len(r2) != 5 {
		t.Fatalf("unexpected layout:\n%+v\n%+v", r1, r2)
	}
	if r1[0].Content != "sys" || r2[0].Content != r1[0].Content {
		t.Fatalf("leading system message is not stable: %q vs %q", r1[0].Content, r2[0].Content)
	}
	if r2[1].Content != "q1" || r2[2].Content != "done" {
		t.Fatalf("history must directly follow the system prefix: %+v", r2)
	}
	if r2[3].Role != models.RoleSystem || !strings.Contains(r2[3].Content, "likes coffee") || !strings.Contains(r2[3].Content, "Current time:") {
		t.Fatalf("per-request context = %+v", r2[3])
	}
	for _, msg := range first.Messages {
		if strings.Contains(msg.Content, "likes tea") {
			t.Fatal("per-request context must not be persisted in the transcript")
		}
	}
}

func TestLegacySystemContextKeepsSingleSystemMessage(t *testing.T) {
	m := &scriptedModel{}
	ag, err := New(AgentConfig{Model: m, Instructions: "sys", LegacySystemContext: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	got := m.received[0]
	if len(got) != 2 || !strings.HasPrefix(got[0].Content, "sys") || !strings.Contains(got[0].Content, "Current time:") {
		t.Fatalf("legacy layout = %+v", got)
	}
}

// barrierRetrieval makes memory and knowledge each wait for the other, so
// the run only succeeds when both lookups are in flight together.
type barrierRetrieval struct{ arrived sync.WaitGroup }

func (b *barrierRetrieval) wait(ctx context.Context) error {
	b.arrived.Done()
	done := make(chan struct{})
	go func() { b.arrived.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(time.Second):
		return errors.New("lookups ran sequentially")
	case <-ctx.Done():
		return ctx.Err()
	}
}

type barrierMemory struct{ *barrierRetrieval }

func (barrierMemory) CreateMemory(context.Context, string, string, string) (*memory.UserMemory, error) {
	return nil, nil
}
func (b barrierMemory) GetMemories(ctx context.Context, _, _ string, _ int) ([]*memory.UserMemory, error) {
	return []*memory.UserMemory{{Memory: "m"}}, b.wait(ctx)
}

type barrierKnowledge struct{ *barrierRetrieval }

func (b barrierKnowledge) Search(ctx context.Context, _ string, _ int) ([]vectordb.Document, error) {
	return []vectordb.Document{{Content: "k"}}, b.wait(ctx)
}
func (barrierKnowledge) GetContent(context.Context, string) (string, error) { return "", nil }

func TestMemoryAndKnowledgeLookupsRunConcurrently(t *testing.T) {
	b := &barrierRetrieval{}
	b.arrived.Add(2)
	m := &scriptedModel{}
	ag, err := New(AgentConfig{Model: m, Memory: barrierMemory{b}, Knowledge: barrierKnowledge{b}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	ctxMsg := m.received[0][0].Content
	if strings.Index(ctxMsg, "User memories") > strings.Index(ctxMsg, "Relevant knowledge") {
		t.Fatalf("sections out of order: %q", ctxMsg)
	}
}

func TestHistoryIsBoundedAndDropsOldReasoning(t *testing.T) {
	m := &scriptedModel{responses: []*models.ModelResponse{
		{Content: "a1", ReasoningContent: "long chain of thought"},
		{Content: "a2"}, {Content: "a3"}, {Content: "a4"},
	}}
	ag, err := New(AgentConfig{Model: m, DB: in_memory.New(), UserID: "u", MaxHistoryRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ag.Run(context.Background(), "q1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Messages[len(first.Messages)-1].ReasoningContent == "" {
		t.Fatal("the transcript itself must keep reasoning")
	}
	for _, q := range []string{"q2", "q3", "q4"} {
		if _, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: q}, SessionID: first.SessionID}); err != nil {
			t.Fatal(err)
		}
	}
	second := m.received[1]
	for _, msg := range second {
		if msg.ReasoningContent != "" {
			t.Fatalf("reasoning of an earlier run was replayed: %+v", msg)
		}
	}
	var users []string
	for _, msg := range m.received[3] {
		if msg.Role == models.RoleUser {
			users = append(users, msg.Content)
		}
	}
	if strings.Join(users, ",") != "q2,q3,q4" {
		t.Fatalf("history users = %v, want the two most recent runs plus the input", users)
	}
}

type limitsModel struct {
	scriptedModel
	window, output int
}

func (m *limitsModel) ContextWindow() int   { return m.window }
func (m *limitsModel) MaxOutputTokens() int { return m.output }

func budgetOf(t *testing.T, ag *Agent) int {
	t.Helper()
	for _, p := range ag.cfg.ContextProviders {
		if c, ok := p.(*CompactionContextProvider); ok {
			return c.Policy.MaxInputTokens
		}
	}
	return 0
}

func TestDefaultContextBudgetComesFromModelLimits(t *testing.T) {
	newAgent := func(cfg AgentConfig) *Agent {
		ag, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return ag
	}
	primary := &limitsModel{window: 128_000, output: 65_536}
	if got := budgetOf(t, newAgent(AgentConfig{Model: primary})); got != 96_000 {
		t.Fatalf("budget = %d, want window minus a quarter reserved for output", got)
	}
	maxTokens := 8_000
	if got := budgetOf(t, newAgent(AgentConfig{Model: primary, MaxTokens: &maxTokens})); got != 120_000 {
		t.Fatalf("budget = %d, want window minus MaxTokens", got)
	}
	small := &limitsModel{window: 32_000, output: 4_000}
	if got := budgetOf(t, newAgent(AgentConfig{Model: primary, FallbackModels: []models.Model{small}})); got != 28_000 {
		t.Fatalf("budget = %d, want the smallest candidate window", got)
	}
	if got := budgetOf(t, newAgent(AgentConfig{Model: primary, ContextBudget: &ContextWindowPolicy{}})); got != 0 {
		t.Fatalf("budget = %d, want disabled", got)
	}
	explicit := &CompactionContextProvider{Policy: &ContextWindowPolicy{MaxInputTokens: 10}}
	ag := newAgent(AgentConfig{Model: primary, ContextProviders: []ContextProvider{explicit}})
	if len(ag.cfg.ContextProviders) != 1 || budgetOf(t, ag) != 10 {
		t.Fatalf("an explicit compaction provider must not be duplicated: %+v", ag.cfg.ContextProviders)
	}
	if got := budgetOf(t, newAgent(AgentConfig{Model: &scriptedModel{}})); got != 0 {
		t.Fatalf("budget = %d, want none for models without limits", got)
	}
	if _, err := New(AgentConfig{Model: primary, ContextBudget: &ContextWindowPolicy{MaxInputTokens: 10, TriggerRatio: 0.5, TargetRatio: 0.6}}); !errors.Is(err, ErrInvalidContextWindowPolicy) {
		t.Fatalf("err = %v, want invalid policy", err)
	}
}

// cancelElsewhereTool simulates run.Manager.Cancel on another replica.
type cancelElsewhereTool struct {
	store db.RunStore
	runID string
}

func (c *cancelElsewhereTool) Name() string           { return "cancel_elsewhere" }
func (c *cancelElsewhereTool) Description() string    { return "d" }
func (c *cancelElsewhereTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (c *cancelElsewhereTool) Call(ctx context.Context, _ map[string]any) (any, error) {
	return "ok", cancelStoredRun(ctx, c.store, c.runID)
}

func cancelStoredRun(ctx context.Context, store db.RunStore, runID string) error {
	run, err := store.GetRun(ctx, db.RunQuery{RunID: runID})
	if err != nil || run == nil {
		return errors.Join(err, errors.New("run not found"))
	}
	run.Status = session.RunStatusCancelled
	run.Error = context.Canceled.Error()
	return store.UpdateRun(ctx, run, session.RunStatusRunning)
}

func TestRoundCheckpointDetectsExternalCancel(t *testing.T) {
	store := in_memory.New()
	m := &scriptedModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{newToolCall("c1", "cancel_elsewhere", `{}`)}},
		{Content: "should not be reached"},
	}}
	tool := &cancelElsewhereTool{store: store, runID: "run-ext"}
	ag, err := New(AgentConfig{Model: m, DB: store, UserID: "u", Tools: []tools.Tool{tool}, RunLeaseTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.RunWith(context.Background(), session.RunRequest{RunID: "run-ext", Input: session.RunInput{Message: "q"}})
	if !errors.Is(err, ErrRunCancelledExternally) {
		t.Fatalf("err = %v, want ErrRunCancelledExternally", err)
	}
	if out.Status != session.RunStatusCancelled || len(m.received) != 1 {
		t.Fatalf("status=%s model calls=%d", out.Status, len(m.received))
	}
	stored, _ := store.GetRun(context.Background(), db.RunQuery{RunID: "run-ext"})
	if stored.Status != session.RunStatusCancelled {
		t.Fatalf("stored status = %s", stored.Status)
	}
}

// probeTool records how many transcript messages are durable when it runs.
type probeTool struct {
	store db.RunStore
	runID string
	seen  []int
}

func (p *probeTool) Name() string           { return "probe" }
func (p *probeTool) Description() string    { return "d" }
func (p *probeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (p *probeTool) Call(ctx context.Context, _ map[string]any) (any, error) {
	run, err := p.store.GetRun(ctx, db.RunQuery{RunID: p.runID})
	if err != nil {
		return nil, err
	}
	p.seen = append(p.seen, len(run.Messages))
	return "ok", nil
}

func TestEachToolRoundIsCheckpointed(t *testing.T) {
	store := in_memory.New()
	m := &scriptedModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{newToolCall("c1", "probe", `{}`)}},
		{ToolCalls: []models.ToolCall{newToolCall("c2", "probe", `{}`)}},
	}}
	probe := &probeTool{store: store, runID: "run-ckpt"}
	ag, err := New(AgentConfig{Model: m, DB: store, UserID: "u", Tools: []tools.Tool{probe}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.RunWith(context.Background(), session.RunRequest{RunID: "run-ckpt", Input: session.RunInput{Message: "q"}}); err != nil {
		t.Fatal(err)
	}
	// The tool calls are durable before each tool runs (write-ahead):
	// round 1 sees user + assistant; round 2 also round 1's result and the
	// second assistant turn.
	if len(probe.seen) != 2 || probe.seen[0] != 2 || probe.seen[1] != 4 {
		t.Fatalf("durable transcript sizes = %v, want [2 4]", probe.seen)
	}
}

// blockingModel blocks until the run context is cancelled.
type blockingModel struct{ entered chan struct{} }

func (b *blockingModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (b *blockingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not streamed")
}
func (*blockingModel) ID() string       { return "blocking" }
func (*blockingModel) Provider() string { return "test" }

func TestHeartbeatCancelsRunCancelledByAnotherProcess(t *testing.T) {
	store := in_memory.New()
	m := &blockingModel{entered: make(chan struct{})}
	ag, err := New(AgentConfig{Model: m, DB: store, UserID: "u", RunLeaseTimeout: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-m.entered
		if err := cancelStoredRun(context.Background(), store, "run-hb"); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := ag.RunWith(ctx, session.RunRequest{RunID: "run-hb", Input: session.RunInput{Message: "q"}})
	if !errors.Is(err, ErrRunCancelledExternally) {
		t.Fatalf("err = %v, want ErrRunCancelledExternally", err)
	}
	if out.Status != session.RunStatusCancelled || out.LeaseTimeout != 40*time.Millisecond {
		t.Fatalf("out = status %s lease %s", out.Status, out.LeaseTimeout)
	}
}

func TestStaleLeasedRunsAreExpired(t *testing.T) {
	store := in_memory.New()
	ctx := context.Background()
	sess := &session.AgentSession{SessionID: "s", AgentID: "a", UserID: "u"}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, run := range []*session.RunOutput{
		{RunID: "stale", Status: session.RunStatusRunning, LeaseTimeout: time.Minute},
		{RunID: "fresh", Status: session.RunStatusRunning, LeaseTimeout: 2 * time.Hour},
		{RunID: "unleased", Status: session.RunStatusRunning},
		{RunID: "paused", Status: session.RunStatusPaused, LeaseTimeout: time.Minute},
	} {
		run.SessionID, run.AgentID, run.UserID, run.CreatedAt, run.UpdatedAt = "s", "a", "u", old, old
		if err := store.AppendRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	ag, err := New(AgentConfig{Model: &scriptedModel{}, DB: store})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := ag.ExpireStaleRuns(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "stale" {
		t.Fatalf("expired = %v, %v", ids, err)
	}
	stale, _ := store.GetRun(ctx, db.RunQuery{RunID: "stale"})
	if stale.Status != session.RunStatusError || !strings.Contains(stale.Error, "lease expired") {
		t.Fatalf("stale run = %s %q", stale.Status, stale.Error)
	}
	if err := store.TouchRun(ctx, db.RunQuery{RunID: "stale"}); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("touch of an expired run = %v, want ErrConflict", err)
	}
}

type failingModel struct {
	calls atomic.Int32
}

func (f *failingModel) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	f.calls.Add(1)
	return nil, &models.ProviderError{Provider: "test", StatusCode: 503, Message: "overloaded"}
}
func (f *failingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	f.calls.Add(1)
	return nil, &models.ProviderError{Provider: "test", StatusCode: 503, Message: "overloaded"}
}
func (*failingModel) ID() string       { return "failing" }
func (*failingModel) Provider() string { return "test" }

func TestCircuitBreakerSkipsFailingPrimaryUntilCooldown(t *testing.T) {
	primary := &failingModel{}
	fallback := &scriptedModel{}
	ag, err := New(AgentConfig{
		Model: primary, FallbackModels: []models.Model{fallback},
		CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: 2, Cooldown: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ag.breakers.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		if _, err := ag.Run(context.Background(), "q"); err != nil {
			t.Fatal(err)
		}
	}
	if got := primary.calls.Load(); got != 2 {
		t.Fatalf("primary calls = %d, want 2 (circuit opens after the threshold)", got)
	}
	now = now.Add(2 * time.Minute)
	if _, err := ag.Run(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if got := primary.calls.Load(); got != 3 {
		t.Fatalf("primary calls = %d, want one probe after the cooldown", got)
	}
	if len(fallback.received) != 5 {
		t.Fatalf("fallback calls = %d", len(fallback.received))
	}
}

func TestCircuitBreakerFailsOpenWhenAllCircuitsAreOpen(t *testing.T) {
	b := newModelBreakers(AgentConfig{FallbackModels: []models.Model{&scriptedModel{}}, CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: 1}})
	ag := &Agent{breakers: b}
	overload := &models.ProviderError{StatusCode: 503}
	b.record(0, overload)
	b.record(1, overload)
	if got := ag.candidateOrder(2); len(got) != 2 {
		t.Fatalf("order = %v, want every candidate when all circuits are open", got)
	}
	b.record(0, &models.ProviderError{StatusCode: 400})
	if newModelBreakers(AgentConfig{}) != nil {
		t.Fatal("breakers must be disabled without fallbacks")
	}
}

func TestConcurrentSessionStateUpdatesAreNotLost(t *testing.T) {
	store := in_memory.New()
	ctx := context.Background()
	if err := store.CreateSession(ctx, &session.AgentSession{SessionID: "s", AgentID: "agent", UserID: "u"}); err != nil {
		t.Fatal(err)
	}
	ag, err := New(AgentConfig{Model: &scriptedModel{}, DB: store})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			if err := ag.UpdateSessionState(ctx, "s", map[string]any{key: true}); err != nil {
				t.Error(err)
			}
		}(string(rune('a' + i)))
	}
	wg.Wait()
	state, err := ag.GetSessionState(ctx, "s")
	if err != nil || len(state) != 4 {
		t.Fatalf("state = %v, %v", state, err)
	}
}

type usageModel struct {
	scriptedModel
	id   string
	cost float64
}

func (m *usageModel) ID() string                                        { return m.id }
func (m *usageModel) EstimateCost(usage *models.MessageMetrics) float64 { return m.cost }

func TestMaxRunTokensStopsRunawayToolLoop(t *testing.T) {
	var calls atomic.Int32
	call := func(id string) *models.ModelResponse {
		return &models.ModelResponse{ToolCalls: []models.ToolCall{newToolCall(id, "mock_tool", `{}`)}, Usage: &models.MessageMetrics{InputTokens: 60, OutputTokens: 10}}
	}
	m := &scriptedModel{responses: []*models.ModelResponse{call("c1"), call("c2"), call("c3")}}
	ag, err := New(AgentConfig{Model: m, MaxRunTokens: 100, Tools: []tools.Tool{&counterTool{name: "mock_tool", calls: &calls}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.Run(context.Background(), "q")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	if len(m.received) != 2 || out.Metrics.TotalTokens != 140 {
		t.Fatalf("model calls = %d, tokens = %d", len(m.received), out.Metrics.TotalTokens)
	}
}

func TestLocalUsageQuotaChargesServingModelAndRefusesWhenUsedUp(t *testing.T) {
	quota := NewLocalUsageQuota(LocalQuotaConfig{CostPerUserUSD: 0.01, TokensPerNamespace: 1000})
	primary := &failingModel{}
	fallback := &usageModel{id: "fallback", cost: 0.006, scriptedModel: scriptedModel{responses: []*models.ModelResponse{
		{Content: "a", Usage: &models.MessageMetrics{InputTokens: 10, OutputTokens: 5}},
		{Content: "b", Usage: &models.MessageMetrics{InputTokens: 10, OutputTokens: 5}},
	}}}
	ag, err := New(AgentConfig{Model: primary, FallbackModels: []models.Model{fallback}, UsageGuard: quota, UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // 0.006 then 0.012: the second call overshoots
		if _, err := ag.Run(context.Background(), "q"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if _, err := ag.Run(context.Background(), "q"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded once the user's cost quota is used up", err)
	}
	other, err := New(AgentConfig{Model: fallback, UsageGuard: quota, UserID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Run(context.Background(), "q"); err != nil {
		t.Fatalf("another user must not share alice's quota: %v", err)
	}
}

func TestLocalUsageQuotaResetsEachWindow(t *testing.T) {
	q := NewLocalUsageQuota(LocalQuotaConfig{TokensPerUser: 10, Window: time.Hour}).(*localUsageQuota)
	now := time.Now()
	q.now = func() time.Time { return now }
	scope := UsageScope{UserID: "u"}
	q.Record(context.Background(), scope, UsageRecord{TotalTokens: 10})
	if err := q.Admit(context.Background(), scope); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v", err)
	}
	now = now.Add(time.Hour)
	if err := q.Admit(context.Background(), scope); err != nil {
		t.Fatalf("quota did not reset: %v", err)
	}
}

func TestTokenCalibrationTracksProviderUsage(t *testing.T) {
	c := newCalibratedCounter(nil)
	msgs := []models.Message{{Role: models.RoleUser, Content: strings.Repeat("你好世界", 100)}}
	est, _ := c.Count(context.Background(), msgs)
	// The provider reports half the estimate (rune counting overestimates
	// this tokenizer); repeated observations converge on it.
	for i := 0; i < 30; i++ {
		c.observe(est, est/2)
	}
	if got, _ := c.Count(context.Background(), msgs); got < est*45/100 || got > est*55/100 {
		t.Fatalf("calibrated count = %d, want about %d", got, est/2)
	}
	c.observe(est, est*100) // outliers are clamped
	if got, _ := c.Count(context.Background(), msgs); got > est*3 {
		t.Fatalf("ratio not clamped: %d", got)
	}
}

func TestTokenCalibrationKeepsToolOverheadFixed(t *testing.T) {
	tool := models.Tool{Type: "function", Function: models.ToolFunction{Name: "big", Description: strings.Repeat("schema ", 400)}}
	c := newCalibratedCounter([]models.Tool{tool})
	short := []models.Message{{Role: models.RoleUser, Content: "hi"}}
	long := []models.Message{{Role: models.RoleUser, Content: strings.Repeat("word ", 4000)}}
	shortEst, _ := c.Count(context.Background(), short)
	c.observe(shortEst, shortEst) // the provider agrees once tools are counted
	longEst, _ := c.Count(context.Background(), long)
	base, _ := ApproximateTokenCounter{}.Count(context.Background(), long)
	if longEst > base+c.overhead+1 {
		t.Fatalf("tool overhead inflated a long context: %d > %d", longEst, base+c.overhead)
	}
}

type summaryModel struct {
	scriptedModel
	prompt []models.Message
}

func (m *summaryModel) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	m.prompt = messages
	return &models.ModelResponse{Content: "user likes tea; asked about 2+2 (=4)"}, nil
}

func TestModelSummarizerSendsPlainTranscript(t *testing.T) {
	m := &summaryModel{}
	summary, err := NewModelSummarizer(m).Summarize(context.Background(), []models.Message{
		{Role: models.RoleUser, Content: "I like tea. What is 2+2?"},
		{Role: models.RoleAssistant, ToolCalls: []models.ToolCall{newToolCall("c1", "calculator", `{"expression":"2+2"}`)}, ReasoningContent: "use the tool"},
		{Role: models.RoleTool, ToolCallID: "c1", Name: "calculator", Content: "4"},
	})
	if err != nil || !strings.Contains(summary, "tea") {
		t.Fatalf("summary = %q, %v", summary, err)
	}
	if len(m.prompt) != 2 || m.prompt[1].Role != models.RoleUser || !strings.Contains(m.prompt[1].Content, "called calculator") ||
		strings.Contains(m.prompt[1].Content, "use the tool") {
		t.Fatalf("summarizer prompt = %+v", m.prompt)
	}
	for _, msg := range m.prompt {
		if len(msg.ToolCalls) > 0 || msg.Role == models.RoleTool {
			t.Fatal("tool-call structure must not reach the summarizer model")
		}
	}
}

func TestDefaultBudgetCalibratesAndSummarizes(t *testing.T) {
	primary := &limitsModel{window: 128_000, output: 65_536}
	summarizer := NewModelSummarizer(&summaryModel{})
	ag, err := New(AgentConfig{Model: primary, ContextSummarizer: summarizer})
	if err != nil {
		t.Fatal(err)
	}
	var budget *CompactionContextProvider
	for _, p := range ag.cfg.ContextProviders {
		budget, _ = p.(*CompactionContextProvider)
	}
	if budget == nil || budget.Summarizer != summarizer || budget.Counter != ag.calibration || ag.calibration == nil {
		t.Fatalf("budget = %+v", budget)
	}
	primary.responses = []*models.ModelResponse{{Content: "ok", Usage: &models.MessageMetrics{InputTokens: 1000}}}
	if _, err := ag.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if ag.calibration.currentRatio() == 0 {
		t.Fatal("provider usage did not calibrate the budget counter")
	}
}

// losableLocker hands out locks that the test can declare lost.
type losableLocker struct {
	lost     chan struct{}
	unlocked atomic.Int32
}

func (l *losableLocker) LockSession(ctx context.Context, key string) (func(), error) {
	unlock, _, err := l.LockSessionWatch(ctx, key)
	return unlock, err
}

func (l *losableLocker) LockSessionWatch(context.Context, string) (func(), <-chan struct{}, error) {
	return func() { l.unlocked.Add(1) }, l.lost, nil
}

func TestLostSessionLockStopsTheRun(t *testing.T) {
	store := in_memory.New()
	locker := &losableLocker{lost: make(chan struct{})}
	first, err := New(AgentConfig{Model: &scriptedModel{}, DB: store, UserID: "u", SessionLocker: locker})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := first.Run(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}

	m := &blockingModel{entered: make(chan struct{})}
	ag, err := New(AgentConfig{Model: m, DB: store, UserID: "u", SessionLocker: locker})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-m.entered
		close(locker.lost)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := ag.RunWith(ctx, session.RunRequest{SessionID: prior.SessionID, Input: session.RunInput{Message: "q"}})
	if !errors.Is(err, ErrSessionLockLost) {
		t.Fatalf("err = %v, want ErrSessionLockLost", err)
	}
	if out.Status != session.RunStatusCancelled {
		t.Fatalf("status = %s", out.Status)
	}
	if got := locker.unlocked.Load(); got != 1 {
		t.Fatalf("unlock called %d times, want 1", got)
	}
}

func TestFallbackUsageDoesNotCalibratePrimaryBudget(t *testing.T) {
	fallback := &limitsModel{window: 128_000, output: 8_000}
	fallback.responses = []*models.ModelResponse{{Content: "ok", Usage: &models.MessageMetrics{InputTokens: 50_000}}}
	ag, err := New(AgentConfig{
		Model: &failingModel{}, FallbackModels: []models.Model{fallback},
		RetryConfig: RetryConfig{MaxRetries: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ag.calibration == nil {
		t.Fatal("no calibrated budget")
	}
	if _, err := ag.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if r := ag.calibration.currentRatio(); r != 0 {
		t.Fatalf("fallback usage calibrated the primary's counter: ratio %.2f", r)
	}
}
