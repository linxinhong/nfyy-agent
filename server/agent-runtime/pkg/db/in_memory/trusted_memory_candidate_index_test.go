package in_memory

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory/memorytest"
)

func TestCandidateIndexRemainsConsistentAcrossMutationContract(t *testing.T) {
	memorytest.Contract(t, candidateIndexHarness)
}

func TestCandidateIndexRemainsConsistentAcrossLifecycleContract(t *testing.T) {
	memorytest.LifecycleContract(t, candidateIndexHarness)
}

func candidateIndexHarness(t *testing.T, limits memory.Limits) memorytest.Harness {
	var clockMu sync.Mutex
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	store := NewMemoryStore().SetClock(clock).SetLimits(limits)
	checked := &candidateIndexCheckingStore{MemoryStore: store, t: t}
	return memorytest.Harness{
		Store: checked,
		Advance: func(delta time.Duration) {
			clockMu.Lock()
			now = now.Add(delta)
			clockMu.Unlock()
		},
		ContentReads:      store.ContentReadCount,
		LifecycleExamined: store.LifecycleExaminedCount,
		LifecycleAudits: func() []memory.AuditEvent {
			var result []memory.AuditEvent
			for _, audit := range store.Audits() {
				if audit.Action == memory.ActionPurge {
					result = append(result, audit)
				}
			}
			return result
		},
		ReceiptContentBytes: store.ReceiptContentBytes,
		FailAudit: func(fail bool) {
			if fail {
				store.SetAuditFailure(func(memory.AuditEvent) error { return fmt.Errorf("injected") })
			} else {
				store.SetAuditFailure(nil)
			}
		},
	}
}

type candidateIndexCheckingStore struct {
	*MemoryStore
	t *testing.T
}

func (s *candidateIndexCheckingStore) CommitMutation(ctx context.Context, cmd memory.Mutation) (memory.MutationResult, error) {
	result, err := s.MemoryStore.CommitMutation(ctx, cmd)
	s.check()
	return result, err
}

func (s *candidateIndexCheckingStore) ListRecords(ctx context.Context, req memory.ListRequest) (memory.ListResult, error) {
	result, err := s.MemoryStore.ListRecords(ctx, req)
	s.check()
	return result, err
}

func (s *candidateIndexCheckingStore) PurgeBatch(ctx context.Context, claim memory.PurgeJob, limit int) (memory.PurgeBatchResult, error) {
	result, err := s.MemoryStore.PurgeBatch(ctx, claim, limit)
	s.check()
	return result, err
}

func (s *candidateIndexCheckingStore) ReclaimLifecycle(ctx context.Context, limit int) (memory.LifecycleReclaimResult, error) {
	result, err := s.MemoryStore.ReclaimLifecycle(ctx, limit)
	s.check()
	return result, err
}

func (s *candidateIndexCheckingStore) check() {
	s.t.Helper()
	if err := s.MemoryStore.candidateIndexInvariant(); err != nil {
		s.t.Errorf("candidate index invariant: %v", err)
	}
}

