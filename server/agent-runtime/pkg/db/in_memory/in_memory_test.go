package in_memory

import (
	"context"
	"testing"
	"time"

	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/dbtest"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestStoreContract(t *testing.T) {
	store := New()
	dbtest.RunStoreContract(t, store)
	dbtest.WorkflowResumeContract(t, store)
}

func TestDB_PingAndClose(t *testing.T) {
	db := New()
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestDB_SessionCRUD(t *testing.T) {
	db := New()
	ctx := context.Background()

	now := time.Now().UTC().Round(time.Second)
	s := &session.AgentSession{
		SessionID: "sess-1",
		AgentID:   "agent-1",
		UserID:    "user-1",
		Metadata:  map[string]any{"key": "value", "number": float64(42)},
		Summary:   "test summary",
		Runs: []*session.RunOutput{
			{
				RunID:     "run-1",
				AgentID:   "agent-1",
				UserID:    "user-1",
				Content:   "hello",
				Status:    session.RunStatusCompleted,
				CreatedAt: now,
				UpdatedAt: now,
				Input:     session.RunInput{Message: "hi"},
				Messages: []models.Message{
					{Role: models.RoleUser, Content: "hi"},
					{Role: models.RoleAssistant, Content: "hello"},
				},
				ToolCalls: []models.ToolCall{
					{ID: "call-1", Function: models.FunctionCall{Name: "fn", Arguments: `{}`}},
				},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := db.CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	read, err := db.ReadSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ReadSession failed: %v", err)
	}
	if read == nil {
		t.Fatal("ReadSession returned nil")
	}
	if read.SessionID != "sess-1" || read.AgentID != "agent-1" {
		t.Fatalf("unexpected session: %+v", read)
	}
	if len(read.Runs) != 1 || len(read.Runs[0].Messages) != 2 {
		t.Fatalf("unexpected runs/messages: %+v", read.Runs)
	}
	if len(read.Runs[0].ToolCalls) != 1 {
		t.Fatalf("unexpected tool_calls: %+v", read.Runs[0].ToolCalls)
	}
	if read.Metadata["key"] != "value" {
		t.Fatalf("unexpected metadata: %+v", read.Metadata)
	}

	// Update.
	s.Summary = "updated summary"
	s.Runs[0].Messages = append(s.Runs[0].Messages, models.Message{Role: models.RoleUser, Content: "bye"})
	if err := db.UpdateSession(ctx, s); err != nil {
		t.Fatalf("UpdateSession failed: %v", err)
	}

	updated, err := db.ReadSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ReadSession after update failed: %v", err)
	}
	if updated.Summary != "updated summary" {
		t.Fatalf("summary not updated: %s", updated.Summary)
	}
	if len(updated.Runs[0].Messages) != 2 {
		t.Fatalf("immutable run messages changed: %d", len(updated.Runs[0].Messages))
	}

	// List.
	list, err := db.ListSessions(ctx, dbpkg.SessionQuery{ComponentID: "agent-1", Limit: 10})
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 session, got %d", len(list))
	}

	// Delete.
	if err := db.DeleteSession(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	deleted, err := db.ReadSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ReadSession after delete failed: %v", err)
	}
	if deleted != nil {
		t.Fatalf("expected nil after delete, got %+v", deleted)
	}
}

func TestDB_ListSessionsPagination(t *testing.T) {
	db := New()
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		s := &session.AgentSession{
			SessionID: "",
			AgentID:   "agent-paginate",
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Hour),
		}
		if err := db.CreateSession(ctx, s); err != nil {
			t.Fatalf("CreateSession failed: %v", err)
		}
	}

	list, err := db.ListSessions(ctx, dbpkg.SessionQuery{ComponentID: "agent-paginate", Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(list))
	}
}

