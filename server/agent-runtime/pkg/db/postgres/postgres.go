// Package postgres provides a PostgreSQL-backed implementation of the db.DB interface.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	dbmigrate "github.com/linxinhong/xingding-agent-runtime/pkg/db/migrate"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
	"time"
)

//go:embed migrations/*.sql
var migrations embed.FS

const schemaVersion = 14

type Options struct {
	AutoMigrate bool
	// SkipSchemaCheck is for isolated setup tools that must validate the live
	// server before choosing whether to run migrations. Normal callers leave it
	// false and receive the existing schema guard.
	SkipSchemaCheck bool
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type DB struct{ db *sql.DB }

var _ db.Store = (*DB)(nil)

func New(ctx context.Context, dsn string, options Options) (*DB, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	d := &DB{db: conn}
	if options.MaxOpenConns > 0 {
		conn.SetMaxOpenConns(options.MaxOpenConns)
	}
	if options.MaxIdleConns > 0 {
		conn.SetMaxIdleConns(options.MaxIdleConns)
	}
	if options.ConnMaxLifetime > 0 {
		conn.SetConnMaxLifetime(options.ConnMaxLifetime)
	}
	if err := d.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if options.AutoMigrate {
		if err := d.Migrate(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
	} else if !options.SkipSchemaCheck {
		if err := d.checkSchema(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return d, nil
}

func (d *DB) Migrate(ctx context.Context) error {
	return dbmigrate.Apply(ctx, d.db, migrations, "migrations", "postgres")
}

func (d *DB) checkSchema(ctx context.Context) error {
	var version int
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM agent_runtime_schema_migrations`).Scan(&version)
	if err != nil || version != schemaVersion {
		return fmt.Errorf("postgres schema version is not %d; enable AutoMigrate", schemaVersion)
	}
	return nil
}

func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }
func (d *DB) Close() error                   { return d.db.Close() }

// PoolStats exposes only aggregate database/sql counters for bounded
// diagnostic tooling. It does not expose a connection, DSN, or query API.
func (d *DB) PoolStats() sql.DBStats { return d.db.Stats() }

// RuntimeSettings reads settings from this DB's own connection pool. It is
// intentionally narrow so isolated verification tools can attest the actual
// adapter connection without receiving the underlying *sql.DB.
func (d *DB) RuntimeSettings(ctx context.Context, keys ...string) (map[string]string, error) {
	settings := make(map[string]string, len(keys))
	for _, key := range keys {
		var value string
		if err := d.db.QueryRowContext(ctx, `SELECT current_setting($1)`, key).Scan(&value); err != nil {
			return nil, fmt.Errorf("read PostgreSQL setting %q: %w", key, err)
		}
		settings[key] = value
	}
	return settings, nil
}

type sessionRecord struct {
	ID, Kind, ComponentID, UserID string
	Namespace                     string
	Payload                       []byte
	Revision                      uint64
	CreatedAt, UpdatedAt          time.Time
}

func prepareSession(id *string, revision *uint64, created, updated *time.Time) {
	if *id == "" {
		*id = uuid.NewString()
	}
	if *revision == 0 {
		*revision = 1
	}
	if created.IsZero() {
		*created = time.Now().UTC()
	}
	if updated.IsZero() {
		*updated = *created
	}
}

func withoutRuns(v any) ([]byte, error) { return json.Marshal(v) }

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func createSession(ctx context.Context, exec sqlExecutor, r sessionRecord) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO agent_runtime_sessions
		(session_id, kind, component_id, user_id, payload, revision, created_at, updated_at, namespace)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9)`, r.ID, r.Kind, r.ComponentID, r.UserID, string(r.Payload), r.Revision, r.CreatedAt, r.UpdatedAt, r.Namespace)
	return err
}

func (d *DB) readRecord(ctx context.Context, kind, id string) (*sessionRecord, error) {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return nil, err
	}
	r := &sessionRecord{}
	err = d.db.QueryRowContext(ctx, `SELECT session_id,kind,component_id,user_id,payload,revision,created_at,updated_at
		FROM agent_runtime_sessions WHERE namespace=$1 AND session_id=$2 AND kind=$3`, namespace, id, kind).Scan(&r.ID, &r.Kind, &r.ComponentID, &r.UserID, &r.Payload, &r.Revision, &r.CreatedAt, &r.UpdatedAt)
	r.Namespace = namespace
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (d *DB) updateRecord(ctx context.Context, r sessionRecord) (uint64, error) {
	namespace, err := db.NamespaceFor(ctx, r.Namespace)
	if err != nil {
		return 0, err
	}
	r.Namespace = namespace
	var revision uint64
	err = d.db.QueryRowContext(ctx, `UPDATE agent_runtime_sessions SET component_id=$1,user_id=$2,payload=$3::jsonb,
		revision=revision+1,updated_at=$4 WHERE namespace=$5 AND session_id=$6 AND kind=$7 AND revision=$8 RETURNING revision`,
		r.ComponentID, r.UserID, string(r.Payload), r.UpdatedAt, namespace, r.ID, r.Kind, r.Revision).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if e := d.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runtime_sessions WHERE namespace=$1 AND session_id=$2 AND kind=$3)`, namespace, r.ID, r.Kind).Scan(&exists); e != nil {
			return 0, e
		}
		if !exists {
			return 0, db.ErrNotFound
		}
		return 0, db.ErrConflict
	}
	return revision, err
}

func (d *DB) deleteSession(ctx context.Context, kind, id string) error {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return err
	}
	res, err := d.db.ExecContext(ctx, `DELETE FROM agent_runtime_sessions WHERE namespace=$1 AND session_id=$2 AND kind=$3`, namespace, id, kind)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

func (d *DB) listRecords(ctx context.Context, kind string, q db.SessionQuery) ([]sessionRecord, error) {
	namespace, err := db.NamespaceFor(ctx, q.Namespace)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 2147483647
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := d.db.QueryContext(ctx, `SELECT session_id,kind,component_id,user_id,payload,revision,created_at,updated_at
		FROM agent_runtime_sessions WHERE namespace=$1 AND kind=$2 AND ($3='' OR component_id=$3) AND ($4='' OR user_id=$4)
		ORDER BY updated_at DESC LIMIT $5 OFFSET $6`, namespace, kind, q.ComponentID, q.UserID, limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRecord
	for rows.Next() {
		var r sessionRecord
		if err := rows.Scan(&r.ID, &r.Kind, &r.ComponentID, &r.UserID, &r.Payload, &r.Revision, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Namespace = namespace
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) loadRuns(ctx context.Context, sessionID string) ([]*session.RunOutput, error) {
	namespace, err := db.NamespaceFor(ctx, "")
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT payload FROM agent_runtime_runs WHERE namespace=$1 AND session_id=$2 ORDER BY created_at`, namespace, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*session.RunOutput
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var run session.RunOutput
		if err := json.Unmarshal(raw, &run); err != nil {
			return nil, err
		}
		run.Namespace = namespace
		out = append(out, &run)
	}
	return out, rows.Err()
}

func (d *DB) CreateSession(ctx context.Context, s *session.AgentSession) error {
	namespace, e := db.NamespaceFor(ctx, s.Namespace)
	if e != nil {
		return e
	}
	s.Namespace = namespace
	prepareSession(&s.SessionID, &s.Revision, &s.CreatedAt, &s.UpdatedAt)
	runs := s.Runs
	s.Runs = nil
	raw, e := withoutRuns(s)
	s.Runs = runs
	if e != nil {
		return e
	}
	tx, e := d.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = createSession(ctx, tx, sessionRecord{ID: s.SessionID, Kind: "agent", ComponentID: s.AgentID, UserID: s.UserID, Namespace: namespace, Payload: raw, Revision: s.Revision, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}); e != nil {
		return e
	}
	for _, r := range runs {
		if r.Namespace == "" {
			r.Namespace = namespace
		}
		if r.Namespace != namespace {
			return db.ErrNotFound
		}
		if r.SessionID == "" {
			r.SessionID = s.SessionID
		}
		if r.AgentID == "" {
			r.AgentID = s.AgentID
		}
		if r.UserID == "" {
			r.UserID = s.UserID
		}
		if e = appendRun(ctx, tx, r); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (d *DB) ReadSession(ctx context.Context, id string) (*session.AgentSession, error) {
	r, e := d.readRecord(ctx, "agent", id)
	if e != nil || r == nil {
		return nil, e
	}
	var s session.AgentSession
	if e = json.Unmarshal(r.Payload, &s); e != nil {
		return nil, e
	}
	s.Namespace = r.Namespace
	s.Revision = r.Revision
	s.CreatedAt, s.UpdatedAt = r.CreatedAt, r.UpdatedAt
	s.Runs, e = d.loadRuns(ctx, id)
	return &s, e
}
func (d *DB) UpdateSession(ctx context.Context, s *session.AgentSession) error {
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	runs := s.Runs
	s.Runs = nil
	raw, e := json.Marshal(s)
	s.Runs = runs
	if e != nil {
		return e
	}
	rev, e := d.updateRecord(ctx, sessionRecord{ID: s.SessionID, Kind: "agent", ComponentID: s.AgentID, UserID: s.UserID, Namespace: s.Namespace, Payload: raw, Revision: s.Revision, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt})
	if e == nil {
		s.Revision = rev
	}
	return e
}
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	return d.deleteSession(ctx, "agent", id)
}
func (d *DB) ListSessions(ctx context.Context, q db.SessionQuery) ([]*session.AgentSession, error) {
	rs, e := d.listRecords(ctx, "agent", q)
	if e != nil {
		return nil, e
	}
	out := make([]*session.AgentSession, 0, len(rs))
	for _, r := range rs {
		var s session.AgentSession
		if e = json.Unmarshal(r.Payload, &s); e != nil {
			return nil, e
		}
		s.Namespace = r.Namespace
		s.Revision = r.Revision
		s.CreatedAt, s.UpdatedAt = r.CreatedAt, r.UpdatedAt
		s.Runs, e = d.loadRuns(ctx, r.ID)
		if e != nil {
			return nil, e
		}
		out = append(out, &s)
	}
	return out, nil
}

func applyFields(metadata *map[string]any, state *map[string]any, summary *string, update db.SessionFieldsUpdate) {
	if update.Metadata != nil {
		*metadata = update.Metadata
	}
	if update.SessionState != nil {
		*state = update.SessionState
	}
	if update.Summary != nil {
		*summary = *update.Summary
	}
}
func (d *DB) UpdateSessionFields(ctx context.Context, u db.SessionFieldsUpdate) (uint64, error) {
	s, e := d.ReadSession(ctx, u.SessionID)
	if e != nil {
		return 0, e
	}
	if s == nil {
		return 0, db.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	e = d.UpdateSession(ctx, s)
	return s.Revision, e
}
func (d *DB) UpdateTeamSessionFields(ctx context.Context, u db.SessionFieldsUpdate) (uint64, error) {
	s, e := d.ReadTeamSession(ctx, u.SessionID)
	if e != nil {
		return 0, e
	}
	if s == nil {
		return 0, db.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	e = d.UpdateTeamSession(ctx, s)
	return s.Revision, e
}
func (d *DB) UpdateWorkflowSessionFields(ctx context.Context, u db.SessionFieldsUpdate) (uint64, error) {
	s, e := d.ReadWorkflowSession(ctx, u.SessionID)
	if e != nil {
		return 0, e
	}
	if s == nil {
		return 0, db.ErrNotFound
	}
	s.Revision = u.ExpectedRevision
	applyFields(&s.Metadata, &s.SessionState, &s.Summary, u)
	e = d.UpdateWorkflowSession(ctx, s)
	return s.Revision, e
}

func runParent(r *session.RunOutput) (string, string, error) {
	if r.WorkflowID != "" && r.TeamID != "" {
		return "", "", fmt.Errorf("run has ambiguous workflow and team parents")
	}
	if r.TeamID != "" && r.AgentID != "" {
		return "", "", fmt.Errorf("run has ambiguous team and agent parents")
	}
	if r.WorkflowID != "" {
		// Workflow step runs may also carry the executing AgentID; WorkflowID
		// remains their persistence parent.
		return "workflow", r.WorkflowID, nil
	}
	if r.TeamID != "" {
		return "team", r.TeamID, nil
	}
	if r.AgentID != "" {
		return "agent", r.AgentID, nil
	}
	return "", "", fmt.Errorf("run has no parent component id")
}
func (d *DB) AppendRun(ctx context.Context, r *session.RunOutput) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := appendRun(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateRun atomically replaces a run when it is still in expectedStatus.
func (d *DB) UpdateRun(ctx context.Context, r *session.RunOutput, expectedStatus session.RunStatus) error {
	if r == nil || r.RunID == "" {
		return fmt.Errorf("run is nil or run_id is empty")
	}
	if !expectedStatus.CanTransitionTo(r.Status) {
		return db.ErrInvalidTransition
	}
	namespace, err := db.NamespaceFor(ctx, r.Namespace)
	if err != nil {
		return err
	}
	r.Namespace = namespace
	kind, _, err := runParent(r)
	if err != nil {
		return err
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now().UTC()
	}
	truncateRunTimes(r)
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE agent_runtime_runs SET payload=$1::jsonb,updated_at=$2,revision=revision+1 WHERE namespace=$3 AND run_id=$4 AND session_id=$5 AND kind=$6 AND user_id=$7 AND payload->>'status'=$8`, string(raw), r.UpdatedAt, namespace, r.RunID, r.SessionID, kind, r.UserID, string(expectedStatus))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_sessions SET updated_at=GREATEST(updated_at,$1) WHERE namespace=$2 AND session_id=$3 AND kind=$4 AND user_id=$5`, r.UpdatedAt, namespace, r.SessionID, kind, r.UserID); err != nil {
			return err
		}
		return tx.Commit()
	}
	var marker int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM agent_runtime_runs WHERE namespace=$1 AND run_id=$2 AND user_id=$3`, namespace, r.RunID, r.UserID).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return err
	}
	return db.ErrConflict
}

// truncateRunTimes rounds run timestamps to PostgreSQL's microsecond
// precision so the JSON payload, the timestamp columns, and the caller's
// copy agree; otherwise a nanosecond payload time can sort after the
// microsecond column derived from it.
func truncateRunTimes(r *session.RunOutput) {
	r.CreatedAt = r.CreatedAt.Truncate(time.Microsecond)
	r.UpdatedAt = r.UpdatedAt.Truncate(time.Microsecond)
}

func appendRun(ctx context.Context, exec sqlExecutor, r *session.RunOutput) error {
	namespace, err := db.NamespaceFor(ctx, r.Namespace)
	if err != nil {
		return err
	}
	r.Namespace = namespace
	if r.RunID == "" {
		r.RunID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	truncateRunTimes(r)
	kind, componentID, err := runParent(r)
	if err != nil {
		return err
	}
	var owner, parentComponent string
	err = exec.QueryRowContext(ctx, `SELECT user_id,component_id FROM agent_runtime_sessions WHERE namespace=$1 AND session_id=$2 AND kind=$3 FOR UPDATE`, namespace, r.SessionID, kind).Scan(&owner, &parentComponent)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner != r.UserID {
		return fmt.Errorf("run user does not own parent session")
	}
	if parentComponent != componentID {
		return fmt.Errorf("run component does not own parent session")
	}
	raw, e := json.Marshal(r)
	if e != nil {
		return e
	}
	parentUpdatedAt := time.Now().UTC()
	if _, e = exec.ExecContext(ctx, `UPDATE agent_runtime_sessions SET updated_at=GREATEST(updated_at,$1) WHERE namespace=$2 AND session_id=$3`, parentUpdatedAt, namespace, r.SessionID); e != nil {
		return e
	}
	_, e = exec.ExecContext(ctx, `INSERT INTO agent_runtime_runs(run_id,session_id,kind,user_id,payload,created_at,updated_at,namespace) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,$8)`, r.RunID, r.SessionID, kind, r.UserID, string(raw), r.CreatedAt, r.UpdatedAt, namespace)
	return e
}
func (d *DB) GetRun(ctx context.Context, q db.RunQuery) (*session.RunOutput, error) {
	namespace, err := db.NamespaceFor(ctx, q.Namespace)
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = d.db.QueryRowContext(ctx, `SELECT payload FROM agent_runtime_runs WHERE namespace=$1 AND run_id=$2 AND ($3='' OR user_id=$3)`, namespace, q.RunID, q.UserID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r session.RunOutput
	err = json.Unmarshal(raw, &r)
	r.Namespace = namespace
	if err == nil && r.WorkflowID != "" && r.Workflow == nil {
		r.Workflow = &workflowschema.RunData{Legacy: true}
	}
	return &r, err
}

func markLegacyWorkflowRuns(runs []*session.RunOutput) {
	for _, run := range runs {
		if run != nil && run.WorkflowID != "" && run.Workflow == nil {
			run.Workflow = &workflowschema.RunData{Legacy: true}
		}
	}
}
