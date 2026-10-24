package contribution

// Persistence for contributions, versions and idempotency keys (migration
// 000024). Every accepted upload writes its version, idempotency record and
// audit event in one transaction.
//
// Pitfalls:
//   - Only the author reads or withdraws a contribution here; reviewers reach
//     contributions through the curation layer, not this store.
//   - A concurrent first upload of the same local page loses the unique race
//     and gets ErrConflict (retriable); never merge two first versions.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/audit"
)

var (
	ErrNotFound            = errors.New("contribution not found")
	ErrConflict            = errors.New("concurrent upload of the same local page")
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
)

// RejectedError carries structured reasons for a refused upload.
type RejectedError struct{ Reasons []Reason }

func (e *RejectedError) Error() string {
	return fmt.Sprintf("contribution rejected (%d reasons)", len(e.Reasons))
}

// TxAudit writes audit events inside a caller-owned transaction.
type TxAudit interface {
	WriteBatchTx(ctx context.Context, tx pgx.Tx, events []audit.Event) error
}

// Author is the uploading member inside the selected Workspace.
type Author struct {
	UserID  string
	AnyTeam bool // may upload into any team (knowledge.manage)
	GroupID string
}

// Contribution is the author's view of one uploaded page.
type Contribution struct {
	ID               string    `json:"id"`
	LocalID          string    `json:"local_id"`
	Version          int       `json:"version"`
	ContentHash      string    `json:"content_hash"`
	Status           string    `json:"status"`
	Scope            string    `json:"scope"`
	GroupID          string    `json:"group_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	IdempotentReplay bool      `json:"idempotent_replay"`
}

// Store is the pgx-backed contribution store.
type Store struct {
	pool  *pgxpool.Pool
	audit TxAudit
}

func New(pool *pgxpool.Pool, txAudit TxAudit) *Store { return &Store{pool: pool, audit: txAudit} }

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ctb_" + hex.EncodeToString(b[:]), nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const contributionSelect = `SELECT c.id, c.local_id, v.version, v.content_hash, c.status, v.scope, COALESCE(v.group_id, ''), c.created_at, c.updated_at
	FROM contributions c JOIN contribution_versions v ON v.contribution_id = c.id`

func scan(row pgx.Row) (Contribution, error) {
	var c Contribution
	err := row.Scan(&c.ID, &c.LocalID, &c.Version, &c.ContentHash, &c.Status, &c.Scope, &c.GroupID, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func loadVersion(ctx context.Context, q pgx.Tx, id string, version int) (Contribution, error) {
	return scan(q.QueryRow(ctx, contributionSelect+` WHERE c.id = $1 AND v.version = $2`, id, version))
}

// Upload accepts one normalized, validated snapshot. Scope rules and blob
// presence are enforced here because they need server state.
func (s *Store) Upload(ctx context.Context, workspaceID string, author Author, idempotencyKey string, u Upload, ev audit.Event) (Contribution, error) {
	var reasons []Reason
	if u.RequestedScope == ScopeTeam && !author.AnyTeam && author.GroupID != u.GroupID {
		reasons = append(reasons, Reason{Field: "group_id", Code: ReasonScopeNotAllowed, Message: "you can only share to your own team"})
	}
	for i, a := range u.Attachments {
		// The blob must be uploaded to this Workspace with the same size and type, and not blocked.
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blobs WHERE workspace_id = $1 AND sha256 = $2
			AND size = $3 AND media_type = $4 AND scan_status <> 'blocked')`, workspaceID, a.SHA256, a.Size, a.MediaType).Scan(&ok); err != nil {
			return Contribution{}, fmt.Errorf("check attachment: %w", err)
		}
		if !ok {
			reasons = append(reasons, Reason{Field: indexed("attachments", i, "sha256"), Code: ReasonAttachmentMissing,
				Message: "upload the attachment to /enterprise/v1/blobs/{sha256} first; size and media_type must match"})
		}
	}
	if len(reasons) > 0 {
		return Contribution{}, &RejectedError{Reasons: reasons}
	}
	contentHash, requestHash := ContentHash(u), RequestHash(u)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Contribution{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var priorHash, priorID string
	var priorVersion int
	err = tx.QueryRow(ctx, `SELECT request_hash, contribution_id, version FROM contribution_idempotency
		WHERE author_id = $1 AND idempotency_key = $2`, author.UserID, idempotencyKey).Scan(&priorHash, &priorID, &priorVersion)
	switch {
	case err == nil:
		if priorHash != requestHash {
			return Contribution{}, ErrIdempotencyMismatch
		}
		c, err := loadVersion(ctx, tx, priorID, priorVersion)
		if err != nil {
			return Contribution{}, fmt.Errorf("load replay: %w", err)
		}
		c.IdempotentReplay = true
		return c, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Contribution{}, fmt.Errorf("idempotency lookup: %w", err)
	}

	var id string
	var current int
	err = tx.QueryRow(ctx, `SELECT id, current_version FROM contributions
		WHERE workspace_id = $1 AND author_id = $2 AND local_id = $3 FOR UPDATE`,
		workspaceID, author.UserID, u.LocalID).Scan(&id, &current)
	replay := false
	version := 1
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if id, err = newID(); err != nil {
			return Contribution{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO contributions (id, workspace_id, author_id, local_id, current_version, status, scope, group_id)
			VALUES ($1, $2, $3, $4, 1, 'received', $5, $6)`, id, workspaceID, author.UserID, u.LocalID, u.RequestedScope, nullable(u.GroupID))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Contribution{}, ErrConflict
		}
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return Contribution{}, &RejectedError{Reasons: []Reason{{Field: "group_id", Code: ReasonUnknownGroup, Message: "group is not part of this workspace"}}}
		}
		if err != nil {
			return Contribution{}, fmt.Errorf("insert contribution: %w", err)
		}
	case err != nil:
		return Contribution{}, fmt.Errorf("lock contribution: %w", err)
	default:
		err = tx.QueryRow(ctx, `SELECT version FROM contribution_versions WHERE contribution_id = $1 AND content_hash = $2`,
			id, contentHash).Scan(&version)
		switch {
		case err == nil:
			replay = true
		case errors.Is(err, pgx.ErrNoRows):
			version = current + 1
		default:
			return Contribution{}, fmt.Errorf("content lookup: %w", err)
		}
	}

	if !replay {
		links, _ := json.Marshal(u.Links)
		refs, _ := json.Marshal(u.SourceRefs)
		attachments, _ := json.Marshal(u.Attachments)
		client, _ := json.Marshal(u.Client)
		if _, err := tx.Exec(ctx, `INSERT INTO contribution_versions
			(contribution_id, version, content_hash, title, summary, markdown, details, links, source_refs, attachments, scope, group_id, upload_mode, client)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			id, version, contentHash, u.Title, u.Summary, u.Markdown, u.Details, links, refs, attachments,
			u.RequestedScope, nullable(u.GroupID), u.UploadMode, client); err != nil {
			return Contribution{}, fmt.Errorf("insert version: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE contributions SET current_version = $2, status = 'received', scope = $3, group_id = $4,
			updated_at = now(), withdrawn_at = NULL WHERE id = $1`, id, version, u.RequestedScope, nullable(u.GroupID))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return Contribution{}, &RejectedError{Reasons: []Reason{{Field: "group_id", Code: ReasonUnknownGroup, Message: "group is not part of this workspace"}}}
		}
		if err != nil {
			return Contribution{}, fmt.Errorf("update contribution: %w", err)
		}
	}
	// A replay of withdrawn content revives the contribution like a new upload.
	revived := false
	if replay {
		tag, err := tx.Exec(ctx, `UPDATE contributions SET status = 'received', withdrawn_at = NULL, updated_at = now()
			WHERE id = $1 AND status = 'withdrawn'`, id)
		if err != nil {
			return Contribution{}, fmt.Errorf("revive contribution: %w", err)
		}
		revived = tag.RowsAffected() == 1
	}
	if !replay || revived {
		if err := enqueue(ctx, tx, workspaceID, "contribution", id, version); err != nil {
			return Contribution{}, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO contribution_idempotency (author_id, idempotency_key, request_hash, contribution_id, version)
		VALUES ($1, $2, $3, $4, $5)`, author.UserID, idempotencyKey, requestHash, id, version); err != nil {
		return Contribution{}, fmt.Errorf("record idempotency: %w", err)
	}
	ev.Type = "contribution.received"
	ev.Resource = "contribution/" + id
	ev.Detail = map[string]any{"version": version, "scope": u.RequestedScope, "upload_mode": u.UploadMode,
		"client": u.Client.Name, "content_replay": replay}
	if err := s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev}); err != nil {
		return Contribution{}, fmt.Errorf("contribution audit: %w", err)
	}
	c, err := loadVersion(ctx, tx, id, version)
	if err != nil {
		return Contribution{}, fmt.Errorf("load version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Contribution{}, fmt.Errorf("commit: %w", err)
	}
	c.IdempotentReplay = replay
	return c, nil
}

// enqueue schedules curation in the caller's transaction; a repeated event
// for the same version re-queues the existing job instead of duplicating it.
func enqueue(ctx context.Context, tx pgx.Tx, workspaceID, kind, contributionID string, version int) error {
	if _, err := tx.Exec(ctx, `INSERT INTO curation_jobs (workspace_id, kind, contribution_id, contribution_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (contribution_id, contribution_version, kind) DO UPDATE
		SET status = 'queued', attempts = 0, available_at = now(), lease_owner = NULL, lease_until = NULL, updated_at = now()`,
		workspaceID, kind, contributionID, version); err != nil {
		return fmt.Errorf("enqueue curation: %w", err)
	}
	return nil
}

// Get returns the author's current version of one contribution.
func (s *Store) Get(ctx context.Context, workspaceID, authorID, id string) (Contribution, error) {
	c, err := scan(s.pool.QueryRow(ctx, contributionSelect+` AND v.version = c.current_version
		WHERE c.id = $1 AND c.workspace_id = $2 AND c.author_id = $3`, id, workspaceID, authorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Contribution{}, ErrNotFound
	}
	return c, err
}

// List returns the author's contributions changed after since, oldest first,
// so a client can page forward with the last updated_at it saw.
func (s *Store) List(ctx context.Context, workspaceID, authorID string, since time.Time, limit int) ([]Contribution, error) {
	rows, err := s.pool.Query(ctx, contributionSelect+` AND v.version = c.current_version
		WHERE c.workspace_id = $1 AND c.author_id = $2 AND c.updated_at > $3
		ORDER BY c.updated_at, c.id LIMIT $4`, workspaceID, authorID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list contributions: %w", err)
	}
	defer rows.Close()
	out := []Contribution{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan contribution: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Withdraw marks every version withdrawn; a later upload of the same page revives it.
func (s *Store) Withdraw(ctx context.Context, workspaceID, authorID, id string, ev audit.Event) (Contribution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Contribution{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE contributions SET status = 'withdrawn', withdrawn_at = now(), updated_at = now()
		WHERE id = $1 AND workspace_id = $2 AND author_id = $3`, id, workspaceID, authorID)
	if err != nil {
		return Contribution{}, fmt.Errorf("withdraw: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Contribution{}, ErrNotFound
	}
	var version int
	if err := tx.QueryRow(ctx, `SELECT current_version FROM contributions WHERE id = $1`, id).Scan(&version); err != nil {
		return Contribution{}, fmt.Errorf("load version: %w", err)
	}
	if err := enqueue(ctx, tx, workspaceID, "withdrawal", id, version); err != nil {
		return Contribution{}, err
	}
	ev.Type = "contribution.withdrawn"
	ev.Resource = "contribution/" + id
	if err := s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev}); err != nil {
		return Contribution{}, fmt.Errorf("contribution audit: %w", err)
	}
	c, err := scan(tx.QueryRow(ctx, contributionSelect+` AND v.version = c.current_version WHERE c.id = $1`, id))
	if err != nil {
		return Contribution{}, fmt.Errorf("load: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Contribution{}, fmt.Errorf("commit: %w", err)
	}
	return c, nil
}
