package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	agentos "github.com/linxinhong/xingding-agent-runtime/pkg/os"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/skills"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

// Dependencies contains executable values and deployment configuration. It is
// deliberately separate from ProjectSpec so specs remain serializable and do
// not contain credentials.
type Dependencies struct {
	DB db.DB
	// Admission is the optional process-local controller shared by all Agents
	// and AgentOS background execution assembled from this dependency graph.
	Admission *run.AdmissionController
	Models    map[string]models.Model
	Tools     map[string]tools.Tool
	// ToolService is the governed execution entry (RC-05). When set, agents
	// route tool invocations through it (authorization/approval/audit) while
	// Tools above still supplies model-visible schemas. Managed builds
	// require it.
	ToolService   service.ToolService
	Knowledge     map[string]knowledge.Knowledge
	Executors     map[string]workflow.Executor
	Predicates    map[string]workflow.Predicate
	Authorization agentos.AuthorizationConfig
	SecurityKey   string
	Hooks         map[string]*agent.Hooks
	TokenCounters map[string]agent.TokenCounter
	Summarizers   map[string]agent.Summarizer
	Memories      map[string]MemoryDependency
}

// MemoryDependency contains executable trusted values which must never be
// serialized into ProjectSpec.
type MemoryDependency struct {
	Module         *memory.Module
	SourceResolver memory.ProposalSourceResolver
}

// Build validates spec references and atomically assembles the complete
// executable project graph.
func Build(spec ProjectSpec, deps Dependencies) (*Project, error) {
	if spec.SchemaVersion == 0 {
		spec.SchemaVersion = LegacySchemaVersion
	}
	var err error
	spec, err = snapshotLimitedWorkflowDefinitions(spec)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, diagnosticError(Diagnostic{Code: DiagnosticNotSerializable, Message: fmt.Sprintf("project spec is not JSON serializable: %v", err)}, err)
	}
	var snapshot ProjectSpec
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, diagnosticError(Diagnostic{Code: DiagnosticNotSerializable, Message: fmt.Sprintf("snapshot project spec: %v", err)}, err)
	}
	spec = snapshot
	canonical, err := json.Marshal(snapshot)
	if err != nil {
		return nil, diagnosticError(Diagnostic{Code: DiagnosticNotSerializable, Message: fmt.Sprintf("canonicalize project spec: %v", err)}, err)
	}
	specHash := fmt.Sprintf("%x", sha256.Sum256(canonical))
	if diagnostics := validateProject(spec, deps); len(diagnostics) > 0 {
		return nil, &BuildError{Diagnostics: diagnostics}
	}
	authorization := deps.Authorization
	securityKey := deps.SecurityKey
	if authorization.SecurityKey != "" {
		securityKey = authorization.SecurityKey
	}
	if securityKey != "" {
		authorization.Enabled = true
	}

	knowledges, err := buildKnowledges(spec.Knowledges, deps.Knowledge)
	if err != nil {
		return nil, constructionError("knowledges", err)
	}
	memoryRuntime, err := buildMemory(spec, deps)
	if err != nil {
		return nil, constructionError("memory", err)
	}
	agents, err := buildAgents(spec.SchemaVersion, spec.Agents, knowledges, memoryRuntime, deps)
	if err != nil {
		return nil, constructionError("agents", err)
	}
	teams, err := buildTeams(spec.Teams, agents, deps)
	if err != nil {
		return nil, constructionError("teams", err)
	}
	registry, err := buildWorkflowRegistry(agents, teams, deps)
	if err != nil {
		return nil, constructionError("dependencies", err)
	}
	workflows, err := buildWorkflows(spec.Workflows, registry, deps.DB)
	if err != nil {
		return nil, constructionError("workflows", err)
	}

	osRuntime, err := agentos.New(agentos.AgentOSConfig{
		Project: &agentos.ProjectInfo{ID: spec.ID, Name: projectName(spec), Version: spec.Version, SchemaVersion: spec.SchemaVersion, SpecHash: specHash},
		Agents:  agents, Teams: teams, Workflows: workflows, Knowledges: knowledges, Models: deps.Models,
		DB: deps.DB, Admission: deps.Admission, Authorization: authorization, SecurityKey: securityKey, Memory: memoryRuntime,
	})
	if err != nil {
		return nil, constructionError("agent_os", err)
	}
	var builtMemory *memory.Module
	if memoryRuntime != nil {
		builtMemory = memoryRuntime.Module
	}
	return &Project{spec: spec, agents: agents, teams: teams, workflows: workflows, knowledges: knowledges, memory: builtMemory, agentOS: osRuntime, specHash: specHash}, nil
}

