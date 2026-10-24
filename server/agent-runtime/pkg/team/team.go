// Package team implements multi-agent team orchestration.
package team

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Team is the primary multi-agent orchestration unit.
type Team struct {
	cfg TeamConfig
}

// ID returns the team identifier.
func (t *Team) ID() string {
	if t.cfg.ID != "" {
		return t.cfg.ID
	}
	if t.cfg.Name != "" {
		return t.cfg.Name
	}
	return "team"
}

// Name returns the team name.
func (t *Team) Name() string {
	if t.cfg.Name != "" {
		return t.cfg.Name
	}
	return t.ID()
}

// SetUserID updates the user identifier associated with subsequent runs.
func (t *Team) SetUserID(userID string) {
	t.cfg.UserID = userID
}

// SetSessionID updates the session identifier used for subsequent runs.
func (t *Team) SetSessionID(sessionID string) {
	t.cfg.SessionID = sessionID
}

// Run executes the team synchronously and returns a RunOutput.
func (t *Team) Run(ctx context.Context, input string) (*session.RunOutput, error) {
	return t.RunWith(ctx, session.RunRequest{Input: session.RunInput{Message: input}})
}

// RunWith executes one immutable request.
func (t *Team) RunWith(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
	if _, tenant := execution.NamespaceFromContext(ctx); tenant {
		if _, ok := t.cfg.DB.(db.NamespaceScopedStore); !ok {
			return nil, db.ErrNamespaceStoreRequired
		}
	}
	if request.Metadata == nil {
		request.Metadata, _ = session.RunMetadataFromContext(ctx)
	}
	metadata, err := session.CloneRunMetadata(request.Metadata)
	if err != nil {
		return nil, err
	}
	ctx = session.ContextWithRunMetadata(ctx, metadata)
	runTeam := *t
	if request.UserID != "" {
		runTeam.cfg.UserID = request.UserID
	}
	if request.SessionID != "" {
		runTeam.cfg.SessionID = request.SessionID
	}
	return runTeam.execute(ctx, request.Input.Message, request.RunID, request.ParentRunID, request.NodeID, metadata, nil)
}

// RunStream executes the team and streams events through a channel.
func (t *Team) RunStream(ctx context.Context, input string) (<-chan agent.Event, error) {
	return t.RunStreamWith(ctx, session.RunRequest{Input: session.RunInput{Message: input}})
}

// RunStreamWith streams one immutable request.
func (t *Team) RunStreamWith(ctx context.Context, request session.RunRequest) (<-chan agent.Event, error) {
	if _, tenant := execution.NamespaceFromContext(ctx); tenant {
		if _, ok := t.cfg.DB.(db.NamespaceScopedStore); !ok {
			return nil, db.ErrNamespaceStoreRequired
		}
	}
	if request.Metadata == nil {
		request.Metadata, _ = session.RunMetadataFromContext(ctx)
	}
	metadata, err := session.CloneRunMetadata(request.Metadata)
	if err != nil {
		return nil, err
	}
	ctx = session.ContextWithRunMetadata(ctx, metadata)
	runTeam := *t
	if request.UserID != "" {
		runTeam.cfg.UserID = request.UserID
	}
	if request.SessionID != "" {
		runTeam.cfg.SessionID = request.SessionID
	}
	ch := make(chan agent.Event)
	emit := func(ev agent.Event) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	go func() {
		defer close(ch)
		_, _ = runTeam.execute(ctx, request.Input.Message, request.RunID, request.ParentRunID, request.NodeID, metadata, emit)
	}()
	return ch, nil
}
