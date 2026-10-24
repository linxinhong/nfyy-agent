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

	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

// execute selects the legacy recursive executor unless the caller explicitly
// opted into Limits. The limited path has one coordinator and a bounded pool
// of callback workers; composite nodes always run in the coordinator.
func (e *execution) execute(ctx context.Context, node NodeSpec, value any, parentID string, emit func(Event)) (NodeResult, error) {
	if e.w.cfg.Limits == nil {
		return e.executeNode(ctx, node, value, parentID, emit)
	}
	// A single callback, or a flat leaf-only parallel group no wider than K,
	// has no queued internal work. Its established direct path therefore has at
	// most K callback goroutines and no waiting-node goroutines. Every broader,
	// nested, conditional, or retrying tree goes through the shared scheduler.
	if canExecuteLimitedDirect(node, e.w.cfg.Limits.MaxConcurrentCallbacks) {
		return e.executeNode(ctx, node, value, parentID, emit)
	}
	return newLimitedScheduler(e, ctx, emit).run(node, value, parentID)
}

func canExecuteLimitedDirect(node NodeSpec, limit int) bool {
	if node.Type == NodeTypeStep {
		return node.Policy.MaxAttempts <= 1
	}
	if node.Type != NodeTypeParallel || len(node.Children) > limit {
		return false
	}
	for _, child := range node.Children {
		if child.Type != NodeTypeStep || child.Policy.MaxAttempts > 1 || child.HumanReview != nil {
			return false
		}
	}
	return true
}

type limitedScheduler struct {
	execution *execution
	ctx       context.Context
	emit      func(Event)

	jobs    []callbackJob
	retries []scheduledRetry

	jobsCh chan callbackJob
	doneCh chan callbackCompletion

	rootResult NodeResult
	rootErr    error
	rootDone   bool
}

type scheduledNode struct {
	node     NodeSpec
	value    any
	parentID string
	ctx      context.Context
	state    map[string]any
	result   NodeResult
	started  bool
	finished bool
	done     func(NodeResult, error)

	attempt     int
	backoff     time.Duration
	maxAttempts int
	resumeInput any
	parallel    *parallelNodeState
}

type parallelNodeState struct {
	cancel    context.CancelFunc
	base      map[string]any
	ordered   []NodeResult
	patches   map[string]any
	remaining int
	firstErr  error
}

type callbackJob struct {
	ctx context.Context
	run func() callbackCompletion
}

type callbackCompletion struct {
	deliver func()
}

type scheduledRetry struct {
	due  time.Time
	node *scheduledNode
}

// limitedSchedulerWorkerStarted is test-only observability for the bounded
// pool. Production code leaves it as a no-op; tests replace it before a run
// and restore it after the run, never concurrently with that replacement.
var limitedSchedulerWorkerStarted = func() {}

func newLimitedScheduler(execution *execution, ctx context.Context, emit func(Event)) *limitedScheduler {
	return &limitedScheduler{execution: execution, ctx: ctx, emit: emit}
}

func (s *limitedScheduler) run(root NodeSpec, value any, parentID string) (NodeResult, error) {
	workers := s.workerCount(root)
	s.jobsCh = make(chan callbackJob)
	s.doneCh = make(chan callbackCompletion, workers)
	var workerGroup sync.WaitGroup
	workerGroup.Add(workers)
	for range workers {
		go func() {
			defer workerGroup.Done()
			limitedSchedulerWorkerStarted()
			for job := range s.jobsCh {
				s.doneCh <- job.run()
			}
		}()
	}

	rootNode := &scheduledNode{node: root, value: value, parentID: parentID, ctx: s.ctx, state: s.execution.state}
	rootNode.done = func(result NodeResult, err error) {
		s.rootResult, s.rootErr, s.rootDone = result, err, true
	}
	s.startNode(rootNode)

	var retryTimer *time.Timer
	var retryTimerC <-chan time.Time
	ctxDone := s.ctx.Done()
	for !s.rootDone {
		s.activateDueRetries(time.Now())
		retryTimer, retryTimerC = s.resetRetryTimer(retryTimer)
		if len(s.jobs) == 0 {
			select {
			case completion := <-s.doneCh:
				completion.deliver()
			case <-retryTimerC:
			case <-ctxDone:
				ctxDone = nil
				s.activateCancelledRetries()
			}
			continue
		}

		job := s.jobs[0]
		select {
		case s.jobsCh <- job:
			s.jobs = s.jobs[1:]
		case completion := <-s.doneCh:
			completion.deliver()
		case <-retryTimerC:
		case <-ctxDone:
			ctxDone = nil
			s.activateCancelledRetries()
		}
	}
	if retryTimer != nil {
		if !retryTimer.Stop() {
			select {
			case <-retryTimer.C:
			default:
			}
		}
	}
	close(s.jobsCh)
	workerGroup.Wait()
	return s.rootResult, s.rootErr
}

