package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
)

func TestRouter_SessionCRUD(t *testing.T) {
	store := in_memory.New()
	router := NewRouter(store)

	// Create
	body, _ := json.Marshal(CreateSessionRequest{
		AgentID:      "test-agent",
		UserID:       "u1",
		SessionState: map[string]any{"theme": "dark"},
	})
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	var created SessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.AgentID != "test-agent" {
		t.Fatalf("agent_id = %q, want test-agent", created.AgentID)
	}

	// Read
	req = httptest.NewRequest(http.MethodGet, "/"+created.SessionID, nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}

	// List
	req = httptest.NewRequest(http.MethodGet, "/?agent_id=test-agent", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	var listResp struct {
		Sessions []SessionResponse `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(listResp.Sessions))
	}

	// Update
	summary := "updated summary"
	patch, _ := json.Marshal(UpdateSessionRequest{
		Revision: created.Revision,
		Summary:  &summary,
	})
	req = httptest.NewRequest(http.MethodPatch, "/"+created.SessionID, bytes.NewReader(patch))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200", rec.Code)
	}
	var updated SessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated: %v", err)
	}
	if updated.Summary != "updated summary" {
		t.Fatalf("summary = %q, want updated summary", updated.Summary)
	}

	// Stale revision is rejected.
	staleSummary := "stale"
	patch, _ = json.Marshal(UpdateSessionRequest{Revision: created.Revision, Summary: &staleSummary})
	req = httptest.NewRequest(http.MethodPatch, "/"+created.SessionID, bytes.NewReader(patch))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale update status = %d, want 409", rec.Code)
	}

	// Delete
	req = httptest.NewRequest(http.MethodDelete, "/"+created.SessionID, nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", rec.Code)
	}

	// Read after delete
	req = httptest.NewRequest(http.MethodGet, "/"+created.SessionID, nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", rec.Code)
	}

	_ = context.Background
}
