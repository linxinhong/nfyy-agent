// Package sqlite provides a SQLite-backed implementation of the db.DB interface.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	_ "modernc.org/sqlite"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrations embed.FS

// DB is a SQLite-backed db.DB implementation.
type DB struct {
	db *sql.DB
}

type Options struct{ AutoMigrate bool }

// compile-time interface check.
var _ db.DB = (*DB)(nil)

// New opens or creates a SQLite database at dataSourceName and initializes tables.
// If dataSourceName is empty, an in-memory database is used.
func New(dataSourceName string) (*DB, error) {
	return NewWithOptions(dataSourceName, Options{AutoMigrate: true})
}

// NewWithOptions opens SQLite and optionally applies embedded migrations.
func NewWithOptions(dataSourceName string, options Options) (*DB, error) {
	if dataSourceName == "" {
		dataSourceName = ":memory:"
	}

	// Enable foreign keys and a busy timeout for every connection via the
	// DSN pragma (modernc.org/sqlite's _pragma query parameter). The busy
	// timeout lets concurrent writers wait instead of failing with
	// SQLITE_BUSY.
	if strings.Contains(dataSourceName, "?") {
		dataSourceName += "&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	} else {
		dataSourceName += "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	}

	conn, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	d := &DB{db: conn}
	var initErr error
	if options.AutoMigrate {
		initErr = d.createTables()
	} else {
		initErr = d.checkSchemaVersion()
	}
	if initErr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("initialize sqlite schema: %w", initErr)
	}

	return d, nil
}

func (d *DB) checkSchemaVersion() error {
	var version int
	if err := d.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM agent_runtime_schema_migrations`).Scan(&version); err != nil || version != 11 {
		return fmt.Errorf("sqlite schema version is not 11; enable AutoMigrate")
	}
	return nil
}

func (d *DB) createTables() error {
	if err := d.applyMigrations(context.Background()); err != nil {
		return err
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			session_id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			user_id TEXT,
			metadata TEXT,
			session_state TEXT,
			summary TEXT,
			revision INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_agent_id ON sessions(agent_id)`,

		`CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			agent_id TEXT,
			user_id TEXT,
			input TEXT,
			content TEXT,
			tool_calls TEXT,
			status TEXT,
			error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (session_id) REFERENCES sessions(session_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_session_id ON runs(session_id)`,

		`CREATE TABLE IF NOT EXISTS run_messages (
			message_id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL,
			message TEXT NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY (run_id) REFERENCES runs(run_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_run_messages_run_id ON run_messages(run_id)`,

		`CREATE TABLE IF NOT EXISTS memories (
			memory_id TEXT PRIMARY KEY,
			memory TEXT NOT NULL,
			topics TEXT,
			user_id TEXT,
			agent_id TEXT,
			team_id TEXT,
			input TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_user_agent ON memories(user_id, agent_id)`,

		`CREATE TABLE IF NOT EXISTS team_sessions (
			session_id TEXT PRIMARY KEY,
			team_id TEXT NOT NULL,
			user_id TEXT,
			members TEXT,
			metadata TEXT,
			session_state TEXT,
			summary TEXT,
			revision INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_team_sessions_team_id ON team_sessions(team_id)`,

		`CREATE TABLE IF NOT EXISTS team_runs (
			run_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			team_id TEXT,
			user_id TEXT,
			input TEXT,
			content TEXT,
			tool_calls TEXT,
			status TEXT,
			error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (session_id) REFERENCES team_sessions(session_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_team_runs_session_id ON team_runs(session_id)`,

		`CREATE TABLE IF NOT EXISTS team_run_messages (
			message_id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL,
			message TEXT NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY (run_id) REFERENCES team_runs(run_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_team_run_messages_run_id ON team_run_messages(run_id)`,

		`CREATE TABLE IF NOT EXISTS workflow_sessions (
			session_id TEXT PRIMARY KEY,
			workflow_id TEXT NOT NULL,
			user_id TEXT,
			metadata TEXT,
			session_state TEXT,
			summary TEXT,
			revision INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_sessions_workflow_id ON workflow_sessions(workflow_id)`,

		`CREATE TABLE IF NOT EXISTS workflow_runs (
			run_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			workflow_id TEXT,
			user_id TEXT,
			input TEXT,
			content TEXT,
			tool_calls TEXT,
			status TEXT,
			error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (session_id) REFERENCES workflow_sessions(session_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_runs_session_id ON workflow_runs(session_id)`,

		`CREATE TABLE IF NOT EXISTS workflow_run_messages (
			message_id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL,
			message TEXT NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY (run_id) REFERENCES workflow_runs(run_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_run_messages_run_id ON workflow_run_messages(run_id)`,
	}

	for _, stmt := range schema {
		if _, err := d.db.Exec(stmt); err != nil {
			return fmt.Errorf("execute schema: %w", err)
		}
	}
	if err := d.ensureSessionColumns(); err != nil {
		return err
	}
	return nil
}

