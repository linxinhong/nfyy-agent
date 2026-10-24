// Package team implements multi-agent team orchestration.
package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

const defaultRunLeaseTimeout = 2 * time.Minute

// execute runs the team in coordinate mode. The coordinator is an internal
// Agent whose only tool delegates to members, so team runs share the Agent
// run loop: retries and fallbacks, finish-reason handling, reasoning
// replay, context budgets, and quotas. emit is nil for synchronous runs;
// otherwise it receives the team stream: team run_started, coordinator
// deltas and tool events, forwarded member events, and one team terminal
// event.
func (t *Team) execute(ctx context.Context, input, requestedRunID, parentRunID, nodeID string, metadata map[string]string, emit func(agent.Event) bool) (*session.RunOutput, error) {
	send := func(ev agent.Event) {
		if emit != nil {
			emit(ev)
		}
	}
	runStartedAt := time.Now().UTC()
	send(agent.Event{Type: agent.EventTypeRunStarted})
	fail := func(output *session.RunOutput, err error) (*session.RunOutput, error) {
		send(agent.Event{Type: agent.EventTypeRunError, Err: err})
		return output, err
	}

	sess, err := t.createTeamSession(ctx)
	if err != nil {
		return fail(nil, fmt.Errorf("create team session: %w", err))
	}
	runID := requestedRunID
	if runID == "" {
		runID = uuid.NewString()
	}
	output := &session.RunOutput{
		RunID:       runID,
		TeamID:      t.ID(),
		ParentRunID: parentRunID,
		NodeID:      nodeID,
		SessionID:   sess.SessionID,
		UserID:      t.cfg.UserID,
		Metadata:    metadata,
		Input:       session.RunInput{Message: input},
		Status:      session.RunStatusRunning,
		CreatedAt:   runStartedAt,
		UpdatedAt:   runStartedAt,
	}
	leases, leaseTimeout := t.leaseStore()
	if leases != nil {
		output.LeaseTimeout = leaseTimeout
	}
	if err := t.persistRun(ctx, output); err != nil {
		return fail(output, fmt.Errorf("start team run: %w", err))
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if leases != nil {
		query := db.RunQuery{RunID: output.RunID, UserID: output.UserID, Namespace: output.Namespace}
		stop := runmanager.StartRunHeartbeat(ctx, leases, query, leaseTimeout/4, func() { cancel(agent.ErrRunCancelledExternally) })
		defer stop()
	}

	coordinated, runErr := t.coordinate(ctx, output, input, metadata, emit)
	if coordinated != nil {
		output.Content = coordinated.Content
		output.Parsed = coordinated.Parsed
		output.Messages = coordinated.Messages
		output.ToolCalls = coordinated.ToolCalls
		output.ToolExecutions = coordinated.ToolExecutions
		output.Metrics = coordinated.Metrics
	}
	switch {
	case runErr == nil:
		output.Status = session.RunStatusCompleted
	case ctx.Err() != nil:
		output.Status = session.RunStatusCancelled
		runErr = context.Cause(ctx)
		output.Error = runErr.Error()
	default:
		output.Status = session.RunStatusError
		output.Error = runErr.Error()
	}

	if err := t.updateRun(ctx, output); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("save team session: %w", err))
	}
	if runErr != nil {
		return fail(output, runErr)
	}
	send(agent.Event{Type: agent.EventTypeRunCompleted, RunOutput: output})
	return output, nil
}

