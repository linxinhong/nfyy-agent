package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

type execution struct {
	w          *Workflow
	runContext nodeRunContext
	output     *session.RunOutput
	state      map[string]any
	resume     map[string]any
	previous   map[string]NodeResult
	progress   *progressRecorder
}

type nodeRunContext struct {
	runID      string
	workflowID string
	sessionID  string
	userID     string
	metadata   map[string]string
}

func newNodeRunContext(output *session.RunOutput) (nodeRunContext, error) {
	metadata, err := session.CloneRunMetadata(output.Metadata)
	if err != nil {
		return nodeRunContext{}, err
	}
	return nodeRunContext{
		runID:      output.RunID,
		workflowID: output.WorkflowID,
		sessionID:  output.SessionID,
		userID:     output.UserID,
		metadata:   metadata,
	}, nil
}

type progressRecorder struct {
	mu      sync.Mutex
	store   db.WorkflowStore
	owner   *runmanager.ManagedRunOwner
	output  *session.RunOutput
	results map[string]NodeResult
	limits  *Limits
	// onConflict cancels the run when a checkpoint finds it is no longer
	// running (cancelled on another replica).
	onConflict context.CancelCauseFunc
}

func executionContext(ctx context.Context, limits *Limits) (context.Context, context.CancelFunc) {
	if limits == nil || limits.RunTimeout == 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, limits.RunTimeout)
}

func persistenceContext(ctx context.Context, limits *Limits) (context.Context, context.CancelFunc) {
	if limits == nil {
		return context.WithoutCancel(ctx), func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), limits.PersistenceTimeout)
}

func (r *progressRecorder) record(ctx context.Context, result NodeResult) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store == nil && r.owner == nil {
		r.results[result.NodeID] = result
		r.output.Workflow.NodeResults = r.results
		return nil
	}
	var snapshot *session.RunOutput
	if r.limits != nil {
		if err := validateRunEnvelope(r.output, r.limits.MaxRunEnvelopeBytes); err != nil {
			return err
		}
		var err error
		snapshot, err = cloneRunOutputStrict(r.output)
		if err != nil {
			return fmt.Errorf("workflow: clone limited progress: %w", err)
		}
		if snapshot.Workflow.NodeResults == nil {
			snapshot.Workflow.NodeResults = make(map[string]NodeResult)
		}
		snapshot.Workflow.NodeResults[result.NodeID] = result
		if err := validateRunEnvelope(snapshot, r.limits.MaxRunEnvelopeBytes); err != nil {
			return err
		}
	} else {
		r.results[result.NodeID] = result
		r.output.Workflow.NodeResults = r.results
		snapshot = cloneRunOutput(r.output)
	}
	persistCtx, cancel := persistenceContext(ctx, r.limits)
	defer cancel()
	if r.owner != nil {
		var err error
		if r.limits != nil {
			_, err = r.owner.UpdateBounded(persistCtx, snapshot, session.RunStatusRunning)
		} else {
			_, err = r.owner.Update(ctx, snapshot, session.RunStatusRunning)
		}
		if err != nil {
			return &persistenceError{operation: "save managed workflow node result", cause: err}
		}
		if r.limits != nil {
			r.results[result.NodeID] = result
			r.output.Workflow.NodeResults = r.results
		}
		return nil
	}
	if err := r.store.UpdateRun(persistCtx, snapshot, session.RunStatusRunning); err != nil {
		if errors.Is(err, db.ErrConflict) && r.onConflict != nil {
			r.onConflict(agent.ErrRunCancelledExternally)
		}
		return &persistenceError{operation: "save workflow node result", cause: err}
	}
	if r.limits != nil {
		r.results[result.NodeID] = result
		r.output.Workflow.NodeResults = r.results
	}
	return nil
}

