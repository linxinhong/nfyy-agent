// Package workflow implements sequential step-based workflow execution.
package workflow

import (
	"context"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/internal/streamtransport"
	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	executionpkg "github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Workflow orchestrates a sequence of steps.
type Workflow struct {
	cfg  Config
	core bool
}

// ID returns the workflow identifier.
func (w *Workflow) ID() string {
	if w.core && w.cfg.Definition.ID != "" {
		return w.cfg.Definition.ID
	}
	if w.cfg.ID != "" {
		return w.cfg.ID
	}
	if w.cfg.Name != "" {
		return w.cfg.Name
	}
	return "workflow"
}

// Name returns the workflow name.
func (w *Workflow) Name() string {
	if w.core && w.cfg.Definition.Name != "" {
		return w.cfg.Definition.Name
	}
	if w.cfg.Name != "" {
		return w.cfg.Name
	}
	return w.ID()
}

// SetUserID updates the user identifier associated with subsequent runs.
func (w *Workflow) SetUserID(userID string) {
	w.cfg.UserID = userID
}

// SetSessionID updates the session identifier used for subsequent runs.
func (w *Workflow) SetSessionID(sessionID string) {
	w.cfg.SessionID = sessionID
}

// requireNamespaceStore ensures every public execution entry point rejects a
// tenant request before its private DB can observe an unscoped context.
func (w *Workflow) requireNamespaceStore(ctx context.Context) error {
	if _, tenant := executionpkg.NamespaceFromContext(ctx); tenant {
		if _, ok := w.cfg.DB.(db.NamespaceScopedStore); !ok {
			return db.ErrNamespaceStoreRequired
		}
	}
	return nil
}

// Run executes the workflow synchronously and returns the final RunOutput.
func (w *Workflow) Run(ctx context.Context, input any) (*session.RunOutput, error) {
	if err := w.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	if request, ok := input.(RunRequest); ok {
		return w.runCore(ctx, request, nil)
	}
	text, ok := input.(string)
	if !ok {
		return nil, fmt.Errorf("workflow: unsupported input type %T", input)
	}
	if w.core {
		return w.runCore(ctx, RunRequest{Input: text}, nil)
	}
	return w.RunWith(ctx, session.RunRequest{Input: session.RunInput{Message: text}})
}

// RunWith executes one immutable request.
func (w *Workflow) RunWith(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
	if err := w.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	if request.Metadata == nil {
		request.Metadata, _ = session.RunMetadataFromContext(ctx)
	}
	metadata, err := session.CloneRunMetadata(request.Metadata)
	if err != nil {
		return nil, err
	}
	ctx = session.ContextWithRunMetadata(ctx, metadata)
	if w.core {
		return w.runCore(ctx, RunRequest{Input: request.Input.Message, RunID: request.RunID, UserID: request.UserID, SessionID: request.SessionID, Metadata: metadata}, nil)
	}
	runWorkflow := *w
	if request.UserID != "" {
		runWorkflow.cfg.UserID = request.UserID
	}
	if request.SessionID != "" {
		runWorkflow.cfg.SessionID = request.SessionID
	}
	return runWorkflow.run(ctx, request.Input.Message, request.RunID, metadata)
}

// RunStream executes a structured workflow and emits workflow-scoped events.
func (w *Workflow) RunStream(ctx context.Context, request RunRequest) (<-chan Event, error) {
	return w.RunEvents(ctx, request)
}

// RunLegacyStream streams the deprecated sequential Steps representation.
func (w *Workflow) RunLegacyStream(ctx context.Context, input string) (<-chan agent.Event, error) {
	return w.RunStreamWith(ctx, session.RunRequest{Input: session.RunInput{Message: input}})
}

// RunStreamWith streams one immutable request.
func (w *Workflow) RunStreamWith(ctx context.Context, request session.RunRequest) (<-chan agent.Event, error) {
	if err := w.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	if request.Metadata == nil {
		request.Metadata, _ = session.RunMetadataFromContext(ctx)
	}
	metadata, err := session.CloneRunMetadata(request.Metadata)
	if err != nil {
		return nil, err
	}
	ctx = session.ContextWithRunMetadata(ctx, metadata)
	runWorkflow := *w
	if request.UserID != "" {
		runWorkflow.cfg.UserID = request.UserID
	}
	if request.SessionID != "" {
		runWorkflow.cfg.SessionID = request.SessionID
	}
	streamCtx, cancel := context.WithCancel(ctx)
	writer, ch := newLegacyEventStream(streamCtx, cancel, runWorkflow.cfg.Stream)
	go func() {
		var runErr error
		defer func() { writer.Finish(runErr) }()
		_, runErr = runWorkflow.runStream(streamCtx, request.Input.Message, request.RunID, metadata, func(event agent.Event, kind streamtransport.Kind) bool {
			return writer.Emit(event, kind)
		})
	}()
	return ch, nil
}

// GetSessionState returns the current session state for the given session id.
// If the workflow has no DB configured, it returns an error.
func (w *Workflow) GetSessionState(ctx context.Context, sessionID string) (map[string]any, error) {
	return w.getSessionState(ctx, sessionID)
}

// UpdateSessionState merges updates into the session's state and persists it.
// If the workflow has no DB configured, it returns an error.
func (w *Workflow) UpdateSessionState(ctx context.Context, sessionID string, state map[string]any) error {
	return w.updateSessionState(ctx, sessionID, state)
}
