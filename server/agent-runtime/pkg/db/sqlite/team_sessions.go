// Package sqlite provides a SQLite-backed implementation of the db.DB interface.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"time"
)

func (d *DB) CreateTeamSession(ctx context.Context, s *session.TeamSession) error {
	if s == nil {
		return fmt.Errorf("session is nil")
	}
	if s.SessionID == "" {
		s.SessionID = generateID()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = s.CreatedAt
	}
	if s.Revision == 0 {
		s.Revision = 1
	}
	namespace, err := db.NamespaceFor(ctx, s.Namespace)
	if err != nil {
		return err
	}
	s.Namespace = namespace
	for _, run := range s.Runs {
		if run.Namespace == "" {
			run.Namespace = namespace
		}
		if run.Namespace != namespace {
			return db.ErrNotFound
		}
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if exists, err := managedSessionExists(ctx, tx, s.SessionID); err != nil {
		return err
	} else if exists {
		return db.ErrConflict
	}

	metadataJSON, err := toJSON(s.Metadata)
	if err != nil {
		return err
	}
	membersJSON, err := toJSON(s.Members)
	if err != nil {
		return err
	}
	sessionStateJSON, err := toJSON(s.SessionState)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO team_sessions (session_id, team_id, user_id, members, metadata, session_state, summary, revision, created_at, updated_at, namespace)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.SessionID, s.TeamID, s.UserID, membersJSON, metadataJSON, sessionStateJSON, s.Summary,
		s.Revision,
		s.CreatedAt.Format(time.RFC3339Nano), s.UpdatedAt.Format(time.RFC3339Nano), namespace,
	); err != nil {
		return fmt.Errorf("insert team session: %w", err)
	}

	if err := d.insertTeamRuns(ctx, tx, s.SessionID, s.Runs); err != nil {
		return err
	}

	return tx.Commit()
}

// ReadTeamSession loads a team session along with its runs and messages.
func (d *DB) ReadTeamSession(ctx context.Context, sessionID string) (*session.TeamSession, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is empty")
	}

	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return nil, err
	}
	row := d.db.QueryRowContext(ctx,
		`SELECT session_id, team_id, user_id, members, metadata, session_state, summary, revision, created_at, updated_at
		 FROM team_sessions WHERE namespace=? AND session_id = ?`, namespace, sessionID)

	s, err := d.scanTeamSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	s.Namespace = namespace
	runs, err := d.loadTeamRunsInNamespace(ctx, namespace, sessionID)
	if err != nil {
		return nil, err
	}
	s.Runs = runs
	return s, nil
}

// UpdateTeamSession updates session fields using revision CAS.
func (d *DB) UpdateTeamSession(ctx context.Context, s *session.TeamSession) error {
	if s == nil {
		return fmt.Errorf("session is nil")
	}
	if s.SessionID == "" {
		return fmt.Errorf("session_id is empty")
	}
	namespace, err := db.NamespaceFor(ctx, s.Namespace)
	if err != nil {
		return err
	}
	s.Namespace = namespace

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}

	metadataJSON, err := toJSON(s.Metadata)
	if err != nil {
		return err
	}
	membersJSON, err := toJSON(s.Members)
	if err != nil {
		return err
	}
	sessionStateJSON, err := toJSON(s.SessionState)
	if err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE team_sessions SET team_id = ?, user_id = ?, members = ?, metadata = ?, session_state = ?, summary = ?, revision = revision + 1, updated_at = ?
		 WHERE namespace=? AND session_id = ? AND revision = ?`,
		s.TeamID, s.UserID, membersJSON, metadataJSON, sessionStateJSON, s.Summary,
		s.UpdatedAt.Format(time.RFC3339Nano), namespace, s.SessionID, s.Revision,
	)
	if err != nil {
		return fmt.Errorf("update team session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return d.sessionUpdateMissScoped(ctx, tx, "team_sessions", s.SessionID, namespace)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.Revision++
	return nil
}

// DeleteTeamSession removes a team session and its related runs/messages.
func (d *DB) DeleteTeamSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is empty")
	}

	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return err
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM team_sessions WHERE namespace=? AND session_id = ?`, namespace, sessionID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

