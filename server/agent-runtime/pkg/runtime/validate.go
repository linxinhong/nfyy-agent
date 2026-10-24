package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

func validateProject(spec ProjectSpec, deps Dependencies) []Diagnostic {
	var diagnostics []Diagnostic
	add := func(code DiagnosticCode, path, componentID, message string) {
		diagnostics = append(diagnostics, Diagnostic{Code: code, Path: path, ComponentID: componentID, Message: message})
	}

	if !isSupportedSchemaVersion(spec.SchemaVersion) {
		add(DiagnosticUnsupportedSchemaVersion, "schema_version", spec.ID, fmt.Sprintf("schema_version %d is not supported", spec.SchemaVersion))
	}
	if spec.ID == "" {
		add(DiagnosticRequired, "id", "", "project id is required")
	}
	if spec.Version == "" {
		add(DiagnosticRequired, "version", spec.ID, "project version is required")
	}
	if deps.SecurityKey != "" && deps.Authorization.SecurityKey != "" && deps.SecurityKey != deps.Authorization.SecurityKey {
		add(DiagnosticConflictingConfiguration, "dependencies.security_key", spec.ID, "Dependencies.SecurityKey conflicts with Authorization.SecurityKey")
	}
	validateMemorySpec(spec, deps, add)

	knowledgeIDs := make(map[string]struct{}, len(spec.Knowledges))
	for i, item := range spec.Knowledges {
		base := fmt.Sprintf("knowledges[%d]", i)
		if item.ID == "" {
			add(DiagnosticRequired, base+".id", "", "knowledge id is required")
		} else if _, exists := knowledgeIDs[item.ID]; exists {
			add(DiagnosticDuplicate, base+".id", item.ID, fmt.Sprintf("duplicate knowledge id %q", item.ID))
		} else {
			knowledgeIDs[item.ID] = struct{}{}
		}
		value, ok := deps.Knowledge[item.Ref]
		if item.Ref == "" {
			add(DiagnosticRequired, base+".ref", item.ID, "knowledge dependency reference is required")
		} else if !ok || value == nil {
			add(DiagnosticUnknownReference, base+".ref", item.ID, fmt.Sprintf("knowledge dependency %q is not registered", item.Ref))
		}
	}

	agentIDs := make(map[string]struct{}, len(spec.Agents))
	for i, item := range spec.Agents {
		base := fmt.Sprintf("agents[%d]", i)
		if item.ID == "" {
			add(DiagnosticRequired, base+".id", "", "agent id is required")
		} else if _, exists := agentIDs[item.ID]; exists {
			add(DiagnosticDuplicate, base+".id", item.ID, fmt.Sprintf("duplicate agent id %q", item.ID))
		} else {
			agentIDs[item.ID] = struct{}{}
		}
		if item.MaxToolCalls < 0 {
			add(DiagnosticInvalidValue, base+".max_tool_calls", item.ID, "max_tool_calls must not be negative")
		}
		if spec.SchemaVersion == LegacySchemaVersion {
			for _, field := range []struct {
				used bool
				path string
			}{
				{item.OutputSchema != nil, "output_schema"},
				{item.ToolTimeoutMS != 0, "tool_timeout_ms"},
				{item.RunTimeoutMS != 0, "run_timeout_ms"},
				{item.MaxConcurrentRuns != 0, "max_concurrent_runs"},
				{item.HooksRef != "", "hooks_ref"},
			} {
				if field.used {
					add(DiagnosticInvalidValue, base+"."+field.path, item.ID, field.path+" requires schema_version 2")
				}
			}
		}
		if item.ToolTimeoutMS < 0 {
			add(DiagnosticInvalidValue, base+".tool_timeout_ms", item.ID, "tool_timeout_ms must not be negative")
		}
		if item.RunTimeoutMS < 0 {
			add(DiagnosticInvalidValue, base+".run_timeout_ms", item.ID, "run_timeout_ms must not be negative")
		}
		if item.MaxConcurrentRuns < 0 {
			add(DiagnosticInvalidValue, base+".max_concurrent_runs", item.ID, "max_concurrent_runs must not be negative")
		}
		if item.HistoryPolicy != nil {
			hp := base + ".history_policy"
			if item.HistoryPolicy.MaxRuns < 0 {
				add(DiagnosticInvalidValue, hp+".max_runs", item.ID, "max_runs must not be negative")
			}
			if item.HistoryPolicy.MaxTurns < 0 {
				add(DiagnosticInvalidValue, hp+".max_turns", item.ID, "max_turns must not be negative")
			}
			if item.HistoryPolicy.MaxMessages < 0 {
				add(DiagnosticInvalidValue, hp+".max_messages", item.ID, "max_messages must not be negative")
			}
		}
		// P0-1: v3-only fields.
		if spec.SchemaVersion < SchemaVersion3 {
			if item.ContextWindow != nil {
				add(DiagnosticInvalidValue, base+".context_window", item.ID, "context_window requires schema_version 3")
			}
			if item.TokenCounterRef != "" {
				add(DiagnosticInvalidValue, base+".token_counter_ref", item.ID, "token_counter_ref requires schema_version 3")
			}
			if item.SummarizerRef != "" {
				add(DiagnosticInvalidValue, base+".summarizer_ref", item.ID, "summarizer_ref requires schema_version 3")
			}
		}
		// Skills: v4-only fields.
		if spec.SchemaVersion < SchemaVersion4 {
			if len(item.SkillDirs) > 0 {
				add(DiagnosticInvalidValue, base+".skill_dirs", item.ID, "skill_dirs requires schema_version 4")
			}
			if len(item.SkillRoots) > 0 {
				add(DiagnosticInvalidValue, base+".skill_roots", item.ID, "skill_roots requires schema_version 4")
			}
		}
		if item.ContextWindow != nil {
			cw := base + ".context_window"
			if item.ContextWindow.MaxInputTokens < 0 {
				add(DiagnosticInvalidValue, cw+".max_input_tokens", item.ID, "max_input_tokens must not be negative")
			}
			if item.ContextWindow.MinRecentTurns < 0 {
				add(DiagnosticInvalidValue, cw+".min_recent_turns", item.ID, "min_recent_turns must not be negative")
			}
			// Reject negative ratios first (0 = default, negative = invalid).
			if item.ContextWindow.TriggerRatio < 0 {
				add(DiagnosticInvalidValue, cw+".trigger_ratio", item.ID, "trigger_ratio must not be negative")
			}
			if item.ContextWindow.TargetRatio < 0 {
				add(DiagnosticInvalidValue, cw+".target_ratio", item.ID, "target_ratio must not be negative")
			}
			// Apply defaults (only when == 0), then validate range and ordering.
			tr := item.ContextWindow.TriggerRatio
			tg := item.ContextWindow.TargetRatio
			if tr == 0 {
				tr = 0.8
			}
			if tg == 0 {
				tg = 0.7
			}
			// After defaults, ratios must be in (0, 1].
			if tr > 1 {
				add(DiagnosticInvalidValue, cw+".trigger_ratio", item.ID, "trigger_ratio must be in (0, 1]")
			}
			if tg > 1 {
				add(DiagnosticInvalidValue, cw+".target_ratio", item.ID, "target_ratio must be in (0, 1]")
			}
			// After applying defaults, target must be < trigger.
			if tg >= tr {
				add(DiagnosticInvalidValue, cw+".target_ratio", item.ID, "target_ratio must be less than trigger_ratio")
			}
		}
		// P0-6: refs present but context_window nil → invalid.
		if item.TokenCounterRef != "" && item.ContextWindow == nil {
			add(DiagnosticInvalidValue, base+".token_counter_ref", item.ID, "token_counter_ref requires context_window")
		}
		if item.SummarizerRef != "" && item.ContextWindow == nil {
			add(DiagnosticInvalidValue, base+".summarizer_ref", item.ID, "summarizer_ref requires context_window")
		}
		// P0-6: missing ref → unknown-reference.
		if item.TokenCounterRef != "" {
			if _, ok := deps.TokenCounters[item.TokenCounterRef]; !ok {
				add(DiagnosticUnknownReference, base+".token_counter_ref", item.ID, fmt.Sprintf("token_counter %q is not registered", item.TokenCounterRef))
			}
		}
		if item.SummarizerRef != "" {
			if _, ok := deps.Summarizers[item.SummarizerRef]; !ok {
				add(DiagnosticUnknownReference, base+".summarizer_ref", item.ID, fmt.Sprintf("summarizer %q is not registered", item.SummarizerRef))
			}
		}
		if item.OutputSchema != nil && spec.SchemaVersion >= SchemaVersion2 {
			if err := agent.ValidateOutputSchema(item.OutputSchema); err != nil {
				add(DiagnosticInvalidSchema, base+".output_schema", item.ID, err.Error())
			}
		}
		if item.HooksRef != "" {
			hooks, ok := deps.Hooks[item.HooksRef]
			if !ok || hooks == nil {
				add(DiagnosticUnknownReference, base+".hooks_ref", item.ID, fmt.Sprintf("hooks %q are not registered", item.HooksRef))
			}
		}
		if item.MaxTokens != nil && *item.MaxTokens <= 0 {
			add(DiagnosticInvalidValue, base+".max_tokens", item.ID, "max_tokens must be positive")
		}
		model, ok := deps.Models[item.ModelRef]
		if item.ModelRef == "" {
			add(DiagnosticRequired, base+".model_ref", item.ID, "model reference is required")
		} else if !ok || model == nil {
			add(DiagnosticUnknownReference, base+".model_ref", item.ID, fmt.Sprintf("model %q is not registered", item.ModelRef))
		}
		for j, ref := range item.ToolRefs {
			tool, ok := deps.Tools[ref]
			if ref == "" {
				add(DiagnosticRequired, fmt.Sprintf("%s.tool_refs[%d]", base, j), item.ID, "tool reference is required")
			} else if !ok || tool == nil {
				add(DiagnosticUnknownReference, fmt.Sprintf("%s.tool_refs[%d]", base, j), item.ID, fmt.Sprintf("tool %q is not registered", ref))
			}
		}
		if item.KnowledgeRef != "" {
			if _, ok := knowledgeIDs[item.KnowledgeRef]; !ok {
				add(DiagnosticUnknownReference, base+".knowledge_ref", item.ID, fmt.Sprintf("project knowledge %q is not defined", item.KnowledgeRef))
			}
		}
	}

	teamIDs := make(map[string]struct{}, len(spec.Teams))
	for i, item := range spec.Teams {
		base := fmt.Sprintf("teams[%d]", i)
		if item.ID == "" {
			add(DiagnosticRequired, base+".id", "", "team id is required")
		} else if _, exists := teamIDs[item.ID]; exists {
			add(DiagnosticDuplicate, base+".id", item.ID, fmt.Sprintf("duplicate team id %q", item.ID))
		} else {
			teamIDs[item.ID] = struct{}{}
		}
		if item.MaxIterations < 0 {
			add(DiagnosticInvalidValue, base+".max_iterations", item.ID, "max_iterations must not be negative")
		}
		model, ok := deps.Models[item.ModelRef]
		if item.ModelRef == "" {
			add(DiagnosticRequired, base+".model_ref", item.ID, "model reference is required")
		} else if !ok || model == nil {
			add(DiagnosticUnknownReference, base+".model_ref", item.ID, fmt.Sprintf("model %q is not registered", item.ModelRef))
		}
		if len(item.MemberRefs) == 0 {
			add(DiagnosticRequired, base+".member_refs", item.ID, "at least one team member is required")
		}
		for j, ref := range item.MemberRefs {
			path := fmt.Sprintf("%s.member_refs[%d]", base, j)
			if ref == "" {
				add(DiagnosticRequired, path, item.ID, "agent member reference is required")
			} else if _, ok := agentIDs[ref]; !ok {
				add(DiagnosticUnknownReference, path, item.ID, fmt.Sprintf("agent %q is not defined", ref))
			}
		}
	}

	references := validationReferences(spec, deps, add)
	registry := structuralRegistry(spec)
	workflowIDs := make(map[string]struct{}, len(spec.Workflows))
	for i, item := range spec.Workflows {
		base := fmt.Sprintf("workflows[%d].definition", i)
		limitsBase := fmt.Sprintf("workflows[%d].limits", i)
		id := item.Definition.ID
		if id != "" {
			if _, exists := workflowIDs[id]; exists {
				add(DiagnosticDuplicate, base+".id", id, fmt.Sprintf("duplicate workflow id %q", id))
			} else {
				workflowIDs[id] = struct{}{}
			}
		}
		validateNodeReferences(item.Definition.Root, base+".root", id, references, add)
		invalidLimits := validateWorkflowLimitFields(item.Limits, limitsBase, id, add)
		if item.Limits != nil && spec.SchemaVersion < SchemaVersion5 {
			add(DiagnosticInvalidValue, limitsBase, id, "limits requires schema_version 5")
			invalidLimits = true
		}
		limits := item.Limits
		if invalidLimits {
			limits = nil
		}
		if _, err := workflow.New(workflow.Config{Definition: item.Definition, Registry: registry, Limits: limits}); err != nil {
			path := workflowLimitDiagnosticPath(limitsBase, err)
			if path == limitsBase {
				path = base
			}
			add(DiagnosticInvalidWorkflow, path, id, err.Error())
		}
	}
	return diagnostics
}

