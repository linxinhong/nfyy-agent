package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

var (
	ErrResumeNotFound        = errors.New("agent: resumable run not found")
	ErrResumeConflict        = errors.New("agent: resume conflict")
	ErrInvalidResume         = errors.New("agent: invalid resume request")
	ErrApprovalStoreRequired = errors.New("agent: durable storage is required for tool approval")
	// ErrFrozenApprovalStateRequired rejects historical paused managed runs
	// that cannot prove which frozen plan and principal were approved.
	ErrFrozenApprovalStateRequired = errors.New("agent: frozen approval state is required")
	// ErrTrustedIdentityNamespaceMismatch rejects contradicting trusted
	// identity and namespace contexts before persistence or tool preparation.
	ErrTrustedIdentityNamespaceMismatch = errors.New("agent: trusted principal and namespace differ")
)

// defaultApprovalTimeout is the safety-net deadline applied to approval-required
// tools that do not declare their own timeout, so a stalled approval cannot
// pend forever and P1 expiry semantics always apply.
const defaultApprovalTimeout = 24 * time.Hour

// Resume resolves the active requirement and durably continues the same run.
func (a *Agent) Resume(ctx context.Context, request session.ResumeRequest) (*session.RunOutput, error) {
	if err := a.requireNamespaceStore(ctx); err != nil {
		return nil, err
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

	resumeAgent := a.forRequest(request.UserID, "")
	if resumeAgent.cfg.RunTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, resumeAgent.cfg.RunTimeout)
		defer cancel()
	}
	return resumeAgent.resume(ctx, request)
}

// ResumeStream emits lifecycle events while resuming a paused run.
func (a *Agent) ResumeStream(ctx context.Context, request session.ResumeRequest) (<-chan Event, error) {
	if err := a.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	if !a.acquireRunSlot() {
		return nil, ErrBusy
	}
	ctx, permit, admissionErr := a.acquireAdmission(ctx)
	if admissionErr != nil {
		a.releaseRunSlot()
		return nil, admissionErr
	}
	resumeAgent := a.forRequest(request.UserID, "")
	streamCtx, cancel := context.WithCancel(ctx)
	writer, ch := newEventStream(streamCtx, cancel, resumeAgent.cfg.Stream)
	go func() {
		var runErr error
		defer func() { writer.Finish(runErr) }()
		defer permit.Release()
		defer a.releaseRunSlot()
		if resumeAgent.cfg.RunTimeout > 0 {
			var timeoutCancel context.CancelFunc
			streamCtx, timeoutCancel = context.WithTimeout(streamCtx, resumeAgent.cfg.RunTimeout)
			defer timeoutCancel()
		}
		// The shared model loop emits the terminal event once the run
		// continues; the emits below only cover failures before that point
		// (the writer keeps the first terminal event and drops later ones).
		output, err := resumeAgent.resumeWithClaim(streamCtx, request, emitter{w: &writer}, func() {
			emitAgentEvent(&writer, Event{Type: EventTypeRunResumed})
		})
		if err != nil {
			runErr = err
			emitAgentEvent(&writer, Event{Type: EventTypeRunError, Err: err, RunOutput: output})
			return
		}
		if output.Status == session.RunStatusPaused {
			emitAgentEvent(&writer, Event{
				Type: EventTypeRunPaused, RunOutput: output, Requirement: output.ActiveRequirement(),
			})
			return
		}
		if output.Status == session.RunStatusCompleted {
			emitAgentEvent(&writer, Event{Type: EventTypeRunCompleted, RunOutput: output})
		}
	}()
	return ch, nil
}

func (a *Agent) resume(ctx context.Context, request session.ResumeRequest) (*session.RunOutput, error) {
	return a.resumeWithClaim(ctx, request, emitter{}, nil)
}