func (s *limitedScheduler) workerCount(root NodeSpec) int {
	count := callbackNodeCount(root)
	if count < 1 {
		count = 1
	}
	workers := s.execution.w.cfg.Limits.MaxConcurrentCallbacks
	if workers > count {
		workers = count
	}
	return workers
}

func callbackNodeCount(root NodeSpec) int {
	count := 0
	stack := []NodeSpec{root}
	for len(stack) > 0 {
		last := len(stack) - 1
		node := stack[last]
		stack = stack[:last]
		switch node.Type {
		case NodeTypeStep, NodeTypeCondition:
			count++
		}
		for index := range node.Children {
			stack = append(stack, node.Children[index])
		}
		if node.Then != nil {
			stack = append(stack, *node.Then)
		}
		if node.Else != nil {
			stack = append(stack, *node.Else)
		}
	}
	return count
}

func (s *limitedScheduler) startNode(n *scheduledNode) {
	if previous, ok := s.execution.previous[n.node.ID]; ok && (previous.Status == workflowschema.NodeStatusCompleted || previous.Status == workflowschema.NodeStatusSkipped) {
		n.finished = true
		n.done(previous, nil)
		return
	}
	n.started = true
	now := time.Now().UTC()
	n.result = NodeResult{NodeID: n.node.ID, NodeType: n.node.Type, Status: workflowschema.NodeStatusRunning, Input: n.value, StartedAt: now}
	if s.emit != nil {
		s.emit(newEvent(EventTypeNodeStarted, s.execution.output, n.node.ID, n.parentID, nil))
	}
	if err := n.ctx.Err(); err != nil {
		s.finishNode(n, err)
		return
	}

	switch n.node.Type {
	case NodeTypeStep:
		s.startStep(n)
	case NodeTypeSequence:
		s.startSequenceChild(n, 0, n.value)
	case NodeTypeParallel:
		s.startParallel(n)
	case NodeTypeCondition:
		s.startCondition(n)
	default:
		s.finishNode(n, fmt.Errorf("workflow: unsupported node type %q", n.node.Type))
	}
}

func (s *limitedScheduler) startStep(n *scheduledNode) {
	if n.node.HumanReview != nil {
		if _, resumed := s.execution.resume[n.node.ID]; !resumed {
			requirement := &workflowschema.Requirement{
				ID: uuid.NewString(), NodeID: n.node.ID, Kind: n.node.HumanReview.Kind, Prompt: n.node.HumanReview.Prompt,
				InputSchema: n.node.HumanReview.InputSchema, Deadline: n.node.HumanReview.Deadline,
			}
			s.execution.output.Workflow.Requirement = requirement
			s.execution.output.Workflow.RequirementHistory = append(s.execution.output.Workflow.RequirementHistory, *requirement)
			s.execution.output.Workflow.Checkpoint = &workflowschema.Checkpoint{Version: 1, PausedNodeID: n.node.ID}
			n.result.Output = n.value
			s.finishNode(n, errWorkflowPaused)
			return
		}
	}
	n.resumeInput = s.execution.resume[n.node.ID]
	n.maxAttempts = n.node.Policy.MaxAttempts
	if n.maxAttempts <= 0 {
		n.maxAttempts = 1
	}
	n.backoff = n.node.Policy.Backoff
	if n.backoff <= 0 {
		n.backoff = 100 * time.Millisecond
	}
	s.enqueueStepAttempt(n)
}

