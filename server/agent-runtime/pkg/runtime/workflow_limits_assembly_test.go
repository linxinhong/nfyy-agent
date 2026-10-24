package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	agentruntime "github.com/linxinhong/xingding-agent-runtime/pkg/runtime"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

func TestWorkflowLimitsAssemblyDecodeValidateBuildAndRun(t *testing.T) {
	raw := `{
		"schema_version":5,"id":"limited-project","version":"v1",
		"workflows":[{"definition":{"id":"limited","version":"v1","root":{"id":"echo","type":"step","executor_ref":"echo","policy":{}}},
		"limits":{"max_input_bytes":16,"max_node_output_bytes":64,"max_concurrent_callbacks":2,"run_timeout":1000000000}}]
	}`
	spec, err := agentruntime.DecodeSpec(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if spec.Workflows[0].Limits == nil || spec.Workflows[0].Limits.MaxInputBytes != 16 || spec.Workflows[0].Limits.MaxConcurrentCallbacks != 2 {
		t.Fatalf("decoded limits = %+v", spec.Workflows[0].Limits)
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		Executors: map[string]workflow.Executor{"echo": workflow.ExecutorFunc(func(_ context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
			return workflow.NodeOutput{Value: input.Value}, nil
		})},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wf, ok := project.Workflow("limited")
	if !ok {
		t.Fatal("assembled workflow is missing")
	}
	if _, err := wf.Run(context.Background(), strings.Repeat("x", 32)); !errors.Is(err, workflow.ErrPayloadLimitExceeded) {
		t.Fatalf("assembled Run error = %v, want payload limit", err)
	}

	encoded, err := json.Marshal(spec)
	if err != nil || !strings.Contains(string(encoded), `"max_input_bytes":16`) {
		t.Fatalf("snapshot JSON = %s, %v", encoded, err)
	}
}

func TestWorkflowLimitsAssemblyInvalidFieldsHavePaths(t *testing.T) {
	_, err := agentruntime.Build(agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.SchemaVersion5, ID: "invalid-limits", Version: "v1",
		Workflows: []agentruntime.WorkflowSpec{{
			Definition: workflow.Definition{ID: "wf", Version: "v1", Root: workflow.ExecutorNode("node", "echo")},
			Limits:     &workflow.Limits{MaxInputBytes: -1, PersistenceTimeout: -time.Second},
		}},
	}, agentruntime.Dependencies{Executors: map[string]workflow.Executor{"echo": workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
		return workflow.NodeOutput{}, nil
	})}})
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build error = %v", err)
	}
	wants := map[string]bool{
		"workflows[0].limits.max_input_bytes":     false,
		"workflows[0].limits.persistence_timeout": false,
	}
	for _, diagnostic := range buildErr.Diagnostics {
		if _, exists := wants[diagnostic.Path]; exists {
			wants[diagnostic.Path] = true
		}
	}
	for path, found := range wants {
		if !found {
			t.Fatalf("missing diagnostic %s: %+v", path, buildErr.Diagnostics)
		}
	}
}

func TestWorkflowLimitsAssemblyOldSpecsRemainLegacy(t *testing.T) {
	for _, schemaVersion := range []int{1, 2, 3, 4} {
		raw := `{"schema_version":` + strconv.Itoa(schemaVersion) + `,"id":"legacy","version":"v1","workflows":[{"definition":{"id":"wf","version":"v1","root":{"id":"node","type":"step","executor_ref":"legacy","policy":{}}}}]}`
		spec, err := agentruntime.DecodeSpec(strings.NewReader(raw))
		if err != nil {
			t.Fatalf("DecodeSpec v%d: %v", schemaVersion, err)
		}
		project, err := agentruntime.Build(spec, agentruntime.Dependencies{Executors: map[string]workflow.Executor{
			"legacy": workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
				return workflow.NodeOutput{Value: struct {
					Message string `json:"message"`
				}{Message: "compatible"}}, nil
			}),
		}})
		if err != nil {
			t.Fatalf("Build v%d: %v", schemaVersion, err)
		}
		wf, _ := project.Workflow("wf")
		out, err := wf.Run(context.Background(), workflow.RunRequest{Input: struct {
			Input string `json:"input"`
		}{Input: "legacy"}})
		if err != nil || out.Status != "completed" {
			t.Fatalf("legacy v%d Run = %+v, %v", schemaVersion, out, err)
		}
	}

	for _, value := range []string{`null`, `{}`} {
		_, err := agentruntime.DecodeSpec(strings.NewReader(`{"schema_version":4,"id":"old","version":"v1","workflows":[{"definition":{"id":"wf","version":"v1","root":{"id":"node","type":"step","executor_ref":"legacy","policy":{}}},"limits":` + value + `}]}`))
		var buildErr *agentruntime.BuildError
		if !errors.As(err, &buildErr) || len(buildErr.Diagnostics) != 1 || buildErr.Diagnostics[0].Path != "workflows[0].limits" {
			t.Fatalf("v4 explicit limits=%s error = %v", value, err)
		}
	}
}

