package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// scriptedModel returns one response per call and records the messages it
// received, so tests can assert what was sent back to the provider.
type scriptedModel struct {
	mu        sync.Mutex
	responses []*models.ModelResponse
	received  [][]models.Message
}

func (m *scriptedModel) Response(_ context.Context, messages []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.received = append(m.received, cloneMessages(messages))
	if len(m.responses) == 0 {
		return &models.ModelResponse{Content: "done", FinishReason: models.FinishReasonStop}, nil
	}
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp, nil
}
func (m *scriptedModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not streamed")
}
func (*scriptedModel) ID() string       { return "scripted" }
func (*scriptedModel) Provider() string { return "test" }

func newToolCall(id, name, args string) models.ToolCall {
	return models.ToolCall{ID: id, Type: "function", Function: models.FunctionCall{Name: name, Arguments: args}}
}

func TestRunFailsOnTruncatedOutputButKeepsPartialContent(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "partial answ", FinishReason: models.FinishReasonLength}
	ag, err := New(AgentConfig{Model: m})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.Run(context.Background(), "q")
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v, want ErrOutputTruncated", err)
	}
	if out.Status != session.RunStatusError || out.Content != "partial answ" {
		t.Fatalf("out = %+v", out)
	}
}

func TestRunNeverExecutesToolsFromTruncatedTurn(t *testing.T) {
	var calls atomic.Int32
	m := &scriptedModel{responses: []*models.ModelResponse{{
		ToolCalls:    []models.ToolCall{newToolCall("c1", "mock_tool", `{"q":"tru`)},
		FinishReason: models.FinishReasonLength,
	}}}
	ag, err := New(AgentConfig{Model: m, Tools: []tools.Tool{&counterTool{name: "mock_tool", calls: &calls}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "q"); !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("tool from a truncated turn must not run")
	}
}

func TestStreamRunReportsContentFilter(t *testing.T) {
	m := mock.New()
	m.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: "half"},
		{Event: "finish", FinishReason: models.FinishReasonContentFilter},
	}
	ag, _ := New(AgentConfig{Model: m})
	events, err := ag.RunStream(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	var last Event
	for ev := range events {
		last = ev
	}
	if last.Type != EventTypeRunError || !errors.Is(last.Err, ErrContentFiltered) {
		t.Fatalf("terminal = %+v", last)
	}
}

// DeepSeek thinking mode with tools requires the assistant tool-call turn's
// reasoning_content on the next request; the sync path used to drop it.
func TestSyncToolLoopKeepsReasoningForNextRound(t *testing.T) {
	var calls atomic.Int32
	m := &scriptedModel{responses: []*models.ModelResponse{
		{ReasoningContent: "I should look it up", ToolCalls: []models.ToolCall{newToolCall("c1", "mock_tool", `{}`)}, FinishReason: models.FinishReasonToolCalls},
		{Content: "42", ReasoningContent: "got it", FinishReason: models.FinishReasonStop},
	}}
	ag, _ := New(AgentConfig{Model: m, Tools: []tools.Tool{&counterTool{name: "mock_tool", calls: &calls, result: "r"}}})
	out, err := ag.Run(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	second := m.received[1]
	var replayed bool
	for _, msg := range second {
		if msg.Role == models.RoleAssistant && len(msg.ToolCalls) > 0 && msg.ReasoningContent == "I should look it up" {
			replayed = true
		}
	}
	if !replayed {
		t.Fatalf("assistant tool-call reasoning not sent back: %+v", second)
	}
	if final := out.Messages[len(out.Messages)-1]; final.ReasoningContent != "got it" {
		t.Fatalf("final reasoning not persisted: %+v", final)
	}
}

func TestStreamRetriesFailureBeforeAnyOutput(t *testing.T) {
	m := mock.New()
	m.StreamEventsPerCall = [][]models.StreamEvent{
		{{Event: "error", Err: models.ErrStreamInterrupted}},
		{{Event: "content_delta", Content: "ok"}, {Event: "finish", FinishReason: models.FinishReasonStop}},
	}
	ag, _ := New(AgentConfig{Model: m, RetryConfig: RetryConfig{MaxRetries: 2, RetryDelay: time.Millisecond}})
	events, _ := ag.RunStream(context.Background(), "q")
	var last Event
	for ev := range events {
		last = ev
	}
	if last.Type != EventTypeRunCompleted || last.RunOutput.Content != "ok" {
		t.Fatalf("terminal = %+v", last)
	}
}

func TestStreamDoesNotRetryAfterOutputWasForwarded(t *testing.T) {
	m := mock.New()
	m.StreamEventsPerCall = [][]models.StreamEvent{
		{{Event: "content_delta", Content: "par"}, {Event: "error", Err: models.ErrStreamInterrupted}},
		{{Event: "content_delta", Content: "never"}},
	}
	ag, _ := New(AgentConfig{Model: m, RetryConfig: RetryConfig{MaxRetries: 2, RetryDelay: time.Millisecond}})
	events, _ := ag.RunStream(context.Background(), "q")
	var last Event
	for ev := range events {
		last = ev
	}
	if last.Type != EventTypeRunError || !errors.Is(last.Err, models.ErrStreamInterrupted) {
		t.Fatalf("terminal = %+v", last)
	}
}

func TestNewRejectsDuplicateToolNames(t *testing.T) {
	var calls atomic.Int32
	_, err := New(AgentConfig{Model: mock.New(), Tools: []tools.Tool{
		&counterTool{name: "dup", calls: &calls}, &counterTool{name: "dup", calls: &calls},
	}})
	if !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("err = %v, want ErrDuplicateTool", err)
	}
}

