package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory/memorytest"
)

var sqliteTestKey = []byte("0123456789abcdef0123456789abcdef")

func sqliteScope() memory.Scope {
	return memory.Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "subject", CollectionID: "private"}
}

func sqlitePrincipal(scope memory.Scope) memory.Principal {
	return memory.Principal{Subject: scope.SubjectID, Namespace: scope.Namespace, Issuer: "sqlite-test", AuthzEpoch: 1}
}

func sqliteModule(t *testing.T, store memory.AtomicStore, limits memory.Limits) *memory.Module {
	t.Helper()
	m, err := memory.NewModule(memory.ModuleConfig{
		Store: store,
		Authorizer: memory.AuthorizerFunc(func(_ context.Context, req memory.AuthorizeRequest) error {
			if req.Principal.Subject != req.Subject.SubjectID {
				return errors.New("wrong subject")
			}
			return nil
		}),
		Keys: map[string][]byte{"test": sqliteTestKey}, CurrentKey: "test", Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sqliteToken(t *testing.T, m *memory.Module, scope memory.Scope, action memory.Action) string {
	t.Helper()
	token, err := m.IssueOperationToken(context.Background(), sqlitePrincipal(scope), scope, action, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func sqliteCreate(t *testing.T, m *memory.Module, scope memory.Scope, content, token string) *memory.Record {
	t.Helper()
	if token == "" {
		token = sqliteToken(t, m, scope, memory.ActionCreate)
	}
	r, err := m.Create(context.Background(), sqlitePrincipal(scope), scope, memory.CreateRequest{Content: content, Kind: memory.KindPreference, OperationToken: token})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMemorySQLiteDurabilityAndCommitOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore(db)
	m := sqliteModule(t, store, memory.DefaultLimits())
	scope := sqliteScope()
	token := sqliteToken(t, m, scope, memory.ActionCreate)
	created := sqliteCreate(t, m, scope, "durable", token)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewWithOptions(path, Options{AutoMigrate: false})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedStore := NewMemoryStore(reopened)
	reopenedModule := sqliteModule(t, reopenedStore, memory.DefaultLimits())
	read, err := reopenedModule.Read(context.Background(), sqlitePrincipal(scope), scope, created.MemoryID)
	if err != nil || read.Revision != created.Revision || read.Content != "durable" {
		t.Fatalf("reopened read = %#v, %v", read, err)
	}
	replay, err := reopenedModule.Create(context.Background(), sqlitePrincipal(scope), scope, memory.CreateRequest{Content: "durable", Kind: memory.KindPreference, OperationToken: token})
	if err != nil || replay.MemoryID != created.MemoryID || replay.Revision != created.Revision || replay.Content != "" {
		t.Fatalf("durable receipt replay = %#v, %v", replay, err)
	}

	beforeToken := sqliteToken(t, reopenedModule, scope, memory.ActionCreate)
	reopenedStore.SetCommitFault(func(stage CommitStage) error {
		if stage == CommitBefore {
			return errors.New("before")
		}
		return nil
	})
	if _, err := reopenedModule.Create(context.Background(), sqlitePrincipal(scope), scope, memory.CreateRequest{Content: "rollback", Kind: memory.KindPreference, OperationToken: beforeToken}); err == nil {
		t.Fatal("before-commit fault succeeded")
	}
	reopenedStore.SetCommitFault(nil)
	rolledBack := sqliteCreate(t, reopenedModule, scope, "rollback", beforeToken)
	if rolledBack == nil {
		t.Fatal("retry after rollback did not commit")
	}

	afterToken := sqliteToken(t, reopenedModule, scope, memory.ActionCreate)
	reopenedStore.SetCommitFault(func(stage CommitStage) error {
		if stage == CommitAfter {
			return errors.New("response lost")
		}
		return nil
	})
	_, err = reopenedModule.Create(context.Background(), sqlitePrincipal(scope), scope, memory.CreateRequest{Content: "unknown", Kind: memory.KindPreference, OperationToken: afterToken})
	if !errors.Is(err, memory.ErrCommitUnknown) {
		t.Fatalf("after-commit fault = %v", err)
	}
	reopenedStore.SetCommitFault(nil)
	unknownReplay, err := reopenedModule.Create(context.Background(), sqlitePrincipal(scope), scope, memory.CreateRequest{Content: "unknown", Kind: memory.KindPreference, OperationToken: afterToken})
	if err != nil || unknownReplay == nil || unknownReplay.Content != "" {
		t.Fatalf("commit-unknown replay = %#v, %v", unknownReplay, err)
	}
	forgetToken, err := reopenedModule.IssueForgetToken(context.Background(), sqlitePrincipal(scope), scope.Subject(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if generation, err := reopenedModule.ForgetSubject(context.Background(), sqlitePrincipal(scope), scope.Subject(), forgetToken); err != nil || generation != 2 {
		t.Fatalf("forget = %d, %v", generation, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := NewWithOptions(path, Options{AutoMigrate: false})
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	snapshot, err := NewMemoryStore(third).SnapshotSubject(context.Background(), scope.Subject())
	if err != nil || snapshot.SubjectGeneration != 2 || !snapshot.SubjectDisabled {
		t.Fatalf("reopened subject guard = %#v, %v", snapshot, err)
	}
}

func TestMemorySQLiteIndependentConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	firstDB, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDB.Close()
	secondDB, err := NewWithOptions(path, Options{AutoMigrate: false})
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	limits := memory.DefaultLimits()
	first, second := sqliteModule(t, NewMemoryStore(firstDB), limits), sqliteModule(t, NewMemoryStore(secondDB), limits)
	scope := sqliteScope()
	created := sqliteCreate(t, first, scope, "v1", "")
	tokenA := sqliteToken(t, first, scope, memory.ActionRevise)
	tokenB := sqliteToken(t, second, scope, memory.ActionRevise)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var wins atomic.Int32
	for index, fixture := range []struct {
		module *memory.Module
		token  string
	}{{first, tokenA}, {second, tokenB}} {
		wg.Add(1)
		go func(index int, fixture struct {
			module *memory.Module
			token  string
		}) {
			defer wg.Done()
			<-start
			_, err := fixture.module.Revise(context.Background(), sqlitePrincipal(scope), scope, memory.ReviseRequest{MemoryID: created.MemoryID, ExpectedRevision: created.Revision, Content: fmt.Sprintf("v%d", index+2), OperationToken: fixture.token})
			if err == nil {
				wins.Add(1)
				return
			}
			if !errors.Is(err, memory.ErrConflict) && !errors.Is(err, memory.ErrOverloaded) {
				t.Errorf("revise error = %v", err)
			}
		}(index, fixture)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("CAS winners = %d", wins.Load())
	}
	current, err := first.Read(context.Background(), sqlitePrincipal(scope), scope, created.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	reviseToken := sqliteToken(t, first, scope, memory.ActionRevise)
	retractToken := sqliteToken(t, second, scope, memory.ActionRetract)
	wins.Store(0)
	start = make(chan struct{})
	for index, run := range []func() error{
		func() error {
			_, err := first.Revise(context.Background(), sqlitePrincipal(scope), scope, memory.ReviseRequest{MemoryID: current.MemoryID, ExpectedRevision: current.Revision, Content: "write-race", OperationToken: reviseToken})
			return err
		},
		func() error {
			_, err := second.RetractRecord(context.Background(), sqlitePrincipal(scope), scope, memory.RetractRequest{MemoryID: current.MemoryID, ExpectedRevision: current.Revision, OperationToken: retractToken})
			return err
		},
	} {
		wg.Add(1)
		go func(index int, run func() error) {
			defer wg.Done()
			<-start
			if err := run(); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, memory.ErrConflict) && !errors.Is(err, memory.ErrOverloaded) {
				t.Errorf("delete/write race %d = %v", index, err)
			}
		}(index, run)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("delete/write winners = %d", wins.Load())
	}

	quotaScope := scope
	quotaScope.ProjectID = "quota"
	quotaScope.CollectionID = "quota"
	quotaLimits := memory.DefaultLimits()
	quotaLimits.MaxRecordsPerScope = 1
	quotaFirstStore, quotaSecondStore := NewMemoryStore(firstDB).SetLimits(quotaLimits), NewMemoryStore(secondDB).SetLimits(quotaLimits)
	quotaFirst, quotaSecond := sqliteModule(t, quotaFirstStore, quotaLimits), sqliteModule(t, quotaSecondStore, quotaLimits)
	tokens := []string{sqliteToken(t, quotaFirst, quotaScope, memory.ActionCreate), sqliteToken(t, quotaSecond, quotaScope, memory.ActionCreate)}
	wins.Store(0)
	start = make(chan struct{})
	for index, module := range []*memory.Module{quotaFirst, quotaSecond} {
		wg.Add(1)
		go func(index int, module *memory.Module) {
			defer wg.Done()
			<-start
			_, err := module.Create(context.Background(), sqlitePrincipal(quotaScope), quotaScope, memory.CreateRequest{Content: fmt.Sprintf("q%d", index), Kind: memory.KindPreference, OperationToken: tokens[index]})
			if err == nil {
				wins.Add(1)
				return
			}
			if !errors.Is(err, memory.ErrQuota) && !errors.Is(err, memory.ErrOverloaded) {
				t.Errorf("quota error = %v", err)
			}
		}(index, module)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("quota winners = %d", wins.Load())
	}
	policyScope := scope
	policyScope.ProjectID = "durable-policy"
	strict := memory.DefaultLimits()
	strict.MaxRecordsPerScope = 1
	strictStore := NewMemoryStore(firstDB).SetLimits(strict)
	strictModule := sqliteModule(t, strictStore, strict)
	sqliteCreate(t, strictModule, policyScope, "strict", "")
	loose := memory.DefaultLimits()
	loose.MaxRecordsPerScope = 100
	looseStore := NewMemoryStore(secondDB).SetLimits(loose)
	looseModule := sqliteModule(t, looseStore, loose)
	if _, err := looseModule.Create(context.Background(), sqlitePrincipal(policyScope), policyScope, memory.CreateRequest{Content: "must fail durable quota", Kind: memory.KindPreference, OperationToken: sqliteToken(t, looseModule, policyScope, memory.ActionCreate)}); !errors.Is(err, memory.ErrQuota) {
		t.Fatalf("local limits overrode durable policy = %v", err)
	}
}

func TestMemorySQLiteProposalCommitFences(t *testing.T) {
	memorytest.ProposalFenceContract(t, func(t *testing.T) memorytest.ProposalFenceHarness {
		path := filepath.Join(t.TempDir(), "proposal-fence.db")
		first, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = first.Close() })
		second, err := NewWithOptions(path, Options{AutoMigrate: false})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = second.Close() })
		return memorytest.ProposalFenceHarness{
			Primary: NewMemoryStore(first), Concurrent: NewMemoryStore(second),
			State: func(ctx context.Context, scope memory.Scope) (state memorytest.ProposalFenceState, err error) {
				err = first.db.QueryRowContext(ctx, `SELECT
					(SELECT COUNT(*) FROM agent_runtime_memory_records WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND status=?),
					(SELECT COUNT(*) FROM agent_runtime_memory_audits WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND action=?),
					(SELECT COUNT(*) FROM agent_runtime_memory_receipts WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND action=?),
					(SELECT COUNT(*) FROM agent_runtime_memory_audits WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND action=?),
					(SELECT COUNT(*) FROM agent_runtime_memory_receipts WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND action=?),
					COALESCE((SELECT physical_records FROM agent_runtime_memory_collections WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?),0),
					COALESCE((SELECT physical_bytes FROM agent_runtime_memory_collections WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?),0),
					COALESCE((SELECT candidate_batches FROM agent_runtime_memory_collections WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?),0)`,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, memory.StatusCandidate,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, memory.ActionPropose,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, memory.ActionPropose,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, memory.ActionConfirm,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, memory.ActionConfirm,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID,
					scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID,
				).Scan(&state.CandidateRecords, &state.ProposalAudits, &state.ProposalReceipts, &state.ConfirmAudits, &state.ConfirmReceipts, &state.PhysicalRecords, &state.PhysicalBytes, &state.CandidateBatches)
				return state, err
			},
		}
	})
}

func TestMemorySQLiteConcurrentAutoMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrate-race.db")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			db, err := New(path)
			if err == nil {
				err = db.Close()
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent auto-migrate = %v", err)
		}
	}
	db, err := NewWithOptions(path, Options{AutoMigrate: false})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	if err := db.db.QueryRow(`SELECT MAX(version),COUNT(*) FROM agent_runtime_schema_migrations`).Scan(&version, &count); err != nil || version != 11 || count != 11 {
		t.Fatalf("migration ledger version=%d count=%d err=%v", version, count, err)
	}
}

func TestMemorySQLiteBusyIsFiniteAndRetryable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	firstDB, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDB.Close()
	secondDB, err := NewWithOptions(path, Options{AutoMigrate: false})
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	scope := sqliteScope()
	m := sqliteModule(t, NewMemoryStore(secondDB), memory.DefaultLimits())
	token := sqliteToken(t, m, scope, memory.ActionCreate)
	locker, err := firstDB.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = m.Create(context.Background(), sqlitePrincipal(scope), scope, memory.CreateRequest{Content: "busy", Kind: memory.KindPreference, OperationToken: token})
	if !errors.Is(err, memory.ErrOverloaded) {
		t.Fatalf("busy error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("busy retry exceeded finite bound: %s", elapsed)
	}
	if _, err := locker.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if got := sqliteCreate(t, m, scope, "busy", token); got == nil {
		t.Fatal("retry after busy did not commit")
	}
}

func TestMemorySQLiteManifestMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, legacy := range []*memory.UserMemory{
		{MemoryID: "mapped-a", Memory: "first", Topics: []string{"a"}, UserID: "untrusted-a", CreatedAt: now, UpdatedAt: now},
		{MemoryID: "mapped-b", Memory: "second", UserID: "untrusted-b", CreatedAt: now, UpdatedAt: now},
		{MemoryID: "unmapped", Memory: "must remain isolated", UserID: "untrusted-c", CreatedAt: now, UpdatedAt: now},
	} {
		if err := db.CreateMemory(context.Background(), legacy); err != nil {
			t.Fatal(err)
		}
	}
	store := NewMemoryStore(db).SetClock(func() time.Time { return now })
	scope := sqliteScope()
	other := scope
	other.CollectionID = "other"
	manifest := LegacyMemoryManifest{SourceName: LegacyMemorySource, CandidateRetention: 24 * time.Hour, Mappings: []LegacyMemoryMapping{{SourceMemoryID: "mapped-b", Scope: other}, {SourceMemoryID: "mapped-a", Scope: scope}}}
	dry, err := store.DryRunLegacyMemoryManifest(context.Background(), manifest)
	if err != nil || dry.Importable != 2 || dry.Total != 2 {
		t.Fatalf("dry run = %#v, %v", dry, err)
	}
	var manifests int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM agent_runtime_memory_migration_manifests`).Scan(&manifests); err != nil || manifests != 0 {
		t.Fatalf("dry run persisted state: count=%d err=%v", manifests, err)
	}
	first, err := store.ImportLegacyMemoryManifest(context.Background(), manifest, 1)
	if err != nil || first.Complete || first.Imported != 1 || first.NextEntry != 1 {
		t.Fatalf("first batch = %#v, %v", first, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewWithOptions(path, Options{AutoMigrate: false})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedStore := NewMemoryStore(reopened).SetClock(func() time.Time { return now })
	second, err := reopenedStore.ImportLegacyMemoryManifest(context.Background(), manifest, 1)
	if err != nil || !second.Complete || second.Imported != 2 || second.NextEntry != 2 {
		t.Fatalf("resumed batch = %#v, %v", second, err)
	}
	repeated, err := reopenedStore.ImportLegacyMemoryManifest(context.Background(), manifest, 100)
	if err != nil || !repeated.Complete || repeated.Imported != 2 {
		t.Fatalf("repeated import = %#v, %v", repeated, err)
	}
	var legacyCount, trustedCount, candidateCount int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&legacyCount); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*), SUM(status=?) FROM agent_runtime_memory_records`, memory.StatusCandidate).Scan(&trustedCount, &candidateCount); err != nil {
		t.Fatal(err)
	}
	if legacyCount != 3 || trustedCount != 2 || candidateCount != 2 {
		t.Fatalf("migration preservation legacy=%d trusted=%d candidates=%d", legacyCount, trustedCount, candidateCount)
	}
	list, err := reopenedStore.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 20, CandidateLimit: 200, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes})
	if err != nil || len(list.Records) != 0 {
		t.Fatalf("candidate or unmapped legacy became visible = %#v, %v", list, err)
	}
	conflict := manifest
	conflict.Mappings = []LegacyMemoryMapping{{SourceMemoryID: "mapped-a", Scope: other}}
	if _, err := reopenedStore.ImportLegacyMemoryManifest(context.Background(), conflict, 1); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("conflicting reassignment = %v", err)
	}
	invalid := manifest
	invalid.Mappings = []LegacyMemoryMapping{{SourceMemoryID: "mapped-a", Scope: memory.Scope{}}}
	if _, err := reopenedStore.DryRunLegacyMemoryManifest(context.Background(), invalid); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("incomplete scope = %v", err)
	}
	missing := manifest
	missing.Mappings = []LegacyMemoryMapping{{SourceMemoryID: "missing", Scope: scope}}
	if _, err := reopenedStore.ImportLegacyMemoryManifest(context.Background(), missing, 1); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("missing manifest row = %v", err)
	}
	atomicLegacy := &memory.UserMemory{MemoryID: "a-atomic-valid", Memory: "atomic", CreatedAt: now, UpdatedAt: now}
	if err := reopened.CreateMemory(context.Background(), atomicLegacy); err != nil {
		t.Fatal(err)
	}
	atomicManifest := LegacyMemoryManifest{SourceName: LegacyMemorySource, CandidateRetention: time.Hour, Mappings: []LegacyMemoryMapping{{SourceMemoryID: "a-atomic-valid", Scope: scope}, {SourceMemoryID: "z-atomic-missing", Scope: scope}}}
	atomicReport, err := reopenedStore.ImportLegacyMemoryManifest(context.Background(), atomicManifest, 2)
	if !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("atomic migration failure = %#v, %v", atomicReport, err)
	}
	var partial int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM agent_runtime_memory_migration_manifests WHERE manifest_hash=?`, atomicReport.ManifestHash).Scan(&partial); err != nil || partial != 0 {
		t.Fatalf("failed batch persisted manifest/progress: count=%d err=%v", partial, err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM agent_runtime_memory_records WHERE memory_id=?`, importedLegacyID(atomicReport.ManifestHash, "a-atomic-valid")).Scan(&partial); err != nil || partial != 0 {
		t.Fatalf("failed batch persisted candidate: count=%d err=%v", partial, err)
	}
	strict := memory.DefaultLimits()
	strict.MaxRecordsPerScope = 1
	strictStore := NewMemoryStore(reopened).SetClock(func() time.Time { return now }).SetLimits(strict)
	if err := reopened.CreateMemory(context.Background(), &memory.UserMemory{MemoryID: "b-atomic-valid", Memory: "atomic-two", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	strictScope := scope
	strictScope.ProjectID = "migration-quota"
	quotaManifest := LegacyMemoryManifest{SourceName: LegacyMemorySource, CandidateRetention: time.Hour, Mappings: []LegacyMemoryMapping{{SourceMemoryID: "a-atomic-valid", Scope: strictScope}, {SourceMemoryID: "b-atomic-valid", Scope: strictScope}}}
	quotaDryRun, err := strictStore.DryRunLegacyMemoryManifest(context.Background(), quotaManifest)
	if err != nil || quotaDryRun.Importable != 1 || quotaDryRun.OverQuota != 1 {
		t.Fatalf("quota dry run = %#v, %v", quotaDryRun, err)
	}
}

func TestMemorySQLiteManifestEntryBound(t *testing.T) {
	makeManifest := func(count int) LegacyMemoryManifest {
		mappings := make([]LegacyMemoryMapping, count)
		for index := range mappings {
			mappings[index] = LegacyMemoryMapping{SourceMemoryID: fmt.Sprintf("legacy-%05d", index), Scope: sqliteScope()}
		}
		return LegacyMemoryManifest{SourceName: LegacyMemorySource, CandidateRetention: time.Hour, Mappings: mappings}
	}
	for _, count := range []int{MaxLegacyMemoryManifestEntries - 1, MaxLegacyMemoryManifestEntries} {
		if _, _, _, err := normalizeLegacyManifest(makeManifest(count), memory.DefaultLimits()); err != nil {
			t.Fatalf("manifest count %d = %v", count, err)
		}
	}
	if _, _, _, err := normalizeLegacyManifest(makeManifest(MaxLegacyMemoryManifestEntries+1), memory.DefaultLimits()); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("over-limit manifest = %v", err)
	}
}