func (s *MemoryStore) candidateIndexInvariant() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for scope, index := range s.candidateScopes {
		count, _, err := validateCandidateTreap(scope, index.all.root, s.candidateRecords)
		if err != nil {
			return fmt.Errorf("scope %v all tree: %w", scope, err)
		}
		if count != index.all.len || count != len(index.recordKeys) {
			return fmt.Errorf("scope %v all tree count=%d len=%d keys=%d", scope, count, index.all.len, len(index.recordKeys))
		}
		for topic, set := range index.byTopic {
			if _, _, err := validateCandidateTreap(scope, set.root, s.candidateRecords); err != nil {
				return fmt.Errorf("scope %v topic %q tree: %w", scope, topic, err)
			}
		}
	}
	for key, metadata := range s.candidateRecords {
		record := s.records[key]
		if record == nil || record.Status != memory.StatusActive || record.Content == "" {
			return fmt.Errorf("indexed record %v is absent or inactive", key)
		}
		if record.UpdatedAt != metadata.OrderKey.UpdatedAt || record.MemoryID != metadata.OrderKey.MemoryID || record.Kind != metadata.Kind || fmt.Sprint(uniqueMemoryTopics(record.Topics)) != fmt.Sprint(metadata.Topics) {
			return fmt.Errorf("indexed metadata for %v differs from record", key)
		}
		if record.Provenance.SourceRef != "" {
			source := s.sources[memorySourceKey{Scope: key.Scope, Ref: record.Provenance.SourceRef}]
			expectedDeadline := record.ExpiresAt
			if source.ExpiresAt.Before(expectedDeadline) {
				expectedDeadline = source.ExpiresAt
			}
			if !metadata.EligibleUntil.Equal(expectedDeadline) {
				return fmt.Errorf("indexed record %v has incorrect source deadline", key)
			}
		} else if !metadata.EligibleUntil.Equal(record.ExpiresAt) {
			return fmt.Errorf("indexed record %v has incorrect record deadline", key)
		}
		index := s.candidateScopes[key.Scope]
		if index == nil || index.generation != metadata.Generation {
			continue
		}
		if metadata.Source != nil && (index.sourceRefs[metadata.Source] <= 0 || index.sources[*metadata.Source] != metadata.Source) {
			return fmt.Errorf("record %v missing interned source reference", key)
		}
		if !index.all.contains(metadata.OrderKey, key.ID) {
			return fmt.Errorf("record %v missing all-candidate tree", key)
		}
		for _, topic := range metadata.Topics {
			if !index.byTopic[topic].contains(metadata.OrderKey, key.ID) {
				return fmt.Errorf("record %v missing topic candidate trees", key)
			}
		}
	}
	for key, record := range s.records {
		subject := s.subjects[key.Scope.Subject()]
		currentIndex := s.candidateScopes[key.Scope]
		shouldBeInCurrentIndex := record.Status == memory.StatusActive && record.Content != "" && !subject.Disabled && record.SubjectGeneration == generationOrInitial(subject.Generation) && currentIndex != nil && currentIndex.generation == record.SubjectGeneration
		metadata, indexed := s.candidateRecords[key]
		inCurrentIndex := indexed && currentIndex != nil && metadata.Generation == currentIndex.generation
		if shouldBeInCurrentIndex && !inCurrentIndex {
			return fmt.Errorf("record %v current-index eligibility=%v indexed=%v", key, shouldBeInCurrentIndex, inCurrentIndex)
		}
	}
	return nil
}

func validateCandidateTreap(scope memory.Scope, node *memoryCandidateTreapNode, records map[memoryRecordKey]memoryCandidateIndexRecord) (int, time.Time, error) {
	if node == nil {
		return 0, time.Time{}, nil
	}
	leftCount, leftMax, err := validateCandidateTreap(scope, node.left, records)
	if err != nil {
		return 0, time.Time{}, err
	}
	rightCount, rightMax, err := validateCandidateTreap(scope, node.right, records)
	if err != nil {
		return 0, time.Time{}, err
	}
	if node.left != nil && compareMemoryCandidateOrder(node.left.orderKey, node.orderKey) >= 0 {
		return 0, time.Time{}, fmt.Errorf("left child is not older")
	}
	if node.right != nil && compareMemoryCandidateOrder(node.right.orderKey, node.orderKey) <= 0 {
		return 0, time.Time{}, fmt.Errorf("right child is not newer")
	}
	key := memoryRecordKey{Scope: scope, ID: node.recordID}
	metadata, ok := records[key]
	if !ok || compareMemoryCandidateOrder(metadata.OrderKey, node.orderKey) != 0 || metadata.Kind != node.kind || metadata.Source != node.source || !metadata.EligibleUntil.Equal(node.eligibleUntil) {
		return 0, time.Time{}, fmt.Errorf("node %v differs from candidate metadata", key)
	}
	maximum := node.eligibleUntil
	if leftMax.After(maximum) {
		maximum = leftMax
	}
	if rightMax.After(maximum) {
		maximum = rightMax
	}
	if !node.maxEligibleUntil.Equal(maximum) {
		return 0, time.Time{}, fmt.Errorf("node %v max deadline=%s want=%s", key, node.maxEligibleUntil, maximum)
	}
	kinds := memoryCandidateKindBit(node.kind)
	uniformSource := true
	if node.left != nil {
		kinds |= node.left.kinds
		uniformSource = uniformSource && node.left.uniformSource && node.left.source == node.source
	}
	if node.right != nil {
		kinds |= node.right.kinds
		uniformSource = uniformSource && node.right.uniformSource && node.right.source == node.source
	}
	if node.kinds != kinds || node.uniformSource != uniformSource {
		return 0, time.Time{}, fmt.Errorf("node %v aggregate kinds/source mismatch", key)
	}
	return leftCount + rightCount + 1, maximum, nil
}

