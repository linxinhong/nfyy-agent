package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/stream/journaltest"
)

func TestReadSessionRecentAndRunLeases(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	sid := uuid.NewString()
	if err := store.CreateSession(ctx, &session.AgentSession{SessionID: sid, AgentID: "agent", UserID: "alice"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	old := time.Now().UTC().Add(-time.Hour)
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%d", sid, i)
		run := &session.RunOutput{RunID: ids[i], SessionID: sid, AgentID: "agent", UserID: "alice",
			Status: session.RunStatusRunning, CreatedAt: old.Add(time.Duration(i) * time.Second), UpdatedAt: old}
		if i == 0 {
			run.LeaseTimeout = time.Minute // stale
		}
		if i == 1 {
			run.LeaseTimeout = 2 * time.Hour // fresh enough
		}
		if err := store.AppendRun(ctx, run); err != nil {
			t.Fatalf("AppendRun: %v", err)
		}
	}
	recent, err := store.ReadSessionRecent(ctx, sid, 2)
	if err != nil || len(recent.Runs) != 2 || recent.Runs[0].RunID != ids[2] || recent.Runs[1].RunID != ids[3] {
		t.Fatalf("ReadSessionRecent = %+v, %v", recent, err)
	}
	expired, err := store.ExpireStaleRuns(ctx, time.Now(), "lease expired", 1000)
	if err != nil {
		t.Fatalf("ExpireStaleRuns: %v", err)
	}
	var sawStale bool
	for _, id := range expired {
		if id == ids[1] || id == ids[2] || id == ids[3] {
			t.Fatalf("expired a fresh or unleased run %s", id)
		}
		sawStale = sawStale || id == ids[0]
	}
	if !sawStale {
		t.Fatalf("stale run not expired: %v", expired)
	}
	stale, _ := store.GetRun(ctx, db.RunQuery{RunID: ids[0]})
	if stale.Status != session.RunStatusError || stale.Error != "lease expired" {
		t.Fatalf("stale run = %s %q", stale.Status, stale.Error)
	}
	if err := store.TouchRun(ctx, db.RunQuery{RunID: ids[0]}); !errors.Is(err, db.ErrConflict) {
		t.Fatalf("TouchRun(expired) = %v, want ErrConflict", err)
	}
	if err := store.TouchRun(ctx, db.RunQuery{RunID: ids[1]}); err != nil {
		t.Fatalf("TouchRun(running) = %v", err)
	}
	if err := store.TouchRun(ctx, db.RunQuery{RunID: "missing-" + sid}); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("TouchRun(missing) = %v, want ErrNotFound", err)
	}
}

func TestDeleteIdleSessionsPostgres(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour)
	idle, busy := uuid.NewString(), uuid.NewString()
	for _, id := range []string{idle, busy} {
		if err := store.CreateSession(ctx, &session.AgentSession{SessionID: id, AgentID: "agent", UserID: "alice", CreatedAt: old, UpdatedAt: old}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	if err := store.AppendRun(ctx, &session.RunOutput{RunID: busy + "-run", SessionID: busy, AgentID: "agent", UserID: "alice",
		Status: session.RunStatusPaused, CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE agent_runtime_sessions SET updated_at=$1 WHERE session_id IN ($2,$3)`, old, idle, busy); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteIdleSessions(ctx, time.Now().Add(-24*time.Hour), 10000); err != nil {
		t.Fatalf("DeleteIdleSessions: %v", err)
	}
	if s, _ := store.ReadSession(ctx, idle); s != nil {
		t.Fatal("idle session survived")
	}
	if s, _ := store.ReadSession(ctx, busy); s == nil {
		t.Fatal("session with a paused run was deleted")
	}
}

func TestClusterAdmissionSharesSlotsAcrossControllers(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	tenant := "t-" + uuid.NewString()
	cluster := store.ClusterAdmission(ClusterAdmissionConfig{PerTenant: 1, LeaseTTL: 2 * time.Second})
	release, ok, err := cluster.TryAcquire(ctx, tenant)
	if err != nil || !ok {
		t.Fatalf("first acquire = %v, %v", ok, err)
	}
	other := store.ClusterAdmission(ClusterAdmissionConfig{PerTenant: 1}) // another replica
	if _, ok, err := other.TryAcquire(ctx, tenant); err != nil || ok {
		t.Fatalf("second acquire = %v, %v, want refused", ok, err)
	}
	release()
	release2, ok, err := other.TryAcquire(ctx, tenant)
	if err != nil || !ok {
		t.Fatalf("acquire after release = %v, %v", ok, err)
	}
	release2()
}

// TestReaperWithFastClockSparesLiveRuns: a replica whose clock runs an
// hour ahead must not expire a run that is heartbeating.
func TestReaperWithFastClockSparesLiveRuns(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	sid := uuid.NewString()
	if err := store.CreateSession(ctx, &session.AgentSession{SessionID: sid, AgentID: "agent", UserID: "alice"}); err != nil {
		t.Fatal(err)
	}
	runID := sid + "-live"
	now := time.Now().UTC()
	if err := store.AppendRun(ctx, &session.RunOutput{RunID: runID, SessionID: sid, AgentID: "agent", UserID: "alice",
		Status: session.RunStatusRunning, LeaseTimeout: time.Minute, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchRun(ctx, db.RunQuery{RunID: runID}); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpireStaleRuns(ctx, time.Now().Add(time.Hour), "lease expired", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range expired {
		if id == runID {
			t.Fatal("a reaper with a fast clock expired a live run")
		}
	}
	run, err := store.GetRun(ctx, db.RunQuery{RunID: runID})
	if err != nil || run.Status != session.RunStatusRunning || time.Since(run.UpdatedAt).Abs() > time.Minute {
		t.Fatalf("run = %+v, %v", run, err)
	}
}

// TestClusterAdmissionReclaimsSweptSlot covers a holder whose renewals
// failed until another claim swept its expired slot: the next renewal
// restores the slot, so the cluster does not admit past the limit.
func TestClusterAdmissionReclaimsSweptSlot(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	tenant := "t-" + uuid.NewString()
	cluster := store.ClusterAdmission(ClusterAdmissionConfig{PerTenant: 1, LeaseTTL: 150 * time.Millisecond})
	release, ok, err := cluster.TryAcquire(ctx, tenant)
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	defer release()
	if _, err := store.db.ExecContext(ctx, `DELETE FROM agent_runtime_admission_slots WHERE tenant = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // > one renewal interval
	other := store.ClusterAdmission(ClusterAdmissionConfig{PerTenant: 1})
	if _, ok, err := other.TryAcquire(ctx, tenant); err != nil || ok {
		t.Fatalf("acquire beside a live run = %v, %v, want refused", ok, err)
	}
	release()
	var n int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_runtime_admission_slots WHERE tenant = $1`, tenant).Scan(&n); err != nil || n != 0 {
		t.Fatalf("slots after release = %d, %v", n, err)
	}
}

func TestStreamJournalContractPostgres(t *testing.T) {
	journaltest.Run(t, testDB(t).StreamJournal(), uuid.NewString())
}
