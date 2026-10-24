package agent_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

type blockingModel struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockingModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	m.started <- struct{}{}
	select {
	case <-m.release:
		return &models.ModelResponse{Content: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (m *blockingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not implemented")
}
func (m *blockingModel) ID() string       { return "blocking" }
func (m *blockingModel) Provider() string { return "test" }

func TestAgentRejectsRunWhenConcurrencyLimitIsFull(t *testing.T) {
	model := &blockingModel{started: make(chan struct{}, 2), release: make(chan struct{})}
	value, err := agent.New(agent.AgentConfig{Model: model, MaxConcurrentRuns: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, runErr := value.Run(context.Background(), "hold"); done <- runErr }()
		<-model.started
	}
	if out, runErr := value.Run(context.Background(), "busy"); !errors.Is(runErr, agent.ErrBusy) || out != nil {
		t.Fatalf("third run = %+v, %v", out, runErr)
	}
	close(model.release)
	for i := 0; i < 2; i++ {
		if runErr := <-done; runErr != nil {
			t.Fatalf("held run: %v", runErr)
		}
	}
	if _, runErr := value.Run(context.Background(), "after release"); runErr != nil {
		t.Fatalf("run after release: %v", runErr)
	}
}

func TestAgentReleasesConcurrencySlotAfterTimeout(t *testing.T) {
	model := &blockingModel{started: make(chan struct{}, 2), release: make(chan struct{})}
	value, err := agent.New(agent.AgentConfig{Model: model, MaxConcurrentRuns: 1, RunTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, runErr := value.Run(context.Background(), "timeout"); !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("timeout run: %v", runErr)
	}
	close(model.release)
	if _, runErr := value.Run(context.Background(), "after timeout"); runErr != nil {
		t.Fatalf("run after timeout: %v", runErr)
	}
}
