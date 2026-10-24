package memorytest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

// LifecycleContract is shared by the reference, SQLite and PostgreSQL
// adapters. It deliberately observes metadata and authoritative reads only;
// no content is copied into job or lag evidence.
func LifecycleContract(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("forget_is_enqueued_and_fenced_across_collections", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		one := baseScope()
		two := one
		two.CollectionID = "other-collection"
		foreign := one
		foreign.SubjectID = "other-subject"
		directCreate(t, h.Store, one, "old-a", "old-a")
		directCreate(t, h.Store, two, "old-b", "old-b")
		directCreate(t, h.Store, foreign, "foreign", "foreign")
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		forget, err := m.IssueForgetToken(context.Background(), principal(one), one.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if generation, err := m.ForgetSubject(context.Background(), principal(one), one.Subject(), forget); err != nil || generation != 2 {
			t.Fatalf("forget generation=%d err=%v", generation, err)
		}
		lag, err := life.PurgeLag(context.Background())
		if err != nil || lag.PendingJobs != 1 || lag.FailedJobs != 0 || lag.ObjectiveMissed {
			t.Fatalf("lag after forget=%#v err=%v", lag, err)
		}
		claim, err := life.ClaimNextPurge(context.Background(), "worker-a", time.Minute)
		if err != nil || claim.Kind != memory.PurgeJobForget || claim.PurgeBeforeGeneration != 1 {
			t.Fatalf("forget claim=%#v err=%v", claim, err)
		}
		enableToken, err := m.IssueEnableSubjectToken(context.Background(), principal(one), one.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if generation, err := m.EnableSubject(context.Background(), principal(one), one.Subject(), enableToken); err != nil || generation != 3 {
			t.Fatalf("enable generation=%d err=%v", generation, err)
		}
		directCreate(t, h.Store, one, "new", "new")
		batch, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch)
		if err != nil || batch.ProcessedTargets != 3 || batch.PurgedRecords != 2 || !batch.Completed {
			t.Fatalf("forget purge=%#v err=%v", batch, err)
		}
		if got, _, err := h.Store.ReadRecord(context.Background(), one, "new"); err != nil || got == nil || got.Content != "new" || got.SubjectGeneration != 3 {
			t.Fatalf("new generation record=%#v err=%v", got, err)
		}
		if got, _, err := h.Store.ReadRecord(context.Background(), foreign, "foreign"); err != nil || got == nil || got.Content != "foreign" {
			t.Fatalf("foreign record=%#v err=%v", got, err)
		}
		audits := h.LifecycleAudits()
		if len(audits) == 0 {
			t.Fatal("physical purge produced no metadata-only audit")
		}
		for _, audit := range audits {
			if audit.OperationID == "" || audit.Action != memory.ActionPurge || audit.Actor == "" || audit.Issuer != "memory-lifecycle" || audit.Subject != one.Subject() || audit.Generation != 1 || audit.MemoryID != "" {
				t.Fatalf("invalid lifecycle audit=%#v", audit)
			}
		}
	})

	t.Run("subject_enable_requires_subject_authorization_and_fresh_binding", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		forget, err := m.IssueForgetToken(context.Background(), p, scope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.ForgetSubject(context.Background(), p, scope.Subject(), forget); err != nil {
			t.Fatal(err)
		}

		counting := &countingAtomicStore{AtomicStore: h.Store}
		denied, err := memory.NewModule(memory.ModuleConfig{
			Store: counting,
			Authorizer: memory.AuthorizerFunc(func(_ context.Context, req memory.AuthorizeRequest) error {
				if req.Action == memory.ActionEnableSubject {
					return errors.New("subject enable denied")
				}
				return nil
			}),
			Keys: map[string][]byte{"old": testKeyOld}, CurrentKey: "old", Limits: memory.DefaultLimits(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := denied.IssueEnableSubjectToken(context.Background(), p, scope.Subject(), time.Hour); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("denied enable issue=%v", err)
		}
		if calls := counting.calls.Load(); calls != 0 {
			t.Fatalf("unauthorized enable reached store %d times", calls)
		}
		if _, err := m.IssueOperationToken(context.Background(), p, scope, memory.ActionEnableSubject, time.Hour); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("collection-scoped enable token=%v", err)
		}

		enable, err := m.IssueEnableSubjectToken(context.Background(), p, scope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		wrongAction, err := m.IssueForgetToken(context.Background(), p, scope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.EnableSubject(context.Background(), p, scope.Subject(), wrongAction); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("wrong action enable=%v", err)
		}
		wrongNamespace := p
		wrongNamespace.Namespace = "other-tenant"
		if _, err := m.EnableSubject(context.Background(), wrongNamespace, scope.Subject(), enable); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("wrong namespace enable=%v", err)
		}

		// A second Forget advances the authoritative subject generation. The
		// earlier, correctly signed enable token must not cross that fence.
		if _, err := m.ForgetSubject(context.Background(), p, scope.Subject(), wrongAction); err != nil {
			t.Fatal(err)
		}
		if _, err := m.EnableSubject(context.Background(), p, scope.Subject(), enable); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("stale generation enable=%v", err)
		}
		fresh, err := m.IssueEnableSubjectToken(context.Background(), p, scope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if generation, err := m.EnableSubject(context.Background(), p, scope.Subject(), fresh); err != nil || generation != 4 {
			t.Fatalf("fresh enable generation=%d err=%v", generation, err)
		}
		if generation, err := m.EnableSubject(context.Background(), p, scope.Subject(), fresh); err != nil || generation != 4 {
			t.Fatalf("enable replay generation=%d err=%v", generation, err)
		}
	})

	t.Run("lease_loss_cancel_failure_and_reclaim", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		created := directCreate(t, h.Store, scope, "retract-me", "body")
		record, _, err := h.Store.ReadRecord(context.Background(), scope, created.MemoryID)
		if err != nil || record == nil {
			t.Fatalf("created record=%#v err=%v", record, err)
		}
		retractDirect(t, h.Store, scope, record.MemoryID, record.Revision)
		first, err := life.ClaimNextPurge(context.Background(), "worker-a", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := life.PurgeBatch(canceled, first, 1); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled purge=%v", err)
		}
		if err := life.FailPurge(context.Background(), first, "temporary_failure"); err != nil {
			t.Fatal(err)
		}
		second, err := life.ClaimNextPurge(context.Background(), "worker-b", time.Second)
		if err != nil || second.TaskID != first.TaskID || second.LeaseEpoch <= first.LeaseEpoch {
			t.Fatalf("failed reclaim=%#v err=%v", second, err)
		}
		h.Advance(time.Second)
		third, err := life.ClaimNextPurge(context.Background(), "worker-c", time.Second)
		if err != nil || third.TaskID != first.TaskID || third.LeaseEpoch <= second.LeaseEpoch {
			t.Fatalf("expired reclaim=%#v err=%v", third, err)
		}
		if _, err := life.PurgeBatch(context.Background(), second, 1); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("lost owner purge=%v", err)
		}
		batch, err := life.PurgeBatch(context.Background(), third, 1)
		if err != nil || batch.PurgedRecords != 1 {
			t.Fatalf("reclaimed purge=%#v err=%v", batch, err)
		}
		if !batch.Completed {
			batch, err = life.PurgeBatch(context.Background(), batch.Job, 1)
		}
		if err != nil || !batch.Completed || batch.ProcessedTargets != 0 {
			t.Fatalf("reclaimed completion=%#v err=%v", batch, err)
		}
	})

	t.Run("lease_precision_is_shared_and_fenced", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		created := directCreate(t, h.Store, scope, "lease-precision", "body")
		record, _, err := h.Store.ReadRecord(context.Background(), scope, created.MemoryID)
		if err != nil || record == nil {
			t.Fatalf("created record=%#v err=%v", record, err)
		}
		retractDirect(t, h.Store, scope, record.MemoryID, record.Revision)
		if _, err := life.ClaimNextPurge(context.Background(), "precision-worker", time.Microsecond-time.Nanosecond); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("sub-microsecond claim=%v", err)
		}
		claim, err := life.ClaimNextPurge(context.Background(), "precision-worker", time.Microsecond+500*time.Nanosecond)
		if err != nil {
			t.Fatal(err)
		}
		if claim.LeaseExpiresAt.Nanosecond()%int(time.Microsecond) != 0 {
			t.Fatalf("non-canonical lease expiry=%s", claim.LeaseExpiresAt)
		}
		if _, err := life.RenewPurgeLease(context.Background(), claim, time.Microsecond-time.Nanosecond); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("sub-microsecond renewal=%v", err)
		}
		tampered := claim
		tampered.LeaseExpiresAt = tampered.LeaseExpiresAt.Add(time.Microsecond)
		if _, err := life.PurgeBatch(context.Background(), tampered, 1); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("tampered microsecond fence=%v", err)
		}
		renewed, err := life.RenewPurgeLease(context.Background(), claim, time.Microsecond+500*time.Nanosecond)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := life.PurgeBatch(context.Background(), renewed, 1); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("purge_audit_failure_rolls_back_body_quota_and_cursor", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		created := directCreate(t, h.Store, scope, "audit-failure", "body")
		record, _, err := h.Store.ReadRecord(context.Background(), scope, created.MemoryID)
		if err != nil || record == nil {
			t.Fatal(err)
		}
		retractDirect(t, h.Store, scope, record.MemoryID, record.Revision)
		claim, err := life.ClaimNextPurge(context.Background(), "audit-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		h.FailAudit(true)
		if _, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch); !errors.Is(err, memory.ErrAudit) {
			t.Fatalf("purge audit failure=%v", err)
		}
		if audits := h.LifecycleAudits(); len(audits) != 0 {
			t.Fatalf("failed purge audits=%#v", audits)
		}
		h.FailAudit(false)
		batch, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch)
		if err != nil || batch.PurgedRecords != 1 || batch.ReleasedBytes != len("body") || !batch.Completed {
			t.Fatalf("purge after audit recovery=%#v err=%v", batch, err)
		}
	})

	t.Run("expiry_discovery_and_lag_boundary", func(t *testing.T) {
		limits := memory.DefaultLimits()
		limits.DefaultRetention = time.Hour
		h := factory(t, limits)
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		directCreate(t, h.Store, scope, "expires", "body")
		if n, err := life.DiscoverExpired(context.Background(), memory.MaxPurgeBatch); err != nil || n != 0 {
			t.Fatalf("early discovery=%d err=%v", n, err)
		}
		h.Advance(time.Hour)
		if n, err := life.DiscoverExpired(context.Background(), memory.MaxPurgeBatch); err != nil || n != 1 {
			t.Fatalf("expiry discovery=%d err=%v", n, err)
		}
		if n, err := life.DiscoverExpired(context.Background(), memory.MaxPurgeBatch); err != nil || n != 0 {
			t.Fatalf("expiry dedupe=%d err=%v", n, err)
		}
		h.Advance(memory.PurgeLagObjective - time.Nanosecond)
		lag, err := life.PurgeLag(context.Background())
		if err != nil || lag.ObjectiveMissed {
			t.Fatalf("lag before boundary=%#v err=%v", lag, err)
		}
		h.Advance(time.Nanosecond)
		lag, err = life.PurgeLag(context.Background())
		if err != nil || !lag.ObjectiveMissed || lag.OldestLag != memory.PurgeLagObjective {
			t.Fatalf("lag at boundary=%#v err=%v", lag, err)
		}
	})

	t.Run("source_retraction_enqueues_fixed_epoch_cleanup", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		created := directCandidateBatch(t, h.Store, scope, memory.CandidateDraft{MemoryID: "source-candidate", Kind: memory.KindPreference, Content: "body", Retention: time.Hour})
		if len(created.Records) != 1 {
			t.Fatalf("candidate result=%#v", created)
		}
		record := created.Records[0]
		retractSource(t, h.Store, scope, record.Provenance.SourceRef, record.Provenance.SourceEpoch)
		claim, err := life.ClaimNextPurge(context.Background(), "source-worker", time.Minute)
		if err != nil || claim.Kind != memory.PurgeJobRetractSource || claim.SourceEpoch != record.Provenance.SourceEpoch {
			t.Fatalf("source claim=%#v err=%v", claim, err)
		}
		batch, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch)
		if err != nil || batch.PurgedRecords != 1 || !batch.Completed {
			t.Fatalf("source purge=%#v err=%v", batch, err)
		}
	})

	t.Run("source_epoch_advancement_enqueues_each_superseded_epoch", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		oldOne := directAttestedLifecycleCreate(t, h.Store, scope, "epoch-1", "source-history", 1)
		oldThree := directAttestedLifecycleCreate(t, h.Store, scope, "epoch-3", "source-history", 3)
		current := directAttestedLifecycleCreate(t, h.Store, scope, "epoch-4", "source-history", 4)

		seen := map[uint64]bool{}
		for range 2 {
			claim, err := life.ClaimNextPurge(context.Background(), "source-history-worker", time.Minute)
			if err != nil || claim.Kind != memory.PurgeJobRetractSource {
				t.Fatalf("source history claim=%#v err=%v", claim, err)
			}
			seen[claim.SourceEpoch] = true
			batch, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch)
			if err != nil || batch.PurgedRecords != 1 || !batch.Completed {
				t.Fatalf("source history purge=%#v err=%v", batch, err)
			}
		}
		if !seen[1] || !seen[3] {
			t.Fatalf("superseded source epochs=%v want 1 and 3", seen)
		}
		for _, old := range []memory.MutationResult{oldOne, oldThree} {
			if got, _, err := h.Store.ReadRecord(context.Background(), scope, old.Record.MemoryID); err != nil || got != nil {
				t.Fatalf("superseded record remained visible: record=%#v err=%v", got, err)
			}
		}
		if got, _, err := h.Store.ReadRecord(context.Background(), scope, current.Record.MemoryID); err != nil || got == nil || got.Provenance.SourceEpoch != 4 {
			t.Fatalf("current source record=%#v err=%v", got, err)
		}
	})

	t.Run("reclaim_waits_for_retention_receipts_and_completed_job", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		created := directCreate(t, h.Store, scope, "reclaim-old", "body")
		record, _, err := h.Store.ReadRecord(context.Background(), scope, created.MemoryID)
		if err != nil || record == nil {
			t.Fatalf("created record=%#v err=%v", record, err)
		}
		retractDirect(t, h.Store, scope, record.MemoryID, record.Revision)
		claim, err := life.ClaimNextPurge(context.Background(), "reclaim-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if batch, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch); err != nil || !batch.Completed || batch.PurgedRecords != 1 {
			t.Fatalf("initial purge=%#v err=%v", batch, err)
		}
		if result, err := life.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch); err != nil || result.ReclaimedRecords != 0 || result.ReclaimedJobs != 0 {
			t.Fatalf("early reclaim=%#v err=%v", result, err)
		}

		h.Advance(24 * time.Hour)
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		p := principal(scope)
		blockingToken := issue(t, m, p, scope, memory.ActionCreate, 7*24*time.Hour)
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "receipt blocker", Kind: memory.KindPreference, OperationToken: blockingToken}); err != nil {
			t.Fatal(err)
		}
		h.Advance(6 * 24 * time.Hour)
		if result, err := life.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch); err != nil || result.ReclaimedRecords != 0 || result.ReclaimedJobs != 0 {
			t.Fatalf("unexpired receipt reclaim=%#v err=%v", result, err)
		}
		h.Advance(24 * time.Hour)
		result, err := life.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch)
		if err != nil || result.ReclaimedRecords != 1 || result.ReclaimedJobs != 1 || result.ReleasedControls != 1 {
			t.Fatalf("eligible reclaim=%#v err=%v", result, err)
		}
	})

	t.Run("reclaim_releases_sourced_record_but_preserves_source_and_candidate_fences", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		verified := directAttestedLifecycleCreate(t, h.Store, scope, "verified-old", "verified-source", 1)
		retractSource(t, h.Store, scope, verified.Record.Provenance.SourceRef, 1)
		claim, err := life.ClaimNextPurge(context.Background(), "verified-reclaim-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if batch, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch); err != nil || !batch.Completed || batch.PurgedRecords != 1 {
			t.Fatalf("verified purge=%#v err=%v", batch, err)
		}
		h.Advance(memory.LifecycleRetention)
		result, err := life.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch)
		if err != nil || result.ReclaimedRecords != 1 || result.ReclaimedJobs != 1 {
			t.Fatalf("verified reclaim=%#v err=%v", result, err)
		}
		snapshot, err := h.Store.Snapshot(context.Background(), scope, "verified-source")
		if err != nil || snapshot.Source == nil || !snapshot.Source.Revoked || snapshot.Source.Epoch != 2 {
			t.Fatalf("source fence=%#v err=%v", snapshot.Source, err)
		}

		candidateScope := scope
		candidateScope.CollectionID = "candidate-fence"
		candidate := directCandidateBatch(t, h.Store, candidateScope, memory.CandidateDraft{MemoryID: "candidate-old", Kind: memory.KindPreference, Content: "body", Retention: time.Hour})
		retractSource(t, h.Store, candidateScope, candidate.Records[0].Provenance.SourceRef, 1)
		claim, err = life.ClaimNextPurge(context.Background(), "candidate-reclaim-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if batch, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch); err != nil || !batch.Completed || batch.PurgedRecords != 1 {
			t.Fatalf("candidate purge=%#v err=%v", batch, err)
		}
		h.Advance(memory.LifecycleRetention)
		result, err = life.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch)
		if err != nil || result.ReclaimedRecords != 0 || result.ReclaimedJobs != 1 {
			t.Fatalf("candidate reclaim=%#v err=%v", result, err)
		}
		if _, err := tryDirectCreate(t, h.Store, candidateScope, "candidate-old", "replacement"); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("candidate tombstone reused=%v", err)
		}
	})

	t.Run("source_backed_reclaim_releases_identity_quota_for_new_epoch", func(t *testing.T) {
		limits := memory.DefaultLimits()
		limits.MaxControlsPerScope = 2 // one source row plus one record identity
		h := factory(t, limits)
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		first := directAttestedLifecycleCreate(t, h.Store, scope, "quota-epoch-1", "quota-source", 1)
		retractSource(t, h.Store, scope, first.Record.Provenance.SourceRef, 1)
		claim, err := life.ClaimNextPurge(context.Background(), "quota-reclaim-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if batch, err := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch); err != nil || !batch.Completed || batch.PurgedRecords != 1 {
			t.Fatalf("quota purge=%#v err=%v", batch, err)
		}
		h.Advance(memory.LifecycleRetention)
		if result, err := life.ReclaimLifecycle(context.Background(), memory.MaxPurgeBatch); err != nil || result.ReleasedControls != 1 {
			t.Fatalf("quota reclaim=%#v err=%v", result, err)
		}
		if result := directAttestedLifecycleCreate(t, h.Store, scope, "quota-epoch-3", "quota-source", 3); result.Record == nil || result.Record.Provenance.SourceEpoch != 3 {
			t.Fatalf("new source epoch after reclaim=%#v", result)
		}
	})

	t.Run("worker_round_counts_processed_targets", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		for i := 0; i < 7; i++ {
			directCreate(t, h.Store, scope, fmt.Sprintf("id-%02d", i), "body")
		}
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		token, err := m.IssueForgetToken(context.Background(), principal(scope), scope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.ForgetSubject(context.Background(), principal(scope), scope.Subject(), token); err != nil {
			t.Fatal(err)
		}
		worker, err := memory.NewLifecycleWorker(memory.LifecycleWorkerConfig{Store: life, OwnerToken: "bounded-worker", LeaseDuration: time.Minute, BatchSize: 3, RoundLimit: 5, RoundTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		result, err := worker.RunOnce(context.Background())
		if err != nil || result.ProcessedTargets != 5 || result.PurgedRecords != 5 || result.Completed {
			t.Fatalf("bounded round=%#v err=%v", result, err)
		}
	})

	t.Run("worker_round_shares_budget_between_reclaim_and_purge", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		old := directCreate(t, h.Store, scope, "old-reclaim", "body")
		oldRecord, _, err := h.Store.ReadRecord(context.Background(), scope, old.MemoryID)
		if err != nil || oldRecord == nil {
			t.Fatal(err)
		}
		retractDirect(t, h.Store, scope, oldRecord.MemoryID, oldRecord.Revision)
		oldJob, err := life.ClaimNextPurge(context.Background(), "budget-prep", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if batch, err := life.PurgeBatch(context.Background(), oldJob, memory.MaxPurgeBatch); err != nil || !batch.Completed {
			t.Fatalf("budget prep purge=%#v err=%v", batch, err)
		}
		h.Advance(memory.LifecycleRetention)
		purgeScope := scope
		purgeScope.SubjectID = "budget-purge-subject"
		for i := 0; i < 4; i++ {
			directCreate(t, h.Store, purgeScope, fmt.Sprintf("budget-%d", i), "body")
		}
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		forget, err := m.IssueForgetToken(context.Background(), principal(purgeScope), purgeScope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.ForgetSubject(context.Background(), principal(purgeScope), purgeScope.Subject(), forget); err != nil {
			t.Fatal(err)
		}
		worker, err := memory.NewLifecycleWorker(memory.LifecycleWorkerConfig{Store: life, OwnerToken: "shared-budget", LeaseDuration: time.Minute, BatchSize: 5, RoundLimit: 5, RoundTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		result, err := worker.RunOnce(context.Background())
		if err != nil || result.ExaminedControls != 2 || result.ProcessedTargets != 3 || result.ExaminedControls+result.ProcessedTargets != 5 || result.PurgedRecords != 3 {
			t.Fatalf("shared budget result=%#v err=%v", result, err)
		}
	})

	t.Run("stale_prefix_is_processed_once_with_physical_keyset_bound", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		for i := 0; i < 200; i++ {
			directCreate(t, h.Store, scope, fmt.Sprintf("a-%03d", i), "unrelated")
		}
		created := directCreate(t, h.Store, scope, "z-target", "target")
		record, _, err := h.Store.ReadRecord(context.Background(), scope, created.MemoryID)
		if err != nil || record == nil {
			t.Fatal(err)
		}
		retractDirect(t, h.Store, scope, record.MemoryID, record.Revision)
		claim, err := life.ClaimNextPurge(context.Background(), "keyset-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		processed, purged := 0, 0
		for {
			batch, batchErr := life.PurgeBatch(context.Background(), claim, memory.MaxPurgeBatch)
			if batchErr != nil {
				t.Fatal(batchErr)
			}
			if batch.ProcessedTargets > memory.MaxPurgeBatch {
				t.Fatalf("processed batch=%d", batch.ProcessedTargets)
			}
			processed += batch.ProcessedTargets
			purged += batch.PurgedRecords
			claim = batch.Job
			if batch.Completed {
				break
			}
		}
		if processed != 201 || purged != 1 {
			t.Fatalf("processed=%d purged=%d", processed, purged)
		}
	})

	t.Run("target_selection_does_not_scan_foreign_subjects", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		if h.LifecycleExamined == nil {
			t.Skip("adapter exposes query-plan evidence instead of an examined counter")
		}
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		created := directCreate(t, h.Store, scope, "local", "body")
		record, _, err := h.Store.ReadRecord(context.Background(), scope, created.MemoryID)
		if err != nil || record == nil {
			t.Fatal(err)
		}
		retractDirect(t, h.Store, scope, record.MemoryID, record.Revision)
		for i := 0; i < 100; i++ {
			foreign := scope
			foreign.SubjectID = fmt.Sprintf("foreign-%03d", i)
			for j := 0; j < 10; j++ {
				directCreate(t, h.Store, foreign, fmt.Sprintf("id-%02d", j), "body")
			}
		}
		claim, err := life.ClaimNextPurge(context.Background(), "indexed-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		before := h.LifecycleExamined()
		batch, err := life.PurgeBatch(context.Background(), claim, 1)
		if err != nil {
			t.Fatalf("indexed purge=%#v err=%v", batch, err)
		}
		if examined := h.LifecycleExamined() - before; examined != 1 {
			t.Fatalf("examined=%d want 1; foreign tenants must not be scanned", examined)
		}
	})

	t.Run("all_lifecycle_entries_obey_context_and_renew_fence", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		life := lifecycleStore(t, h.Store)
		scope := baseScope()
		created := directCreate(t, h.Store, scope, "entry", "body")
		record, _, err := h.Store.ReadRecord(context.Background(), scope, created.MemoryID)
		if err != nil || record == nil {
			t.Fatal(err)
		}
		retractDirect(t, h.Store, scope, record.MemoryID, record.Revision)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := life.DiscoverExpired(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := life.PurgeLag(ctx); err != nil {
			t.Fatal(err)
		}
		claim, err := life.ClaimNextPurge(ctx, "entry-worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		renewed, err := life.RenewPurgeLease(ctx, claim, 2*time.Minute)
		if err != nil || !renewed.LeaseExpiresAt.After(claim.LeaseExpiresAt) {
			t.Fatalf("renewed=%#v err=%v", renewed, err)
		}
		if _, err := life.PurgeBatch(ctx, claim, 1); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("pre-renew claim remained valid: %v", err)
		}
		if err := life.FailPurge(ctx, renewed, "retryable"); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatalf("lifecycle entries exceeded deadline: %v", ctx.Err())
		}
	})
}

func lifecycleStore(t *testing.T, store memory.AtomicStore) memory.LifecycleStore {
	t.Helper()
	lifecycle, ok := store.(memory.LifecycleStore)
	if !ok {
		t.Fatalf("%T does not implement memory.LifecycleStore", store)
	}
	return lifecycle
}

func directAttestedLifecycleCreate(t *testing.T, store memory.AtomicStore, scope memory.Scope, id, sourceRef string, epoch uint64) memory.MutationResult {
	t.Helper()
	snapshot, err := store.Snapshot(context.Background(), scope, sourceRef)
	if err != nil {
		t.Fatal(err)
	}
	opID := fmt.Sprintf("attested-lifecycle:%d", operationSequence.Add(1))
	attestation := &memory.SourceAttestation{
		Scope:       scope,
		SourceRef:   sourceRef,
		Version:     fmt.Sprintf("v%d", epoch),
		Digest:      fmt.Sprintf("digest-%d", epoch),
		SourceEpoch: epoch,
		ExpiresAt:   snapshot.StoreNow.Add(time.Hour),
		Verifier:    "contract-host",
	}
	cmd := memory.Mutation{
		Type:        memory.MutationAttestCreate,
		Scope:       scope,
		Subject:     scope.Subject(),
		MemoryID:    id,
		Kind:        memory.KindVerifiedFact,
		Content:     "verified body",
		Retention:   time.Hour,
		Attestation: attestation,
		Operation: memory.Operation{
			ID: opID, KeyID: "test", Action: memory.ActionCreateVerifiedFact, Scope: scope, Subject: scope.Subject(),
			SubjectGeneration: snapshot.SubjectGeneration, IssuedAt: snapshot.StoreNow, ExpiresAt: snapshot.StoreNow.Add(time.Hour), CommandDigest: []byte(opID),
		},
		Preconditions: memory.Preconditions{SubjectGeneration: snapshot.SubjectGeneration, PolicyGeneration: snapshot.PolicyGeneration, SourceEpoch: snapshot.SourceEpoch},
		Audit:         memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionCreateVerifiedFact, Actor: scope.SubjectID, Issuer: "contract-host", MemoryID: id},
	}
	result, err := store.CommitMutation(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
