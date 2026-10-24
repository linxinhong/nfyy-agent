package in_memory

import (
	"context"
	"testing"

	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// TestSecondaryIndexDeleteThenRead verifies the index is cleaned up when a
// record is deleted, so a subsequent unscoped read is a not-found instead of
// resolving a stale namespace.
func TestSecondaryIndexDeleteThenRead(t *testing.T) {
	db := New()
	ctx := context.Background()

	if err := db.CreateSession(ctx, &session.AgentSession{
		SessionID: "s1", AgentID: "agent", Namespace: "ns-a", UserID: "u1",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := db.ReadSession(ctx, "s1"); err != nil {
		t.Fatalf("ReadSession before delete: %v", err)
	}
	if err := db.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got, err := db.ReadSession(ctx, "s1"); err != nil || got != nil {
		t.Fatalf("ReadSession after delete = %+v, %v; want nil, nil", got, err)
	}
	if _, ok := db.sessionIndex["s1"]; ok {
		t.Fatalf("sessionIndex still holds deleted id %q", "s1")
	}
}

// TestSecondaryIndexMultiNamespaceAndKindIsolation verifies that (a) the
// index counts every namespace sharing an id and keeps unscoped reads
// fail-closed as ErrConflict, and (b) per-kind indexes do not contaminate
// each other when the same id is used by different record kinds.
func TestSecondaryIndexMultiNamespaceAndKindIsolation(t *testing.T) {
	db := New()
	ctx := context.Background()

	const id = "shared-id"
	for _, ns := range []string{"ns-a", "ns-b"} {
		if err := db.CreateSession(ctx, &session.AgentSession{
			SessionID: id, AgentID: "agent", Namespace: ns, UserID: "u1",
		}); err != nil {
			t.Fatalf("CreateSession %s: %v", ns, err)
		}
	}
	if n := len(db.sessionIndex[id]); n != 2 {
		t.Fatalf("sessionIndex[%q] has %d namespaces, want 2", id, n)
	}
	if _, err := db.ReadSession(ctx, id); err != dbpkg.ErrConflict {
		t.Fatalf("unscoped session read err = %v, want ErrConflict", err)
	}

	// A memory with the same id must live in its own index: it neither adds
	// a session namespace nor is shadowed by the two sessions.
	if err := db.CreateMemory(ctx, &memory.UserMemory{
		MemoryID: id, Memory: "payload", Namespace: "ns-a", UserID: "u1", AgentID: "agent",
	}); err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	if n := len(db.sessionIndex[id]); n != 2 {
		t.Fatalf("sessionIndex[%q] after memory create has %d namespaces, want 2", id, n)
	}
	if n := len(db.memoryIndex[id]); n != 1 {
		t.Fatalf("memoryIndex[%q] has %d namespaces, want 1", id, n)
	}
	m, err := db.ReadMemory(ctx, id)
	if err != nil || m == nil || m.MemoryID != id {
		t.Fatalf("unscoped memory read = %+v, %v", m, err)
	}
}
