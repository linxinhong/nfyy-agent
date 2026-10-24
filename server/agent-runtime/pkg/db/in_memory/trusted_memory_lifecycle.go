package in_memory

import (
	"context"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

const maxExpiryScanPerCall = 1000

func (s *MemoryStore) enqueueLifecycle(job memory.PurgeJob) {
	if _, exists := s.lifecycleJobs[job.TaskID]; exists {
		return
	}
	job.Status = memory.PurgeJobPending
	s.lifecycleJobs[job.TaskID] = job
}

func (s *MemoryStore) enqueueReclaimRecord(key memoryRecordKey, taskID string) {
	if _, exists := s.reclaimRecordIndex[key]; exists {
		return
	}
	s.enqueueReclaimEntry(memoryLifecycleReclaimEntry{RecordKey: key, TaskID: taskID})
	s.reclaimJobRefs[taskID]++
}

func (s *MemoryStore) enqueueReclaimJob(taskID string) {
	if _, exists := s.reclaimJobIndex[taskID]; exists {
		return
	}
	s.enqueueReclaimEntry(memoryLifecycleReclaimEntry{TaskID: taskID, JobOnly: true})
}

func (s *MemoryStore) enqueueReclaimEntry(entry memoryLifecycleReclaimEntry) {
	index := len(s.reclaimQueue)
	if len(s.reclaimQueueFree) > 0 {
		index = s.reclaimQueueFree[len(s.reclaimQueueFree)-1]
		s.reclaimQueueFree = s.reclaimQueueFree[:len(s.reclaimQueueFree)-1]
		s.reclaimQueue[index] = entry
	} else {
		s.reclaimQueue = append(s.reclaimQueue, entry)
	}
	if entry.JobOnly {
		s.reclaimJobIndex[entry.TaskID] = index
	} else {
		s.reclaimRecordIndex[entry.RecordKey] = index
	}
}

func (s *MemoryStore) DiscoverExpired(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > memory.MaxPurgeBatch {
		return 0, memory.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(s.recordOrder) == 0 {
		return 0, nil
	}
	now := s.storeNowLocked()
	created, inspected := 0, 0
	for inspected < maxExpiryScanPerCall && inspected < len(s.recordOrder) && created < limit {
		index := s.expiryScanOffset % len(s.recordOrder)
		s.expiryScanOffset = (index + 1) % len(s.recordOrder)
		inspected++
		key := s.recordOrder[index]
		record := s.records[key]
		if record == nil || record.Content == "" || record.ExpiresAt.After(now) {
			continue
		}
		id := memory.LifecycleTaskID(memory.PurgeJobExpiry, record.Scope.Subject(), record.Scope, "", record.MemoryID, record.Revision, record.SubjectGeneration)
		if _, exists := s.lifecycleJobs[id]; exists {
			continue
		}
		s.enqueueLifecycle(memory.PurgeJob{TaskID: id, Kind: memory.PurgeJobExpiry, Subject: record.Scope.Subject(), Scope: record.Scope, MemoryID: record.MemoryID, ExpectedRevision: record.Revision, PurgeBeforeGeneration: record.SubjectGeneration, SelectionCutoff: record.ExpiresAt, CreatedAt: now})
		created++
	}
	return created, nil
}

func (s *MemoryStore) ClaimNextPurge(ctx context.Context, owner string, leaseDuration time.Duration) (memory.PurgeJob, error) {
	if owner == "" || len(owner) > 256 || !utf8.ValidString(owner) {
		return memory.PurgeJob{}, memory.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memory.PurgeJob{}, err
	}
	now := s.storeNowLocked()
	deadline, err := memory.PurgeLeaseDeadline(now, leaseDuration)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	eligible := make([]memory.PurgeJob, 0)
	for _, job := range s.lifecycleJobs {
		if job.Status == memory.PurgeJobCompleted || (job.Status == memory.PurgeJobLeased && job.LeaseExpiresAt.After(now)) {
			continue
		}
		eligible = append(eligible, job)
	}
	if len(eligible) == 0 {
		return memory.PurgeJob{}, memory.ErrNotFound
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].CreatedAt.Equal(eligible[j].CreatedAt) {
			return eligible[i].TaskID < eligible[j].TaskID
		}
		return eligible[i].CreatedAt.Before(eligible[j].CreatedAt)
	})
	job := eligible[0]
	job.Status = memory.PurgeJobLeased
	job.OwnerToken = owner
	job.LeaseEpoch++
	if job.LeaseEpoch == 0 {
		job.LeaseEpoch = 1
	}
	job.LeaseExpiresAt = deadline
	job.LastFailureCode = ""
	s.lifecycleJobs[job.TaskID] = job
	return job, nil
}