func (s *memoryCandidateOrderedSet) contains(orderKey memoryCandidateOrderKey, recordID string) bool {
	if s == nil {
		return false
	}
	for node := s.root; node != nil; {
		switch comparison := compareMemoryCandidateOrder(orderKey, node.orderKey); {
		case comparison < 0:
			node = node.left
		case comparison > 0:
			node = node.right
		default:
			return node.recordID == recordID
		}
	}
	return false
}

func TestListRecordsCandidateWorkIsBoundedByWindow(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	limits := memory.DefaultLimits()
	limits.MaxRecordsPerScope = 100_000
	limits.MaxContentPerScope = 100_000 * 768

	for _, size := range []int{1_000, 100_000} {
		t.Run(fmt.Sprintf("scope_%d", size), func(t *testing.T) {
			store := NewMemoryStore().SetClock(func() time.Time { return now }).SetLimits(limits)
			scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
			other := memory.Scope{Namespace: "other", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
			for i := 0; i < size; i++ {
				seedCandidateIndexRecord(store, memory.Record{
					Scope: scope, MemoryID: fmt.Sprintf("target-%06d", i), Revision: 1,
					SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference,
					Content: "coffee", Topics: []string{"drink"}, Status: memory.StatusActive,
					CreatedAt: now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour),
				})
			}
			for i := 0; i < 1_000; i++ {
				seedCandidateIndexRecord(store, memory.Record{
					Scope: other, MemoryID: fmt.Sprintf("other-%06d", i), Revision: 1,
					SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference,
					Content: "coffee", Topics: []string{"drink"}, Status: memory.StatusActive,
					CreatedAt: now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour),
				})
			}

			before := store.listExaminedCount()
			beforeReads := store.ContentReadCount()
			got, err := store.ListRecords(context.Background(), memory.ListRequest{
				Scope: scope, Limit: 200, CandidateLimit: 200, CandidateBytes: limits.MaxCandidateBytes,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Records) != 200 || !got.Truncated {
				t.Fatalf("records=%d truncated=%v, want 200/true", len(got.Records), got.Truncated)
			}
			if got.Records[0].MemoryID != "target-000000" || got.Records[199].MemoryID != "target-000199" {
				t.Fatalf("ordered IDs = %q ... %q", got.Records[0].MemoryID, got.Records[199].MemoryID)
			}
			if examined := store.listExaminedCount() - before; examined == 0 || examined > 20_000 {
				t.Fatalf("examined %d ordered-index nodes, want a bounded candidate-window traversal", examined)
			}
			if reads := store.ContentReadCount() - beforeReads; reads != 200 {
				t.Fatalf("decoded %d records, want 200", reads)
			}
		})
	}
}

func TestListRecordsPrunesLargeExpiredPrefix(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 30, 0, 0, time.UTC)
	limits := memory.DefaultLimits()
	limits.MaxRecordsPerScope = 100_000
	limits.MaxContentPerScope = 100_000 * 32
	store := NewMemoryStore().SetClock(func() time.Time { return now }).SetLimits(limits)
	scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: "expired-prefix", CollectionID: "profile"}
	for i := 0; i < 100_000; i++ {
		expiresAt := now
		if i == 99_999 {
			expiresAt = now.Add(time.Hour)
		}
		seedCandidateIndexRecord(store, memory.Record{
			Scope: scope, MemoryID: fmt.Sprintf("record-%06d", i), Revision: 1,
			SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference,
			Content: "coffee", Status: memory.StatusActive, CreatedAt: now,
			UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: expiresAt,
		})
	}
	before := store.listExaminedCount()
	got, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: limits.MaxCandidateBytes})
	if err != nil {
		t.Fatal(err)
	}
	if ids := candidateIndexRecordIDs(got.Records); fmt.Sprint(ids) != "[record-099999]" || got.Truncated {
		t.Fatalf("records=%v truncated=%v", ids, got.Truncated)
	}
	if examined := store.listExaminedCount() - before; examined == 0 || examined > 256 {
		t.Fatalf("examined %d ordered-index nodes across a 100k expired prefix", examined)
	}
}

