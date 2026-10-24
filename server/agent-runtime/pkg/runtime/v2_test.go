package runtime_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	agentruntime "github.com/linxinhong/xingding-agent-runtime/pkg/runtime"
)

func TestProjectSpecV2BuildsAgentExecutionPolicy(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		SchemaVersion: 2,
		ID:            "v2-project",
		Version:       "release-1",
		Agents: []agentruntime.AgentSpec{{
			ID: "assistant", ModelRef: "chat", HooksRef: "audit",
			OutputSchema:  map[string]any{"type": "object"},
			ToolTimeoutMS: 250, RunTimeoutMS: 500, MaxConcurrentRuns: 2,
		}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Models: map[string]models.Model{"chat": mock.New()},
		Hooks:  map[string]*agent.Hooks{"audit": {}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if project.SchemaVersion() != 2 {
		t.Fatalf("schema version = %d", project.SchemaVersion())
	}

	decoded, err := agentruntime.DecodeSpec(strings.NewReader(`{"schema_version":2,"id":"v2-project","version":"release-1","agents":[{"id":"assistant","model_ref":"chat","output_schema":{"type":"object"},"tool_timeout_ms":250,"run_timeout_ms":500,"max_concurrent_runs":2,"hooks_ref":"audit"}]}`))
	if err != nil || decoded.Agents[0].HooksRef != "audit" || decoded.Agents[0].RunTimeoutMS != 500 {
		t.Fatalf("DecodeSpec = %+v, %v", decoded, err)
	}
}

func TestProjectSpecV1RejectsV2ExecutionPolicy(t *testing.T) {
	_, err := agentruntime.Build(agentruntime.ProjectSpec{
		SchemaVersion: 1, ID: "legacy", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat", RunTimeoutMS: 1}},
	}, agentruntime.Dependencies{Models: map[string]models.Model{"chat": mock.New()}})
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) || len(buildErr.Diagnostics) == 0 || buildErr.Diagnostics[0].Path != "agents[0].run_timeout_ms" {
		t.Fatalf("Build error = %v", err)
	}
}

func TestDecodeProjectSpecV1RejectsExplicitZeroV2Field(t *testing.T) {
	_, err := agentruntime.DecodeSpec(strings.NewReader(`{"schema_version":1,"id":"legacy","version":"v1","agents":[{"id":"assistant","model_ref":"chat","run_timeout_ms":0}]}`))
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) || len(buildErr.Diagnostics) != 1 || buildErr.Diagnostics[0].Path != "agents[0].run_timeout_ms" {
		t.Fatalf("DecodeSpec error = %v", err)
	}
}

func TestProjectSpecV2RejectsUnknownHookAndNegativePolicy(t *testing.T) {
	_, err := agentruntime.Build(agentruntime.ProjectSpec{
		SchemaVersion: 2, ID: "invalid", Version: "v2",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat", HooksRef: "missing", ToolTimeoutMS: -1, RunTimeoutMS: -2, MaxConcurrentRuns: -3}},
	}, agentruntime.Dependencies{Models: map[string]models.Model{"chat": mock.New()}})
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build error = %v", err)
	}
	wants := map[string]bool{
		"agents[0].tool_timeout_ms":     false,
		"agents[0].run_timeout_ms":      false,
		"agents[0].max_concurrent_runs": false,
		"agents[0].hooks_ref":           false,
	}
	for _, diagnostic := range buildErr.Diagnostics {
		if _, ok := wants[diagnostic.Path]; ok {
			wants[diagnostic.Path] = true
		}
	}
	for path, found := range wants {
		if !found {
			t.Fatalf("missing diagnostic %s: %+v", path, buildErr.Diagnostics)
		}
	}
}

func TestProjectSpecV2RejectsInvalidOutputSchemaAtAgentPath(t *testing.T) {
	_, err := agentruntime.Build(agentruntime.ProjectSpec{
		SchemaVersion: 2, ID: "invalid-schema", Version: "v2",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat", OutputSchema: map[string]any{"type": "not-a-type"}}},
	}, agentruntime.Dependencies{Models: map[string]models.Model{"chat": mock.New()}})
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build error = %v", err)
	}
	for _, diagnostic := range buildErr.Diagnostics {
		if diagnostic.Code == agentruntime.DiagnosticInvalidSchema && diagnostic.Path == "agents[0].output_schema" {
			return
		}
	}
	t.Fatalf("missing schema diagnostic: %+v", buildErr.Diagnostics)
}