func (s *MemoryStore) RenewPurgeLease(ctx context.Context, claim memory.PurgeJob, leaseDuration time.Duration) (memory.PurgeJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memory.PurgeJob{}, err
	}
	now := s.storeNowLocked()
	deadline, err := memory.PurgeLeaseDeadline(now, leaseDuration)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	job, err := s.validateLifecycleClaim(claim, now)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	job.LeaseExpiresAt = deadline
	s.lifecycleJobs[job.TaskID] = job
	return job, nil
}

func (s *MemoryStore) PurgeBatch(ctx context.Context, claim memory.PurgeJob, limit int) (memory.PurgeBatchResult, error) {
	if limit <= 0 || limit > memory.MaxPurgeBatch {
		return memory.PurgeBatchResult{}, memory.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memory.PurgeBatchResult{}, err
	}
	now := s.storeNowLocked()
	job, err := s.validateLifecycleClaim(claim, now)
	if err != nil {
		return memory.PurgeBatchResult{}, err
	}
	targets := s.lifecycleTargets(job, limit)
	result := memory.PurgeBatchResult{Job: job, StoreNow: now}
	// A full physical page is conservatively treated as possibly truncated;
	// the next zero-target call completes it without examining >limit rows.
	hasMore := len(targets) == limit
	auditJob := job
	eligible := 0
	for _, key := range targets {
		auditJob.CursorCollectionID = key.Scope.CollectionID
		auditJob.CursorMemoryID = key.ID
		record := s.records[key]
		if record != nil && record.Content != "" && s.lifecycleEligible(job, record) {
			eligible++
		}
	}
	var purgeAudit *memory.AuditEvent
	if eligible > 0 {
		audit := memory.AuditEvent{OperationID: memory.LifecycleAuditID(auditJob, len(targets)), Scope: job.Scope, Subject: job.Subject, Action: memory.ActionPurge, Actor: job.OwnerToken, Issuer: "memory-lifecycle", Generation: job.PurgeBeforeGeneration, OccurredAt: now}
		if s.auditFailure != nil {
			if err := s.auditFailure(audit); err != nil {
				return memory.PurgeBatchResult{}, fmt.Errorf("%w: %v", memory.ErrAudit, err)
			}
		}
		purgeAudit = &audit
	}
	for _, key := range targets {
		record := s.records[key]
		result.ProcessedTargets++
		job.CursorCollectionID = key.Scope.CollectionID
		job.CursorMemoryID = key.ID
		if record == nil || record.Content == "" || !s.lifecycleEligible(job, record) {
			continue
		}
		collection := s.collections[record.Scope]
		collection.Records--
		collection.Bytes -= len(record.Content)
		s.collections[record.Scope] = collection
		result.PurgedRecords++
		result.ReleasedBytes += len(record.Content)
		s.deindexCandidateRecord(key)
		next := cloneTrustedRecord(record)
		next.Content = ""
		next.Topics = nil
		next.UpdatedAt = now
		s.records[key] = next
		if next.Status != memory.StatusCandidate {
			s.enqueueReclaimRecord(key, job.TaskID)
		}
	}
	if purgeAudit != nil {
		s.audits = append(s.audits, *purgeAudit)
	}
	if !hasMore {
		job.Status = memory.PurgeJobCompleted
		job.CompletedAt = now
		job.OwnerToken = ""
		job.LeaseExpiresAt = time.Time{}
		result.Completed = true
		s.enqueueReclaimJob(job.TaskID)
	}
	s.lifecycleJobs[job.TaskID] = job
	result.Job = job
	return result, nil
}