func TestSourceAndGenerationInvalidationDoNotWalkHotScope(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 45, 0, 0, time.UTC)
	store := NewMemoryStore().SetClock(func() time.Time { return now })
	scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: "logical-invalidation", CollectionID: "profile"}
	sourceKey := memorySourceKey{Scope: scope, Ref: "source"}
	store.sources[sourceKey] = memorySourceState{Version: "v1", Epoch: 1, ExpiresAt: now.Add(time.Hour)}
	for i := 0; i < 10_000; i++ {
		seedCandidateIndexRecord(store, memory.Record{
			Scope: scope, MemoryID: fmt.Sprintf("source-%05d", i), Revision: 1,
			SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindVerifiedFact,
			Content: "verified", Topics: []string{"source"}, Status: memory.StatusActive,
			Provenance: memory.Provenance{SourceRef: "source", SourceVersion: "v1", SourceEpoch: 1},
			CreatedAt:  now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour),
		})
	}
	index := store.candidateScopes[scope]
	indexedRecords := len(store.candidateRecords)
	store.mu.Lock()
	source := store.sources[sourceKey]
	source.Epoch++
	source.Revoked = true
	store.sources[sourceKey] = source
	store.mu.Unlock()
	if store.candidateScopes[scope] != index || len(store.candidateRecords) != indexedRecords {
		t.Fatal("source invalidation rebuilt or walked the record index")
	}
	before := store.listExaminedCount()
	got, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 0 {
		t.Fatalf("revoked source returned %v", candidateIndexRecordIDs(got.Records))
	}
	if examined := store.listExaminedCount() - before; examined != 1 {
		t.Fatalf("revoked uniform-source subtree examined %d nodes, want 1", examined)
	}
	seedCandidateIndexRecord(store, memory.Record{Scope: scope, MemoryID: "unsourced", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "visible", Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)})
	before = store.listExaminedCount()
	got, err = store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes})
	if err != nil || fmt.Sprint(candidateIndexRecordIDs(got.Records)) != "[unsourced]" {
		t.Fatalf("mixed revoked/unsourced list = %v, %v", candidateIndexRecordIDs(got.Records), err)
	}
	if examined := store.listExaminedCount() - before; examined == 0 || examined > 256 {
		t.Fatalf("mixed revoked-source prefix examined %d nodes", examined)
	}
	indexedRecords = len(store.candidateRecords)

	store.mu.Lock()
	store.subjects[scope.Subject()] = memorySubjectState{Generation: 2, Disabled: true}
	store.mu.Unlock()
	if store.candidateScopes[scope] != index || len(store.candidateRecords) != indexedRecords {
		t.Fatal("forget generation fence rebuilt or walked the record index")
	}
	store.mu.Lock()
	store.subjects[scope.Subject()] = memorySubjectState{Generation: 3}
	store.mu.Unlock()
	got, err = store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes})
	if err != nil || len(got.Records) != 0 {
		t.Fatalf("old generation after enable = %v, %v", candidateIndexRecordIDs(got.Records), err)
	}
	seedCandidateIndexRecord(store, memory.Record{Scope: scope, MemoryID: "new-generation", Revision: 1, SubjectGeneration: 3, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "new", Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if store.candidateScopes[scope] == index || len(store.candidateScopes[scope].recordKeys) != 1 {
		t.Fatal("first new-generation write did not replace the visible scope index")
	}
	got, err = store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 1, CandidateLimit: 1, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes})
	if err != nil || fmt.Sprint(candidateIndexRecordIDs(got.Records)) != "[new-generation]" {
		t.Fatalf("new generation list = %v, %v", candidateIndexRecordIDs(got.Records), err)
	}
}

func TestCandidateIndexWorstCaseTopicNodeBound(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 50, 0, 0, time.UTC)
	store := NewMemoryStore().SetClock(func() time.Time { return now })
	scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: "topics", CollectionID: "profile"}
	topics := make([]string, 32)
	for i := range topics {
		topics[i] = fmt.Sprintf("topic-%02d", i)
	}
	seedCandidateIndexRecord(store, memory.Record{Scope: scope, MemoryID: "record", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "coffee", Topics: topics, Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)})
	index := store.candidateScopes[scope]
	nodes := index.all.len
	for _, set := range index.byTopic {
		nodes += set.len
	}
	if nodes != 33 {
		t.Fatalf("worst-case record uses %d ordered nodes, want all + 32 topic nodes", nodes)
	}
}

