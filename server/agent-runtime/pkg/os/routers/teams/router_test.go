package teams

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
)

func newTestAgent(t *testing.T, name string) *agent.Agent {
	t.Helper()
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "hello from " + name}
	m.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: "hello from " + name},
	}
	ag, err := agent.New(agent.AgentConfig{
		ID:           name,
		Name:         "Test " + name,
		Instructions: "You are a test agent.",
		Model:        m,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return ag
}

func newTestTeam(t *testing.T, name string, store db.DB) *team.Team {
	t.Helper()
	m := mock.New()
	content := "hello from " + name
	m.ResponseData = &models.ModelResponse{Content: content}
	m.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: content},
	}
	member := newTestAgent(t, name+"-member")
	tm, err := team.New(team.TeamConfig{
		ID:           name,
		Name:         "Test " + name,
		Instructions: "You are a test team.",
		Model:        m,
		Members:      []*agent.Agent{member},
		DB:           store,
	})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	return tm
}

func TestListTeams(t *testing.T) {
	teams := map[string]*team.Team{
		"t1": newTestTeam(t, "t1", nil),
	}
	router := NewRouter(teams, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp ListTeamsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Teams) != 1 || resp.Teams[0].ID != "t1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestGetTeam(t *testing.T) {
	teams := map[string]*team.Team{
		"t1": newTestTeam(t, "t1", nil),
	}
	router := NewRouter(teams, nil)
	req := httptest.NewRequest(http.MethodGet, "/t1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp TeamInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != "t1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestRunTeamInBackground(t *testing.T) {
	store := in_memory.New()
	manager := runmanager.New(store, time.Second)
	router := NewRouter(map[string]*team.Team{"t1": newTestTeam(t, "t1", store)}, store, manager)
	req := httptest.NewRequest(http.MethodPost, "/t1/runs?background=true", strings.NewReader("hello"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var pending session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil || pending.RunID == "" {
		t.Fatalf("pending response = %+v, %v", pending, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		output, err := manager.Get(context.Background(), db.RunQuery{RunID: pending.RunID})
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if output != nil && output.Status == session.RunStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("team run did not complete: %+v", output)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGetTeamNotFound(t *testing.T) {
	router := NewRouter(map[string]*team.Team{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestRunTeamJSON(t *testing.T) {
	teams := map[string]*team.Team{
		"t1": newTestTeam(t, "t1", nil),
	}
	router := NewRouter(teams, nil)
	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/t1/runs", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var output session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if output.Content != "hello from t1" {
		t.Fatalf("unexpected content: %q", output.Content)
	}
}

func TestRunTeamSSE(t *testing.T) {
	teams := map[string]*team.Team{
		"t1": newTestTeam(t, "t1", nil),
	}
	router := NewRouter(teams, nil)
	req := httptest.NewRequest(http.MethodPost, "/t1/runs?stream=true", strings.NewReader("hi"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	var events []string
	scanner := bufio.NewScanner(rec.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}

	want := []string{"run_started", "message", "run_completed"}
	if len(events) < len(want) {
		t.Fatalf("expected at least %d events, got %d: %v", len(want), len(events), events)
	}
	for i, e := range want {
		if events[i] != e {
			t.Fatalf("event %d: want %q, got %q", i, e, events[i])
		}
	}
}

func TestRunTeamWithUserSession(t *testing.T) {
	store := in_memory.New()
	teams := map[string]*team.Team{
		"t1": newTestTeam(t, "t1", store),
	}
	router := NewRouter(teams, store)

	// Pre-create a team session so the request session_id isolates runs.
	if err := store.CreateTeamSession(context.Background(), &session.TeamSession{
		SessionID: "shared-sess",
		TeamID:    "t1",
		UserID:    "u1",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create team session: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"message":    "hi",
		"user_id":    "u1",
		"session_id": "shared-sess",
	})
	req := httptest.NewRequest(http.MethodPost, "/t1/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var output session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if output.UserID != "u1" {
		t.Fatalf("user_id = %q, want u1", output.UserID)
	}
	if output.SessionID != "shared-sess" {
		t.Fatalf("session_id = %q, want shared-sess", output.SessionID)
	}
}

func TestListTeamRuns(t *testing.T) {
	store := in_memory.New()
	teams := map[string]*team.Team{
		"t1": newTestTeam(t, "t1", store),
	}
	router := NewRouter(teams, store)

	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/t1/runs", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/t1/runs", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs []*session.RunOutput `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(resp.Runs))
	}
}