func (s *MemoryStore) ReclaimLifecycle(ctx context.Context, limit int) (memory.LifecycleReclaimResult, error) {
	if limit <= 0 || limit > memory.MaxPurgeBatch {
		return memory.LifecycleReclaimResult{}, memory.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memory.LifecycleReclaimResult{}, err
	}
	now := s.storeNowLocked()
	result := memory.LifecycleReclaimResult{StoreNow: now}
	if len(s.reclaimQueue) == 0 {
		return result, nil
	}
	physical := len(s.reclaimQueue)
	for result.ExaminedControls < limit && result.ExaminedControls < physical {
		index := s.reclaimScanOffset % physical
		s.reclaimScanOffset = (index + 1) % physical
		entry := s.reclaimQueue[index]
		result.ExaminedControls++
		if entry == (memoryLifecycleReclaimEntry{}) {
			continue
		}
		if entry.JobOnly {
			job, ok := s.lifecycleJobs[entry.TaskID]
			if !ok || job.Status != memory.PurgeJobCompleted || s.reclaimJobRefs[entry.TaskID] != 0 {
				continue
			}
			delete(s.lifecycleJobs, entry.TaskID)
			delete(s.reclaimJobIndex, entry.TaskID)
			s.clearReclaimEntry(index)
			result.ReclaimedJobs++
			continue
		}
		record := s.records[entry.RecordKey]
		if record == nil || record.Content != "" || record.Status == memory.StatusCandidate || record.UpdatedAt.Add(memory.LifecycleRetention).After(now) {
			continue
		}
		if job, exists := s.lifecycleJobs[entry.TaskID]; exists && job.Status != memory.PurgeJobCompleted {
			continue
		}
		receiptScope := memoryReceiptScope{Subject: record.Scope.Subject(), Scope: record.Scope}
		s.cleanupReceipts(receiptScope, now)
		if len(s.receiptIndex[receiptScope]) != 0 {
			continue
		}
		s.removeLifecycleRecord(entry.RecordKey)
		if refs := s.reclaimJobRefs[entry.TaskID]; refs <= 1 {
			delete(s.reclaimJobRefs, entry.TaskID)
		} else {
			s.reclaimJobRefs[entry.TaskID] = refs - 1
		}
		delete(s.reclaimRecordIndex, entry.RecordKey)
		s.clearReclaimEntry(index)
		result.ReclaimedRecords++
		result.ReleasedControls++
	}
	if len(s.reclaimRecordIndex) == 0 && len(s.reclaimJobIndex) == 0 {
		s.reclaimQueue = nil
		s.reclaimQueueFree = nil
		s.reclaimScanOffset = 0
	}
	return result, nil
}

func (s *MemoryStore) clearReclaimEntry(index int) {
	s.reclaimQueue[index] = memoryLifecycleReclaimEntry{}
	s.reclaimQueueFree = append(s.reclaimQueueFree, index)
}

func (s *MemoryStore) indexRecordOrder(key memoryRecordKey) {
	if _, exists := s.recordOrderIndex[key]; exists {
		return
	}
	index := len(s.recordOrder)
	if len(s.recordOrderFree) > 0 {
		index = s.recordOrderFree[len(s.recordOrderFree)-1]
		s.recordOrderFree = s.recordOrderFree[:len(s.recordOrderFree)-1]
		s.recordOrder[index] = key
	} else {
		s.recordOrder = append(s.recordOrder, key)
	}
	s.recordOrderIndex[key] = index
}

func (s *MemoryStore) removeLifecycleRecord(key memoryRecordKey) {
	s.deindexCandidateRecord(key)
	delete(s.records, key)
	ids := s.scopeRecordIDs[key.Scope]
	if index := sort.SearchStrings(ids, key.ID); index < len(ids) && ids[index] == key.ID {
		copy(ids[index:], ids[index+1:])
		ids = ids[:len(ids)-1]
		s.scopeRecordIDs[key.Scope] = ids
	}
	if len(ids) == 0 {
		delete(s.scopeRecordIDs, key.Scope)
		collections := s.subjectCollections[key.Scope.Subject()]
		if index := sort.SearchStrings(collections, key.Scope.CollectionID); index < len(collections) && collections[index] == key.Scope.CollectionID {
			copy(collections[index:], collections[index+1:])
			collections = collections[:len(collections)-1]
			s.subjectCollections[key.Scope.Subject()] = collections
		}
		if len(collections) == 0 {
			delete(s.subjectCollections, key.Scope.Subject())
		}
	}
	if index, ok := s.recordOrderIndex[key]; ok {
		s.recordOrder[index] = memoryRecordKey{}
		delete(s.recordOrderIndex, key)
		s.recordOrderFree = append(s.recordOrderFree, index)
	}
	collection := s.collections[key.Scope]
	if collection.RecordIdentities > 0 {
		collection.RecordIdentities--
	}
	s.collections[key.Scope] = collection
}