func (s *limitedScheduler) enqueueStepAttempt(n *scheduledNode) {
	n.attempt++
	n.result.Attempts = n.attempt
	executor, _ := s.execution.w.cfg.Registry.executor(n.node.ExecutorRef)
	s.jobs = append(s.jobs, callbackJob{ctx: n.ctx, run: func() callbackCompletion {
		if err := n.ctx.Err(); err != nil {
			return callbackCompletion{deliver: func() { s.stepAttemptComplete(n, NodeOutput{}, err) }}
		}
		input, err := s.execution.nodeInputForState(n.node, n.value, n.state)
		if err == nil {
			input.ResumeInput = n.resumeInput
			claim := n.result
			claim.Status = workflowschema.NodeStatusApplying
			if persistErr := s.execution.progress.record(n.ctx, claim); persistErr != nil {
				err = fmt.Errorf("workflow: claim node %q applying: %w", n.node.ID, persistErr)
			}
		}
		if err != nil {
			return callbackCompletion{deliver: func() { s.stepAttemptComplete(n, NodeOutput{}, err) }}
		}

		execCtx := n.ctx
		cancel := func() {}
		if n.node.Policy.Timeout > 0 {
			execCtx, cancel = context.WithTimeout(n.ctx, n.node.Policy.Timeout)
		}
		execCtx, span := observability.Tracer("xingding.workflow").Start(execCtx, "workflow.node")
		span.SetAttributes(observability.SafeAttrs(
			attribute.String("node_id", n.node.ID),
			attribute.String("executor_ref", n.node.ExecutorRef),
			attribute.Int("attempt", n.attempt),
		)...)
		output, execErr := executor.Execute(execCtx, input)
		if execErr == nil && execCtx.Err() != nil {
			execErr = execCtx.Err()
		}
		outcome := "succeeded"
		if execErr != nil {
			outcome = "failed"
		}
		span.SetAttributes(observability.SafeAttrs(
			attribute.String(observability.FieldOutcome, outcome),
		)...)
		span.End()
		cancel()
		if execErr == nil {
			execErr = validateNodeOutput(output, s.execution.w.cfg.Limits.MaxNodeOutputBytes)
			if execErr == nil {
				_, marshalErr := json.Marshal(output)
				if marshalErr != nil {
					execErr = fmt.Errorf("workflow: node %q output is not JSON serializable: %w", n.node.ID, marshalErr)
				}
			}
		}
		return callbackCompletion{deliver: func() { s.stepAttemptComplete(n, output, execErr) }}
	}})
}