// ListTeamSessions returns team sessions for a team, or all sessions when teamID is empty.
func (d *DB) ListTeamSessions(ctx context.Context, query db.SessionQuery) ([]*session.TeamSession, error) {
	if query.Offset < 0 {
		query.Offset = 0
	}
	namespace, err := db.NamespaceFor(ctx, query.Namespace)
	if err != nil {
		return nil, err
	}

	var rows *sql.Rows
	if query.Limit > 0 {
		rows, err = d.db.QueryContext(ctx,
			`SELECT session_id, team_id, user_id, members, metadata, session_state, summary, revision, created_at, updated_at
			 FROM team_sessions WHERE namespace=? AND (? = '' OR team_id = ?) AND (? = '' OR user_id = ?) ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
			namespace, query.ComponentID, query.ComponentID, query.UserID, query.UserID, query.Limit, query.Offset)
	} else {
		rows, err = d.db.QueryContext(ctx,
			`SELECT session_id, team_id, user_id, members, metadata, session_state, summary, revision, created_at, updated_at
			 FROM team_sessions WHERE namespace=? AND (? = '' OR team_id = ?) AND (? = '' OR user_id = ?) ORDER BY updated_at DESC LIMIT -1 OFFSET ?`,
			namespace, query.ComponentID, query.ComponentID, query.UserID, query.UserID, query.Offset)
	}
	if err != nil {
		return nil, err
	}
	var result []*session.TeamSession
	for rows.Next() {
		s, err := d.scanTeamSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, s := range result {
		s.Namespace = namespace
		runs, err := d.loadTeamRunsInNamespace(ctx, namespace, s.SessionID)
		if err != nil {
			return nil, err
		}
		s.Runs = runs
	}
	return result, nil
}

// CreateWorkflowSession inserts a new workflow session including all of its runs and messages.
func (d *DB) insertTeamRuns(ctx context.Context, tx *sql.Tx, sessionID string, runs []*session.RunOutput) error {
	for _, run := range runs {
		if run.RunID == "" {
			run.RunID = generateID()
		}
		if run.SessionID == "" {
			run.SessionID = sessionID
		}
		if run.CreatedAt.IsZero() {
			run.CreatedAt = time.Now().UTC()
		}
		if run.UpdatedAt.IsZero() {
			run.UpdatedAt = run.CreatedAt
		}
		namespace, err := db.NamespaceFor(ctx, run.Namespace)
		if err != nil {
			return err
		}
		run.Namespace = namespace
		if exists, err := managedRunExists(ctx, tx, namespace, run.RunID); err != nil {
			return err
		} else if exists {
			return db.ErrConflict
		}

		inputJSON, err := toJSON(run.Input)
		if err != nil {
			return err
		}
		toolCallsJSON, err := encodeRunEnvelope(run)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO team_runs (run_id, session_id, team_id, user_id, input, content, tool_calls, status, error, parent_run_id, node_id, created_at, updated_at, namespace)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			run.RunID, run.SessionID, run.TeamID, run.UserID, inputJSON, run.Content,
			toolCallsJSON, string(run.Status), run.Error, run.ParentRunID, run.NodeID,
			run.CreatedAt.Format(time.RFC3339Nano), run.UpdatedAt.Format(time.RFC3339Nano), namespace,
		); err != nil {
			return fmt.Errorf("insert team run: %w", err)
		}

		if err := d.insertTeamRunMessages(execution.ContextWithNamespace(ctx, namespace), tx, run.RunID, run.Messages); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) insertTeamRunMessages(ctx context.Context, tx *sql.Tx, runID string, messages []models.Message) error {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return err
	}
	for i, msg := range messages {
		msgJSON, err := toJSON(msg)
		if err != nil {
			return err
		}
		createdAt := time.Now().UTC().Add(time.Duration(i) * time.Microsecond)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO team_run_messages (run_id, message, created_at, namespace) VALUES (?, ?, ?, ?)`,
			runID, msgJSON, createdAt.Format(time.RFC3339Nano), namespace,
		); err != nil {
			return fmt.Errorf("insert team run message: %w", err)
		}
	}
	return nil
}

