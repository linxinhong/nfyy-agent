package workflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestWorkflowExecutesStructuredDefinition(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterExecutor("prefix", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "prefix:" + input.Value.(string)}, nil
	}))
	registry.RegisterExecutor("upper", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "UPPER:" + input.Value.(string)}, nil
	}))
	registry.RegisterExecutor("lower", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "lower:" + input.Value.(string)}, nil
	}))
	registry.RegisterPredicate("is-go", PredicateFunc(func(_ context.Context, input NodeInput) (bool, error) {
		return input.Value == "prefix:go", nil
	}))

	wf, err := New(Config{
		Definition: Definition{
			ID:      "structured",
			Name:    "Structured",
			Version: "v1",
			Root: Sequence("root",
				ExecutorNode("prefix", "prefix"),
				Condition("language", "is-go",
					ExecutorNode("go-branch", "upper"),
					ExecutorNode("other-branch", "lower"),
				),
			),
		},
		Registry: registry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out, err := wf.Run(context.Background(), RunRequest{Input: "go"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Content != "UPPER:prefix:go" {
		t.Fatalf("content = %q", out.Content)
	}
	if out.Workflow == nil || out.Workflow.DefinitionVersion != "v1" {
		t.Fatalf("workflow details = %+v", out.Workflow)
	}
	if got := resultIDs(out.Workflow.RootResult); !reflect.DeepEqual(got, []string{"root", "prefix", "language", "go-branch"}) {
		t.Fatalf("result ids = %v", got)
	}
	if out.Workflow.RootResult.Children[1].SelectedBranch != "then" {
		t.Fatalf("selected branch = %q", out.Workflow.RootResult.Children[1].SelectedBranch)
	}
}

func TestWorkflowPropagatesRunMetadataToNodesAndOutput(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterExecutor("metadata", ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
		fromContext, _ := session.RunMetadataFromContext(ctx)
		if input.Metadata["project_id"] != "project-a" || fromContext["actor_id"] != "actor-a" {
			return NodeOutput{}, errors.New("metadata was not propagated")
		}
		return NodeOutput{Value: "ok"}, nil
	}))
	wf, err := New(Config{Definition: Definition{ID: "metadata", Version: "v1", Root: ExecutorNode("read", "metadata")}, Registry: registry})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, runErr := wf.Run(context.Background(), RunRequest{Input: "go", Metadata: map[string]string{"project_id": "project-a", "actor_id": "actor-a"}})
	if runErr != nil || out.Metadata["project_id"] != "project-a" {
		t.Fatalf("Run = %+v, %v", out, runErr)
	}
}

func TestParallelWorkflowNodesIsolateRunMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry := NewRegistry()
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	mutated := make(chan struct{}, 2)
	bothMutated := make(chan struct{})
	registerMutator := func(name string) {
		registry.RegisterExecutor(name, ExecutorFunc(func(ctx context.Context, input NodeInput) (NodeOutput, error) {
			ready <- struct{}{}
			select {
			case <-start:
			case <-ctx.Done():
				return NodeOutput{}, ctx.Err()
			}
			for range 10000 {
				input.Metadata["branch"] = name
			}
			mutated <- struct{}{}
			select {
			case <-bothMutated:
			case <-ctx.Done():
				return NodeOutput{}, ctx.Err()
			}
			if input.Metadata["branch"] != name {
				return NodeOutput{}, fmt.Errorf("%s node metadata was polluted: %q", name, input.Metadata["branch"])
			}
			return NodeOutput{Value: name}, nil
		}))
	}
	registerMutator("first")
	registerMutator("second")

	wf, err := New(Config{
		Definition: Definition{
			ID:      "parallel-metadata",
			Version: "v1",
			Root: Parallel("parallel",
				ExecutorNode("first-node", "first"),
				ExecutorNode("second-node", "second"),
			),
		},
		Registry: registry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type runResult struct {
		output *session.RunOutput
		err    error
	}
	result := make(chan runResult, 1)
	go func() {
		out, runErr := wf.Run(ctx, RunRequest{
			Input:    "go",
			Metadata: map[string]string{"project_id": "project-a", "branch": "root"},
		})
		result <- runResult{output: out, err: runErr}
	}()
	for range 2 {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("parallel nodes did not become ready before deadline")
		}
	}
	close(start)
	for range 2 {
		select {
		case <-mutated:
		case <-ctx.Done():
			t.Fatal("parallel nodes did not finish metadata mutation before deadline")
		}
	}
	close(bothMutated)
	var run runResult
	select {
	case run = <-result:
	case <-ctx.Done():
		t.Fatal("workflow did not finish before deadline")
	}
	out, runErr := run.output, run.err
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if out.Metadata["branch"] != "root" || out.Metadata["project_id"] != "project-a" {
		t.Fatalf("root metadata was mutated: %+v", out.Metadata)
	}
}

func TestResumeRejectsOversizedStoredRunMetadata(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	executed := false
	registry.RegisterExecutor("write", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		executed = true
		return NodeOutput{Value: "written"}, nil
	}))
	node := ExecutorNode("write", "write")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	wf, err := New(Config{Definition: Definition{ID: "metadata-validation", Version: "v1", Root: node}, Registry: registry, DB: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := wf.Run(context.Background(), RunRequest{Input: "value"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	broken := cloneRunOutput(paused)
	broken.Metadata = map[string]string{"oversized": strings.Repeat("x", session.MaxRunMetadataBytes)}
	if err := store.UpdateRun(context.Background(), broken, session.RunStatusPaused); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}

	_, err = wf.Resume(context.Background(), ResumeRequest{
		RunID:         paused.RunID,
		RequirementID: paused.Workflow.Requirement.ID,
		Action:        ResumeActionApprove,
	})
	if !errors.Is(err, session.ErrRunMetadataTooLarge) {
		t.Fatalf("Resume error = %v, want ErrRunMetadataTooLarge", err)
	}
	if executed {
		t.Fatal("executor ran with invalid stored metadata")
	}
}

func TestDefinitionRejectsDuplicateNodeIDs(t *testing.T) {
	registry := NewRegistry()
	registry.RegisterExecutor("noop", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}))
	_, err := New(Config{
		Definition: Definition{ID: "duplicate", Version: "v1", Root: Sequence("root", ExecutorNode("same", "noop"), ExecutorNode("same", "noop"))},
		Registry:   registry,
	})
	if err == nil {
		t.Fatal("New accepted duplicate node ids")
	}
}

