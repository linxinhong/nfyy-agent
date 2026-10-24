package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/vectordb"
)

type mockMemory struct{}

func (m *mockMemory) CreateMemory(_ context.Context, input, userID, agentID string) (*memory.UserMemory, error) {
	return &memory.UserMemory{Memory: input, Input: input, UserID: userID, AgentID: agentID}, nil
}

func (m *mockMemory) GetMemories(_ context.Context, _, _ string, _ int) ([]*memory.UserMemory, error) {
	return nil, nil
}

type mockKnowledge struct{}

func (k *mockKnowledge) Search(_ context.Context, query string, limit int) ([]vectordb.Document, error) {
	return []vectordb.Document{{ID: "1", Content: query}}, nil
}

func (k *mockKnowledge) GetContent(_ context.Context, docID string) (string, error) {
	return "", nil
}

func TestContextHelpers(t *testing.T) {
	rc := &RunContext{AgentID: "a1", UserID: "u1"}
	ctx := ContextWithRunContext(context.Background(), rc)
	got, ok := RunContextFromContext(ctx)
	if !ok {
		t.Fatal("expected run context")
	}
	if got.AgentID != "a1" || got.UserID != "u1" {
		t.Fatalf("unexpected context: %+v", got)
	}
}

func TestCalculator(t *testing.T) {
	res, err := Calculator(context.Background(), "(1 + 2) * 3")
	if err != nil {
		t.Fatalf("Calculator failed: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out["result"] != float64(9) {
		t.Fatalf("expected 9, got %v", out["result"])
	}
}

func TestCalculatorDivisionByZero(t *testing.T) {
	_, err := Calculator(context.Background(), "1 / 0")
	if err == nil {
		t.Fatal("expected division-by-zero error")
	}
}

func TestChatHistory(t *testing.T) {
	s := &session.AgentSession{
		Runs: []*session.RunOutput{
			{
				Messages: []models.Message{
					{Role: models.RoleUser, Content: "hi"},
					{Role: models.RoleAssistant, Content: "hello"},
				},
			},
		},
	}
	rc := &RunContext{Session: s}
	ctx := ContextWithRunContext(context.Background(), rc)
	res, err := ChatHistory(ctx)
	if err != nil {
		t.Fatalf("ChatHistory failed: %v", err)
	}
	var msgs []models.Message
	if err := json.Unmarshal([]byte(res), &msgs); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func TestKnowledgeSearch(t *testing.T) {
	rc := &RunContext{Knowledge: &mockKnowledge{}}
	ctx := ContextWithRunContext(context.Background(), rc)
	res, err := KnowledgeSearch(ctx, "hello", 3)
	if err != nil {
		t.Fatalf("KnowledgeSearch failed: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out["query"] != "hello" {
		t.Fatalf("expected query hello, got %v", out["query"])
	}
}

func TestMemoryUpdate(t *testing.T) {
	rc := &RunContext{Memory: &mockMemory{}, AgentID: "a1", UserID: "u1"}
	ctx := ContextWithRunContext(context.Background(), rc)
	res, err := MemoryUpdate(ctx, "I like Go")
	if err != nil {
		t.Fatalf("MemoryUpdate failed: %v", err)
	}
	var out memory.UserMemory
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out.Input != "I like Go" {
		t.Fatalf("unexpected input: %v", out.Input)
	}
}
