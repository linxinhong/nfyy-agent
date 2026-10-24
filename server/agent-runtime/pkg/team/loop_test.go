package team

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	mockmodel "github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// reasoningLeader delegates once with reasoning (as DeepSeek thinking mode
// does) and records what it receives afterwards.
type reasoningLeader struct {
	mu       sync.Mutex
	first    *models.ModelResponse
	received [][]models.Message
}

func (m *reasoningLeader) Response(_ context.Context, messages []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.received = append(m.received, append([]models.Message(nil), messages...))
	if len(m.received) == 1 {
		return m.first, nil
	}
	return &models.ModelResponse{Content: "final", FinishReason: models.FinishReasonStop}, nil
}
func (m *reasoningLeader) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	resp, err := m.Response(ctx, messages, opts)
	if err != nil {
		return nil, err
	}
	ch := make(chan models.StreamEvent, 8)
	if resp.ReasoningContent != "" {
		ch <- models.StreamEvent{Event: "reasoning_delta", ReasoningContent: resp.ReasoningContent}
	}
	for i := range resp.ToolCalls {
		tc := resp.ToolCalls[i]
		tc.Index = i
		ch <- models.StreamEvent{Event: "tool_call_delta", ToolCall: &tc}
	}
	if resp.Content != "" {
		ch <- models.StreamEvent{Event: "content_delta", Content: resp.Content}
	}
	ch <- models.StreamEvent{Event: "finish", FinishReason: resp.FinishReason}
	close(ch)
	return ch, nil
}
func (*reasoningLeader) ID() string       { return "reasoning-leader" }
func (*reasoningLeader) Provider() string { return "mock" }

func delegateCall() models.ToolCall {
	return models.ToolCall{ID: "tc1", Type: "function", Function: models.FunctionCall{
		Name: "delegate_to_member", Arguments: `{"member_id":"member1","task":"t"}`,
	}}
}

func newLoopTeam(t *testing.T, leader models.Model) *Team {
	t.Helper()
	member, err := agent.New(agent.AgentConfig{ID: "member1", Model: mockmodel.New()})
	if err != nil {
		t.Fatal(err)
	}
	team, err := New(TeamConfig{Model: leader, Members: []*agent.Agent{member}})
	if err != nil {
		t.Fatal(err)
	}
	return team
}

func TestTeamReplaysCoordinatorReasoningWithinToolLoop(t *testing.T) {
	for _, stream := range []bool{false, true} {
		leader := &reasoningLeader{first: &models.ModelResponse{
			ReasoningContent: "who should do this?", ToolCalls: []models.ToolCall{delegateCall()},
			FinishReason: models.FinishReasonToolCalls,
		}}
		team := newLoopTeam(t, leader)
		if stream {
			events, err := team.RunStream(context.Background(), "q")
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
		} else if _, err := team.Run(context.Background(), "q"); err != nil {
			t.Fatal(err)
		}
		if len(leader.received) != 2 {
			t.Fatalf("stream=%v leader calls = %d", stream, len(leader.received))
		}
		var replayed bool
		for _, msg := range leader.received[1] {
			replayed = replayed || (len(msg.ToolCalls) > 0 && msg.ReasoningContent == "who should do this?")
		}
		if !replayed {
			t.Fatalf("stream=%v: coordinator reasoning not replayed: %+v", stream, leader.received[1])
		}
	}
}

func TestTeamNeverDelegatesFromTruncatedTurn(t *testing.T) {
	for _, stream := range []bool{false, true} {
		leader := &reasoningLeader{first: &models.ModelResponse{
			ToolCalls: []models.ToolCall{delegateCall()}, FinishReason: models.FinishReasonLength,
		}}
		team := newLoopTeam(t, leader)
		var out *session.RunOutput
		var err error
		if stream {
			events, streamErr := team.RunStream(context.Background(), "q")
			if streamErr != nil {
				t.Fatal(streamErr)
			}
			for ev := range events {
				if ev.Type == agent.EventTypeRunError {
					err = ev.Err
				}
				if ev.Type == agent.EventTypeToolResult {
					t.Fatalf("stream: delegated from a truncated turn")
				}
			}
		} else {
			out, err = team.Run(context.Background(), "q")
			if out.Metrics.ToolCalls != 0 {
				t.Fatalf("delegated from a truncated turn: %+v", out.ToolExecutions)
			}
		}
		if !errors.Is(err, agent.ErrOutputTruncated) {
			t.Fatalf("stream=%v err = %v, want ErrOutputTruncated", stream, err)
		}
	}
}

// flakyLeader fails its first call with a retryable error.
type flakyLeader struct {
	reasoningLeader
	failed bool
}

func (m *flakyLeader) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	if !m.failed {
		m.failed = true
		return nil, &models.ProviderError{Provider: "test", StatusCode: 503, Message: "overloaded"}
	}
	return m.reasoningLeader.Response(ctx, messages, opts)
}

