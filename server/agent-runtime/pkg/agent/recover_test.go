package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// crashedRun leaves the state a process dying mid tool batch leaves: the
// write-ahead checkpoint holds the assistant's tool calls but no results,
// and the reaper has failed the run.
func crashedRun(t *testing.T, store *in_memory.DB, agentID string) (*session.RunOutput, string) {
	t.Helper()
	ctx := context.Background()
	sess := &session.AgentSession{SessionID: "s-crash", AgentID: agentID, UserID: "u"}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	run := &session.RunOutput{
		RunID: "crashed", AgentID: agentID, SessionID: sess.SessionID, UserID: "u",
		Input:  session.RunInput{Message: "pay the invoice and refresh the dashboard"},
		Status: session.RunStatusRunning, LeaseTimeout: time.Minute, CreatedAt: old, UpdatedAt: old,
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "pay the invoice and refresh the dashboard"},
			{Role: models.RoleAssistant, ToolCalls: []models.ToolCall{
				newToolCall("c-pay", "pay_invoice", `{"id":"inv-1"}`),
				newToolCall("c-refresh", "refresh_dashboard", `{}`),
			}},
		},
	}
	if err := store.AppendRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if ids, err := runmanager.ExpireStaleRuns(ctx, store, 10); err != nil || len(ids) != 1 {
		t.Fatalf("reap = %v, %v", ids, err)
	}
	return run, sess.SessionID
}

func TestRecoverContinuesFromCheckpointWithoutRepeatingSideEffects(t *testing.T) {
	store := in_memory.New()
	_, sessionID := crashedRun(t, store, "payer")
	var payments, refreshes atomic.Int32
	model := &scriptedModel{responses: []*models.ModelResponse{{Content: "The dashboard is refreshed; the payment status is unknown, I will check it."}}}
	ag, err := New(AgentConfig{ID: "payer", Model: model, DB: store, UserID: "u", Tools: []tools.Tool{
		&counterTool{name: "pay_invoice", calls: &payments, result: "paid"},
		tools.WithIdempotent(&counterTool{name: "refresh_dashboard", calls: &refreshes, result: "refreshed"}),
	}})
	if err != nil {
		t.Fatal(err)
	}

	out, err := ag.Recover(context.Background(), RecoverRequest{RunID: "crashed"})
	if err != nil {
		t.Fatal(err)
	}
	if payments.Load() != 0 || refreshes.Load() != 1 {
		t.Fatalf("payments = %d (must not repeat), refreshes = %d (idempotent, replayed)", payments.Load(), refreshes.Load())
	}
	if out.Status != session.RunStatusCompleted || out.Metadata["recovered_from"] != "crashed" || out.SessionID != sessionID {
		t.Fatalf("recovered run = %+v", out)
	}
	sent := model.received[0]
	var payResult, refreshResult string
	for _, m := range sent {
		switch m.ToolCallID {
		case "c-pay":
			payResult = m.Content
		case "c-refresh":
			refreshResult = m.Content
		}
	}
	if !strings.Contains(payResult, "outcome is unknown") || refreshResult != "refreshed" {
		t.Fatalf("tool results sent to the model: pay=%q refresh=%q", payResult, refreshResult)
	}
	orig, _ := store.GetRun(context.Background(), db.RunQuery{RunID: "crashed"})
	if orig.RecoveredBy != out.RunID {
		t.Fatalf("original RecoveredBy = %q", orig.RecoveredBy)
	}

	if _, err := ag.Recover(context.Background(), RecoverRequest{RunID: "crashed"}); !errors.Is(err, ErrRecoverConflict) {
		t.Fatalf("second recover = %v", err)
	}
	// Later turns see the transcript once, through the recovered run.
	if _, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: "next"}, SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	userTurns := 0
	for _, m := range model.received[len(model.received)-1] {
		if m.Role == models.RoleUser && strings.HasPrefix(m.Content, "pay the invoice") {
			userTurns++
		}
	}
	if userTurns != 1 {
		t.Fatalf("crashed run's transcript appears %d times in history", userTurns)
	}
}

func TestRecoverRejectsRunsThatDidNotCrash(t *testing.T) {
	store := in_memory.New()
	ag, err := New(AgentConfig{ID: "a", Model: &scriptedModel{}, DB: store, UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Recover(context.Background(), RecoverRequest{RunID: out.RunID}); !errors.Is(err, ErrRecoverNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	if _, err := ag.Recover(context.Background(), RecoverRequest{RunID: "missing"}); !errors.Is(err, ErrRecoverNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestRecoverCrashBeforeAnyToolCall(t *testing.T) {
	store := in_memory.New()
	ctx := context.Background()
	_ = store.CreateSession(ctx, &session.AgentSession{SessionID: "s", AgentID: "a", UserID: "u"})
	old := time.Now().Add(-time.Hour)
	_ = store.AppendRun(ctx, &session.RunOutput{RunID: "r", AgentID: "a", SessionID: "s", UserID: "u",
		Input: session.RunInput{Message: "hello"}, Status: session.RunStatusRunning, LeaseTimeout: time.Minute, CreatedAt: old, UpdatedAt: old})
	if _, err := runmanager.ExpireStaleRuns(ctx, store, 10); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{}
	ag, _ := New(AgentConfig{ID: "a", Model: model, DB: store, UserID: "u"})
	out, err := ag.Recover(ctx, RecoverRequest{RunID: "r"})
	if err != nil || out.Content != "done" {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if last := model.received[0][len(model.received[0])-1]; last.Role != models.RoleUser || last.Content != "hello" {
		t.Fatalf("recovered context ends with %+v", last)
	}
}
