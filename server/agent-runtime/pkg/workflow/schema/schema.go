// Package schema contains the serializable workflow definition and run data.
// It is dependency-free so storage and runtime packages can share the model.
package schema

import (
	"encoding/json"
	"time"
)

type NodeType string

const (
	NodeTypeStep      NodeType = "step"
	NodeTypeSequence  NodeType = "sequence"
	NodeTypeParallel  NodeType = "parallel"
	NodeTypeCondition NodeType = "condition"
)

type Definition struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	Version string   `json:"version"`
	Root    NodeSpec `json:"root"`
}

type NodeSpec struct {
	ID           string          `json:"id"`
	Name         string          `json:"name,omitempty"`
	Type         NodeType        `json:"type"`
	ExecutorRef  string          `json:"executor_ref,omitempty"`
	PredicateRef string          `json:"predicate_ref,omitempty"`
	Config       json.RawMessage `json:"config,omitempty"`
	Children     []NodeSpec      `json:"children,omitempty"`
	Then         *NodeSpec       `json:"then,omitempty"`
	Else         *NodeSpec       `json:"else,omitempty"`
	Policy       NodePolicy      `json:"policy,omitempty"`
	HumanReview  *HumanReview    `json:"human_review,omitempty"`
}

type NodePolicy struct {
	Timeout     time.Duration `json:"timeout,omitempty"`
	MaxAttempts int           `json:"max_attempts,omitempty"`
	Backoff     time.Duration `json:"backoff,omitempty"`
	OnError     ErrorPolicy   `json:"on_error,omitempty"`
}

type ErrorPolicy string

const (
	ErrorPolicyFail ErrorPolicy = "fail"
	ErrorPolicySkip ErrorPolicy = "skip"
)

type HumanReview struct {
	Kind        RequirementKind `json:"kind"`
	Prompt      string          `json:"prompt,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Deadline    *time.Time      `json:"deadline,omitempty"`
}

type NodeStatus string

const (
	NodeStatusPending   NodeStatus = "pending"
	NodeStatusRunning   NodeStatus = "running"
	NodeStatusApplying  NodeStatus = "applying"
	NodeStatusCompleted NodeStatus = "completed"
	NodeStatusSkipped   NodeStatus = "skipped"
	NodeStatusPaused    NodeStatus = "paused"
	NodeStatusError     NodeStatus = "error"
	NodeStatusCancelled NodeStatus = "cancelled"
)

type NodeResult struct {
	NodeID         string         `json:"node_id"`
	NodeType       NodeType       `json:"node_type"`
	Status         NodeStatus     `json:"status"`
	Attempts       int            `json:"attempts,omitempty"`
	Input          any            `json:"input,omitempty"`
	Output         any            `json:"output,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	StatePatch     map[string]any `json:"state_patch,omitempty"`
	Error          string         `json:"error,omitempty"`
	SelectedBranch string         `json:"selected_branch,omitempty"`
	ChildRunIDs    []string       `json:"child_run_ids,omitempty"`
	Children       []NodeResult   `json:"children,omitempty"`
	StartedAt      time.Time      `json:"started_at"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`
}

type RequirementKind string

const (
	RequirementConfirmation RequirementKind = "confirmation"
	RequirementUserInput    RequirementKind = "user_input"
)

type Requirement struct {
	ID          string          `json:"id"`
	NodeID      string          `json:"node_id"`
	Kind        RequirementKind `json:"kind"`
	Prompt      string          `json:"prompt,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Deadline    *time.Time      `json:"deadline,omitempty"`
	Resolution  *Resolution     `json:"resolution,omitempty"`
}

type Resolution struct {
	ActorID string    `json:"actor_id,omitempty"`
	Action  string    `json:"action"`
	Input   any       `json:"input,omitempty"`
	At      time.Time `json:"at"`
}

type Checkpoint struct {
	Version      int    `json:"version"`
	PausedNodeID string `json:"paused_node_id"`
}

type RunData struct {
	SchemaVersion      int                   `json:"schema_version"`
	DefinitionVersion  string                `json:"definition_version,omitempty"`
	DefinitionHash     string                `json:"definition_hash,omitempty"`
	Definition         *Definition           `json:"definition,omitempty"`
	Input              any                   `json:"input,omitempty"`
	Output             any                   `json:"output,omitempty"`
	State              map[string]any        `json:"state,omitempty"`
	RootResult         *NodeResult           `json:"root_result,omitempty"`
	NodeResults        map[string]NodeResult `json:"node_results,omitempty"`
	Requirement        *Requirement          `json:"requirement,omitempty"`
	RequirementHistory []Requirement         `json:"requirement_history,omitempty"`
	Checkpoint         *Checkpoint           `json:"checkpoint,omitempty"`
	Legacy             bool                  `json:"legacy,omitempty"`
}