func TestWorkflowLimitsAssemblySDKAndProjectConcurrencyMatch(t *testing.T) {
	definition := workflow.Definition{ID: "parallel", Version: "v1", Root: workflow.Parallel("root",
		workflow.ExecutorNode("one", "gate"), workflow.ExecutorNode("two", "gate"),
		workflow.ExecutorNode("three", "gate"), workflow.ExecutorNode("four", "gate"),
	)}
	runFixture := func(t *testing.T, build func(workflow.Executor) (*workflow.Workflow, error)) {
		t.Helper()
		entered := make(chan struct{}, 4)
		release := make(chan struct{}, 4)
		var active atomic.Int32
		var peak atomic.Int32
		executor := workflow.ExecutorFunc(func(ctx context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
			current := active.Add(1)
			for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
			}
			defer active.Add(-1)
			entered <- struct{}{}
			select {
			case <-release:
				return workflow.NodeOutput{Value: input.NodeID}, nil
			case <-ctx.Done():
				return workflow.NodeOutput{}, ctx.Err()
			}
		})
		wf, err := build(executor)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := wf.Run(ctx, "input")
			done <- err
		}()
		waitEntered := func() {
			t.Helper()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatalf("waiting for callback: %v", ctx.Err())
			}
		}
		waitEntered()
		waitEntered()
		release <- struct{}{}
		release <- struct{}{}
		waitEntered()
		waitEntered()
		release <- struct{}{}
		release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := peak.Load(); got != 2 {
			t.Fatalf("peak callbacks = %d, want 2", got)
		}
	}

	t.Run("SDK", func(t *testing.T) {
		runFixture(t, func(executor workflow.Executor) (*workflow.Workflow, error) {
			registry := workflow.NewRegistry()
			if err := registry.RegisterExecutor("gate", executor); err != nil {
				return nil, err
			}
			return workflow.New(workflow.Config{Definition: definition, Registry: registry, Limits: &workflow.Limits{MaxConcurrentCallbacks: 2}})
		})
	})
	t.Run("ProjectSpec", func(t *testing.T) {
		runFixture(t, func(executor workflow.Executor) (*workflow.Workflow, error) {
			project, err := agentruntime.Build(agentruntime.ProjectSpec{
				SchemaVersion: agentruntime.SchemaVersion5, ID: "assembly", Version: "v1",
				Workflows: []agentruntime.WorkflowSpec{{Definition: definition, Limits: &workflow.Limits{MaxConcurrentCallbacks: 2}}},
			}, agentruntime.Dependencies{Executors: map[string]workflow.Executor{"gate": executor}})
			if err != nil {
				return nil, err
			}
			wf, _ := project.Workflow("parallel")
			return wf, nil
		})
	})
}

