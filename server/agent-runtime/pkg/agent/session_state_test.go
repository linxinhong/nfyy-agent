package agent

import (
	"context"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
)

func TestAgent_SessionState(t *testing.T) {
	store := in_memory.New()
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "ok"}

	ag, err := New(AgentConfig{
		Name:         "state-test",
		Model:        m,
		DB:           store,
		SessionState: map[string]any{"theme": "dark"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := ag.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	state, err := ag.GetSessionState(context.Background(), out.SessionID)
	if err != nil {
		t.Fatalf("GetSessionState: %v", err)
	}
	if state["theme"] != "dark" {
		t.Fatalf("expected theme=dark, got %+v", state)
	}

	if err := ag.UpdateSessionState(context.Background(), out.SessionID, map[string]any{"counter": 42}); err != nil {
		t.Fatalf("UpdateSessionState: %v", err)
	}

	state, err = ag.GetSessionState(context.Background(), out.SessionID)
	if err != nil {
		t.Fatalf("GetSessionState after update: %v", err)
	}
	counter, _ := state["counter"].(float64)
	if state["theme"] != "dark" || counter != 42 {
		t.Fatalf("expected theme and counter, got %+v", state)
	}
}
