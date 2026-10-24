package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	knowledgepkg "github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	agentos "github.com/linxinhong/xingding-agent-runtime/pkg/os"
	agentruntime "github.com/linxinhong/xingding-agent-runtime/pkg/runtime"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/vectordb"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

type fakeKnowledge struct{}

func (fakeKnowledge) Search(context.Context, string, int) ([]vectordb.Document, error) {
	return nil, nil
}

func (fakeKnowledge) GetContent(context.Context, string) (string, error) { return "", nil }

func TestBuildAssemblesProjectAndAgentOS(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		ID: "reporting", Version: "v1", Name: "Reporting",
		Agents: []agentruntime.AgentSpec{{
			ID: "researcher", ModelRef: "chat", Instructions: "Research the subject.",
		}},
		Teams: []agentruntime.TeamSpec{{
			ID: "editorial", ModelRef: "chat", MemberRefs: []string{"researcher"},
		}},
		Workflows: []agentruntime.WorkflowSpec{{Definition: workflow.Definition{
			ID: "report", Version: "v1", Root: workflow.Sequence("root",
				workflow.ExecutorNode("research", agentruntime.AgentExecutorRef("researcher")),
				workflow.ExecutorNode("review", agentruntime.TeamExecutorRef("editorial")),
			),
		}}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Models: map[string]models.Model{"chat": mock.New()},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if project.ID() != "reporting" || project.Version() != "v1" {
		t.Fatalf("project identity = %q@%q", project.ID(), project.Version())
	}
	if _, ok := project.Agent("researcher"); !ok {
		t.Fatal("researcher agent was not assembled")
	}
	if _, ok := project.Team("editorial"); !ok {
		t.Fatal("editorial team was not assembled")
	}
	if _, ok := project.Workflow("report"); !ok {
		t.Fatal("report workflow was not assembled")
	}

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	project.AgentOS().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "researcher") || !strings.Contains(rec.Body.String(), "report") {
		t.Fatalf("config response = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBuildRejectsUnresolvedReference(t *testing.T) {
	_, err := agentruntime.Build(agentruntime.ProjectSpec{
		ID: "invalid", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "missing"}},
	}, agentruntime.Dependencies{})
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) || len(buildErr.Diagnostics) != 1 ||
		buildErr.Diagnostics[0].Code != agentruntime.DiagnosticUnknownReference ||
		buildErr.Diagnostics[0].Path != "agents[0].model_ref" || buildErr.Diagnostics[0].ComponentID != "assistant" {
		t.Fatalf("Build error = %v", err)
	}
}

func TestBuildRejectsDuplicateComponentID(t *testing.T) {
	_, err := agentruntime.Build(agentruntime.ProjectSpec{
		ID: "invalid", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat"}, {ID: "assistant", ModelRef: "chat"}},
	}, agentruntime.Dependencies{Models: map[string]models.Model{"chat": mock.New()}})
	if err == nil || !strings.Contains(err.Error(), `duplicate agent id "assistant"`) {
		t.Fatalf("Build error = %v", err)
	}
}

func TestProjectSpecRoundTripsReferences(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		ID: "safe", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "private-model", ToolRefs: []string{"crm"}}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `"model_ref":"private-model"`) || !strings.Contains(got, `"tool_refs":["crm"]`) {
		t.Fatalf("serialized spec = %s", got)
	}
}

func TestBuildRejectsNonSerializableProjectSpec(t *testing.T) {
	_, err := agentruntime.Build(agentruntime.ProjectSpec{
		ID: "invalid", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "assistant", ModelRef: "chat", SessionState: map[string]any{"work": make(chan int)},
		}},
	}, agentruntime.Dependencies{Models: map[string]models.Model{"chat": mock.New()}})
	if err == nil || !strings.Contains(err.Error(), "project spec is not JSON serializable") {
		t.Fatalf("Build error = %v", err)
	}
}