// ensureSessionStateColumns upgrades databases created before SessionState was
// persisted. SQLite ADD COLUMN is safe for existing rows, which read as NULL.
func (d *DB) ensureSessionColumns() error {
	for _, table := range []string{"sessions", "team_sessions", "workflow_sessions"} {
		rows, err := d.db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			return fmt.Errorf("inspect %s schema: %w", table, err)
		}
		hasState, hasRevision := false, false
		for rows.Next() {
			var cid int
			var name, columnType string
			var notNull, primaryKey int
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				rows.Close()
				return fmt.Errorf("read %s schema: %w", table, err)
			}
			hasState = hasState || name == "session_state"
			hasRevision = hasRevision || name == "revision"
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !hasState {
			if _, err := d.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN session_state TEXT`); err != nil {
				return fmt.Errorf("add %s.session_state: %w", table, err)
			}
		}
		if !hasRevision {
			if _, err := d.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`); err != nil {
				return fmt.Errorf("add %s.revision: %w", table, err)
			}
		}
	}
	return nil
}

// CreateSession inserts a new session including all of its runs and messages.
func (d *DB) CreateSession(ctx context.Context, s *session.AgentSession) error {
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
	sessionStateJSON, err := toJSON(s.SessionState)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (session_id, agent_id, user_id, metadata, session_state, summary, revision, created_at, updated_at, namespace)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.SessionID, s.AgentID, s.UserID, metadataJSON, sessionStateJSON, s.Summary,
		s.Revision,
		s.CreatedAt.Format(time.RFC3339Nano), s.UpdatedAt.Format(time.RFC3339Nano), namespace,
	); err != nil {
		return fmt.Errorf("insert session: %w", err)
	}

	if err := d.insertRuns(ctx, tx, s.SessionID, s.Runs); err != nil {
		return err
	}

	return tx.Commit()
}

// ReadSession loads a session along with its runs and messages.
func (d *DB) ReadSession(ctx context.Context, sessionID string) (*session.AgentSession, error) {
	return d.ReadSessionRecent(ctx, sessionID, -1)
}

var _ db.RecentSessionReader = (*DB)(nil)

// ReadSessionRecent reads a session with only its newest maxRuns runs; zero
// or negative loads every run.
func (d *DB) ReadSessionRecent(ctx context.Context, sessionID string, maxRuns int) (*session.AgentSession, error) {
	if maxRuns <= 0 {
		maxRuns = -1
	}
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is empty")
	}

	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return nil, err
	}
	row := d.db.QueryRowContext(ctx,
		`SELECT session_id, agent_id, user_id, metadata, session_state, summary, revision, created_at, updated_at
		 FROM sessions WHERE session_id = ? AND namespace = ?`, sessionID, namespace)

	s, err := d.scanSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	s.Namespace = namespace
	runs, err := d.loadRecentRunsInNamespace(ctx, namespace, sessionID, maxRuns)
	if err != nil {
		return nil, err
	}
	s.Runs = runs
	return s, nil
}

// UpdateSession updates session fields using revision CAS. Runs remain append-only.
func (d *DB) UpdateSession(ctx context.Context, s *session.AgentSession) error {
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
	sessionStateJSON, err := toJSON(s.SessionState)
	if err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET agent_id = ?, user_id = ?, metadata = ?, session_state = ?, summary = ?, revision = revision + 1, updated_at = ?
		 WHERE session_id = ? AND namespace = ? AND revision = ?`,
		s.AgentID, s.UserID, metadataJSON, sessionStateJSON, s.Summary,
		s.UpdatedAt.Format(time.RFC3339Nano), s.SessionID, namespace, s.Revision,
	)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return d.sessionUpdateMissScoped(ctx, tx, "sessions", s.SessionID, namespace)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.Revision++
	return nil
}

// DeleteSession removes a session and its related runs/messages.
func (d *DB) DeleteSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is empty")
	}

	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return err
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM sessions WHERE session_id = ? AND namespace = ?`, sessionID, namespace)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