func (a *Agent) resumeWithClaim(ctx context.Context, request session.ResumeRequest, em emitter, claimed func()) (*session.RunOutput, error) {
	if err := a.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.cfg.DB == nil {
		return nil, errors.Join(ErrInvalidResume, errors.New("durable storage is required"))
	}
	if request.RunID == "" || request.RequirementID == "" {
		return nil, errors.Join(ErrInvalidResume, errors.New("run_id and requirement_id are required"))
	}
	action, ok := requirement.ResolveAction(request.Action)
	if !ok {
		return nil, fmt.Errorf("%w: invalid action %q", ErrInvalidResume, request.Action)
	}

	run, err := a.cfg.DB.GetRun(ctx, db.RunQuery{RunID: request.RunID, UserID: a.cfg.UserID})
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	if run == nil || run.AgentID != a.ID() || (a.cfg.UserID != "" && run.UserID != a.cfg.UserID) {
		return nil, ErrResumeNotFound
	}
	if err := a.validateFrozenApprovalState(ctx, run); err != nil {
		return nil, err
	}
	reqIndex := requirementIndex(run, request.RequirementID)
	if reqIndex < 0 {
		return nil, ErrResumeNotFound
	}
	storedRequirement := &run.Requirements[reqIndex]
	if storedRequirement.IsResolved() {
		if run.Status == session.RunStatusRunning {
			return nil, ErrResumeConflict
		}
		if storedRequirement.Resolution != nil && storedRequirement.Resolution.Action != action {
			return nil, ErrResumeConflict
		}
		return run, nil
	}
	now := time.Now().UTC()
	if storedRequirement.IsExpiredAt(now) {
		action = requirement.ActionExpire
	}
	if run.Status != session.RunStatusPaused || run.ActiveRequirementID != request.RequirementID {
		return nil, ErrResumeConflict
	}

	storedRequirement.Status = statusForAction(action)
	storedRequirement.ResolvedAt = &now
	storedRequirement.ActorID = request.ActorID
	storedRequirement.Resolution = &requirement.Resolution{
		ActorID: request.ActorID,
		Action:  action,
		Input:   request.Input,
		At:      now,
	}
	run.Status = session.RunStatusRunning
	run.Error = ""
	run.UpdatedAt = now
	// The lease belongs to the process executing the run now; one that
	// does not heartbeat must not leave a lease the reaper would expire.
	run.LeaseTimeout = 0
	if store, timeout := a.leaseStore(ctx, run); store != nil {
		run.LeaseTimeout = timeout
	}
	// The paused->running claim and the resolution are one storage CAS.
	if err := a.cfg.DB.UpdateRun(ctx, run, session.RunStatusPaused); err != nil {
		if errors.Is(err, db.ErrConflict) {
			return nil, ErrResumeConflict
		}
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrResumeNotFound
		}
		return nil, fmt.Errorf("claim run: %w", err)
	}
	if claimed != nil {
		claimed()
	}

	result, continueErr := a.continueRun(ctx, run, action, em)
	if continueErr != nil && result != nil && result.Status == session.RunStatusRunning {
		result.Status = session.RunStatusError
		if ctx.Err() != nil {
			result.Status = session.RunStatusCancelled
		}
		result.Error = continueErr.Error()
		finishMetrics(result)
		if saveErr := a.updateExpectedRun(context.WithoutCancel(ctx), result, session.RunStatusRunning); saveErr != nil {
			continueErr = errors.Join(continueErr, saveErr)
		}
	}
	return result, continueErr
}

// validateFrozenApprovalState runs before the paused->running claim and
// before ResolveApproval/Execute. A missing historical plan must not cause a
// newly prepared invocation to masquerade as the plan a reviewer approved.
func (a *Agent) validateFrozenApprovalState(ctx context.Context, run *session.RunOutput) error {
	if a.cfg.ToolService == nil || run == nil || run.Status != session.RunStatusPaused {
		return nil
	}
	principal, err := service.PrincipalFromContext(ctx)
	if err != nil {
		return errors.Join(ErrFrozenApprovalStateRequired, err)
	}
	if run.Principal != principal {
		return ErrFrozenApprovalStateRequired
	}
	if namespace, scoped := execution.NamespaceFromContext(ctx); scoped && (namespace != principal.Namespace || run.Namespace != namespace) {
		return ErrFrozenApprovalStateRequired
	}
	if len(run.PendingToolCalls) == 0 || len(run.PendingPlans) != len(run.PendingToolCalls) {
		return ErrFrozenApprovalStateRequired
	}
	for _, call := range run.PendingToolCalls {
		ref, ok := run.PendingPlans[call.ID]
		if !ok || (ref.Denied == "" && (ref.ID == "" || ref.Version == "")) {
			return ErrFrozenApprovalStateRequired
		}
	}
	return nil
}