func TestListRecordsVisitBudgetBoundsInterleavedInvalidSources(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 55, 0, 0, time.UTC)
	store := NewMemoryStore().SetClock(func() time.Time { return now })
	scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: "interleaved-sources", CollectionID: "profile"}
	for source := 0; source < 2; source++ {
		ref := fmt.Sprintf("invalid-%d", source)
		store.sources[memorySourceKey{Scope: scope, Ref: ref}] = memorySourceState{Version: "v1", Epoch: 1, ExpiresAt: now.Add(time.Hour)}
	}
	for i := 0; i < 100_000; i++ {
		ref := fmt.Sprintf("invalid-%d", i&1)
		seedCandidateIndexRecord(store, memory.Record{
			Scope: scope, MemoryID: fmt.Sprintf("invalid-%06d", i), Revision: 1,
			SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference,
			Content: "invalid", Status: memory.StatusActive,
			Provenance: memory.Provenance{SourceRef: ref, SourceVersion: "v1", SourceEpoch: 1},
			CreatedAt:  now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour),
		})
	}
	store.mu.Lock()
	for source := 0; source < 2; source++ {
		key := memorySourceKey{Scope: scope, Ref: fmt.Sprintf("invalid-%d", source)}
		state := store.sources[key]
		state.Epoch++
		state.Revoked = true
		store.sources[key] = state
	}
	store.mu.Unlock()

	request := memory.ListRequest{Scope: scope, Limit: 10, CandidateLimit: 10, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes}
	beforeVisits := store.listExaminedCount()
	beforeReads := store.ContentReadCount()
	got, err := store.ListRecords(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 0 || !got.Truncated {
		t.Fatalf("all-invalid result records=%v truncated=%v", candidateIndexRecordIDs(got.Records), got.Truncated)
	}
	if visits := store.listExaminedCount() - beforeVisits; visits == 0 || visits > uint64(request.CandidateLimit*memoryCandidateVisitMultiplier) {
		t.Fatalf("all-invalid visits=%d budget=%d", visits, request.CandidateLimit*memoryCandidateVisitMultiplier)
	}
	if reads := store.ContentReadCount() - beforeReads; reads != 0 {
		t.Fatalf("all-invalid decoded=%d", reads)
	}

	for i := 0; i < 500; i++ {
		seedCandidateIndexRecord(store, memory.Record{
			Scope: scope, MemoryID: fmt.Sprintf("valid-%03d", i), Revision: 1,
			SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference,
			Content: "valid", Status: memory.StatusActive, CreatedAt: now,
			UpdatedAt: now.Add(time.Nanosecond - time.Duration(i*200)*time.Nanosecond), ExpiresAt: now.Add(time.Hour),
		})
	}
	beforeVisits = store.listExaminedCount()
	beforeReads = store.ContentReadCount()
	got, err = store.ListRecords(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) == 0 || len(got.Records) >= request.Limit || !got.Truncated {
		t.Fatalf("sparse-valid records=%v truncated=%v", candidateIndexRecordIDs(got.Records), got.Truncated)
	}
	assertCandidateRecordsOrdered(t, got.Records)
	for _, record := range got.Records {
		if !strings.HasPrefix(record.MemoryID, "valid-") {
			t.Fatalf("sparse-valid returned ineligible record %q", record.MemoryID)
		}
	}
	if visits := store.listExaminedCount() - beforeVisits; visits == 0 || visits > uint64(request.CandidateLimit*memoryCandidateVisitMultiplier) {
		t.Fatalf("sparse-valid visits=%d budget=%d", visits, request.CandidateLimit*memoryCandidateVisitMultiplier)
	}
	if reads := store.ContentReadCount() - beforeReads; reads != uint64(len(got.Records)) || reads > 200 {
		t.Fatalf("sparse-valid decoded=%d records=%d", reads, len(got.Records))
	}

	last := got.Records[len(got.Records)-1]
	request.AfterScope = scope
	request.AfterUpdatedAt = last.UpdatedAt
	request.AfterMemoryID = last.MemoryID
	page, err := store.ListRecords(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) == 0 || !page.Truncated {
		t.Fatalf("sparse-valid cursor page records=%v truncated=%v", candidateIndexRecordIDs(page.Records), page.Truncated)
	}
	assertCandidateRecordsOrdered(t, page.Records)
	for _, record := range page.Records {
		if !record.UpdatedAt.Before(last.UpdatedAt) && !(record.UpdatedAt.Equal(last.UpdatedAt) && record.MemoryID < last.MemoryID) {
			t.Fatalf("cursor returned non-older record %q", record.MemoryID)
		}
		if !strings.HasPrefix(record.MemoryID, "valid-") {
			t.Fatalf("cursor returned ineligible record %q", record.MemoryID)
		}
	}
}

