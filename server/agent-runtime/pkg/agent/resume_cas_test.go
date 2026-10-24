package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Agent resume intentionally remains a legacy paused-to-running CAS. This
// contrast test uses independent Agent instances so Workflow managed takeover
// work cannot accidentally weaken the existing single-entry guarantee.
func TestAgent_Resume_IndependentInstancesPausedCASHasOneExecutor(t *testing.T) {
	tool := &blockingApprovalTool{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(tool.release) }) })
	store := in_memory.New()
	model := &queueModel{responses: []*models.ModelResponse{{
		ToolCalls: []models.ToolCall{{
			ID: "blocking-call", Type: "function",
			Function: models.FunctionCall{Name: tool.Name(), Arguments: "{}"},
		}},
	}}}
	paused, err := newApprovalAgent(t, tool, model, store).Run(context.Background(), "concurrent")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	request := session.ResumeRequest{
		RunID: paused.RunID, RequirementID: paused.ActiveRequirement().ID,
		Action: string(requirement.ActionApprove),
	}
	firstRuntime := newResumeAgent(t, tool, &queueModel{}, store)
	secondRuntime := newResumeAgent(t, tool, &queueModel{}, store)
	firstDone := make(chan error, 1)
	go func() {
		_, resumeErr := firstRuntime.Resume(context.Background(), request)
		firstDone <- resumeErr
	}()
	select {
	case <-tool.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first Agent instance did not enter the tool")
	}
	if _, err := secondRuntime.Resume(context.Background(), request); !errors.Is(err, ErrResumeConflict) {
		t.Fatalf("second Agent instance Resume error = %v, want ErrResumeConflict", err)
	}
	releaseOnce.Do(func() { close(tool.release) })
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Agent instance Resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Agent instance Resume did not complete")
	}
	if got := atomic.LoadInt32(&tool.calls); got != 1 {
		t.Fatalf("tool calls = %d, want exactly 1", got)
	}
}