func (w *Workflow) runCore(ctx context.Context, request RunRequest, emit func(Event)) (*session.RunOutput, error) {
	executionStartedAt := time.Now()
	if !w.core {
		return nil, errors.New("workflow: structured definition is not configured")
	}
	if request.Metadata == nil {
		request.Metadata, _ = session.RunMetadataFromContext(ctx)
	}
	if w.cfg.Limits != nil {
		if err := validatePayloadJSON("metadata", request.Metadata, w.cfg.Limits.MaxInputBytes); err != nil {
			return nil, err
		}
	}
	metadata, err := session.CloneRunMetadata(request.Metadata)
	if err != nil {
		return nil, err
	}
	request.Metadata = metadata
	ctx = session.ContextWithRunMetadata(ctx, metadata)
	if w.cfg.Limits != nil {
		if err := validatePayloadJSON("input", request.Input, w.cfg.Limits.MaxInputBytes); err != nil {
			return nil, err
		}
		if err := validatePayloadJSON("state", w.cfg.SessionState, w.cfg.Limits.MaxStateBytes); err != nil {
			return nil, err
		}
	}
	if _, err := json.Marshal(request.Input); err != nil {
		return nil, fmt.Errorf("workflow: input is not JSON serializable: %w", err)
	}
	runID := request.RunID
	if runID == "" {
		runID = uuid.NewString()
	}
	runner := *w
	if request.UserID != "" {
		runner.cfg.UserID = request.UserID
	}
	if request.SessionID != "" {
		runner.cfg.SessionID = request.SessionID
	}
	sess, err := runner.loadOrCreateSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("load workflow session: %w", err)
	}
	if sess.WorkflowID != runner.ID() || sess.UserID != runner.cfg.UserID {
		return nil, errors.New("workflow: session does not belong to workflow and user")
	}
	if runner.cfg.Limits != nil {
		if err := validatePayloadJSON("state", sess.SessionState, runner.cfg.Limits.MaxStateBytes); err != nil {
			return nil, err
		}
	}
	sessionID := sess.SessionID
	def := runner.cfg.Definition
	if runner.cfg.Limits != nil {
		def, err = cloneLimitedDefinition(def, runner.cfg.Limits.MaxDefinitionBytes)
		if err != nil {
			return nil, err
		}
	}
	hash, err := definitionHash(def)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := &session.RunOutput{
		RunID: runID, WorkflowID: def.ID, SessionID: sessionID, UserID: runner.cfg.UserID,
		Metadata: metadata,
		Input:    session.RunInput{Message: valueString(request.Input)}, Status: session.RunStatusRunning,
		CreatedAt: now, UpdatedAt: now,
		Workflow: &workflowschema.RunData{SchemaVersion: 1, DefinitionVersion: def.Version, DefinitionHash: hash, Definition: &def, Input: request.Input, State: cloneExecutionState(sess.SessionState, runner.cfg.Limits != nil), NodeResults: make(map[string]NodeResult)},
	}
	runContext, err := newNodeRunContext(out)
	if err != nil {
		return out, err
	}
	if runner.cfg.Limits != nil {
		if err := validateRunEnvelope(out, runner.cfg.Limits.MaxRunEnvelopeBytes); err != nil {
			return out, err
		}
	}
	runner.markLease(out)
	if err := runner.persistRun(ctx, out); err != nil {
		return out, fmt.Errorf("start workflow run: %w", err)
	}
	ctx, cancelRun, stopLease := runner.guardLease(ctx, out)
	defer stopLease()
	progress := &progressRecorder{store: runner.cfg.DB, output: out, results: out.Workflow.NodeResults, limits: runner.cfg.Limits, onConflict: cancelRun}
	ex := &execution{w: &runner, runContext: runContext, output: out, state: cloneExecutionState(sess.SessionState, runner.cfg.Limits != nil), resume: make(map[string]any), previous: make(map[string]NodeResult), progress: progress}
	if emit != nil {
		emit(newEvent(EventTypeRunStarted, out, "", "", nil))
	}
	activeCtx, cancelActive := executionContext(ctx, runner.cfg.Limits)
	defer cancelActive()
	result, runErr := ex.execute(activeCtx, def.Root, request.Input, "", emit)
	if runErr == nil && runner.cfg.Limits != nil {
		if err := validatePayloadJSON("node output", result.Output, runner.cfg.Limits.MaxNodeOutputBytes); err != nil {
			runErr = err
		}
		if runErr == nil {
			runErr = validatePayloadJSON("state", ex.state, runner.cfg.Limits.MaxStateBytes)
		}
	}
	out.Workflow.RootResult = &result
	out.UpdatedAt = time.Now().UTC()
	if errors.Is(runErr, errWorkflowPaused) {
		out.Status = session.RunStatusPaused
		out.Error = ""
		runErr = nil
	} else if runErr != nil && activeCtx.Err() != nil {
		out.Status = session.RunStatusCancelled
		runErr = cancelledError(activeCtx)
		out.Error = runErr.Error()
	} else if runErr != nil {
		out.Status = session.RunStatusError
		out.Error = runErr.Error()
	} else {
		out.Status = session.RunStatusCompleted
		out.Workflow.Output = result.Output
		out.Parsed = result.Output
		out.Content = valueString(result.Output)
	}
	out.Workflow.State = cloneExecutionState(ex.state, runner.cfg.Limits != nil)
	out.Metrics.DurationMS += time.Since(executionStartedAt).Milliseconds()
	if runner.cfg.Limits != nil {
		if err := validateRunEnvelope(out, runner.cfg.Limits.MaxRunEnvelopeBytes); err != nil {
			return out, err
		}
	}
	if runner.cfg.DB != nil {
		persistCtx, cancel := persistenceContext(activeCtx, runner.cfg.Limits)
		defer cancel()
		if err := runner.cfg.DB.UpdateRun(persistCtx, out, session.RunStatusRunning); err != nil && !settledElsewhere(persistCtx, runner.cfg.DB, out, err) {
			if emit != nil {
				emit(newEvent(EventTypeRunError, out, "", "", err))
			}
			return out, fmt.Errorf("save workflow run: %w", err)
		}
	}
	if emit != nil {
		if out.Status == session.RunStatusPaused {
			requirementEvent := newEvent(EventTypeRequirementCreated, out, out.Workflow.Checkpoint.PausedNodeID, "", nil)
			requirementEvent.Payload = out.Workflow.Requirement
			emit(requirementEvent)
			emit(newEvent(EventTypeRunPaused, out, out.Workflow.Checkpoint.PausedNodeID, "", nil))
		} else if out.Status == session.RunStatusCancelled {
			emit(newEvent(EventTypeRunCancelled, out, "", "", runErr))
		} else if runErr != nil {
			emit(newEvent(EventTypeRunError, out, "", "", runErr))
		} else {
			emit(newEvent(EventTypeRunCompleted, out, "", "", nil))
		}
	}
	return out, classifyExecutionError(runErr)
}

