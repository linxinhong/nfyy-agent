package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// GetRun, AppendRun, and UpdateRun preserve the existing non-managed path.
func (d *LeaseDispatcher) GetRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	return d.store.GetRun(ctx, query)
}

func (d *LeaseDispatcher) AppendRun(ctx context.Context, output *session.RunOutput) error {
	return d.store.AppendRun(ctx, output)
}

func (d *LeaseDispatcher) UpdateRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	return d.store.UpdateRun(ctx, output, expected)
}

// GetManagedRun returns a namespace-scoped Run and its independent content
// revision. A transaction keeps the payload, revision, and messages in one
// SQLite snapshot.
func (d *LeaseDispatcher) GetManagedRun(ctx context.Context, scope execution.NamespaceScope, runID string) (*run.ManagedRunSnapshot, error) {
	if !scope.HasNamespace() || runID == "" {
		return nil, run.ErrInvalidClaim
	}
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	type candidate struct {
		table, messageTable string
		scan                func(interface{ Scan(...any) error }) (*session.RunOutput, error)
		columns             string
	}
	candidates := []candidate{
		{"runs", "run_messages", d.store.scanRun, "run_id,session_id,agent_id,user_id,input,content,tool_calls,status,error,parent_run_id,node_id,created_at,updated_at"},
		{"team_runs", "team_run_messages", d.store.scanTeamRun, "run_id,session_id,team_id,user_id,input,content,tool_calls,status,error,parent_run_id,node_id,created_at,updated_at"},
		{"workflow_runs", "workflow_run_messages", d.store.scanWorkflowRun, "run_id,session_id,workflow_id,user_id,input,content,tool_calls,status,error,workflow_payload,created_at,updated_at"},
	}
	var snapshot *run.ManagedRunSnapshot
	for _, candidate := range candidates {
		var revision uint64
		err := tx.QueryRowContext(ctx, "SELECT revision FROM "+candidate.table+" WHERE namespace=? AND run_id=?", scope.Namespace, runID).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if snapshot != nil {
			return nil, db.ErrConflict
		}
		output, err := candidate.scan(tx.QueryRowContext(ctx, "SELECT "+candidate.columns+" FROM "+candidate.table+" WHERE namespace=? AND run_id=?", scope.Namespace, runID))
		if err != nil {
			return nil, err
		}
		output.Namespace = scope.Namespace
		output.Messages, err = loadManagedMessages(ctx, tx, candidate.messageTable, scope.Namespace, runID)
		if err != nil {
			return nil, err
		}
		snapshot = &run.ManagedRunSnapshot{Run: output, Revision: revision}
	}
	if snapshot == nil {
		return nil, db.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func loadManagedMessages(ctx context.Context, tx *sql.Tx, table, namespace, runID string) ([]models.Message, error) {
	rows, err := tx.QueryContext(ctx, "SELECT message FROM "+table+" WHERE namespace=? AND run_id=? ORDER BY message_id", namespace, runID)
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
		var message models.Message
		if err := fromJSON(raw, &message); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// CommitManagedRun first performs a conditional no-op write on the exact
// lease row. That obtains SQLite's database write lock before the authoritative
// expiry read, so another connection cannot take over between the check and
// the Run CAS. RFC3339Nano parsing preserves the existing sub-millisecond
// lease boundary.
func (d *LeaseDispatcher) CommitManagedRun(ctx context.Context, scope execution.NamespaceScope, claim *run.Claim, commit run.ManagedRunCommit) (uint64, error) {
	if !validLeaseClaim(scope, claim) || commit.Run == nil || commit.Run.RunID == "" ||
		commit.Run.RunID != claim.RunID || commit.Run.Namespace != scope.Namespace {
		return 0, run.ErrInvalidClaim
	}
	if commit.Run.SessionID == "" {
		return 0, fmt.Errorf("session_id is empty")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE dispatch_leases SET fencing_token=fencing_token
		WHERE namespace=? AND run_id=? AND worker_id=? AND fencing_token=?`,
		scope.Namespace, claim.RunID, claim.WorkerID, int64(claim.FencingToken))
	if err != nil {
		return 0, err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return 0, err
	} else if changed == 0 {
		return 0, run.ErrStaleFencingToken
	}
	var expiresRaw string
	if err := tx.QueryRowContext(ctx, `SELECT lease_expires_at FROM dispatch_leases
		WHERE namespace=? AND run_id=? AND worker_id=? AND fencing_token=?`,
		scope.Namespace, claim.RunID, claim.WorkerID, int64(claim.FencingToken)).Scan(&expiresRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, run.ErrStaleFencingToken
		}
		return 0, err
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresRaw)
	if err != nil {
		return 0, fmt.Errorf("parse authoritative lease expiry: %w", err)
	}
	if !d.clock().Before(expires) {
		return 0, run.ErrLeaseExpired
	}

	table, messageTable, parentTable, componentColumn, componentID, err := sqliteRunTables(commit.Run)
	if err != nil {
		return 0, err
	}
	var parentExists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+parentTable+" WHERE namespace=? AND session_id=? AND "+componentColumn+"=? AND user_id=?)",
		scope.Namespace, commit.Run.SessionID, componentID, commit.Run.UserID).Scan(&parentExists); err != nil {
		return 0, err
	}
	if !parentExists {
		return 0, db.ErrNotFound
	}
	if commit.Run.CreatedAt.IsZero() {
		commit.Run.CreatedAt = d.clock().UTC()
	}
	if commit.Run.UpdatedAt.IsZero() {
		commit.Run.UpdatedAt = commit.Run.CreatedAt
	}

	var revision uint64
	if commit.ExpectedRevision == 0 && commit.ExpectedStatus == "" {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE run_id=?)", commit.Run.RunID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			exists, err = managedRunExists(ctx, tx, scope.Namespace, commit.Run.RunID)
			if err != nil {
				return 0, err
			}
		}
		if exists {
			return 0, db.ErrConflict
		}
		if err := d.insertManagedRun(ctx, tx, table, messageTable, scope.Namespace, commit.Run); err != nil {
			return 0, err
		}
		revision = 1
	} else {
		if commit.ExpectedRevision == 0 {
			return 0, db.ErrConflict
		}
		if !commit.ExpectedStatus.CanTransitionTo(commit.Run.Status) {
			return 0, db.ErrInvalidTransition
		}
		revision, err = d.updateManagedRun(ctx, tx, table, messageTable, scope.Namespace, commit)
		if err != nil {
			return 0, err
		}
	}
	updatedAt := commit.Run.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE "+parentTable+" SET updated_at=CASE WHEN updated_at < ? THEN ? ELSE updated_at END WHERE namespace=? AND session_id=?",
		updatedAt, updatedAt, scope.Namespace, commit.Run.SessionID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revision, nil
}

func managedRunExists(ctx context.Context, tx *sql.Tx, namespace, runID string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM runs WHERE namespace=? AND run_id=?
		UNION ALL SELECT 1 FROM team_runs WHERE namespace=? AND run_id=?
		UNION ALL SELECT 1 FROM workflow_runs WHERE namespace=? AND run_id=?
	)`, namespace, runID, namespace, runID, namespace, runID).Scan(&exists)
	return exists, err
}

// managedSessionExists keeps SQLite's per-kind legacy tables aligned with the
// PostgreSQL store, whose session ID is globally unique. Rejecting a duplicate
// across kinds prevents ambiguous externally-addressable IDs.
func managedSessionExists(ctx context.Context, tx *sql.Tx, sessionID string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sessions WHERE session_id=?
		UNION ALL SELECT 1 FROM team_sessions WHERE session_id=?
		UNION ALL SELECT 1 FROM workflow_sessions WHERE session_id=?
	)`, sessionID, sessionID, sessionID).Scan(&exists)
	return exists, err
}

func sqliteRunTables(output *session.RunOutput) (table, messages, parent, componentColumn, componentID string, err error) {
	switch {
	case output.WorkflowID != "":
		return "workflow_runs", "workflow_run_messages", "workflow_sessions", "workflow_id", output.WorkflowID, nil
	case output.TeamID != "":
		return "team_runs", "team_run_messages", "team_sessions", "team_id", output.TeamID, nil
	case output.AgentID != "":
		return "runs", "run_messages", "sessions", "agent_id", output.AgentID, nil
	default:
		return "", "", "", "", "", fmt.Errorf("run has no component id")
	}
}

func (d *LeaseDispatcher) insertManagedRun(ctx context.Context, tx *sql.Tx, table, messageTable, namespace string, output *session.RunOutput) error {
	var err error
	switch table {
	case "workflow_runs":
		err = d.store.insertWorkflowRuns(ctx, tx, output.SessionID, []*session.RunOutput{output})
	case "team_runs":
		err = d.store.insertTeamRuns(ctx, tx, output.SessionID, []*session.RunOutput{output})
	default:
		err = d.store.insertRuns(ctx, tx, output.SessionID, []*session.RunOutput{output})
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET namespace=? WHERE run_id=?", namespace, output.RunID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+messageTable+" SET namespace=? WHERE run_id=?", namespace, output.RunID)
	return err
}

func (d *LeaseDispatcher) updateManagedRun(ctx context.Context, tx *sql.Tx, table, messageTable, namespace string, commit run.ManagedRunCommit) (uint64, error) {
	output := commit.Run
	inputJSON, err := toJSON(output.Input)
	if err != nil {
		return 0, err
	}
	envelopeJSON, err := encodeRunEnvelope(output)
	if err != nil {
		return 0, err
	}
	updatedAt := output.UpdatedAt.UTC().Format(time.RFC3339Nano)
	var result sql.Result
	switch table {
	case "workflow_runs":
		workflowJSON, err := toJSON(output.Workflow)
		if err != nil {
			return 0, err
		}
		result, err = tx.ExecContext(ctx, `UPDATE workflow_runs SET input=?,content=?,tool_calls=?,status=?,error=?,workflow_payload=?,updated_at=?,revision=revision+1
			WHERE namespace=? AND run_id=? AND session_id=? AND workflow_id=? AND user_id=? AND revision=? AND status=?`,
			inputJSON, output.Content, envelopeJSON, string(output.Status), output.Error, workflowJSON, updatedAt,
			namespace, output.RunID, output.SessionID, output.WorkflowID, output.UserID, commit.ExpectedRevision, string(commit.ExpectedStatus))
	case "team_runs":
		result, err = tx.ExecContext(ctx, `UPDATE team_runs SET input=?,content=?,tool_calls=?,status=?,error=?,parent_run_id=?,node_id=?,updated_at=?,revision=revision+1
			WHERE namespace=? AND run_id=? AND session_id=? AND team_id=? AND user_id=? AND revision=? AND status=?`,
			inputJSON, output.Content, envelopeJSON, string(output.Status), output.Error, output.ParentRunID, output.NodeID, updatedAt,
			namespace, output.RunID, output.SessionID, output.TeamID, output.UserID, commit.ExpectedRevision, string(commit.ExpectedStatus))
	default:
		result, err = tx.ExecContext(ctx, `UPDATE runs SET input=?,content=?,tool_calls=?,status=?,error=?,parent_run_id=?,node_id=?,updated_at=?,revision=revision+1
			WHERE namespace=? AND run_id=? AND session_id=? AND agent_id=? AND user_id=? AND revision=? AND status=?`,
			inputJSON, output.Content, envelopeJSON, string(output.Status), output.Error, output.ParentRunID, output.NodeID, updatedAt,
			namespace, output.RunID, output.SessionID, output.AgentID, output.UserID, commit.ExpectedRevision, string(commit.ExpectedStatus))
	}
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed == 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE namespace=? AND run_id=?)", namespace, output.RunID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, db.ErrNotFound
		}
		return 0, db.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+messageTable+" WHERE namespace=? AND run_id=?", namespace, output.RunID); err != nil {
		return 0, err
	}
	switch messageTable {
	case "workflow_run_messages":
		err = d.store.insertWorkflowRunMessages(ctx, tx, output.RunID, output.Messages)
	case "team_run_messages":
		err = d.store.insertTeamRunMessages(ctx, tx, output.RunID, output.Messages)
	default:
		err = d.store.insertRunMessages(ctx, tx, output.RunID, output.Messages)
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE "+messageTable+" SET namespace=? WHERE run_id=?", namespace, output.RunID); err != nil {
		return 0, err
	}
	return commit.ExpectedRevision + 1, nil
}