func validateMemorySpec(spec ProjectSpec, deps Dependencies, add func(DiagnosticCode, string, string, string)) {
	if spec.Memory == nil {
		return
	}
	if spec.SchemaVersion < SchemaVersion6 {
		add(DiagnosticInvalidValue, "memory", spec.ID, "memory requires schema_version 6")
	}
	if spec.ID != "" && memory.ValidateScopeComponent(spec.ID) != nil {
		add(DiagnosticInvalidValue, "id", spec.ID, "memory project id must satisfy the finite scope-component contract")
	}
	if spec.Memory.Ref == "" {
		add(DiagnosticRequired, "memory.ref", spec.ID, "memory dependency reference is required")
	} else if dependency, ok := deps.Memories[spec.Memory.Ref]; !ok || dependency.Module == nil {
		add(DiagnosticUnknownReference, "memory.ref", spec.ID, fmt.Sprintf("memory dependency %q is not registered", spec.Memory.Ref))
	} else if spec.Memory.ProposeEnabled {
		if !memory.ProposalSourceResolverAvailable(dependency.SourceResolver) {
			add(DiagnosticRequired, "memory.propose_enabled", spec.ID, "enabled proposal HTTP requires a trusted source resolver")
		}
		if !dependency.Module.ProposalEnabled() {
			add(DiagnosticRequired, "memory.propose_enabled", spec.ID, "enabled proposal HTTP requires a proposal-capable memory module")
		}
	}
	if memory.ValidateScopeComponent(spec.Memory.CollectionID) != nil {
		add(DiagnosticInvalidValue, "memory.collection_id", spec.ID, "memory collection_id must be a finite non-empty identifier")
	}
	if spec.Memory.Recall == nil {
		return
	}
	recall := spec.Memory.Recall
	if recall.Limit < 0 || recall.Limit > memory.DefaultLimits().MaxRecallLimit {
		add(DiagnosticInvalidValue, "memory.recall.limit", spec.ID, "memory recall limit exceeds the finite maximum")
	}
	if recall.FailureMode != "" && recall.FailureMode != "strict" && recall.FailureMode != "omit_on_unavailable" {
		add(DiagnosticInvalidValue, "memory.recall.failure_mode", spec.ID, "memory recall failure_mode is invalid")
	}
	if len(recall.Kinds) > memory.MaxRecallKinds {
		add(DiagnosticInvalidValue, "memory.recall.kinds", spec.ID, "memory recall kinds exceed the finite maximum")
	}
	if len(recall.Topics) > memory.MaxRecallTopics {
		add(DiagnosticInvalidValue, "memory.recall.topics", spec.ID, "memory recall topics exceed the finite maximum")
	}
	for i, kind := range recall.Kinds {
		if kind != memory.KindUserStatement && kind != memory.KindPreference && kind != memory.KindVerifiedFact {
			add(DiagnosticInvalidValue, fmt.Sprintf("memory.recall.kinds[%d]", i), spec.ID, "memory recall kind is invalid")
		}
	}
	for i, topic := range recall.Topics {
		if topic == "" || len(topic) > memory.MaxRecallTopicBytes || !utf8.ValidString(topic) {
			add(DiagnosticInvalidValue, fmt.Sprintf("memory.recall.topics[%d]", i), spec.ID, "memory recall topic is invalid")
		}
	}
}

