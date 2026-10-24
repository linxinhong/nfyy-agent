package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

type sqliteLifecycleTarget struct {
	scope    memory.Scope
	id       string
	revision uint64
	bytes    int
	record   *memory.Record
}

type sqliteRowScanner interface{ Scan(...any) error }

func scanSQLiteLifecycleJob(row sqliteRowScanner) (memory.PurgeJob, error) {
	var job memory.PurgeJob
	var cutoff, leaseExpires, created, completed int64
	err := row.Scan(&job.TaskID, &job.Kind, &job.Subject.Namespace, &job.Subject.ProjectID, &job.Subject.SubjectID,
		&job.Scope.CollectionID, &job.SourceRef, &job.SourceEpoch, &job.MemoryID, &job.ExpectedRevision,
		&job.PurgeBeforeGeneration, &cutoff, &job.Status, &job.OwnerToken, &job.LeaseEpoch, &leaseExpires,
		&job.CursorCollectionID, &job.CursorMemoryID, &created, &completed, &job.LastFailureCode)
	if err != nil {
		return job, err
	}
	job.Scope.Namespace, job.Scope.ProjectID, job.Scope.SubjectID = job.Subject.Namespace, job.Subject.ProjectID, job.Subject.SubjectID
	job.SelectionCutoff = fromNS(cutoff)
	job.CreatedAt = fromNS(created)
	if leaseExpires != 0 {
		job.LeaseExpiresAt = fromNS(leaseExpires)
	}
	if completed != 0 {
		job.CompletedAt = fromNS(completed)
	}
	return job, nil
}

const sqliteLifecycleColumns = `task_id,kind,namespace,project_id,subject_id,collection_id,source_ref,source_epoch,memory_id,expected_revision,purge_before_generation,selection_cutoff_ns,status,owner_token,lease_epoch,lease_expires_at_ns,cursor_collection_id,cursor_memory_id,created_at_ns,completed_at_ns,last_failure_code`

