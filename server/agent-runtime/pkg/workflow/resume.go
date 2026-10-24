package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

func (w *Workflow) Resume(ctx context.Context, request ResumeRequest) (*session.RunOutput, error) {
	return w.resumeCore(ctx, request, nil)
}

type resumeRunStore struct {
	legacy db.WorkflowStore
	owner  *runmanager.ManagedRunOwner
	limits *Limits
}

func (s resumeRunStore) get(ctx context.Context, request ResumeRequest) (*session.RunOutput, error) {
	if s.owner != nil {
		snapshot, err := s.owner.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		if request.UserID != "" && snapshot.Run.UserID != request.UserID {
			return nil, db.ErrNotFound
		}
		return snapshot.Run, nil
	}
	return s.legacy.GetRun(ctx, db.RunQuery{RunID: request.RunID, UserID: request.UserID})
}

func (s resumeRunStore) update(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	if s.limits != nil {
		if err := validateRunEnvelope(output, s.limits.MaxRunEnvelopeBytes); err != nil {
			return err
		}
	}
	if s.owner != nil {
		var err error
		if s.limits != nil {
			_, err = s.owner.UpdateBounded(ctx, output, expected)
		} else {
			_, err = s.owner.Update(ctx, output, expected)
		}
		return err
	}
	return s.legacy.UpdateRun(ctx, output, expected)
}

