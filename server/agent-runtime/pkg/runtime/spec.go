// Package runtime assembles a customized agent project from a serializable
// specification and injected runtime dependencies.
package runtime

import (
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

// CurrentSchemaVersion is the latest ProjectSpec JSON format supported.
const (
	LegacySchemaVersion  = 1
	SchemaVersion2       = 2
	SchemaVersion3       = 3
	SchemaVersion4       = 4
	SchemaVersion5       = 5
	SchemaVersion6       = 6
	CurrentSchemaVersion = SchemaVersion6
)

func isSupportedSchemaVersion(v int) bool {
	return v == LegacySchemaVersion || v == SchemaVersion2 || v == SchemaVersion3 || v == SchemaVersion4 || v == SchemaVersion5 || v == SchemaVersion6
}

// ProjectSpec is the serializable description of one customized agent project.
// Executable values and secrets are referenced by name and supplied separately
// through Dependencies.
type ProjectSpec struct {
	SchemaVersion int             `json:"schema_version"`
	ID            string          `json:"id"`
	Version       string          `json:"version"`
	Name          string          `json:"name,omitempty"`
	Agents        []AgentSpec     `json:"agents,omitempty"`
	Teams         []TeamSpec      `json:"teams,omitempty"`
	Workflows     []WorkflowSpec  `json:"workflows,omitempty"`
	Knowledges    []KnowledgeSpec `json:"knowledges,omitempty"`
	Memory        *MemorySpec     `json:"memory,omitempty"`
}

// MemorySpec opts a v6 project into the strict Memory Module. Ref resolves a
// trusted executable dependency; project and collection authority are never
// accepted from an HTTP request.
type MemorySpec struct {
	Ref            string            `json:"ref"`
	CollectionID   string            `json:"collection_id"`
	Recall         *MemoryRecallSpec `json:"recall,omitempty"`
	ProposeEnabled bool              `json:"propose_enabled,omitempty"`
}

// MemoryRecallSpec is the serializable, project-frozen Agent recall policy.
// Nil disables Agent recall while leaving the governed management API enabled.
type MemoryRecallSpec struct {
	Limit       int           `json:"limit,omitempty"`
	Kinds       []memory.Kind `json:"kinds,omitempty"`
	Topics      []string      `json:"topics,omitempty"`
	FailureMode string        `json:"failure_mode,omitempty"`
}

type AgentSpec struct {
	ID                string             `json:"id"`
	Name              string             `json:"name,omitempty"`
	Instructions      string             `json:"instructions,omitempty"`
	ModelRef          string             `json:"model_ref"`
	ToolRefs          []string           `json:"tool_refs,omitempty"`
	KnowledgeRef      string             `json:"knowledge_ref,omitempty"`
	MaxToolCalls      int                `json:"max_tool_calls,omitempty"`
	Temperature       *float32           `json:"temperature,omitempty"`
	MaxTokens         *int               `json:"max_tokens,omitempty"`
	SessionState      map[string]any     `json:"session_state,omitempty"`
	OutputSchema      map[string]any     `json:"output_schema,omitempty"`
	ToolTimeoutMS     int                `json:"tool_timeout_ms,omitempty"`
	RunTimeoutMS      int                `json:"run_timeout_ms,omitempty"`
	MaxConcurrentRuns int                `json:"max_concurrent_runs,omitempty"`
	HooksRef          string             `json:"hooks_ref,omitempty"`
	HistoryPolicy     *HistoryPolicySpec `json:"history_policy,omitempty"`
	ContextWindow     *ContextWindowSpec `json:"context_window,omitempty"`
	TokenCounterRef   string             `json:"token_counter_ref,omitempty"`
	SummarizerRef     string             `json:"summarizer_ref,omitempty"`
	// SkillDirs are explicit directories each containing a SKILL.md.
	SkillDirs []string `json:"skill_dirs,omitempty"`
	// SkillRoots are directories scanned recursively for SKILL.md files.
	SkillRoots []string `json:"skill_roots,omitempty"`
}

// HistoryPolicySpec is the serializable form of agent.HistoryPolicy. It
// budgets conversation history: max_runs keeps the last N runs; after MaxRuns,
// max_turns and max_messages are applied globally across all remaining runs
// from the tail, so the earliest kept run may be partially truncated. max_messages
// is a soft cap applied on tool-call/result atomic-group boundaries (a group is
// never split). All fields default to zero (no budget) when omitted.
type HistoryPolicySpec struct {
	MaxRuns     int `json:"max_runs,omitempty"`
	MaxTurns    int `json:"max_turns,omitempty"`
	MaxMessages int `json:"max_messages,omitempty"`
}

// ContextWindowSpec is the serializable form of agent.ContextWindowPolicy.
// It budgets the assembled context by token count. MaxInputTokens is the hard
// cap; TriggerRatio and TargetRatio control when compaction triggers and the
// target token count; MinRecentTurns is the minimum number of recent turns to
// preserve (with a hard floor of 2). A MaxInputTokens of zero disables compaction.
type ContextWindowSpec struct {
	MaxInputTokens int     `json:"max_input_tokens,omitempty"`
	TriggerRatio   float64 `json:"trigger_ratio,omitempty"`
	TargetRatio    float64 `json:"target_ratio,omitempty"`
	MinRecentTurns int     `json:"min_recent_turns,omitempty"`
}

type TeamSpec struct {
	ID            string         `json:"id"`
	Name          string         `json:"name,omitempty"`
	Instructions  string         `json:"instructions,omitempty"`
	ModelRef      string         `json:"model_ref"`
	MemberRefs    []string       `json:"member_refs"`
	MaxIterations int            `json:"max_iterations,omitempty"`
	SessionState  map[string]any `json:"session_state,omitempty"`
}

type WorkflowSpec struct {
	Definition   workflow.Definition `json:"definition"`
	SessionState map[string]any      `json:"session_state,omitempty"`
	// Limits explicitly opts this Workflow into finite definition, execution,
	// payload, and persistence budgets. Nil preserves legacy behavior.
	Limits *workflow.Limits `json:"limits,omitempty"`
}

// KnowledgeSpec exposes an injected knowledge dependency under a project ID.
type KnowledgeSpec struct {
	ID  string `json:"id"`
	Ref string `json:"ref"`
}

// AgentExecutorRef returns the stable workflow executor reference for an
// assembled project agent.
func AgentExecutorRef(agentID string) string { return "agent/" + agentID }

// TeamExecutorRef returns the stable workflow executor reference for an
// assembled project team.
func TeamExecutorRef(teamID string) string { return "team/" + teamID }