func TestDB_ListSessionsScopesBeforePagination(t *testing.T) {
	store := New()
	ctx := context.Background()
	base := time.Now().UTC()
	for _, item := range []struct {
		id, user string
		updated  time.Time
	}{
		{"alice-old", "alice", base},
		{"alice-new", "alice", base.Add(2 * time.Minute)},
		{"bob-newest", "bob", base.Add(3 * time.Minute)},
	} {
		if err := store.CreateSession(ctx, &session.AgentSession{
			SessionID: item.id, AgentID: "a1", UserID: item.user, CreatedAt: item.updated, UpdatedAt: item.updated,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	items, err := store.ListSessions(ctx, dbpkg.SessionQuery{ComponentID: "a1", UserID: "alice", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(items) != 1 || items[0].SessionID != "alice-old" {
		t.Fatalf("scoped page = %+v, want alice-old", items)
	}
}

func TestDB_GetRunScopesByUser(t *testing.T) {
	store := New()
	now := time.Now().UTC()
	if err := store.CreateSession(context.Background(), &session.AgentSession{
		SessionID: "alice-session", AgentID: "a1", UserID: "alice", CreatedAt: now, UpdatedAt: now,
		Runs: []*session.RunOutput{{RunID: "run-alice", AgentID: "a1", UserID: "alice", Status: session.RunStatusCompleted, CreatedAt: now, UpdatedAt: now}},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	run, err := store.GetRun(context.Background(), dbpkg.RunQuery{RunID: "run-alice", UserID: "alice"})
	if err != nil || run == nil || run.RunID != "run-alice" {
		t.Fatalf("GetRun as owner = %+v, %v", run, err)
	}
	run, err = store.GetRun(context.Background(), dbpkg.RunQuery{RunID: "run-alice", UserID: "bob"})
	if err != nil || run != nil {
		t.Fatalf("GetRun as other user = %+v, %v; want nil", run, err)
	}
}

func TestDB_AppendRunRejectsDifferentSessionUser(t *testing.T) {
	store := New()
	now := time.Now().UTC()
	if err := store.CreateSession(context.Background(), &session.AgentSession{
		SessionID: "alice-session", AgentID: "a1", UserID: "alice", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.AppendRun(context.Background(), &session.RunOutput{
		RunID: "bob-run", SessionID: "alice-session", AgentID: "a1", UserID: "bob",
		Status: session.RunStatusCompleted, CreatedAt: now, UpdatedAt: now,
	}); err == nil {
		t.Fatal("AppendRun accepted a run for a different user")
	}
}

func TestDB_MemoryCRUD(t *testing.T) {
	db := New()
	ctx := context.Background()

	m := &memory.UserMemory{
		MemoryID:  "mem-1",
		Memory:    "I love Go",
		Topics:    []string{"golang", "programming"},
		UserID:    "user-1",
		AgentID:   "agent-1",
		Input:     "Tell me about Go",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	if err := db.CreateMemory(ctx, m); err != nil {
		t.Fatalf("CreateMemory failed: %v", err)
	}

	memories, err := db.GetMemories(ctx, "user-1", "agent-1", 10)
	if err != nil {
		t.Fatalf("GetMemories failed: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(memories))
	}
	if memories[0].Memory != "I love Go" || memories[0].MemoryID != "mem-1" {
		t.Fatalf("unexpected memory: %+v", memories[0])
	}
	if len(memories[0].Topics) != 2 {
		t.Fatalf("unexpected topics: %+v", memories[0].Topics)
	}

	if err := db.DeleteMemory(ctx, "mem-1"); err != nil {
		t.Fatalf("DeleteMemory failed: %v", err)
	}

	memories, err = db.GetMemories(ctx, "user-1", "agent-1", 10)
	if err != nil {
		t.Fatalf("GetMemories after delete failed: %v", err)
	}
	if len(memories) != 0 {
		t.Fatalf("expected 0 memories after delete, got %d", len(memories))
	}
}

func TestDB_GetMemoriesLimitAndFilter(t *testing.T) {
	db := New()
	ctx := context.Background()

	for i := 1; i <= 4; i++ {
		m := &memory.UserMemory{
			MemoryID:  "",
			Memory:    "memory",
			UserID:    "user-limit",
			AgentID:   "agent-limit",
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Minute),
		}
		if err := db.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory failed: %v", err)
		}
	}

	memories, err := db.GetMemories(ctx, "user-limit", "agent-limit", 2)
	if err != nil {
		t.Fatalf("GetMemories failed: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(memories))
	}

	// Most recent first.
	if memories[0].CreatedAt.Before(memories[1].CreatedAt) {
		t.Fatal("memories are not sorted by created_at desc")
	}

	other, err := db.GetMemories(ctx, "other-user", "agent-limit", 10)
	if err != nil {
		t.Fatalf("GetMemories failed: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("expected 0 memories for other user, got %d", len(other))
	}
}