// Structured reports whether the workflow uses a serializable Definition.
func (w *Workflow) Structured() bool { return w.core }

// RunEvents executes a structured workflow and emits workflow-scoped events.
func (w *Workflow) RunEvents(ctx context.Context, request RunRequest) (<-chan Event, error) {
	if !w.core {
		return nil, errors.New("workflow: structured definition is not configured")
	}
	if err := w.requireNamespaceStore(ctx); err != nil {
		return nil, err
	}
	return streamEvents(ctx, w.cfg.Stream, func(streamCtx context.Context, emit func(Event)) (*session.RunOutput, error) {
		return w.runCore(streamCtx, request, emit)
	}), nil
}

func cloneRunOutput(output *session.RunOutput) *session.RunOutput {
	if output == nil {
		return nil
	}
	raw, err := json.Marshal(output)
	if err != nil {
		clone := *output
		return &clone
	}
	var clone session.RunOutput
	if err := json.Unmarshal(raw, &clone); err != nil {
		shallow := *output
		return &shallow
	}
	return &clone
}

func cloneRunOutputStrict(output *session.RunOutput) (*session.RunOutput, error) {
	raw, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	var clone session.RunOutput
	if err := json.Unmarshal(raw, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func (e *execution) executeNode(ctx context.Context, node NodeSpec, value any, parentID string, emit func(Event)) (NodeResult, error) {
	previous, hasPrevious := e.previous[node.ID]
	if hasPrevious && (previous.Status == workflowschema.NodeStatusCompleted || previous.Status == workflowschema.NodeStatusSkipped) {
		return previous, nil
	}
	now := time.Now().UTC()
	result := NodeResult{NodeID: node.ID, NodeType: node.Type, Status: workflowschema.NodeStatusRunning, Input: value, StartedAt: now}
	if emit != nil {
		emit(newEvent(EventTypeNodeStarted, e.output, node.ID, parentID, nil))
	}
	var err error
	switch node.Type {
	case NodeTypeStep:
		result, err = e.executeStep(ctx, node, value, result)
	case NodeTypeSequence:
		current := value
		for _, child := range node.Children {
			var childResult NodeResult
			childResult, err = e.executeNode(ctx, child, current, node.ID, emit)
			result.Children = append(result.Children, childResult)
			if err != nil {
				break
			}
			current = childResult.Output
		}
		result.Output = current
	case NodeTypeParallel:
		result, err = e.executeParallel(ctx, node, value, result, emit)
	case NodeTypeCondition:
		branch := node.Else
		if hasPrevious && previous.SelectedBranch != "" {
			result.SelectedBranch = previous.SelectedBranch
			if previous.SelectedBranch == "then" {
				branch = node.Then
			}
		} else {
			predicate, _ := e.w.cfg.Registry.predicate(node.PredicateRef)
			input, inputErr := e.nodeInput(node, value)
			if inputErr != nil {
				err = inputErr
				break
			}
			selected, predErr := predicate.Evaluate(ctx, input)
			if predErr == nil && ctx.Err() != nil {
				predErr = ctx.Err()
			}
			if predErr != nil {
				err = predErr
				break
			}
			result.SelectedBranch = "else"
			if selected {
				branch = node.Then
				result.SelectedBranch = "then"
			}
		}
		childResult, childErr := e.executeNode(ctx, *branch, value, node.ID, emit)
		result.Children = append(result.Children, childResult)
		result.Output = childResult.Output
		err = childErr
	}
	completed := time.Now().UTC()
	result.CompletedAt = &completed
	if errors.Is(err, errWorkflowPaused) {
		result.Status = workflowschema.NodeStatusPaused
	} else if err != nil && ctx.Err() != nil {
		result.Status = workflowschema.NodeStatusCancelled
		result.Error = ctx.Err().Error()
	} else if err != nil {
		result.Status = workflowschema.NodeStatusError
		result.Error = err.Error()
	} else if result.Status == workflowschema.NodeStatusRunning {
		result.Status = workflowschema.NodeStatusCompleted
	}
	if emit != nil {
		eventType := EventTypeNodeCompleted
		if errors.Is(err, errWorkflowPaused) {
			eventType = EventTypeNodePaused
		} else if err != nil {
			eventType = EventTypeNodeError
		}
		emit(newEvent(eventType, e.output, node.ID, parentID, err))
	}
	if persistErr := e.progress.record(ctx, result); persistErr != nil {
		if err == nil || errors.Is(err, errWorkflowPaused) {
			err = persistErr
		} else {
			err = errors.Join(err, persistErr)
		}
		result.Status = workflowschema.NodeStatusError
		result.Error = err.Error()
	}
	return result, err
}

func (e *execution) executeStep(ctx context.Context, node NodeSpec, value any, result NodeResult) (NodeResult, error) {
	resumeInput, resumed := e.resume[node.ID]
	if node.HumanReview != nil && !resumed {
		requirement := &workflowschema.Requirement{
			ID: uuid.NewString(), NodeID: node.ID, Kind: node.HumanReview.Kind, Prompt: node.HumanReview.Prompt,
			InputSchema: node.HumanReview.InputSchema, Deadline: node.HumanReview.Deadline,
		}
		e.output.Workflow.Requirement = requirement
		e.output.Workflow.RequirementHistory = append(e.output.Workflow.RequirementHistory, *requirement)
		e.output.Workflow.Checkpoint = &workflowschema.Checkpoint{Version: 1, PausedNodeID: node.ID}
		result.Output = value
		return result, errWorkflowPaused
	}
	executor, _ := e.w.cfg.Registry.executor(node.ExecutorRef)
	attempts := node.Policy.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	backoff := node.Policy.Backoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		result.Attempts = attempt
		input, inputErr := e.nodeInput(node, value)
		if inputErr != nil {
			return result, inputErr
		}
		input.ResumeInput = resumeInput
		// Claim the node as applying before any external I/O: a crash
		// during Execute leaves a durable record that side effects may
		// have happened, so recovery can fail closed instead of silently
		// re-running or losing the outcome.
		claim := result
		claim.Status = workflowschema.NodeStatusApplying
		if persistErr := e.progress.record(ctx, claim); persistErr != nil {
			return result, fmt.Errorf("workflow: claim node %q applying: %w", node.ID, persistErr)
		}
		execCtx := ctx
		cancel := func() {}
		if node.Policy.Timeout > 0 {
			execCtx, cancel = context.WithTimeout(ctx, node.Policy.Timeout)
		}
		execCtx, span := observability.Tracer("xingding.workflow").Start(execCtx, "workflow.node")
		span.SetAttributes(observability.SafeAttrs(
			attribute.String("node_id", node.ID),
			attribute.String("executor_ref", node.ExecutorRef),
			attribute.Int("attempt", attempt),
		)...)
		output, err := executor.Execute(execCtx, input)
		if err == nil && execCtx.Err() != nil {
			err = execCtx.Err()
		}
		outcome := "succeeded"
		if err != nil {
			outcome = "failed"
		}
		span.SetAttributes(observability.SafeAttrs(
			attribute.String(observability.FieldOutcome, outcome),
		)...)
		span.End()
		cancel()
		if err == nil {
			if e.w.cfg.Limits != nil {
				err = validateNodeOutput(output, e.w.cfg.Limits.MaxNodeOutputBytes)
			}
			if err == nil {
				if _, marshalErr := json.Marshal(output); marshalErr != nil {
					err = fmt.Errorf("workflow: node %q output is not JSON serializable: %w", node.ID, marshalErr)
				}
			}
			if err != nil {
				// Fall through to retry/error policy without admitting an
				// unbounded value into state or downstream execution.
			} else {
				if e.w.cfg.Limits == nil {
					result.Output = output.Value
					result.Metadata = output.Metadata
					result.StatePatch = output.StatePatch
					result.ChildRunIDs = output.ChildRunIDs
					for key, patchValue := range output.StatePatch {
						e.state[key] = patchValue
					}
					return result, nil
				}
				candidateState := cloneLimitedState(e.state)
				for key, patchValue := range output.StatePatch {
					candidateState[key] = patchValue
				}
				if e.w.cfg.Limits != nil {
					err = validatePayloadJSON("state", candidateState, e.w.cfg.Limits.MaxStateBytes)
				}
				if err == nil {
					result.Output = output.Value
					result.Metadata = output.Metadata
					result.StatePatch = output.StatePatch
					result.ChildRunIDs = output.ChildRunIDs
					e.state = candidateState
					return result, nil
				}
			}
		}
		lastErr = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || attempt == attempts {
			break
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
	}
	if node.Policy.OnError == ErrorPolicySkip && ctx.Err() == nil {
		result.Status = workflowschema.NodeStatusSkipped
		result.Error = lastErr.Error()
		result.Output = value
		return result, nil
	}
	return result, lastErr
}

func (e *execution) executeParallel(ctx context.Context, node NodeSpec, value any, result NodeResult, emit func(Event)) (NodeResult, error) {
	type childResult struct {
		index  int
		result NodeResult
		state  map[string]any
		err    error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan childResult, len(node.Children))
	for i, child := range node.Children {
		go func(index int, spec NodeSpec) {
			branch := *e
			branch.state = cloneExecutionState(e.state, e.w.cfg.Limits != nil)
			r, err := branch.executeNode(ctx, spec, value, node.ID, emit)
			results <- childResult{index: index, result: r, state: branch.state, err: err}
		}(i, child)
	}
	ordered := make([]NodeResult, len(node.Children))
	var firstErr error
	patches := make(map[string]any)
	for range node.Children {
		child := <-results
		ordered[child.index] = child.result
		if child.err != nil && firstErr == nil {
			firstErr = child.err
			cancel()
		}
		for key, patchValue := range child.state {
			if original, exists := e.state[key]; exists && reflect.DeepEqual(original, patchValue) {
				continue
			}
			if existing, exists := patches[key]; exists {
				if reflect.DeepEqual(existing, patchValue) {
					// Parallel branches writing the same value are idempotent
					// merges, not conflicts.
					continue
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("workflow: parallel state patch conflict on %q", key)
					cancel()
				}
				continue
			}
			patches[key] = patchValue
		}
	}
	result.Children = ordered
	outputs := make([]map[string]any, len(ordered))
	for i := range ordered {
		outputs[i] = map[string]any{"node_id": ordered[i].NodeID, "output": ordered[i].Output}
	}
	result.Output = outputs
	if firstErr == nil {
		if e.w.cfg.Limits == nil {
			for key, patchValue := range patches {
				e.state[key] = patchValue
			}
			result.StatePatch = patches
			return result, nil
		}
		candidateState := cloneLimitedState(e.state)
		for key, patchValue := range patches {
			candidateState[key] = patchValue
		}
		if e.w.cfg.Limits != nil {
			if err := validatePayloadJSON("node output", result.Output, e.w.cfg.Limits.MaxNodeOutputBytes); err != nil {
				return result, err
			}
			if err := validatePayloadJSON("state", candidateState, e.w.cfg.Limits.MaxStateBytes); err != nil {
				return result, err
			}
		}
		e.state = candidateState
		result.StatePatch = patches
	}
	return result, firstErr
}

func (e *execution) nodeInput(node NodeSpec, value any) (NodeInput, error) {
	return e.nodeInputForState(node, value, e.state)
}

func (e *execution) nodeInputForState(node NodeSpec, value any, state map[string]any) (NodeInput, error) {
	metadata, err := session.CloneRunMetadata(e.runContext.metadata)
	if err != nil {
		return NodeInput{}, err
	}
	config := node.Config
	if e.w.cfg.Limits != nil && config != nil {
		config = append(json.RawMessage(nil), config...)
	}
	return NodeInput{
		Value: value, State: cloneExecutionState(state, e.w.cfg.Limits != nil), RunID: e.runContext.runID, WorkflowID: e.runContext.workflowID,
		NodeID: node.ID, SessionID: e.runContext.sessionID, UserID: e.runContext.userID, Config: config, Metadata: metadata,
	}, nil
}