func (w *Workflow) resumeCore(ctx context.Context, request ResumeRequest, emit func(Event)) (result *session.RunOutput, resultErr error) {
	executionStartedAt := time.Now()
	if !w.core || (w.cfg.DB == nil && w.cfg.ManagedResume == nil) {
		return nil, errors.New("workflow: durable store is required to resume")
	}
	store := resumeRunStore{legacy: w.cfg.DB, limits: w.cfg.Limits}
	var owner *runmanager.ManagedRunOwner
	releaseSafe := true
	cleanupOwner := func(release bool) error {
		if owner == nil {
			return nil
		}
		timeout := 5 * time.Second
		if w.cfg.ManagedResume != nil && w.cfg.ManagedResume.Lease > 0 && w.cfg.ManagedResume.Lease < timeout {
			timeout = w.cfg.ManagedResume.Lease
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if release && owner.OwnershipError() == nil {
			return owner.Release(cleanupCtx)
		}
		return owner.StopHeartbeat(cleanupCtx)
	}
	if w.cfg.ManagedResume != nil {
		configured := *w.cfg.ManagedResume
		var err error
		owner, err = runmanager.AcquireManagedRunOwner(ctx, request.RunID, configured)
		if err != nil {
			return nil, err
		}
		store.owner = owner
		ctx = owner.Context()
		defer func() {
			if owner == nil {
				return
			}
			resultErr = errors.Join(resultErr, cleanupOwner(releaseSafe), owner.OwnershipError())
		}()
	} else if err := w.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	stored, err := store.get(ctx, request)
	if err != nil {
		return nil, err
	}
	if stored == nil || stored.Workflow == nil || stored.Workflow.Legacy {
		return nil, db.ErrNotFound
	}
	if w.cfg.Limits != nil {
		if err := validateRunEnvelope(stored, w.cfg.Limits.MaxRunEnvelopeBytes); err != nil {
			return stored, err
		}
		if err := validatePayloadJSON("input", stored.Workflow.Input, w.cfg.Limits.MaxInputBytes); err != nil {
			return stored, err
		}
		if err := validatePayloadJSON("state", stored.Workflow.State, w.cfg.Limits.MaxStateBytes); err != nil {
			return stored, err
		}
		if err := validatePayloadJSON("resume input", request.Input, w.cfg.Limits.MaxInputBytes); err != nil {
			return stored, err
		}
		if stored.Workflow.Definition == nil {
			return stored, db.ErrNotFound
		}
		frozen, limitErr := prepareLimitedDefinition(*stored.Workflow.Definition, *w.cfg.Limits)
		if limitErr != nil {
			return stored, limitErr
		}
		if err := validateDefinition(frozen, w.cfg.Registry); err != nil {
			return stored, err
		}
		stored.Workflow.Definition = &frozen
		if err := normalizeRunEnvelopeRaw(stored, w.cfg.Limits.MaxRunEnvelopeBytes); err != nil {
			return stored, err
		}
	}
	metadata, err := session.CloneRunMetadata(stored.Metadata)
	if err != nil {
		return stored, err
	}
	stored.Metadata = metadata
	ctx = session.ContextWithRunMetadata(ctx, metadata)
	if stored.WorkflowID != w.ID() || stored.Workflow.Definition == nil || stored.Workflow.Definition.ID != w.ID() {
		return stored, db.ErrNotFound
	}
	definition := stored.Workflow.Definition
	var resolvedRequirement *workflowschema.Requirement
	var resumedNodeID string
	if stored.Status == session.RunStatusPaused {
		requirement := stored.Workflow.Requirement
		if requirement == nil {
			return stored, db.ErrConflict
		}
		if requirement.ID != request.RequirementID {
			return stored, db.ErrConflict
		}
		if requirement.Deadline != nil && time.Now().UTC().After(*requirement.Deadline) {
			requirement.Resolution = &workflowschema.Resolution{ActorID: request.ActorID, Action: "expire", At: time.Now().UTC()}
			updateRequirementHistory(stored.Workflow, requirement)
			stored.Status = session.RunStatusCancelled
			stored.Error = "workflow requirement expired"
			stored.UpdatedAt = time.Now().UTC()
			stored.Metrics.DurationMS += time.Since(executionStartedAt).Milliseconds()
			if err := store.update(ctx, stored, session.RunStatusPaused); err != nil {
				return stored, err
			}
			if owner != nil {
				cleanupErr := cleanupOwner(true)
				owner = nil
				if cleanupErr != nil {
					return stored, cleanupErr
				}
			}
			if emit != nil {
				resolved := newEvent(EventTypeRequirementResolved, stored, requirement.NodeID, "", nil)
				resolved.Payload = requirement
				emit(resolved)
				emit(newEvent(EventTypeRunCancelled, stored, requirement.NodeID, "", nil))
			}
			return stored, nil
		}
		if !validResumeAction(requirement.Kind, request.Action) {
			return stored, fmt.Errorf("workflow: invalid resume action %q for %s", request.Action, requirement.Kind)
		}
		if request.Action != ResumeActionReject {
			if err := validateDefinition(*definition, w.cfg.Registry); err != nil {
				return stored, err
			}
		}
		requirement.Resolution = &workflowschema.Resolution{ActorID: request.ActorID, Action: string(request.Action), Input: request.Input, At: time.Now().UTC()}
		updateRequirementHistory(stored.Workflow, requirement)
		if request.Action == ResumeActionReject {
			stored.Status = session.RunStatusCancelled
			stored.Error = "workflow requirement rejected"
			stored.UpdatedAt = time.Now().UTC()
			stored.Metrics.DurationMS += time.Since(executionStartedAt).Milliseconds()
			if err := store.update(ctx, stored, session.RunStatusPaused); err != nil {
				return stored, err
			}
			if owner != nil {
				cleanupErr := cleanupOwner(true)
				owner = nil
				if cleanupErr != nil {
					return stored, cleanupErr
				}
			}
			if emit != nil {
				resolved := newEvent(EventTypeRequirementResolved, stored, requirement.NodeID, "", nil)
				resolved.Payload = requirement
				emit(resolved)
				emit(newEvent(EventTypeRunCancelled, stored, requirement.NodeID, "", nil))
			}
			return stored, nil
		}
		resolvedRequirement = requirement
		resumedNodeID = requirement.NodeID
	} else if applyingNodeID := findApplyingNodeID(stored); applyingNodeID != "" {
		// The run crashed while a step was applying: the step's side
		// effects may have happened. Fail closed unless the caller
		// explicitly opts into re-running the step.
		if request.Action != ResumeActionRetryApplying {
			// Fail closed. The conflict wrapper keeps concurrent-resume
			// callers seeing a CAS conflict while the unknown-outcome
			// wrapper signals why the run is not resumable normally.
			return stored, fmt.Errorf("%w: %w: node %q", db.ErrConflict, ErrApplyingUnknown, applyingNodeID)
		}
		if err := clearApplyingNode(stored, applyingNodeID); err != nil {
			return stored, err
		}
		resumedNodeID = applyingNodeID
	} else if owner != nil && stored.Status == session.RunStatusRunning {
		requirement, takeoverErr := managedTakeoverRequirement(stored, request)
		if takeoverErr != nil {
			return stored, takeoverErr
		}
		resumedNodeID = requirement.NodeID
	} else {
		return stored, db.ErrConflict
	}
	if err := validateDefinition(*definition, w.cfg.Registry); err != nil {
		return stored, err
	}
	prevStatus := stored.Status
	stored.Status = session.RunStatusRunning
	stored.Error = ""
	stored.UpdatedAt = time.Now().UTC()
	if owner == nil {
		w.markLease(stored)
	}
	if err := store.update(ctx, stored, prevStatus); err != nil {
		return stored, err
	}
	var cancelRun context.CancelCauseFunc
	if owner == nil {
		var stopLease func()
		ctx, cancelRun, stopLease = w.guardLease(ctx, stored)
		defer stopLease()
	}
	if owner != nil {
		releaseSafe = false
	}
	if emit != nil {
		if resolvedRequirement != nil {
			resolved := newEvent(EventTypeRequirementResolved, stored, resolvedRequirement.NodeID, "", nil)
			resolved.Payload = resolvedRequirement
			emit(resolved)
		}
		emit(newEvent(EventTypeRunResumed, stored, resumedNodeID, "", nil))
	}
	runner := *w
	runner.cfg.Definition = *definition
	previous := make(map[string]NodeResult)
	collectResults(stored.Workflow.RootResult, previous)
	for nodeID, result := range stored.Workflow.NodeResults {
		previous[nodeID] = result
	}
	resumeInputs := make(map[string]any)
	if stored.Workflow.Requirement != nil {
		resumeInput := request.Input
		if stored.Workflow.Requirement.Resolution != nil {
			resumeInput = stored.Workflow.Requirement.Resolution.Input
		}
		resumeInputs[stored.Workflow.Requirement.NodeID] = resumeInput
	} else if request.Action == ResumeActionRetryApplying {
		// The live Requirement is cleared before the applying marker is
		// persisted. Its resolved input remains in history so an explicit
		// retry of that same reviewed step does not pause for a new decision.
		resumeInputs[resumedNodeID] = resolvedResumeInput(stored.Workflow, resumedNodeID)
	}
	stored.Workflow.Requirement = nil
	stored.Workflow.Checkpoint = nil
	if stored.Workflow.NodeResults == nil {
		stored.Workflow.NodeResults = make(map[string]NodeResult)
	}
	progress := &progressRecorder{store: runner.cfg.DB, owner: owner, output: stored, results: stored.Workflow.NodeResults, limits: runner.cfg.Limits, onConflict: cancelRun}
	runContext, err := newNodeRunContext(stored)
	if err != nil {
		return stored, err
	}
	ex := &execution{
		w: &runner, runContext: runContext, output: stored, state: cloneExecutionState(stored.Workflow.State, runner.cfg.Limits != nil),
		resume: resumeInputs, previous: previous, progress: progress,
	}
	activeCtx, cancelActive := executionContext(ctx, runner.cfg.Limits)
	defer cancelActive()
	nodeResult, runErr := ex.execute(activeCtx, definition.Root, stored.Workflow.Input, "", emit)
	stored.Workflow.RootResult = &nodeResult
	stored.Workflow.State = cloneExecutionState(ex.state, runner.cfg.Limits != nil)
	stored.UpdatedAt = time.Now().UTC()
	if errors.Is(runErr, errWorkflowPaused) {
		stored.Status = session.RunStatusPaused
		stored.Error = ""
		runErr = nil
	} else if runErr != nil && activeCtx.Err() != nil {
		stored.Status = session.RunStatusCancelled
		runErr = cancelledError(activeCtx)
		stored.Error = runErr.Error()
	} else if runErr != nil {
		stored.Status = session.RunStatusError
		stored.Error = runErr.Error()
	} else {
		stored.Status = session.RunStatusCompleted
		stored.Workflow.Output = nodeResult.Output
		stored.Parsed = nodeResult.Output
		stored.Content = valueString(nodeResult.Output)
	}
	stored.Metrics.DurationMS += time.Since(executionStartedAt).Milliseconds()
	persistCtx, cancelPersist := persistenceContext(activeCtx, runner.cfg.Limits)
	defer cancelPersist()
	if err := store.update(persistCtx, stored, session.RunStatusRunning); err != nil && !(owner == nil && settledElsewhere(persistCtx, runner.cfg.DB, stored, err)) {
		if emit != nil {
			emit(newEvent(EventTypeRunError, stored, "", "", err))
		}
		return stored, err
	}
	if owner != nil {
		releaseSafe = true
		cleanupErr := cleanupOwner(true)
		owner = nil
		if cleanupErr != nil {
			if emit != nil {
				emit(newEvent(EventTypeRunError, stored, "", "", cleanupErr))
			}
			return stored, cleanupErr
		}
	}
	if emit != nil {
		switch stored.Status {
		case session.RunStatusPaused:
			requirementEvent := newEvent(EventTypeRequirementCreated, stored, stored.Workflow.Checkpoint.PausedNodeID, "", nil)
			requirementEvent.Payload = stored.Workflow.Requirement
			emit(requirementEvent)
			emit(newEvent(EventTypeRunPaused, stored, stored.Workflow.Checkpoint.PausedNodeID, "", nil))
		case session.RunStatusCompleted:
			emit(newEvent(EventTypeRunCompleted, stored, "", "", nil))
		case session.RunStatusCancelled:
			emit(newEvent(EventTypeRunCancelled, stored, "", "", runErr))
		default:
			emit(newEvent(EventTypeRunError, stored, "", "", runErr))
		}
	}
	return stored, classifyExecutionError(runErr)
}

// ResumeEvents resumes a paused workflow and emits workflow-scoped events.
func (w *Workflow) ResumeEvents(ctx context.Context, request ResumeRequest) (<-chan Event, error) {
	if !w.core || (w.cfg.DB == nil && w.cfg.ManagedResume == nil) {
		return nil, errors.New("workflow: durable store is required to resume")
	}
	if w.cfg.ManagedResume == nil {
		if err := w.requireNamespaceStore(ctx); err != nil {
			return nil, err
		}
	}
	return streamEvents(ctx, w.cfg.Stream, func(streamCtx context.Context, emit func(Event)) (*session.RunOutput, error) {
		return w.resumeCore(streamCtx, request, emit)
	}), nil
}

func managedTakeoverRequirement(output *session.RunOutput, request ResumeRequest) (*workflowschema.Requirement, error) {
	if output == nil || output.Workflow == nil || output.Workflow.Requirement == nil || output.Workflow.Checkpoint == nil {
		return nil, db.ErrConflict
	}
	requirement := output.Workflow.Requirement
	resolution := requirement.Resolution
	if resolution == nil || requirement.ID != request.RequirementID || output.Workflow.Checkpoint.PausedNodeID != requirement.NodeID ||
		resolution.Action != string(request.Action) || !validResumeAction(requirement.Kind, request.Action) {
		return nil, db.ErrConflict
	}
	same, err := sameResumeInput(resolution.Input, request.Input)
	if err != nil {
		return nil, fmt.Errorf("workflow: compare managed resume input: %w", err)
	}
	if !same {
		return nil, db.ErrConflict
	}
	return requirement, nil
}

func validResumeAction(kind workflowschema.RequirementKind, action ResumeAction) bool {
	return kind == workflowschema.RequirementConfirmation && (action == ResumeActionApprove || action == ResumeActionReject) ||
		kind == workflowschema.RequirementUserInput && action == ResumeActionSubmit
}

func sameResumeInput(stored, requested any) (bool, error) {
	storedJSON, err := json.Marshal(stored)
	if err != nil {
		return false, err
	}
	requestedJSON, err := json.Marshal(requested)
	if err != nil {
		return false, err
	}
	return bytes.Equal(storedJSON, requestedJSON), nil
}

func resolvedResumeInput(data *workflowschema.RunData, nodeID string) any {
	if data == nil {
		return nil
	}
	for index := len(data.RequirementHistory) - 1; index >= 0; index-- {
		requirement := data.RequirementHistory[index]
		if requirement.NodeID == nodeID && requirement.Resolution != nil {
			return requirement.Resolution.Input
		}
	}
	return nil
}

// findApplyingNodeID returns the ID of the first node recorded as applying
// (a crash left its outcome unknown), or "" if there is none.
func findApplyingNodeID(output *session.RunOutput) string {
	if output == nil || output.Workflow == nil {
		return ""
	}
	for nodeID, result := range output.Workflow.NodeResults {
		if result.Status == workflowschema.NodeStatusApplying {
			return nodeID
		}
	}
	return ""
}

// clearApplyingNode removes an applying claim so the step can re-run.
func clearApplyingNode(output *session.RunOutput, nodeID string) error {
	if output == nil || output.Workflow == nil {
		return nil
	}
	if nodeID != "" {
		delete(output.Workflow.NodeResults, nodeID)
	}
	return nil
}

func collectResults(root *NodeResult, destination map[string]NodeResult) {
	if root == nil {
		return
	}
	destination[root.NodeID] = *root
	for i := range root.Children {
		collectResults(&root.Children[i], destination)
	}
}

func updateRequirementHistory(data *workflowschema.RunData, requirement *workflowschema.Requirement) {
	if data == nil || requirement == nil {
		return
	}
	for i := range data.RequirementHistory {
		if data.RequirementHistory[i].ID == requirement.ID {
			data.RequirementHistory[i] = *requirement
			return
		}
	}
	data.RequirementHistory = append(data.RequirementHistory, *requirement)
}