func statusForAction(action requirement.Action) requirement.Status {
	switch action {
	case requirement.ActionApprove:
		return requirement.StatusApproved
	case requirement.ActionReject:
		return requirement.StatusRejected
	default:
		return requirement.StatusExpired
	}
}

func (a *Agent) continueRun(ctx context.Context, run *session.RunOutput, action requirement.Action, em emitter) (*session.RunOutput, error) {
	ctx, unlock, err := a.lockSession(ctx, run.SessionID)
	if err != nil {
		return run, err
	}
	defer unlock()
	ctx = contextWithMetrics(ctx, &run.Metrics)
	sess, err := a.readSession(ctx, run.SessionID)
	if err != nil {
		return run, fmt.Errorf("load session: %w", err)
	}
	if sess == nil || sess.AgentID != run.AgentID || sess.UserID != run.UserID {
		return run, ErrResumeNotFound
	}
	a.cfg.SessionID = run.SessionID

	st := &runState{sess: sess, output: run}
	ctx, release := a.guardRun(ctx, st)
	defer release()
	if err := a.prepareModelOptions(st); err != nil {
		return run, err
	}
	if _, paused, err := a.resumeToolBatch(contextWithEmitter(ctx, em), sess, run, st.toolMap, action); err != nil || paused {
		if paused {
			em.emit(Event{Type: EventTypeRunPaused, RunOutput: run, Requirement: run.ActiveRequirement()})
		}
		return run, err
	}
	// Tool results of the resumed batch are already durable in run.Messages.
	messages, err := a.rebuildMessages(ctx, sess, run)
	if err != nil {
		return run, err
	}
	st.messages, st.runMessages = messages, run.Messages
	return a.modelLoop(ctx, st, run.ModelRound+1, em)
}

func (a *Agent) rebuildMessages(ctx context.Context, sess *session.AgentSession, run *session.RunOutput) ([]models.Message, error) {
	system, ephemeral, err := a.buildContextMessages(ctx, sess, run.Input.Message)
	if err != nil {
		return nil, err
	}
	request := ContextRequest{System: system, Ephemeral: ephemeral}
	request.History = sessionHistory(sess, run.RunID, a.cfg.MaxHistoryRuns)
	request.Current = cloneMessages(run.Messages)
	if len(request.Current) == 0 {
		request.Current = []models.Message{{Role: models.RoleUser, Content: run.Input.Message}}
	}
	request.Input = request.Current[0]
	if len(a.cfg.ContextProviders) > 0 {
		request, err = a.applyContextProviders(ctx, request)
		if err != nil {
			return nil, err
		}
	}
	return assembleContextRequest(request), nil
}

type batchResult struct {
	messages    []models.Message
	runMessages []models.Message
	paused      bool
}

