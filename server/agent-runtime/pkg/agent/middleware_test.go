package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// ---------- RunMiddleware chain tests ----------

func TestRunMiddleware_ChainOrder(t *testing.T) {
	order := []string{}
	mwA := RunMiddlewareFunc(func(next RunFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
		order = append(order, "A-before")
		resp, err := next(ctx, messages, opts)
		order = append(order, "A-after")
		return resp, err
	})
	mwB := RunMiddlewareFunc(func(next RunFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
		order = append(order, "B-before")
		resp, err := next(ctx, messages, opts)
		order = append(order, "B-after")
		return resp, err
	})
	base := RunFunc(func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
		order = append(order, "base")
		return &models.ModelResponse{Content: "ok"}, nil
	})

	chained := buildRunChain([]RunMiddleware{mwA, mwB}, base)
	_, err := chained(context.Background(), []models.Message{{Content: "hi"}}, models.ModelOptions{})
	if err != nil {
		t.Fatalf("chained: %v", err)
	}

	want := []string{"A-before", "B-before", "base", "B-after", "A-after"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}

func TestRunMiddleware_ShortCircuit(t *testing.T) {
	called := false
	shortCircuit := RunMiddlewareFunc(func(next RunFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
		return &models.ModelResponse{Content: "short-circuited"}, nil
	})
	base := RunFunc(func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
		called = true
		return &models.ModelResponse{Content: "base"}, nil
	})

	chained := buildRunChain([]RunMiddleware{shortCircuit}, base)
	resp, err := chained(context.Background(), nil, models.ModelOptions{})
	if err != nil {
		t.Fatalf("chained: %v", err)
	}
	if resp.Content != "short-circuited" {
		t.Fatalf("content = %q, want short-circuited", resp.Content)
	}
	if called {
		t.Fatal("base was called despite short-circuit")
	}
}

func TestRunMiddleware_EmptyChainReturnsBase(t *testing.T) {
	called := false
	base := RunFunc(func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
		called = true
		return &models.ModelResponse{}, nil
	})
	chained := buildRunChain(nil, base)
	_, err := chained(context.Background(), nil, models.ModelOptions{})
	if err != nil {
		t.Fatalf("chained: %v", err)
	}
	if !called {
		t.Fatal("base was not called")
	}
}

func TestRunMiddleware_AgentIntegration(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "hello"}

	var runCalled bool
	runMW := RunMiddlewareFunc(func(next RunFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
		runCalled = true
		return next(ctx, messages, opts)
	})

	ag, err := New(AgentConfig{
		Name:           "run-mw-test",
		Model:          m,
		RunMiddlewares: []RunMiddleware{runMW},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !runCalled {
		t.Fatal("run middleware was not called")
	}
	if out.Content != "hello" {
		t.Fatalf("content = %q, want hello", out.Content)
	}
}

// ---------- StreamMiddleware tests ----------

func TestStreamMiddleware_ChainOrder(t *testing.T) {
	order := []string{}
	mwA := StreamMiddlewareFunc(func(next StreamFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
		order = append(order, "A")
		return next(ctx, messages, opts)
	})
	mwB := StreamMiddlewareFunc(func(next StreamFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
		order = append(order, "B")
		return next(ctx, messages, opts)
	})
	base := StreamFunc(func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
		order = append(order, "base")
		ch := make(chan models.StreamEvent)
		close(ch)
		return ch, nil
	})

	chained := buildStreamChain([]StreamMiddleware{mwA, mwB}, base)
	stream, err := chained(context.Background(), nil, models.ModelOptions{})
	if err != nil {
		t.Fatalf("buildStreamChain: %v", err)
	}
	for range stream {
	}

	want := []string{"A", "B", "base"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}

func TestStreamMiddleware_EmptyChain(t *testing.T) {
	called := false
	base := StreamFunc(func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
		called = true
		ch := make(chan models.StreamEvent)
		close(ch)
		return ch, nil
	})
	chained := buildStreamChain(nil, base)
	stream, err := chained(context.Background(), nil, models.ModelOptions{})
	if err != nil {
		t.Fatalf("buildStreamChain: %v", err)
	}
	for range stream {
	}
	if !called {
		t.Fatal("base was not called")
	}
}

// ---------- ToolMiddleware tests ----------

type auditToolMW struct {
	mu      sync.Mutex
	records []string
}

func (m *auditToolMW) PreTool(ctx context.Context, call *models.ToolCall) (*models.ToolCall, error) {
	m.mu.Lock()
	m.records = append(m.records, "pre:"+call.Function.Name)
	m.mu.Unlock()
	return call, nil
}

func (m *auditToolMW) PostTool(ctx context.Context, call *models.ToolCall, result any, err error) error {
	m.mu.Lock()
	m.records = append(m.records, "post:"+call.Function.Name)
	m.mu.Unlock()
	return nil
}

type renameToolMW struct {
	from, to string
}

func (m *renameToolMW) PreTool(ctx context.Context, call *models.ToolCall) (*models.ToolCall, error) {
	if call.Function.Name == m.from {
		call.Function.Name = m.to
	}
	return call, nil
}

func (m *renameToolMW) PostTool(ctx context.Context, call *models.ToolCall, result any, err error) error {
	return nil
}

type blockToolMW struct{}

