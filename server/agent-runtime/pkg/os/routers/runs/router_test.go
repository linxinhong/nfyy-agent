package runs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func createSessionWithRun(t *testing.T, store *in_memory.DB, runID string) {
	t.Helper()
	now := time.Now().UTC()
	sess := &session.AgentSession{
		SessionID: "sess-1",
		AgentID:   "a1",
		CreatedAt: now,
		UpdatedAt: now,
		Runs: []*session.RunOutput{
			{
				RunID:     runID,
				AgentID:   "a1",
				SessionID: "sess-1",
				Content:   "hello",
				Status:    session.RunStatusCompleted,
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
	if err := store.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
}

func TestGetRun(t *testing.T) {
	store := in_memory.New()
	createSessionWithRun(t, store, "run-1")

	router := NewRouter(store)
	req := httptest.NewRequest(http.MethodGet, "/run-1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var output session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if output.RunID != "run-1" {
		t.Fatalf("run_id = %q, want run-1", output.RunID)
	}
	if output.Content != "hello" {
		t.Fatalf("content = %q, want hello", output.Content)
	}
}

func TestGetRunNotFound(t *testing.T) {
	store := in_memory.New()
	router := NewRouter(store)
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
