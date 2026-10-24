package workflows

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

func newTestWorkflow(t *testing.T, id string, store db.DB) *workflow.Workflow {
	t.Helper()
	wf, err := workflow.New(workflow.WorkflowConfig{
		ID:   id,
		Name: "Test " + id,
		Steps: []workflow.Step{
			{
				Name: "step1",
				Function: func(_ context.Context, input workflow.StepInput) (workflow.StepOutput, error) {
					return workflow.StepOutput{Content: "workflow response from " + id}, nil
				},
			},
		},
		DB: store,
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	return wf
}

func TestListWorkflows(t *testing.T) {
	workflows := map[string]*workflow.Workflow{
		"w1": newTestWorkflow(t, "w1", nil),
	}
	router := NewRouter(workflows, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp ListWorkflowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Workflows) != 1 || resp.Workflows[0].ID != "w1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestGetWorkflow(t *testing.T) {
	workflows := map[string]*workflow.Workflow{
		"w1": newTestWorkflow(t, "w1", nil),
	}
	router := NewRouter(workflows, nil)

	req := httptest.NewRequest(http.MethodGet, "/w1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp WorkflowInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != "w1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestRunWorkflowInBackground(t *testing.T) {
	store := in_memory.New()
	manager := runmanager.New(store, time.Second)
	router := NewRouter(map[string]*workflow.Workflow{"w1": newTestWorkflow(t, "w1", store)}, store, manager)
	req := httptest.NewRequest(http.MethodPost, "/w1/runs?background=true", strings.NewReader("hello"))
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
			t.Fatalf("workflow run did not complete: %+v", output)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGetWorkflowNotFound(t *testing.T) {
	router := NewRouter(map[string]*workflow.Workflow{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestRunWorkflowJSON(t *testing.T) {
	workflows := map[string]*workflow.Workflow{
		"w1": newTestWorkflow(t, "w1", nil),
	}
	router := NewRouter(workflows, nil)

	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/w1/runs", body)
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
	if output.Content != "workflow response from w1" {
		t.Fatalf("unexpected content: %q", output.Content)
	}
	if output.WorkflowID != "w1" {
		t.Fatalf("unexpected workflow id: %q", output.WorkflowID)
	}
}

func TestRunWorkflowSSE(t *testing.T) {
	workflows := map[string]*workflow.Workflow{
		"w1": newTestWorkflow(t, "w1", nil),
	}
	router := NewRouter(workflows, nil)

	req := httptest.NewRequest(http.MethodPost, "/w1/runs?stream=true", strings.NewReader("hi"))
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

	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %v", len(events), events)
	}
	if events[0] != "run_started" {
		t.Fatalf("expected run_started first, got %v", events)
	}
	if events[len(events)-1] != "run_completed" {
		t.Fatalf("expected run_completed last, got %v", events)
	}
}

func TestRunWorkflowWithUserSession(t *testing.T) {
	store := in_memory.New()
	workflows := map[string]*workflow.Workflow{
		"w1": newTestWorkflow(t, "w1", store),
	}
	router := NewRouter(workflows, store)

	// Pre-create the workflow session so the requested session_id is honored.
	if err := store.CreateWorkflowSession(context.Background(), &session.WorkflowSession{
		SessionID:  "s1",
		WorkflowID: "w1",
		UserID:     "u1",
	}); err != nil {
		t.Fatalf("create workflow session: %v", err)
	}

	body := strings.NewReader(`{"message":"hi","user_id":"u1","session_id":"s1"}`)
	req := httptest.NewRequest(http.MethodPost, "/w1/runs", body)
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
		t.Fatalf("unexpected user id: %q", output.UserID)
	}
	if output.SessionID != "s1" {
		t.Fatalf("unexpected session id: %q", output.SessionID)
	}
}

func TestListWorkflowRuns(t *testing.T) {
	store := in_memory.New()
	workflows := map[string]*workflow.Workflow{
		"w1": newTestWorkflow(t, "w1", store),
	}
	router := NewRouter(workflows, store)

	body := strings.NewReader(`{"message":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/w1/runs", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run failed: %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/w1/runs", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runs []*session.RunOutput `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(resp.Runs))
	}
	if resp.Runs[0].WorkflowID != "w1" {
		t.Fatalf("unexpected workflow id: %q", resp.Runs[0].WorkflowID)
	}
}

func TestRunWorkflowNotFound(t *testing.T) {
	router := NewRouter(map[string]*workflow.Workflow{}, nil)

	req := httptest.NewRequest(http.MethodPost, "/missing/runs", strings.NewReader("hi"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestResumeStructuredWorkflow(t *testing.T) {
	store := in_memory.New()
	registry := workflow.NewRegistry()
	registry.RegisterExecutor("write", workflow.ExecutorFunc(func(_ context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
		return workflow.NodeOutput{Value: "done:" + input.Value.(string)}, nil
	}))
	node := workflow.ExecutorNode("write", "write")
	node.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementConfirmation, Prompt: "approve"}
	wf, err := workflow.New(workflow.Config{
		Definition: workflow.Definition{ID: "approval", Version: "v1", Root: workflow.Sequence("root", node)},
		Registry:   registry,
		DB:         store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	router := NewRouter(map[string]*workflow.Workflow{"approval": wf}, store)

	runReq := httptest.NewRequest(http.MethodPost, "/approval/runs", strings.NewReader(`{"message":"record","user_id":"alice"}`))
	runReq.Header.Set("Content-Type", "application/json")
	runRec := httptest.NewRecorder()
	router.ServeHTTP(runRec, runReq)
	if runRec.Code != http.StatusOK {
		t.Fatalf("run status = %d: %s", runRec.Code, runRec.Body.String())
	}
	var paused session.RunOutput
	if err := json.Unmarshal(runRec.Body.Bytes(), &paused); err != nil {
		t.Fatalf("decode paused: %v", err)
	}

	resumeBody := `{"requirement_id":"` + paused.Workflow.Requirement.ID + `","action":"approve"}`
	resumeReq := httptest.NewRequest(http.MethodPost, "/approval/runs/"+paused.RunID+"/resume", strings.NewReader(resumeBody))
	resumeReq.Header.Set("Content-Type", "application/json")
	resumeRec := httptest.NewRecorder()
	router.ServeHTTP(resumeRec, resumeReq)
	if resumeRec.Code != http.StatusOK {
		t.Fatalf("resume status = %d: %s", resumeRec.Code, resumeRec.Body.String())
	}
	var completed session.RunOutput
	if err := json.Unmarshal(resumeRec.Body.Bytes(), &completed); err != nil {
		t.Fatalf("decode completed: %v", err)
	}
	if completed.Status != session.RunStatusCompleted || completed.Content != "done:record" {
		t.Fatalf("completed = %+v", completed)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/approval/runs/"+paused.RunID, nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", getRec.Code, getRec.Body.String())
	}
}

func TestStructuredWorkflowSSEIncludesNodeEvents(t *testing.T) {
	registry := workflow.NewRegistry()
	registry.RegisterExecutor("echo", workflow.ExecutorFunc(func(_ context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
		return workflow.NodeOutput{Value: input.Value}, nil
	}))
	wf, err := workflow.New(workflow.Config{
		Definition: workflow.Definition{ID: "events", Version: "v1", Root: workflow.Sequence("root", workflow.ExecutorNode("echo", "echo"))},
		Registry:   registry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	router := NewRouter(map[string]*workflow.Workflow{"events": wf}, nil)
	req := httptest.NewRequest(http.MethodPost, "/events/runs?stream=true", strings.NewReader("hello"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{"event: run_started", "event: node_started", "event: node_completed", "event: run_completed"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("SSE missing %q:\n%s", expected, body)
		}
	}
}

func TestStructuredWorkflowExecutionErrorReturnsRunOutput(t *testing.T) {
	registry := workflow.NewRegistry()
	registry.RegisterExecutor("fail", workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
		return workflow.NodeOutput{}, errors.New("business failure")
	}))
	wf, err := workflow.New(workflow.Config{Definition: workflow.Definition{ID: "failure", Version: "v1", Root: workflow.ExecutorNode("fail", "fail")}, Registry: registry})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	router := NewRouter(map[string]*workflow.Workflow{"failure": wf}, nil)
	req := httptest.NewRequest(http.MethodPost, "/failure/runs", strings.NewReader("input"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var output session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if output.Status != session.RunStatusError || !strings.Contains(output.Error, "business failure") {
		t.Fatalf("output = %+v", output)
	}
}

type updateFailStore struct {
	db.DB
}

func (updateFailStore) UpdateRun(context.Context, *session.RunOutput, session.RunStatus) error {
	return errors.New("storage unavailable")
}

func TestStructuredWorkflowStorageErrorReturnsServerError(t *testing.T) {
	store := updateFailStore{DB: in_memory.New()}
	registry := workflow.NewRegistry()
	registry.RegisterExecutor("echo", workflow.ExecutorFunc(func(_ context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
		return workflow.NodeOutput{Value: input.Value}, nil
	}))
	wf, err := workflow.New(workflow.Config{
		Definition: workflow.Definition{ID: "storage-failure", Version: "v1", Root: workflow.ExecutorNode("echo", "echo")},
		Registry:   registry,
		DB:         store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	router := NewRouter(map[string]*workflow.Workflow{"storage-failure": wf}, store)
	req := httptest.NewRequest(http.MethodPost, "/storage-failure/runs", strings.NewReader("input"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}
