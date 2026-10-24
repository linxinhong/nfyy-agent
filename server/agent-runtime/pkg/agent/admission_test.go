package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

type admissionBlockingModel struct {
	started chan struct{}
	release chan struct{}
}

func (m *admissionBlockingModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	m.started <- struct{}{}
	select {
	case <-m.release:
		return &models.ModelResponse{Content: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *admissionBlockingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not used")
}
func (m *admissionBlockingModel) ID() string       { return "admission-blocking" }
func (m *admissionBlockingModel) Provider() string { return "test" }

func TestAgentAdmissionCoversDirectStreamAndResume(t *testing.T) {
	controller, err := run.NewAdmissionController(run.AdmissionConfig{Total: 1, Queue: 2, PerTenant: 1, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	holdModel := &admissionBlockingModel{started: make(chan struct{}, 1), release: make(chan struct{})}
	holder, err := New(AgentConfig{Model: holdModel, MaxConcurrentRuns: 1, Admission: controller})
	if err != nil {
		t.Fatalf("New holder: %v", err)
	}
	holderDone := make(chan error, 1)
	go func() { _, runErr := holder.Run(context.Background(), "hold"); holderDone <- runErr }()
	<-holdModel.started
	waitAgentAdmission(t, controller, 1, 0)

	// The pre-existing local slot remains fail-fast and does not add a shared
	// queue entry while the same Agent is already active.
	if _, err := holder.Run(context.Background(), "busy"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second local run = %v, want ErrBusy", err)
	}
	waitAgentAdmission(t, controller, 1, 0)

	streamModel := mock.New()
	streamModel.StreamEvents = []models.StreamEvent{{Event: "content_delta", Content: "stream"}}
	streamAgent, err := New(AgentConfig{Model: streamModel, Admission: controller})
	if err != nil {
		t.Fatalf("New stream agent: %v", err)
	}
	streamResult := make(chan struct {
		ch  <-chan Event
		err error
	}, 1)
	go func() {
		ch, streamErr := streamAgent.RunStream(context.Background(), "wait")
		streamResult <- struct {
			ch  <-chan Event
			err error
		}{ch, streamErr}
	}()
	waitAgentAdmission(t, controller, 1, 1)
	select {
	case result := <-streamResult:
		t.Fatalf("stream returned before a permit: %+v", result.err)
	default:
	}
	close(holdModel.release)
	if err := <-holderDone; err != nil {
		t.Fatalf("held run: %v", err)
	}
	stream := <-streamResult
	if stream.err != nil {
		t.Fatalf("RunStream after permit: %v", stream.err)
	}
	for range stream.ch {
	}
	waitAgentAdmission(t, controller, 0, 0)

	// Resume is a separate public entry point, so keep it behind an occupied
	// shared permit and prove it cannot advance until the permit is released.
	var calls int32
	tool := approvalTool{kind: tools.ApprovalRequired, calls: &calls}
	model := &queueModel{responses: []*models.ModelResponse{
		{Content: "pause", ToolCalls: []models.ToolCall{{ID: "call", Type: "function", Function: models.FunctionCall{Name: "approval_tool", Arguments: "{}"}}}},
		{Content: "done"},
	}}
	store := in_memory.New()
	approvalAgent, err := New(AgentConfig{ID: "admission-resume", Model: model, Tools: []tools.Tool{tool}, DB: store, UserID: "user", Admission: controller})
	if err != nil {
		t.Fatalf("New approval agent: %v", err)
	}
	paused, err := approvalAgent.Run(context.Background(), "pause")
	if err != nil || paused.Status != session.RunStatusPaused {
		t.Fatalf("paused Run = %+v, %v", paused, err)
	}
	_, held, err := controller.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold resume admission: %v", err)
	}
	resumeResult := make(chan struct {
		out *session.RunOutput
		err error
	}, 1)
	go func() {
		out, resumeErr := approvalAgent.Resume(context.Background(), session.ResumeRequest{RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID, Action: string(requirement.ActionApprove)})
		resumeResult <- struct {
			out *session.RunOutput
			err error
		}{out, resumeErr}
	}()
	waitAgentAdmission(t, controller, 1, 1)
	held.Release()
	resumed := <-resumeResult
	if resumed.err != nil || resumed.out.Status != session.RunStatusCompleted {
		t.Fatalf("Resume after permit = %+v, %v", resumed.out, resumed.err)
	}
	waitAgentAdmission(t, controller, 0, 0)
}

func TestAgentAdmissionCancellationReleasesLocalSlot(t *testing.T) {
	controller, err := run.NewAdmissionController(run.AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	_, held, err := controller.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold admission: %v", err)
	}
	value, err := New(AgentConfig{Model: mock.New(), MaxConcurrentRuns: 1, Admission: controller})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, runErr := value.Run(ctx, "cancel"); result <- runErr }()
	waitAgentAdmission(t, controller, 1, 1)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run = %v, want context.Canceled", err)
	}
	waitAgentAdmission(t, controller, 1, 0)
	held.Release()
	if _, err := value.Run(context.Background(), "after cancel"); err != nil {
		t.Fatalf("local slot not released after cancelled queue wait: %v", err)
	}
}

func TestManagerAdmissionPermitIsReusedByAgent(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "admission-agent", UserID: "alice"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	controller, err := run.NewAdmissionController(run.AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	model := &admissionBlockingModel{started: make(chan struct{}, 1), release: make(chan struct{})}
	value, err := New(AgentConfig{ID: "admission-agent", Model: model, DB: store, SessionID: "s1", UserID: "alice", Admission: controller})
	if err != nil {
		t.Fatalf("New Agent: %v", err)
	}
	manager := run.New(store, time.Second).SetAdmission(controller)
	started := make(chan error, 1)
	go func() {
		_, startErr := manager.Start(session.RunRequest{SessionID: "s1", UserID: "alice"}, value.RunWith)
		started <- startErr
	}()
	<-model.started
	select {
	case startErr := <-started:
		if startErr != nil {
			t.Fatalf("background Start: %v", startErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Manager->Agent path waited for a duplicate admission permit")
	}
	waitAgentAdmission(t, controller, 1, 0)
	close(model.release)
	waitAgentAdmission(t, controller, 0, 0)
}

func TestManagerKeepsAdmissionUntilOuterAgentExecutorExits(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "outer-agent", UserID: "alice"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	controller, err := run.NewAdmissionController(run.AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	value, err := New(AgentConfig{ID: "outer-agent", Model: mock.New(), DB: store, SessionID: "s1", UserID: "alice", Admission: controller})
	if err != nil {
		t.Fatalf("New Agent: %v", err)
	}
	manager := run.New(store, time.Second).SetAdmission(controller)
	agentReturned := make(chan struct{}, 1)
	allowOuterExit := make(chan struct{})
	startResult := make(chan error, 1)
	go func() {
		_, startErr := manager.Start(session.RunRequest{SessionID: "s1", UserID: "alice"}, func(ctx context.Context, request session.RunRequest) (*session.RunOutput, error) {
			output, runErr := value.RunWith(ctx, request)
			agentReturned <- struct{}{}
			<-allowOuterExit
			return output, runErr
		})
		startResult <- startErr
	}()
	select {
	case err := <-startResult:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("background run did not durably register")
	}
	<-agentReturned
	waitAgentAdmission(t, controller, 1, 0)

	queued := make(chan *run.AdmissionPermit, 1)
	go func() {
		_, permit, _ := controller.Acquire(context.Background())
		queued <- permit
	}()
	waitAgentAdmission(t, controller, 1, 1)
	select {
	case permit := <-queued:
		t.Fatalf("new run acquired while outer executor still blocked: %v", permit)
	default:
	}
	close(allowOuterExit)
	var permit *run.AdmissionPermit
	select {
	case permit = <-queued:
	case <-time.After(time.Second):
		t.Fatal("queued run did not acquire after outer executor exited")
	}
	permit.Release()
	waitAgentAdmission(t, controller, 0, 0)
}

func waitAgentAdmission(t *testing.T, controller *run.AdmissionController, active, queued int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stats := controller.Stats()
		if stats.Active == active && stats.Queued == queued {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("admission stats = %+v, want active=%d queued=%d", controller.Stats(), active, queued)
}
