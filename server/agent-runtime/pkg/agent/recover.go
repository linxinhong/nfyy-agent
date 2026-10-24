package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

var (
	// ErrRecoverNotFound reports that the run does not exist for this
	// Agent and user.
	ErrRecoverNotFound = errors.New("agent: run to recover not found")
	// ErrRecoverNotAllowed reports that the run did not end by a process
	// crash (run.ExpireStaleRuns), so there is nothing to recover.
	ErrRecoverNotAllowed = errors.New("agent: only runs failed by a crashed process can be recovered")
	// ErrRecoverConflict reports that the run was already recovered.
	ErrRecoverConflict = errors.New("agent: run was already recovered")
)

// interruptedToolResult answers a tool call whose outcome is unknown and
// which is not safe to repeat.
const interruptedToolResult = "error: this tool call was interrupted by a process crash and its outcome is unknown. " +
	"It was not retried because the tool is not declared idempotent; check the current state before relying on it or calling it again."

// RecoverRequest identifies a crashed run to continue.
type RecoverRequest struct {
	RunID  string
	UserID string
}

// Recover continues a run that a crashed process left behind (failed by
// run.ExpireStaleRuns) from its last checkpoint, as a new run in the same
// session; the original records RecoveredBy and leaves the history.
//
// Tool calls the crashed process had started but not finished have an
// unknown outcome. Calls to tools declared idempotent (or read-only) are
// executed again; the others are answered with an "interrupted, outcome
// unknown" result so the model can check before acting, and no side
// effect is repeated blindly.
func (a *Agent) Recover(ctx context.Context, request RecoverRequest) (*session.RunOutput, error) {
	if err := a.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	if a.cfg.DB == nil {
		return nil, errors.New("agent: Recover requires durable storage")
	}
	if !a.acquireRunSlot() {
		return nil, ErrBusy
	}
	defer a.releaseRunSlot()
	ctx, permit, err := a.acquireAdmission(ctx)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	runAgent := a.forRequest(request.UserID, "")
	if runAgent.cfg.RunTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runAgent.cfg.RunTimeout)
		defer cancel()
	}
	return runAgent.recover(ctx, request.RunID, emitter{})
}

func (a *Agent) loadRecoverable(ctx context.Context, runID string) (*session.RunOutput, error) {
	orig, err := a.cfg.DB.GetRun(ctx, db.RunQuery{RunID: runID, UserID: a.cfg.UserID})
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	if orig == nil || orig.AgentID != a.ID() || (a.cfg.UserID != "" && orig.UserID != a.cfg.UserID) {
		return nil, ErrRecoverNotFound
	}
	return orig, nil
}

func (a *Agent) recover(ctx context.Context, runID string, em emitter) (*session.RunOutput, error) {
	orig, err := a.loadRecoverable(ctx, runID)
	if err != nil {
		return nil, err
	}
	// The session lock makes the RecoveredBy check-and-claim below atomic
	// across replicas sharing a distributed SessionLocker.
	ctx, unlock, err := a.lockSession(ctx, orig.SessionID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if orig, err = a.loadRecoverable(ctx, runID); err != nil {
		return nil, err
	}
	if orig.RecoveredBy != "" {
		return nil, fmt.Errorf("%w by run %s", ErrRecoverConflict, orig.RecoveredBy)
	}
	if orig.Status != session.RunStatusError || orig.Error != runmanager.StaleRunReason {
		return nil, ErrRecoverNotAllowed
	}
	sess, err := a.readSession(ctx, orig.SessionID)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if sess == nil || sess.AgentID != orig.AgentID || sess.UserID != orig.UserID {
		return nil, ErrRecoverNotFound
	}
	a.cfg.SessionID = orig.SessionID

	newRunID := uuid.NewString()
	orig.RecoveredBy = newRunID
	if err := a.cfg.DB.UpdateRun(ctx, orig, session.RunStatusError); err != nil {
		return nil, fmt.Errorf("claim run for recovery: %w", err)
	}
	for _, r := range sess.Runs {
		if r != nil && r.RunID == orig.RunID {
			r.RecoveredBy = newRunID
		}
	}

	now := time.Now().UTC()
	metadata, err := session.CloneRunMetadata(orig.Metadata)
	if err != nil {
		return nil, err
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["recovered_from"] = orig.RunID
	output := &session.RunOutput{
		RunID: newRunID, AgentID: a.ID(), ParentRunID: orig.ParentRunID, NodeID: orig.NodeID,
		SessionID: orig.SessionID, UserID: orig.UserID, Metadata: metadata, Input: orig.Input,
		Status: session.RunStatusRunning, ModelRound: orig.ModelRound, CreatedAt: now, UpdatedAt: now,
	}
	if err := applyTrustedRunIdentity(ctx, output); err != nil {
		return nil, err
	}
	if store, timeout := a.leaseStore(ctx, output); store != nil {
		output.LeaseTimeout = timeout
	}
	if err := a.persistRun(ctx, sess, output); err != nil {
		return nil, fmt.Errorf("start recovered run: %w", err)
	}
	ctx = contextWithMetrics(ctx, &output.Metrics)
	st := &runState{sess: sess, output: output}
	ctx, release := a.guardRun(ctx, st)
	defer release()
	em.emit(Event{Type: EventTypeRunStarted})

	if err := a.prepareModelOptions(st); err != nil {
		return a.finishRun(ctx, st, em, err)
	}
	transcript, err := a.settleInterruptedTools(contextWithEmitter(ctx, em), st, cloneMessages(orig.Messages))
	if err != nil {
		return a.finishRun(ctx, st, em, err)
	}
	st.runMessages = transcript
	output.Messages = transcript
	messages, err := a.rebuildMessages(ctx, sess, output)
	if err != nil {
		return a.finishRun(ctx, st, em, err)
	}
	st.messages = messages
	if err := a.checkpoint(ctx, st); err != nil {
		return a.finishRun(ctx, st, em, err)
	}
	firstRound := 0
	for _, m := range transcript {
		if m.Role == models.RoleAssistant {
			firstRound = orig.ModelRound + 1
			break
		}
	}
	return a.modelLoop(ctx, st, firstRound, em)
}

// settleInterruptedTools answers the tool calls of a trailing assistant
// message that have no result: idempotent tools run again, others get
// interruptedToolResult. Results keep the original call order.
func (a *Agent) settleInterruptedTools(ctx context.Context, st *runState, transcript []models.Message) ([]models.Message, error) {
	if len(transcript) == 0 {
		return []models.Message{{Role: models.RoleUser, Content: st.output.Input.Message}}, nil
	}
	last := transcript[len(transcript)-1]
	if last.Role != models.RoleAssistant || len(last.ToolCalls) == 0 {
		return transcript, nil
	}
	var replay []models.ToolCall
	for _, call := range last.ToolCalls {
		if tool, ok := st.toolMap[call.Function.Name]; ok && tools.IsIdempotent(tool) {
			replay = append(replay, call)
		}
	}
	replayed := map[string]models.Message{}
	if len(replay) > 0 {
		results, err := a.executeToolCalls(ctx, st.sess, st.output, st.toolMap, replay)
		if err != nil {
			return nil, err
		}
		for _, m := range results {
			replayed[m.ToolCallID] = m
		}
	}
	for _, call := range last.ToolCalls {
		if m, ok := replayed[call.ID]; ok {
			transcript = append(transcript, m)
			continue
		}
		transcript = append(transcript, models.Message{
			Role: models.RoleTool, ToolCallID: call.ID, Name: call.Function.Name, Content: interruptedToolResult,
		})
	}
	return transcript, nil
}