func TestTeamCoordinatorRetriesRetryableErrors(t *testing.T) {
	leader := &flakyLeader{reasoningLeader: reasoningLeader{first: &models.ModelResponse{Content: "direct", FinishReason: models.FinishReasonStop}}}
	member, _ := agent.New(agent.AgentConfig{ID: "member1", Model: mockmodel.New()})
	team, err := New(TeamConfig{Model: leader, Members: []*agent.Agent{member},
		RetryConfig: agent.RetryConfig{MaxRetries: 2, RetryDelay: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := team.Run(context.Background(), "q")
	if err != nil || out.Content != "direct" {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
}

func TestTeamStreamForwardsMemberEventsAfterCoordinatorToolStart(t *testing.T) {
	leader := &reasoningLeader{first: &models.ModelResponse{ToolCalls: []models.ToolCall{delegateCall()}, FinishReason: models.FinishReasonToolCalls}}
	memberModel := mockmodel.New()
	memberModel.StreamEvents = []models.StreamEvent{{Event: "content_delta", Content: "member says hi"}}
	member, _ := agent.New(agent.AgentConfig{ID: "member1", Model: memberModel})
	team, err := New(TeamConfig{Model: leader, Members: []*agent.Agent{member}})
	if err != nil {
		t.Fatal(err)
	}
	events, err := team.RunStream(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	var final *session.RunOutput
	for ev := range events {
		switch {
		case ev.Type == agent.EventTypeToolStarted:
			order = append(order, "tool_started")
		case ev.Type == agent.EventTypeMessage && ev.Content == "member says hi":
			order = append(order, "member_message")
		case ev.Type == agent.EventTypeToolResult:
			order = append(order, "tool_result")
		case ev.Type == agent.EventTypeRunCompleted && ev.RunOutput != nil && ev.RunOutput.TeamID != "":
			final = ev.RunOutput
		}
	}
	if strings.Join(order, ",") != "tool_started,member_message,tool_result" {
		t.Fatalf("event order = %v", order)
	}
	if final == nil || final.Content != "final" || len(final.ToolExecutions) != 1 || final.ToolExecutions[0].Status != session.ToolExecutionStatusSuccess {
		t.Fatalf("final = %+v", final)
	}
}

// blockingMember blocks until its context ends.
type blockingMember struct{ started chan struct{} }

func (b *blockingMember) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (b *blockingMember) ResponseStream(ctx context.Context, m []models.Message, o models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not streamed")
}
func (*blockingMember) ID() string       { return "blocking" }
func (*blockingMember) Provider() string { return "test" }

func TestTeamRunCancelledFromAnotherProcess(t *testing.T) {
	store := in_memory.New()
	blocker := &blockingMember{started: make(chan struct{})}
	member, _ := agent.New(agent.AgentConfig{ID: "member1", Model: blocker})
	leader := &reasoningLeader{first: &models.ModelResponse{ToolCalls: []models.ToolCall{delegateCall()}, FinishReason: models.FinishReasonToolCalls}}
	team, err := New(TeamConfig{ID: "team-lease", Model: leader, Members: []*agent.Agent{member}, DB: store, UserID: "u", RunLeaseTimeout: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-blocker.started
		run, err := store.GetRun(context.Background(), db.RunQuery{RunID: "team-run"})
		if err != nil || run == nil {
			t.Errorf("GetRun: %v", err)
			return
		}
		run.Status = session.RunStatusCancelled
		if err := store.UpdateRun(context.Background(), run, session.RunStatusRunning); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := team.RunWith(ctx, session.RunRequest{RunID: "team-run", Input: session.RunInput{Message: "q"}})
	if !errors.Is(err, agent.ErrRunCancelledExternally) || out.Status != session.RunStatusCancelled || out.LeaseTimeout != 40*time.Millisecond {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
}

func TestTeamDelegationTimeoutFailsTheDelegation(t *testing.T) {
	blocker := &blockingMember{started: make(chan struct{})}
	member, _ := agent.New(agent.AgentConfig{ID: "member1", Model: blocker})
	leader := &reasoningLeader{first: &models.ModelResponse{ToolCalls: []models.ToolCall{delegateCall()}, FinishReason: models.FinishReasonToolCalls}}
	team, err := New(TeamConfig{Model: leader, Members: []*agent.Agent{member}, DelegationTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	out, err := team.Run(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ToolExecutions) != 1 || out.ToolExecutions[0].Status == session.ToolExecutionStatusSuccess {
		t.Fatalf("tool executions = %+v", out.ToolExecutions)
	}
}

func TestTeamStreamSurvivesRejectedDelegationArguments(t *testing.T) {
	bad := models.ToolCall{ID: "bad", Type: "function", Function: models.FunctionCall{Name: "delegate_to_member", Arguments: `{not json`}}
	leader := &scriptedLeader{responses: []*models.ModelResponse{
		{ToolCalls: []models.ToolCall{bad}, FinishReason: models.FinishReasonToolCalls},
		{ToolCalls: []models.ToolCall{bad}, FinishReason: models.FinishReasonToolCalls},
		{ToolCalls: []models.ToolCall{delegateCall()}, FinishReason: models.FinishReasonToolCalls},
		{Content: "final", FinishReason: models.FinishReasonStop},
	}}
	member, _ := agent.New(agent.AgentConfig{ID: "member1", Model: mockmodel.New()})
	team, err := New(TeamConfig{Model: leader, Members: []*agent.Agent{member}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := team.RunStream(ctx, "q")
	if err != nil {
		t.Fatal(err)
	}
	var final *session.RunOutput
	for ev := range events {
		if ev.Type == agent.EventTypeRunCompleted && ev.RunOutput != nil && ev.RunOutput.TeamID != "" {
			final = ev.RunOutput
		}
	}
	if final == nil || final.Content != "final" || len(final.ToolExecutions) != 3 || final.ToolExecutions[2].Status != session.ToolExecutionStatusSuccess {
		t.Fatalf("final = %+v", final)
	}
}

// scriptedLeader streams one scripted response per call.
type scriptedLeader struct {
	mu        sync.Mutex
	responses []*models.ModelResponse
}

func (m *scriptedLeader) next() *models.ModelResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	resp := m.responses[0]
	m.responses = m.responses[1:]
	return resp
}
func (m *scriptedLeader) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	return m.next(), nil
}
func (m *scriptedLeader) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	r := &reasoningLeader{first: m.next()}
	return r.ResponseStream(ctx, messages, opts)
}
func (*scriptedLeader) ID() string       { return "scripted-leader" }
func (*scriptedLeader) Provider() string { return "mock" }