// processToolBatch enforces the batch barrier: if any call needs approval,
// every call is deferred and the full batch is snapshotted before side effects.
func (a *Agent) processToolBatch(
	ctx context.Context,
	sess *session.AgentSession,
	output *session.RunOutput,
	toolMap map[string]tools.Tool,
	calls []models.ToolCall,
	messages, runMessages []models.Message,
	modelRound int,
) (*batchResult, error) {
	if a.cfg.ToolService != nil {
		return a.processManagedToolBatch(ctx, sess, output, toolMap, calls, messages, runMessages, modelRound)
	}
	requiredIndex := firstRequiredTool(calls, toolMap)
	if requiredIndex < 0 {
		toolMessages, err := a.executeBatchWithAudit(ctx, sess, output, toolMap, calls, modelRound, 0)
		if err != nil {
			return nil, err
		}
		return &batchResult{
			messages:    append(messages, toolMessages...),
			runMessages: append(runMessages, toolMessages...),
		}, nil
	}
	if a.cfg.DB == nil {
		return nil, ErrApprovalStoreRequired
	}

	output.Messages = runMessages
	output.PendingToolCalls = cloneToolCalls(calls)
	output.NextToolIndex = 0
	output.ModelRound = modelRound
	a.setPendingRequirement(output, calls[requiredIndex], modelRound, requiredIndex, toolMap[calls[requiredIndex].Function.Name])
	output.Status = session.RunStatusPaused
	if err := a.updateExpectedRun(ctx, output, session.RunStatusRunning); err != nil {
		return nil, fmt.Errorf("save paused run: %w", err)
	}
	return &batchResult{paused: true}, nil
}

// processManagedToolBatch is the managed branch of processToolBatch: it
// prepares every tool up front (freezing plans and learning approval
// posture), pauses with zero side effects if any tool requires approval, and
// otherwise executes the frozen plans.
func (a *Agent) processManagedToolBatch(
	ctx context.Context,
	sess *session.AgentSession,
	output *session.RunOutput,
	toolMap map[string]tools.Tool,
	calls []models.ToolCall,
	messages, runMessages []models.Message,
	modelRound int,
) (*batchResult, error) {
	plans := make(map[string]session.PendingPlanRef, len(calls))
	requiredIndex := -1
	for i := range calls {
		ref, required, err := a.prepareManagedTool(ctx, &calls[i])
		if errors.Is(err, service.ErrUnknownTool) || errors.Is(err, service.ErrForbidden) {
			// A policy denial is feedback for the model, not a run failure:
			// the call is frozen as denied and its tool result reports it.
			plans[calls[i].ID] = session.PendingPlanRef{Denied: err.Error()}
			continue
		}
		if err != nil {
			return nil, err
		}
		plans[calls[i].ID] = session.PendingPlanRef{ID: ref.ID, Version: ref.Version, ApprovalRequired: required}
		if required && requiredIndex < 0 {
			requiredIndex = i
		}
	}
	if requiredIndex >= 0 {
		if a.cfg.DB == nil {
			return nil, ErrApprovalStoreRequired
		}
		output.Messages = runMessages
		output.PendingToolCalls = cloneToolCalls(calls)
		output.PendingPlans = plans
		output.NextToolIndex = 0
		output.ModelRound = modelRound
		a.setPendingRequirement(output, calls[requiredIndex], modelRound, requiredIndex, toolMap[calls[requiredIndex].Function.Name])
		output.Status = session.RunStatusPaused
		if err := a.updateExpectedRun(ctx, output, session.RunStatusRunning); err != nil {
			return nil, fmt.Errorf("save paused run: %w", err)
		}
		return &batchResult{paused: true}, nil
	}
	// No approval required: execute the frozen plans. output.PendingPlans is
	// temporarily exposed so executeToolCalls reuses the same plan (no
	// double prepare), then cleared for the non-paused run.
	output.PendingPlans = plans
	defer func() { output.PendingPlans = nil }()
	toolMessages, err := a.executeBatchWithAudit(ctx, sess, output, toolMap, calls, modelRound, 0)
	if err != nil {
		return nil, err
	}
	return &batchResult{
		messages:    append(messages, toolMessages...),
		runMessages: append(runMessages, toolMessages...),
	}, nil
}