func (s *MemoryStore) FailPurge(ctx context.Context, claim memory.PurgeJob, failureCode string) error {
	if failureCode == "" || len(failureCode) > memory.MaxPurgeFailureCode || !utf8.ValidString(failureCode) {
		return memory.ErrInvalid
	}
	for _, r := range failureCode {
		if r < 0x20 || r == 0x7f {
			return memory.ErrInvalid
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	job, err := s.validateLifecycleClaim(claim, s.storeNowLocked())
	if err != nil {
		return err
	}
	job.Status = memory.PurgeJobFailed
	job.LastFailureCode = failureCode
	job.OwnerToken = ""
	job.LeaseExpiresAt = time.Time{}
	s.lifecycleJobs[job.TaskID] = job
	return nil
}

func (s *MemoryStore) PurgeLag(ctx context.Context) (memory.PurgeLagSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return memory.PurgeLagSnapshot{}, err
	}
	now := s.storeNowLocked()
	result := memory.PurgeLagSnapshot{StoreNow: now}
	for _, job := range s.lifecycleJobs {
		if job.Status == memory.PurgeJobCompleted {
			continue
		}
		result.PendingJobs++
		if job.Status == memory.PurgeJobFailed {
			result.FailedJobs++
		}
		if result.OldestCreatedAt.IsZero() || job.CreatedAt.Before(result.OldestCreatedAt) {
			result.OldestCreatedAt = job.CreatedAt
		}
	}
	if !result.OldestCreatedAt.IsZero() {
		result.OldestLag = now.Sub(result.OldestCreatedAt)
		if result.OldestLag < 0 {
			result.OldestLag = 0
		}
		result.ObjectiveMissed = result.OldestLag >= memory.PurgeLagObjective
	}
	return result, nil
}

func (s *MemoryStore) validateLifecycleClaim(provided memory.PurgeJob, now time.Time) (memory.PurgeJob, error) {
	current, ok := s.lifecycleJobs[provided.TaskID]
	if !ok || current.Status != memory.PurgeJobLeased || current.OwnerToken != provided.OwnerToken || current.LeaseEpoch != provided.LeaseEpoch || !current.LeaseExpiresAt.Equal(provided.LeaseExpiresAt) || !current.LeaseExpiresAt.After(now) || current.CursorCollectionID != provided.CursorCollectionID || current.CursorMemoryID != provided.CursorMemoryID {
		return memory.PurgeJob{}, fmt.Errorf("%w: lifecycle lease", memory.ErrConflict)
	}
	return current, nil
}

func (s *MemoryStore) lifecycleTargets(job memory.PurgeJob, limit int) []memoryRecordKey {
	if limit <= 0 {
		return nil
	}
	collections := s.subjectCollections[job.Subject]
	startCollection := sort.SearchStrings(collections, job.CursorCollectionID)
	targets := make([]memoryRecordKey, 0, limit)
	for collectionIndex := startCollection; collectionIndex < len(collections) && len(targets) < limit; collectionIndex++ {
		collectionID := collections[collectionIndex]
		if job.Scope.CollectionID != "" && collectionID != job.Scope.CollectionID {
			continue
		}
		scope := memory.Scope{Namespace: job.Subject.Namespace, ProjectID: job.Subject.ProjectID, SubjectID: job.Subject.SubjectID, CollectionID: collectionID}
		ids := s.scopeRecordIDs[scope]
		startID := 0
		if collectionID == job.CursorCollectionID {
			startID = sort.Search(len(ids), func(i int) bool { return ids[i] > job.CursorMemoryID })
		}
		for _, id := range ids[startID:] {
			s.lifecycleExamined.Add(1)
			key := memoryRecordKey{Scope: scope, ID: id}
			targets = append(targets, key)
			if len(targets) == limit {
				break
			}
		}
	}
	return targets
}

func (s *MemoryStore) indexLifecycleRecord(key memoryRecordKey) {
	ids := s.scopeRecordIDs[key.Scope]
	index := sort.SearchStrings(ids, key.ID)
	if index == len(ids) || ids[index] != key.ID {
		ids = append(ids, "")
		copy(ids[index+1:], ids[index:])
		ids[index] = key.ID
		s.scopeRecordIDs[key.Scope] = ids
	}
	collections := s.subjectCollections[key.Scope.Subject()]
	collectionIndex := sort.SearchStrings(collections, key.Scope.CollectionID)
	if collectionIndex == len(collections) || collections[collectionIndex] != key.Scope.CollectionID {
		collections = append(collections, "")
		copy(collections[collectionIndex+1:], collections[collectionIndex:])
		collections[collectionIndex] = key.Scope.CollectionID
		s.subjectCollections[key.Scope.Subject()] = collections
	}
}

func (s *MemoryStore) lifecycleEligible(job memory.PurgeJob, record *memory.Record) bool {
	if record.SubjectGeneration > job.PurgeBeforeGeneration {
		return false
	}
	switch job.Kind {
	case memory.PurgeJobForget:
		return true
	case memory.PurgeJobRetract:
		return record.Scope == job.Scope && record.MemoryID == job.MemoryID && record.Revision == job.ExpectedRevision && record.Status == memory.StatusRetracted && !record.UpdatedAt.After(job.SelectionCutoff)
	case memory.PurgeJobRetractSource:
		return record.Scope == job.Scope && record.Provenance.SourceRef == job.SourceRef && record.Provenance.SourceEpoch == job.SourceEpoch && !record.UpdatedAt.After(job.SelectionCutoff)
	case memory.PurgeJobExpiry:
		return record.Scope == job.Scope && record.MemoryID == job.MemoryID && record.Revision == job.ExpectedRevision && !record.ExpiresAt.After(job.SelectionCutoff)
	default:
		return false
	}
}

var _ memory.LifecycleStore = (*MemoryStore)(nil)
