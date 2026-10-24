// Package sessions defines HTTP schema types for the sessions router.
package sessions

import (
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// CreateSessionRequest is the body for POST /sessions.
type CreateSessionRequest struct {
	// AgentID is the agent that owns the session.
	AgentID string `json:"agent_id,omitempty"`
	// UserID is the user that owns the session.
	UserID string `json:"user_id,omitempty"`
	// Metadata is optional session metadata.
	Metadata map[string]any `json:"metadata,omitempty"`
	// SessionState is the initial session state.
	SessionState map[string]any `json:"session_state,omitempty"`
}

// UpdateSessionRequest is the body for PATCH /sessions/{id}.
type UpdateSessionRequest struct {
	// Revision is the version observed by the caller and is required.
	Revision uint64 `json:"revision,omitempty"`
	// Metadata is the updated session metadata.
	Metadata map[string]any `json:"metadata,omitempty"`
	// SessionState is the updated session state.
	SessionState map[string]any `json:"session_state,omitempty"`
	// Summary is the updated session summary.
	Summary *string `json:"summary,omitempty"`
}

// SessionResponse wraps an AgentSession for JSON responses.
type SessionResponse struct {
	// SessionID is the unique session identifier.
	SessionID string `json:"session_id"`
	// AgentID is the agent that owns the session.
	AgentID string `json:"agent_id,omitempty"`
	// UserID is the user that owns the session.
	UserID string `json:"user_id,omitempty"`
	// Metadata is optional session metadata.
	Metadata map[string]any `json:"metadata,omitempty"`
	// SessionState is the persisted session state.
	SessionState map[string]any `json:"session_state,omitempty"`
	// Runs are the runs recorded in the session.
	Runs []*session.RunOutput `json:"runs,omitempty"`
	// Summary is an optional session summary.
	Summary string `json:"summary,omitempty"`
	// Revision is incremented by each successful session field update.
	Revision uint64 `json:"revision"`
	// CreatedAt is the session creation timestamp.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the last-update timestamp.
	UpdatedAt time.Time `json:"updated_at"`
}

func toSessionResponse(s *session.AgentSession) SessionResponse {
	return SessionResponse{
		SessionID:    s.SessionID,
		AgentID:      s.AgentID,
		UserID:       s.UserID,
		Metadata:     s.Metadata,
		SessionState: s.SessionState,
		Runs:         s.Runs,
		Summary:      s.Summary,
		Revision:     s.Revision,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
}