func TestListRecordsSaturatesExtremePolicyCandidateLimit(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 58, 0, 0, time.UTC)
	limits := memory.DefaultLimits()
	limits.MaxRecallCandidates = math.MaxInt
	store := NewMemoryStore().SetClock(func() time.Time { return now }).SetLimits(limits)
	scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: "extreme-policy", CollectionID: "profile"}
	for i := 0; i < 300; i++ {
		seedCandidateIndexRecord(store, memory.Record{Scope: scope, MemoryID: fmt.Sprintf("record-%03d", i), Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "valid", Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour)})
	}
	beforeVisits := store.listExaminedCount()
	beforeReads := store.ContentReadCount()
	got, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: math.MaxInt, CandidateLimit: math.MaxInt, CandidateBytes: limits.MaxCandidateBytes})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != memoryCandidateMaterializationLimit || !got.Truncated {
		t.Fatalf("extreme policy records=%d truncated=%v", len(got.Records), got.Truncated)
	}
	if visits := store.listExaminedCount() - beforeVisits; visits == 0 || visits > uint64(memoryCandidateMaxVisitBudget) {
		t.Fatalf("extreme policy visits=%d max=%d", visits, memoryCandidateMaxVisitBudget)
	}
	if reads := store.ContentReadCount() - beforeReads; reads != uint64(memoryCandidateMaterializationLimit) {
		t.Fatalf("extreme policy decoded=%d want=%d", reads, memoryCandidateMaterializationLimit)
	}
}

func assertCandidateRecordsOrdered(t *testing.T, records []memory.Record) {
	t.Helper()
	for i := 1; i < len(records); i++ {
		previous, current := records[i-1], records[i]
		if current.UpdatedAt.After(previous.UpdatedAt) || (current.UpdatedAt.Equal(previous.UpdatedAt) && current.MemoryID > previous.MemoryID) {
			t.Fatalf("records out of order at %q then %q", previous.MemoryID, current.MemoryID)
		}
	}
}

