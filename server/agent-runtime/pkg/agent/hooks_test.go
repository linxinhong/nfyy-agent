package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

type rejectEmptyInput struct{}

func (rejectEmptyInput) PreRun(ctx context.Context, input *session.RunInput) error {
	if strings.TrimSpace(input.Message) == "" {
		return errors.New("input is empty")
	}
	return nil
}

type appendTag struct{}

func (appendTag) PostRun(ctx context.Context, output *session.RunOutput) error {
	output.Content += " [verified]"
	return nil
}

func TestAgent_PreRunHookRejectsEmptyInput(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "ok"}
	ag, err := New(AgentConfig{
		Name:  "hook-test",
		Model: m,
		Hooks: &Hooks{PreRun: rejectEmptyInput{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "   ")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if out.Status != session.RunStatusError {
		t.Fatalf("expected error status, got %q", out.Status)
	}
}

func TestAgent_PreRunFailureIsPersisted(t *testing.T) {
	store := in_memory.New()
	m := mock.New()
	ag, err := New(AgentConfig{
		Name: "hook-persistence-test", Model: m, DB: store,
		Hooks: &Hooks{PreRun: rejectEmptyInput{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), " ")
	if err == nil {
		t.Fatal("expected pre-run error")
	}
	sess, readErr := store.ReadSession(context.Background(), out.SessionID)
	if readErr != nil {
		t.Fatalf("ReadSession: %v", readErr)
	}
	if sess == nil || len(sess.Runs) != 1 || sess.Runs[0].Status != session.RunStatusError {
		t.Fatalf("expected persisted failed run, got %+v", sess)
	}
}

func TestAgent_PostRunHookModifiesOutput(t *testing.T) {
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "hello"}
	ag, err := New(AgentConfig{
		Name:  "hook-test",
		Model: m,
		Hooks: &Hooks{PostRun: appendTag{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "hello [verified]" {
		t.Fatalf("content = %q, want hello [verified]", out.Content)
	}
}

func TestKeywordGuardrail(t *testing.T) {
	g := &KeywordGuardrail{Forbidden: []string{"secret"}}
	if err := g.ValidateInput(context.Background(), &session.RunInput{Message: "tell me a secret"}); err == nil {
		t.Fatal("expected forbidden keyword error")
	}
	if err := g.ValidateInput(context.Background(), &session.RunInput{Message: "hello"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