// snapshotLimitedWorkflowDefinitions bounds and normalizes opted-in
// programmatic definitions before the project-wide JSON snapshot. Other
// ProjectSpec fields remain trusted host configuration and retain the existing
// serialization contract; Run payload admission is enforced by Workflow.
func snapshotLimitedWorkflowDefinitions(spec ProjectSpec) (ProjectSpec, error) {
	var diagnostics []Diagnostic
	add := func(code DiagnosticCode, path, resourceID, message string) {
		diagnostics = append(diagnostics, Diagnostic{Code: code, Path: path, ComponentID: resourceID, Message: message})
	}
	workflows := append([]WorkflowSpec(nil), spec.Workflows...)
	for i := range workflows {
		item := &workflows[i]
		if item.Limits == nil {
			continue
		}
		base := fmt.Sprintf("workflows[%d].limits", i)
		if validateWorkflowLimitFields(item.Limits, base, item.Definition.ID, add) {
			continue
		}
		if spec.SchemaVersion < SchemaVersion5 {
			add(DiagnosticInvalidValue, base, item.Definition.ID, "limits requires schema_version 5")
			continue
		}
		definition, err := workflow.SnapshotLimitedDefinition(item.Definition, *item.Limits)
		if err != nil {
			path := workflowLimitDiagnosticPath(base, err)
			if path == base {
				path = fmt.Sprintf("workflows[%d].definition", i)
			}
			add(DiagnosticInvalidWorkflow, path, item.Definition.ID, err.Error())
			continue
		}
		item.Definition = definition
	}
	if len(diagnostics) > 0 {
		return ProjectSpec{}, &BuildError{Diagnostics: diagnostics}
	}
	spec.Workflows = workflows
	return spec, nil
}

func projectName(spec ProjectSpec) string {
	if spec.Name != "" {
		return spec.Name
	}
	return spec.ID
}