func TestListRecordsCandidateIndexFiltersCursorExpiryAndSource(t *testing.T) {
	now := time.Date(2026, 9, 9, 1, 0, 0, 0, time.UTC)
	store := NewMemoryStore().SetClock(func() time.Time { return now })
	scope := memory.Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
	sourceKey := memorySourceKey{Scope: scope, Ref: "source"}
	store.sources[sourceKey] = memorySourceState{Version: "v1", Epoch: 1, ExpiresAt: now.Add(time.Hour)}
	store.sources[memorySourceKey{Scope: scope, Ref: "expired-source"}] = memorySourceState{Version: "v1", Epoch: 1, ExpiresAt: now}

	records := []memory.Record{
		{Scope: scope, MemoryID: "z-newest", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "z", Topics: []string{"coffee"}, Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{Scope: scope, MemoryID: "a-newest", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindUserStatement, Content: "a", Topics: []string{"coffee", "work"}, Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{Scope: scope, MemoryID: "older", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "old", Topics: []string{"work"}, Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)},
		{Scope: scope, MemoryID: "expired", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "expired", Topics: []string{"coffee"}, Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now.Add(time.Second), ExpiresAt: now},
		{Scope: scope, MemoryID: "bad-source", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "bad", Topics: []string{"coffee"}, Status: memory.StatusActive, Provenance: memory.Provenance{SourceRef: "source", SourceVersion: "wrong", SourceEpoch: 1}, CreatedAt: now, UpdatedAt: now.Add(2 * time.Second), ExpiresAt: now.Add(time.Hour)},
		{Scope: scope, MemoryID: "expired-source", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: "expired source", Topics: []string{"coffee"}, Status: memory.StatusActive, Provenance: memory.Provenance{SourceRef: "expired-source", SourceVersion: "v1", SourceEpoch: 1}, CreatedAt: now, UpdatedAt: now.Add(3 * time.Second), ExpiresAt: now.Add(time.Hour)},
	}
	for _, record := range records {
		seedCandidateIndexRecord(store, record)
	}

	got, err := store.ListRecords(context.Background(), memory.ListRequest{
		Scope: scope, Limit: 2, CandidateLimit: 2, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes,
		Kinds: []memory.Kind{memory.KindPreference, memory.KindUserStatement}, Topics: []string{"coffee", "work"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := candidateIndexRecordIDs(got.Records); fmt.Sprint(ids) != "[z-newest a-newest]" || !got.Truncated {
		t.Fatalf("first page IDs=%v truncated=%v", ids, got.Truncated)
	}

	page, err := store.ListRecords(context.Background(), memory.ListRequest{
		Scope: scope, Limit: 2, CandidateLimit: 2, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes,
		Kinds: []memory.Kind{memory.KindPreference, memory.KindUserStatement}, Topics: []string{"coffee", "work"},
		AfterScope: scope, AfterUpdatedAt: got.Records[1].UpdatedAt, AfterMemoryID: got.Records[1].MemoryID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := candidateIndexRecordIDs(page.Records); fmt.Sprint(ids) != "[older]" || page.Truncated {
		t.Fatalf("second page IDs=%v truncated=%v", ids, page.Truncated)
	}
}

func BenchmarkMemoryInMemoryBoundedListScale(b *testing.B) {
	now := time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC)
	limits := memory.DefaultLimits()
	limits.MaxRecordsPerScope = 100_000
	limits.MaxContentPerScope = 100_000 * 768
	store := NewMemoryStore().SetClock(func() time.Time { return now }).SetLimits(limits)

	for _, size := range []int{1_000, 10_000, 100_000} {
		scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: fmt.Sprintf("subject-%d", size), CollectionID: "profile"}
		for i := 0; i < size; i++ {
			seedCandidateIndexRecord(store, memory.Record{Scope: scope, MemoryID: fmt.Sprintf("target-%06d", i), Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: boundedListBody(i), Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour)})
		}
	}
	other := memory.Scope{Namespace: "other", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
	for i := 0; i < 1_000_000; i++ {
		seedCandidateIndexRecord(store, memory.Record{Scope: other, MemoryID: fmt.Sprintf("other-%07d", i), Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: boundedListBody(i), Status: memory.StatusActive, CreatedAt: now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour)})
	}

	for _, size := range []int{1_000, 10_000, 100_000} {
		scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: fmt.Sprintf("subject-%d", size), CollectionID: "profile"}
		b.Run(fmt.Sprintf("scope_%06d_other_1000000_candidates_200", size), func(b *testing.B) {
			b.ReportAllocs()
			before := store.listExaminedCount()
			beforeReads := store.ContentReadCount()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 200, CandidateLimit: 200, CandidateBytes: limits.MaxCandidateBytes})
				if err != nil {
					b.Fatal(err)
				}
				if len(got.Records) != 200 || !got.Truncated {
					b.Fatalf("records=%d truncated=%v", len(got.Records), got.Truncated)
				}
			}
			b.StopTimer()
			examined := store.listExaminedCount() - before
			b.ReportMetric(float64(examined)/float64(b.N), "indexnodes/op")
			if examined == 0 || examined > uint64(20_000*b.N) {
				b.Fatalf("examined=%d exceeds bounded traversal ceiling=%d", examined, 20_000*b.N)
			}
			if reads := store.ContentReadCount() - beforeReads; reads != uint64(200*b.N) {
				b.Fatalf("decoded=%d want=%d", reads, 200*b.N)
			}
		})
	}
}