func insertSQLiteLifecycleJob(ctx context.Context, q sqliteQueryer, job memory.PurgeJob) error {
	_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO agent_runtime_memory_lifecycle_jobs(`+sqliteLifecycleColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.TaskID, job.Kind, job.Subject.Namespace, job.Subject.ProjectID, job.Subject.SubjectID, job.Scope.CollectionID,
		job.SourceRef, job.SourceEpoch, job.MemoryID, job.ExpectedRevision, job.PurgeBeforeGeneration, toNS(job.SelectionCutoff),
		memory.PurgeJobPending, "", 0, 0, "", "", toNS(job.CreatedAt), 0, "")
	return mapSQLiteError(err)
}

func (s *MemoryStore) DiscoverExpired(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > memory.MaxPurgeBatch {
		return 0, memory.ErrInvalid
	}
	conn, err := s.beginLifecycle(ctx)
	if err != nil {
		return 0, err
	}
	committed := false
	defer rollbackSQLiteLifecycle(conn, &committed)
	now := s.storeNow()
	jobs := make([]memory.PurgeJob, 0, limit)

	rows, err := conn.QueryContext(ctx, `SELECT s.namespace,s.project_id,s.subject_id,s.generation
		FROM agent_runtime_memory_subjects s WHERE s.disabled=1 AND s.generation>1
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='forget' AND j.namespace=s.namespace AND j.project_id=s.project_id AND j.subject_id=s.subject_id AND j.purge_before_generation=s.generation-1)
		ORDER BY s.namespace,s.project_id,s.subject_id LIMIT ?`, limit-len(jobs))
	if err != nil {
		return 0, mapSQLiteError(err)
	}
	for rows.Next() {
		var subject memory.SubjectScope
		var generation uint64
		if err := rows.Scan(&subject.Namespace, &subject.ProjectID, &subject.SubjectID, &generation); err != nil {
			rows.Close()
			return 0, mapSQLiteError(err)
		}
		cutoff := generation - 1
		jobs = append(jobs, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobForget, subject, memory.Scope{}, "", "", 0, cutoff), Kind: memory.PurgeJobForget, Subject: subject, PurgeBeforeGeneration: cutoff, SelectionCutoff: now, CreatedAt: now})
	}
	if err := rows.Close(); err != nil {
		return 0, mapSQLiteError(err)
	}

	if len(jobs) < limit {
		rows, err = conn.QueryContext(ctx, `SELECT r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id,r.revision,r.subject_generation,r.updated_at_ns
			FROM agent_runtime_memory_records r WHERE r.content<>'' AND r.status='retracted'
			AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='retract' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND j.collection_id=r.collection_id AND j.memory_id=r.memory_id AND j.expected_revision=r.revision)
			ORDER BY r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id LIMIT ?`, limit-len(jobs))
		if err != nil {
			return 0, mapSQLiteError(err)
		}
		for rows.Next() {
			var scope memory.Scope
			var id string
			var revision, generation uint64
			var updated int64
			if err := rows.Scan(&scope.Namespace, &scope.ProjectID, &scope.SubjectID, &scope.CollectionID, &id, &revision, &generation, &updated); err != nil {
				rows.Close()
				return 0, mapSQLiteError(err)
			}
			jobs = append(jobs, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetract, scope.Subject(), scope, "", id, revision, generation), Kind: memory.PurgeJobRetract, Subject: scope.Subject(), Scope: scope, MemoryID: id, ExpectedRevision: revision, PurgeBeforeGeneration: generation, SelectionCutoff: fromNS(updated), CreatedAt: now})
		}
		if err := rows.Close(); err != nil {
			return 0, mapSQLiteError(err)
		}
	}

	if len(jobs) < limit {
		rows, err = conn.QueryContext(ctx, `SELECT DISTINCT r.namespace,r.project_id,r.subject_id,r.collection_id,r.source_ref,r.source_epoch,u.generation
			FROM agent_runtime_memory_records r
			JOIN agent_runtime_memory_sources s ON s.namespace=r.namespace AND s.project_id=r.project_id AND s.subject_id=r.subject_id AND s.collection_id=r.collection_id AND s.source_ref=r.source_ref
			JOIN agent_runtime_memory_subjects u ON u.namespace=r.namespace AND u.project_id=r.project_id AND u.subject_id=r.subject_id
			WHERE r.content<>'' AND r.source_ref<>'' AND r.source_epoch<s.source_epoch
			AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='retract_source' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND j.collection_id=r.collection_id AND j.source_ref=r.source_ref AND j.source_epoch=r.source_epoch)
			ORDER BY r.namespace,r.project_id,r.subject_id,r.collection_id,r.source_ref,r.source_epoch LIMIT ?`, limit-len(jobs))
		if err != nil {
			return 0, mapSQLiteError(err)
		}
		for rows.Next() {
			var scope memory.Scope
			var sourceRef string
			var oldEpoch, generation uint64
			if err := rows.Scan(&scope.Namespace, &scope.ProjectID, &scope.SubjectID, &scope.CollectionID, &sourceRef, &oldEpoch, &generation); err != nil {
				rows.Close()
				return 0, mapSQLiteError(err)
			}
			jobs = append(jobs, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetractSource, scope.Subject(), scope, sourceRef, "", oldEpoch, generation), Kind: memory.PurgeJobRetractSource, Subject: scope.Subject(), Scope: scope, SourceRef: sourceRef, SourceEpoch: oldEpoch, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now})
		}
		if err := rows.Close(); err != nil {
			return 0, mapSQLiteError(err)
		}
	}

	if len(jobs) < limit {
		rows, err = conn.QueryContext(ctx, `SELECT r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id,r.revision,r.subject_generation,r.expires_at_ns
		FROM agent_runtime_memory_records r
		WHERE r.content<>'' AND r.expires_at_ns<=?
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='expiry' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND j.collection_id=r.collection_id AND j.memory_id=r.memory_id AND j.expected_revision=r.revision)
		ORDER BY r.expires_at_ns,r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id LIMIT ?`, toNS(now), limit-len(jobs))
		if err != nil {
			return 0, mapSQLiteError(err)
		}
		for rows.Next() {
			var scope memory.Scope
			var id string
			var revision, generation uint64
			var expires int64
			if err := rows.Scan(&scope.Namespace, &scope.ProjectID, &scope.SubjectID, &scope.CollectionID, &id, &revision, &generation, &expires); err != nil {
				rows.Close()
				return 0, mapSQLiteError(err)
			}
			jobs = append(jobs, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobExpiry, scope.Subject(), scope, "", id, revision, generation), Kind: memory.PurgeJobExpiry, Subject: scope.Subject(), Scope: scope, MemoryID: id, ExpectedRevision: revision, PurgeBeforeGeneration: generation, SelectionCutoff: fromNS(expires), CreatedAt: now})
		}
		if err := rows.Close(); err != nil {
			return 0, mapSQLiteError(err)
		}
	}
	for _, job := range jobs {
		if err := insertSQLiteLifecycleJob(ctx, conn, job); err != nil {
			return 0, err
		}
	}
	if err := commitSQLiteLifecycle(ctx, conn, &committed); err != nil {
		return 0, err
	}
	return len(jobs), nil
}

func (s *MemoryStore) ClaimNextPurge(ctx context.Context, owner string, leaseDuration time.Duration) (memory.PurgeJob, error) {
	if !validLifecycleText(owner, 256) {
		return memory.PurgeJob{}, memory.ErrInvalid
	}
	conn, err := s.beginLifecycle(ctx)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	committed := false
	defer rollbackSQLiteLifecycle(conn, &committed)
	now := s.storeNow()
	deadline, err := memory.PurgeLeaseDeadline(now, leaseDuration)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	job, err := scanSQLiteLifecycleJob(conn.QueryRowContext(ctx, `SELECT `+sqliteLifecycleColumns+` FROM agent_runtime_memory_lifecycle_jobs WHERE status IN ('pending','failed') OR (status='leased' AND lease_expires_at_ns<=?) ORDER BY created_at_ns,task_id LIMIT 1`, toNS(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.PurgeJob{}, memory.ErrNotFound
	}
	if err != nil {
		return memory.PurgeJob{}, mapSQLiteError(err)
	}
	job.Status = memory.PurgeJobLeased
	job.OwnerToken = owner
	job.LeaseEpoch++
	if job.LeaseEpoch == 0 {
		job.LeaseEpoch = 1
	}
	job.LeaseExpiresAt = deadline
	job.LastFailureCode = ""
	result, err := conn.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET status='leased',owner_token=?,lease_epoch=?,lease_expires_at_ns=?,last_failure_code='' WHERE task_id=?`, owner, job.LeaseEpoch, toNS(job.LeaseExpiresAt), job.TaskID)
	if err != nil {
		return memory.PurgeJob{}, mapSQLiteError(err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return memory.PurgeJob{}, memory.ErrConflict
	}
	if err := commitSQLiteLifecycle(ctx, conn, &committed); err != nil {
		return memory.PurgeJob{}, err
	}
	return job, nil
}