// ListSessions returns sessions for an agent, or all sessions when agentID is empty.
func (d *DB) ListSessions(ctx context.Context, query db.SessionQuery) ([]*session.AgentSession, error) {
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
			`SELECT session_id, agent_id, user_id, metadata, session_state, summary, revision, created_at, updated_at
			 FROM sessions WHERE namespace = ? AND (? = '' OR agent_id = ?) AND (? = '' OR user_id = ?) ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
			namespace, query.ComponentID, query.ComponentID, query.UserID, query.UserID, query.Limit, query.Offset)
	} else {
		rows, err = d.db.QueryContext(ctx,
			`SELECT session_id, agent_id, user_id, metadata, session_state, summary, revision, created_at, updated_at
			 FROM sessions WHERE namespace = ? AND (? = '' OR agent_id = ?) AND (? = '' OR user_id = ?) ORDER BY updated_at DESC LIMIT -1 OFFSET ?`,
			namespace, query.ComponentID, query.ComponentID, query.UserID, query.UserID, query.Offset)
	}
	if err != nil {
		return nil, err
	}
	var result []*session.AgentSession
	for rows.Next() {
		s, err := d.scanSession(rows)
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
		runs, err := d.loadRunsInNamespace(ctx, namespace, s.SessionID)
		if err != nil {
			return nil, err
		}
		s.Runs = runs
	}
	return result, nil
}

// GetRun returns a run by id from any supported session type, optionally
// restricted to the owning user.
func (d *DB) GetRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	if query.RunID == "" {
		return nil, fmt.Errorf("run_id is empty")
	}

	if run, err := d.getAgentRun(ctx, query); err != nil || run != nil {
		return run, err
	}
	if run, err := d.getTeamRun(ctx, query); err != nil || run != nil {
		return run, err
	}
	return d.getWorkflowRun(ctx, query)
}

// AppendRun stores one run and only updates the timestamp of its parent
// session. Existing run records and messages are left intact.
func (d *DB) AppendRun(ctx context.Context, run *session.RunOutput) error {
	if run == nil {
		return fmt.Errorf("run is nil")
	}
	if run.SessionID == "" {
		return fmt.Errorf("session_id is empty")
	}
	namespace, err := db.NamespaceFor(ctx, run.Namespace)
	if err != nil {
		return err
	}
	run.Namespace = namespace

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if run.RunID != "" {
		exists, err := managedRunExists(ctx, tx, namespace, run.RunID)
		if err != nil {
			return err
		}
		if exists {
			return db.ErrConflict
		}
	}

	updatedAt := time.Now().UTC()
	var res sql.Result
	var insert func() error
	switch {
	case run.WorkflowID != "":
		res, err = tx.ExecContext(ctx,
			`UPDATE workflow_sessions SET updated_at = ? WHERE namespace = ? AND session_id = ? AND workflow_id = ? AND user_id = ?`,
			updatedAt.Format(time.RFC3339Nano), namespace, run.SessionID, run.WorkflowID, run.UserID)
		insert = func() error { return d.insertWorkflowRuns(ctx, tx, run.SessionID, []*session.RunOutput{run}) }
	case run.TeamID != "":
		res, err = tx.ExecContext(ctx,
			`UPDATE team_sessions SET updated_at = ? WHERE namespace = ? AND session_id = ? AND team_id = ? AND user_id = ?`,
			updatedAt.Format(time.RFC3339Nano), namespace, run.SessionID, run.TeamID, run.UserID)
		insert = func() error { return d.insertTeamRuns(ctx, tx, run.SessionID, []*session.RunOutput{run}) }
	case run.AgentID != "":
		res, err = tx.ExecContext(ctx,
			`UPDATE sessions SET updated_at = ? WHERE namespace = ? AND session_id = ? AND agent_id = ? AND user_id = ?`,
			updatedAt.Format(time.RFC3339Nano), namespace, run.SessionID, run.AgentID, run.UserID)
		insert = func() error { return d.insertRuns(ctx, tx, run.SessionID, []*session.RunOutput{run}) }
	default:
		return fmt.Errorf("run has no component id")
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("parent session not found: %s", run.SessionID)
	}
	if err := insert(); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateRun atomically replaces a run when it is still in expectedStatus.
func (d *DB) UpdateRun(ctx context.Context, run *session.RunOutput, expectedStatus session.RunStatus) error {
	if run == nil || run.RunID == "" {
		return fmt.Errorf("run is nil or run_id is empty")
	}
	if !expectedStatus.CanTransitionTo(run.Status) {
		return db.ErrInvalidTransition
	}
	namespace, err := db.NamespaceFor(ctx, run.Namespace)
	if err != nil {
		return err
	}
	run.Namespace = namespace
	inputJSON, err := toJSON(run.Input)
	if err != nil {
		return err
	}
	toolCallsJSON, err := encodeRunEnvelope(run)
	if err != nil {
		return err
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = time.Now().UTC()
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var result sql.Result
	var messageTable string
	var runTable string
	var parentTable string
	switch {
	case run.WorkflowID != "":
		workflowJSON, marshalErr := toJSON(run.Workflow)
		if marshalErr != nil {
			return marshalErr
		}
		result, err = tx.ExecContext(ctx, `UPDATE workflow_runs SET input=?,content=?,tool_calls=?,status=?,error=?,workflow_payload=?,updated_at=?,revision=revision+1 WHERE namespace=? AND run_id=? AND session_id=? AND workflow_id=? AND user_id=? AND status=?`, inputJSON, run.Content, toolCallsJSON, string(run.Status), run.Error, workflowJSON, run.UpdatedAt.Format(time.RFC3339Nano), namespace, run.RunID, run.SessionID, run.WorkflowID, run.UserID, string(expectedStatus))
		messageTable = "workflow_run_messages"
		runTable = "workflow_runs"
		parentTable = "workflow_sessions"
	case run.TeamID != "":
		result, err = tx.ExecContext(ctx, `UPDATE team_runs SET input=?,content=?,tool_calls=?,status=?,error=?,parent_run_id=?,node_id=?,updated_at=?,revision=revision+1 WHERE namespace=? AND run_id=? AND session_id=? AND team_id=? AND user_id=? AND status=?`, inputJSON, run.Content, toolCallsJSON, string(run.Status), run.Error, run.ParentRunID, run.NodeID, run.UpdatedAt.Format(time.RFC3339Nano), namespace, run.RunID, run.SessionID, run.TeamID, run.UserID, string(expectedStatus))
		messageTable = "team_run_messages"
		runTable = "team_runs"
		parentTable = "team_sessions"
	case run.AgentID != "":
		result, err = tx.ExecContext(ctx, `UPDATE runs SET input=?,content=?,tool_calls=?,status=?,error=?,parent_run_id=?,node_id=?,updated_at=?,revision=revision+1 WHERE namespace=? AND run_id=? AND session_id=? AND agent_id=? AND user_id=? AND status=?`, inputJSON, run.Content, toolCallsJSON, string(run.Status), run.Error, run.ParentRunID, run.NodeID, run.UpdatedAt.Format(time.RFC3339Nano), namespace, run.RunID, run.SessionID, run.AgentID, run.UserID, string(expectedStatus))
		messageTable = "run_messages"
		runTable = "runs"
		parentTable = "sessions"
	default:
		return fmt.Errorf("run has no component id")
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		var marker int
		getErr := tx.QueryRowContext(ctx, "SELECT 1 FROM "+runTable+" WHERE namespace=? AND run_id=? AND user_id=?", namespace, run.RunID, run.UserID).Scan(&marker)
		if errors.Is(getErr, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if getErr != nil {
			return getErr
		}
		return db.ErrConflict
	}
	updatedAt := run.UpdatedAt.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE "+parentTable+" SET updated_at=CASE WHEN updated_at < ? THEN ? ELSE updated_at END WHERE namespace=? AND session_id=?", updatedAt, updatedAt, namespace, run.SessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+messageTable+" WHERE namespace=? AND run_id = ?", namespace, run.RunID); err != nil {
		return err
	}
	switch messageTable {
	case "workflow_run_messages":
		err = d.insertWorkflowRunMessages(execution.ContextWithNamespace(ctx, namespace), tx, run.RunID, run.Messages)
	case "team_run_messages":
		err = d.insertTeamRunMessages(execution.ContextWithNamespace(ctx, namespace), tx, run.RunID, run.Messages)
	default:
		err = d.insertRunMessages(execution.ContextWithNamespace(ctx, namespace), tx, run.RunID, run.Messages)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) getAgentRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	namespace, err := db.NamespaceFor(ctx, query.Namespace)
	if err != nil {
		return nil, err
	}
	row := d.db.QueryRowContext(ctx,
		`SELECT run_id, session_id, agent_id, user_id, input, content, tool_calls, status, error, parent_run_id, node_id, created_at, updated_at
		 FROM runs WHERE namespace=? AND run_id = ? AND (? = '' OR user_id = ?)`, namespace, query.RunID, query.UserID, query.UserID)
	run, err := d.scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run.Namespace = namespace
	run.Messages, err = d.loadRunMessagesInNamespace(ctx, namespace, run.RunID)
	return run, err
}

func (d *DB) getTeamRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	namespace, err := db.NamespaceFor(ctx, query.Namespace)
	if err != nil {
		return nil, err
	}
	row := d.db.QueryRowContext(ctx,
		`SELECT run_id, session_id, team_id, user_id, input, content, tool_calls, status, error, parent_run_id, node_id, created_at, updated_at
		 FROM team_runs WHERE namespace=? AND run_id = ? AND (? = '' OR user_id = ?)`, namespace, query.RunID, query.UserID, query.UserID)
	run, err := d.scanTeamRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run.Namespace = namespace
	run.Messages, err = d.loadTeamRunMessagesInNamespace(ctx, namespace, run.RunID)
	return run, err
}

func (d *DB) getWorkflowRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	namespace, err := db.NamespaceFor(ctx, query.Namespace)
	if err != nil {
		return nil, err
	}
	row := d.db.QueryRowContext(ctx,
		`SELECT run_id, session_id, workflow_id, user_id, input, content, tool_calls, status, error, workflow_payload, created_at, updated_at
		 FROM workflow_runs WHERE namespace=? AND run_id = ? AND (? = '' OR user_id = ?)`, namespace, query.RunID, query.UserID, query.UserID)
	run, err := d.scanWorkflowRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run.Namespace = namespace
	run.Messages, err = d.loadWorkflowRunMessagesInNamespace(ctx, namespace, run.RunID)
	return run, err
}

// CreateTeamSession inserts a new team session including all of its runs and messages.
func (d *DB) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) insertRuns(ctx context.Context, tx *sql.Tx, sessionID string, runs []*session.RunOutput) error {
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
			`INSERT INTO runs (run_id, session_id, agent_id, user_id, input, content, tool_calls, status, error, parent_run_id, node_id, created_at, updated_at, namespace)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			run.RunID, run.SessionID, run.AgentID, run.UserID, inputJSON, run.Content,
			toolCallsJSON, string(run.Status), run.Error, run.ParentRunID, run.NodeID,
			run.CreatedAt.Format(time.RFC3339Nano), run.UpdatedAt.Format(time.RFC3339Nano), namespace,
		); err != nil {
			return fmt.Errorf("insert run: %w", err)
		}

		if err := d.insertRunMessages(execution.ContextWithNamespace(ctx, namespace), tx, run.RunID, run.Messages); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) insertRunMessages(ctx context.Context, tx *sql.Tx, runID string, messages []models.Message) error {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return err
	}
	for i, msg := range messages {
		msgJSON, err := toJSON(msg)
		if err != nil {
			return err
		}
		// Preserve message order by using a small offset from a base time.
		createdAt := time.Now().UTC().Add(time.Duration(i) * time.Microsecond)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO run_messages (run_id, message, created_at, namespace) VALUES (?, ?, ?, ?)`,
			runID, msgJSON, createdAt.Format(time.RFC3339Nano), namespace,
		); err != nil {
			return fmt.Errorf("insert run message: %w", err)
		}
	}
	return nil
}

func (d *DB) loadRuns(ctx context.Context, sessionID string) ([]*session.RunOutput, error) {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return nil, err
	}
	return d.loadRunsInNamespace(ctx, namespace, sessionID)
}

func (d *DB) loadRunsInNamespace(ctx context.Context, namespace, sessionID string) ([]*session.RunOutput, error) {
	return d.loadRecentRunsInNamespace(ctx, namespace, sessionID, -1)
}

// loadRecentRunsInNamespace loads the newest limit runs oldest first; a
// negative limit loads every run (SQLite treats LIMIT -1 as unbounded).
func (d *DB) loadRecentRunsInNamespace(ctx context.Context, namespace, sessionID string, limit int) ([]*session.RunOutput, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT * FROM (SELECT run_id, session_id, agent_id, user_id, input, content, tool_calls, status, error, parent_run_id, node_id, created_at, updated_at
		 FROM runs WHERE namespace = ? AND session_id = ? ORDER BY created_at DESC LIMIT ?) ORDER BY created_at ASC`, namespace, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []*session.RunOutput
	for rows.Next() {
		run, err := d.scanRun(rows)
		if err != nil {
			return nil, err
		}
		run.Namespace = namespace
		msgs, err := d.loadRunMessagesInNamespace(ctx, namespace, run.RunID)
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

func (d *DB) loadRunMessages(ctx context.Context, runID string) ([]models.Message, error) {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return nil, err
	}
	return d.loadRunMessagesInNamespace(ctx, namespace, runID)
}

func (d *DB) loadRunMessagesInNamespace(ctx context.Context, namespace, runID string) ([]models.Message, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT message FROM run_messages WHERE namespace = ? AND run_id = ? ORDER BY message_id ASC`, namespace, runID)
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

func (d *DB) scanSession(scanner interface {
	Scan(dest ...any) error
}) (*session.AgentSession, error) {
	var s session.AgentSession
	var metadataRaw, sessionStateRaw, summaryRaw sql.NullString
	var createdAtStr, updatedAtStr string

	if err := scanner.Scan(&s.SessionID, &s.AgentID, &s.UserID, &metadataRaw, &sessionStateRaw, &summaryRaw, &s.Revision, &createdAtStr, &updatedAtStr); err != nil {
		return nil, err
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

func (d *DB) scanRun(scanner interface {
	Scan(dest ...any) error
}) (*session.RunOutput, error) {
	var run session.RunOutput
	var inputRaw, toolCallsRaw sql.NullString
	var statusStr, createdAtStr, updatedAtStr string

	if err := scanner.Scan(&run.RunID, &run.SessionID, &run.AgentID, &run.UserID,
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

func toJSON(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func fromJSON(s string, v any) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), v)
}

type runEnvelope struct {
	Version             int                               `json:"version"`
	Principal           execution.Principal               `json:"principal,omitempty"`
	ToolCalls           []models.ToolCall                 `json:"tool_calls,omitempty"`
	ToolExecutions      []session.ToolExecution           `json:"tool_executions,omitempty"`
	Metadata            map[string]string                 `json:"metadata,omitempty"`
	Metrics             session.RunMetrics                `json:"metrics,omitempty"`
	Parsed              any                               `json:"parsed,omitempty"`
	ActiveRequirementID string                            `json:"active_requirement_id,omitempty"`
	Requirements        []requirement.Requirement         `json:"requirements,omitempty"`
	PendingToolCalls    []models.ToolCall                 `json:"pending_tool_calls,omitempty"`
	PendingPlans        map[string]session.PendingPlanRef `json:"pending_plans,omitempty"`
	NextToolIndex       int                               `json:"next_tool_index,omitempty"`
	ModelRound          int                               `json:"model_round,omitempty"`
}

func encodeRunEnvelope(run *session.RunOutput) (string, error) {
	if run == nil {
		return "", nil
	}
	return toJSON(runEnvelope{
		Version: 1, Principal: run.Principal, ToolCalls: run.ToolCalls, ToolExecutions: run.ToolExecutions,
		Metadata: run.Metadata, Metrics: run.Metrics, Parsed: run.Parsed,
		ActiveRequirementID: run.ActiveRequirementID, Requirements: run.Requirements,
		PendingToolCalls: run.PendingToolCalls, PendingPlans: run.PendingPlans, NextToolIndex: run.NextToolIndex,
		ModelRound: run.ModelRound,
	})
}

func decodeRunEnvelope(raw string, run *session.RunOutput) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		return fromJSON(trimmed, &run.ToolCalls)
	}
	var envelope runEnvelope
	if err := fromJSON(trimmed, &envelope); err != nil {
		return err
	}
	run.ToolCalls = envelope.ToolCalls
	run.Principal = envelope.Principal
	run.ToolExecutions = envelope.ToolExecutions
	run.Metadata = envelope.Metadata
	run.Metrics = envelope.Metrics
	run.Parsed = envelope.Parsed
	run.ActiveRequirementID = envelope.ActiveRequirementID
	run.Requirements = envelope.Requirements
	run.PendingToolCalls = envelope.PendingToolCalls
	run.PendingPlans = envelope.PendingPlans
	run.NextToolIndex = envelope.NextToolIndex
	run.ModelRound = envelope.ModelRound
	return nil
}

func (d *DB) sessionUpdateMiss(ctx context.Context, tx *sql.Tx, table, sessionID string) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE session_id = ?`, sessionID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return err
	}
	return db.ErrConflict
}

func (d *DB) sessionUpdateMissScoped(ctx context.Context, tx *sql.Tx, table, sessionID, namespace string) error {
	var marker int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM "+table+" WHERE session_id=? AND namespace=?", sessionID, namespace).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return err
	}
	return db.ErrConflict
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