func TestBuildSnapshotsProjectSpec(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		ID: "snapshot", Version: "v1",
		Workflows: []agentruntime.WorkflowSpec{{Definition: workflow.Definition{
			ID: "stable", Version: "v1", Root: workflow.Sequence("root", workflow.ExecutorNode("step", "echo")),
		}}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{Executors: map[string]workflow.Executor{
		"echo": workflow.ExecutorFunc(func(_ context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
			return workflow.NodeOutput{Value: input.Value}, nil
		}),
	}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	hash := project.SpecHash()
	spec.Workflows[0].Definition.Root.Children[0].ExecutorRef = "changed-after-build"
	if project.SpecHash() != hash {
		t.Fatalf("spec hash changed after caller mutation: %q != %q", project.SpecHash(), hash)
	}
	built, _ := project.Workflow("stable")
	output, err := built.Run(context.Background(), "original")
	if err != nil || output.Content != "original" {
		t.Fatalf("Run after caller mutation = %+v, %v", output, err)
	}
}

func TestBuildResolvesAgentKnowledgeByProjectID(t *testing.T) {
	knowledge := fakeKnowledge{}
	project, err := agentruntime.Build(agentruntime.ProjectSpec{
		ID: "knowledge", Version: "v1",
		Knowledges: []agentruntime.KnowledgeSpec{{ID: "docs", Ref: "production-kb"}},
		Agents:     []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat", KnowledgeRef: "docs"}},
	}, agentruntime.Dependencies{
		Models:    map[string]models.Model{"chat": mock.New()},
		Knowledge: map[string]knowledgepkg.Knowledge{"production-kb": knowledge},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if value, ok := project.Knowledge("docs"); !ok || value != knowledge {
		t.Fatalf("Knowledge lookup = %T, %v", value, ok)
	}
}

func TestBuildEnablesAuthenticationWhenSecurityKeyIsProvided(t *testing.T) {
	project, err := agentruntime.Build(
		agentruntime.ProjectSpec{ID: "secured", Version: "v1"},
		agentruntime.Dependencies{SecurityKey: "deployment-key"},
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	project.AgentOS().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer deployment-key")
	rec = httptest.NewRecorder()
	project.AgentOS().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBuildNormalizesAuthorizationSecurityKey(t *testing.T) {
	project, err := agentruntime.Build(
		agentruntime.ProjectSpec{ID: "secured", Version: "v1"},
		agentruntime.Dependencies{Authorization: agentos.AuthorizationConfig{SecurityKey: "nested-key"}},
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rec := httptest.NewRecorder()
	project.AgentOS().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer nested-key")
	rec = httptest.NewRecorder()
	project.AgentOS().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d: %s", rec.Code, rec.Body.String())
	}

	_, err = agentruntime.Build(
		agentruntime.ProjectSpec{ID: "conflict", Version: "v1"},
		agentruntime.Dependencies{SecurityKey: "one", Authorization: agentos.AuthorizationConfig{SecurityKey: "two"}},
	)
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) || len(buildErr.Diagnostics) != 1 || buildErr.Diagnostics[0].Code != agentruntime.DiagnosticConflictingConfiguration {
		t.Fatalf("Build conflict error = %v", err)
	}
}

func TestBuildRejectsInvalidExecutionLimits(t *testing.T) {
	negative := -1
	tests := []struct {
		name string
		spec agentruntime.ProjectSpec
		want string
	}{
		{
			name: "agent max tool calls",
			spec: agentruntime.ProjectSpec{ID: "invalid", Version: "v1", Agents: []agentruntime.AgentSpec{{
				ID: "assistant", ModelRef: "chat", MaxToolCalls: -1,
			}}},
			want: "max_tool_calls must not be negative",
		},
		{
			name: "agent max tokens",
			spec: agentruntime.ProjectSpec{ID: "invalid", Version: "v1", Agents: []agentruntime.AgentSpec{{
				ID: "assistant", ModelRef: "chat", MaxTokens: &negative,
			}}},
			want: "max_tokens must be positive",
		},
		{
			name: "team max iterations",
			spec: agentruntime.ProjectSpec{
				ID: "invalid", Version: "v1",
				Agents: []agentruntime.AgentSpec{{ID: "member", ModelRef: "chat"}},
				Teams:  []agentruntime.TeamSpec{{ID: "team", ModelRef: "chat", MemberRefs: []string{"member"}, MaxIterations: -1}},
			},
			want: "max_iterations must not be negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := agentruntime.Build(tt.spec, agentruntime.Dependencies{Models: map[string]models.Model{"chat": mock.New()}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Build error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBuildNormalizesSchemaVersionAndHashesSnapshot(t *testing.T) {
	spec := agentruntime.ProjectSpec{ID: "versioned", Version: "release-1", Name: "Versioned"}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if project.SchemaVersion() != agentruntime.LegacySchemaVersion {
		t.Fatalf("schema version = %d", project.SchemaVersion())
	}
	if len(project.SpecHash()) != 64 {
		t.Fatalf("spec hash = %q", project.SpecHash())
	}

	rebuilt, err := agentruntime.Build(spec, agentruntime.Dependencies{SecurityKey: "different-deployment"})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if rebuilt.SpecHash() != project.SpecHash() {
		t.Fatalf("dependency changed hash: %q != %q", rebuilt.SpecHash(), project.SpecHash())
	}
	changed, err := agentruntime.Build(agentruntime.ProjectSpec{ID: "versioned", Version: "release-2", Name: "Versioned"}, agentruntime.Dependencies{})
	if err != nil {
		t.Fatalf("changed build: %v", err)
	}
	if changed.SpecHash() == project.SpecHash() {
		t.Fatal("business version change did not change spec hash")
	}
}

func TestDecodeSpecIsStrictAndRequiresSchemaVersion(t *testing.T) {
	spec, err := agentruntime.DecodeSpec(strings.NewReader(`{"schema_version":1,"id":"strict","version":"v1"}`))
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if spec.SchemaVersion != agentruntime.LegacySchemaVersion || spec.ID != "strict" {
		t.Fatalf("decoded spec = %+v", spec)
	}

	tests := []struct {
		name  string
		input string
		code  agentruntime.DiagnosticCode
		path  string
	}{
		{name: "missing version", input: `{"id":"strict","version":"v1"}`, code: agentruntime.DiagnosticRequired, path: "schema_version"},
		{name: "unsupported version", input: `{"schema_version":99,"id":"strict","version":"v1"}`, code: agentruntime.DiagnosticUnsupportedSchemaVersion, path: "schema_version"},
		{name: "unknown field", input: `{"schema_version":1,"id":"strict","version":"v1","modle_ref":"x"}`, code: agentruntime.DiagnosticInvalidJSON},
		{name: "nested unknown field", input: `{"schema_version":1,"id":"strict","version":"v1","agents":[{"id":"a","model_ref":"m","typo":true}]}`, code: agentruntime.DiagnosticInvalidJSON},
		{name: "trailing document", input: `{"schema_version":1,"id":"strict","version":"v1"} {}`, code: agentruntime.DiagnosticInvalidJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := agentruntime.DecodeSpec(strings.NewReader(tt.input))
			var buildErr *agentruntime.BuildError
			if !errors.As(err, &buildErr) || len(buildErr.Diagnostics) != 1 {
				t.Fatalf("DecodeSpec error = %#v", err)
			}
			if got := buildErr.Diagnostics[0]; got.Code != tt.code || got.Path != tt.path {
				t.Fatalf("diagnostic = %+v, want code=%q path=%q", got, tt.code, tt.path)
			}
		})
	}
}

func TestBuildAggregatesStableStructuredDiagnostics(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		SchemaVersion: 2,
		Knowledges:    []agentruntime.KnowledgeSpec{{ID: "docs", Ref: "missing-kb"}},
		Agents: []agentruntime.AgentSpec{
			{ID: "assistant", ModelRef: "missing-model", ToolRefs: []string{"missing-tool"}, KnowledgeRef: "docs", MaxToolCalls: -1},
			{ID: "assistant", ModelRef: "missing-model"},
		},
		Teams: []agentruntime.TeamSpec{{
			ID: "reviewers", ModelRef: "missing-model", MemberRefs: []string{"ghost"}, MaxIterations: -1,
		}},
		Workflows: []agentruntime.WorkflowSpec{
			{Definition: workflow.Definition{ID: "flow", Root: workflow.NodeSpec{ID: "root", Type: workflow.NodeTypeStep, ExecutorRef: "missing-executor"}}},
			{Definition: workflow.Definition{ID: "conditional", Version: "v1", Root: workflow.Condition("condition", "missing-predicate",
				workflow.ExecutorNode("then", agentruntime.AgentExecutorRef("assistant")),
				workflow.ExecutorNode("else", agentruntime.AgentExecutorRef("assistant")),
			)}},
			{Definition: workflow.Definition{ID: "broken", Root: workflow.ExecutorNode("step", agentruntime.AgentExecutorRef("assistant"))}},
		},
	}
	_, err := agentruntime.Build(spec, agentruntime.Dependencies{})
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build error = %#v", err)
	}
	wants := []struct {
		code agentruntime.DiagnosticCode
		path string
	}{
		{agentruntime.DiagnosticRequired, "id"},
		{agentruntime.DiagnosticRequired, "version"},
		{agentruntime.DiagnosticUnknownReference, "knowledges[0].ref"},
		{agentruntime.DiagnosticInvalidValue, "agents[0].max_tool_calls"},
		{agentruntime.DiagnosticUnknownReference, "agents[0].model_ref"},
		{agentruntime.DiagnosticUnknownReference, "agents[0].tool_refs[0]"},
		{agentruntime.DiagnosticDuplicate, "agents[1].id"},
		{agentruntime.DiagnosticUnknownReference, "agents[1].model_ref"},
		{agentruntime.DiagnosticInvalidValue, "teams[0].max_iterations"},
		{agentruntime.DiagnosticUnknownReference, "teams[0].model_ref"},
		{agentruntime.DiagnosticUnknownReference, "teams[0].member_refs[0]"},
		{agentruntime.DiagnosticUnknownReference, "workflows[0].definition.root.executor_ref"},
		{agentruntime.DiagnosticInvalidWorkflow, "workflows[0].definition"},
		{agentruntime.DiagnosticUnknownReference, "workflows[1].definition.root.predicate_ref"},
		{agentruntime.DiagnosticInvalidWorkflow, "workflows[2].definition"},
	}
	if len(buildErr.Diagnostics) != len(wants) {
		t.Fatalf("diagnostics = %+v", buildErr.Diagnostics)
	}
	last := -1
	for _, want := range wants {
		found := -1
		for i, diagnostic := range buildErr.Diagnostics {
			if diagnostic.Code == want.code && diagnostic.Path == want.path {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("missing diagnostic code=%q path=%q: %+v", want.code, want.path, buildErr.Diagnostics)
		}
		if found <= last {
			t.Fatalf("diagnostics are not stable: %+v", buildErr.Diagnostics)
		}
		last = found
	}
	_, repeatedErr := agentruntime.Build(spec, agentruntime.Dependencies{})
	var repeated *agentruntime.BuildError
	if !errors.As(repeatedErr, &repeated) || !reflect.DeepEqual(repeated.Diagnostics, buildErr.Diagnostics) {
		t.Fatalf("diagnostics are not deterministic:\n%+v\n%+v", buildErr.Diagnostics, repeated.Diagnostics)
	}
}

func TestProjectMetadataIsExposedByAgentOSInfo(t *testing.T) {
	project, err := agentruntime.Build(agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion,
		ID:            "metadata",
		Name:          "Metadata Project",
		Version:       "release-7",
	}, agentruntime.Dependencies{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rec := httptest.NewRecorder()
	project.AgentOS().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("info status = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Project *struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Version       string `json:"version"`
			SchemaVersion int    `json:"schema_version"`
			SpecHash      string `json:"spec_hash"`
		} `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if response.Project == nil || response.Project.ID != project.ID() || response.Project.Name != project.Name() ||
		response.Project.Version != project.Version() || response.Project.SchemaVersion != project.SchemaVersion() ||
		response.Project.SpecHash != project.SpecHash() {
		t.Fatalf("project info = %+v", response.Project)
	}
}

func TestBuildResolvesToolExecutorAndPredicateReferences(t *testing.T) {
	lookup, err := tools.NewFunction("lookup", "lookup data", func(query string) string { return query })
	if err != nil {
		t.Fatalf("NewFunction: %v", err)
	}
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion,
		ID:            "references",
		Version:       "v1",
		Agents:        []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat", ToolRefs: []string{"lookup"}}},
		Workflows: []agentruntime.WorkflowSpec{{Definition: workflow.Definition{
			ID: "route", Version: "v1",
			Root: workflow.Condition("condition", "enabled",
				workflow.ExecutorNode("yes", "respond"),
				workflow.ExecutorNode("no", "respond"),
			),
		}}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Models: map[string]models.Model{"chat": mock.New()},
		Tools:  map[string]tools.Tool{"lookup": lookup},
		Executors: map[string]workflow.Executor{"respond": workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
			return workflow.NodeOutput{Value: "resolved"}, nil
		})},
		Predicates: map[string]workflow.Predicate{"enabled": workflow.PredicateFunc(func(context.Context, workflow.NodeInput) (bool, error) {
			return true, nil
		})},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wf, _ := project.Workflow("route")
	output, err := wf.Run(context.Background(), "input")
	if err != nil || output.Content != "resolved" {
		t.Fatalf("Run = %+v, %v", output, err)
	}
}

func TestSpecSnapshotAndHashPreserveLargeIntegers(t *testing.T) {
	const large = int64(9007199254740993)
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion,
		ID:            "numbers",
		Version:       "v1",
		Workflows: []agentruntime.WorkflowSpec{{
			Definition:   workflow.Definition{ID: "read", Version: "v1", Root: workflow.ExecutorNode("read", "read-state")},
			SessionState: map[string]any{"large": large},
		}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	decoded, err := agentruntime.DecodeSpec(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	deps := agentruntime.Dependencies{Executors: map[string]workflow.Executor{
		"read-state": workflow.ExecutorFunc(func(_ context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
			return workflow.NodeOutput{Value: fmt.Sprint(input.State["large"])}, nil
		}),
	}}
	fromGo, err := agentruntime.Build(spec, deps)
	if err != nil {
		t.Fatalf("Build Go spec: %v", err)
	}
	fromJSON, err := agentruntime.Build(decoded, deps)
	if err != nil {
		t.Fatalf("Build decoded spec: %v", err)
	}
	if fromGo.SpecHash() != fromJSON.SpecHash() {
		t.Fatalf("hashes differ: %s != %s", fromGo.SpecHash(), fromJSON.SpecHash())
	}
	wf, _ := fromGo.Workflow("read")
	output, err := wf.Run(context.Background(), "input")
	if err != nil || output.Content != "9007199254740993" {
		t.Fatalf("large integer output = %q, %v", output.Content, err)
	}
}

func TestBuildWiresHistoryPolicy(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		ID: "history", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "assistant", ModelRef: "chat",
			HistoryPolicy: &agentruntime.HistoryPolicySpec{MaxRuns: 3, MaxTurns: 2, MaxMessages: 50},
		}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Models: map[string]models.Model{"chat": mock.New()},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := project.Agent("assistant"); !ok {
		t.Fatal("agent was not assembled")
	}
	// Verify that a nil HistoryPolicy builds without providers (backward compat).
	spec2 := agentruntime.ProjectSpec{
		ID: "no-policy", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat"}},
	}
	project2, err := agentruntime.Build(spec2, agentruntime.Dependencies{
		Models: map[string]models.Model{"chat": mock.New()},
	})
	if err != nil {
		t.Fatalf("Build without policy: %v", err)
	}
	if _, ok := project2.Agent("assistant"); !ok {
		t.Fatal("agent without policy was not assembled")
	}
}

func TestBuildRejectsInvalidHistoryPolicy(t *testing.T) {
	negative := -1
	tests := []struct {
		name string
		spec agentruntime.ProjectSpec
		want string
	}{
		{
			name: "negative max_runs",
			spec: agentruntime.ProjectSpec{ID: "invalid", Version: "v1", Agents: []agentruntime.AgentSpec{{
				ID: "assistant", ModelRef: "chat",
				HistoryPolicy: &agentruntime.HistoryPolicySpec{MaxRuns: -1},
			}}},
			want: "max_runs must not be negative",
		},
		{
			name: "negative max_turns",
			spec: agentruntime.ProjectSpec{ID: "invalid", Version: "v1", Agents: []agentruntime.AgentSpec{{
				ID: "assistant", ModelRef: "chat",
				HistoryPolicy: &agentruntime.HistoryPolicySpec{MaxTurns: -1},
			}}},
			want: "max_turns must not be negative",
		},
		{
			name: "negative max_messages",
			spec: agentruntime.ProjectSpec{ID: "invalid", Version: "v1", Agents: []agentruntime.AgentSpec{{
				ID: "assistant", ModelRef: "chat",
				HistoryPolicy: &agentruntime.HistoryPolicySpec{MaxMessages: -1},
			}}},
			want: "max_messages must not be negative",
		},
		{
			name: "zero max_tokens and negative max_messages",
			spec: agentruntime.ProjectSpec{ID: "invalid", Version: "v1", Agents: []agentruntime.AgentSpec{{
				ID: "assistant", ModelRef: "chat", MaxTokens: &negative,
				HistoryPolicy: &agentruntime.HistoryPolicySpec{MaxMessages: -1},
			}}},
			want: "max_messages must not be negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := agentruntime.Build(tt.spec, agentruntime.Dependencies{
				Models: map[string]models.Model{"chat": mock.New()},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Build error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBuildRejectsInvalidContextWindowRatios(t *testing.T) {
	tests := []struct {
		name string
		spec agentruntime.ProjectSpec
		want string
	}{
		{
			name: "negative trigger_ratio",
			spec: agentruntime.ProjectSpec{
				SchemaVersion: agentruntime.CurrentSchemaVersion,
				ID:            "bad", Version: "v1",
				Agents: []agentruntime.AgentSpec{{
					ID: "a", ModelRef: "m",
					ContextWindow: &agentruntime.ContextWindowSpec{
						MaxInputTokens: 1000,
						TriggerRatio:   -0.1,
					},
				}},
			},
			want: "trigger_ratio must not be negative",
		},
		{
			name: "negative target_ratio",
			spec: agentruntime.ProjectSpec{
				SchemaVersion: agentruntime.CurrentSchemaVersion,
				ID:            "bad", Version: "v1",
				Agents: []agentruntime.AgentSpec{{
					ID: "a", ModelRef: "m",
					ContextWindow: &agentruntime.ContextWindowSpec{
						MaxInputTokens: 1000,
						TargetRatio:    -0.1,
					},
				}},
			},
			want: "target_ratio must not be negative",
		},
		{
			name: "target >= trigger after defaults (trigger=0, target=.9)",
			spec: agentruntime.ProjectSpec{
				SchemaVersion: agentruntime.CurrentSchemaVersion,
				ID:            "bad", Version: "v1",
				Agents: []agentruntime.AgentSpec{{
					ID: "a", ModelRef: "m",
					ContextWindow: &agentruntime.ContextWindowSpec{
						MaxInputTokens: 1000,
						TriggerRatio:   0,
						TargetRatio:    0.9,
					},
				}},
			},
			want: "target_ratio must be less than trigger_ratio",
		},
		{
			name: "trigger=.6, target=0 → target defaults to .7 > .6",
			spec: agentruntime.ProjectSpec{
				SchemaVersion: agentruntime.CurrentSchemaVersion,
				ID:            "bad", Version: "v1",
				Agents: []agentruntime.AgentSpec{{
					ID: "a", ModelRef: "m",
					ContextWindow: &agentruntime.ContextWindowSpec{
						MaxInputTokens: 1000,
						TriggerRatio:   0.6,
						TargetRatio:    0,
					},
				}},
			},
			want: "target_ratio must be less than trigger_ratio",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := agentruntime.Build(tt.spec, agentruntime.Dependencies{
				Models: map[string]models.Model{"m": mock.New()},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Build error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestHistoryPolicySpecJSONRoundTrip(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		ID: "json", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "assistant", ModelRef: "chat",
			HistoryPolicy: &agentruntime.HistoryPolicySpec{MaxRuns: 5, MaxTurns: 3, MaxMessages: 100},
		}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `"history_policy"`) ||
		!strings.Contains(got, `"max_runs":5`) ||
		!strings.Contains(got, `"max_turns":3`) ||
		!strings.Contains(got, `"max_messages":100`) {
		t.Fatalf("serialized spec = %s", got)
	}
	var decoded agentruntime.ProjectSpec
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Agents[0].HistoryPolicy == nil ||
		decoded.Agents[0].HistoryPolicy.MaxRuns != 5 ||
		decoded.Agents[0].HistoryPolicy.MaxTurns != 3 ||
		decoded.Agents[0].HistoryPolicy.MaxMessages != 100 {
		t.Fatalf("decoded policy = %+v", decoded.Agents[0].HistoryPolicy)
	}
	// nil policy omits the field entirely.
	spec.Agents[0].HistoryPolicy = nil
	raw, err = json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal nil policy: %v", err)
	}
	if strings.Contains(string(raw), "history_policy") {
		t.Fatalf("nil HistoryPolicy should be omitted: %s", raw)
	}
}

func TestBuildWiresContextWindowPolicy(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion,
		ID:            "window", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "assistant", ModelRef: "chat",
			ContextWindow: &agentruntime.ContextWindowSpec{
				MaxInputTokens: 1000,
				TriggerRatio:   0.8,
				TargetRatio:    0.7,
				MinRecentTurns: 2,
			},
		}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Models: map[string]models.Model{"chat": mock.New()},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := project.Agent("assistant"); !ok {
		t.Fatal("agent was not assembled")
	}
}

func TestBuildWiresContextWindowWithInjectorRefs(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion,
		ID:            "window-refs", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "assistant", ModelRef: "chat",
			ContextWindow: &agentruntime.ContextWindowSpec{
				MaxInputTokens: 2000,
			},
			TokenCounterRef: "my-counter",
			SummarizerRef:   "my-summarizer",
		}},
	}
	counter := &fakeCounter{}
	summarizer := &fakeSummarizer{}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Models:        map[string]models.Model{"chat": mock.New()},
		TokenCounters: map[string]agent.TokenCounter{"my-counter": counter},
		Summarizers:   map[string]agent.Summarizer{"my-summarizer": summarizer},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := project.Agent("assistant"); !ok {
		t.Fatal("agent was not assembled")
	}
}

func TestContextWindowSpecJSONRoundTrip(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		ID: "json3", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "assistant", ModelRef: "chat",
			ContextWindow: &agentruntime.ContextWindowSpec{
				MaxInputTokens: 4096,
				TriggerRatio:   0.85,
				TargetRatio:    0.75,
				MinRecentTurns: 3,
			},
		}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `"context_window"`) ||
		!strings.Contains(got, `"max_input_tokens":4096`) {
		t.Fatalf("serialized spec = %s", got)
	}
	var decoded agentruntime.ProjectSpec
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Agents[0].ContextWindow == nil ||
		decoded.Agents[0].ContextWindow.MaxInputTokens != 4096 ||
		decoded.Agents[0].ContextWindow.MinRecentTurns != 3 {
		t.Fatalf("decoded context_window = %+v", decoded.Agents[0].ContextWindow)
	}
}

func TestDecodeSpecV3AcceptsContextWindow(t *testing.T) {
	input := `{"schema_version":3,"id":"v3","version":"v1","agents":[{"id":"a","model_ref":"m","context_window":{"max_input_tokens":1000}}]}`
	decoded, err := agentruntime.DecodeSpec(strings.NewReader(input))
	if err != nil {
		t.Fatalf("DecodeSpec v3: %v", err)
	}
	if decoded.Agents[0].ContextWindow == nil || decoded.Agents[0].ContextWindow.MaxInputTokens != 1000 {
		t.Fatalf("decoded context_window = %+v", decoded.Agents[0].ContextWindow)
	}
}

func TestDecodeSpecV1V2RejectV3Fields(t *testing.T) {
	for _, v := range []int{1, 2} {
		input := fmt.Sprintf(`{"schema_version":%d,"id":"v%d","version":"v1","agents":[{"id":"a","model_ref":"m","context_window":{"max_input_tokens":1000}}]}`, v, v)
		_, err := agentruntime.DecodeSpec(strings.NewReader(input))
		var buildErr *agentruntime.BuildError
		if !errors.As(err, &buildErr) || len(buildErr.Diagnostics) != 1 {
			t.Fatalf("v%d: expected 1 diagnostic, got %v", v, err)
		}
		if buildErr.Diagnostics[0].Code != agentruntime.DiagnosticInvalidValue {
			t.Fatalf("v%d: expected InvalidValue, got %v", v, buildErr.Diagnostics[0])
		}
	}
}

func TestContextWindowSpecV3RoundTrip(t *testing.T) {
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion,
		ID:            "rt", Version: "v1",
		Agents: []agentruntime.AgentSpec{{
			ID: "a", ModelRef: "m",
			ContextWindow: &agentruntime.ContextWindowSpec{
				MaxInputTokens: 4096,
				TriggerRatio:   0.85,
				TargetRatio:    0.75,
				MinRecentTurns: 3,
			},
		}},
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	decoded, err := agentruntime.DecodeSpec(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if decoded.Agents[0].ContextWindow == nil ||
		decoded.Agents[0].ContextWindow.MaxInputTokens != 4096 ||
		decoded.Agents[0].ContextWindow.MinRecentTurns != 3 {
		t.Fatalf("decoded = %+v", decoded.Agents[0].ContextWindow)
	}
}

// fakeCounter is a runtime test double for agent.TokenCounter.
type fakeCounter struct{ perMessage int }

func (c *fakeCounter) Count(ctx context.Context, msgs []models.Message) (int, error) {
	return len(msgs) * c.perMessage, nil
}

// fakeSummarizer is a runtime test double for agent.Summarizer.
type fakeSummarizer struct{ summary string }

func (s *fakeSummarizer) Summarize(ctx context.Context, msgs []models.Message) (string, error) {
	return s.summary, nil
}

func TestBuildClassifiesEmptyTeamMemberAsRequired(t *testing.T) {
	_, err := agentruntime.Build(agentruntime.ProjectSpec{
		ID: "team-member", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "member", ModelRef: "chat"}},
		Teams:  []agentruntime.TeamSpec{{ID: "team", ModelRef: "chat", MemberRefs: []string{""}}},
	}, agentruntime.Dependencies{Models: map[string]models.Model{"chat": mock.New()}})
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build error = %v", err)
	}
	for _, diagnostic := range buildErr.Diagnostics {
		if diagnostic.Path == "teams[0].member_refs[0]" {
			if diagnostic.Code != agentruntime.DiagnosticRequired {
				t.Fatalf("diagnostic = %+v", diagnostic)
			}
			return
		}
	}
	t.Fatalf("missing member diagnostic: %+v", buildErr.Diagnostics)
}
