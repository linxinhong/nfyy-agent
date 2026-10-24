package os

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

func newTestAgent(t *testing.T, name string) *agent.Agent {
	t.Helper()
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "hello"}
	ag, err := agent.New(agent.AgentConfig{
		ID:    name,
		Name:  "Test " + name,
		Model: m,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return ag
}

func newTestTeam(t *testing.T, name string) *team.Team {
	t.Helper()
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "hello"}
	m.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: "hello"},
	}
	member, err := agent.New(agent.AgentConfig{
		ID:    name + "-member",
		Name:  "Test " + name + " Member",
		Model: m,
	})
	if err != nil {
		t.Fatalf("create member agent: %v", err)
	}
	tm, err := team.New(team.TeamConfig{
		ID:      name,
		Name:    "Test " + name,
		Model:   m,
		Members: []*agent.Agent{member},
	})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	return tm
}

func newTestWorkflow(t *testing.T, name string) *workflow.Workflow {
	t.Helper()
	wf, err := workflow.New(workflow.WorkflowConfig{
		ID:   name,
		Name: "Test " + name,
		Steps: []workflow.Step{
			{
				Name: "step-1",
				Function: func(_ context.Context, _ workflow.StepInput) (workflow.StepOutput, error) {
					return workflow.StepOutput{Content: "hello"}, nil
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	return wf
}

func TestConfigRoute(t *testing.T) {
	os, err := New(AgentOSConfig{
		Agents: map[string]*agent.Agent{
			"a1": newTestAgent(t, "a1"),
		},
		Teams: map[string]*team.Team{
			"t1": newTestTeam(t, "t1"),
		},
		Workflows: map[string]*workflow.Workflow{
			"w1": newTestWorkflow(t, "w1"),
		},
	})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	agents, ok := resp["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %v", resp["agents"])
	}
	teams, ok := resp["teams"].([]any)
	if !ok || len(teams) != 1 {
		t.Fatalf("expected 1 team, got %v", resp["teams"])
	}
	workflows, ok := resp["workflows"].([]any)
	if !ok || len(workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %v", resp["workflows"])
	}

	assertHasID := func(kind string, items []any, id string) {
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if m["id"] == id {
				return
			}
		}
		t.Fatalf("expected %s with id %q in config", kind, id)
	}
	assertHasID("team", teams, "t1")
	assertHasID("workflow", workflows, "w1")
}

func TestHealthRoute(t *testing.T) {
	os, err := New(AgentOSConfig{})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("status = %q, want ok", resp["status"])
	}
}

func TestModelsRoute(t *testing.T) {
	mockModel := mock.New()
	os, err := New(AgentOSConfig{
		Models: map[string]models.Model{
			"chat": mockModel,
		},
	})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Models []map[string]string `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(resp.Models))
	}
	if resp.Models[0]["id"] != "chat" {
		t.Fatalf("first model = %v, want chat", resp.Models[0])
	}
	if resp.Models[0]["provider"] != "mock" {
		t.Fatalf("first model provider = %v, want mock", resp.Models[0])
	}
}