func TestWorkflowPausesAndResumesSameRun(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	registry.RegisterExecutor("write", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "written:" + input.Value.(string)}, nil
	}))
	reviewed := ExecutorNode("write", "write")
	reviewed.HumanReview = &HumanReview{Kind: RequirementConfirmation, Prompt: "Approve write?"}
	config := Config{
		Definition: Definition{ID: "approval", Version: "v1", Root: Sequence("root", reviewed)},
		Registry:   registry,
		DB:         store,
	}
	wf, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	paused, err := wf.Run(context.Background(), RunRequest{Input: "record", RunID: "approval-run", UserID: "alice"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if paused.Status != session.RunStatusPaused || paused.Workflow.Requirement == nil {
		t.Fatalf("paused output = %+v", paused)
	}

	restarted, err := New(config)
	if err != nil {
		t.Fatalf("New restarted: %v", err)
	}
	completed, err := restarted.Resume(context.Background(), ResumeRequest{
		RunID:         paused.RunID,
		RequirementID: paused.Workflow.Requirement.ID,
		Action:        "approve",
		ActorID:       "reviewer-1",
		UserID:        "alice",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if completed.RunID != paused.RunID || completed.Status != session.RunStatusCompleted || completed.Content != "written:record" {
		t.Fatalf("completed output = %+v", completed)
	}
	if len(completed.Workflow.RequirementHistory) != 1 || completed.Workflow.RequirementHistory[0].Resolution == nil || completed.Workflow.RequirementHistory[0].Resolution.ActorID != "reviewer-1" {
		t.Fatalf("requirement history = %+v", completed.Workflow.RequirementHistory)
	}
	persisted, err := store.ReadWorkflowSession(context.Background(), paused.SessionID)
	if err != nil {
		t.Fatalf("ReadWorkflowSession: %v", err)
	}
	if len(persisted.Runs) != 1 || persisted.Runs[0].RunID != paused.RunID {
		t.Fatalf("persisted runs = %+v", persisted.Runs)
	}
}

func TestConcurrentResumeHasOneWinner(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	registry.RegisterExecutor("write", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
		started <- struct{}{}
		<-release
		return NodeOutput{Value: input.Value}, nil
	}))
	node := ExecutorNode("write", "write")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	wf, err := New(Config{Definition: Definition{ID: "resume-race", Version: "v1", Root: node}, Registry: registry, DB: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := wf.Run(context.Background(), RunRequest{Input: "value", UserID: "alice"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	request := ResumeRequest{RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: "approve", UserID: "alice"}
	results := make(chan error, 2)
	go func() { _, err := wf.Resume(context.Background(), request); results <- err }()
	<-started
	go func() { _, err := wf.Resume(context.Background(), request); results <- err }()
	second := <-results
	if !errors.Is(second, db.ErrConflict) {
		t.Fatalf("second resume error = %v, want conflict", second)
	}
	close(release)
	if err := <-results; err != nil {
		t.Fatalf("winning resume: %v", err)
	}
}

func TestUserInputResumeAndDeadline(t *testing.T) {
	t.Run("submitted input reaches executor", func(t *testing.T) {
		store := in_memory.New()
		registry := NewRegistry()
		registry.RegisterExecutor("collect", ExecutorFunc(func(_ context.Context, input NodeInput) (NodeOutput, error) {
			return NodeOutput{Value: input.ResumeInput}, nil
		}))
		node := ExecutorNode("collect", "collect")
		node.HumanReview = &HumanReview{Kind: RequirementUserInput, Prompt: "provide value"}
		wf, err := New(Config{Definition: Definition{ID: "input", Version: "v1", Root: node}, Registry: registry, DB: store})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		paused, err := wf.Run(context.Background(), RunRequest{Input: "original"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		completed, err := wf.Resume(context.Background(), ResumeRequest{RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: "submit", Input: map[string]any{"answer": "yes"}})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if !reflect.DeepEqual(completed.Workflow.Output, map[string]any{"answer": "yes"}) {
			t.Fatalf("output = %#v", completed.Workflow.Output)
		}
	})

	t.Run("expired requirement cancels run", func(t *testing.T) {
		store := in_memory.New()
		registry := NewRegistry()
		registry.RegisterExecutor("noop", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) { return NodeOutput{}, nil }))
		deadline := time.Now().UTC().Add(-time.Minute)
		node := ExecutorNode("noop", "noop")
		node.HumanReview = &HumanReview{Kind: RequirementConfirmation, Deadline: &deadline}
		wf, err := New(Config{Definition: Definition{ID: "expiry", Version: "v1", Root: node}, Registry: registry, DB: store})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		paused, err := wf.Run(context.Background(), RunRequest{Input: "value"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		cancelled, err := wf.Resume(context.Background(), ResumeRequest{RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: "approve"})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if cancelled.Status != session.RunStatusCancelled {
			t.Fatalf("status = %q", cancelled.Status)
		}
	})
}

func TestParallelRejectsOverlappingStatePatches(t *testing.T) {
	registry := NewRegistry()
	for _, ref := range []string{"left", "right"} {
		value := ref
		registry.RegisterExecutor(ref, ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
			return NodeOutput{Value: value, StatePatch: map[string]any{"shared": value}}, nil
		}))
	}
	wf, err := New(Config{
		Definition: Definition{ID: "parallel", Version: "v1", Root: Parallel("root", ExecutorNode("left", "left"), ExecutorNode("right", "right"))},
		Registry:   registry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := wf.Run(context.Background(), RunRequest{Input: "value"})
	if err == nil || out.Status != session.RunStatusError {
		t.Fatalf("output = %+v, err = %v", out, err)
	}
}

func TestParallelMergesIdenticalStatePatches(t *testing.T) {
	registry := NewRegistry()
	for _, ref := range []string{"left", "right"} {
		value := ref
		registry.RegisterExecutor(ref, ExecutorFunc(func(_ context.Context, _ NodeInput) (NodeOutput, error) {
			// Both branches write the same key with the same value (an
			// idempotent merge, not a conflict) plus a branch-private key.
			return NodeOutput{Value: value, StatePatch: map[string]any{"shared": "same", "own_" + value: value}}, nil
		}))
	}
	wf, err := New(Config{
		Definition: Definition{ID: "parallel", Version: "v1", Root: Parallel("root", ExecutorNode("left", "left"), ExecutorNode("right", "right"))},
		Registry:   registry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := wf.Run(context.Background(), RunRequest{Input: "value"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	state := out.Workflow.State
	if state["shared"] != "same" {
		t.Fatalf("state[shared] = %v, want same", state["shared"])
	}
	if state["own_left"] != "left" || state["own_right"] != "right" {
		t.Fatalf("branch-private keys not merged: %v", state)
	}
}

func TestAgentExecutorPersistsParentRelationship(t *testing.T) {
	store := in_memory.New()
	model := mock.New()
	model.ResponseData = &models.ModelResponse{Content: "agent result"}
	agentValue, err := agent.New(agent.AgentConfig{ID: "child-agent", Model: model, DB: store})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	registry := NewRegistry()
	registry.RegisterExecutor("agent", AgentExecutor(agentValue))
	wf, err := New(Config{
		Definition: Definition{ID: "parent", Version: "v1", Root: ExecutorNode("agent-node", "agent")},
		Registry:   registry,
		DB:         store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := wf.Run(context.Background(), RunRequest{Input: "hello", UserID: "alice"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	childID := out.Workflow.RootResult.ChildRunIDs[0]
	child, err := store.GetRun(context.Background(), db.RunQuery{RunID: childID, UserID: "alice"})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if child.ParentRunID != out.RunID || child.NodeID != "agent-node" {
		t.Fatalf("child relationship = %+v", child)
	}
}

func TestNodeCompletionIsDurableBeforeWorkflowFinishes(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	registry.RegisterExecutor("first", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: "first"}, nil
	}))
	started := make(chan struct{})
	release := make(chan struct{})
	registry.RegisterExecutor("second", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		close(started)
		<-release
		return NodeOutput{Value: "second"}, nil
	}))
	wf, err := New(Config{
		Definition: Definition{ID: "progress", Version: "v1", Root: Sequence("root", ExecutorNode("first", "first"), ExecutorNode("second", "second"))},
		Registry:   registry, DB: store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := wf.Run(context.Background(), RunRequest{RunID: "progress-run", Input: "start"})
		finished <- err
	}()
	<-started
	stored, err := store.GetRun(context.Background(), db.RunQuery{RunID: "progress-run"})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	first := stored.Workflow.NodeResults["first"]
	if first.Status != "completed" || first.Output != "first" {
		t.Fatalf("durable first result = %+v", first)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestInvalidStoredDefinitionDoesNotClaimPausedRun(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	registry.RegisterExecutor("write", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) { return NodeOutput{}, nil }))
	node := ExecutorNode("write", "write")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	wf, err := New(Config{Definition: Definition{ID: "validation", Version: "v1", Root: node}, Registry: registry, DB: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := wf.Run(context.Background(), RunRequest{Input: "value"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	broken := cloneRunOutput(paused)
	broken.Workflow.Definition.Root.ExecutorRef = "missing"
	if err := store.UpdateRun(context.Background(), broken, session.RunStatusPaused); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}
	_, err = wf.Resume(context.Background(), ResumeRequest{RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: ResumeActionApprove})
	if err == nil {
		t.Fatal("Resume accepted missing executor")
	}
	stored, _ := store.GetRun(context.Background(), db.RunQuery{RunID: paused.RunID})
	if stored.Status != session.RunStatusPaused {
		t.Fatalf("status = %q, want paused", stored.Status)
	}
}

func TestResumeRejectsRunOwnedByAnotherWorkflow(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	registry.RegisterExecutor("shared", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) { return NodeOutput{}, nil }))
	node := ExecutorNode("step", "shared")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	owner, err := New(Config{Definition: Definition{ID: "owner", Version: "v1", Root: node}, Registry: registry, DB: store})
	if err != nil {
		t.Fatalf("owner New: %v", err)
	}
	otherNode := ExecutorNode("step", "shared")
	otherNode.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	other, err := New(Config{Definition: Definition{ID: "other", Version: "v1", Root: otherNode}, Registry: registry, DB: store})
	if err != nil {
		t.Fatalf("other New: %v", err)
	}
	paused, err := owner.Run(context.Background(), RunRequest{Input: "value"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, err = other.Resume(context.Background(), ResumeRequest{RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: ResumeActionApprove})
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("Resume error = %v, want not found", err)
	}
}

func TestHumanReviewEventsIncludeRequirementAndCancellation(t *testing.T) {
	store := in_memory.New()
	registry := NewRegistry()
	registry.RegisterExecutor("write", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) { return NodeOutput{}, nil }))
	node := ExecutorNode("write", "write")
	node.HumanReview = &HumanReview{Kind: RequirementConfirmation}
	wf, err := New(Config{Definition: Definition{ID: "review-events", Version: "v1", Root: Sequence("root", node)}, Registry: registry, DB: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, err := wf.RunStream(context.Background(), RunRequest{Input: "value"})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var paused *session.RunOutput
	var runTypes []EventType
	pausedCount := 0
	requirementIndex := -1
	pausedIndex := -1
	for event := range events {
		runTypes = append(runTypes, event.Type)
		if event.Type == EventTypeRequirementCreated {
			requirementIndex = len(runTypes) - 1
		}
		if event.Type == EventTypeRunPaused {
			pausedCount++
			pausedIndex = len(runTypes) - 1
			paused = event.RunOutput
			if event.RunOutput == nil || event.RunOutput.Status != session.RunStatusPaused {
				t.Fatalf("run_paused output = %+v", event.RunOutput)
			}
		}
	}
	if paused == nil || pausedCount != 1 || requirementIndex < 0 || requirementIndex >= pausedIndex {
		t.Fatalf("run events = %v", runTypes)
	}
	resumeEvents, err := wf.ResumeEvents(context.Background(), ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: ResumeActionReject,
	})
	if err != nil {
		t.Fatalf("ResumeEvents: %v", err)
	}
	var resumeTypes []EventType
	for event := range resumeEvents {
		resumeTypes = append(resumeTypes, event.Type)
	}
	if !containsEvent(resumeTypes, EventTypeRequirementResolved) || !containsEvent(resumeTypes, EventTypeRunCancelled) {
		t.Fatalf("resume events = %v", resumeTypes)
	}
}

func containsEvent(events []EventType, expected EventType) bool {
	for _, event := range events {
		if event == expected {
			return true
		}
	}
	return false
}

func resultIDs(root *NodeResult) []string {
	if root == nil {
		return nil
	}
	ids := []string{root.NodeID}
	for i := range root.Children {
		ids = append(ids, resultIDs(&root.Children[i])...)
	}
	return ids
}