func workflowLimitDiagnosticPath(base string, err error) string {
	var definitionLimit *workflow.DefinitionLimitError
	var payloadLimit *workflow.PayloadLimitError
	switch {
	case errors.As(err, &definitionLimit):
		field := map[string]string{"nodes": "max_definition_nodes", "depth": "max_definition_depth", "bytes": "max_definition_bytes"}[definitionLimit.Resource]
		if field != "" {
			return base + "." + field
		}
	case errors.As(err, &payloadLimit):
		return base + ".max_run_envelope_bytes"
	}
	return base
}

func validateWorkflowLimitFields(limits *workflow.Limits, base, id string, add func(DiagnosticCode, string, string, string)) bool {
	if limits == nil {
		return false
	}
	invalid := false
	fields := []struct {
		name  string
		value int64
	}{
		{"max_definition_nodes", int64(limits.MaxDefinitionNodes)},
		{"max_definition_depth", int64(limits.MaxDefinitionDepth)},
		{"max_definition_bytes", int64(limits.MaxDefinitionBytes)},
		{"max_concurrent_callbacks", int64(limits.MaxConcurrentCallbacks)},
		{"run_timeout", int64(limits.RunTimeout)},
		{"persistence_timeout", int64(limits.PersistenceTimeout)},
		{"max_input_bytes", int64(limits.MaxInputBytes)},
		{"max_node_output_bytes", int64(limits.MaxNodeOutputBytes)},
		{"max_state_bytes", int64(limits.MaxStateBytes)},
		{"max_run_envelope_bytes", int64(limits.MaxRunEnvelopeBytes)},
	}
	for _, field := range fields {
		if field.value < 0 {
			invalid = true
			add(DiagnosticInvalidValue, base+"."+field.name, id, field.name+" must not be negative")
		}
	}
	return invalid
}

