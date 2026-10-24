package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

func TestPostgresMemoryLifecycleV9UpgradeBackfill(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	now := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	store := NewMemoryStore(db).SetClock(func() time.Time { return now })
	retractedScope := memory.Scope{Namespace: "upgrade", ProjectID: "project", SubjectID: "retracted", CollectionID: "private"}
	retracted, _ := rawCreate(t, store, retractedScope, "old-retracted", "retracted-body")
	pgLifecycleRetract(t, store, retractedScope, retracted.MemoryID, retracted.Revision)
	forgottenScope := retractedScope
	forgottenScope.SubjectID = "forgotten"
	rawCreate(t, store, forgottenScope, "old-forgotten", "forgotten-body")
	pgLifecycleForget(t, store, forgottenScope.Subject())
	sourceScope := retractedScope
	sourceScope.SubjectID = "source"
	sourceEpochOne, _ := rawCreate(t, store, sourceScope, "old-source-1", "source-body-1")
	sourceEpochThree, _ := rawCreate(t, store, sourceScope, "old-source-3", "source-body-3")
	if _, err := db.db.ExecContext(context.Background(), `UPDATE agent_runtime_memory_records SET source_ref='legacy-source',source_version='v1',source_epoch=1 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND memory_id=$5`, sourceScope.Namespace, sourceScope.ProjectID, sourceScope.SubjectID, sourceScope.CollectionID, sourceEpochOne.MemoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `UPDATE agent_runtime_memory_records SET source_ref='legacy-source',source_version='v3',source_epoch=3 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND memory_id=$5`, sourceScope.Namespace, sourceScope.ProjectID, sourceScope.SubjectID, sourceScope.CollectionID, sourceEpochThree.MemoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO agent_runtime_memory_sources(namespace,project_id,subject_id,collection_id,source_ref,source_version,source_digest,source_epoch,expires_at,verifier,revoked) VALUES($1,$2,$3,$4,'legacy-source','v4','digest',4,$5,'upgrade',true)`, sourceScope.Namespace, sourceScope.ProjectID, sourceScope.SubjectID, sourceScope.CollectionID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `DROP TABLE agent_runtime_memory_lifecycle_jobs`); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"idx_runtime_memory_records_expiry_discovery", "idx_runtime_memory_records_lifecycle_subject", "idx_runtime_memory_records_lifecycle_source", "idx_runtime_memory_records_lifecycle_reclaim"} {
		if _, err := db.db.ExecContext(context.Background(), `DROP INDEX IF EXISTS `+index); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.ExecContext(context.Background(), `DELETE FROM agent_runtime_schema_migrations WHERE version=10`); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(context.Background(), os.Getenv("POSTGRES_TEST_DSN"), Options{AutoMigrate: true})
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
			pgLifecycleEnable(t, store, forgottenScope)
			newRecord, _ = rawCreate(t, store, forgottenScope, "new-generation", "new-generation")
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
	if err := reopened.db.QueryRowContext(context.Background(), `SELECT count(*) FROM agent_runtime_memory_records WHERE content<>'' AND memory_id<>$1`, newRecord.MemoryID).Scan(&uncleared); err != nil || uncleared != 0 {
		t.Fatalf("uncleared old bodies=%d err=%v", uncleared, err)
	}
	if reclaimed, err := store.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch); err != nil || reclaimed.ReclaimedRecords != 0 || reclaimed.ReclaimedJobs != 0 {
		t.Fatalf("upgrade early reclaim=%#v err=%v", reclaimed, err)
	}
	now = now.Add(memory.LifecycleRetention)
	reclaimed, err := store.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch)
	if err != nil || reclaimed.ReclaimedRecords != 4 || reclaimed.ReclaimedJobs != 4 || reclaimed.ReleasedControls != 4 {
		t.Fatalf("upgrade durable reclaim=%#v err=%v", reclaimed, err)
	}
	var records, sources int
	if err := reopened.db.QueryRowContext(context.Background(), `SELECT (SELECT count(*) FROM agent_runtime_memory_records),(SELECT count(*) FROM agent_runtime_memory_sources)`).Scan(&records, &sources); err != nil || records != 1 || sources != 1 {
		t.Fatalf("upgrade retained controls records=%d sources=%d err=%v", records, sources, err)
	}
}

func TestPostgresMemoryLifecycleIndependentWorkersAndFaultRollback(t *testing.T) {
	firstDB := testDB(t)
	truncateTrustedMemory(t, firstDB)
	secondDB, err := New(context.Background(), os.Getenv("POSTGRES_TEST_DSN"), Options{AutoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondDB.Close() })
	now := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	a, b := NewMemoryStore(firstDB).SetClock(clock), NewMemoryStore(secondDB).SetClock(clock)
	scope := memory.Scope{Namespace: "workers", ProjectID: "project", SubjectID: "subject", CollectionID: "private"}
	record, _ := rawCreate(t, a, scope, "fault-record", "fault-body")
	pgLifecycleRetract(t, a, scope, record.MemoryID, record.Revision)
	type claimResult struct {
		claim memory.PurgeJob
		err   error
	}
	start := make(chan struct{})
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
		} else if !errors.Is(candidate.err, memory.ErrNotFound) {
			t.Fatalf("claim error=%v", candidate.err)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners=%d", winners)
	}
	a.SetLifecycleCommitFailure(func() error { return errors.New("injected before commit") })
	if _, err := a.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch); err == nil {
		t.Fatal("expected lifecycle transaction failure")
	}
	a.SetLifecycleCommitFailure(nil)
	batch, err := b.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch)
	if err != nil || batch.PurgedRecords != 1 || !batch.Completed {
		t.Fatalf("retry after rollback=%#v err=%v", batch, err)
	}
	clockMu.Lock()
	now = now.Add(memory.LifecycleRetention)
	clockMu.Unlock()
	a.SetLifecycleCommitFailure(func() error { return errors.New("injected reclaim failure") })
	if _, err := a.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch); err == nil {
		t.Fatal("expected lifecycle reclaim transaction failure")
	}
	var records, identities, jobs int
	if err := firstDB.db.QueryRowContext(context.Background(), `SELECT (SELECT count(*) FROM agent_runtime_memory_records WHERE memory_id=$1),(SELECT record_identities FROM agent_runtime_memory_collections WHERE namespace=$2 AND project_id=$3 AND subject_id=$4 AND collection_id=$5),(SELECT count(*) FROM agent_runtime_memory_lifecycle_jobs)`, record.MemoryID, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID).Scan(&records, &identities, &jobs); err != nil || records != 1 || identities != 1 || jobs != 1 {
		t.Fatalf("reclaim rollback records=%d identities=%d jobs=%d err=%v", records, identities, jobs, err)
	}
	a.SetLifecycleCommitFailure(nil)
	reclaimed, err := b.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch)
	if err != nil || reclaimed.ReclaimedRecords != 1 || reclaimed.ReclaimedJobs != 1 || reclaimed.ReleasedControls != 1 {
		t.Fatalf("reclaim retry=%#v err=%v", reclaimed, err)
	}
}

func TestPostgresMemoryLifecycleQueryPlansAreIndexed(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 5, 0, 0, 0, time.UTC)
	limits, _ := json.Marshal(memory.DefaultLimits())
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_policies VALUES('plan','project',1,$1::jsonb)`, string(limits)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_subjects VALUES('plan','project','subject',1,false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id) VALUES('plan','project','subject','collection')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_sources(namespace,project_id,subject_id,collection_id,source_ref,source_version,source_digest,source_epoch,expires_at,verifier,revoked) VALUES('plan','project','subject','collection','source','v4','digest',4,$1,'test',true)`, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_records(namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,policy_generation,kind,content,topics,status,source_ref,source_version,source_epoch,producer,policy_version,created_at,updated_at,expires_at)
		SELECT 'plan','project','subject','collection','id-'||lpad(g::text,6,'0'),1,1,1,'verified_fact','body','[]'::jsonb,'active','source','v1',CASE WHEN g%2=0 THEN 1 ELSE 3 END,'test','1',$1::timestamptz,$1::timestamptz,$1::timestamptz+interval '1 day' FROM generate_series(1,5000) g`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `ANALYZE agent_runtime_memory_records`); err != nil {
		t.Fatal(err)
	}
	assertIndex := func(name, query, index string, args ...any) {
		t.Helper()
		var raw []byte
		if err := db.db.QueryRowContext(ctx, `EXPLAIN (ANALYZE,FORMAT JSON) `+query, args...).Scan(&raw); err != nil {
			t.Fatalf("%s explain: %v", name, err)
		}
		var plan []map[string]any
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		root, _ := plan[0]["Plan"].(map[string]any)
		used, recordSeq := inspectLifecyclePlan(root, index)
		if !used || recordSeq {
			t.Fatalf("%s unbounded plan: %s", name, raw)
		}
	}
	assertIndex("source history", `SELECT DISTINCT r.namespace,r.project_id,r.subject_id,r.collection_id,r.source_ref,r.source_epoch,u.generation
		FROM agent_runtime_memory_records r
		JOIN agent_runtime_memory_sources s ON s.namespace=r.namespace AND s.project_id=r.project_id AND s.subject_id=r.subject_id AND s.collection_id=r.collection_id AND s.source_ref=r.source_ref
		JOIN agent_runtime_memory_subjects u ON u.namespace=r.namespace AND u.project_id=r.project_id AND u.subject_id=r.subject_id
		WHERE r.content<>'' AND r.source_ref<>'' AND r.source_epoch<s.source_epoch
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='retract_source' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND j.collection_id=r.collection_id AND j.source_ref=r.source_ref AND j.source_epoch=r.source_epoch)
		ORDER BY r.namespace,r.project_id,r.subject_id,r.collection_id,r.source_ref,r.source_epoch LIMIT 100`, "idx_runtime_memory_records_lifecycle_source")
	assertIndex("physical target", `SELECT memory_id FROM agent_runtime_memory_records WHERE namespace='plan' AND project_id='project' AND subject_id='subject' AND (collection_id>'' OR (collection_id='' AND memory_id>'')) ORDER BY collection_id,memory_id LIMIT 100`, "agent_runtime_memory_records_pkey")
	if _, err := db.db.ExecContext(ctx, `UPDATE agent_runtime_memory_records SET content='',status='retracted' WHERE memory_id<'id-000201'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `ANALYZE agent_runtime_memory_records`); err != nil {
		t.Fatal(err)
	}
	assertIndex("reclaim", `SELECT memory_id FROM agent_runtime_memory_records WHERE content='' AND status<>'candidate' AND updated_at<=$1 ORDER BY updated_at,namespace,project_id,subject_id,collection_id,memory_id LIMIT 100`, "idx_runtime_memory_records_lifecycle_reclaim", now)
}

func inspectLifecyclePlan(node map[string]any, expectedIndex string) (indexUsed, recordSeq bool) {
	relation, _ := node["Relation Name"].(string)
	nodeType, _ := node["Node Type"].(string)
	indexName, _ := node["Index Name"].(string)
	if relation == "agent_runtime_memory_records" && nodeType == "Seq Scan" {
		recordSeq = true
	}
	if relation == "agent_runtime_memory_records" && indexName == expectedIndex {
		indexUsed = true
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		mapped, _ := child.(map[string]any)
		used, seq := inspectLifecyclePlan(mapped, expectedIndex)
		indexUsed = indexUsed || used
		recordSeq = recordSeq || seq
	}
	return indexUsed, recordSeq
}

func pgLifecycleRetract(t *testing.T, store memory.AtomicStore, scope memory.Scope, id string, revision uint64) {
	t.Helper()
	snapshot, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	opID := "retract-lifecycle-" + id
	_, err = store.CommitMutation(context.Background(), memory.Mutation{Type: memory.MutationRetract, Scope: scope, Subject: scope.Subject(), MemoryID: id,
		Operation:     memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionRetract, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snapshot.SubjectGeneration, IssuedAt: snapshot.StoreNow, ExpiresAt: snapshot.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)},
		Preconditions: memory.Preconditions{SubjectGeneration: snapshot.SubjectGeneration, PolicyGeneration: snapshot.PolicyGeneration, ExpectedRevision: revision},
		Audit:         memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionRetract, Actor: scope.SubjectID, Issuer: "lifecycle-test", MemoryID: id},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pgLifecycleForget(t *testing.T, store memory.AtomicStore, subject memory.SubjectScope) {
	t.Helper()
	snapshot, err := store.SnapshotSubject(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	opID := "forget-lifecycle-" + subject.SubjectID
	_, err = store.CommitMutation(context.Background(), memory.Mutation{Type: memory.MutationForget, Subject: subject,
		Operation:     memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionForgetSubject, Subject: subject, SubjectGeneration: snapshot.SubjectGeneration, IssuedAt: snapshot.StoreNow, ExpiresAt: snapshot.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)},
		Preconditions: memory.Preconditions{SubjectGeneration: snapshot.SubjectGeneration, PolicyGeneration: snapshot.PolicyGeneration},
		Audit:         memory.AuditEvent{OperationID: opID, Subject: subject, Action: memory.ActionForgetSubject, Actor: subject.SubjectID, Issuer: "lifecycle-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pgLifecycleEnable(t *testing.T, store memory.AtomicStore, scope memory.Scope) {
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
