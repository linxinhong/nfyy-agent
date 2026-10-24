package team

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	mockmodel "github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// countingLeader is a fake model that delegates once, then returns a final answer.
type countingLeader struct {
	mu    sync.Mutex
	calls int
}

type metadataModel struct {
	inner *mockmodel.MockModel
	seen  chan map[string]string
}

type cancellableStreamModel struct{}

func (m *cancellableStreamModel) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	return &models.ModelResponse{Content: "done"}, nil
}
func (m *cancellableStreamModel) ResponseStream(ctx context.Context, _ []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		<-ctx.Done()
		ch <- models.StreamEvent{Event: "error", Err: ctx.Err()}
	}()
	return ch, nil
}
func (m *cancellableStreamModel) ID() string       { return "cancellable" }
func (m *cancellableStreamModel) Provider() string { return "mock" }

func (m *metadataModel) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	metadata, _ := session.RunMetadataFromContext(ctx)
	m.seen <- metadata
	return m.inner.Response(ctx, messages, opts)
}
func (m *metadataModel) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	return m.inner.ResponseStream(ctx, messages, opts)
}
func (m *metadataModel) ID() string       { return "metadata-member" }
func (m *metadataModel) Provider() string { return "mock" }

func (m *countingLeader) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return &models.ModelResponse{
			ToolCalls: []models.ToolCall{
				{
					ID:   "tc1",
					Type: "function",
					Function: models.FunctionCall{
						Name:      "delegate_to_member",
						Arguments: `{"member_id":"member1","task":"do the thing"}`,
					},
				},
			},
		}, nil
	}
	return &models.ModelResponse{Content: "final answer"}, nil
}

func (m *countingLeader) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++

	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		if m.calls == 1 {
			ch <- models.StreamEvent{
				Event: "tool_call_delta",
				ToolCall: &models.ToolCall{
					Index: 0,
					ID:    "tc1",
					Type:  "function",
					Function: models.FunctionCall{
						Name:      "delegate_to_member",
						Arguments: `{"member_id":"member1","task":"do the thing"}`,
					},
				},
			}
		} else {
			ch <- models.StreamEvent{Event: "content_delta", Content: "final answer"}
		}
		ch <- models.StreamEvent{Event: "done", Done: true}
	}()
	return ch, nil
}

func (m *countingLeader) ID() string       { return "counting-leader" }
func (m *countingLeader) Provider() string { return "mock" }

