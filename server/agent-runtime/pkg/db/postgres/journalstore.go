package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
)

// jsonOrNil returns a NULL-capable value for a JSONB column: an empty (nil
// or zero-length) payload is stored as SQL NULL, because PostgreSQL rejects
// an empty string cast to json/jsonb.
func jsonOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// JournalStore is the PostgreSQL durable implementation of
// run.ExecutionJournalStore (migration 2026080904). Unlike the SQLite/in-
// memory references, it provides the multi-worker atomic claim/heartbeat
// surface that managed multi-worker deployments require.
type JournalStore struct {
	db *sql.DB
}

// NewJournalStore wraps an existing PostgreSQL DB for the execution journal.
func NewJournalStore(d *DB) *JournalStore {
	return &JournalStore{db: d.db}
}

var _ run.ExecutionJournalStore = (*JournalStore)(nil)

// BeginAttemptAndAppendRequested writes the attempt, requested audit
// record, and outbox event in one transaction.
func (s *JournalStore) BeginAttemptAndAppendRequested(ctx context.Context, scope execution.NamespaceScope, attempt *run.Attempt, requested *run.AuditRequestedRecord, event *run.OutboxEvent) error {
	if attempt == nil || attempt.AttemptID == "" {
		return run.ErrAttemptNotFound
	}
	now := time.Now().UTC()
	if attempt.StartedAt.IsZero() {
		attempt.StartedAt = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_runtime_dispatch_attempts (
			attempt_id, run_id, namespace, state, fencing_token, attempt_number,
			idempotency_key, worker_id, started_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		attempt.AttemptID, attempt.RunID, scope.Namespace, string(attempt.State),
		int64(attempt.FencingToken), attempt.AttemptNumber,
		nullString(attempt.IdempotencyKey), nullString(attempt.WorkerID),
		attempt.StartedAt.UTC(),
	); err != nil {
		return err
	}
	if event != nil {
		event.Namespace = scope.Namespace
		if event.CreatedAt.IsZero() {
			event.CreatedAt = now
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_runtime_outbox (
				event_id, namespace, aggregate_id, sequence, type, payload, claimed_by, created_at
			) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8)`,
			event.EventID, event.Namespace, event.AggregateID, int64(event.Sequence),
			event.Type, jsonOrNil(event.Payload), nullString(event.ClaimedBy),
			event.CreatedAt.UTC(),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AppendOutcome appends a terminal/unknown outcome plus an outbox event.
func (s *JournalStore) AppendOutcome(ctx context.Context, scope execution.NamespaceScope, attemptID string, state run.AttemptState, event *run.OutboxEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM agent_runtime_dispatch_attempts WHERE attempt_id = $1 AND namespace = $2`,
		attemptID, scope.Namespace).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return run.ErrAttemptNotFound
		}
		return err
	}
	if err := run.ValidateAttempt(run.AttemptState(current), state); err != nil {
		return err
	}
	finished := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_runtime_dispatch_attempts SET state = $1, finished_at = $2 WHERE attempt_id = $3 AND namespace = $4`,
		string(state), finished, attemptID, scope.Namespace); err != nil {
		return err
	}
	if event != nil {
		event.Namespace = scope.Namespace
		if event.CreatedAt.IsZero() {
			event.CreatedAt = finished
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_runtime_outbox (
				event_id, namespace, aggregate_id, sequence, type, payload, claimed_by, created_at
			) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8)`,
			event.EventID, event.Namespace, event.AggregateID, int64(event.Sequence),
			event.Type, jsonOrNil(event.Payload), nullString(event.ClaimedBy),
			event.CreatedAt.UTC(),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClaimOutbox claims up to limit undelivered events for a worker.
func (s *JournalStore) ClaimOutbox(ctx context.Context, scope execution.NamespaceScope, workerID string, limit int) ([]run.OutboxEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, namespace, aggregate_id, sequence, type, payload, created_at
		FROM agent_runtime_outbox
		WHERE namespace = $1 AND delivered_at IS NULL
		ORDER BY sequence LIMIT $2`, scope.Namespace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []run.OutboxEvent
	for rows.Next() {
		var e run.OutboxEvent
		var payload []byte
		var seq int64
		var createdAt time.Time
		if err := rows.Scan(&e.EventID, &e.Namespace, &e.AggregateID, &seq, &e.Type, &payload, &createdAt); err != nil {
			return nil, err
		}
		e.Sequence = uint64(seq)
		e.Payload = payload
		e.CreatedAt = createdAt.UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}

// AckOutbox marks an event delivered (deduplicated by event ID).
func (s *JournalStore) AckOutbox(ctx context.Context, scope execution.NamespaceScope, eventID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE agent_runtime_outbox SET delivered_at = $1 WHERE event_id = $2 AND namespace = $3`,
		time.Now().UTC(), eventID, scope.Namespace)
	return err
}

// ReconcileUnknown marks stale invoking attempts as unknown.
func (s *JournalStore) ReconcileUnknown(ctx context.Context, scope execution.NamespaceScope, olderThan time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_runtime_dispatch_attempts SET state = $1, finished_at = $2
		WHERE namespace = $3 AND state = $4 AND started_at < $5`,
		string(run.AttemptUnknown), time.Now().UTC(),
		scope.Namespace, string(run.AttemptInvoking), cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