// barrierTool blocks until n calls are in flight, proving concurrency.
type barrierTool struct {
	name    string
	arrived *sync.WaitGroup
}

func (b *barrierTool) Name() string        { return b.name }
func (b *barrierTool) Description() string { return "d" }
func (b *barrierTool) Schema() map[string]any {
	return map[string]any{"function": map[string]any{"name": b.name, "parameters": map[string]any{"type": "object"}}}
}
func (b *barrierTool) Call(ctx context.Context, _ map[string]any) (any, error) {
	b.arrived.Done()
	done := make(chan struct{})
	go func() { b.arrived.Wait(); close(done) }()
	select {
	case <-done:
		return b.name + "-result", nil
	case <-ctx.Done():
		return nil, errors.New("tools did not run concurrently")
	}
}

func TestReadOnlyToolsRunConcurrentlyAndKeepOrder(t *testing.T) {
	var arrived sync.WaitGroup
	arrived.Add(3)
	var ts []tools.Tool
	var calls []models.ToolCall
	for _, name := range []string{"a", "b", "c"} {
		ts = append(ts, tools.WithReadOnly(&barrierTool{name: name, arrived: &arrived}))
		calls = append(calls, newToolCall("id-"+name, name, `{}`))
	}
	m := &scriptedModel{responses: []*models.ModelResponse{{ToolCalls: calls}, {Content: "done"}}}
	ag, err := New(AgentConfig{Model: m, Tools: ts, MaxParallelTools: 3, ToolTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.Run(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, msg := range out.Messages {
		if msg.Role == models.RoleTool {
			results = append(results, msg.ToolCallID+"="+msg.Content)
		}
	}
	want := "id-a=a-result,id-b=b-result,id-c=c-result"
	if strings.Join(results, ",") != want {
		t.Fatalf("results = %v, want %s", results, want)
	}
	if len(out.ToolExecutions) != 3 || out.Metrics.ToolCalls != 3 {
		t.Fatalf("executions = %+v", out.ToolExecutions)
	}
}

type bigTool struct{}

func (bigTool) Name() string        { return "big" }
func (bigTool) Description() string { return "d" }
func (bigTool) Schema() map[string]any {
	return map[string]any{"function": map[string]any{"name": "big", "parameters": map[string]any{"type": "object"}}}
}
func (bigTool) Call(context.Context, map[string]any) (any, error) {
	return strings.Repeat("界", 1000), nil
}

func TestToolResultIsBoundedAndErrorsAreClassified(t *testing.T) {
	m := &scriptedModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{newToolCall("c1", "big", `{}`), newToolCall("c2", "missing", `{}`), newToolCall("c3", "big", `{bad`)}},
		{Content: "done"},
	}}
	var formatted []session.ToolExecutionStatus
	ag, _ := New(AgentConfig{
		Model: m, Tools: []tools.Tool{bigTool{}}, MaxToolResultBytes: 100,
		ToolErrorFormatter: func(call models.ToolCall, status session.ToolExecutionStatus, err error) string {
			formatted = append(formatted, status)
			return "error: redacted"
		},
	})
	out, err := ag.Run(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	tool := out.Messages[2]
	if !strings.Contains(tool.Content, "[truncated") || !strings.HasPrefix(tool.Content, strings.Repeat("界", 33)) {
		t.Fatalf("result not truncated on a rune boundary: %q", tool.Content)
	}
	if out.Messages[3].Content != "error: redacted" {
		t.Fatalf("formatter not applied: %q", out.Messages[3].Content)
	}
	got := []session.ToolExecutionStatus{out.ToolExecutions[0].Status, out.ToolExecutions[1].Status, out.ToolExecutions[2].Status}
	want := []session.ToolExecutionStatus{session.ToolExecutionStatusSuccess, session.ToolExecutionStatusNotFound, session.ToolExecutionStatusInvalidArguments}
	if fmt.Sprint(got) != fmt.Sprint(want) || len(formatted) != 2 {
		t.Fatalf("statuses = %v, formatted = %v", got, formatted)
	}
}

func TestStreamEmitsLiveToolLifecycle(t *testing.T) {
	var calls atomic.Int32
	m := mock.New()
	m.StreamEventsPerCall = [][]models.StreamEvent{
		{{Event: "tool_call_delta", ToolCall: &models.ToolCall{Index: 0, ID: "c1", Type: "function", Function: models.FunctionCall{Name: "mock_tool", Arguments: "{}"}}}},
		{{Event: "content_delta", Content: "done"}},
	}
	ag, _ := New(AgentConfig{Model: m, Tools: []tools.Tool{&counterTool{name: "mock_tool", calls: &calls, result: "r"}}})
	events, _ := ag.RunStream(context.Background(), "q")
	var types []EventType
	for ev := range events {
		types = append(types, ev.Type)
	}
	joined := fmt.Sprint(types)
	if !strings.Contains(joined, "tool_call tool_started tool_result message run_completed") {
		t.Fatalf("events = %v", types)
	}
}

func TestDeprecatedSettersDoNotRaceWithRuns(t *testing.T) {
	ag, _ := New(AgentConfig{Model: mock.New()})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			ag.SetUserID(fmt.Sprintf("u%d", i))
			ag.SetSessionID("")
		}(i)
		go func() {
			defer wg.Done()
			_, _ = ag.Run(context.Background(), "q")
		}()
	}
	wg.Wait()
	ag.SetUserID("final")
	out, err := ag.Run(context.Background(), "q")
	if err != nil || out.UserID != "final" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