type referenceIndex struct {
	executors  map[string]struct{}
	predicates map[string]struct{}
}

func validationReferences(spec ProjectSpec, deps Dependencies, add func(DiagnosticCode, string, string, string)) referenceIndex {
	references := referenceIndex{executors: make(map[string]struct{}), predicates: make(map[string]struct{})}
	executorRefs := sortedKeys(deps.Executors)
	for _, ref := range executorRefs {
		if ref == "" || deps.Executors[ref] == nil {
			add(DiagnosticInvalidValue, fmt.Sprintf("dependencies.executors[%q]", ref), "", "workflow executor reference and implementation are required")
			continue
		}
		references.executors[ref] = struct{}{}
	}
	predicateRefs := sortedKeys(deps.Predicates)
	for _, ref := range predicateRefs {
		if ref == "" || deps.Predicates[ref] == nil {
			add(DiagnosticInvalidValue, fmt.Sprintf("dependencies.predicates[%q]", ref), "", "workflow predicate reference and implementation are required")
			continue
		}
		references.predicates[ref] = struct{}{}
	}
	seenAgents := make(map[string]struct{}, len(spec.Agents))
	for i, item := range spec.Agents {
		if item.ID == "" {
			continue
		}
		if _, exists := seenAgents[item.ID]; exists {
			continue
		}
		seenAgents[item.ID] = struct{}{}
		ref := AgentExecutorRef(item.ID)
		if _, exists := references.executors[ref]; exists {
			add(DiagnosticConflictingConfiguration, fmt.Sprintf("agents[%d].id", i), item.ID, fmt.Sprintf("workflow executor %q is already registered", ref))
		} else {
			references.executors[ref] = struct{}{}
		}
	}
	seenTeams := make(map[string]struct{}, len(spec.Teams))
	for i, item := range spec.Teams {
		if item.ID == "" {
			continue
		}
		if _, exists := seenTeams[item.ID]; exists {
			continue
		}
		seenTeams[item.ID] = struct{}{}
		ref := TeamExecutorRef(item.ID)
		if _, exists := references.executors[ref]; exists {
			add(DiagnosticConflictingConfiguration, fmt.Sprintf("teams[%d].id", i), item.ID, fmt.Sprintf("workflow executor %q is already registered", ref))
		} else {
			references.executors[ref] = struct{}{}
		}
	}
	return references
}