func (s *MemoryStore) RenewPurgeLease(ctx context.Context, claim memory.PurgeJob, leaseDuration time.Duration) (memory.PurgeJob, error) {
	conn, err := s.beginLifecycle(ctx)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	committed := false
	defer rollbackSQLiteLifecycle(conn, &committed)
	now := s.storeNow()
	deadline, err := memory.PurgeLeaseDeadline(now, leaseDuration)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	job, err := loadSQLiteLifecycleJob(ctx, conn, claim.TaskID)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	if err := validateSQLiteLifecycleClaim(job, claim, now); err != nil {
		return memory.PurgeJob{}, err
	}
	job.LeaseExpiresAt = deadline
	if _, err := conn.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET lease_expires_at_ns=? WHERE task_id=? AND lease_epoch=?`, toNS(job.LeaseExpiresAt), job.TaskID, job.LeaseEpoch); err != nil {
		return memory.PurgeJob{}, mapSQLiteError(err)
	}
	if err := commitSQLiteLifecycle(ctx, conn, &committed); err != nil {
		return memory.PurgeJob{}, err
	}
	return job, nil
}

func (s *MemoryStore) PurgeBatch(ctx context.Context, claim memory.PurgeJob, limit int) (memory.PurgeBatchResult, error) {
	if limit <= 0 || limit > memory.MaxPurgeBatch {
		return memory.PurgeBatchResult{}, memory.ErrInvalid
	}
	conn, err := s.beginLifecycle(ctx)
	if err != nil {
		return memory.PurgeBatchResult{}, err
	}
	committed := false
	defer rollbackSQLiteLifecycle(conn, &committed)
	now := s.storeNow()
	job, err := loadSQLiteLifecycleJob(ctx, conn, claim.TaskID)
	if err != nil {
		return memory.PurgeBatchResult{}, err
	}
	if err := validateSQLiteLifecycleClaim(job, claim, now); err != nil {
		return memory.PurgeBatchResult{}, err
	}
	targets, err := selectSQLiteLifecycleTargets(ctx, conn, job, limit)
	if err != nil {
		return memory.PurgeBatchResult{}, err
	}
	result := memory.PurgeBatchResult{Job: job, StoreNow: now, ProcessedTargets: len(targets)}
	deltas := make(map[memory.Scope]struct{ records, bytes int })
	for _, target := range targets {
		job.CursorCollectionID, job.CursorMemoryID = target.scope.CollectionID, target.id
		if target.record == nil || target.record.Content == "" || !sqliteLifecycleEligible(job, target.record) {
			continue
		}
		changed, err := conn.ExecContext(ctx, `UPDATE agent_runtime_memory_records SET content='',topics_json='[]',updated_at_ns=? WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=? AND revision=? AND content<>''`, toNS(now), target.scope.Namespace, target.scope.ProjectID, target.scope.SubjectID, target.scope.CollectionID, target.id, target.revision)
		if err != nil {
			return memory.PurgeBatchResult{}, mapSQLiteError(err)
		}
		if rows, _ := changed.RowsAffected(); rows != 1 {
			return memory.PurgeBatchResult{}, memory.ErrConflict
		}
		delta := deltas[target.scope]
		delta.records++
		delta.bytes += target.bytes
		deltas[target.scope] = delta
		result.PurgedRecords++
		result.ReleasedBytes += target.bytes
	}
	for scope, delta := range deltas {
		changed, err := conn.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET physical_records=physical_records-?,physical_bytes=physical_bytes-? WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND physical_records>=? AND physical_bytes>=?`, delta.records, delta.bytes, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, delta.records, delta.bytes)
		if err != nil {
			return memory.PurgeBatchResult{}, mapSQLiteError(err)
		}
		if rows, _ := changed.RowsAffected(); rows != 1 {
			return memory.PurgeBatchResult{}, memory.ErrConflict
		}
	}
	if result.PurgedRecords > 0 {
		audit := memory.AuditEvent{OperationID: memory.LifecycleAuditID(job, result.ProcessedTargets), Scope: job.Scope, Subject: job.Subject, Action: memory.ActionPurge, Actor: job.OwnerToken, Issuer: "memory-lifecycle", Generation: job.PurgeBeforeGeneration, OccurredAt: now}
		s.mu.RLock()
		auditFailure := s.auditFailure
		s.mu.RUnlock()
		if auditFailure != nil {
			if err := auditFailure(audit); err != nil {
				return memory.PurgeBatchResult{}, fmt.Errorf("%w: %v", memory.ErrAudit, err)
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_audits(operation_id,namespace,project_id,subject_id,collection_id,action,actor,issuer,memory_id,revision,generation,occurred_at_ns) VALUES(?,?,?,?,?,'purge',?,'memory-lifecycle','',0,?,?)`, audit.OperationID, job.Subject.Namespace, job.Subject.ProjectID, job.Subject.SubjectID, job.Scope.CollectionID, job.OwnerToken, job.PurgeBeforeGeneration, toNS(now)); err != nil {
			return memory.PurgeBatchResult{}, mapSQLiteError(err)
		}
	}
	if len(targets) < limit {
		job.Status = memory.PurgeJobCompleted
		job.CompletedAt = now
		job.OwnerToken = ""
		job.LeaseExpiresAt = time.Time{}
		result.Completed = true
		_, err = conn.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET status='completed',owner_token='',lease_expires_at_ns=0,cursor_collection_id=?,cursor_memory_id=?,completed_at_ns=?,last_failure_code='' WHERE task_id=? AND lease_epoch=?`, job.CursorCollectionID, job.CursorMemoryID, toNS(now), job.TaskID, job.LeaseEpoch)
	} else {
		_, err = conn.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET cursor_collection_id=?,cursor_memory_id=? WHERE task_id=? AND lease_epoch=?`, job.CursorCollectionID, job.CursorMemoryID, job.TaskID, job.LeaseEpoch)
	}
	if err != nil {
		return memory.PurgeBatchResult{}, mapSQLiteError(err)
	}
	s.mu.RLock()
	fault := s.commitFault
	s.mu.RUnlock()
	if fault != nil {
		if err := fault(CommitBefore); err != nil {
			return memory.PurgeBatchResult{}, err
		}
	}
	if err := commitSQLiteLifecycle(ctx, conn, &committed); err != nil {
		return memory.PurgeBatchResult{}, err
	}
	result.Job = job
	if fault != nil {
		if err := fault(CommitAfter); err != nil {
			return memory.PurgeBatchResult{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
		}
	}
	return result, nil
}

func (s *MemoryStore) FailPurge(ctx context.Context, claim memory.PurgeJob, failureCode string) error {
	if !validLifecycleText(failureCode, memory.MaxPurgeFailureCode) {
		return memory.ErrInvalid
	}
	conn, err := s.beginLifecycle(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer rollbackSQLiteLifecycle(conn, &committed)
	job, err := loadSQLiteLifecycleJob(ctx, conn, claim.TaskID)
	if err != nil {
		return err
	}
	if err := validateSQLiteLifecycleClaim(job, claim, s.storeNow()); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET status='failed',owner_token='',lease_expires_at_ns=0,last_failure_code=? WHERE task_id=? AND lease_epoch=?`, failureCode, job.TaskID, job.LeaseEpoch); err != nil {
		return mapSQLiteError(err)
	}
	return commitSQLiteLifecycle(ctx, conn, &committed)
}

