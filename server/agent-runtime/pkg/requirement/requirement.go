// Package requirement defines the types for persistent tool approval.
// It is dependency-free so storage, agent, and HTTP layers can share the model.
package requirement

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// Kind identifies why execution stopped or why an audit record was created.
type Kind string

const (
	KindToolApproval Kind = "tool_approval"
	KindToolAudit    Kind = "tool_audit"
)

// Status tracks the lifecycle of a requirement.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
	StatusExpired  Status = "expired"
)

// Action is the typed resolution action.
type Action string

const (
	ActionApprove Action = "approve"
	ActionReject  Action = "reject"
	ActionExpire  Action = "expire"
)

// ResolveAction validates and normalizes a raw action string.
func ResolveAction(raw string) (Action, bool) {
	switch Action(raw) {
	case ActionApprove:
		return ActionApprove, true
	case ActionReject:
		return ActionReject, true
	case ActionExpire:
		return ActionExpire, true
	default:
		return "", false
	}
}

// Requirement is a stable record that a tool call needs approval before execution.
type Requirement struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	AgentID    string          `json:"agent_id,omitempty"`
	SessionID  string          `json:"session_id,omitempty"`
	UserID     string          `json:"user_id,omitempty"`
	Kind       Kind            `json:"kind"`
	ToolCall   models.ToolCall `json:"tool_call"`
	ModelRound int             `json:"model_round,omitempty"`
	ToolIndex  int             `json:"tool_index,omitempty"`
	ActorID    string          `json:"actor_id,omitempty"`
	Deadline   *time.Time      `json:"deadline,omitempty"`
	Status     Status          `json:"status"`
	Resolution *Resolution     `json:"resolution,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
}

// StableID derives the same opaque ID for one run/model-round/tool call.
func StableID(runID string, modelRound, toolIndex int, toolCallID string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + toolCallID + "\x00" +
		strconv.Itoa(modelRound) + "\x00" + strconv.Itoa(toolIndex)))
	return "req_" + hex.EncodeToString(sum[:12])
}

// Resolution records who resolved a requirement and how.
type Resolution struct {
	ActorID string    `json:"actor_id,omitempty"`
	Action  Action    `json:"action"`
	Input   any       `json:"input,omitempty"`
	At      time.Time `json:"at"`
}

// Key returns the stable deduplication key for a requirement within a run.
func (r *Requirement) Key() string {
	return r.RunID + ":" + strconv.Itoa(r.ModelRound) + ":" +
		strconv.Itoa(r.ToolIndex) + ":" + r.ToolCall.ID
}

// IsExpiredAt reports whether the requirement is expired at the given time.
func (r *Requirement) IsExpiredAt(now time.Time) bool {
	return r.Deadline != nil && !now.Before(*r.Deadline)
}

// CanResolveAt reports whether the requirement can be resolved at the given time.
func (r *Requirement) CanResolveAt(now time.Time) bool {
	if r.Status != StatusPending {
		return false
	}
	return !r.IsExpiredAt(now)
}

// IsResolved reports whether the requirement has been resolved.
func (r *Requirement) IsResolved() bool {
	return r.Status == StatusApproved || r.Status == StatusRejected || r.Status == StatusExpired
}

// SnapshotToolCall captures a ToolCall by value. ToolCall and FunctionCall
// currently contain only value fields, so assignment is an independent
// snapshot; this function must be updated if either model gains reference fields.
func SnapshotToolCall(call models.ToolCall) models.ToolCall {
	return call
}
