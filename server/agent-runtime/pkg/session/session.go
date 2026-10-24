// Package session contains the core runtime data models.
package session

import (
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

// RunStatus represents the state of a run.
type RunStatus string

const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusError     RunStatus = "error"
	RunStatusCancelled RunStatus = "cancelled"
	RunStatusPaused    RunStatus = "paused"
)

// CanTransitionTo reports whether a run may move to next. Repeating a status
// is allowed so lifecycle commands can be safely retried.
func (s RunStatus) CanTransitionTo(next RunStatus) bool {
	if s == next {
		return true
	}
	switch s {
	case RunStatusPending:
		return next == RunStatusRunning || next == RunStatusCancelled || next == RunStatusError
	case RunStatusRunning:
		return next == RunStatusCompleted || next == RunStatusError || next == RunStatusCancelled || next == RunStatusPaused
	case RunStatusPaused:
		return next == RunStatusRunning || next == RunStatusCancelled
	default:
		return false
	}
}

// RunInput captures the user input for a run.
type RunInput struct {
	Message string         `json:"message,omitempty"`
	Images  []models.Image `json:"-"`
	Audios  []models.Audio `json:"-"`
	Videos  []models.Video `json:"-"`
	Files   []models.File  `json:"-"`
}

// RunRequest contains the immutable, per-invocation values used to execute a
// component. It prevents a shared Agent, Team, or Workflow instance from
// carrying request-specific user or session state.
type RunRequest struct {
	Input       RunInput
	RunID       string
	UserID      string
	SessionID   string
	ParentRunID string
	NodeID      string
	Metadata    map[string]string
}

// ResumeRequest resumes a paused run after a tool approval decision.
type ResumeRequest struct {
	RunID         string
	RequirementID string
	Action        string
	ActorID       string
	UserID        string
	Input         any
}

// PendingPlanRef is the frozen managed execution plan for one paused tool
// call (RC-05). It mirrors service.PlanRef without importing the service
// package, so session stays dependency-light and round-trips through storage.
type PendingPlanRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	// ApprovalRequired records the authorizer's posture at prepare time, so
	// resume can pause again at the next required tool without re-preparing.
	ApprovalRequired bool `json:"approval_required,omitempty"`
	// Denied records a prepare-time policy denial (unknown tool or
	// forbidden). The call has no plan; its tool result reports the denial
	// to the model instead of failing the run.
	Denied string `json:"denied,omitempty"`
}

// RunOutput is the result of a single agent run.
type RunOutput struct {
	RunID       string `json:"run_id"`
	AgentID     string `json:"agent_id,omitempty"`
	TeamID      string `json:"team_id,omitempty"`
	WorkflowID  string `json:"workflow_id,omitempty"`
	ParentRunID string `json:"parent_run_id,omitempty"`
	NodeID      string `json:"node_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	// Principal carries the trusted execution identity (audit/owner); it is
	// injected by the host and never accepted from HTTP bodies.
	Principal      execution.Principal     `json:"principal,omitempty"`
	Metadata       map[string]string       `json:"metadata,omitempty"`
	Metrics        RunMetrics              `json:"metrics,omitempty"`
	ToolExecutions []ToolExecution         `json:"tool_executions,omitempty"`
	Input          RunInput                `json:"input,omitempty"`
	Content        string                  `json:"content,omitempty"`
	Messages       []models.Message        `json:"messages,omitempty"`
	ToolCalls      []models.ToolCall       `json:"tool_calls,omitempty"`
	Parsed         any                     `json:"parsed,omitempty"`
	Status         RunStatus               `json:"status"`
	Error          string                  `json:"error,omitempty"`
	Workflow       *workflowschema.RunData `json:"workflow,omitempty"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
	// Continuation state for paused runs waiting for tool approval.
	ActiveRequirementID string                    `json:"active_requirement_id,omitempty"`
	Requirements        []requirement.Requirement `json:"requirements,omitempty"`
	PendingToolCalls    []models.ToolCall         `json:"pending_tool_calls,omitempty"`
	// PendingPlans holds the frozen managed execution plans for the paused
	// batch, keyed by tool-call ID, so resume executes the same plan it was
	// approved against (RC-06 grant binding).
	PendingPlans  map[string]PendingPlanRef `json:"pending_plans,omitempty"`
	NextToolIndex int                       `json:"next_tool_index,omitempty"`
	ModelRound    int                       `json:"model_round,omitempty"`
	// LeaseTimeout, when set, marks a run kept alive by heartbeats
	// (db.RunLeaseStore.TouchRun). A running run whose last heartbeat is
	// older than LeaseTimeout is presumed orphaned by a crashed process and
	// may be expired by run.ExpireStaleRuns. Runs without it are never
	// expired that way (managed runs use dispatcher leases instead).
	LeaseTimeout time.Duration `json:"lease_timeout,omitempty"`
	// RecoveredBy names the run that continued this one from its last
	// checkpoint after its process crashed (agent.Agent.Recover). Such a
	// run is left out of conversation history; its successor carries the
	// transcript.
	RecoveredBy string `json:"recovered_by,omitempty"`
}