func (s *MemoryStore) PurgeLag(ctx context.Context) (memory.PurgeLagSnapshot, error) {
	now := s.storeNow()
	var pending, failed int
	var oldest sql.NullInt64
	if err := s.db.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN status='failed' THEN 1 ELSE 0 END),0),min(created_at_ns) FROM agent_runtime_memory_lifecycle_jobs WHERE status<>'completed'`).Scan(&pending, &failed, &oldest); err != nil {
		return memory.PurgeLagSnapshot{}, mapSQLiteError(err)
	}
	result := memory.PurgeLagSnapshot{StoreNow: now, PendingJobs: pending, FailedJobs: failed}
	if oldest.Valid {
		result.OldestCreatedAt = fromNS(oldest.Int64)
		result.OldestLag = now.Sub(result.OldestCreatedAt)
		if result.OldestLag < 0 {
			result.OldestLag = 0
		}
		result.ObjectiveMissed = result.OldestLag >= memory.PurgeLagObjective
	}
	return result, nil
}

func (s *MemoryStore) ReclaimLifecycle(ctx context.Context, limit int) (memory.LifecycleReclaimResult, error) {
	if limit <= 0 || limit > memory.MaxPurgeBatch {
		return memory.LifecycleReclaimResult{}, memory.ErrInvalid
	}
	conn, err := s.beginLifecycle(ctx)
	if err != nil {
		return memory.LifecycleReclaimResult{}, err
	}
	committed := false
	defer rollbackSQLiteLifecycle(conn, &committed)
	now := s.storeNow()
	result := memory.LifecycleReclaimResult{StoreNow: now}
	cutoff := now.Add(-memory.LifecycleRetention)
	rows, err := conn.QueryContext(ctx, `SELECT r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id,r.revision,r.updated_at_ns
		FROM agent_runtime_memory_records r
		WHERE r.content='' AND r.status<>'candidate' AND r.updated_at_ns<=?
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_receipts p WHERE p.namespace=r.namespace AND p.project_id=r.project_id AND p.subject_id=r.subject_id AND p.collection_id=r.collection_id AND p.expires_at_ns>?)
		AND EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.status='completed' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND ((j.kind='forget' AND r.subject_generation<=j.purge_before_generation) OR (j.kind IN ('retract','expiry') AND j.collection_id=r.collection_id AND j.memory_id=r.memory_id AND j.expected_revision=r.revision) OR (j.kind='retract_source' AND j.collection_id=r.collection_id AND j.source_ref=r.source_ref AND j.source_epoch=r.source_epoch)))
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.status<>'completed' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND ((j.kind='forget' AND r.subject_generation<=j.purge_before_generation) OR (j.kind IN ('retract','expiry') AND j.collection_id=r.collection_id AND j.memory_id=r.memory_id AND j.expected_revision=r.revision) OR (j.kind='retract_source' AND j.collection_id=r.collection_id AND j.source_ref=r.source_ref AND j.source_epoch=r.source_epoch)))
		ORDER BY r.updated_at_ns,r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id LIMIT ?`, toNS(cutoff), toNS(now), limit)
	if err != nil {
		return result, mapSQLiteError(err)
	}
	type reclaimRecord struct {
		scope             memory.Scope
		id                string
		revision          uint64
		updatedAtUnixNano int64
	}
	records := make([]reclaimRecord, 0, limit)
	for rows.Next() {
		var record reclaimRecord
		if err := rows.Scan(&record.scope.Namespace, &record.scope.ProjectID, &record.scope.SubjectID, &record.scope.CollectionID, &record.id, &record.revision, &record.updatedAtUnixNano); err != nil {
			rows.Close()
			return result, mapSQLiteError(err)
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return result, mapSQLiteError(err)
	}
	result.ExaminedControls += len(records)
	for _, record := range records {
		deleted, err := conn.ExecContext(ctx, `DELETE FROM agent_runtime_memory_records WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=? AND revision=? AND updated_at_ns=? AND content='' AND status<>'candidate'`, record.scope.Namespace, record.scope.ProjectID, record.scope.SubjectID, record.scope.CollectionID, record.id, record.revision, record.updatedAtUnixNano)
		if err != nil {
			return result, mapSQLiteError(err)
		}
		if affected, _ := deleted.RowsAffected(); affected != 1 {
			return result, memory.ErrConflict
		}
		updated, err := conn.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET record_identities=record_identities-1 WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND record_identities>0`, record.scope.Namespace, record.scope.ProjectID, record.scope.SubjectID, record.scope.CollectionID)
		if err != nil {
			return result, mapSQLiteError(err)
		}
		if affected, _ := updated.RowsAffected(); affected != 1 {
			return result, memory.ErrConflict
		}
		result.ReclaimedRecords++
		result.ReleasedControls++
	}

	remaining := limit - result.ExaminedControls
	if remaining > 0 {
		rows, err = conn.QueryContext(ctx, `SELECT j.task_id FROM agent_runtime_memory_lifecycle_jobs j
			WHERE j.status='completed' AND NOT EXISTS (
				SELECT 1 FROM agent_runtime_memory_records r WHERE r.content='' AND r.status<>'candidate'
				AND r.namespace=j.namespace AND r.project_id=j.project_id AND r.subject_id=j.subject_id
				AND ((j.kind='forget' AND r.subject_generation<=j.purge_before_generation) OR (j.kind IN ('retract','expiry') AND r.collection_id=j.collection_id AND r.memory_id=j.memory_id AND r.revision=j.expected_revision) OR (j.kind='retract_source' AND r.collection_id=j.collection_id AND r.source_ref=j.source_ref AND r.source_epoch=j.source_epoch)))
			ORDER BY j.completed_at_ns,j.task_id LIMIT ?`, remaining)
		if err != nil {
			return result, mapSQLiteError(err)
		}
		jobs := make([]string, 0, remaining)
		for rows.Next() {
			var taskID string
			if err := rows.Scan(&taskID); err != nil {
				rows.Close()
				return result, mapSQLiteError(err)
			}
			jobs = append(jobs, taskID)
		}
		if err := rows.Close(); err != nil {
			return result, mapSQLiteError(err)
		}
		result.ExaminedControls += len(jobs)
		for _, taskID := range jobs {
			deleted, err := conn.ExecContext(ctx, `DELETE FROM agent_runtime_memory_lifecycle_jobs WHERE task_id=? AND status='completed'`, taskID)
			if err != nil {
				return result, mapSQLiteError(err)
			}
			if affected, _ := deleted.RowsAffected(); affected != 1 {
				return result, memory.ErrConflict
			}
			result.ReclaimedJobs++
		}
	}
	s.mu.RLock()
	fault := s.commitFault
	s.mu.RUnlock()
	if fault != nil {
		if err := fault(CommitBefore); err != nil {
			return result, err
		}
	}
	if err := commitSQLiteLifecycle(ctx, conn, &committed); err != nil {
		return result, err
	}
	if fault != nil {
		if err := fault(CommitAfter); err != nil {
			return result, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
		}
	}
	return result, nil
}

