package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory/memorytest"
)

type postgresProposalExtractor struct{ calls atomic.Int64 }

func (e *postgresProposalExtractor) Extract(context.Context, memory.ProposalExtractionRequest) (memory.ProposalExtractionResult, error) {
	e.calls.Add(1)
	return memory.ProposalExtractionResult{Candidates: []memory.CandidateDTO{{Kind: memory.KindPreference, Content: "postgres candidate"}}}, nil
}

func TestMemoryPostgresContract(t *testing.T) {
	db := testDB(t)
	memorytest.Contract(t, func(t *testing.T, limits memory.Limits) memorytest.Harness {
		t.Helper()
		truncateTrustedMemory(t, db)
		var clockMu sync.Mutex
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		store := NewMemoryStore(db).SetClock(clock).SetLimits(limits)
		return memorytest.Harness{
			Store:               store,
			Advance:             func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() },
			ContentReads:        store.ContentReadCount,
			ReceiptContentBytes: store.ReceiptContentBytes,
			FailAudit: func(fail bool) {
				if fail {
					store.SetAuditFailure(func(memory.AuditEvent) error { return errors.New("injected") })
				} else {
					store.SetAuditFailure(nil)
				}
			},
		}
	})
}

func TestMemoryPostgresLifecycle(t *testing.T) {
	db := testDB(t)
	memorytest.LifecycleContract(t, func(t *testing.T, limits memory.Limits) memorytest.Harness {
		t.Helper()
		truncateTrustedMemory(t, db)
		var clockMu sync.Mutex
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
		store := NewMemoryStore(db).SetClock(clock).SetLimits(limits)
		return memorytest.Harness{Store: store, Advance: func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }, ContentReads: store.ContentReadCount, LifecycleAudits: func() []memory.AuditEvent {
			rows, err := db.db.Query(`SELECT operation_id,namespace,project_id,subject_id,collection_id,action,actor,issuer,memory_id,revision,generation,occurred_at FROM agent_runtime_memory_audits WHERE action='purge' ORDER BY audit_id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []memory.AuditEvent
			for rows.Next() {
				var audit memory.AuditEvent
				if err := rows.Scan(&audit.OperationID, &audit.Subject.Namespace, &audit.Subject.ProjectID, &audit.Subject.SubjectID, &audit.Scope.CollectionID, &audit.Action, &audit.Actor, &audit.Issuer, &audit.MemoryID, &audit.Revision, &audit.Generation, &audit.OccurredAt); err != nil {
					t.Fatal(err)
				}
				audit.Scope.Namespace, audit.Scope.ProjectID, audit.Scope.SubjectID = audit.Subject.Namespace, audit.Subject.ProjectID, audit.Subject.SubjectID
				out = append(out, audit)
			}
			return out
		}, ReceiptContentBytes: store.ReceiptContentBytes, FailAudit: func(fail bool) {
			if fail {
				store.SetAuditFailure(func(memory.AuditEvent) error { return errors.New("injected") })
			} else {
				store.SetAuditFailure(nil)
			}
		}}
	})
}

func TestMemoryPostgresIndependentConnectionsAndUnknownCommit(t *testing.T) {
	first := testDB(t)
	truncateTrustedMemory(t, first)
	second, err := New(context.Background(), os.Getenv("POSTGRES_TEST_DSN"), Options{AutoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	now := time.Date(2026, 9, 8, 1, 0, 0, 0, time.UTC)
	a := NewMemoryStore(first).SetClock(func() time.Time { return now })
	b := NewMemoryStore(second).SetClock(func() time.Time { return now })
	scope := memory.Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "subject", CollectionID: "collection"}
	created, cmd := rawCreate(t, a, scope, "shared", "original")

	makeRevise := func(id, content string) memory.Mutation {
		snap, e := a.Snapshot(context.Background(), scope, "")
		if e != nil {
			t.Fatal(e)
		}
		return memory.Mutation{Type: memory.MutationRevise, Scope: scope, Subject: scope.Subject(), MemoryID: created.MemoryID, Content: content, Kind: memory.KindPreference, Retention: time.Hour,
			Operation:     memory.Operation{ID: id, KeyID: "test", Action: memory.ActionRevise, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(id)},
			Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration, ExpectedRevision: created.Revision}, Audit: memory.AuditEvent{OperationID: id, Scope: scope, Subject: scope.Subject(), Action: memory.ActionRevise, Actor: "actor", Issuer: "test", MemoryID: created.MemoryID}}
	}
	commands := []memory.Mutation{makeRevise("cas-a", "a"), makeRevise("cas-b", "b")}
	errs := make(chan error, 2)
	start := make(chan struct{})
	for i, store := range []*MemoryStore{a, b} {
		go func(store *MemoryStore, cmd memory.Mutation) {
			<-start
			_, e := store.CommitMutation(context.Background(), cmd)
			errs <- e
		}(store, commands[i])
	}
	close(start)
	winners, conflicts := 0, 0
	for range 2 {
		e := <-errs
		if e == nil {
			winners++
		} else if errors.Is(e, memory.ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("CAS error: %v", e)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", winners, conflicts)
	}

	unknownID := "commit-unknown"
	snap, err := a.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	unknown := cmd
	unknown.MemoryID = "unknown"
	unknown.Audit.MemoryID = "unknown"
	unknown.Content = "once"
	unknown.Operation.ID = unknownID
	unknown.Operation.CommandDigest = []byte(unknownID)
	unknown.Audit.OperationID = unknownID
	unknown.Operation.SubjectGeneration = snap.SubjectGeneration
	unknown.Operation.IssuedAt = snap.StoreNow
	unknown.Operation.ExpiresAt = snap.StoreNow.Add(time.Hour)
	unknown.Preconditions.SubjectGeneration = snap.SubjectGeneration
	unknown.Preconditions.PolicyGeneration = snap.PolicyGeneration
	a.SetAfterCommitFailure(func() error { return errors.New("response lost") })
	if _, err := a.CommitMutation(context.Background(), unknown); !errors.Is(err, memory.ErrCommitUnknown) {
		t.Fatalf("commit response = %v", err)
	}
	a.SetAfterCommitFailure(nil)
	replayed, err := b.CommitMutation(context.Background(), unknown)
	if err != nil || !replayed.Replayed {
		t.Fatalf("retry = %#v, %v", replayed, err)
	}
	var audits, receipts int
	if err := first.db.QueryRow(`SELECT (SELECT count(*) FROM agent_runtime_memory_audits WHERE operation_id=$1),(SELECT count(*) FROM agent_runtime_memory_receipts WHERE operation_id=$1)`, unknownID).Scan(&audits, &receipts); err != nil || audits != 1 || receipts != 1 {
		t.Fatalf("atomic rows audits=%d receipts=%d err=%v", audits, receipts, err)
	}

	limits := memory.DefaultLimits()
	limits.MaxRecordsPerScope = 1
	a.SetLimits(limits)
	b.SetLimits(limits)
	quotaScope := memory.Scope{Namespace: "tenant", ProjectID: "quota-project", SubjectID: "subject", CollectionID: "collection"}
	qa := rawCreateCommand(t, a, quotaScope, "quota-a", "a")
	qb := rawCreateCommand(t, b, quotaScope, "quota-b", "b")
	errs = make(chan error, 2)
	start = make(chan struct{})
	for i, store := range []*MemoryStore{a, b} {
		go func(store *MemoryStore, cmd memory.Mutation) {
			<-start
			_, e := store.CommitMutation(context.Background(), cmd)
			errs <- e
		}(store, []memory.Mutation{qa, qb}[i])
	}
	close(start)
	winners, conflicts = 0, 0
	for range 2 {
		e := <-errs
		if e == nil {
			winners++
		} else if errors.Is(e, memory.ErrQuota) {
			conflicts++
		} else {
			t.Fatalf("quota error: %v", e)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("quota winners=%d quota_errors=%d", winners, conflicts)
	}
}

func TestMemoryPostgresProposalCommitFences(t *testing.T) {
	memorytest.ProposalFenceContract(t, func(t *testing.T) memorytest.ProposalFenceHarness {
		first := testDB(t)
		truncateTrustedMemory(t, first)
		second, err := New(context.Background(), os.Getenv("POSTGRES_TEST_DSN"), Options{AutoMigrate: false})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = second.Close() })
		return memorytest.ProposalFenceHarness{
			Primary: NewMemoryStore(first), Concurrent: NewMemoryStore(second),
			State: func(ctx context.Context, scope memory.Scope) (state memorytest.ProposalFenceState, err error) {
				err = first.db.QueryRowContext(ctx, `SELECT
					(SELECT COUNT(*) FROM agent_runtime_memory_records WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND status=$5),
					(SELECT COUNT(*) FROM agent_runtime_memory_audits WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND action=$6),
					(SELECT COUNT(*) FROM agent_runtime_memory_receipts WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND action=$6),
					(SELECT COUNT(*) FROM agent_runtime_memory_audits WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND action=$7),
					(SELECT COUNT(*) FROM agent_runtime_memory_receipts WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND action=$7),
					COALESCE((SELECT records_count FROM agent_runtime_memory_collections WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4),0),
					COALESCE((SELECT content_bytes FROM agent_runtime_memory_collections WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4),0),
					COALESCE((SELECT candidate_batches FROM agent_runtime_memory_collections WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4),0)`,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, memory.StatusCandidate, memory.ActionPropose, memory.ActionConfirm,
				).Scan(&state.CandidateRecords, &state.ProposalAudits, &state.ProposalReceipts, &state.ConfirmAudits, &state.ConfirmReceipts, &state.PhysicalRecords, &state.PhysicalBytes, &state.CandidateBatches)
				return state, err
			},
		}
	})
}

func TestMemoryPostgresProposalCanonicalSourceExpiry(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	store := NewMemoryStore(db)
	extractor := &postgresProposalExtractor{}
	module, err := memory.NewModule(memory.ModuleConfig{
		Store: store, Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
		Proposal: &memory.ProposalConfig{Extractor: extractor, OutboundPolicy: memory.ProposalOutboundPolicyFunc(func(context.Context, memory.ProposalOutboundRequest) error { return nil }), Producer: "postgres-extractor", ModelConfigVersion: "v1"},
		Keys:     map[string][]byte{"test": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := memory.Scope{Namespace: "proposal", ProjectID: "precision", SubjectID: "subject", CollectionID: "private"}
	principal := memory.Principal{Subject: "reviewer", Namespace: scope.Namespace, Issuer: "postgres-test"}
	att := memory.SourceAttestation{Scope: scope, SourceRef: "nanosecond-source", Version: "v1", Digest: "digest", SourceEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour).Add(789 * time.Nanosecond), Verifier: "trusted-host"}
	createToken, err := module.IssueOperationToken(context.Background(), principal, scope, memory.ActionCreateVerifiedFact, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := module.CreateVerifiedFact(context.Background(), principal, scope, memory.CreateRequest{Content: "verified source anchor", Kind: memory.KindVerifiedFact, OperationToken: createToken}, att); err != nil {
		t.Fatal(err)
	}
	propose := func(source memory.SourceAttestation) (memory.ProposalResult, error) {
		token, err := module.IssueOperationToken(context.Background(), principal, scope, memory.ActionPropose, time.Hour)
		if err != nil {
			return memory.ProposalResult{}, err
		}
		return module.Propose(context.Background(), principal, scope, memory.ProposalRequest{Fragments: []memory.ProposalFragment{{Content: "bounded"}}, Source: source, OperationToken: token})
	}
	first, err := propose(att)
	if err != nil || len(first.Candidates) != 1 {
		t.Fatalf("first proposal=%#v error=%v", first, err)
	}
	replay, err := propose(att)
	if err != nil || !replay.Replayed || replay.Candidates[0].MemoryID != first.Candidates[0].MemoryID {
		t.Fatalf("replay=%#v error=%v", replay, err)
	}
	changed := att
	changed.ExpiresAt = memory.CanonicalSourceAttestation(att).ExpiresAt.Add(time.Microsecond)
	if _, err := propose(changed); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("one-microsecond expiry change error=%v", err)
	}
	if extractor.calls.Load() != 2 {
		t.Fatalf("mismatched source reached model, calls=%d", extractor.calls.Load())
	}
}

func rawCreate(t *testing.T, store memory.AtomicStore, scope memory.Scope, id, content string) (*memory.Record, memory.Mutation) {
	t.Helper()
	cmd := rawCreateCommand(t, store, scope, id, content)
	result, err := store.CommitMutation(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return result.Record, cmd
}

func rawCreateCommand(t *testing.T, store memory.AtomicStore, scope memory.Scope, id, content string) memory.Mutation {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	op := "create-" + id
	cmd := memory.Mutation{Type: memory.MutationCreate, Scope: scope, Subject: scope.Subject(), MemoryID: id, Kind: memory.KindPreference, Content: content, Retention: time.Hour,
		Operation: memory.Operation{ID: op, KeyID: "test", Action: memory.ActionCreate, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(op)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration, CollectionProposalEpoch: snap.CollectionProposalEpoch}, Audit: memory.AuditEvent{OperationID: op, Scope: scope, Subject: scope.Subject(), Action: memory.ActionCreate, Actor: "actor", Issuer: "test", MemoryID: id}}
	return cmd
}

func TestMemoryPostgresLegacyManifestMigration(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	if _, err := db.db.Exec(`DELETE FROM agent_runtime_memories WHERE memory_id=ANY($1::text[])`, []string{"legacy-1", "legacy-2", "legacy-fail"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("legacy-%d", i)
		payload, _ := json.Marshal(memory.UserMemory{MemoryID: id, Memory: fmt.Sprintf("body-%d", i), Topics: []string{"legacy"}})
		if _, err := db.db.Exec(`INSERT INTO agent_runtime_memories(memory_id,user_id,agent_id,payload,created_at,updated_at) VALUES($1,'untrusted-user','untrusted-agent',$2::jsonb,now(),now()) ON CONFLICT(memory_id) DO UPDATE SET payload=EXCLUDED.payload`, id, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	entries := []LegacyMemoryManifestEntry{{LegacyMemoryID: "legacy-1", Scope: memory.Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}}, {LegacyMemoryID: "legacy-2", Scope: memory.Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "other"}}}
	hash, err := LegacyMemoryManifestHash("fixture-db", 24*time.Hour, entries)
	if err != nil {
		t.Fatal(err)
	}
	manifest := LegacyMemoryManifest{SourceID: "fixture-db", ManifestHash: hash, CandidateRetention: 24 * time.Hour, Entries: entries}
	store := NewMemoryStore(db).SetClock(func() time.Time { return time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC) })
	dry, err := store.DryRunLegacyMemoryMigration(context.Background(), manifest)
	if err != nil || dry.Ready != 2 || dry.Missing != 0 || dry.Invalid != 0 {
		t.Fatalf("dry run=%#v err=%v", dry, err)
	}
	first, err := store.MigrateLegacyMemories(context.Background(), manifest, 1)
	if err != nil || first.NextIndex != 1 || first.Imported != 1 || first.Completed {
		t.Fatalf("first batch=%#v err=%v", first, err)
	}
	second, err := store.MigrateLegacyMemories(context.Background(), manifest, 1)
	if err != nil || second.NextIndex != 2 || second.Imported != 2 || !second.Completed {
		t.Fatalf("resume=%#v err=%v", second, err)
	}
	repeat, err := store.MigrateLegacyMemories(context.Background(), manifest, 100)
	if err != nil || repeat.Imported != 2 || !repeat.Completed {
		t.Fatalf("repeat=%#v err=%v", repeat, err)
	}
	var legacyRows, candidates int
	if err := db.db.QueryRow(`SELECT (SELECT count(*) FROM agent_runtime_memories WHERE memory_id=ANY($1::text[])),(SELECT count(*) FROM agent_runtime_memory_records WHERE status='candidate')`, []string{"legacy-1", "legacy-2"}).Scan(&legacyRows, &candidates); err != nil || legacyRows != 2 || candidates != 2 {
		t.Fatalf("rows legacy=%d candidates=%d err=%v", legacyRows, candidates, err)
	}
	var sourced int
	if err := db.db.QueryRow(`SELECT count(*) FROM agent_runtime_memory_records WHERE status='candidate' AND source_ref<>'' AND source_epoch=1`).Scan(&sourced); err != nil || sourced != 2 {
		t.Fatalf("sourced candidates=%d err=%v", sourced, err)
	}
	var migrationAudits int
	if err := db.db.QueryRow(`SELECT count(*) FROM agent_runtime_memory_audits WHERE action='propose' AND actor='legacy-migration'`).Scan(&migrationAudits); err != nil || migrationAudits != 2 {
		t.Fatalf("migration audits=%d err=%v", migrationAudits, err)
	}
	visible, _, err := store.ReadRecord(context.Background(), entries[0].Scope, "legacy-1")
	if err != nil || visible != nil {
		t.Fatalf("candidate became visible=%#v err=%v", visible, err)
	}
	altered := manifest
	altered.Entries = append([]LegacyMemoryManifestEntry(nil), entries...)
	altered.Entries[0].Scope.ProjectID = "other"
	altered.ManifestHash, _ = LegacyMemoryManifestHash(altered.SourceID, altered.CandidateRetention, altered.Entries)
	if _, err := store.MigrateLegacyMemories(context.Background(), altered, 1); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("source remap = %v", err)
	}
	crossEntries := []LegacyMemoryManifestEntry{{LegacyMemoryID: "legacy-1", Scope: memory.Scope{Namespace: "other", ProjectID: "p", SubjectID: "s", CollectionID: "c"}}}
	crossHash, _ := LegacyMemoryManifestHash("other-source", time.Hour, crossEntries)
	cross := LegacyMemoryManifest{SourceID: "other-source", ManifestHash: crossHash, CandidateRetention: time.Hour, Entries: crossEntries}
	if dry, err := store.DryRunLegacyMemoryMigration(context.Background(), cross); err != nil || dry.Conflicts != 1 || dry.Ready != 0 {
		t.Fatalf("cross dry=%#v err=%v", dry, err)
	}
	if _, err := store.MigrateLegacyMemories(context.Background(), cross, 1); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("cross source import=%v", err)
	}
	var copies int
	if err := db.db.QueryRow(`SELECT count(*) FROM agent_runtime_memory_records WHERE memory_id='legacy-1'`).Scan(&copies); err != nil || copies != 1 {
		t.Fatalf("legacy copies=%d err=%v", copies, err)
	}
	drift := manifest
	drift.CandidateRetention = 48 * time.Hour
	drift.ManifestHash, _ = LegacyMemoryManifestHash(drift.SourceID, drift.CandidateRetention, drift.Entries)
	if _, err := store.MigrateLegacyMemories(context.Background(), drift, 1); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("retention drift=%v", err)
	}
	invalid := manifest
	invalid.SourceID = "bad"
	invalid.Entries[0].Scope.Namespace = ""
	invalid.ManifestHash, _ = LegacyMemoryManifestHash(invalid.SourceID, invalid.CandidateRetention, invalid.Entries)
	if _, err := store.DryRunLegacyMemoryMigration(context.Background(), invalid); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("invalid scope = %v", err)
	}
	failPayload, _ := json.Marshal(memory.UserMemory{MemoryID: "legacy-fail", Memory: "body"})
	if _, err := db.db.Exec(`INSERT INTO agent_runtime_memories(memory_id,payload,created_at,updated_at) VALUES('legacy-fail',$1::jsonb,now(),now()) ON CONFLICT(memory_id) DO UPDATE SET payload=EXCLUDED.payload`, string(failPayload)); err != nil {
		t.Fatal(err)
	}
	failEntries := []LegacyMemoryManifestEntry{{LegacyMemoryID: "legacy-fail", Scope: memory.Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "fail"}}}
	failHash, _ := LegacyMemoryManifestHash("fail-source", time.Hour, failEntries)
	failManifest := LegacyMemoryManifest{SourceID: "fail-source", ManifestHash: failHash, CandidateRetention: time.Hour, Entries: failEntries}
	store.SetAuditFailure(func(memory.AuditEvent) error { return errors.New("injected") })
	if _, err := store.MigrateLegacyMemories(context.Background(), failManifest, 1); !errors.Is(err, memory.ErrAudit) {
		t.Fatalf("migration audit failure=%v", err)
	}
	store.SetAuditFailure(nil)
	var partial int
	if err := db.db.QueryRow(`SELECT (SELECT count(*) FROM agent_runtime_memory_records WHERE memory_id='legacy-fail')+(SELECT count(*) FROM agent_runtime_memory_legacy_assignments WHERE legacy_memory_id='legacy-fail')+(SELECT count(*) FROM agent_runtime_memory_legacy_migrations WHERE source_id='fail-source')`).Scan(&partial); err != nil || partial != 0 {
		t.Fatalf("migration partial rows=%d err=%v", partial, err)
	}
	if result, err := store.MigrateLegacyMemories(context.Background(), failManifest, 1); err != nil || !result.Completed {
		t.Fatalf("migration retry=%#v err=%v", result, err)
	}
}

func TestMemoryPostgresDryRunQuotaAndDisabled(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	if _, err := db.db.Exec(`DELETE FROM agent_runtime_memories WHERE memory_id=ANY($1::text[])`, []string{"dry-1", "dry-2", "dry-disabled"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"dry-1", "dry-2", "dry-disabled"} {
		payload, _ := json.Marshal(memory.UserMemory{MemoryID: id, Memory: "body"})
		if _, err := db.db.Exec(`INSERT INTO agent_runtime_memories(memory_id,payload,created_at,updated_at) VALUES($1,$2::jsonb,now(),now()) ON CONFLICT(memory_id) DO UPDATE SET payload=EXCLUDED.payload`, id, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	limits := memory.DefaultLimits()
	limits.MaxRecordsPerScope = 1
	raw, _ := json.Marshal(limits)
	defaults, _ := json.Marshal(memory.DefaultLimits())
	if _, err := db.db.Exec(`INSERT INTO agent_runtime_memory_policies VALUES('n','quota',1,$1::jsonb),('n','disabled',1,$2::jsonb)`, string(raw), string(defaults)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO agent_runtime_memory_subjects VALUES('n','quota','s',1,false),('n','disabled','s',1,true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id,proposal_epoch) VALUES('n','quota','s','c',123)`); err != nil {
		t.Fatal(err)
	}
	entries := []LegacyMemoryManifestEntry{{LegacyMemoryID: "dry-1", Scope: memory.Scope{Namespace: "n", ProjectID: "quota", SubjectID: "s", CollectionID: "c"}}, {LegacyMemoryID: "dry-2", Scope: memory.Scope{Namespace: "n", ProjectID: "quota", SubjectID: "s", CollectionID: "c"}}, {LegacyMemoryID: "dry-disabled", Scope: memory.Scope{Namespace: "n", ProjectID: "disabled", SubjectID: "s", CollectionID: "c"}}}
	hash, err := LegacyMemoryManifestHash("dry-source", time.Hour, entries)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(db)
	manifest := LegacyMemoryManifest{SourceID: "dry-source", ManifestHash: hash, CandidateRetention: time.Hour, Entries: entries}
	report, err := store.DryRunLegacyMemoryMigration(context.Background(), manifest)
	if err != nil || report.Ready != 1 || report.OverQuota != 1 || report.Conflicts != 1 {
		t.Fatalf("dry report=%#v err=%v", report, err)
	}
	tooLarge := []LegacyMemoryManifestEntry{{LegacyMemoryID: strings.Repeat("x", 257), Scope: entries[0].Scope}}
	if _, err := LegacyMemoryManifestHash("dry-source", time.Hour, tooLarge); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("unbounded id=%v", err)
	}
	if result, err := store.MigrateLegacyMemories(context.Background(), manifest, 1); err != nil || result.NextIndex != 1 {
		t.Fatalf("epoch import=%#v err=%v", result, err)
	}
	var epoch uint64
	if err := db.db.QueryRow(`SELECT proposal_epoch FROM agent_runtime_memory_records WHERE namespace='n' AND project_id='quota' AND subject_id='s' AND collection_id='c' AND memory_id='dry-1'`).Scan(&epoch); err != nil || epoch != 123 {
		t.Fatalf("proposal epoch=%d err=%v", epoch, err)
	}
}

func TestMemoryPostgresRejectsCorruptPolicy(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	store := NewMemoryStore(db).SetClock(func() time.Time { return time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC) })
	scope := memory.Scope{Namespace: "n", ProjectID: "corrupt", SubjectID: "s", CollectionID: "c"}
	cmd := rawCreateCommand(t, store, scope, "id", "body")
	if _, err := db.db.Exec(`INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits) VALUES('n','corrupt',1,'{}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(context.Background(), scope, ""); !errors.Is(err, memory.ErrUnavailable) {
		t.Fatalf("corrupt read=%v", err)
	}
	if _, err := store.CommitMutation(context.Background(), cmd); !errors.Is(err, memory.ErrUnavailable) {
		t.Fatalf("corrupt write=%v", err)
	}
}

func TestMemoryPostgresListRecordsCombinedControlContract(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	store := NewMemoryStore(db).SetClock(func() time.Time { return time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC) })
	scope := memory.Scope{Namespace: "list-control", ProjectID: "project", SubjectID: "subject", CollectionID: "collection"}
	limits := memory.DefaultLimits()
	limits.DefaultRecallLimit = 1
	limits.MaxRecallLimit = 2
	limits.MaxRecallCandidates = 2
	limits.MaxCandidateBytes = 2048
	rawLimits, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits) VALUES($1,$2,1,$3::jsonb)`, scope.Namespace, scope.ProjectID, string(rawLimits)); err != nil {
		t.Fatal(err)
	}
	rawCreate(t, store, scope, "record", "combined list control")
	request := memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: limits.MaxCandidateBytes}
	if listed, err := store.ListRecords(context.Background(), request); err != nil || listed.Generation != 1 || len(listed.Records) != 1 {
		t.Fatalf("list with persisted limits = %#v, %v", listed, err)
	}
	tooManyCandidates := request
	tooManyCandidates.CandidateLimit = limits.MaxRecallCandidates + 1
	if _, err := store.ListRecords(context.Background(), tooManyCandidates); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("candidate limit error=%v", err)
	}
	tooManyBytes := request
	tooManyBytes.CandidateBytes++
	if _, err := store.ListRecords(context.Background(), tooManyBytes); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("candidate bytes error=%v", err)
	}

	for _, corrupt := range []string{"[]", "{}"} {
		if _, err := db.db.ExecContext(context.Background(), `UPDATE agent_runtime_memory_policies SET limits=$3::jsonb WHERE namespace=$1 AND project_id=$2`, scope.Namespace, scope.ProjectID, corrupt); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ListRecords(context.Background(), request); !errors.Is(err, memory.ErrUnavailable) {
			t.Fatalf("corrupt policy %s error=%v", corrupt, err)
		}
	}
	if _, err := db.db.ExecContext(context.Background(), `UPDATE agent_runtime_memory_policies SET limits=$3::jsonb WHERE namespace=$1 AND project_id=$2`, scope.Namespace, scope.ProjectID, string(rawLimits)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `UPDATE agent_runtime_memory_subjects SET generation=7,disabled=true WHERE namespace=$1 AND project_id=$2 AND subject_id=$3`, scope.Namespace, scope.ProjectID, scope.SubjectID); err != nil {
		t.Fatal(err)
	}
	reads := store.ContentReadCount()
	disabled, err := store.ListRecords(context.Background(), request)
	if err != nil || disabled.Generation != 7 || len(disabled.Records) != 0 || store.ContentReadCount() != reads {
		t.Fatalf("disabled list=%#v reads=%d/%d err=%v", disabled, store.ContentReadCount(), reads, err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ListRecords(canceled, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled list error=%v", err)
	}
	closed, err := New(context.Background(), os.Getenv("POSTGRES_TEST_DSN"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMemoryStore(closed).ListRecords(context.Background(), request); !errors.Is(err, memory.ErrUnavailable) {
		t.Fatalf("closed list error=%v", err)
	}
}

func TestMemoryPostgresListRecordsRetainsRepeatableReadSnapshot(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	store := NewMemoryStore(db).SetClock(func() time.Time { return time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC) })
	scope := memory.Scope{Namespace: "list-snapshot", ProjectID: "project", SubjectID: "subject", CollectionID: "collection"}
	initial := memory.DefaultLimits()
	initial.MaxCandidateBytes = 2048
	encodedInitial, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits) VALUES($1,$2,1,$3::jsonb)`, scope.Namespace, scope.ProjectID, string(encodedInitial)); err != nil {
		t.Fatal(err)
	}
	rawCreate(t, store, scope, "old-record", "visible in the original snapshot")
	other, err := New(context.Background(), os.Getenv("POSTGRES_TEST_DSN"), Options{MaxOpenConns: 2, MaxIdleConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	controlRead := make(chan struct{})
	continueCandidate := make(chan struct{})
	var controlOnce, releaseOnce sync.Once
	releaseCandidate := func() { releaseOnce.Do(func() { close(continueCandidate) }) }
	t.Cleanup(releaseCandidate)
	store.mu.Lock()
	store.afterListControl = func() {
		controlOnce.Do(func() { close(controlRead) })
		<-continueCandidate
	}
	store.mu.Unlock()
	t.Cleanup(func() {
		store.mu.Lock()
		store.afterListControl = nil
		store.mu.Unlock()
	})
	request := memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: initial.MaxCandidateBytes}
	type outcome struct {
		result memory.ListResult
		err    error
	}
	firstResult := make(chan outcome, 1)
	go func() {
		result, err := store.ListRecords(context.Background(), request)
		firstResult <- outcome{result: result, err: err}
	}()
	select {
	case <-controlRead:
	case <-time.After(5 * time.Second):
		t.Fatal("ListRecords did not reach the post-control barrier")
	}
	updated := initial
	updated.MaxCandidateBytes = 1024
	encodedUpdated, err := json.Marshal(updated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.db.ExecContext(context.Background(), `UPDATE agent_runtime_memory_policies SET generation=2,limits=$3::jsonb WHERE namespace=$1 AND project_id=$2`, scope.Namespace, scope.ProjectID, string(encodedUpdated)); err != nil {
		t.Fatal(err)
	}
	if _, err := other.db.ExecContext(context.Background(), `UPDATE agent_runtime_memory_subjects SET generation=2,disabled=true WHERE namespace=$1 AND project_id=$2 AND subject_id=$3`, scope.Namespace, scope.ProjectID, scope.SubjectID); err != nil {
		t.Fatal(err)
	}
	retracted, err := other.db.ExecContext(context.Background(), `UPDATE agent_runtime_memory_records SET status='retracted' WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND memory_id='old-record'`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := retracted.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("retract old record changed=%d err=%v", changed, err)
	}
	releaseCandidate()
	got := <-firstResult
	store.mu.Lock()
	store.afterListControl = nil
	store.mu.Unlock()
	if got.err != nil || got.result.Generation != 1 || len(got.result.Records) != 1 || got.result.Records[0].MemoryID != "old-record" {
		t.Fatalf("repeatable-read result=%#v err=%v", got.result, got.err)
	}
	reads := store.ContentReadCount()
	if _, err := store.ListRecords(context.Background(), request); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("next list did not observe changed policy: %v", err)
	}
	newRequest := request
	newRequest.CandidateBytes = updated.MaxCandidateBytes
	next, err := store.ListRecords(context.Background(), newRequest)
	if err != nil || next.Generation != 2 || len(next.Records) != 0 || store.ContentReadCount() != reads {
		t.Fatalf("next list did not observe subject fence: %#v reads=%d/%d err=%v", next, store.ContentReadCount(), reads, err)
	}
}

func TestMemoryPostgresSnapshotControlStateAndClockPrecision(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	clock := time.Date(2026, 9, 8, 1, 2, 3, 123456789, time.UTC)
	store := NewMemoryStore(db).SetClock(func() time.Time { return clock })
	scope := memory.Scope{Namespace: "snapshot", ProjectID: "project", SubjectID: "subject", CollectionID: "collection"}

	missing, err := store.Snapshot(context.Background(), scope, "absent")
	if err != nil {
		t.Fatal(err)
	}
	if missing.StoreNow.UnixNano() != clock.UnixNano() || missing.Policy != memory.DefaultLimits() || missing.PolicyGeneration != 1 || missing.SubjectGeneration != 1 || missing.SubjectDisabled || missing.CollectionProposalEpoch != 0 || missing.Source != nil || missing.SourceEpoch != 0 {
		t.Fatalf("missing snapshot=%+v", missing)
	}
	missingSubject, err := store.SnapshotSubject(context.Background(), scope.Subject())
	if err != nil {
		t.Fatal(err)
	}
	if missingSubject.StoreNow.UnixNano() != clock.UnixNano() || missingSubject.Policy != memory.DefaultLimits() || missingSubject.PolicyGeneration != 1 || missingSubject.SubjectGeneration != 1 || missingSubject.SubjectDisabled {
		t.Fatalf("missing subject snapshot=%+v", missingSubject)
	}
	listed, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes})
	if err != nil {
		t.Fatal(err)
	}
	if listed.StoreNow.UnixNano() != clock.UnixNano() || listed.Generation != 1 || len(listed.Records) != 0 {
		t.Fatalf("missing list control snapshot=%+v", listed)
	}

	limits := memory.DefaultLimits()
	limits.MaxRecordsPerScope = 777
	rawLimits, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	expires := clock.Add(time.Hour).Truncate(time.Microsecond)
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits) VALUES($1,$2,7,$3::jsonb)`, scope.Namespace, scope.ProjectID, string(rawLimits)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO agent_runtime_memory_subjects(namespace,project_id,subject_id,generation,disabled) VALUES($1,$2,$3,11,true)`, scope.Namespace, scope.ProjectID, scope.SubjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id,proposal_epoch) VALUES($1,$2,$3,$4,13)`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO agent_runtime_memory_sources(namespace,project_id,subject_id,collection_id,source_ref,source_version,source_digest,source_epoch,expires_at,verifier,revoked) VALUES($1,$2,$3,$4,'present','v1','digest',17,$5,'issuer',true)`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, expires); err != nil {
		t.Fatal(err)
	}

	present, err := store.Snapshot(context.Background(), scope, "present")
	if err != nil {
		t.Fatal(err)
	}
	if present.StoreNow.UnixNano() != clock.UnixNano() || present.Policy != limits || present.PolicyGeneration != 7 || present.SubjectGeneration != 11 || !present.SubjectDisabled || present.CollectionProposalEpoch != 13 || present.Source == nil || present.Source.Version != "v1" || present.Source.Digest != "digest" || present.Source.Epoch != 17 || !present.Source.ExpiresAt.Equal(expires) || present.Source.Verifier != "issuer" || !present.Source.Revoked {
		t.Fatalf("present snapshot=%+v", present)
	}
	absent, err := store.Snapshot(context.Background(), scope, "not-present")
	if err != nil {
		t.Fatal(err)
	}
	if absent.Source != nil || absent.SourceEpoch != 0 {
		t.Fatalf("absent source snapshot=%+v", absent)
	}
	subject, err := store.SnapshotSubject(context.Background(), scope.Subject())
	if err != nil {
		t.Fatal(err)
	}
	if subject.StoreNow.UnixNano() != clock.UnixNano() || subject.Policy != limits || subject.PolicyGeneration != 7 || subject.SubjectGeneration != 11 || !subject.SubjectDisabled {
		t.Fatalf("subject snapshot=%+v", subject)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Snapshot(canceled, scope, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot error=%v", err)
	}
	if _, err := store.SnapshotSubject(canceled, scope.Subject()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled subject snapshot error=%v", err)
	}

	closed, err := New(context.Background(), os.Getenv("POSTGRES_TEST_DSN"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMemoryStore(closed).Snapshot(context.Background(), scope, ""); !errors.Is(err, memory.ErrUnavailable) {
		t.Fatalf("closed database snapshot error=%v", err)
	}
}

func TestMemoryPostgresSnapshotQueryShape(t *testing.T) {
	source, err := os.ReadFile("trusted_memory_store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	subjectStart := strings.Index(text, "func (s *MemoryStore) SnapshotSubject")
	scopeStart := strings.Index(text, "func (s *MemoryStore) Snapshot(ctx")
	readStart := strings.Index(text, "func (s *MemoryStore) ReadRecord")
	if subjectStart < 0 || scopeStart < 0 || readStart < 0 {
		t.Fatal("snapshot function boundaries not found")
	}
	for name, body := range map[string]string{
		"SnapshotSubject": text[subjectStart:scopeStart],
		"Snapshot":        text[scopeStart:readStart],
	} {
		if strings.Count(body, ".QueryRowContext(") != 1 || strings.Contains(body, "BeginTx(") {
			t.Fatalf("%s must retain one non-transactional snapshot query", name)
		}
	}
}

func TestMemoryPostgresListControlQueryShape(t *testing.T) {
	source, err := os.ReadFile("trusted_memory_store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	listStart := strings.Index(text, "func (s *MemoryStore) ListRecords")
	controlStart := strings.Index(text, "func (s *MemoryStore) listControlSnapshot")
	filtersStart := strings.Index(text, "func validMemoryListFilters")
	if listStart < 0 || controlStart < 0 || filtersStart < 0 {
		t.Fatal("list control function boundaries not found")
	}
	listBody := text[listStart:controlStart]
	controlBody := text[controlStart:filtersStart]
	if strings.Count(listBody, "s.listControlSnapshot(") != 1 || strings.Contains(listBody, "s.storeNow(") || strings.Contains(listBody, "s.readPolicy(") || strings.Contains(listBody, "readSubject(") {
		t.Fatal("ListRecords must use one combined control read")
	}
	if !strings.Contains(listBody, "sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}") {
		t.Fatal("ListRecords must retain a read-only repeatable-read transaction")
	}
	if strings.Count(controlBody, ".QueryRowContext(") != 1 || strings.Contains(controlBody, "BeginTx(") {
		t.Fatal("list control snapshot must retain exactly one query without a nested transaction")
	}
}

func TestMemoryPostgresBoundedExplainPlan(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	limits, _ := json.Marshal(memory.DefaultLimits())
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_policies VALUES('n','p',1,$1::jsonb)`, string(limits)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_subjects VALUES('n','p','target',1,false),('n','p','decoy',1,false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id) VALUES('n','p','target','c'),('n','p','decoy','c')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_records(namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,policy_generation,kind,content,topics,status,producer,policy_version,created_at,updated_at,expires_at)
		SELECT 'n','p',CASE WHEN g<=20 THEN 'target' ELSE 'decoy' END,'c','id-'||g,1,1,1,'preference','body',CASE WHEN g<=20 THEN '["wanted"]'::jsonb ELSE '["decoy"]'::jsonb END,'active','test','1',$1::timestamptz,$1::timestamptz+g*interval '1 microsecond',$1::timestamptz+interval '1 day' FROM generate_series(1,5020) g`, now); err != nil {
		t.Fatal(err)
	}
	var planRaw []byte
	if err := db.db.QueryRowContext(ctx, `EXPLAIN (ANALYZE,FORMAT JSON) SELECT r.memory_id,r.content FROM agent_runtime_memory_records r
		WHERE r.namespace='n' AND r.project_id='p' AND r.subject_id='target' AND r.collection_id='c' AND r.subject_generation=1 AND r.status='active' AND r.content<>'' AND r.expires_at>$1
		AND r.kind=ANY($2::text[]) AND r.topics ?| $3::text[]
		AND (r.source_ref='' OR EXISTS(SELECT 1 FROM agent_runtime_memory_sources s WHERE s.namespace=r.namespace AND s.project_id=r.project_id AND s.subject_id=r.subject_id AND s.collection_id=r.collection_id AND s.source_ref=r.source_ref AND NOT s.revoked AND s.source_epoch=r.source_epoch AND s.source_version=r.source_version AND s.expires_at>$1))
		ORDER BY r.updated_at DESC,r.memory_id DESC LIMIT 20`, now, []string{"preference"}, []string{"wanted"}).Scan(&planRaw); err != nil {
		t.Fatal(err)
	}
	var plan []map[string]any
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		t.Fatal(err)
	}
	root, ok := plan[0]["Plan"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected plan: %s", planRaw)
	}
	indexUsed, recordSeq := inspectPlan(root)
	if !indexUsed || recordSeq || root["Actual Rows"] != float64(20) {
		t.Fatalf("unbounded plan: %s", planRaw)
	}
}

func TestMemoryPostgresFiltersBeforeCandidateLimit(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	store := NewMemoryStore(db).SetClock(func() time.Time { return now })
	scope := memory.Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	allowed := rawCreateCommand(t, store, scope, "allowed-old", "wanted body")
	allowed.Topics = []string{"wanted"}
	if _, err := store.CommitMutation(context.Background(), allowed); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	for i := 0; i < 200; i++ {
		cmd := rawCreateCommand(t, store, scope, fmt.Sprintf("z-decoy-%03d", i), "newer disallowed")
		cmd.Kind = memory.KindUserStatement
		cmd.Topics = []string{"decoy"}
		if _, err := store.CommitMutation(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 200, CandidateLimit: 200, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes, Kinds: []memory.Kind{memory.KindPreference}, Topics: []string{"wanted"}})
	if err != nil || len(listed.Records) != 1 || listed.Records[0].MemoryID != "allowed-old" {
		t.Fatalf("filtered candidates=%#v err=%v", listed, err)
	}
	before := store.ContentReadCount()
	if _, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: 1024, Kinds: []memory.Kind{"invalid"}}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("invalid filter=%v", err)
	}
	if store.ContentReadCount() != before {
		t.Fatal("invalid filter materialized content")
	}
}

func inspectPlan(node map[string]any) (indexUsed, recordSeq bool) {
	name, _ := node["Relation Name"].(string)
	kind, _ := node["Node Type"].(string)
	indexName, _ := node["Index Name"].(string)
	if name == "agent_runtime_memory_records" && kind == "Seq Scan" {
		recordSeq = true
	}
	if name == "agent_runtime_memory_records" && (kind == "Index Scan" || kind == "Index Only Scan") && indexName == "idx_runtime_memory_records_visible" {
		indexUsed = true
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		mapped, _ := child.(map[string]any)
		i, s := inspectPlan(mapped)
		indexUsed = indexUsed || i
		recordSeq = recordSeq || s
	}
	return indexUsed, recordSeq
}

func truncateTrustedMemory(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.db.ExecContext(context.Background(), `TRUNCATE
		agent_runtime_memory_legacy_assignments, agent_runtime_memory_legacy_migrations,
		agent_runtime_memory_audits, agent_runtime_memory_receipts,
		agent_runtime_memory_lifecycle_jobs, agent_runtime_memory_purge_jobs, agent_runtime_memory_candidate_batches,
		agent_runtime_memory_records, agent_runtime_memory_sources,
		agent_runtime_memory_collections, agent_runtime_memory_subjects,
		agent_runtime_memory_policies CASCADE`)
	if err != nil {
		t.Fatalf("truncate trusted memory: %v", err)
	}
}