func (*blockToolMW) PreTool(ctx context.Context, call *models.ToolCall) (*models.ToolCall, error) {
	return nil, errors.New("blocked")
}

func (*blockToolMW) PostTool(ctx context.Context, call *models.ToolCall, result any, err error) error {
	return nil
}

func echoTool(_ context.Context, msg string) string {
	return msg
}

func TestToolMiddleware_ForwardReverse(t *testing.T) {
	mw1 := &auditToolMW{}
	mw2 := &auditToolMW{}

	tc := &models.ToolCall{Function: models.FunctionCall{Name: "echo"}}
	execute := func(ctx context.Context, call *models.ToolCall) (any, error) {
		return "result", nil
	}

	_, err := runToolChain([]ToolMiddleware{mw1, mw2}, context.Background(), tc, execute)
	if err != nil {
		t.Fatalf("runToolChain: %v", err)
	}

	if len(mw1.records) != 2 || mw1.records[0] != "pre:echo" || mw1.records[1] != "post:echo" {
		t.Fatalf("mw1 = %v, want [pre:echo post:echo]", mw1.records)
	}
	if len(mw2.records) != 2 || mw2.records[0] != "pre:echo" || mw2.records[1] != "post:echo" {
		t.Fatalf("mw2 = %v, want [pre:echo post:echo]", mw2.records)
	}
}

func TestToolMiddleware_PreToolModifiesCall(t *testing.T) {
	renamer := &renameToolMW{from: "add", to: "echo"}

	tc := &models.ToolCall{Function: models.FunctionCall{Name: "add"}}
	var executed string
	execute := func(ctx context.Context, call *models.ToolCall) (any, error) {
		executed = call.Function.Name
		return nil, nil
	}

	_, err := runToolChain([]ToolMiddleware{renamer}, context.Background(), tc, execute)
	if err != nil {
		t.Fatalf("runToolChain: %v", err)
	}
	if executed != "echo" {
		t.Fatalf("executed = %q, want echo", executed)
	}
}

func TestToolMiddleware_PreToolRejects(t *testing.T) {
	var executed bool
	blocker := &blockToolMW{}
	tc := &models.ToolCall{Function: models.FunctionCall{Name: "test"}}
	execute := func(ctx context.Context, call *models.ToolCall) (any, error) {
		executed = true
		return nil, nil
	}

	_, err := runToolChain([]ToolMiddleware{blocker}, context.Background(), tc, execute)
	if err == nil {
		t.Fatal("expected error from PreTool rejection")
	}
	if executed {
		t.Fatal("tool was executed despite PreTool rejection")
	}
}

func TestToolMiddleware_AgentIntegration(t *testing.T) {
	m := mock.New()
	m.ToolCallRounds = 1
	m.ResponseData = &models.ModelResponse{Content: "Done"}

	echo, err := tools.NewFunction("echo", "echoes input", echoTool)
	if err != nil {
		t.Fatalf("NewFunction: %v", err)
	}

	audit := &auditToolMW{}

	ag, err := New(AgentConfig{
		Name:            "tool-mw-test",
		Model:           m,
		Tools:           []tools.Tool{echo},
		ToolMiddlewares: []ToolMiddleware{audit},
		DB:              in_memory.New(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = ag.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(audit.records) < 2 {
		t.Fatalf("audit.records = %v, expected at least pre+post", audit.records)
	}
}

// ---------- Legacy Hooks adapter tests ----------

type rejectInputHook struct{}

func (rejectInputHook) PreRun(ctx context.Context, input *session.RunInput) error {
	if strings.TrimSpace(input.Message) == "" {
		return errors.New("empty input")
	}
	return nil
}

type appendOutputHook struct{}

func (appendOutputHook) PostRun(ctx context.Context, output *session.RunOutput) error {
	output.Content += " [hooked]"
	return nil
}

func TestLegacyHooks_AgentIntegration(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "hello"}

	ag, err := New(AgentConfig{
		Name:  "legacy-hook-test",
		Model: m,
		Hooks: &Hooks{PreRun: rejectInputHook{}, PostRun: appendOutputHook{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "  ")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if out.Status != session.RunStatusError {
		t.Fatalf("status = %q, want error", out.Status)
	}

	out2, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out2.Content != "hello [hooked]" {
		t.Fatalf("content = %q, want 'hello [hooked]'", out2.Content)
	}
}

// ---------- MergeToolMiddlewares tests ----------

func TestMergeToolMiddlewares_NilHooks(t *testing.T) {
	mws := []ToolMiddleware{&auditToolMW{}}
	result := MergeToolMiddlewares(nil, mws)
	if len(result) != 1 {
		t.Fatalf("len = %d, want 1", len(result))
	}
}

func TestMergeToolMiddlewares_NilBoth(t *testing.T) {
	result := MergeToolMiddlewares(nil, nil)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestMergeToolMiddlewares_WithHooks(t *testing.T) {
	audit := &auditToolMW{}
	result := MergeToolMiddlewares(&Hooks{}, []ToolMiddleware{audit})
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2 (hooks adapter + explicit)", len(result))
	}
}

// ---------- Compile-time interface checks ----------

var _ RunMiddleware = RunMiddlewareFunc(nil)
var _ StreamMiddleware = StreamMiddlewareFunc(nil)
var _ ToolMiddleware = &auditToolMW{}
