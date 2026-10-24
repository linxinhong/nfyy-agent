package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/postgres"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/sqlite"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

type managedWorkflowStores struct {
	first, second db.WorkflowStore
	firstManaged  runmanager.ManagedStore
	secondManaged runmanager.ManagedStore
	close         func()
}

func sqliteManagedWorkflowStores(t *testing.T, clock runmanager.Clock) managedWorkflowStores {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workflow-managed.db")
	first, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("open first sqlite store: %v", err)
	}
	second, err := sqlite.New(path)
	if err != nil {
		_ = first.Close()
		t.Fatalf("open second sqlite store: %v", err)
	}
	return managedWorkflowStores{
		first: first, second: second,
		firstManaged:  sqlite.NewLeaseDispatcher(first).SetClock(clock),
		secondManaged: sqlite.NewLeaseDispatcher(second).SetClock(clock),
		close: func() {
			_ = second.Close()
			_ = first.Close()
		},
	}
}

func postgresManagedWorkflowStores(t *testing.T, clock runmanager.Clock) managedWorkflowStores {
	t.Helper()
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		if os.Getenv("POSTGRES_TEST_FAIL_ON_SKIP") != "" {
			t.Fatal("POSTGRES_TEST_DSN is required (POSTGRES_TEST_FAIL_ON_SKIP=1)")
		}
		t.Skip("POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	first, err := postgres.New(ctx, dsn, postgres.Options{AutoMigrate: true})
	if err != nil {
		t.Fatalf("open first postgres store: %v", err)
	}
	second, err := postgres.New(ctx, dsn, postgres.Options{})
	if err != nil {
		_ = first.Close()
		t.Fatalf("open second postgres store: %v", err)
	}
	return managedWorkflowStores{
		first: first, second: second,
		firstManaged:  postgres.NewLeaseDispatcher(first).SetClock(clock),
		secondManaged: postgres.NewLeaseDispatcher(second).SetClock(clock),
		close: func() {
			_ = second.Close()
			_ = first.Close()
		},
	}
}

type resumeRaceResult struct {
	output        *session.RunOutput
	err           error
	terminal      workflow.Event
	terminalCount int
}

func TestManagedResumeTwoSQLiteConnectionsSingleStepEntry(t *testing.T) {
	runManagedResumeRace(t, sqliteManagedWorkflowStores, false, nil)
}

func TestManagedResumeEventsTwoSQLiteConnectionsSingleStepEntry(t *testing.T) {
	runManagedResumeRace(t, sqliteManagedWorkflowStores, true, nil)
}

func TestManagedResumeTwoPostgresConnectionsSingleStepEntry(t *testing.T) {
	runManagedResumeRace(t, postgresManagedWorkflowStores, false, nil)
}

func TestWorkflowLimitsManagedResumeTwoSQLiteConnectionsSingleStepEntry(t *testing.T) {
	runManagedResumeRace(t, sqliteManagedWorkflowStores, false, &workflow.Limits{})
}

func TestWorkflowLimitsManagedResumeEventsTwoSQLiteConnectionsSingleStepEntry(t *testing.T) {
	runManagedResumeRace(t, sqliteManagedWorkflowStores, true, &workflow.Limits{})
}

func TestWorkflowLimitsManagedResumeTwoPostgresConnectionsSingleStepEntry(t *testing.T) {
	runManagedResumeRace(t, postgresManagedWorkflowStores, false, &workflow.Limits{})
}

func TestWorkflowLimitsManagedResumeEventsTwoPostgresConnectionsSingleStepEntry(t *testing.T) {
	runManagedResumeRace(t, postgresManagedWorkflowStores, true, &workflow.Limits{})
}

func TestWorkflowLimitsDefinitionManagedResumeRejectsOversizedHistory(t *testing.T) {
	now := time.Now().UTC()
	stores := sqliteManagedWorkflowStores(t, func() time.Time { return now })
	t.Cleanup(stores.close)
	scope := execution.NamespaceScope{Namespace: "workflow-definition-limit-" + uuid.NewString()}
	ctx := execution.ContextWithNamespace(context.Background(), scope.Namespace)
	var entries atomic.Int32
	registry := workflow.NewRegistry()
	if err := registry.RegisterExecutor("write", workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
		entries.Add(1)
		return workflow.NodeOutput{Value: "written"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	node := workflow.ExecutorNode("write", "write")
	node.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementConfirmation}
	definition := workflow.Definition{ID: "managed-definition-limit", Version: "v1", Root: node}
	limits := &workflow.Limits{MaxDefinitionBytes: 512}
	seed, err := workflow.New(workflow.Config{Definition: definition, Registry: registry, DB: stores.first, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := seed.Run(ctx, workflow.RunRequest{RunID: "run-" + uuid.NewString(), UserID: "alice", Input: "value"})
	if err != nil {
		t.Fatalf("seed paused run: %v", err)
	}
	raw, err := json.Marshal(paused)
	if err != nil {
		t.Fatal(err)
	}
	var corrupt session.RunOutput
	if err := json.Unmarshal(raw, &corrupt); err != nil {
		t.Fatal(err)
	}
	corrupt.Workflow.Definition.Name = strings.Repeat("x", 513)
	if err := stores.first.UpdateRun(ctx, &corrupt, session.RunStatusPaused); err != nil {
		t.Fatalf("store oversized definition: %v", err)
	}

	runtime, err := workflow.New(workflow.Config{
		Definition: definition, Registry: registry, Limits: limits,
		ManagedResume: &runmanager.ManagedConfig{
			Store: stores.firstManaged, Scope: scope,
			Worker: runmanager.WorkerIdentity{WorkerID: "definition-limit-worker"}, Lease: time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Resume(ctx, workflow.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID,
		Action: workflow.ResumeActionApprove, UserID: "alice",
	})
	if !errors.Is(err, workflow.ErrDefinitionLimitExceeded) {
		t.Fatalf("Resume error = %v, want definition limit", err)
	}
	if entries.Load() != 0 {
		t.Fatalf("executor entries = %d, want 0", entries.Load())
	}
	persisted, err := stores.first.GetRun(ctx, db.RunQuery{RunID: paused.RunID, UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != session.RunStatusPaused || persisted.Workflow.Requirement == nil || persisted.Workflow.Requirement.Resolution != nil {
		t.Fatalf("managed rejection overwrote history: status=%q requirement=%+v", persisted.Status, persisted.Workflow.Requirement)
	}
}

func runManagedResumeRace(t *testing.T, storesFactory func(*testing.T, runmanager.Clock) managedWorkflowStores, stream bool, limits *workflow.Limits) {
	t.Helper()
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	stores := storesFactory(t, clock)
	t.Cleanup(stores.close)
	scope := execution.NamespaceScope{Namespace: "workflow-race-" + uuid.NewString()}
	ctx := execution.ContextWithNamespace(context.Background(), scope.Namespace)

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var entries atomic.Int32
	registry := workflow.NewRegistry()
	if err := registry.RegisterExecutor("write", workflow.ExecutorFunc(func(ctx context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
		entries.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return workflow.NodeOutput{Value: input.Value}, nil
		case <-ctx.Done():
			return workflow.NodeOutput{}, ctx.Err()
		}
	})); err != nil {
		t.Fatalf("register executor: %v", err)
	}
	node := workflow.ExecutorNode("write", "write")
	node.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementConfirmation}
	definition := workflow.Definition{ID: "managed-race", Version: "v1", Root: node}
	seed, err := workflow.New(workflow.Config{Definition: definition, Registry: registry, DB: stores.first, Limits: limits})
	if err != nil {
		t.Fatalf("new seed workflow: %v", err)
	}
	paused, err := seed.Run(ctx, workflow.RunRequest{RunID: "run-" + uuid.NewString(), UserID: "alice", Input: "value"})
	if err != nil {
		t.Fatalf("seed paused run: %v", err)
	}
	request := workflow.ResumeRequest{RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID, Action: workflow.ResumeActionApprove, UserID: "alice"}
	newRuntime := func(managed runmanager.ManagedStore, worker string) *workflow.Workflow {
		value, newErr := workflow.New(workflow.Config{
			Definition: definition, Registry: registry, Limits: limits,
			ManagedResume: &runmanager.ManagedConfig{
				Store: managed, Scope: scope, Worker: runmanager.WorkerIdentity{WorkerID: worker}, Lease: time.Minute,
			},
		})
		if newErr != nil {
			t.Fatalf("new managed runtime: %v", newErr)
		}
		return value
	}
	runtimes := []*workflow.Workflow{
		newRuntime(stores.firstManaged, "worker-a"),
		newRuntime(stores.secondManaged, "worker-b"),
	}

	gate := make(chan struct{})
	results := make(chan resumeRaceResult, 2)
	for _, runtime := range runtimes {
		runtime := runtime
		go func() {
			<-gate
			if !stream {
				out, resumeErr := runtime.Resume(ctx, request)
				results <- resumeRaceResult{output: out, err: resumeErr}
				return
			}
			events, resumeErr := runtime.ResumeEvents(ctx, request)
			result := resumeRaceResult{err: resumeErr}
			if resumeErr == nil {
				for event := range events {
					if isWorkflowTerminal(event.Type) {
						result.terminal = event
						result.terminalCount++
					}
				}
			}
			results <- result
		}()
	}
	close(gate)
	waitSignal(t, started, "winning workflow step did not start")
	loser := waitResult(t, results, "losing resume did not return")
	if !stream {
		if !errors.Is(loser.err, runmanager.ErrLeaseHeld) {
			t.Fatalf("losing Resume error = %v, want ErrLeaseHeld", loser.err)
		}
	} else if loser.err != nil || loser.terminalCount != 1 || loser.terminal.Type != workflow.EventTypeRunError || !strings.Contains(loser.terminal.Error, runmanager.ErrLeaseHeld.Error()) {
		t.Fatalf("losing ResumeEvents result = %+v, want one recognizable lease-held terminal", loser)
	}
	if got := entries.Load(); got != 1 {
		t.Fatalf("step entries before release = %d, want 1", got)
	}
	releaseOnce.Do(func() { close(release) })
	winner := waitResult(t, results, "winning resume did not return")
	if !stream {
		if winner.err != nil || winner.output == nil || winner.output.Status != session.RunStatusCompleted {
			t.Fatalf("winning Resume result = %+v, %v", winner.output, winner.err)
		}
	} else if winner.err != nil || winner.terminalCount != 1 || winner.terminal.Type != workflow.EventTypeRunCompleted {
		t.Fatalf("winning ResumeEvents result = %+v, want one completed terminal", winner)
	}
	if got := entries.Load(); got != 1 {
		t.Fatalf("step entries = %d, want exactly 1", got)
	}
}

func TestManagedResumeSameInputTakesOverBeforeApplyingAndRejectsStaleOwner(t *testing.T) {
	runManagedResumeTakeover(t, sqliteManagedWorkflowStores, nil)
}

func TestWorkflowLimitsManagedResumeSQLiteLostOwnerCannotWrite(t *testing.T) {
	runManagedResumeTakeover(t, sqliteManagedWorkflowStores, &workflow.Limits{})
}

func TestWorkflowLimitsManagedResumePostgresLostOwnerCannotWrite(t *testing.T) {
	runManagedResumeTakeover(t, postgresManagedWorkflowStores, &workflow.Limits{})
}

func runManagedResumeTakeover(t *testing.T, storesFactory func(*testing.T, runmanager.Clock) managedWorkflowStores, limits *workflow.Limits) {
	t.Helper()
	var nowNanos atomic.Int64
	nowNanos.Store(time.Now().UTC().UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() }
	stores := storesFactory(t, clock)
	t.Cleanup(stores.close)
	scope := execution.NamespaceScope{Namespace: "workflow-takeover-" + uuid.NewString()}
	ctx := execution.ContextWithNamespace(context.Background(), scope.Namespace)

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var takeoverGeneration atomic.Uint64
	registry := workflow.NewRegistry()
	if err := registry.RegisterExecutor("collect", workflow.ExecutorFunc(func(ctx context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
		owner, ok := runmanager.ManagedRunOwnerFromContext(ctx)
		if !ok {
			return workflow.NodeOutput{}, errors.New("managed owner missing from takeover executor context")
		}
		takeoverGeneration.Store(owner.Claim().FencingToken)
		started <- struct{}{}
		select {
		case <-release:
			return workflow.NodeOutput{Value: input.ResumeInput}, nil
		case <-ctx.Done():
			return workflow.NodeOutput{}, ctx.Err()
		}
	})); err != nil {
		t.Fatalf("register executor: %v", err)
	}
	node := workflow.ExecutorNode("collect", "collect")
	node.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementUserInput}
	definition := workflow.Definition{ID: "managed-takeover", Version: "v1", Root: node}
	seed, err := workflow.New(workflow.Config{Definition: definition, Registry: registry, DB: stores.first, Limits: limits})
	if err != nil {
		t.Fatalf("new seed workflow: %v", err)
	}
	paused, err := seed.Run(ctx, workflow.RunRequest{RunID: "run-" + uuid.NewString(), UserID: "alice", Input: "original"})
	if err != nil {
		t.Fatalf("seed paused run: %v", err)
	}
	request := workflow.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID,
		Action: workflow.ResumeActionSubmit, Input: map[string]any{"answer": "same"}, UserID: "alice",
	}
	lease := 10 * time.Minute
	ownerA, err := runmanager.AcquireManagedRunOwner(ctx, paused.RunID, runmanager.ManagedConfig{
		Store: stores.firstManaged, Scope: scope, Worker: runmanager.WorkerIdentity{WorkerID: "crashed-owner"},
		Lease: lease, HeartbeatInterval: 5 * time.Minute, HeartbeatTimeout: time.Minute, Clock: clock,
	})
	if err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}
	snapshot, err := ownerA.Snapshot(ctx)
	if err != nil {
		t.Fatalf("owner A snapshot: %v", err)
	}
	interrupted := snapshot.Run
	resolution := &workflowschema.Resolution{ActorID: "reviewer", Action: string(request.Action), Input: request.Input, At: clock()}
	interrupted.Workflow.Requirement.Resolution = resolution
	interrupted.Workflow.RequirementHistory[0].Resolution = resolution
	interrupted.Status = session.RunStatusRunning
	interrupted.UpdatedAt = clock()
	if _, err := ownerA.Update(ctx, interrupted, session.RunStatusPaused); err != nil {
		t.Fatalf("owner A paused-to-running commit: %v", err)
	}
	oldGeneration := ownerA.Claim().FencingToken
	if err := ownerA.StopHeartbeat(context.Background()); err != nil {
		t.Fatalf("stop crashed owner heartbeat: %v", err)
	}
	nowNanos.Add(int64(lease))

	mismatched, err := workflow.New(workflow.Config{
		Definition: definition, Registry: registry, Limits: limits,
		ManagedResume: &runmanager.ManagedConfig{Store: stores.secondManaged, Scope: scope, Worker: runmanager.WorkerIdentity{WorkerID: "mismatch-owner"}, Lease: lease, Clock: clock},
	})
	if err != nil {
		t.Fatalf("new mismatch runtime: %v", err)
	}
	wrongAction := request
	wrongAction.Action = workflow.ResumeActionApprove
	if _, err := mismatched.Resume(ctx, wrongAction); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("different-action takeover = %v, want conflict", err)
	}
	wrong := request
	wrong.Input = map[string]any{"answer": "different"}
	if _, err := mismatched.Resume(ctx, wrong); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("different-input takeover = %v, want conflict", err)
	}

	takeover, err := workflow.New(workflow.Config{
		Definition: definition, Registry: registry, Limits: limits,
		ManagedResume: &runmanager.ManagedConfig{Store: stores.secondManaged, Scope: scope, Worker: runmanager.WorkerIdentity{WorkerID: "takeover-owner"}, Lease: lease, Clock: clock},
	})
	if err != nil {
		t.Fatalf("new takeover runtime: %v", err)
	}
	done := make(chan resumeRaceResult, 1)
	go func() {
		out, resumeErr := takeover.Resume(ctx, request)
		done <- resumeRaceResult{output: out, err: resumeErr}
	}()
	waitSignal(t, started, "takeover step did not start")
	stale := *interrupted
	stale.Content = "stale overwrite"
	var staleErr error
	if limits == nil {
		_, staleErr = ownerA.Update(ctx, &stale, session.RunStatusRunning)
	} else {
		_, staleErr = ownerA.UpdateBounded(ctx, &stale, session.RunStatusRunning)
	}
	if !errors.Is(staleErr, runmanager.ErrOwnershipLost) || !errors.Is(staleErr, runmanager.ErrLeaseExpired) {
		t.Fatalf("stale owner late update = %v, want ownership-lost lease-expired", staleErr)
	}
	releaseOnce.Do(func() { close(release) })
	result := waitResult(t, done, "takeover did not complete")
	if result.err != nil || result.output == nil || result.output.Status != session.RunStatusCompleted {
		t.Fatalf("takeover result = %+v, %v", result.output, result.err)
	}
	if got := result.output.Workflow.Output; !sameJSONValue(got, request.Input) {
		t.Fatalf("takeover output = %#v, want persisted resume input %#v", got, request.Input)
	}
	if got := takeoverGeneration.Load(); got <= oldGeneration {
		t.Fatalf("takeover generation = %d, want > crashed generation %d", got, oldGeneration)
	}
}

func TestManagedResumeApplyingFailsClosedUntilExplicitRetryClaimsNewOwner(t *testing.T) {
	var nowNanos atomic.Int64
	nowNanos.Store(time.Now().UTC().UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() }
	stores := sqliteManagedWorkflowStores(t, clock)
	t.Cleanup(stores.close)
	scope := execution.NamespaceScope{Namespace: "workflow-applying-" + uuid.NewString()}
	ctx := execution.ContextWithNamespace(context.Background(), scope.Namespace)

	var entries atomic.Int32
	var retryGeneration atomic.Uint64
	registry := workflow.NewRegistry()
	if err := registry.RegisterExecutor("write", workflow.ExecutorFunc(func(ctx context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
		entries.Add(1)
		owner, ok := runmanager.ManagedRunOwnerFromContext(ctx)
		if !ok {
			return workflow.NodeOutput{}, errors.New("managed owner missing from executor context")
		}
		retryGeneration.Store(owner.Claim().FencingToken)
		return workflow.NodeOutput{Value: input.ResumeInput}, nil
	})); err != nil {
		t.Fatalf("register executor: %v", err)
	}
	node := workflow.ExecutorNode("write", "write")
	node.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementUserInput}
	definition := workflow.Definition{ID: "managed-applying", Version: "v1", Root: node}
	seed, err := workflow.New(workflow.Config{Definition: definition, Registry: registry, DB: stores.first})
	if err != nil {
		t.Fatalf("new seed workflow: %v", err)
	}
	paused, err := seed.Run(ctx, workflow.RunRequest{RunID: "run-" + uuid.NewString(), UserID: "alice", Input: "original"})
	if err != nil {
		t.Fatalf("seed paused run: %v", err)
	}
	lease := 10 * time.Minute
	ownerA, err := runmanager.AcquireManagedRunOwner(ctx, paused.RunID, runmanager.ManagedConfig{
		Store: stores.firstManaged, Scope: scope, Worker: runmanager.WorkerIdentity{WorkerID: "crashed-applying-owner"},
		Lease: lease, HeartbeatInterval: 5 * time.Minute, HeartbeatTimeout: time.Minute, Clock: clock,
	})
	if err != nil {
		t.Fatalf("owner A acquire: %v", err)
	}
	snapshot, err := ownerA.Snapshot(ctx)
	if err != nil {
		t.Fatalf("owner A snapshot: %v", err)
	}
	interrupted := snapshot.Run
	resumeInput := map[string]any{"approved": true}
	resolution := &workflowschema.Resolution{ActorID: "reviewer", Action: string(workflow.ResumeActionSubmit), Input: resumeInput, At: clock()}
	interrupted.Workflow.Requirement.Resolution = resolution
	interrupted.Workflow.RequirementHistory[0].Resolution = resolution
	interrupted.Status = session.RunStatusRunning
	interrupted.UpdatedAt = clock()
	if _, err := ownerA.Update(ctx, interrupted, session.RunStatusPaused); err != nil {
		t.Fatalf("owner A resume commit: %v", err)
	}
	interrupted.Workflow.Requirement = nil
	interrupted.Workflow.Checkpoint = nil
	interrupted.Workflow.NodeResults["write"] = workflow.NodeResult{
		NodeID: "write", NodeType: workflow.NodeTypeStep, Status: workflowschema.NodeStatusApplying, StartedAt: clock(),
	}
	if _, err := ownerA.Update(ctx, interrupted, session.RunStatusRunning); err != nil {
		t.Fatalf("owner A applying commit: %v", err)
	}
	oldGeneration := ownerA.Claim().FencingToken
	if err := ownerA.StopHeartbeat(context.Background()); err != nil {
		t.Fatalf("stop crashed owner heartbeat: %v", err)
	}
	nowNanos.Add(int64(lease))

	newRuntime := func(worker string) *workflow.Workflow {
		value, newErr := workflow.New(workflow.Config{
			Definition: definition, Registry: registry,
			ManagedResume: &runmanager.ManagedConfig{Store: stores.secondManaged, Scope: scope, Worker: runmanager.WorkerIdentity{WorkerID: worker}, Lease: lease, Clock: clock},
		})
		if newErr != nil {
			t.Fatalf("new managed runtime: %v", newErr)
		}
		return value
	}
	withoutRetry := newRuntime("fail-closed-owner")
	if _, err := withoutRetry.Resume(ctx, workflow.ResumeRequest{RunID: paused.RunID, Action: workflow.ResumeActionSubmit, Input: resumeInput, UserID: "alice"}); !errors.Is(err, workflow.ErrApplyingUnknown) {
		t.Fatalf("resume applying without retry = %v, want ErrApplyingUnknown", err)
	}
	if got := entries.Load(); got != 0 {
		t.Fatalf("step entries before explicit retry = %d, want 0", got)
	}

	retry := newRuntime("retry-owner")
	out, err := retry.Resume(ctx, workflow.ResumeRequest{RunID: paused.RunID, Action: workflow.ResumeActionRetryApplying, UserID: "alice"})
	if err != nil {
		t.Fatalf("explicit applying retry: %v", err)
	}
	if out.Status != session.RunStatusCompleted || !sameJSONValue(out.Workflow.Output, resumeInput) {
		t.Fatalf("retry output = %+v, want completed with persisted resume input", out)
	}
	if got := entries.Load(); got != 1 {
		t.Fatalf("explicit retry step entries = %d, want 1", got)
	}
	if got := retryGeneration.Load(); got <= oldGeneration {
		t.Fatalf("retry generation = %d, want > crashed generation %d", got, oldGeneration)
	}
}

func isWorkflowTerminal(eventType workflow.EventType) bool {
	switch eventType {
	case workflow.EventTypeRunPaused, workflow.EventTypeRunCompleted, workflow.EventTypeRunError, workflow.EventTypeRunCancelled:
		return true
	default:
		return false
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

func waitResult(t *testing.T, results <-chan resumeRaceResult, message string) resumeRaceResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal(message)
		return resumeRaceResult{}
	}
}

func sameJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
