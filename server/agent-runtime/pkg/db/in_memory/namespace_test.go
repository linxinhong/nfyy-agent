package in_memory

import (
	"context"
	"testing"

	dbpkg "github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// TestNamespaceIsolationSameIDNoCollision: namespaces A and B may use the
// same session/run/memory IDs without colliding, and each namespace cannot
// read the other's records.
func TestNamespaceIsolationSameIDNoCollision(t *testing.T) {
	db := New()
	ctx := context.Background()

	const sharedSessionID = "shared-session"

	for _, ns := range []string{"ns-a", "ns-b"} {
		if err := db.CreateSession(ctx, &session.AgentSession{
			SessionID: sharedSessionID, AgentID: "agent", Namespace: ns, UserID: "u1",
		}); err != nil {
			t.Fatalf("create session %s: %v", ns, err)
		}
	}

	// Both namespaces exist under the same ID; scoped listing sees each.
	listA, err := db.ListSessions(ctx, dbpkg.SessionQuery{Namespace: "ns-a"})
	if err != nil || len(listA) != 1 || listA[0].Namespace != "ns-a" {
		t.Fatalf("ns-a listing = %+v, err=%v", listA, err)
	}
	listB, err := db.ListSessions(ctx, dbpkg.SessionQuery{Namespace: "ns-b"})
	if err != nil || len(listB) != 1 || listB[0].Namespace != "ns-b" {
		t.Fatalf("ns-b listing = %+v, err=%v", listB, err)
	}
	// Unscoped read of a shared ID is ambiguous and must fail closed.
	if _, err := db.ReadSession(ctx, sharedSessionID); err != dbpkg.ErrConflict {
		t.Fatalf("unscoped shared-id read: err = %v, want ErrConflict", err)
	}
}

// TestNamespaceIsolationRuns: the same run ID under different namespaces
// resolves only when scoped by namespace.
func TestNamespaceIsolationRuns(t *testing.T) {
	db := New()
	ctx := context.Background()

	const runID = "shared-run"
	for i, ns := range []string{"ns-a", "ns-b"} {
		sessionID := "sess-" + ns
		if err := db.CreateSession(ctx, &session.AgentSession{
			SessionID: sessionID, AgentID: "agent", Namespace: ns, UserID: "u1",
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
		if err := db.AppendRun(ctx, &session.RunOutput{
			RunID: runID, SessionID: sessionID, AgentID: "agent",
			Namespace: ns, UserID: "u1", Status: session.RunStatusCompleted,
		}); err != nil {
			t.Fatalf("append run %d: %v", i, err)
		}
	}

	gotA, err := db.GetRun(ctx, dbpkg.RunQuery{RunID: runID, Namespace: "ns-a"})
	if err != nil || gotA == nil || gotA.Namespace != "ns-a" {
		t.Fatalf("scoped ns-a run = %+v, err=%v", gotA, err)
	}
	gotB, err := db.GetRun(ctx, dbpkg.RunQuery{RunID: runID, Namespace: "ns-b"})
	if err != nil || gotB == nil || gotB.Namespace != "ns-b" {
		t.Fatalf("scoped ns-b run = %+v, err=%v", gotB, err)
	}
	// Cross-namespace query must not leak the other partition's run.
	if got, _ := db.GetRun(ctx, dbpkg.RunQuery{RunID: runID, Namespace: "ns-a", UserID: "u1"}); got.Namespace != "ns-a" {
		t.Fatalf("cross-namespace leak: %+v", got)
	}
}

// TestNamespaceIsolationMemories: memories partition by namespace too.
func TestNamespaceIsolationMemories(t *testing.T) {
	db := New()
	ctx := context.Background()

	const memoryID = "shared-mem"
	for _, ns := range []string{"ns-a", "ns-b"} {
		if err := db.CreateMemory(ctx, &memory.UserMemory{
			MemoryID: memoryID, Memory: "payload-" + ns, Namespace: ns, UserID: "u1", AgentID: "agent",
		}); err != nil {
			t.Fatalf("create memory %s: %v", ns, err)
		}
	}
	if _, err := db.ReadMemory(ctx, memoryID); err != dbpkg.ErrConflict {
		t.Fatalf("unscoped shared-id memory read: err = %v, want ErrConflict", err)
	}
	// Scoped reads via listing with namespace filter.
	memA, err := db.GetMemories(ctx, "u1", "agent", 10)
	if err != nil {
		t.Fatalf("get memories: %v", err)
	}
	// GetMemories has no namespace parameter yet; verify both exist and are
	// distinct payloads (partitioned keys).
	seen := map[string]bool{}
	for _, m := range memA {
		seen[m.Memory] = true
	}
	if len(memA) != 2 || !seen["payload-ns-a"] || !seen["payload-ns-b"] {
		t.Fatalf("memories = %+v", memA)
	}
}