func (s *MemoryStore) beginLifecycle(ctx context.Context) (*sql.Conn, error) {
	conn, err := s.db.db.Conn(ctx)
	if err != nil {
		return nil, mapSQLiteError(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=25`); err != nil {
		conn.Close()
		return nil, mapSQLiteError(err)
	}
	if err := beginImmediate(ctx, conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func rollbackSQLiteLifecycle(conn *sql.Conn, committed *bool) {
	if !*committed {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
	}
	_ = conn.Close()
}

func commitSQLiteLifecycle(ctx context.Context, conn *sql.Conn, committed *bool) error {
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("%w: %v", memory.ErrCommitUnknown, mapSQLiteError(err))
	}
	*committed = true
	return nil
}

func loadSQLiteLifecycleJob(ctx context.Context, q sqliteQueryer, taskID string) (memory.PurgeJob, error) {
	if taskID == "" {
		return memory.PurgeJob{}, memory.ErrInvalid
	}
	job, err := scanSQLiteLifecycleJob(q.QueryRowContext(ctx, `SELECT `+sqliteLifecycleColumns+` FROM agent_runtime_memory_lifecycle_jobs WHERE task_id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.PurgeJob{}, memory.ErrNotFound
	}
	return job, mapSQLiteError(err)
}

func validateSQLiteLifecycleClaim(current, provided memory.PurgeJob, now time.Time) error {
	if current.Status != memory.PurgeJobLeased || current.OwnerToken == "" || current.OwnerToken != provided.OwnerToken || current.LeaseEpoch == 0 || current.LeaseEpoch != provided.LeaseEpoch || !current.LeaseExpiresAt.Equal(provided.LeaseExpiresAt) || !current.LeaseExpiresAt.After(now) || current.CursorCollectionID != provided.CursorCollectionID || current.CursorMemoryID != provided.CursorMemoryID {
		return fmt.Errorf("%w: lifecycle lease", memory.ErrConflict)
	}
	return nil
}

func selectSQLiteLifecycleTargets(ctx context.Context, q sqliteQueryer, job memory.PurgeJob, limit int) ([]sqliteLifecycleTarget, error) {
	query := `SELECT namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,status,updated_at_ns,expires_at_ns,source_ref,source_epoch,(content<>''),length(CAST(content AS BLOB))
		FROM agent_runtime_memory_records WHERE namespace=? AND project_id=? AND subject_id=? AND (collection_id>? OR (collection_id=? AND memory_id>?))`
	args := []any{job.Subject.Namespace, job.Subject.ProjectID, job.Subject.SubjectID, job.CursorCollectionID, job.CursorCollectionID, job.CursorMemoryID}
	if job.Scope.CollectionID != "" {
		query += ` AND collection_id=?`
		args = append(args, job.Scope.CollectionID)
	}
	query += ` ORDER BY collection_id,memory_id LIMIT ?`
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapSQLiteError(err)
	}
	defer rows.Close()
	targets := make([]sqliteLifecycleTarget, 0, limit)
	for rows.Next() {
		var scope memory.Scope
		var record memory.Record
		var updated, expires int64
		var hasContent bool
		var contentBytes int
		if err := rows.Scan(&scope.Namespace, &scope.ProjectID, &scope.SubjectID, &scope.CollectionID, &record.MemoryID, &record.Revision, &record.SubjectGeneration, &record.Status, &updated, &expires, &record.Provenance.SourceRef, &record.Provenance.SourceEpoch, &hasContent, &contentBytes); err != nil {
			return nil, mapSQLiteError(err)
		}
		record.Scope, record.UpdatedAt, record.ExpiresAt = scope, fromNS(updated), fromNS(expires)
		if hasContent {
			record.Content = "present"
		}
		targets = append(targets, sqliteLifecycleTarget{scope: scope, id: record.MemoryID, revision: record.Revision, bytes: contentBytes, record: &record})
	}
	return targets, mapSQLiteError(rows.Err())
}

func sqliteLifecycleEligible(job memory.PurgeJob, record *memory.Record) bool {
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

func validLifecycleText(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

var _ memory.LifecycleStore = (*MemoryStore)(nil)