type countingRunMiddleware struct{ calls atomic.Int32 }

func (c *countingRunMiddleware) Run(next RunFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	c.calls.Add(1)
	return next(ctx, messages, opts)
}

func TestResumeRunsModelCallsThroughRunMiddlewares(t *testing.T) {
	var calls atomic.Int32
	mw := &countingRunMiddleware{}
	m := &scriptedModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{newToolCall("c1", "guarded", `{}`)}},
		{Content: "after approval"},
	}}
	ag, err := New(AgentConfig{
		Model: m, DB: in_memory.New(), UserID: "u1", RunMiddlewares: []RunMiddleware{mw},
		Tools: []tools.Tool{tools.WithApproval(&counterTool{name: "guarded", calls: &calls, result: "ok"}, tools.ApprovalRequired)},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := ag.Run(context.Background(), "q")
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("paused = %+v err=%v", paused, err)
	}
	out, err := ag.Resume(context.Background(), session.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove),
	})
	if err != nil || out.Content != "after approval" {
		t.Fatalf("out = %+v err=%v", out, err)
	}
	if mw.calls.Load() != 2 {
		t.Fatalf("middleware saw %d model calls, want 2 (resume must not bypass it)", mw.calls.Load())
	}
}

type denyTool struct{ denied string }