// coordinate runs the coordinator Agent for one team run.
func (t *Team) coordinate(ctx context.Context, output *session.RunOutput, input string, metadata map[string]string, emit func(agent.Event) bool) (*session.RunOutput, error) {
	schema, err := delegateSchema()
	if err != nil {
		return nil, err
	}
	delegate := &delegateTool{team: t, parent: output, schema: schema, emit: emit, forwarded: make(chan struct{}, 1)}
	coordinator, err := agent.New(agent.AgentConfig{
		ID:              t.ID(),
		Name:            t.Name(),
		Model:           t.cfg.Model,
		Instructions:    t.systemPrompt(),
		Tools:           []tools.Tool{delegate},
		MaxToolCalls:    t.cfg.MaxIterations,
		ToolTimeout:     t.cfg.DelegationTimeout,
		RetryConfig:     t.cfg.RetryConfig,
		FallbackModels:  t.cfg.FallbackModels,
		CircuitBreaker:  t.cfg.CircuitBreaker,
		UsageGuard:      t.cfg.UsageGuard,
		MaxRunTokens:    t.cfg.MaxRunTokens,
		UserID:          output.UserID,
		RunLeaseTimeout: -1,
	})
	if err != nil {
		return nil, err
	}
	request := session.RunRequest{Input: session.RunInput{Message: input}, UserID: output.UserID, Metadata: metadata}
	if emit == nil {
		return coordinator.RunWith(ctx, request)
	}

	events, err := coordinator.RunStreamWith(ctx, request)
	if err != nil {
		return nil, err
	}
	var final *session.RunOutput
	var runErr error
	for ev := range events {
		switch ev.Type {
		case agent.EventTypeRunStarted:
			// The team emitted its own run_started.
		case agent.EventTypeRunCompleted:
			final = ev.RunOutput
		case agent.EventTypeRunError:
			runErr = ev.Err
		case agent.EventTypeRunPaused:
			runErr = errors.New("team: coordinator paused unexpectedly")
		default:
			emit(ev)
			if ev.ToolCall == nil || ev.ToolCall.Function.Name != delegateToolName {
				continue
			}
			switch ev.Type {
			case agent.EventTypeToolStarted:
				select {
				case delegate.forwarded <- struct{}{}:
				case <-ctx.Done():
				}
			case agent.EventTypeToolResult:
				// A call rejected before reaching the tool (invalid
				// arguments, a blocking hook) never takes its signal;
				// drop it so the next delegation pairs correctly.
				select {
				case <-delegate.forwarded:
				default:
				}
			}
		}
	}
	if runErr == nil && final == nil {
		runErr = errors.New("team: coordinator stream ended without a result")
		if ctx.Err() != nil {
			runErr = context.Cause(ctx)
		}
	}
	return final, runErr
}

func (t *Team) systemPrompt() string {
	var memberLines []string
	for _, m := range t.cfg.Members {
		memberLines = append(memberLines, fmt.Sprintf("- id: %s, name: %s", m.ID(), m.Name()))
	}
	return fmt.Sprintf(`You are the leader of a team of agents.

Team instructions: %s

Members:
%s

To delegate a task to a member, call the delegate_to_member tool with the member's id and the task description. The tool will return the member's response.`, t.cfg.Instructions, strings.Join(memberLines, "\n"))
}

func (t *Team) leaseStore() (db.RunLeaseStore, time.Duration) {
	if t.cfg.DB == nil || t.cfg.RunLeaseTimeout < 0 {
		return nil, 0
	}
	store, ok := t.cfg.DB.(db.RunLeaseStore)
	if !ok {
		return nil, 0
	}
	if t.cfg.RunLeaseTimeout > 0 {
		return store, t.cfg.RunLeaseTimeout
	}
	return store, defaultRunLeaseTimeout
}

func (t *Team) createTeamSession(ctx context.Context) (*session.TeamSession, error) {
	if t.cfg.DB == nil {
		return newTeamSession(t.ID(), t.cfg.UserID, t.cfg.SessionState), nil
	}

	if t.cfg.SessionID != "" {
		sess, err := t.cfg.DB.ReadTeamSession(ctx, t.cfg.SessionID)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			return sess, nil
		}
	}

	sess := newTeamSession(t.ID(), t.cfg.UserID, t.cfg.SessionState)
	if err := t.cfg.DB.CreateTeamSession(ctx, sess); err != nil {
		return nil, err
	}
	t.cfg.SessionID = sess.SessionID
	return sess, nil
}

func (t *Team) persistRun(ctx context.Context, output *session.RunOutput) error {
	if t.cfg.DB == nil {
		return nil
	}
	return t.cfg.DB.AppendRun(ctx, output)
}

func (t *Team) updateRun(ctx context.Context, output *session.RunOutput) error {
	if output != nil && !output.CreatedAt.IsZero() {
		output.Metrics.DurationMS = time.Since(output.CreatedAt).Milliseconds()
	}
	if t.cfg.DB == nil {
		return nil
	}
	return runmanager.Finalize(ctx, t.cfg.DB, output, session.RunStatusRunning)
}

func (t *Team) saveTeamSession(ctx context.Context, sess *session.TeamSession) error {
	if t.cfg.DB == nil {
		return nil
	}
	existing, err := t.cfg.DB.ReadTeamSession(ctx, sess.SessionID)
	if err != nil {
		return err
	}
	if existing == nil {
		return t.cfg.DB.CreateTeamSession(ctx, sess)
	}
	return t.cfg.DB.UpdateTeamSession(ctx, sess)
}

func newTeamSession(teamID, userID string, initialState map[string]any) *session.TeamSession {
	now := time.Now().UTC()
	return &session.TeamSession{
		SessionID:    uuid.NewString(),
		TeamID:       teamID,
		UserID:       userID,
		SessionState: copyMap(initialState),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}