func (a *Agent) resumeToolBatch(
	ctx context.Context,
	sess *session.AgentSession,
	output *session.RunOutput,
	toolMap map[string]tools.Tool,
	action requirement.Action,
) ([]models.Message, bool, error) {
	active := output.ActiveRequirement()
	if active == nil {
		return nil, false, errors.Join(ErrResumeConflict, errors.New("active requirement is missing"))
	}
	var produced []models.Message
	for i := output.NextToolIndex; i < len(output.PendingToolCalls); i++ {
		call := output.PendingToolCalls[i]
		if i == active.ToolIndex && call.ID == active.ToolCall.ID {
			var toolMessages []models.Message
			if action == requirement.ActionApprove {
				// Managed: create the approval grant for the frozen plan
				// before executing it (RC-06 grant binding).
				if a.cfg.ToolService != nil {
					if approver, ok := a.cfg.ToolService.(service.Approver); ok {
						if ref, ok := output.PendingPlans[call.ID]; ok {
							if err := approver.ResolveApproval(ctx, ref.ID, active.ActorID, "approve"); err != nil {
								return produced, false, err
							}
						}
					}
				}
				var err error
				toolMessages, err = a.executeSingleTool(ctx, sess, output, toolMap, call)
				if err != nil {
					return produced, false, err
				}
			} else {
				toolMessages = []models.Message{rejectionMessage(call, action)}
				output.ToolExecutions = append(output.ToolExecutions, session.ToolExecution{
					CallID: call.ID, Name: call.Function.Name, Status: session.ToolExecutionStatusRejected,
				})
				output.Metrics.ToolCalls++
			}
			produced = append(produced, toolMessages...)
			output.Messages = append(output.Messages, toolMessages...)
			output.NextToolIndex = i + 1
			if err := a.updateExpectedRun(ctx, output, session.RunStatusRunning); err != nil {
				return produced, false, err
			}
			continue
		}

		tool, exists := toolMap[call.Function.Name]
		required := exists && effectiveApproval(tool) == tools.ApprovalRequired
		if a.cfg.ToolService != nil {
			// Managed posture is dynamic (authorizer) and frozen at prepare
			// time in PendingPlans.
			if ref, ok := output.PendingPlans[call.ID]; ok {
				required = ref.ApprovalRequired
			}
		}
		if required {
			output.NextToolIndex = i
			a.setPendingRequirement(output, call, output.ModelRound, i, tool)
			output.Status = session.RunStatusPaused
			if err := a.updateExpectedRun(ctx, output, session.RunStatusRunning); err != nil {
				return produced, false, err
			}
			return produced, true, nil
		}

		toolMessages, err := a.executeSingleTool(ctx, sess, output, toolMap, call)
		if err != nil {
			return produced, false, err
		}
		if a.cfg.ToolService == nil && exists && effectiveApproval(tool) == tools.ApprovalAudit {
			a.appendAuditRequirement(output, call, output.ModelRound, i)
		}
		produced = append(produced, toolMessages...)
		output.Messages = append(output.Messages, toolMessages...)
		output.NextToolIndex = i + 1
		if err := a.updateExpectedRun(ctx, output, session.RunStatusRunning); err != nil {
			return produced, false, err
		}
	}
	output.PendingToolCalls = nil
	output.NextToolIndex = 0
	output.ActiveRequirementID = ""
	if err := a.updateExpectedRun(ctx, output, session.RunStatusRunning); err != nil {
		return produced, false, err
	}
	return produced, false, nil
}

func (a *Agent) executeBatchWithAudit(
	ctx context.Context,
	sess *session.AgentSession,
	output *session.RunOutput,
	toolMap map[string]tools.Tool,
	calls []models.ToolCall,
	modelRound, baseIndex int,
) ([]models.Message, error) {
	messages, err := a.executeToolCalls(ctx, sess, output, toolMap, calls)
	for i := range messages {
		if tool, ok := toolMap[calls[i].Function.Name]; ok && effectiveApproval(tool) == tools.ApprovalAudit {
			a.appendAuditRequirement(output, calls[i], modelRound, baseIndex+i)
		}
	}
	return messages, err
}

func (a *Agent) executeSingleTool(
	ctx context.Context,
	sess *session.AgentSession,
	output *session.RunOutput,
	toolMap map[string]tools.Tool,
	call models.ToolCall,
) ([]models.Message, error) {
	return a.executeToolCalls(ctx, sess, output, toolMap, []models.ToolCall{call})
}

