package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/sqlite"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/middleware"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/authz"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	toolservice "github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

func newTestAgent(t *testing.T, name string) *agent.Agent {
	t.Helper()
	m := mock.New()
	content := "hello from " + name
	m.ResponseData = &models.ModelResponse{Content: content}
	m.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: content},
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

func TestListAgents(t *testing.T) {
	agents := map[string]*agent.Agent{
		"a1": newTestAgent(t, "a1"),
	}
	router := NewRouter(agents, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp ListAgentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].ID != "a1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestGetAgent(t *testing.T) {
	agents := map[string]*agent.Agent{
		"a1": newTestAgent(t, "a1"),
	}
	router := NewRouter(agents, nil)
	req := httptest.NewRequest(http.MethodGet, "/a1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp AgentInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != "a1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestRunAgentJSON(t *testing.T) {
	agents := map[string]*agent.Agent{
		"a1": newTestAgent(t, "a1"),
	}
	router := NewRouter(agents, nil)
	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/a1/runs", body)
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
	if output.Content != "hello from a1" {
		t.Fatalf("unexpected content: %q", output.Content)
	}
}

func TestRunAgentSSE(t *testing.T) {
	agents := map[string]*agent.Agent{
		"a1": newTestAgent(t, "a1"),
	}
	router := NewRouter(agents, nil)
	req := httptest.NewRequest(http.MethodPost, "/a1/runs?stream=true", strings.NewReader("hi"))
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

func TestRunAgentNotFound(t *testing.T) {
	router := NewRouter(map[string]*agent.Agent{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/missing/runs", strings.NewReader("hi"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestRunAgentRejectsClientMetadata(t *testing.T) {
	router := NewRouter(map[string]*agent.Agent{"a1": newTestAgent(t, "a1")}, nil)
	req := httptest.NewRequest(http.MethodPost, "/a1/runs", strings.NewReader(`{"message":"hi","metadata":{"project_id":"forged"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

type busyModel struct {
	started chan struct{}
	release chan struct{}
}

func (m *busyModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	m.started <- struct{}{}
	select {
	case <-m.release:
		return &models.ModelResponse{Content: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (m *busyModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, context.Canceled
}
func (m *busyModel) ID() string       { return "busy" }
func (m *busyModel) Provider() string { return "test" }

func TestRunAgentMapsBusyBeforeWritingResponse(t *testing.T) {
	model := &busyModel{started: make(chan struct{}, 1), release: make(chan struct{})}
	value, err := agent.New(agent.AgentConfig{ID: "a1", Model: model, MaxConcurrentRuns: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = value.Run(context.Background(), "hold") }()
	<-model.started

	for _, path := range []string{"/a1/runs", "/a1/runs?stream=true"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("busy"))
		rec := httptest.NewRecorder()
		NewRouter(map[string]*agent.Agent{"a1": value}, nil).ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s status = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	close(model.release)
	<-done
}

func TestRunAgentMapsBackgroundBusyTo429WithoutCreatingRun(t *testing.T) {
	store := in_memory.New()
	model := &busyModel{started: make(chan struct{}, 1), release: make(chan struct{})}
	value, err := agent.New(agent.AgentConfig{ID: "a1", Model: model, DB: store, MaxConcurrentRuns: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = value.Run(context.Background(), "hold") }()
	<-model.started

	router := NewRouter(map[string]*agent.Agent{"a1": value}, store, runmanager.New(store, time.Second))
	req := httptest.NewRequest(http.MethodPost, "/a1/runs?background=true", strings.NewReader("busy"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	close(model.release)
	<-done
}

func TestRunAgentMapsAdmissionRejectionTo429(t *testing.T) {
	controller, err := runmanager.NewAdmissionController(runmanager.AdmissionConfig{Total: 1, Queue: 0, PerTenant: 1})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	_, held, err := controller.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold admission: %v", err)
	}
	defer held.Release()
	store := in_memory.New()
	value, err := agent.New(agent.AgentConfig{ID: "a1", Model: mock.New(), DB: store, Admission: controller})
	if err != nil {
		t.Fatalf("New agent: %v", err)
	}
	router := NewRouter(map[string]*agent.Agent{"a1": value}, store, runmanager.New(store, time.Second).SetAdmission(controller))
	for _, path := range []string{"/a1/runs", "/a1/runs?stream=true", "/a1/runs?background=true"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("full"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s status = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func newApprovalRouteAgent(t *testing.T, store db.DB, userID string) *agent.Agent {
	t.Helper()
	model := mock.New()
	model.ToolCallRounds = 1
	model.ResponseData = &models.ModelResponse{Content: "resumed"}
	base, err := tools.NewFunction("mock_tool", "approval tool", func(args map[string]any) string {
		return "approved"
	})
	if err != nil {
		t.Fatalf("NewFunction: %v", err)
	}
	value, err := agent.New(agent.AgentConfig{
		ID: "a1", Model: model, Tools: []tools.Tool{tools.WithApproval(base, tools.ApprovalRequired)},
		DB: store, UserID: userID,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return value
}

func TestResumeAgentSynchronouslyValidatesAgentAndRequirement(t *testing.T) {
	store := in_memory.New()
	value := newApprovalRouteAgent(t, store, "owner")
	paused, err := value.Run(context.Background(), "approve")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	requirementID := paused.ActiveRequirement().ID
	other, err := agent.New(agent.AgentConfig{ID: "a2", Model: mock.New(), DB: store, UserID: "owner"})
	if err != nil {
		t.Fatalf("New other agent: %v", err)
	}
	router := NewRouter(map[string]*agent.Agent{"a1": value, "a2": other}, store)

	request := httptest.NewRequest(http.MethodPost, "/a1/runs/"+paused.RunID+"/resume",
		strings.NewReader(`{"requirement_id":"`+requirementID+`","action":"approve"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("resume status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var output session.RunOutput
	if err := json.Unmarshal(recorder.Body.Bytes(), &output); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if output.Status != session.RunStatusCompleted || output.Requirements[0].Status != requirement.StatusApproved {
		t.Fatalf("output = %+v", output)
	}

	for _, test := range []struct {
		path string
		body string
	}{
		{"/a2/runs/" + paused.RunID + "/resume", `{"requirement_id":"` + requirementID + `","action":"approve"}`},
		{"/a1/runs/" + paused.RunID + "/resume", `{"requirement_id":"wrong","action":"approve"}`},
	} {
		bad := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		bad.Header.Set("Content-Type", "application/json")
		badRecorder := httptest.NewRecorder()
		router.ServeHTTP(badRecorder, bad)
		if badRecorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d: %s", test.path, badRecorder.Code, badRecorder.Body.String())
		}
	}
}

func TestResumeAgentSSEEmitsResumedAndCompleted(t *testing.T) {
	store := in_memory.New()
	value := newApprovalRouteAgent(t, store, "owner")
	paused, err := value.Run(context.Background(), "approve")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/a1/runs/"+paused.RunID+"/resume?stream=true",
		strings.NewReader(`{"requirement_id":"`+paused.ActiveRequirement().ID+`","action":"approve"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	NewRouter(map[string]*agent.Agent{"a1": value}, store).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, event := range []string{"event: run_resumed", "event: run_completed"} {
		if !strings.Contains(body, event) {
			t.Fatalf("missing %q in:\n%s", event, body)
		}
	}
}

func TestResumeAgentHidesAnotherUsersRun(t *testing.T) {
	const key = "resume-secret"
	store := in_memory.New()
	value := newApprovalRouteAgent(t, store, "")
	paused, err := value.RunWith(context.Background(), session.RunRequest{
		Input: session.RunInput{Message: "approve"}, UserID: "alice",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	handler := middleware.AuthMiddleware(middleware.AuthorizationConfig{
		Enabled: true, UserIsolation: true, JWTVerificationKey: key,
	})(NewRouter(map[string]*agent.Agent{"a1": value}, store))
	tokenFor := func(user string) string {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": user, "exp": time.Now().Add(time.Hour).Unix(),
		})
		signed, signErr := token.SignedString([]byte(key))
		if signErr != nil {
			t.Fatalf("sign token: %v", signErr)
		}
		return signed
	}
	body := `{"requirement_id":"` + paused.ActiveRequirement().ID + `","action":"approve"}`
	bobRequest := httptest.NewRequest(http.MethodPost, "/a1/runs/"+paused.RunID+"/resume", strings.NewReader(body))
	bobRequest.Header.Set("Authorization", "Bearer "+tokenFor("bob"))
	bobRequest.Header.Set("Content-Type", "application/json")
	bobRecorder := httptest.NewRecorder()
	handler.ServeHTTP(bobRecorder, bobRequest)
	if bobRecorder.Code != http.StatusNotFound {
		t.Fatalf("bob status = %d: %s", bobRecorder.Code, bobRecorder.Body.String())
	}

	aliceRequest := httptest.NewRequest(http.MethodPost, "/a1/runs/"+paused.RunID+"/resume", strings.NewReader(body))
	aliceRequest.Header.Set("Authorization", "Bearer "+tokenFor("alice"))
	aliceRequest.Header.Set("Content-Type", "application/json")
	aliceRecorder := httptest.NewRecorder()
	handler.ServeHTTP(aliceRecorder, aliceRequest)
	if aliceRecorder.Code != http.StatusOK {
		t.Fatalf("alice status = %d: %s", aliceRecorder.Code, aliceRecorder.Body.String())
	}
}

func TestStrictMemoryRoutesRequirePrincipalAndIgnoreBodyUserOnRunAndResume(t *testing.T) {
	store, err := sqlite.New(t.TempDir() + "/strict.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	value := newApprovalRouteAgent(t, store, "")
	principal := execution.Principal{Subject: "alice", Namespace: "tenant", Issuer: "issuer"}
	trusted := execution.ContextWithNamespace(toolservice.ContextWithPrincipal(context.Background(), principal), principal.Namespace)
	paused, err := value.RunWith(trusted, session.RunRequest{
		Input: session.RunInput{Message: "approve"}, UserID: principal.Subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouterWithConfig(Config{Agents: map[string]*agent.Agent{"a1": value}, DB: store, RequirePrincipal: true})

	missing := httptest.NewRequest(http.MethodPost, "/a1/runs", strings.NewReader(`{"message":"hello","user_id":"bob"}`))
	missing.Header.Set("Content-Type", "application/json")
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, missing)
	if missingRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal status=%d body=%s", missingRecorder.Code, missingRecorder.Body.String())
	}

	run := httptest.NewRequest(http.MethodPost, "/a1/runs", strings.NewReader(`{"message":"hello","user_id":"bob"}`))
	run.Header.Set("Content-Type", "application/json")
	run = run.WithContext(trusted)
	runRecorder := httptest.NewRecorder()
	router.ServeHTTP(runRecorder, run)
	if runRecorder.Code != http.StatusOK {
		t.Fatalf("run status=%d body=%s", runRecorder.Code, runRecorder.Body.String())
	}
	var runOutput session.RunOutput
	if err := json.Unmarshal(runRecorder.Body.Bytes(), &runOutput); err != nil || runOutput.UserID != principal.Subject {
		t.Fatalf("run user=%q err=%v", runOutput.UserID, err)
	}

	resume := httptest.NewRequest(http.MethodPost, "/a1/runs/"+paused.RunID+"/resume", strings.NewReader(`{"requirement_id":"`+paused.ActiveRequirement().ID+`","action":"approve","user_id":"bob"}`))
	resume.Header.Set("Content-Type", "application/json")
	resume = resume.WithContext(trusted)
	resumeRecorder := httptest.NewRecorder()
	router.ServeHTTP(resumeRecorder, resume)
	if resumeRecorder.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", resumeRecorder.Code, resumeRecorder.Body.String())
	}
	var resumed session.RunOutput
	if err := json.Unmarshal(resumeRecorder.Body.Bytes(), &resumed); err != nil || resumed.UserID != principal.Subject {
		t.Fatalf("resume user=%q err=%v", resumed.UserID, err)
	}
}

func TestDetachedRunAuthorityCopiesOnlyVerifiedIdentity(t *testing.T) {
	type privateKey struct{}
	principal := execution.Principal{Subject: "alice", Namespace: "tenant", Issuer: "issuer"}
	requestCtx, cancel := context.WithCancel(context.WithValue(context.Background(), privateKey{}, "request-private"))
	requestCtx = execution.ContextWithNamespace(toolservice.ContextWithPrincipal(requestCtx, principal), principal.Namespace)
	detached := authz.BackgroundExecutionContext(requestCtx, true)
	cancel()
	if detached.Done() != nil || detached.Value(privateKey{}) != nil {
		t.Fatalf("detached context retained cancellation or private request value")
	}
	got, err := toolservice.PrincipalFromContext(detached)
	namespace, ok := execution.NamespaceFromContext(detached)
	if err != nil || got != principal || !ok || namespace != principal.Namespace {
		t.Fatalf("principal=%+v namespace=%q ok=%v err=%v", got, namespace, ok, err)
	}
	legacy := authz.BackgroundExecutionContext(requestCtx, false)
	if legacy.Value(privateKey{}) != "request-private" || legacy.Err() != context.Canceled {
		t.Fatalf("legacy background context did not retain caller values and cancellation")
	}
}