func TestMemorySQLiteScopeQueryPlanAndCandidateBound(t *testing.T) {
	db := newAdapterDB(t)
	store := NewMemoryStore(db)
	m := sqliteModule(t, store, memory.DefaultLimits())
	scope := sqliteScope()
	for index := 0; index < 201; index++ {
		sqliteCreate(t, m, scope, fmt.Sprintf("record-%03d", index), "")
	}
	page, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 200, CandidateLimit: 200, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes})
	if err != nil || len(page.Records) != 200 || !page.Truncated {
		t.Fatalf("internal candidate bound = %d truncated=%v err=%v", len(page.Records), page.Truncated, err)
	}
	filterScope := scope
	filterScope.CollectionID = "filters"
	allowedToken := sqliteToken(t, m, filterScope, memory.ActionCreate)
	if _, err := m.Create(context.Background(), sqlitePrincipal(filterScope), filterScope, memory.CreateRequest{Content: "old allowed", Kind: memory.KindUserStatement, Topics: []string{"allowed"}, OperationToken: allowedToken}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 201; index++ {
		token := sqliteToken(t, m, filterScope, memory.ActionCreate)
		if _, err := m.Create(context.Background(), sqlitePrincipal(filterScope), filterScope, memory.CreateRequest{Content: fmt.Sprintf("new wrong kind %03d", index), Kind: memory.KindPreference, Topics: []string{"allowed"}, OperationToken: token}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 201; index++ {
		token := sqliteToken(t, m, filterScope, memory.ActionCreate)
		if _, err := m.Create(context.Background(), sqlitePrincipal(filterScope), filterScope, memory.CreateRequest{Content: fmt.Sprintf("new wrong topic %03d", index), Kind: memory.KindUserStatement, Topics: []string{"wrong"}, OperationToken: token}); err != nil {
			t.Fatal(err)
		}
	}
	before := store.ContentReadCount()
	filtered, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: filterScope, Limit: 200, CandidateLimit: 200, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes, Kinds: []memory.Kind{memory.KindUserStatement}, Topics: []string{"allowed"}})
	if err != nil || len(filtered.Records) != 1 || filtered.Records[0].Content != "old allowed" {
		t.Fatalf("filters before candidate limit = %#v, %v", filtered, err)
	}
	if reads := store.ContentReadCount() - before; reads != 1 {
		t.Fatalf("filtered body reads = %d", reads)
	}
	rows, err := db.db.Query(`EXPLAIN QUERY PLAN SELECT memory_id FROM agent_runtime_memory_records INDEXED BY idx_memory_records_scope_visible_keyset WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND subject_generation=? AND status=? AND expires_at_ns>? ORDER BY updated_at_ns DESC,memory_id DESC LIMIT ?`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, 1, memory.StatusActive, 0, 200)
	if err != nil {
		t.Fatal(err)
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
	if !strings.Contains(plan, "idx_memory_records_scope_visible_keyset") || strings.Contains(plan, "SCAN agent_runtime_memory_records") {
		t.Fatalf("unexpected query plan: %s", plan)
	}
	topicRows, err := db.db.Query(`EXPLAIN QUERY PLAN SELECT memory_id FROM agent_runtime_memory_records INDEXED BY idx_memory_records_scope_visible_keyset WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND subject_generation=? AND status=? AND kind IN (?) AND EXISTS (SELECT 1 FROM agent_runtime_memory_record_topics mt WHERE mt.namespace=agent_runtime_memory_records.namespace AND mt.project_id=agent_runtime_memory_records.project_id AND mt.subject_id=agent_runtime_memory_records.subject_id AND mt.collection_id=agent_runtime_memory_records.collection_id AND mt.memory_id=agent_runtime_memory_records.memory_id AND mt.topic IN (?)) ORDER BY updated_at_ns DESC,memory_id DESC LIMIT ?`, filterScope.Namespace, filterScope.ProjectID, filterScope.SubjectID, filterScope.CollectionID, 1, memory.StatusActive, memory.KindUserStatement, "allowed", 200)
	if err != nil {
		t.Fatal(err)
	}
	defer topicRows.Close()
	details = details[:0]
	for topicRows.Next() {
		var id, parent, unused int
		var detail string
		if err := topicRows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	topicPlan := strings.Join(details, "\n")
	if !strings.Contains(topicPlan, "agent_runtime_memory_record_topics") || !strings.Contains(topicPlan, "USING COVERING INDEX") {
		t.Fatalf("topic filter plan is not indexed: %s", topicPlan)
	}
}
