package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

func TestSQLiteMemoryLifecycleV10UpgradeBackfillAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle-upgrade.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	store := NewMemoryStore(db).SetClock(func() time.Time { return now })
	m := sqliteModule(t, store, memory.DefaultLimits())
	retractedScope := sqliteScope()
	retracted := sqliteCreate(t, m, retractedScope, "retracted-body", "")
	retractToken := sqliteToken(t, m, retractedScope, memory.ActionRetract)
	if _, err := m.RetractRecord(context.Background(), sqlitePrincipal(retractedScope), retractedScope, memory.RetractRequest{MemoryID: retracted.MemoryID, ExpectedRevision: retracted.Revision, OperationToken: retractToken}); err != nil {
		t.Fatal(err)
	}
	forgottenScope := retractedScope
	forgottenScope.SubjectID = "forgotten-upgrade"
	sqliteCreate(t, m, forgottenScope, "forgotten-body", "")
	forgetToken, err := m.IssueForgetToken(context.Background(), sqlitePrincipal(forgottenScope), forgottenScope.Subject(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ForgetSubject(context.Background(), sqlitePrincipal(forgottenScope), forgottenScope.Subject(), forgetToken); err != nil {
		t.Fatal(err)
	}
	sourceScope := retractedScope
	sourceScope.SubjectID = "source-upgrade"
	sourceEpochOne := sqliteCreate(t, m, sourceScope, "source-body-1", "")
	sourceEpochThree := sqliteCreate(t, m, sourceScope, "source-body-3", "")
	if _, err := db.db.Exec(`UPDATE agent_runtime_memory_records SET source_ref='legacy-source',source_version='v1',source_epoch=1 WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=?`, sourceScope.Namespace, sourceScope.ProjectID, sourceScope.SubjectID, sourceScope.CollectionID, sourceEpochOne.MemoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE agent_runtime_memory_records SET source_ref='legacy-source',source_version='v3',source_epoch=3 WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=?`, sourceScope.Namespace, sourceScope.ProjectID, sourceScope.SubjectID, sourceScope.CollectionID, sourceEpochThree.MemoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO agent_runtime_memory_sources(namespace,project_id,subject_id,collection_id,source_ref,source_version,digest,source_epoch,expires_at_ns,verifier,revoked) VALUES(?,?,?,?,?,'v4','digest',4,?,'upgrade',1)`, sourceScope.Namespace, sourceScope.ProjectID, sourceScope.SubjectID, sourceScope.CollectionID, "legacy-source", toNS(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DELETE FROM agent_runtime_memory_lifecycle_jobs`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE agent_runtime_memory_lifecycle_jobs`,
		`DROP INDEX IF EXISTS idx_memory_records_expiry_discovery`,
		`DROP INDEX IF EXISTS idx_memory_records_lifecycle_subject`,
		`DROP INDEX IF EXISTS idx_memory_records_lifecycle_source`,
		`DROP INDEX IF EXISTS idx_memory_records_lifecycle_reclaim`,
		`DELETE FROM agent_runtime_schema_migrations WHERE version=11`,
	} {
		if _, err := db.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	store = NewMemoryStore(reopened).SetClock(func() time.Time { return now })
	if discovered, err := store.DiscoverExpired(context.Background(), memory.MaxPurgeBatch); err != nil || discovered != 4 {
		t.Fatalf("upgrade discovery=%d err=%v", discovered, err)
	}
	lag, err := store.PurgeLag(context.Background())
	if err != nil || lag.PendingJobs != 4 {
		t.Fatalf("upgrade lag=%#v err=%v", lag, err)
	}
	var newRecord *memory.Record
	for completed := 0; completed < 4; completed++ {
		claim, err := store.ClaimNextPurge(context.Background(), "upgrade-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if claim.Kind == memory.PurgeJobForget {
			enableLifecycleSubject(t, store, forgottenScope)
			newModule := sqliteModule(t, store, memory.DefaultLimits())
			newRecord = sqliteCreate(t, newModule, forgottenScope, "new-generation", "")
		}
		for {
			batch, err := store.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch)
			if err != nil {
				t.Fatal(err)
			}
			claim = batch.Job
			if batch.Completed {
				break
			}
		}
	}
	if newRecord == nil {
		t.Fatal("forget backfill job not claimed")
	}
	if got, _, err := store.ReadRecord(context.Background(), forgottenScope, newRecord.MemoryID); err != nil || got == nil || got.Content != "new-generation" || got.SubjectGeneration != 3 {
		t.Fatalf("new generation after upgrade=%#v err=%v", got, err)
	}
	var uncleared int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM agent_runtime_memory_records WHERE content<>'' AND memory_id<>?`, newRecord.MemoryID).Scan(&uncleared); err != nil || uncleared != 0 {
		t.Fatalf("uncleared old bodies=%d err=%v", uncleared, err)
	}
	if reclaimed, err := store.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch); err != nil || reclaimed.ReclaimedRecords != 0 || reclaimed.ReclaimedJobs != 0 {
		t.Fatalf("upgrade early reclaim=%#v err=%v", reclaimed, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(memory.LifecycleRetention)
	again, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	store = NewMemoryStore(again).SetClock(func() time.Time { return now })
	reclaimed, err := store.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch)
	if err != nil || reclaimed.ReclaimedRecords != 4 || reclaimed.ReclaimedJobs != 4 || reclaimed.ReleasedControls != 4 {
		t.Fatalf("upgrade durable reclaim=%#v err=%v", reclaimed, err)
	}
	var records, sources int
	if err := again.db.QueryRow(`SELECT (SELECT count(*) FROM agent_runtime_memory_records),(SELECT count(*) FROM agent_runtime_memory_sources)`).Scan(&records, &sources); err != nil || records != 1 || sources != 1 {
		t.Fatalf("upgrade retained controls records=%d sources=%d err=%v", records, sources, err)
	}
}

func TestSQLiteMemoryLifecycleIndependentWorkersAndFaultRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle-workers.db")
	firstDB, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstDB.Close() })
	secondDB, err := NewWithOptions(path, Options{AutoMigrate: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondDB.Close() })
	now := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	a, b := NewMemoryStore(firstDB).SetClock(clock), NewMemoryStore(secondDB).SetClock(clock)
	m := sqliteModule(t, a, memory.DefaultLimits())
	scope := sqliteScope()
	record := sqliteCreate(t, m, scope, "fault-body", "")
	token := sqliteToken(t, m, scope, memory.ActionRetract)
	if _, err := m.RetractRecord(context.Background(), sqlitePrincipal(scope), scope, memory.RetractRequest{MemoryID: record.MemoryID, ExpectedRevision: record.Revision, OperationToken: token}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type claimResult struct {
		claim memory.PurgeJob
		err   error
	}
	claims := make(chan claimResult, 2)
	for i, store := range []*MemoryStore{a, b} {
		go func(index int, store *MemoryStore) {
			<-start
			claim, err := store.ClaimNextPurge(context.Background(), fmt.Sprintf("worker-%d", index), time.Minute)
			claims <- claimResult{claim: claim, err: err}
		}(i, store)
	}
	close(start)
	var claim memory.PurgeJob
	winners := 0
	for i := 0; i < 2; i++ {
		candidate := <-claims
		if candidate.err == nil {
			claim = candidate.claim
			winners++
		} else if !errors.Is(candidate.err, memory.ErrNotFound) && !errors.Is(candidate.err, memory.ErrOverloaded) {
			t.Fatalf("claim error=%v", candidate.err)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners=%d", winners)
	}
	a.SetCommitFault(func(stage CommitStage) error {
		if stage == CommitBefore {
			return errors.New("injected before commit")
		}
		return nil
	})
	if _, err := a.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch); err == nil {
		t.Fatal("expected injected purge failure")
	}
	a.SetCommitFault(nil)
	batch, err := b.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch)
	if err != nil || batch.PurgedRecords != 1 || !batch.Completed {
		t.Fatalf("retry after rollback=%#v err=%v", batch, err)
	}
	clockMu.Lock()
	now = now.Add(memory.LifecycleRetention)
	clockMu.Unlock()
	a.SetCommitFault(func(stage CommitStage) error {
		if stage == CommitBefore {
			return errors.New("injected reclaim failure")
		}
		return nil
	})
	if _, err := a.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch); err == nil {
		t.Fatal("expected injected reclaim failure")
	}
	var records, identities, jobs int
	if err := firstDB.db.QueryRow(`SELECT (SELECT count(*) FROM agent_runtime_memory_records WHERE memory_id=?),(SELECT record_identities FROM agent_runtime_memory_collections WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?),(SELECT count(*) FROM agent_runtime_memory_lifecycle_jobs)`, record.MemoryID, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID).Scan(&records, &identities, &jobs); err != nil || records != 1 || identities != 1 || jobs != 1 {
		t.Fatalf("reclaim rollback records=%d identities=%d jobs=%d err=%v", records, identities, jobs, err)
	}
	a.SetCommitFault(nil)
	reclaimed, err := b.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch)
	if err != nil || reclaimed.ReclaimedRecords != 1 || reclaimed.ReclaimedJobs != 1 || reclaimed.ReleasedControls != 1 {
		t.Fatalf("reclaim retry=%#v err=%v", reclaimed, err)
	}
}

func TestSQLiteMemoryLifecycleQueryPlansAreIndexed(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "lifecycle-plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assertPlan := func(name, query, index string, args ...any) {
		t.Helper()
		rows, err := db.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatalf("%s explain: %v", name, err)
		}
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		plan := strings.Join(details, "\n")
		if !strings.Contains(plan, index) {
			t.Fatalf("%s plan did not use %s:\n%s", name, index, plan)
		}
	}
	assertPlan("source history", `SELECT DISTINCT r.namespace,r.project_id,r.subject_id,r.collection_id,r.source_ref,r.source_epoch,u.generation
		FROM agent_runtime_memory_records r
		JOIN agent_runtime_memory_sources s ON s.namespace=r.namespace AND s.project_id=r.project_id AND s.subject_id=r.subject_id AND s.collection_id=r.collection_id AND s.source_ref=r.source_ref
		JOIN agent_runtime_memory_subjects u ON u.namespace=r.namespace AND u.project_id=r.project_id AND u.subject_id=r.subject_id
		WHERE r.content<>'' AND r.source_ref<>'' AND r.source_epoch<s.source_epoch
		ORDER BY r.namespace,r.project_id,r.subject_id,r.collection_id,r.source_ref,r.source_epoch LIMIT ?`, "idx_memory_records_lifecycle_source", memory.MaxPurgeBatch)
	assertPlan("physical target", `SELECT memory_id FROM agent_runtime_memory_records WHERE namespace=? AND project_id=? AND subject_id=? AND (collection_id>? OR (collection_id=? AND memory_id>?)) ORDER BY collection_id,memory_id LIMIT ?`, "sqlite_autoindex_agent_runtime_memory_records_1", "n", "p", "s", "", "", "", memory.MaxPurgeBatch)
	assertPlan("reclaim", `SELECT memory_id FROM agent_runtime_memory_records WHERE content='' AND status<>'candidate' AND updated_at_ns<=? ORDER BY updated_at_ns,namespace,project_id,subject_id,collection_id,memory_id LIMIT ?`, "idx_memory_records_lifecycle_reclaim", time.Now().UnixNano(), memory.MaxPurgeBatch)
}

func enableLifecycleSubject(t *testing.T, store memory.AtomicStore, scope memory.Scope) {
	t.Helper()
	snapshot, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	opID := "enable-lifecycle-" + scope.SubjectID
	_, err = store.CommitMutation(context.Background(), memory.Mutation{Type: memory.MutationEnableSubject, Subject: scope.Subject(),
		Operation:     memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionEnableSubject, Subject: scope.Subject(), SubjectGeneration: snapshot.SubjectGeneration, IssuedAt: snapshot.StoreNow, ExpiresAt: snapshot.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)},
		Preconditions: memory.Preconditions{SubjectGeneration: snapshot.SubjectGeneration, PolicyGeneration: snapshot.PolicyGeneration},
		Audit:         memory.AuditEvent{OperationID: opID, Subject: scope.Subject(), Action: memory.ActionEnableSubject, Actor: scope.SubjectID, Issuer: "lifecycle-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
}
