package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// --- harness self-tests ---

func TestFakeChatQueueConsumesInOrder(t *testing.T) {
	fake := NewFakeChat(
		&models.ModelResponse{Content: "first"},
		&models.ModelResponse{Content: "second"},
	)
	got1, err := fake.Response(context.Background(), nil, models.ModelOptions{})
	if err != nil || got1.Content != "first" {
		t.Fatalf("first response = %+v, err=%v", got1, err)
	}
	got2, err := fake.Response(context.Background(), nil, models.ModelOptions{})
	if err != nil || got2.Content != "second" {
		t.Fatalf("second response = %+v, err=%v", got2, err)
	}
	// Exhausted queue falls back to an empty response, not an error.
	got3, err := fake.Response(context.Background(), nil, models.ModelOptions{})
	if err != nil || got3.Content != "" {
		t.Fatalf("fallback response = %+v, err=%v", got3, err)
	}
	if fake.Calls() != 3 {
		t.Fatalf("calls = %d, want 3", fake.Calls())
	}
}

func TestServerBootsAndListsAgent(t *testing.T) {
	srv := NewServer(t)
	var info struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	status, err := srv.GetJSON("/agents/"+srv.AgentID(), &info)
	if err != nil {
		t.Fatalf("GET agent: %v", err)
	}
	if status != 200 || info.ID != srv.AgentID() {
		t.Fatalf("status=%d info=%+v", status, info)
	}
}

// --- end-to-end cases ---

// Case 1: synchronous run returns the fake preset content.
func TestE2ESyncRunReturnsPresetContent(t *testing.T) {
	fake := NewFakeChat(&models.ModelResponse{Content: "hello world"})
	srv := NewServer(t, WithFakeChat(fake))

	var out session.RunOutput
	status, err := srv.PostJSON("/agents/"+srv.AgentID()+"/runs", map[string]any{
		"message": "hi",
	}, &out)
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200 (body error: %q)", status, out.Error)
	}
	if out.Content != "hello world" {
		t.Fatalf("content = %q, want hello world", out.Content)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	if fake.Calls() != 1 {
		t.Fatalf("model calls = %d, want 1", fake.Calls())
	}
}

// Case 2: approval-required tool pauses the run; resume completes it and the
// model receives a second round of messages.
func TestE2EApprovalPauseAndResume(t *testing.T) {
	var executed int
	approvalTool := NewFuncTool("approve_me", "needs approval", func(ctx context.Context, args map[string]any) (any, error) {
		executed++
		return "tool-done", nil
	})
	approvalTool = tools.WithApproval(approvalTool, tools.ApprovalRequired)

	fake := NewFakeChat(
		&models.ModelResponse{ToolCalls: []models.ToolCall{{
			ID: "call-1", Type: "function",
			Function: models.FunctionCall{Name: "approve_me", Arguments: "{}"},
		}}},
		&models.ModelResponse{Content: "approved-done"},
	)

	srv := NewServer(t, WithTools([]tools.Tool{approvalTool}), WithFakeChat(fake))

	var out session.RunOutput
	status, err := srv.PostJSON("/agents/"+srv.AgentID()+"/runs", map[string]any{
		"message": "please approve",
	}, &out)
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	if status != 200 {
		t.Fatalf("run status = %d, want 200", status)
	}
	if out.Status != session.RunStatusPaused {
		t.Fatalf("status = %q, want paused", out.Status)
	}
	if out.ActiveRequirementID == "" {
		t.Fatalf("no active requirement: %+v", out.Requirements)
	}
	if executed != 0 {
		t.Fatalf("tool executed before approval: %d", executed)
	}

	// Approve and resume.
	var resumed session.RunOutput
	status, err = srv.PostJSON("/agents/"+srv.AgentID()+"/runs/"+out.RunID+"/resume", map[string]any{
		"requirement_id": out.ActiveRequirementID,
		"action":         "approve",
	}, &resumed)
	if err != nil {
		t.Fatalf("POST resume: %v", err)
	}
	if status != 200 {
		t.Fatalf("resume status = %d, want 200 (error: %q)", status, resumed.Error)
	}
	if resumed.Status != session.RunStatusCompleted {
		t.Fatalf("status = %q, want completed", resumed.Status)
	}
	if resumed.Content != "approved-done" {
		t.Fatalf("content = %q, want approved-done", resumed.Content)
	}
	if executed != 1 {
		t.Fatalf("tool executed = %d, want 1 after approval", executed)
	}
	if fake.Calls() != 2 {
		t.Fatalf("model calls = %d, want 2 (pause then resume)", fake.Calls())
	}
	last := fake.LastMessages()
	found := false
	for _, m := range last {
		if strings.Contains(m.Content, "tool-done") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("second round messages missing tool result: %+v", last)
	}
}

// Case 3: background run completes and its state is queryable.
func TestE2EBackgroundRunCompletes(t *testing.T) {
	fake := NewFakeChat(&models.ModelResponse{Content: "background-done"})
	srv := NewServer(t, WithFakeChat(fake))

	var started session.RunOutput
	status, err := srv.PostJSON("/agents/"+srv.AgentID()+"/runs?background=true", map[string]any{
		"message": "run in background",
	}, &started)
	if err != nil {
		t.Fatalf("POST background run: %v", err)
	}
	if status != 202 {
		t.Fatalf("status = %d, want 202 (error: %q)", status, started.Error)
	}
	if started.RunID == "" {
		t.Fatalf("background run returned no run_id")
	}

	var final session.RunOutput
	WaitFor(t, 5*time.Second, func() bool {
		st, err := srv.GetJSON("/runs/"+started.RunID, &final)
		return err == nil && st == 200 && final.Status == session.RunStatusCompleted
	})
	if final.Content != "background-done" {
		t.Fatalf("content = %q, want background-done", final.Content)
	}
	if final.RunID != started.RunID {
		t.Fatalf("run_id = %q, want %q", final.RunID, started.RunID)
	}
}