func BenchmarkMemoryInMemoryWorstCaseTopicIndexScale(b *testing.B) {
	now := time.Date(2026, 9, 9, 2, 30, 0, 0, time.UTC)
	topics := make([]string, 32)
	for i := range topics {
		topics[i] = fmt.Sprintf("topic-%02d", i)
	}
	for _, size := range []int{1_000, 10_000, 100_000} {
		store := NewMemoryStore().SetClock(func() time.Time { return now })
		scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: fmt.Sprintf("topics-%d", size), CollectionID: "profile"}
		for i := 0; i < size; i++ {
			seedCandidateIndexRecord(store, memory.Record{
				Scope: scope, MemoryID: fmt.Sprintf("topic-record-%06d", i), Revision: 1,
				SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference,
				Content: "coffee", Topics: topics, Status: memory.StatusActive,
				CreatedAt: now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour),
			})
		}
		index := store.candidateScopes[scope]
		if index.all.len != size {
			b.Fatalf("all index len=%d want=%d", index.all.len, size)
		}
		topicNodes := 0
		for _, set := range index.byTopic {
			topicNodes += set.len
		}
		if topicNodes != 32*size {
			b.Fatalf("topic index nodes=%d want=%d", topicNodes, 32*size)
		}

		b.Run(fmt.Sprintf("scope_%06d_topics_32_list", size), func(b *testing.B) {
			b.ReportAllocs()
			before := store.listExaminedCount()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 200, CandidateLimit: 200, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes, Kinds: []memory.Kind{memory.KindPreference}, Topics: []string{"topic-31"}})
				if err != nil || len(got.Records) != 200 || !got.Truncated {
					b.Fatalf("records=%d truncated=%v err=%v", len(got.Records), got.Truncated, err)
				}
			}
			b.StopTimer()
			examined := store.listExaminedCount() - before
			b.ReportMetric(float64(examined)/float64(b.N), "indexnodes/op")
			b.ReportMetric(33, "treapnodes/record")
		})

		b.Run(fmt.Sprintf("scope_%06d_topics_32_reindex_write_lock", size), func(b *testing.B) {
			b.ReportAllocs()
			key := memoryRecordKey{Scope: scope, ID: "topic-record-000000"}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store.mu.Lock()
				current := store.records[key]
				store.deindexCandidateRecord(key)
				next := cloneTrustedRecord(current)
				next.Revision++
				next.UpdatedAt = now.Add(time.Duration(i+1) * time.Nanosecond)
				store.records[key] = next
				store.indexCandidateRecord(next)
				store.mu.Unlock()
			}
			b.StopTimer()
			b.ReportMetric(33, "treapnodes/record")
		})
	}
}

func BenchmarkMemoryInMemoryLogicalFenceLockScale(b *testing.B) {
	now := time.Date(2026, 9, 9, 3, 0, 0, 0, time.UTC)
	for _, size := range []int{1_000, 10_000, 100_000} {
		store := NewMemoryStore().SetClock(func() time.Time { return now })
		scope := memory.Scope{Namespace: "target", ProjectID: "project", SubjectID: fmt.Sprintf("source-%d", size), CollectionID: "profile"}
		sourceKey := memorySourceKey{Scope: scope, Ref: "source"}
		store.sources[sourceKey] = memorySourceState{Version: "v1", Epoch: 1, ExpiresAt: now.Add(time.Hour)}
		for i := 0; i < size; i++ {
			seedCandidateIndexRecord(store, memory.Record{
				Scope: scope, MemoryID: fmt.Sprintf("source-record-%06d", i), Revision: 1,
				SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindVerifiedFact,
				Content: "verified", Status: memory.StatusActive,
				Provenance: memory.Provenance{SourceRef: "source", SourceVersion: "v1", SourceEpoch: 1},
				CreatedAt:  now, UpdatedAt: now.Add(-time.Duration(i) * time.Nanosecond), ExpiresAt: now.Add(time.Hour),
			})
		}
		b.Run(fmt.Sprintf("scope_%06d_source_epoch_fence_lock", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store.mu.Lock()
				source := store.sources[sourceKey]
				source.Revoked = i&1 == 0
				store.sources[sourceKey] = source
				store.mu.Unlock()
			}
			b.StopTimer()
		})
	}
}

func seedCandidateIndexRecord(store *MemoryStore, record memory.Record) {
	key := memoryRecordKey{Scope: record.Scope, ID: record.MemoryID}
	copy := record
	copy.Topics = append([]string(nil), record.Topics...)
	store.records[key] = &copy
	store.indexCandidateRecord(&copy)
}

func candidateIndexRecordIDs(records []memory.Record) []string {
	ids := make([]string, len(records))
	for i := range records {
		ids[i] = records[i].MemoryID
	}
	return ids
}

func boundedListBody(i int) string {
	size := []int{256, 512, 512, 768}[i%4]
	prefix := fmt.Sprintf("coffee fixture %07d ", i)
	return prefix + strings.Repeat("x", size-len(prefix))
}