func buildAgents(schemaVersion int, specs []AgentSpec, knowledges map[string]knowledge.Knowledge, memoryRuntime *agentos.MemoryConfig, deps Dependencies) (map[string]*agent.Agent, error) {
	result := make(map[string]*agent.Agent, len(specs))
	for _, spec := range specs {
		model := deps.Models[spec.ModelRef]
		agentTools := make([]tools.Tool, 0, len(spec.ToolRefs))
		for _, ref := range spec.ToolRefs {
			agentTools = append(agentTools, deps.Tools[ref])
		}
		// Load skills (schema v4): explicit skill dirs plus recursively
		// discovered roots. Load failures are fail-fast so a broken skill
		// cannot silently disable the feature.
		var agentSkills []*skills.Skill
		if len(spec.SkillDirs) > 0 || len(spec.SkillRoots) > 0 {
			loaded, err := skills.LoadDirs(spec.SkillDirs)
			if err != nil {
				return nil, fmt.Errorf("runtime: build agent %q: %w", spec.ID, err)
			}
			discovered, err := skills.Discover(spec.SkillRoots)
			if err != nil {
				return nil, fmt.Errorf("runtime: build agent %q: %w", spec.ID, err)
			}
			agentSkills = append(agentSkills, loaded...)
			agentSkills = append(agentSkills, discovered...)
		}
		contextProviders := buildContextProviders(spec.HistoryPolicy, spec.ContextWindow, spec.TokenCounterRef, spec.SummarizerRef, deps)
		if memoryRuntime != nil && memoryRuntime.Recall != nil {
			provider, err := agent.NewMemoryContextProvider(agent.MemoryContextProviderConfig{
				Recaller: memoryRuntime.Module,
				AuthorityResolver: agent.MemoryAuthorityResolverFunc(func(ctx context.Context) (memory.Principal, memory.Scope, error) {
					principal, err := service.PrincipalFromContext(ctx)
					if err != nil {
						return memory.Principal{}, memory.Scope{}, err
					}
					memoryPrincipal := memory.Principal{Subject: principal.Subject, Namespace: principal.Namespace, Issuer: principal.Issuer, AuthzEpoch: principal.AuthzEpoch}
					return memoryPrincipal, memory.Scope{Namespace: principal.Namespace, ProjectID: memoryRuntime.ProjectID, SubjectID: principal.Subject, CollectionID: memoryRuntime.CollectionID}, nil
				}),
				Limit: memoryRuntime.Recall.Limit, Kinds: memoryRuntime.Recall.Kinds, Topics: memoryRuntime.Recall.Topics,
				FailureMode: memoryRuntime.Recall.FailureMode,
			})
			if err != nil {
				return nil, fmt.Errorf("runtime: build agent %q memory: %w", spec.ID, err)
			}
			contextProviders = append(contextProviders, provider)
		}
		if len(agentSkills) > 0 {
			contextProviders = append(contextProviders, &agent.SkillContextProvider{Skills: agentSkills})
			agentTools = append(agentTools, &agent.LoadSkillTool{Skills: agentSkills})
		}
		var agentKnowledge knowledge.Knowledge
		if spec.KnowledgeRef != "" {
			agentKnowledge = knowledges[spec.KnowledgeRef]
		}
		maxToolCalls := spec.MaxToolCalls
		toolTimeout := time.Duration(spec.ToolTimeoutMS) * time.Millisecond
		runTimeout := time.Duration(spec.RunTimeoutMS) * time.Millisecond
		if schemaVersion >= SchemaVersion2 {
			if maxToolCalls == 0 {
				maxToolCalls = 6
			}
			if toolTimeout == 0 {
				toolTimeout = 30 * time.Second
			}
			if runTimeout == 0 {
				runTimeout = 60 * time.Second
			}
		}
		built, err := agent.New(agent.AgentConfig{
			ID: spec.ID, Name: spec.Name, Instructions: spec.Instructions, Model: model,
			Tools: agentTools, ToolService: deps.ToolService, Knowledge: agentKnowledge, DB: deps.DB,
			MaxToolCalls: maxToolCalls, Temperature: spec.Temperature, MaxTokens: spec.MaxTokens,
			SessionState: cloneState(spec.SessionState), OutputSchema: spec.OutputSchema,
			ToolTimeout: toolTimeout, RunTimeout: runTimeout, MaxConcurrentRuns: spec.MaxConcurrentRuns, Admission: deps.Admission,
			Hooks:            deps.Hooks[spec.HooksRef],
			ContextProviders: contextProviders,
		})
		if err != nil {
			return nil, fmt.Errorf("runtime: build agent %q: %w", spec.ID, err)
		}
		result[spec.ID] = built
	}
	return result, nil
}

func buildTeams(specs []TeamSpec, agents map[string]*agent.Agent, deps Dependencies) (map[string]*team.Team, error) {
	result := make(map[string]*team.Team, len(specs))
	for _, spec := range specs {
		model := deps.Models[spec.ModelRef]
		members := make([]*agent.Agent, 0, len(spec.MemberRefs))
		for _, ref := range spec.MemberRefs {
			members = append(members, agents[ref])
		}
		built, err := team.New(team.TeamConfig{
			ID: spec.ID, Name: spec.Name, Model: model, Members: members,
			Instructions: spec.Instructions, DB: deps.DB, MaxIterations: spec.MaxIterations,
			SessionState: cloneState(spec.SessionState),
		})
		if err != nil {
			return nil, fmt.Errorf("runtime: build team %q: %w", spec.ID, err)
		}
		result[spec.ID] = built
	}
	return result, nil
}

