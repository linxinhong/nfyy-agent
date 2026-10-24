package os

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func serveOS(t *testing.T, cfg AgentOSConfig) (*AgentOS, string) {
	t.Helper()
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.ServeListener(listener) }()
	return app, "http://" + listener.Addr().String()
}

func startStream(t *testing.T, url string) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/agents/a/runs?stream=true", strings.NewReader(`{"message":"q"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp.Body)
	readFrames(t, r, func(f sseFrame) bool { return f.event == "message" })
	return resp, r
}

func TestShutdownDrainsInFlightStreams(t *testing.T) {
	m := &pausingStreamModel{release: make(chan struct{}), cancelled: make(chan struct{})}
	ag, _ := agent.New(agent.AgentConfig{ID: "a", Model: m})
	app, url := serveOS(t, AgentOSConfig{Agents: map[string]*agent.Agent{"a": ag}})
	resp, r := startStream(t, url)
	defer resp.Body.Close()

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- app.Shutdown(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "draining") {
		t.Fatalf("health while draining = %d %s", rec.Code, rec.Body.String())
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before the in-flight run finished: %v", err)
	default:
	}
	close(m.release)
	rest := readFrames(t, r, nil)
	if len(rest) == 0 || rest[len(rest)-1].event != "run_completed" {
		t.Fatalf("in-flight stream did not complete: %+v", rest)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestShutdownCancelsRunsAfterDeadline(t *testing.T) {
	m := &pausingStreamModel{release: make(chan struct{}), cancelled: make(chan struct{})}
	ag, _ := agent.New(agent.AgentConfig{ID: "a", Model: m})
	app, url := serveOS(t, AgentOSConfig{Agents: map[string]*agent.Agent{"a": ag}})
	resp, r := startStream(t, url)
	defer resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := app.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown err = %v, want deadline exceeded", err)
	}
	select {
	case <-m.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("run still executing after the shutdown deadline")
	}
	rest := readFrames(t, r, nil)
	if len(rest) == 0 || rest[len(rest)-1].event != "run_error" {
		t.Fatalf("client did not get a terminal error frame: %+v", rest)
	}
}

func TestStartRunsMaintenance(t *testing.T) {
	store := in_memory.New()
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	if err := store.CreateSession(ctx, &session.AgentSession{SessionID: "busy", AgentID: "a", UserID: "u", CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, &session.AgentSession{SessionID: "idle", AgentID: "a", UserID: "u", CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRun(ctx, &session.RunOutput{RunID: "orphan", SessionID: "busy", AgentID: "a", UserID: "u",
		Status: session.RunStatusRunning, LeaseTimeout: time.Minute, CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	app, err := New(AgentOSConfig{DB: store, Maintenance: MaintenanceConfig{SessionRetention: 24 * time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	app.Start(ctx)
	defer func() { _ = app.Shutdown(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, _ := store.GetRun(ctx, db.RunQuery{RunID: "orphan"})
		idle, _ := store.ReadSession(ctx, "idle")
		if run != nil && run.Status == session.RunStatusError && idle == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("maintenance did not reap the orphaned run and delete the idle session")
}

func TestRunTimeoutIsConfigurable(t *testing.T) {
	m := &pausingStreamModel{release: make(chan struct{}), cancelled: make(chan struct{})}
	ag, _ := agent.New(agent.AgentConfig{ID: "a", Model: m})
	_, url := serveOS(t, AgentOSConfig{Agents: map[string]*agent.Agent{"a": ag}, RunTimeout: 50 * time.Millisecond})
	started := time.Now()
	resp, r := startStream(t, url)
	defer resp.Body.Close()
	rest := readFrames(t, r, nil)
	if len(rest) == 0 || rest[len(rest)-1].event != "run_error" || !strings.Contains(rest[len(rest)-1].data, "deadline") {
		t.Fatalf("frames = %+v", rest)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("run lasted %s despite a 50ms RunTimeout", elapsed)
	}
}

func TestRecoverEndpoint(t *testing.T) {
	store := in_memory.New()
	ctx := context.Background()
	_ = store.CreateSession(ctx, &session.AgentSession{SessionID: "s", AgentID: "a", UserID: ""})
	old := time.Now().Add(-time.Hour)
	_ = store.AppendRun(ctx, &session.RunOutput{RunID: "r", AgentID: "a", SessionID: "s",
		Input: session.RunInput{Message: "hello"}, Status: session.RunStatusRunning, LeaseTimeout: time.Minute, CreatedAt: old, UpdatedAt: old})
	ag, _ := agent.New(agent.AgentConfig{ID: "a", Model: mock.New(), DB: store})
	app, err := New(AgentOSConfig{Agents: map[string]*agent.Agent{"a": ag}, DB: store})
	if err != nil {
		t.Fatal(err)
	}
	post := func() int {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents/a/runs/r/recover", nil))
		return rec.Code
	}
	if code := post(); code != http.StatusConflict {
		t.Fatalf("recover before the run is reaped = %d, want 409", code)
	}
	app.Start(ctx) // the reaper fails the orphaned run at startup
	defer func() { _ = app.Shutdown(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, _ := store.GetRun(ctx, db.RunQuery{RunID: "r"})
		if run.Status == session.RunStatusError || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("recover = %d", code)
	}
	recovered, _ := store.GetRun(ctx, db.RunQuery{RunID: "r"})
	successor, _ := store.GetRun(ctx, db.RunQuery{RunID: recovered.RecoveredBy})
	if successor == nil || successor.Status != session.RunStatusCompleted {
		t.Fatalf("recovered run = %+v", successor)
	}
	if code := post(); code != http.StatusConflict {
		t.Fatalf("second recover = %d, want 409", code)
	}
}
