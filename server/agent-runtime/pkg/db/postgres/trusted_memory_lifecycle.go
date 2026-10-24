package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

const postgresLifecycleColumns = `task_id,kind,namespace,project_id,subject_id,collection_id,source_ref,source_epoch,memory_id,expected_revision,purge_before_generation,selection_cutoff,status,owner_token,lease_epoch,lease_expires_at,cursor_collection_id,cursor_memory_id,created_at,completed_at,last_failure_code`

type postgresLifecycleTarget struct {
	scope    memory.Scope
	id       string
	revision uint64
	bytes    int
	record   *memory.Record
}

type postgresRowScanner interface{ Scan(...any) error }
type postgresExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func scanPostgresLifecycleJob(row postgresRowScanner) (memory.PurgeJob, error) {
	var job memory.PurgeJob
	var leaseExpires, completed sql.NullTime
	err := row.Scan(&job.TaskID, &job.Kind, &job.Subject.Namespace, &job.Subject.ProjectID, &job.Subject.SubjectID,
		&job.Scope.CollectionID, &job.SourceRef, &job.SourceEpoch, &job.MemoryID, &job.ExpectedRevision,
		&job.PurgeBeforeGeneration, &job.SelectionCutoff, &job.Status, &job.OwnerToken, &job.LeaseEpoch, &leaseExpires,
		&job.CursorCollectionID, &job.CursorMemoryID, &job.CreatedAt, &completed, &job.LastFailureCode)
	if err != nil {
		return job, err
	}
	job.Scope.Namespace, job.Scope.ProjectID, job.Scope.SubjectID = job.Subject.Namespace, job.Subject.ProjectID, job.Subject.SubjectID
	job.SelectionCutoff, job.CreatedAt = job.SelectionCutoff.UTC(), job.CreatedAt.UTC()
	if leaseExpires.Valid {
		job.LeaseExpiresAt = leaseExpires.Time.UTC()
	}
	if completed.Valid {
		job.CompletedAt = completed.Time.UTC()
	}
	return job, nil
}