func (d *DB) loadTeamRuns(ctx context.Context, sessionID string) ([]*session.RunOutput, error) {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return nil, err
	}
	return d.loadTeamRunsInNamespace(ctx, namespace, sessionID)
}

func (d *DB) loadTeamRunsInNamespace(ctx context.Context, namespace, sessionID string) ([]*session.RunOutput, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT run_id, session_id, team_id, user_id, input, content, tool_calls, status, error, parent_run_id, node_id, created_at, updated_at
		 FROM team_runs WHERE namespace=? AND session_id = ? ORDER BY created_at ASC`, namespace, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []*session.RunOutput
	for rows.Next() {
		run, err := d.scanTeamRun(rows)
		if err != nil {
			return nil, err
		}
		run.Namespace = namespace
		msgs, err := d.loadTeamRunMessagesInNamespace(ctx, namespace, run.RunID)
		if err != nil {
			return nil, err
		}
		run.Messages = msgs
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return runs, nil
}

func (d *DB) loadTeamRunMessages(ctx context.Context, runID string) ([]models.Message, error) {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return nil, err
	}
	return d.loadTeamRunMessagesInNamespace(ctx, namespace, runID)
}

func (d *DB) loadTeamRunMessagesInNamespace(ctx context.Context, namespace, runID string) ([]models.Message, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT message FROM team_run_messages WHERE namespace=? AND run_id = ? ORDER BY message_id ASC`, namespace, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []models.Message
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var msg models.Message
		if err := fromJSON(raw, &msg); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func (d *DB) scanTeamSession(scanner interface {
	Scan(dest ...any) error
}) (*session.TeamSession, error) {
	var s session.TeamSession
	var metadataRaw, membersRaw, sessionStateRaw, summaryRaw sql.NullString
	var createdAtStr, updatedAtStr string

	if err := scanner.Scan(&s.SessionID, &s.TeamID, &s.UserID, &membersRaw, &metadataRaw, &sessionStateRaw, &summaryRaw, &s.Revision, &createdAtStr, &updatedAtStr); err != nil {
		return nil, err
	}

	if membersRaw.Valid && membersRaw.String != "" {
		if err := fromJSON(membersRaw.String, &s.Members); err != nil {
			return nil, err
		}
	}
	if metadataRaw.Valid && metadataRaw.String != "" {
		if err := fromJSON(metadataRaw.String, &s.Metadata); err != nil {
			return nil, err
		}
	}
	if sessionStateRaw.Valid && sessionStateRaw.String != "" {
		if err := fromJSON(sessionStateRaw.String, &s.SessionState); err != nil {
			return nil, err
		}
	}
	s.Summary = summaryRaw.String

	createdAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtStr)
	if err != nil {
		return nil, err
	}
	s.CreatedAt = createdAt
	s.UpdatedAt = updatedAt
	return &s, nil
}

func (d *DB) scanTeamRun(scanner interface {
	Scan(dest ...any) error
}) (*session.RunOutput, error) {
	var run session.RunOutput
	var inputRaw, toolCallsRaw sql.NullString
	var statusStr, createdAtStr, updatedAtStr string

	if err := scanner.Scan(&run.RunID, &run.SessionID, &run.TeamID, &run.UserID,
		&inputRaw, &run.Content, &toolCallsRaw, &statusStr, &run.Error, &run.ParentRunID, &run.NodeID,
		&createdAtStr, &updatedAtStr); err != nil {
		return nil, err
	}

	if inputRaw.Valid && inputRaw.String != "" {
		if err := fromJSON(inputRaw.String, &run.Input); err != nil {
			return nil, err
		}
	}
	if toolCallsRaw.Valid && toolCallsRaw.String != "" {
		if err := decodeRunEnvelope(toolCallsRaw.String, &run); err != nil {
			return nil, err
		}
	}

	createdAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtStr)
	if err != nil {
		return nil, err
	}
	run.CreatedAt = createdAt
	run.UpdatedAt = updatedAt
	run.Status = session.RunStatus(statusStr)
	return &run, nil
}