func TestWorkflowLimitsAssemblySDKAndProjectDataDeadlineMatch(t *testing.T) {
	definition := workflow.Definition{ID: "bounded", Version: "v1", Root: workflow.ExecutorNode("node", "work")}
	buildSDK := func(limits workflow.Limits, executor workflow.Executor) (*workflow.Workflow, error) {
		registry := workflow.NewRegistry()
		if err := registry.RegisterExecutor("work", executor); err != nil {
			return nil, err
		}
		return workflow.New(workflow.Config{Definition: definition, Registry: registry, Limits: &limits})
	}
	buildProject := func(limits workflow.Limits, executor workflow.Executor) (*workflow.Workflow, error) {
		project, err := agentruntime.Build(agentruntime.ProjectSpec{
			SchemaVersion: agentruntime.SchemaVersion5, ID: "assembly", Version: "v1",
			Workflows: []agentruntime.WorkflowSpec{{Definition: definition, Limits: &limits}},
		}, agentruntime.Dependencies{Executors: map[string]workflow.Executor{"work": executor}})
		if err != nil {
			return nil, err
		}
		wf, _ := project.Workflow("bounded")
		return wf, nil
	}

	for name, build := range map[string]func(workflow.Limits, workflow.Executor) (*workflow.Workflow, error){
		"SDK": buildSDK, "ProjectSpec": buildProject,
	} {
		t.Run(name+"Data", func(t *testing.T) {
			var calls atomic.Int32
			wf, err := build(workflow.Limits{MaxInputBytes: 16}, workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
				calls.Add(1)
				return workflow.NodeOutput{}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wf.Run(context.Background(), workflow.RunRequest{Input: strings.Repeat("x", 32)}); !errors.Is(err, workflow.ErrPayloadLimitExceeded) {
				t.Fatalf("Run error = %v, want payload limit", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("executor calls = %d, want 0", calls.Load())
			}
			events, err := wf.RunEvents(context.Background(), workflow.RunRequest{Input: strings.Repeat("x", 32)})
			if err != nil {
				t.Fatalf("RunEvents: %v", err)
			}
			terminal := drainWorkflowEvents(events)
			if terminal.Type != workflow.EventTypeRunError || !strings.Contains(terminal.Error, "payload limit exceeded") {
				t.Fatalf("RunEvents terminal = %+v, want payload limit", terminal)
			}
		})
		t.Run(name+"Deadline", func(t *testing.T) {
			wf, err := build(workflow.Limits{RunTimeout: 20 * time.Millisecond}, workflow.ExecutorFunc(func(ctx context.Context, _ workflow.NodeInput) (workflow.NodeOutput, error) {
				<-ctx.Done()
				return workflow.NodeOutput{}, ctx.Err()
			}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wf.Run(context.Background(), "input"); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Run error = %v, want deadline", err)
			}
		})
	}
}

func TestWorkflowLimitsAssemblyResumeEntryPoints(t *testing.T) {
	store := in_memory.New()
	definition := workflow.Definition{ID: "review", Version: "v1", Root: workflow.ExecutorNode("approval", "work")}
	definition.Root.HumanReview = &workflow.HumanReview{Kind: workflow.RequirementUserInput, Prompt: "input"}
	var calls atomic.Int32
	project, err := agentruntime.Build(limitedProjectSpec(definition, workflow.Limits{MaxInputBytes: 16}), agentruntime.Dependencies{
		DB: store,
		Executors: map[string]workflow.Executor{"work": workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
			calls.Add(1)
			return workflow.NodeOutput{Value: "unexpected"}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, _ := project.Workflow("review")
	for _, streamed := range []bool{false, true} {
		paused, err := wf.Run(context.Background(), "start")
		if err != nil || paused.Workflow.Requirement == nil {
			t.Fatalf("Run paused = %+v, %v", paused, err)
		}
		request := workflow.ResumeRequest{
			RunID: paused.RunID, RequirementID: paused.Workflow.Requirement.ID,
			Action: workflow.ResumeActionSubmit, Input: strings.Repeat("x", 32),
		}
		if streamed {
			events, err := wf.ResumeEvents(context.Background(), request)
			if err != nil {
				t.Fatalf("ResumeEvents: %v", err)
			}
			terminal := drainWorkflowEvents(events)
			if terminal.Type != workflow.EventTypeRunError || !strings.Contains(terminal.Error, "payload limit exceeded") {
				t.Fatalf("ResumeEvents terminal = %+v, want payload limit", terminal)
			}
		} else if _, err := wf.Resume(context.Background(), request); !errors.Is(err, workflow.ErrPayloadLimitExceeded) {
			t.Fatalf("Resume error = %v, want payload limit", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("executor calls = %d, want 0", calls.Load())
	}
}

func TestWorkflowLimitsAssemblyBoundsDefinitionBeforeProjectSnapshot(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		root := workflow.NodeSpec{ID: "root", Type: workflow.NodeTypeCondition, PredicateRef: "choose"}
		root.Then = &root
		root.Else = nodePtr(workflow.ExecutorNode("else", "echo"))
		_, err := agentruntime.Build(limitedProjectSpec(workflow.Definition{ID: "wf", Version: "v1", Root: root}, workflow.Limits{MaxDefinitionDepth: 8}), agentruntime.Dependencies{})
		if err == nil {
			t.Fatal("Build accepted cyclic definition")
		}
	})

	t.Run("depth", func(t *testing.T) {
		node := workflow.ExecutorNode("leaf", "echo")
		for i := 0; i < 16; i++ {
			node = workflow.Sequence("level-"+strconv.Itoa(i), node)
		}
		_, err := agentruntime.Build(limitedProjectSpec(workflow.Definition{ID: "wf", Version: "v1", Root: node}, workflow.Limits{MaxDefinitionDepth: 4}), agentruntime.Dependencies{})
		assertDiagnosticPath(t, err, "workflows[0].limits.max_definition_depth")
	})

	t.Run("raw normalization", func(t *testing.T) {
		definition := workflow.Definition{ID: "wf", Version: "v1", Root: workflow.ExecutorNode("node", "echo")}
		definition.Root.Config = json.RawMessage(strings.Repeat(" ", 1<<20) + `{}`)
		var config string
		project, err := agentruntime.Build(limitedProjectSpec(definition, workflow.Limits{MaxDefinitionBytes: 1024}), agentruntime.Dependencies{
			Executors: map[string]workflow.Executor{"echo": workflow.ExecutorFunc(func(_ context.Context, input workflow.NodeInput) (workflow.NodeOutput, error) {
				config = string(input.Config)
				return workflow.NodeOutput{Value: "ok"}, nil
			})},
		})
		if err != nil {
			t.Fatal(err)
		}
		wf, _ := project.Workflow("wf")
		if _, err := wf.Run(context.Background(), "input"); err != nil {
			t.Fatal(err)
		}
		if config != `{}` {
			t.Fatalf("executor config = %q, want compact JSON", config)
		}
	})
}

func limitedProjectSpec(definition workflow.Definition, limits workflow.Limits) agentruntime.ProjectSpec {
	return agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.SchemaVersion5, ID: "limited", Version: "v1",
		Workflows: []agentruntime.WorkflowSpec{{Definition: definition, Limits: &limits}},
	}
}

func nodePtr(node workflow.NodeSpec) *workflow.NodeSpec { return &node }

func drainWorkflowEvents(events <-chan workflow.Event) workflow.Event {
	var terminal workflow.Event
	for event := range events {
		terminal = event
	}
	return terminal
}

func assertDiagnosticPath(t *testing.T, err error, want string) {
	t.Helper()
	var buildErr *agentruntime.BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build error = %v", err)
	}
	for _, diagnostic := range buildErr.Diagnostics {
		if diagnostic.Path == want {
			return
		}
	}
	t.Fatalf("missing diagnostic %q: %+v", want, buildErr.Diagnostics)
}

func TestWorkflowLimitsAssemblyHTTPCannotRaiseHostBudget(t *testing.T) {
	store := in_memory.New()
	var calls atomic.Int32
	project, err := agentruntime.Build(agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.SchemaVersion5, ID: "http-limits", Version: "v1",
		Workflows: []agentruntime.WorkflowSpec{{
			Definition: workflow.Definition{ID: "limited", Version: "v1", Root: workflow.ExecutorNode("node", "echo")},
			Limits:     &workflow.Limits{MaxInputBytes: 16},
		}},
	}, agentruntime.Dependencies{
		DB: store,
		Executors: map[string]workflow.Executor{"echo": workflow.ExecutorFunc(func(context.Context, workflow.NodeInput) (workflow.NodeOutput, error) {
			calls.Add(1)
			return workflow.NodeOutput{Value: "unexpected"}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"message":"` + strings.Repeat("x", 64) + `","limits":{"max_input_bytes":1048576}}`
	for _, path := range []string{"/workflows/limited/runs", "/workflows/limited/runs?stream=true"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		project.AgentOS().Handler().ServeHTTP(recorder, request)
		if strings.Contains(path, "stream") {
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "run_error") || !strings.Contains(recorder.Body.String(), "payload limit exceeded") {
				t.Fatalf("stream response = %d %s", recorder.Code, recorder.Body.String())
			}
		} else if recorder.Code == http.StatusOK {
			t.Fatalf("HTTP request raised host budget: %s", recorder.Body.String())
		}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/workflows/limited/runs?background=true", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	project.AgentOS().Handler().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusAccepted || recorder.Code == http.StatusOK {
		t.Fatalf("background request raised host budget: %d %s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("executor calls after rejected requests = %d, want 0", calls.Load())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/workflows/limited/runs?background=true", strings.NewReader(`{"message":"ok"}`))
	request.Header.Set("Content-Type", "application/json")
	project.AgentOS().Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("valid background response = %d %s", recorder.Code, recorder.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("valid background run did not enter executor")
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("executor calls = %d, want 1 valid background call", calls.Load())
	}
}
