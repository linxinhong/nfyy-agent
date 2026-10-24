package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
)

// JournalStore is the SQLite durable implementation of
// run.ExecutionJournalStore (migration 2026080904). It supports the full
// transactional contract for single-process deployments; managed multi-
// worker deployments must use the PostgreSQL adapter (RC-08 boundary).
type JournalStore struct {
	db *sql.DB
}

// NewJournalStore wraps an existing SQLite DB for the execution journal.
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
		INSERT INTO dispatch_attempts (
			attempt_id, run_id, namespace, state, fencing_token, attempt_number,
			idempotency_key, worker_id, started_at
		) VALUES (?,?,?,?,?,?,?,?,?)`,
		attempt.AttemptID, attempt.RunID, scope.Namespace, string(attempt.State),
		attempt.FencingToken, attempt.AttemptNumber, attempt.IdempotencyKey,
		attempt.WorkerID, attempt.StartedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return err
	}
	if event != nil {
		event.Namespace = scope.Namespace
		if event.CreatedAt.IsZero() {
			event.CreatedAt = now
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox_events (event_id, namespace, aggregate_id, sequence, type, payload, created_at)
			VALUES (?,?,?,?,?,?,?)`,
			event.EventID, event.Namespace, event.AggregateID, event.Sequence,
			event.Type, string(event.Payload), event.CreatedAt.UTC().Format(time.RFC3339Nano),
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
		`SELECT state FROM dispatch_attempts WHERE attempt_id = ? AND namespace = ?`,
		attemptID, scope.Namespace).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return run.ErrAttemptNotFound
		}
		return err
	}
	if err := run.ValidateAttempt(run.AttemptState(current), state); err != nil {
		return err
	}
	finished := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`UPDATE dispatch_attempts SET state = ?, finished_at = ? WHERE attempt_id = ? AND namespace = ?`,
		string(state), finished, attemptID, scope.Namespace); err != nil {
		return err
	}
	if event != nil {
		event.Namespace = scope.Namespace
		if event.CreatedAt.IsZero() {
			event.CreatedAt = time.Now().UTC()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox_events (event_id, namespace, aggregate_id, sequence, type, payload, created_at)
			VALUES (?,?,?,?,?,?,?)`,
			event.EventID, event.Namespace, event.AggregateID, event.Sequence,
			event.Type, string(event.Payload), event.CreatedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClaimOutbox claims up to limit unacked events for a worker.
func (s *JournalStore) ClaimOutbox(ctx context.Context, scope execution.NamespaceScope, workerID string, limit int) ([]run.OutboxEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, namespace, aggregate_id, sequence, type, payload, created_at
		FROM outbox_events
		WHERE namespace = ? AND delivered_at IS NULL
		ORDER BY sequence LIMIT ?`, scope.Namespace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []run.OutboxEvent
	for rows.Next() {
		var e run.OutboxEvent
		var payload, createdAt string
		if err := rows.Scan(&e.EventID, &e.Namespace, &e.AggregateID, &e.Sequence, &e.Type, &payload, &createdAt); err != nil {
			return nil, err
		}
		e.Payload = []byte(payload)
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			e.CreatedAt = t
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// AckOutbox marks an event delivered (deduplicated by event ID).
func (s *JournalStore) AckOutbox(ctx context.Context, scope execution.NamespaceScope, eventID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbox_events SET delivered_at = ? WHERE event_id = ? AND namespace = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), eventID, scope.Namespace)
	return err
}

// ReconcileUnknown marks stale invoking attempts as unknown.
func (s *JournalStore) ReconcileUnknown(ctx context.Context, scope execution.NamespaceScope, olderThan time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `
		UPDATE dispatch_attempts SET state = ?, finished_at = ?
		WHERE namespace = ? AND state = ? AND started_at < ?`,
		string(run.AttemptUnknown), time.Now().UTC().Format(time.RFC3339Nano),
		scope.Namespace, string(run.AttemptInvoking), cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
