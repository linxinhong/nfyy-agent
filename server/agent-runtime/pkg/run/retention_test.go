package run

import (
	"context"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestDeleteIdleSessionsKeepsActiveAndInFlightSessions(t *testing.T) {
	store := in_memory.New()
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	for _, s := range []*session.AgentSession{
		{SessionID: "idle", AgentID: "a", UserID: "u", CreatedAt: old, UpdatedAt: old},
		{SessionID: "paused", AgentID: "a", UserID: "u", CreatedAt: old, UpdatedAt: old,
			Runs: []*session.RunOutput{{RunID: "p", SessionID: "paused", AgentID: "a", UserID: "u", Status: session.RunStatusPaused}}},
		{SessionID: "recent", AgentID: "a", UserID: "u"},
	} {
		if err := store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	n, err := DeleteIdleSessions(ctx, store, 24*time.Hour, 100)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if s, _ := store.ReadSession(ctx, "idle"); s != nil {
		t.Fatal("idle session survived")
	}
	for _, id := range []string{"paused", "recent"} {
		if s, _ := store.ReadSession(ctx, id); s == nil {
			t.Fatalf("%s session was deleted", id)
		}
	}
}