func insertPostgresLifecycleJob(ctx context.Context, q postgresExecer, job memory.PurgeJob) error {
	_, err := q.ExecContext(ctx, `INSERT INTO agent_runtime_memory_lifecycle_jobs(`+postgresLifecycleColumns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'pending','',0,NULL,'','',$13,NULL,'') ON CONFLICT(task_id) DO NOTHING`,
		job.TaskID, job.Kind, job.Subject.Namespace, job.Subject.ProjectID, job.Subject.SubjectID, job.Scope.CollectionID,
		job.SourceRef, job.SourceEpoch, job.MemoryID, job.ExpectedRevision, job.PurgeBeforeGeneration, job.SelectionCutoff, job.CreatedAt)
	return wrapStoreErr(err)
}

func (s *MemoryStore) DiscoverExpired(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > memory.MaxPurgeBatch {
		return 0, memory.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	jobs := make([]memory.PurgeJob, 0, limit)
	rows, err := tx.QueryContext(ctx, `SELECT s.namespace,s.project_id,s.subject_id,s.generation FROM agent_runtime_memory_subjects s
		WHERE s.disabled AND s.generation>1
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='forget' AND j.namespace=s.namespace AND j.project_id=s.project_id AND j.subject_id=s.subject_id AND j.purge_before_generation=s.generation-1)
		ORDER BY s.namespace,s.project_id,s.subject_id LIMIT $1 FOR UPDATE OF s SKIP LOCKED`, limit-len(jobs))
	if err != nil {
		return 0, wrapStoreErr(err)
	}
	for rows.Next() {
		var subject memory.SubjectScope
		var generation uint64
		if err := rows.Scan(&subject.Namespace, &subject.ProjectID, &subject.SubjectID, &generation); err != nil {
			rows.Close()
			return 0, wrapStoreErr(err)
		}
		cutoff := generation - 1
		jobs = append(jobs, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobForget, subject, memory.Scope{}, "", "", 0, cutoff), Kind: memory.PurgeJobForget, Subject: subject, PurgeBeforeGeneration: cutoff, SelectionCutoff: now, CreatedAt: now})
	}
	if err := rows.Close(); err != nil {
		return 0, wrapStoreErr(err)
	}

	if len(jobs) < limit {
		rows, err = tx.QueryContext(ctx, `SELECT r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id,r.revision,r.subject_generation,r.updated_at
			FROM agent_runtime_memory_records r WHERE r.content<>'' AND r.status='retracted'
			AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='retract' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND j.collection_id=r.collection_id AND j.memory_id=r.memory_id AND j.expected_revision=r.revision)
			ORDER BY r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id LIMIT $1 FOR UPDATE OF r SKIP LOCKED`, limit-len(jobs))
		if err != nil {
			return 0, wrapStoreErr(err)
		}
		for rows.Next() {
			var scope memory.Scope
			var id string
			var revision, generation uint64
			var updated time.Time
			if err := rows.Scan(&scope.Namespace, &scope.ProjectID, &scope.SubjectID, &scope.CollectionID, &id, &revision, &generation, &updated); err != nil {
				rows.Close()
				return 0, wrapStoreErr(err)
			}
			jobs = append(jobs, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetract, scope.Subject(), scope, "", id, revision, generation), Kind: memory.PurgeJobRetract, Subject: scope.Subject(), Scope: scope, MemoryID: id, ExpectedRevision: revision, PurgeBeforeGeneration: generation, SelectionCutoff: updated.UTC(), CreatedAt: now})
		}
		if err := rows.Close(); err != nil {
			return 0, wrapStoreErr(err)
		}
	}

	if len(jobs) < limit {
		rows, err = tx.QueryContext(ctx, `SELECT DISTINCT r.namespace,r.project_id,r.subject_id,r.collection_id,r.source_ref,r.source_epoch,u.generation
			FROM agent_runtime_memory_records r
			JOIN agent_runtime_memory_sources s ON s.namespace=r.namespace AND s.project_id=r.project_id AND s.subject_id=r.subject_id AND s.collection_id=r.collection_id AND s.source_ref=r.source_ref
			JOIN agent_runtime_memory_subjects u ON u.namespace=r.namespace AND u.project_id=r.project_id AND u.subject_id=r.subject_id
			WHERE r.content<>'' AND r.source_ref<>'' AND r.source_epoch<s.source_epoch
			AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='retract_source' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND j.collection_id=r.collection_id AND j.source_ref=r.source_ref AND j.source_epoch=r.source_epoch)
			ORDER BY r.namespace,r.project_id,r.subject_id,r.collection_id,r.source_ref,r.source_epoch LIMIT $1`, limit-len(jobs))
		if err != nil {
			return 0, wrapStoreErr(err)
		}
		for rows.Next() {
			var scope memory.Scope
			var sourceRef string
			var oldEpoch, generation uint64
			if err := rows.Scan(&scope.Namespace, &scope.ProjectID, &scope.SubjectID, &scope.CollectionID, &sourceRef, &oldEpoch, &generation); err != nil {
				rows.Close()
				return 0, wrapStoreErr(err)
			}
			jobs = append(jobs, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetractSource, scope.Subject(), scope, sourceRef, "", oldEpoch, generation), Kind: memory.PurgeJobRetractSource, Subject: scope.Subject(), Scope: scope, SourceRef: sourceRef, SourceEpoch: oldEpoch, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now})
		}
		if err := rows.Close(); err != nil {
			return 0, wrapStoreErr(err)
		}
	}

	if len(jobs) < limit {
		rows, err = tx.QueryContext(ctx, `SELECT r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id,r.revision,r.subject_generation,r.expires_at
		FROM agent_runtime_memory_records r
		WHERE r.content<>'' AND r.expires_at<=$1
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.kind='expiry' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND j.collection_id=r.collection_id AND j.memory_id=r.memory_id AND j.expected_revision=r.revision)
		ORDER BY r.expires_at,r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id LIMIT $2 FOR UPDATE OF r SKIP LOCKED`, now, limit-len(jobs))
		if err != nil {
			return 0, wrapStoreErr(err)
		}
		for rows.Next() {
			var scope memory.Scope
			var id string
			var revision, generation uint64
			var expires time.Time
			if err := rows.Scan(&scope.Namespace, &scope.ProjectID, &scope.SubjectID, &scope.CollectionID, &id, &revision, &generation, &expires); err != nil {
				rows.Close()
				return 0, wrapStoreErr(err)
			}
			jobs = append(jobs, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobExpiry, scope.Subject(), scope, "", id, revision, generation), Kind: memory.PurgeJobExpiry, Subject: scope.Subject(), Scope: scope, MemoryID: id, ExpectedRevision: revision, PurgeBeforeGeneration: generation, SelectionCutoff: expires.UTC(), CreatedAt: now})
		}
		if err := rows.Close(); err != nil {
			return 0, wrapStoreErr(err)
		}
	}
	for _, job := range jobs {
		if err := insertPostgresLifecycleJob(ctx, tx, job); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	return len(jobs), nil
}

func (s *MemoryStore) ClaimNextPurge(ctx context.Context, owner string, leaseDuration time.Duration) (memory.PurgeJob, error) {
	if !validPostgresLifecycleText(owner, 256) {
		return memory.PurgeJob{}, memory.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return memory.PurgeJob{}, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return memory.PurgeJob{}, wrapStoreErr(err)
	}
	deadline, err := memory.PurgeLeaseDeadline(now, leaseDuration)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	job, err := scanPostgresLifecycleJob(tx.QueryRowContext(ctx, `SELECT `+postgresLifecycleColumns+` FROM agent_runtime_memory_lifecycle_jobs WHERE status IN ('pending','failed') OR (status='leased' AND lease_expires_at<=$1) ORDER BY created_at,task_id LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.PurgeJob{}, memory.ErrNotFound
	}
	if err != nil {
		return memory.PurgeJob{}, wrapStoreErr(err)
	}
	job.Status = memory.PurgeJobLeased
	job.OwnerToken = owner
	job.LeaseEpoch++
	if job.LeaseEpoch == 0 {
		job.LeaseEpoch = 1
	}
	job.LeaseExpiresAt = deadline
	job.LastFailureCode = ""
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET status='leased',owner_token=$1,lease_epoch=$2,lease_expires_at=$3,last_failure_code='' WHERE task_id=$4`, owner, job.LeaseEpoch, job.LeaseExpiresAt, job.TaskID); err != nil {
		return memory.PurgeJob{}, wrapStoreErr(err)
	}
	if err := tx.Commit(); err != nil {
		return memory.PurgeJob{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	return job, nil
}

func (s *MemoryStore) RenewPurgeLease(ctx context.Context, claim memory.PurgeJob, leaseDuration time.Duration) (memory.PurgeJob, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return memory.PurgeJob{}, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return memory.PurgeJob{}, wrapStoreErr(err)
	}
	deadline, err := memory.PurgeLeaseDeadline(now, leaseDuration)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	job, err := loadPostgresLifecycleJob(ctx, tx, claim.TaskID, true)
	if err != nil {
		return memory.PurgeJob{}, err
	}
	if err := validatePostgresLifecycleClaim(job, claim, now); err != nil {
		return memory.PurgeJob{}, err
	}
	job.LeaseExpiresAt = deadline
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET lease_expires_at=$1 WHERE task_id=$2 AND lease_epoch=$3`, job.LeaseExpiresAt, job.TaskID, job.LeaseEpoch); err != nil {
		return memory.PurgeJob{}, wrapStoreErr(err)
	}
	if err := tx.Commit(); err != nil {
		return memory.PurgeJob{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	return job, nil
}

func (s *MemoryStore) PurgeBatch(ctx context.Context, claim memory.PurgeJob, limit int) (memory.PurgeBatchResult, error) {
	if limit <= 0 || limit > memory.MaxPurgeBatch {
		return memory.PurgeBatchResult{}, memory.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return memory.PurgeBatchResult{}, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return memory.PurgeBatchResult{}, wrapStoreErr(err)
	}
	job, err := loadPostgresLifecycleJob(ctx, tx, claim.TaskID, true)
	if err != nil {
		return memory.PurgeBatchResult{}, err
	}
	if err := validatePostgresLifecycleClaim(job, claim, now); err != nil {
		return memory.PurgeBatchResult{}, err
	}
	targets, err := selectPostgresLifecycleTargets(ctx, tx, job, limit)
	if err != nil {
		return memory.PurgeBatchResult{}, err
	}
	result := memory.PurgeBatchResult{Job: job, StoreNow: now, ProcessedTargets: len(targets)}
	deltas := make(map[memory.Scope]struct{ records, bytes int })
	for _, target := range targets {
		job.CursorCollectionID, job.CursorMemoryID = target.scope.CollectionID, target.id
		if target.record == nil || target.record.Content == "" || !postgresLifecycleEligible(job, target.record) {
			continue
		}
		changed, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_records SET content='',topics='[]'::jsonb,updated_at=$1 WHERE namespace=$2 AND project_id=$3 AND subject_id=$4 AND collection_id=$5 AND memory_id=$6 AND revision=$7 AND content<>''`, now, target.scope.Namespace, target.scope.ProjectID, target.scope.SubjectID, target.scope.CollectionID, target.id, target.revision)
		if err != nil {
			return memory.PurgeBatchResult{}, wrapStoreErr(err)
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
		changed, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET records_count=records_count-$1,content_bytes=content_bytes-$2 WHERE namespace=$3 AND project_id=$4 AND subject_id=$5 AND collection_id=$6 AND records_count>=$1 AND content_bytes>=$2`, delta.records, delta.bytes, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID)
		if err != nil {
			return memory.PurgeBatchResult{}, wrapStoreErr(err)
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
		if err := insertAudit(ctx, tx, audit); err != nil {
			return memory.PurgeBatchResult{}, err
		}
	}
	if len(targets) < limit {
		job.Status = memory.PurgeJobCompleted
		job.CompletedAt = now
		job.OwnerToken = ""
		job.LeaseExpiresAt = time.Time{}
		result.Completed = true
		_, err = tx.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET status='completed',owner_token='',lease_expires_at=NULL,cursor_collection_id=$1,cursor_memory_id=$2,completed_at=$3,last_failure_code='' WHERE task_id=$4 AND lease_epoch=$5`, job.CursorCollectionID, job.CursorMemoryID, now, job.TaskID, job.LeaseEpoch)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET cursor_collection_id=$1,cursor_memory_id=$2 WHERE task_id=$3 AND lease_epoch=$4`, job.CursorCollectionID, job.CursorMemoryID, job.TaskID, job.LeaseEpoch)
	}
	if err != nil {
		return memory.PurgeBatchResult{}, wrapStoreErr(err)
	}
	s.mu.RLock()
	beforeCommit := s.lifecycleCommitFailure
	s.mu.RUnlock()
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return memory.PurgeBatchResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return memory.PurgeBatchResult{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	result.Job = job
	s.mu.RLock()
	after := s.afterCommitFailure
	s.mu.RUnlock()
	if after != nil {
		if err := after(); err != nil {
			return memory.PurgeBatchResult{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
		}
	}
	return result, nil
}

func (s *MemoryStore) FailPurge(ctx context.Context, claim memory.PurgeJob, failureCode string) error {
	if !validPostgresLifecycleText(failureCode, memory.MaxPurgeFailureCode) {
		return memory.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return wrapStoreErr(err)
	}
	job, err := loadPostgresLifecycleJob(ctx, tx, claim.TaskID, true)
	if err != nil {
		return err
	}
	if err := validatePostgresLifecycleClaim(job, claim, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_lifecycle_jobs SET status='failed',owner_token='',lease_expires_at=NULL,last_failure_code=$1 WHERE task_id=$2 AND lease_epoch=$3`, failureCode, job.TaskID, job.LeaseEpoch); err != nil {
		return wrapStoreErr(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	return nil
}

func (s *MemoryStore) PurgeLag(ctx context.Context) (memory.PurgeLagSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return memory.PurgeLagSnapshot{}, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return memory.PurgeLagSnapshot{}, wrapStoreErr(err)
	}
	var pending, failed int
	var oldest sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER (WHERE status='failed'),min(created_at) FROM agent_runtime_memory_lifecycle_jobs WHERE status<>'completed'`).Scan(&pending, &failed, &oldest); err != nil {
		return memory.PurgeLagSnapshot{}, wrapStoreErr(err)
	}
	if err := tx.Commit(); err != nil {
		return memory.PurgeLagSnapshot{}, wrapStoreErr(err)
	}
	result := memory.PurgeLagSnapshot{StoreNow: now, PendingJobs: pending, FailedJobs: failed}
	if oldest.Valid {
		result.OldestCreatedAt = oldest.Time.UTC()
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
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return memory.LifecycleReclaimResult{}, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return memory.LifecycleReclaimResult{}, wrapStoreErr(err)
	}
	result := memory.LifecycleReclaimResult{StoreNow: now}
	rows, err := tx.QueryContext(ctx, `SELECT r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id,r.revision,r.updated_at
		FROM agent_runtime_memory_records r
		WHERE r.content='' AND r.status<>'candidate' AND r.updated_at<=$1
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_receipts p WHERE p.namespace=r.namespace AND p.project_id=r.project_id AND p.subject_id=r.subject_id AND p.collection_id=r.collection_id AND p.expires_at>$2)
		AND EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.status='completed' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND ((j.kind='forget' AND r.subject_generation<=j.purge_before_generation) OR (j.kind IN ('retract','expiry') AND j.collection_id=r.collection_id AND j.memory_id=r.memory_id AND j.expected_revision=r.revision) OR (j.kind='retract_source' AND j.collection_id=r.collection_id AND j.source_ref=r.source_ref AND j.source_epoch=r.source_epoch)))
		AND NOT EXISTS (SELECT 1 FROM agent_runtime_memory_lifecycle_jobs j WHERE j.status<>'completed' AND j.namespace=r.namespace AND j.project_id=r.project_id AND j.subject_id=r.subject_id AND ((j.kind='forget' AND r.subject_generation<=j.purge_before_generation) OR (j.kind IN ('retract','expiry') AND j.collection_id=r.collection_id AND j.memory_id=r.memory_id AND j.expected_revision=r.revision) OR (j.kind='retract_source' AND j.collection_id=r.collection_id AND j.source_ref=r.source_ref AND j.source_epoch=r.source_epoch)))
		ORDER BY r.updated_at,r.namespace,r.project_id,r.subject_id,r.collection_id,r.memory_id LIMIT $3 FOR UPDATE OF r SKIP LOCKED`, now.Add(-memory.LifecycleRetention), now, limit)
	if err != nil {
		return result, wrapStoreErr(err)
	}
	type reclaimRecord struct {
		scope     memory.Scope
		id        string
		revision  uint64
		updatedAt time.Time
	}
	records := make([]reclaimRecord, 0, limit)
	for rows.Next() {
		var record reclaimRecord
		if err := rows.Scan(&record.scope.Namespace, &record.scope.ProjectID, &record.scope.SubjectID, &record.scope.CollectionID, &record.id, &record.revision, &record.updatedAt); err != nil {
			rows.Close()
			return result, wrapStoreErr(err)
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return result, wrapStoreErr(err)
	}
	result.ExaminedControls += len(records)
	for _, record := range records {
		deleted, err := tx.ExecContext(ctx, `DELETE FROM agent_runtime_memory_records WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND memory_id=$5 AND revision=$6 AND updated_at=$7 AND content='' AND status<>'candidate'`, record.scope.Namespace, record.scope.ProjectID, record.scope.SubjectID, record.scope.CollectionID, record.id, record.revision, record.updatedAt)
		if err != nil {
			return result, wrapStoreErr(err)
		}
		if affected, _ := deleted.RowsAffected(); affected != 1 {
			return result, memory.ErrConflict
		}
		updated, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET record_identities=record_identities-1 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND record_identities>0`, record.scope.Namespace, record.scope.ProjectID, record.scope.SubjectID, record.scope.CollectionID)
		if err != nil {
			return result, wrapStoreErr(err)
		}
		if affected, _ := updated.RowsAffected(); affected != 1 {
			return result, memory.ErrConflict
		}
		result.ReclaimedRecords++
		result.ReleasedControls++
	}
	remaining := limit - result.ExaminedControls
	if remaining > 0 {
		rows, err = tx.QueryContext(ctx, `SELECT j.task_id FROM agent_runtime_memory_lifecycle_jobs j
			WHERE j.status='completed' AND NOT EXISTS (
				SELECT 1 FROM agent_runtime_memory_records r WHERE r.content='' AND r.status<>'candidate'
				AND r.namespace=j.namespace AND r.project_id=j.project_id AND r.subject_id=j.subject_id
				AND ((j.kind='forget' AND r.subject_generation<=j.purge_before_generation) OR (j.kind IN ('retract','expiry') AND r.collection_id=j.collection_id AND r.memory_id=j.memory_id AND r.revision=j.expected_revision) OR (j.kind='retract_source' AND r.collection_id=j.collection_id AND r.source_ref=j.source_ref AND r.source_epoch=j.source_epoch)))
			ORDER BY j.completed_at,j.task_id LIMIT $1 FOR UPDATE OF j SKIP LOCKED`, remaining)
		if err != nil {
			return result, wrapStoreErr(err)
		}
		jobs := make([]string, 0, remaining)
		for rows.Next() {
			var taskID string
			if err := rows.Scan(&taskID); err != nil {
				rows.Close()
				return result, wrapStoreErr(err)
			}
			jobs = append(jobs, taskID)
		}
		if err := rows.Close(); err != nil {
			return result, wrapStoreErr(err)
		}
		result.ExaminedControls += len(jobs)
		for _, taskID := range jobs {
			deleted, err := tx.ExecContext(ctx, `DELETE FROM agent_runtime_memory_lifecycle_jobs WHERE task_id=$1 AND status='completed'`, taskID)
			if err != nil {
				return result, wrapStoreErr(err)
			}
			if affected, _ := deleted.RowsAffected(); affected != 1 {
				return result, memory.ErrConflict
			}
			result.ReclaimedJobs++
		}
	}
	s.mu.RLock()
	beforeCommit := s.lifecycleCommitFailure
	s.mu.RUnlock()
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	s.mu.RLock()
	after := s.afterCommitFailure
	s.mu.RUnlock()
	if after != nil {
		if err := after(); err != nil {
			return result, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
		}
	}
	return result, nil
}

func loadPostgresLifecycleJob(ctx context.Context, tx *sql.Tx, taskID string, lock bool) (memory.PurgeJob, error) {
	if taskID == "" {
		return memory.PurgeJob{}, memory.ErrInvalid
	}
	query := `SELECT ` + postgresLifecycleColumns + ` FROM agent_runtime_memory_lifecycle_jobs WHERE task_id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	job, err := scanPostgresLifecycleJob(tx.QueryRowContext(ctx, query, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.PurgeJob{}, memory.ErrNotFound
	}
	return job, wrapStoreErr(err)
}

func validatePostgresLifecycleClaim(current, provided memory.PurgeJob, now time.Time) error {
	if current.Status != memory.PurgeJobLeased || current.OwnerToken == "" || current.OwnerToken != provided.OwnerToken || current.LeaseEpoch == 0 || current.LeaseEpoch != provided.LeaseEpoch || !sameInstant(current.LeaseExpiresAt, provided.LeaseExpiresAt) || !current.LeaseExpiresAt.After(now) || current.CursorCollectionID != provided.CursorCollectionID || current.CursorMemoryID != provided.CursorMemoryID {
		return fmt.Errorf("%w: lifecycle lease", memory.ErrConflict)
	}
	return nil
}

func selectPostgresLifecycleTargets(ctx context.Context, tx *sql.Tx, job memory.PurgeJob, limit int) ([]postgresLifecycleTarget, error) {
	query := `SELECT namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,status,updated_at,expires_at,source_ref,source_epoch,(content<>''),octet_length(content)
		FROM agent_runtime_memory_records WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND (collection_id>$4 OR (collection_id=$4 AND memory_id>$5))`
	args := []any{job.Subject.Namespace, job.Subject.ProjectID, job.Subject.SubjectID, job.CursorCollectionID, job.CursorMemoryID}
	if job.Scope.CollectionID != "" {
		query += fmt.Sprintf(` AND collection_id=$%d`, len(args)+1)
		args = append(args, job.Scope.CollectionID)
	}
	query += fmt.Sprintf(` ORDER BY collection_id,memory_id LIMIT $%d FOR UPDATE`, len(args)+1)
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	defer rows.Close()
	targets := make([]postgresLifecycleTarget, 0, limit)
	for rows.Next() {
		var scope memory.Scope
		var record memory.Record
		var hasContent bool
		var contentBytes int
		if err := rows.Scan(&scope.Namespace, &scope.ProjectID, &scope.SubjectID, &scope.CollectionID, &record.MemoryID, &record.Revision, &record.SubjectGeneration, &record.Status, &record.UpdatedAt, &record.ExpiresAt, &record.Provenance.SourceRef, &record.Provenance.SourceEpoch, &hasContent, &contentBytes); err != nil {
			return nil, wrapStoreErr(err)
		}
		record.Scope, record.UpdatedAt, record.ExpiresAt = scope, record.UpdatedAt.UTC(), record.ExpiresAt.UTC()
		if hasContent {
			record.Content = "present"
		}
		targets = append(targets, postgresLifecycleTarget{scope: scope, id: record.MemoryID, revision: record.Revision, bytes: contentBytes, record: &record})
	}
	return targets, wrapStoreErr(rows.Err())
}

func postgresLifecycleEligible(job memory.PurgeJob, record *memory.Record) bool {
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

func validPostgresLifecycleText(value string, max int) bool {
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