// ActiveRequirement returns the active pending requirement, or nil if none.
func (r *RunOutput) ActiveRequirement() *requirement.Requirement {
	if r.ActiveRequirementID == "" {
		return nil
	}
	for i := range r.Requirements {
		if r.Requirements[i].ID == r.ActiveRequirementID {
			return &r.Requirements[i]
		}
	}
	return nil
}

// SetActiveRequirement sets the active requirement ID and appends to history if new.
func (r *RunOutput) SetActiveRequirement(req requirement.Requirement) {
	r.ActiveRequirementID = req.ID
	for i := range r.Requirements {
		if r.Requirements[i].ID == req.ID {
			r.Requirements[i] = req
			return
		}
	}
	r.Requirements = append(r.Requirements, req)
}

// RunMetrics summarizes model and tool activity owned by one Run.
type RunMetrics struct {
	DurationMS            int64 `json:"duration_ms"`
	ModelCalls            int   `json:"model_calls"`
	ToolCalls             int   `json:"tool_calls"`
	InputTokens           int   `json:"input_tokens"`
	OutputTokens          int   `json:"output_tokens"`
	TotalTokens           int   `json:"total_tokens"`
	OriginalContextTokens int   `json:"original_context_tokens,omitempty"`
	FinalContextTokens    int   `json:"final_context_tokens,omitempty"`
	ContextCompactions    int   `json:"context_compactions,omitempty"`
}

// ToolExecutionStatus is the normalized outcome of a runtime tool call.
type ToolExecutionStatus string

const (
	ToolExecutionStatusSuccess          ToolExecutionStatus = "success"
	ToolExecutionStatusFailed           ToolExecutionStatus = "failed"
	ToolExecutionStatusRejected         ToolExecutionStatus = "rejected"
	ToolExecutionStatusNotFound         ToolExecutionStatus = "not_found"
	ToolExecutionStatusInvalidArguments ToolExecutionStatus = "invalid_arguments"
)

// ToolExecution is a compact execution summary; result bodies remain in Messages.
type ToolExecution struct {
	CallID     string              `json:"call_id"`
	Name       string              `json:"name"`
	Status     ToolExecutionStatus `json:"status"`
	DurationMS int64               `json:"duration_ms"`
	Error      string              `json:"error,omitempty"`
}

// WorkflowRunOutput names the workflow-specific use of RunOutput. The common
// lifecycle envelope remains shared so /runs can query every run kind, while
// workflow-only state lives under Workflow.
type WorkflowRunOutput = RunOutput

// AgentSession stores the persistent state of an agent conversation.
type AgentSession struct {
	SessionID    string         `json:"session_id"`
	AgentID      string         `json:"agent_id"`
	Namespace    string         `json:"namespace,omitempty"`
	UserID       string         `json:"user_id,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	SessionState map[string]any `json:"session_state,omitempty"`
	Runs         []*RunOutput   `json:"runs,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	Revision     uint64         `json:"revision"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// GetMessages returns the flattened assistant/user/tool messages across all runs.
func (s *AgentSession) GetMessages() []models.Message {
	var msgs []models.Message
	for _, run := range s.Runs {
		msgs = append(msgs, run.Messages...)
	}
	return msgs
}

// GetLastRun returns the most recent run or nil.
func (s *AgentSession) GetLastRun() *RunOutput {
	if len(s.Runs) == 0 {
		return nil
	}
	return s.Runs[len(s.Runs)-1]
}

// TeamSession stores the persistent state of a team run.
type TeamSession struct {
	SessionID    string         `json:"session_id"`
	TeamID       string         `json:"team_id"`
	Namespace    string         `json:"namespace,omitempty"`
	UserID       string         `json:"user_id,omitempty"`
	Members      []string       `json:"members,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	SessionState map[string]any `json:"session_state,omitempty"`
	Runs         []*RunOutput   `json:"runs,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	Revision     uint64         `json:"revision"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// GetLastRun returns the most recent run or nil.
func (s *TeamSession) GetLastRun() *RunOutput {
	if len(s.Runs) == 0 {
		return nil
	}
	return s.Runs[len(s.Runs)-1]
}

// WorkflowSession stores the persistent state of a workflow run.
type WorkflowSession struct {
	SessionID    string         `json:"session_id"`
	WorkflowID   string         `json:"workflow_id"`
	Namespace    string         `json:"namespace,omitempty"`
	UserID       string         `json:"user_id,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	SessionState map[string]any `json:"session_state,omitempty"`
	Runs         []*RunOutput   `json:"runs,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	Revision     uint64         `json:"revision"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// GetLastRun returns the most recent run or nil.
func (s *WorkflowSession) GetLastRun() *RunOutput {
	if len(s.Runs) == 0 {
		return nil
	}
	return s.Runs[len(s.Runs)-1]
}
