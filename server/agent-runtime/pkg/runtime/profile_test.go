package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/middleware"
	agentruntime "github.com/linxinhong/xingding-agent-runtime/pkg/runtime"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

func TestProfileValidity(t *testing.T) {
	if !agentruntime.ProfileManaged.Valid() || !agentruntime.ProfileLegacyEmbedded.Valid() {
		t.Fatalf("frozen profiles must be valid")
	}
	if (agentruntime.Profile("")).Valid() {
		t.Fatalf("zero profile must be invalid")
	}
	if (agentruntime.Profile("weird")).Valid() {
		t.Fatalf("unknown profile must be invalid")
	}
}

type staticMapper struct{ principal execution.Principal }

func (m staticMapper) PrincipalFor(ctx context.Context, inbound any) (execution.Principal, error) {
	return m.principal, nil
}

func TestBuildManagedRequiresManagedProfile(t *testing.T) {
	_, err := agentruntime.BuildManaged(agentruntime.ProjectSpec{},
		agentruntime.Dependencies{}, agentruntime.ManagedOptions{})
	if err == nil || !strings.Contains(err.Error(), "requires ProfileManaged") {
		t.Fatalf("err = %v, want profile rejection", err)
	}
}

func TestBuildManagedRequiresIdentityMapper(t *testing.T) {
	_, err := agentruntime.BuildManaged(agentruntime.ProjectSpec{},
		agentruntime.Dependencies{}, agentruntime.ManagedOptions{Profile: agentruntime.ProfileManaged})
	if err == nil || !strings.Contains(err.Error(), "IdentityMapper") {
		t.Fatalf("err = %v, want IdentityMapper rejection", err)
	}
}

// stubToolService satisfies service.ToolService for managed build tests.
type stubToolService struct{}

func (stubToolService) List(context.Context, service.ToolQuery) (service.ToolPage, error) {
	return service.ToolPage{}, nil
}
func (stubToolService) Describe(context.Context, string) (catalog.ToolDescriptor, error) {
	return catalog.ToolDescriptor{}, service.ErrUnknownTool
}
func (stubToolService) Prepare(context.Context, service.PrepareRequest) (service.PreparedExecution, error) {
	return service.PreparedExecution{}, service.ErrUnknownTool
}
func (stubToolService) Execute(context.Context, service.PlanRef) (service.ToolResult, error) {
	return service.ToolResult{}, service.ErrUnknownTool
}

// managedDeps returns Dependencies with a governed ToolService plus
// auth+isolation enabled so managed build constraint tests pass the guards.
func managedDeps() agentruntime.Dependencies {
	return agentruntime.Dependencies{
		ToolService: stubToolService{},
		Authorization: middleware.AuthorizationConfig{
			Enabled:       true,
			SecurityKey:   "test-key",
			UserIsolation: true,
		},
	}
}

func TestBuildManagedRequiresToolService(t *testing.T) {
	mapper := staticMapper{principal: execution.Principal{Subject: "a", Namespace: "ns"}}
	deps := managedDeps()
	deps.ToolService = nil
	_, err := agentruntime.BuildManaged(agentruntime.ProjectSpec{
		SchemaVersion: 1, ID: "p", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "a", ModelRef: "chat"}},
	}, deps, agentruntime.ManagedOptions{
		Profile:        agentruntime.ProfileManaged,
		IdentityMapper: mapper,
	})
	if err == nil || !strings.Contains(err.Error(), "governed ToolService") {
		t.Fatalf("err = %v, want ToolService rejection", err)
	}
}

// stubResolver resolves every ref to the same fake model.
type stubResolver struct{ model toolsModel }

func (r stubResolver) Resolve(ctx context.Context, ref string) (models.Model, error) {
	return r.model, nil
}

type toolsModel struct{}

func (toolsModel) ID() string       { return "m" }
func (toolsModel) Provider() string { return "fake" }
func (toolsModel) Response(ctx context.Context, msgs []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	return &models.ModelResponse{Content: "ok"}, nil
}
func (toolsModel) ResponseStream(ctx context.Context, msgs []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	ch := make(chan models.StreamEvent, 1)
	ch <- models.StreamEvent{Done: true}
	close(ch)
	return ch, nil
}

func TestBuildManagedWithModelResolver(t *testing.T) {
	mapper := staticMapper{principal: execution.Principal{Subject: "a", Namespace: "ns"}}
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.CurrentSchemaVersion, ID: "p", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "a", ModelRef: "chat", Instructions: "x"}},
	}
	project, err := agentruntime.BuildManaged(spec, managedDeps(), agentruntime.ManagedOptions{
		Profile:        agentruntime.ProfileManaged,
		IdentityMapper: mapper,
		ModelResolver:  stubResolver{},
	})
	if err != nil {
		t.Fatalf("BuildManaged: %v", err)
	}
	if project == nil {
		t.Fatalf("project not built")
	}
	if _, ok := project.Agent("a"); !ok {
		t.Fatalf("agent not built")
	}
}

func TestBuildManagedRejectsModelsWithResolver(t *testing.T) {
	mapper := staticMapper{principal: execution.Principal{Subject: "a", Namespace: "ns"}}
	deps := managedDeps()
	deps.Models = map[string]models.Model{"chat": toolsModel{}}
	_, err := agentruntime.BuildManaged(agentruntime.ProjectSpec{}, deps, agentruntime.ManagedOptions{
		Profile:        agentruntime.ProfileManaged,
		IdentityMapper: mapper,
		ModelResolver:  stubResolver{},
	})
	if err == nil || !strings.Contains(err.Error(), "rejects Dependencies.Models") {
		t.Fatalf("err = %v, want models rejection", err)
	}
}

func TestBuildManagedRequiresAuthentication(t *testing.T) {
	mapper := staticMapper{principal: execution.Principal{Subject: "a", Namespace: "ns"}}
	deps := managedDeps()
	deps.Authorization.Enabled = false
	_, err := agentruntime.BuildManaged(agentruntime.ProjectSpec{}, deps, agentruntime.ManagedOptions{
		Profile:        agentruntime.ProfileManaged,
		IdentityMapper: mapper,
	})
	if err == nil || !strings.Contains(err.Error(), "requires authentication") {
		t.Fatalf("err = %v, want authentication rejection", err)
	}
}

func TestBuildManagedRequiresUserIsolation(t *testing.T) {
	mapper := staticMapper{principal: execution.Principal{Subject: "a", Namespace: "ns"}}
	deps := managedDeps()
	deps.Authorization.UserIsolation = false
	_, err := agentruntime.BuildManaged(agentruntime.ProjectSpec{}, deps, agentruntime.ManagedOptions{
		Profile:        agentruntime.ProfileManaged,
		IdentityMapper: mapper,
	})
	if err == nil || !strings.Contains(err.Error(), "requires UserIsolation") {
		t.Fatalf("err = %v, want UserIsolation rejection", err)
	}
}