func firstRequiredTool(calls []models.ToolCall, toolMap map[string]tools.Tool) int {
	for i, call := range calls {
		if tool, ok := toolMap[call.Function.Name]; ok && effectiveApproval(tool) == tools.ApprovalRequired {
			return i
		}
	}
	return -1
}

func effectiveApproval(tool tools.Tool) tools.ApprovalKind {
	switch tools.ApprovalOf(tool) {
	case tools.ApprovalNone, tools.ApprovalAudit, tools.ApprovalRequired:
		return tools.ApprovalOf(tool)
	default:
		// Unknown policy values fail closed.
		return tools.ApprovalRequired
	}
}

func (a *Agent) setPendingRequirement(
	output *session.RunOutput,
	call models.ToolCall,
	modelRound, toolIndex int,
	tool tools.Tool,
) {
	now := time.Now().UTC()
	var deadline *time.Time
	if timeout := tools.ApprovalTimeoutOf(tool); timeout > 0 {
		value := now.Add(timeout)
		deadline = &value
	} else if effectiveApproval(tool) == tools.ApprovalRequired {
		// Safety default: an approval-required tool without a configured
		// timeout still gets a deadline so the requirement can expire.
		value := now.Add(defaultApprovalTimeout)
		deadline = &value
	}
	req := requirement.Requirement{
		ID:    requirement.StableID(output.RunID, modelRound, toolIndex, call.ID),
		RunID: output.RunID, AgentID: output.AgentID, SessionID: output.SessionID, UserID: output.UserID,
		Kind: requirement.KindToolApproval, ToolCall: requirement.SnapshotToolCall(call),
		ModelRound: modelRound, ToolIndex: toolIndex, Deadline: deadline, Status: requirement.StatusPending, CreatedAt: now,
	}
	output.SetActiveRequirement(req)
}

func (a *Agent) appendAuditRequirement(output *session.RunOutput, call models.ToolCall, modelRound, toolIndex int) {
	now := time.Now().UTC()
	req := requirement.Requirement{
		ID:    requirement.StableID(output.RunID, modelRound, toolIndex, call.ID),
		RunID: output.RunID, AgentID: output.AgentID, SessionID: output.SessionID, UserID: output.UserID,
		Kind: requirement.KindToolAudit, ToolCall: requirement.SnapshotToolCall(call),
		ModelRound: modelRound, ToolIndex: toolIndex, Status: requirement.StatusApproved, ActorID: "runtime", CreatedAt: now, ResolvedAt: &now,
		Resolution: &requirement.Resolution{ActorID: "runtime", Action: requirement.ActionApprove, At: now},
	}
	for i := range output.Requirements {
		if output.Requirements[i].ID == req.ID {
			output.Requirements[i] = req
			return
		}
	}
	output.Requirements = append(output.Requirements, req)
}

func rejectionMessage(call models.ToolCall, action requirement.Action) models.Message {
	return models.Message{
		Role: models.RoleTool, ToolCallID: call.ID, Name: call.Function.Name,
		Content: fmt.Sprintf("tool call %s: approval was not granted", action),
	}
}

func requirementIndex(run *session.RunOutput, id string) int {
	for i := range run.Requirements {
		if run.Requirements[i].ID == id {
			return i
		}
	}
	return -1
}

func cloneToolCalls(calls []models.ToolCall) []models.ToolCall {
	if calls == nil {
		return nil
	}
	cloned := make([]models.ToolCall, len(calls))
	copy(cloned, calls)
	return cloned
}

func (a *Agent) updateExpectedRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	if owner, managed, err := managedRootOwner(ctx, output); managed {
		if err != nil {
			return err
		}
		output.UpdatedAt = time.Now().UTC()
		_, err = owner.Update(ctx, output, expected)
		return err
	}
	if a.cfg.DB == nil {
		return nil
	}
	output.UpdatedAt = time.Now().UTC()
	return a.cfg.DB.UpdateRun(context.WithoutCancel(ctx), output, expected)
}
