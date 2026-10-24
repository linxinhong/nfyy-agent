package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestReadSessionRecentLoadsNewestRunsOldestFirst(t *testing.T) {
	store := newTestDB(t)
	ctx := context.Background()
	s := &session.AgentSession{SessionID: "s-recent", AgentID: "a", UserID: "u"}
	if err := store.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		run := &session.RunOutput{
			RunID: fmt.Sprintf("r%d", i), SessionID: "s-recent", AgentID: "a", UserID: "u",
			Status:    session.RunStatusCompleted,
			Messages:  []models.Message{{Role: models.RoleUser, Content: fmt.Sprintf("q%d", i)}},
			CreatedAt: base.Add(time.Duration(i) * time.Minute), UpdatedAt: base,
		}
		if err := store.AppendRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ReadSessionRecent(ctx, "s-recent", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 2 || got.Runs[0].RunID != "r3" || got.Runs[1].RunID != "r4" || got.Runs[1].Messages[0].Content != "q4" {
		t.Fatalf("runs = %+v", got.Runs)
	}
	all, err := store.ReadSession(ctx, "s-recent")
	if err != nil || len(all.Runs) != 5 || all.Runs[0].RunID != "r0" {
		t.Fatalf("ReadSession = %+v, %v", all, err)
	}
}

func TestDeleteIdleSessionsSQLite(t *testing.T) {
	store := newTestDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour)
	for _, id := range []string{"idle", "busy"} {
		if err := store.CreateSession(ctx, &session.AgentSession{SessionID: id, AgentID: "a", UserID: "u", CreatedAt: old, UpdatedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendRun(ctx, &session.RunOutput{RunID: "busy-run", SessionID: "busy", AgentID: "a", UserID: "u",
		Status: session.RunStatusRunning, CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	// AppendRun refreshes the session; age it again for the test.
	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET updated_at = ?`, old.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	n, err := store.DeleteIdleSessions(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if s, _ := store.ReadSession(ctx, "idle"); s != nil {
		t.Fatal("idle session survived")
	}
	if s, _ := store.ReadSession(ctx, "busy"); s == nil || len(s.Runs) != 1 {
		t.Fatalf("session with a running run was deleted: %+v", s)
	}
}