func TestTeamCoordinateRun(t *testing.T) {
	ctx := context.Background()
	db := in_memory.New()

	memberMock := mockmodel.New()
	memberMock.ResponseData = &models.ModelResponse{Content: "member output"}
	memberModel := &metadataModel{inner: memberMock, seen: make(chan map[string]string, 1)}
	memberAgent, err := agent.New(agent.AgentConfig{ID: "member1", Name: "Member 1", Model: memberModel})
	if err != nil {
		t.Fatalf("create member agent: %v", err)
	}

	leader := &countingLeader{}
	team, err := New(TeamConfig{
		ID:            "team-1",
		Name:          "Test Team",
		Model:         leader,
		Members:       []*agent.Agent{memberAgent},
		Instructions:  "Coordinate the team.",
		DB:            db,
		MaxIterations: 5,
	})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	out, err := team.RunWith(ctx, session.RunRequest{RunID: "team-run-1", Input: session.RunInput{Message: "hello team"}, Metadata: map[string]string{"project_id": "project-a"}})
	if err != nil {
		t.Fatalf("team run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("expected status completed, got %s", out.Status)
	}
	if out.Content != "final answer" {
		t.Fatalf("expected content %q, got %q", "final answer", out.Content)
	}
	if out.TeamID != "team-1" {
		t.Fatalf("expected team_id %q, got %q", "team-1", out.TeamID)
	}
	if out.RunID != "team-run-1" {
		t.Fatalf("expected run_id team-run-1, got %q", out.RunID)
	}
	if out.SessionID == "" {
		t.Fatal("expected SessionID to be set")
	}
	if seen := <-memberModel.seen; seen["project_id"] != "project-a" {
		t.Fatalf("member metadata = %+v", seen)
	}
	if out.Metrics.ModelCalls != 2 || out.Metrics.ToolCalls != 1 || len(out.ToolExecutions) != 1 || out.ToolExecutions[0].Status != "success" {
		t.Fatalf("team observability = metrics:%+v tools:%+v", out.Metrics, out.ToolExecutions)
	}

	sess, err := db.ReadTeamSession(ctx, out.SessionID)
	if err != nil {
		t.Fatalf("read team session: %v", err)
	}
	if len(sess.Runs) != 1 {
		t.Fatalf("expected 1 run in session, got %d", len(sess.Runs))
	}
}

func TestTeamSessionStateReadWrite(t *testing.T) {
	ctx := context.Background()
	db := in_memory.New()

	memberModel := mockmodel.New()
	memberModel.ResponseData = &models.ModelResponse{Content: "member output"}
	memberAgent, err := agent.New(agent.AgentConfig{ID: "member1", Name: "Member 1", Model: memberModel})
	if err != nil {
		t.Fatalf("create member agent: %v", err)
	}

	leader := &countingLeader{}
	team, err := New(TeamConfig{
		ID:           "team-2",
		Model:        leader,
		Members:      []*agent.Agent{memberAgent},
		DB:           db,
		SessionState: map[string]any{"key1": "value1"},
	})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	out, err := team.Run(ctx, "hello")
	if err != nil {
		t.Fatalf("team run: %v", err)
	}

	state, err := team.GetSessionState(ctx, out.SessionID)
	if err != nil {
		t.Fatalf("get session state: %v", err)
	}
	if state["key1"] != "value1" {
		t.Fatalf("expected key1=value1, got %v", state["key1"])
	}

	if err := team.UpdateSessionState(ctx, out.SessionID, map[string]any{"key2": "value2"}); err != nil {
		t.Fatalf("update session state: %v", err)
	}

	sess, err := db.ReadTeamSession(ctx, out.SessionID)
	if err != nil {
		t.Fatalf("read team session: %v", err)
	}
	if sess.SessionState["key1"] != "value1" || sess.SessionState["key2"] != "value2" {
		t.Fatalf("expected merged state, got %v", sess.SessionState)
	}
}

func TestTeamCoordinateStream(t *testing.T) {
	ctx := context.Background()

	memberModel := mockmodel.New()
	memberModel.StreamEvents = []models.StreamEvent{
		{Event: "content_delta", Content: "member stream output"},
	}
	memberAgent, err := agent.New(agent.AgentConfig{ID: "member1", Name: "Member 1", Model: memberModel})
	if err != nil {
		t.Fatalf("create member agent: %v", err)
	}

	leader := &countingLeader{}
	team, err := New(TeamConfig{
		ID:            "team-3",
		Model:         leader,
		Members:       []*agent.Agent{memberAgent},
		MaxIterations: 5,
	})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	ch, err := team.RunStream(ctx, "hello stream")
	if err != nil {
		t.Fatalf("team run stream: %v", err)
	}

	var sawStarted, sawCompleted bool
	var finalContent string
	for ev := range ch {
		switch ev.Type {
		case agent.EventTypeRunStarted:
			sawStarted = true
		case agent.EventTypeRunCompleted:
			sawCompleted = true
			if ev.RunOutput != nil {
				finalContent = ev.RunOutput.Content
			}
		}
	}

	if !sawStarted {
		t.Fatal("expected run_started event")
	}
	if !sawCompleted {
		t.Fatal("expected run_completed event")
	}
	if finalContent != "final answer" {
		t.Fatalf("expected final content %q, got %q", "final answer", finalContent)
	}
}

func TestTeamCoordinateStreamRecordsFailedMemberRun(t *testing.T) {
	memberModel := mockmodel.New()
	memberModel.StreamEvents = []models.StreamEvent{{Event: "error", Err: errors.New("member failed")}}
	memberAgent, err := agent.New(agent.AgentConfig{ID: "member1", Model: memberModel})
	if err != nil {
		t.Fatalf("create member agent: %v", err)
	}
	team, err := New(TeamConfig{ID: "team-stream-failure", Model: &countingLeader{}, Members: []*agent.Agent{memberAgent}, MaxIterations: 5})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	ch, err := team.RunStream(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	var final *session.RunOutput
	for ev := range ch {
		if ev.Type == agent.EventTypeRunCompleted && ev.RunOutput != nil && ev.RunOutput.TeamID != "" {
			final = ev.RunOutput
		}
	}
	if final == nil || len(final.ToolExecutions) != 1 || final.ToolExecutions[0].Status != session.ToolExecutionStatusFailed {
		t.Fatalf("final tool executions = %+v", final)
	}
}

func TestTeamStreamCancellationReleasesMemberConcurrency(t *testing.T) {
	memberAgent, err := agent.New(agent.AgentConfig{ID: "member1", Model: &cancellableStreamModel{}, MaxConcurrentRuns: 1})
	if err != nil {
		t.Fatalf("create member agent: %v", err)
	}
	team, err := New(TeamConfig{ID: "team-cancel", Model: &countingLeader{}, Members: []*agent.Agent{memberAgent}, MaxIterations: 5})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := team.RunStream(ctx, "delegate")
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	started := 0
	deadline := time.After(time.Second)
	for started < 2 {
		select {
		case ev := <-ch:
			if ev.Type == agent.EventTypeRunStarted {
				started++
			}
		case <-deadline:
			t.Fatal("member run did not start")
		}
	}
	cancel()

	retryDeadline := time.Now().Add(time.Second)
	for {
		_, runErr := memberAgent.Run(context.Background(), "after cancel")
		if !errors.Is(runErr, agent.ErrBusy) {
			if runErr != nil {
				t.Fatalf("member run after cancellation: %v", runErr)
			}
			break
		}
		if time.Now().After(retryDeadline) {
			t.Fatal("member concurrency slot was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