func structuralRegistry(spec ProjectSpec) *workflow.Registry {
	executors := make(map[string]struct{})
	predicates := make(map[string]struct{})
	for _, item := range spec.Workflows {
		walkNodeReferences(item.Definition.Root, func(executorRef, predicateRef string) {
			if executorRef != "" {
				executors[executorRef] = struct{}{}
			}
			if predicateRef != "" {
				predicates[predicateRef] = struct{}{}
			}
		})
	}
	registry := workflow.NewRegistry()
	for _, ref := range sortedKeys(executors) {
		_ = registry.RegisterExecutor(ref, workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
			return workflow.NodeOutput{}, nil
		}))
	}
	for _, ref := range sortedKeys(predicates) {
		_ = registry.RegisterPredicate(ref, workflow.PredicateFunc(func(context.Context, workflow.NodeInput) (bool, error) {
			return false, nil
		}))
	}
	return registry
}

func validateNodeReferences(node workflow.NodeSpec, path, workflowID string, references referenceIndex, add func(DiagnosticCode, string, string, string)) {
	switch node.Type {
	case workflow.NodeTypeStep:
		if node.ExecutorRef == "" {
			add(DiagnosticRequired, path+".executor_ref", workflowID, "executor reference is required")
		} else if _, ok := references.executors[node.ExecutorRef]; !ok {
			add(DiagnosticUnknownReference, path+".executor_ref", workflowID, fmt.Sprintf("workflow executor %q is not registered", node.ExecutorRef))
		}
	case workflow.NodeTypeSequence, workflow.NodeTypeParallel:
		for i, child := range node.Children {
			validateNodeReferences(child, fmt.Sprintf("%s.children[%d]", path, i), workflowID, references, add)
		}
	case workflow.NodeTypeCondition:
		if node.PredicateRef == "" {
			add(DiagnosticRequired, path+".predicate_ref", workflowID, "predicate reference is required")
		} else if _, ok := references.predicates[node.PredicateRef]; !ok {
			add(DiagnosticUnknownReference, path+".predicate_ref", workflowID, fmt.Sprintf("workflow predicate %q is not registered", node.PredicateRef))
		}
		if node.Then != nil {
			validateNodeReferences(*node.Then, path+".then", workflowID, references, add)
		}
		if node.Else != nil {
			validateNodeReferences(*node.Else, path+".else", workflowID, references, add)
		}
	}
}

func walkNodeReferences(node workflow.NodeSpec, visit func(string, string)) {
	visit(node.ExecutorRef, node.PredicateRef)
	for _, child := range node.Children {
		walkNodeReferences(child, visit)
	}
	if node.Then != nil {
		walkNodeReferences(*node.Then, visit)
	}
	if node.Else != nil {
		walkNodeReferences(*node.Else, visit)
	}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
