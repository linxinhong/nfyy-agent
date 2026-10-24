package agent

import (
	"context"
	"errors"
	"testing"

	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

func TestAgent_TenantExecutionRequiresNamespaceStore(t *testing.T) {
	ag, err := New(AgentConfig{Name: "tenant-gate", Model: mock.New(), DB: in_memory.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := execution.ContextWithNamespace(context.Background(), "tenant")
	if _, err := ag.RunWith(ctx, session.RunRequest{Input: session.RunInput{Message: "blocked"}}); !errors.Is(err, dbpkg.ErrNamespaceStoreRequired) {
		t.Fatalf("RunWith tenant adapter gate = %v", err)
	}
	if _, err := ag.Resume(ctx, session.ResumeRequest{RunID: "run", RequirementID: "requirement", Action: "approve"}); !errors.Is(err, dbpkg.ErrNamespaceStoreRequired) {
		t.Fatalf("Resume tenant adapter gate = %v", err)
	}
}

func TestAgent_Run_MockModel(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "Hello from mock"}

	store := in_memory.New()
	ag, err := New(AgentConfig{
		Name:  "test",
		Model: m,
		DB:    store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "Hello from mock" {
		t.Errorf("content = %q, want Hello from mock", out.Content)
	}
	if out.Status != session.RunStatusCompleted {
		t.Errorf("status = %q, want completed", out.Status)
	}
	if out.SessionID == "" {
		t.Error("session id not set")
	}

	// Session should be persisted.
	sess, err := store.ReadSession(context.Background(), out.SessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if sess == nil || len(sess.Runs) != 1 {
		t.Fatalf("expected 1 run in session, got %+v", sess)
	}
}

func TestAgent_RunCreatesNewSessionWithoutExplicitSessionID(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "ok"}
	store := in_memory.New()
	ag, err := New(AgentConfig{Name: "history-test", Model: m, DB: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := ag.Run(context.Background(), "first")
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, err := ag.Run(context.Background(), "second")
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.SessionID == first.SessionID {
		t.Fatalf("second session = %q, want a new session", second.SessionID)
	}
}

func TestAgent_RunWithUsesCallerRunIDAndPersistsFinalState(t *testing.T) {
	store := in_memory.New()
	ag, err := New(AgentConfig{Name: "managed-run", Model: mock.New(), DB: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.RunWith(context.Background(), session.RunRequest{RunID: "run-managed", Input: session.RunInput{Message: "hello"}})
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if out.RunID != "run-managed" {
		t.Fatalf("run id = %q, want run-managed", out.RunID)
	}
	stored, err := store.GetRun(context.Background(), dbpkg.RunQuery{RunID: "run-managed"})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored == nil || stored.Status != session.RunStatusCompleted {
		t.Fatalf("stored run = %+v, want completed", stored)
	}
}

func TestAgent_RunWithReusesExplicitSessionAndStoresOnlyCurrentTranscript(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "ok"}
	store := in_memory.New()
	ag, err := New(AgentConfig{Name: "history-test", Model: m, DB: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: "first"}})
	if err != nil {
		t.Fatalf("first RunWith: %v", err)
	}
	second, err := ag.RunWith(context.Background(), session.RunRequest{SessionID: first.SessionID, Input: session.RunInput{Message: "second"}})
	if err != nil {
		t.Fatalf("second RunWith: %v", err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("second session = %q, want %q", second.SessionID, first.SessionID)
	}

	sess, err := store.ReadSession(context.Background(), first.SessionID)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if len(sess.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(sess.Runs))
	}
	for i, run := range sess.Runs {
		if len(run.Messages) != 2 {
			t.Fatalf("run %d messages = %d, want only user and assistant messages", i, len(run.Messages))
		}
	}
	if sess.Runs[1].Messages[0].Content != "second" {
		t.Fatalf("second run user message = %q, want second", sess.Runs[1].Messages[0].Content)
	}
}

func addToolFunc(_ context.Context, a, b int) int {
	return a + b
}

func TestAgent_Run_ToolCall(t *testing.T) {
	m := mock.New()
	m.ToolCallRounds = 1
	m.ResponseData = &models.ModelResponse{Content: "Done"}

	calc, err := tools.NewFunction("add", "Add two numbers.", addToolFunc)
	if err != nil {
		t.Fatalf("NewFunction: %v", err)
	}

	ag, err := New(AgentConfig{
		Name:         "tool-test",
		Model:        m,
		Tools:        []tools.Tool{calc},
		MaxToolCalls: 5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "add 2 and 3")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "Done" {
		t.Errorf("content = %q, want Done", out.Content)
	}
	if len(out.ToolCalls) != 1 {
		t.Errorf("tool calls = %d, want 1", len(out.ToolCalls))
	}
}

func TestAgent_Run_Memory(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "ok"}

	store := in_memory.New()
	mgr := memory.NewManager(store)

	ag, err := New(AgentConfig{
		Name:   "mem-test",
		Model:  m,
		DB:     store,
		Memory: mgr,
		UserID: "u1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = mgr.CreateMemory(context.Background(), "I like Go", "u1", ag.ID())
	if err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}

	out, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Errorf("status = %q, want completed", out.Status)
	}
}

func TestAgent_Run_ReturnsModelError(t *testing.T) {
	m := mock.New()
	m.ResponseErrors = []error{errors.New("model unavailable")}

	ag, err := New(AgentConfig{
		Name:  "error-test",
		Model: m,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "hello")
	if err == nil {
		t.Fatalf("expected error, got nil; status=%s", out.Status)
	}
	if out.Status != session.RunStatusError {
		t.Errorf("status = %q, want error", out.Status)
	}
}

// modelWithoutJSONSchema is a mock model that does not implement
// models.JSONSchemaCapable.
type modelWithoutJSONSchema struct {
	*mock.MockModel
}

func TestAgent_ResponseFormatFallbackToJSONObject(t *testing.T) {
	m := &modelWithoutJSONSchema{MockModel: mock.New()}
	m.ResponseData = &models.ModelResponse{Content: `{"city":"Tokyo","temperature":28,"unit":"C"}`}

	ag, err := New(AgentConfig{
		Name:         "schema-fallback-test",
		Model:        m,
		OutputSchema: weatherResponse{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "weather")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	w, ok := out.Parsed.(weatherResponse)
	if !ok {
		t.Fatalf("parsed type = %T, want weatherResponse", out.Parsed)
	}
	if w.City != "Tokyo" {
		t.Errorf("city = %q, want Tokyo", w.City)
	}
}