func (s *limitedScheduler) stepAttemptComplete(n *scheduledNode, output NodeOutput, err error) {
	if n.finished {
		return
	}
	if err == nil {
		candidateState := cloneLimitedState(n.state)
		for key, patchValue := range output.StatePatch {
			candidateState[key] = patchValue
		}
		if stateErr := validatePayloadJSON("state", candidateState, s.execution.w.cfg.Limits.MaxStateBytes); stateErr != nil {
			err = stateErr
		} else {
			n.result.Output = output.Value
			n.result.Metadata = output.Metadata
			n.result.StatePatch = output.StatePatch
			n.result.ChildRunIDs = output.ChildRunIDs
			for key, patchValue := range output.StatePatch {
				n.state[key] = patchValue
			}
			s.finishNode(n, nil)
			return
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || n.attempt == n.maxAttempts {
		if n.node.Policy.OnError == ErrorPolicySkip && n.ctx.Err() == nil {
			n.result.Status = workflowschema.NodeStatusSkipped
			n.result.Error = err.Error()
			n.result.Output = n.value
			s.finishNode(n, nil)
			return
		}
		s.finishNode(n, err)
		return
	}
	s.retries = append(s.retries, scheduledRetry{due: time.Now().Add(n.backoff), node: n})
	n.backoff *= 2
}

func (s *limitedScheduler) startSequenceChild(n *scheduledNode, index int, value any) {
	if n.finished {
		return
	}
	if index == len(n.node.Children) {
		n.result.Output = value
		s.finishNode(n, nil)
		return
	}
	child := &scheduledNode{node: n.node.Children[index], value: value, parentID: n.node.ID, ctx: n.ctx, state: n.state}
	child.done = func(result NodeResult, err error) {
		n.result.Children = append(n.result.Children, result)
		if err != nil {
			s.finishNode(n, err)
			return
		}
		s.startSequenceChild(n, index+1, result.Output)
	}
	s.startNode(child)
}

func (s *limitedScheduler) startParallel(n *scheduledNode) {
	ctx, cancel := context.WithCancel(n.ctx)
	state := &parallelNodeState{
		cancel: cancel, base: n.state, ordered: make([]NodeResult, len(n.node.Children)),
		patches: make(map[string]any), remaining: len(n.node.Children),
	}
	n.parallel = state
	for index, childSpec := range n.node.Children {
		index, childSpec := index, childSpec
		branchState := cloneLimitedState(n.state)
		child := &scheduledNode{node: childSpec, value: n.value, parentID: n.node.ID, ctx: ctx, state: branchState}
		child.done = func(result NodeResult, err error) {
			s.parallelChildComplete(n, index, branchState, result, err)
		}
		s.startNode(child)
	}
}

func (s *limitedScheduler) parallelChildComplete(n *scheduledNode, index int, state map[string]any, result NodeResult, err error) {
	if n.finished {
		return
	}
	p := n.parallel
	p.ordered[index] = result
	p.remaining--
	if err != nil && p.firstErr == nil {
		p.firstErr = err
		p.cancel()
		s.activateCancelledRetries()
	}
	for key, patchValue := range state {
		if original, exists := p.base[key]; exists && reflect.DeepEqual(original, patchValue) {
			continue
		}
		if existing, exists := p.patches[key]; exists {
			if reflect.DeepEqual(existing, patchValue) {
				continue
			}
			if p.firstErr == nil {
				p.firstErr = fmt.Errorf("workflow: parallel state patch conflict on %q", key)
				p.cancel()
				s.activateCancelledRetries()
			}
			continue
		}
		p.patches[key] = patchValue
	}
	if p.remaining != 0 {
		return
	}
	p.cancel()
	n.result.Children = p.ordered
	outputs := make([]map[string]any, len(p.ordered))
	for index := range p.ordered {
		outputs[index] = map[string]any{"node_id": p.ordered[index].NodeID, "output": p.ordered[index].Output}
	}
	n.result.Output = outputs
	if p.firstErr == nil {
		candidateState := cloneLimitedState(n.state)
		for key, patchValue := range p.patches {
			candidateState[key] = patchValue
		}
		if err := validatePayloadJSON("node output", n.result.Output, s.execution.w.cfg.Limits.MaxNodeOutputBytes); err != nil {
			p.firstErr = err
		} else if err := validatePayloadJSON("state", candidateState, s.execution.w.cfg.Limits.MaxStateBytes); err != nil {
			p.firstErr = err
		} else {
			for key, patchValue := range p.patches {
				n.state[key] = patchValue
			}
			n.result.StatePatch = p.patches
		}
	}
	s.finishNode(n, p.firstErr)
}

func (s *limitedScheduler) startCondition(n *scheduledNode) {
	if previous, ok := s.execution.previous[n.node.ID]; ok && previous.SelectedBranch != "" {
		n.result.SelectedBranch = previous.SelectedBranch
		branch := n.node.Else
		if previous.SelectedBranch == "then" {
			branch = n.node.Then
		}
		s.startConditionBranch(n, *branch)
		return
	}
	predicate, _ := s.execution.w.cfg.Registry.predicate(n.node.PredicateRef)
	s.jobs = append(s.jobs, callbackJob{ctx: n.ctx, run: func() callbackCompletion {
		if err := n.ctx.Err(); err != nil {
			return callbackCompletion{deliver: func() { s.conditionComplete(n, false, err) }}
		}
		input, err := s.execution.nodeInputForState(n.node, n.value, n.state)
		if err == nil {
			var selected bool
			selected, err = predicate.Evaluate(n.ctx, input)
			if err == nil && n.ctx.Err() != nil {
				err = n.ctx.Err()
			}
			return callbackCompletion{deliver: func() { s.conditionComplete(n, selected, err) }}
		}
		return callbackCompletion{deliver: func() { s.conditionComplete(n, false, err) }}
	}})
}

func (s *limitedScheduler) conditionComplete(n *scheduledNode, selected bool, err error) {
	if err != nil {
		s.finishNode(n, err)
		return
	}
	n.result.SelectedBranch = "else"
	branch := n.node.Else
	if selected {
		n.result.SelectedBranch = "then"
		branch = n.node.Then
	}
	s.startConditionBranch(n, *branch)
}

func (s *limitedScheduler) startConditionBranch(n *scheduledNode, branch NodeSpec) {
	child := &scheduledNode{node: branch, value: n.value, parentID: n.node.ID, ctx: n.ctx, state: n.state}
	child.done = func(result NodeResult, err error) {
		n.result.Children = append(n.result.Children, result)
		n.result.Output = result.Output
		s.finishNode(n, err)
	}
	s.startNode(child)
}

func (s *limitedScheduler) finishNode(n *scheduledNode, err error) {
	if n.finished {
		return
	}
	n.finished = true
	completed := time.Now().UTC()
	n.result.CompletedAt = &completed
	if errors.Is(err, errWorkflowPaused) {
		n.result.Status = workflowschema.NodeStatusPaused
	} else if err != nil && n.ctx.Err() != nil {
		n.result.Status = workflowschema.NodeStatusCancelled
		n.result.Error = n.ctx.Err().Error()
	} else if err != nil {
		n.result.Status = workflowschema.NodeStatusError
		n.result.Error = err.Error()
	} else if n.result.Status == workflowschema.NodeStatusRunning {
		n.result.Status = workflowschema.NodeStatusCompleted
	}
	if s.emit != nil {
		eventType := EventTypeNodeCompleted
		if errors.Is(err, errWorkflowPaused) {
			eventType = EventTypeNodePaused
		} else if err != nil {
			eventType = EventTypeNodeError
		}
		s.emit(newEvent(eventType, s.execution.output, n.node.ID, n.parentID, err))
	}
	if persistErr := s.execution.progress.record(n.ctx, n.result); persistErr != nil {
		if err == nil || errors.Is(err, errWorkflowPaused) {
			err = persistErr
		} else {
			err = errors.Join(err, persistErr)
		}
		n.result.Status = workflowschema.NodeStatusError
		n.result.Error = err.Error()
	}
	n.done(n.result, err)
}

func (s *limitedScheduler) activateDueRetries(now time.Time) {
	for index := 0; index < len(s.retries); {
		retry := s.retries[index]
		if now.Before(retry.due) && retry.node.ctx.Err() == nil {
			index++
			continue
		}
		s.retries = append(s.retries[:index], s.retries[index+1:]...)
		s.enqueueStepAttempt(retry.node)
	}
}

func (s *limitedScheduler) activateCancelledRetries() {
	s.activateDueRetries(time.Now())
}

func (s *limitedScheduler) resetRetryTimer(timer *time.Timer) (*time.Timer, <-chan time.Time) {
	if len(s.retries) == 0 {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return timer, nil
	}
	next := s.retries[0].due
	for _, retry := range s.retries[1:] {
		if retry.due.Before(next) {
			next = retry.due
		}
	}
	delay := time.Until(next)
	if delay < 0 {
		delay = 0
	}
	if timer == nil {
		timer = time.NewTimer(delay)
	} else {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}
	return timer, timer.C
}