func (d denyTool) AuthorizeDiscovery(context.Context, execution.Principal, catalog.ToolDescriptor) (service.AuthorizationDecision, error) {
	return service.AuthorizationDecision{Allowed: true, AuthorizationPolicyRevision: "pol-1"}, nil
}
func (d denyTool) Authorize(_ context.Context, req service.AuthorizeRequest) (service.AuthorizationDecision, error) {
	return service.AuthorizationDecision{Allowed: req.Descriptor.Name != d.denied, AuthorizationPolicyRevision: "pol-1"}, nil
}

func TestManagedPolicyDenialIsReturnedToModel(t *testing.T) {
	var calls atomic.Int32
	b := catalog.NewBuilder()
	for _, name := range []string{"allowed", "secret"} {
		desc := catalog.ToolDescriptor{
			Name: name, Description: "d", InputSchema: map[string]any{"type": "object"},
			ReadOnly: catalog.TriStateTrue, Source: catalog.Source{Kind: "local", Name: "test"},
		}
		if err := b.Add(desc, &counterTool{name: name, calls: &calls, result: name + "-ok"}); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(reg, service.NewMemoryPlanStore(), denyTool{denied: "secret"})
	m := &scriptedModel{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{newToolCall("c1", "secret", `{}`), newToolCall("c2", "allowed", `{}`)}},
		{Content: "used the allowed tool"},
	}}
	ag, err := New(AgentConfig{Model: m, ToolService: svc})
	if err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithPrincipal(context.Background(), execution.Principal{Subject: "alice", Namespace: "ns"})
	out, err := ag.Run(ctx, "q")
	if err != nil {
		t.Fatalf("a policy denial must not fail the run: %v", err)
	}
	if !strings.Contains(out.Messages[2].Content, "forbidden") || out.Messages[3].Content != "allowed-ok" {
		t.Fatalf("messages = %+v", out.Messages)
	}
	if calls.Load() != 1 || out.Content != "used the allowed tool" {
		t.Fatalf("calls = %d, out = %+v", calls.Load(), out)
	}
}

// A run that died mid tool loop, or one paused for approval, must not leave
// dangling tool calls in later requests: providers reject them with 400.
func TestHistoryDropsInFlightRunsAndRepairsBrokenToolLoops(t *testing.T) {
	sess := &session.AgentSession{Runs: []*session.RunOutput{
		{RunID: "ok", Status: session.RunStatusCompleted, Messages: []models.Message{
			{Role: models.RoleUser, Content: "q1"}, {Role: models.RoleAssistant, Content: "a1"},
		}},
		{RunID: "crashed", Status: session.RunStatusError, Messages: []models.Message{
			{Role: models.RoleUser, Content: "q2"},
			{Role: models.RoleAssistant, ToolCalls: []models.ToolCall{newToolCall("c1", "t", "{}"), newToolCall("c2", "t", "{}")}},
			{Role: models.RoleTool, ToolCallID: "c1", Content: "r1"},
			{Role: models.RoleTool, ToolCallID: "stray", Content: "x"},
		}},
		{RunID: "paused", Status: session.RunStatusPaused, Messages: []models.Message{
			{Role: models.RoleUser, Content: "q3"},
			{Role: models.RoleAssistant, ToolCalls: []models.ToolCall{newToolCall("c3", "t", "{}")}},
		}},
	}}
	history := sessionHistory(sess, "", 0)
	if len(history) != 2 {
		t.Fatalf("history runs = %d, want 2 (paused run excluded)", len(history))
	}
	crashed := history[1].Messages
	if len(crashed) != 3 || len(crashed[1].ToolCalls) != 1 || crashed[1].ToolCalls[0].ID != "c1" || crashed[2].ToolCallID != "c1" {
		t.Fatalf("repaired = %+v", crashed)
	}
	if len(sess.Runs[1].Messages[1].ToolCalls) != 2 {
		t.Fatal("repair must not mutate the persisted session")
	}
}