func buildWorkflowRegistry(agents map[string]*agent.Agent, teams map[string]*team.Team, deps Dependencies) (*workflow.Registry, error) {
	registry := workflow.NewRegistry()
	for ref, executor := range deps.Executors {
		if err := registry.RegisterExecutor(ref, executor); err != nil {
			return nil, fmt.Errorf("runtime: register workflow executor %q: %w", ref, err)
		}
	}
	for ref, predicate := range deps.Predicates {
		if err := registry.RegisterPredicate(ref, predicate); err != nil {
			return nil, fmt.Errorf("runtime: register workflow predicate %q: %w", ref, err)
		}
	}
	for agentID, value := range agents {
		if err := registry.RegisterExecutor(AgentExecutorRef(agentID), workflow.AgentExecutor(value)); err != nil {
			return nil, err
		}
	}
	for teamID, value := range teams {
		if err := registry.RegisterExecutor(TeamExecutorRef(teamID), workflow.TeamExecutor(value)); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func buildWorkflows(specs []WorkflowSpec, registry *workflow.Registry, store db.WorkflowStore) (map[string]*workflow.Workflow, error) {
	result := make(map[string]*workflow.Workflow, len(specs))
	for _, spec := range specs {
		id := spec.Definition.ID
		built, err := workflow.New(workflow.Config{
			Definition: spec.Definition, Registry: registry, DB: store,
			SessionState: cloneState(spec.SessionState), Limits: spec.Limits,
		})
		if err != nil {
			return nil, fmt.Errorf("runtime: build workflow %q: %w", id, err)
		}
		result[id] = built
	}
	return result, nil
}

func buildKnowledges(specs []KnowledgeSpec, dependencies map[string]knowledge.Knowledge) (map[string]knowledge.Knowledge, error) {
	result := make(map[string]knowledge.Knowledge, len(specs))
	for _, spec := range specs {
		result[spec.ID] = dependencies[spec.Ref]
	}
	return result, nil
}

func cloneState(state map[string]any) map[string]any {
	if state == nil {
		return nil
	}
	clone := make(map[string]any, len(state))
	for key, value := range state {
		clone[key] = value
	}
	return clone
}

func buildContextProviders(historySpec *HistoryPolicySpec, windowSpec *ContextWindowSpec, tokenCounterRef, summarizerRef string, deps Dependencies) []agent.ContextProvider {
	var providers []agent.ContextProvider
	if historySpec != nil {
		providers = append(providers, &agent.HistoryContextProvider{
			Policy: &agent.HistoryPolicy{
				MaxRuns:     historySpec.MaxRuns,
				MaxTurns:    historySpec.MaxTurns,
				MaxMessages: historySpec.MaxMessages,
			},
		})
	}
	if windowSpec != nil {
		var counter agent.TokenCounter
		var summarizer agent.Summarizer
		if tokenCounterRef != "" {
			counter = deps.TokenCounters[tokenCounterRef]
		}
		if summarizerRef != "" {
			summarizer = deps.Summarizers[summarizerRef]
		}
		providers = append(providers, &agent.CompactionContextProvider{
			Policy: &agent.ContextWindowPolicy{
				MaxInputTokens: windowSpec.MaxInputTokens,
				TriggerRatio:   windowSpec.TriggerRatio,
				TargetRatio:    windowSpec.TargetRatio,
				MinRecentTurns: windowSpec.MinRecentTurns,
			},
			Counter:    counter,
			Summarizer: summarizer,
		})
	}
	return providers
}
