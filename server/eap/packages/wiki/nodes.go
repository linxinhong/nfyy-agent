package wiki

// Node tree persistence (migration 000025). Admins shape the taxonomy;
// pages hang under chapters through content_resources.node_id.
//
// Pitfalls:
//   - Level rules (shelf → book → chapter) are checked here, not by SQL.
//   - Renaming a slug changes the path of every page below it; there are no
//     redirects yet, so internal [[links]] to old paths stop resolving.
//   - A node with children or pages cannot be deleted (RESTRICT → ErrInUse).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/audit"
)

var (
	ErrInvalid  = errors.New("invalid wiki node")
	ErrNotFound = errors.New("wiki node not found")
	ErrConflict = errors.New("slug already used by a sibling")
	ErrInUse    = errors.New("node still has children or pages")
)

// TxAudit writes audit events inside a caller-owned transaction.
type TxAudit interface {
	WriteBatchTx(ctx context.Context, tx pgx.Tx, events []audit.Event) error
}

// Node is one shelf, book or chapter.
type Node struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	Level    string `json:"level"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Sort     int    `json:"sort"`
	Path     string `json:"path"`
}

// NodeInput creates a node; on update only Title, Slug and Sort apply.
type NodeInput struct {
	ParentID string `json:"parent_id"`
	Level    string `json:"level"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Sort     int    `json:"sort"`
}

// Store persists the node tree.
type Store struct {
	pool  *pgxpool.Pool
	audit TxAudit
}

func NewStore(pool *pgxpool.Pool, txAudit TxAudit) *Store { return &Store{pool: pool, audit: txAudit} }

func validTitle(t string) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(t))
	return n >= 1 && n <= 120
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func (s *Store) inTx(ctx context.Context, ev audit.Event, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev}); err != nil {
		return fmt.Errorf("wiki audit: %w", err)
	}
	return tx.Commit(ctx)
}

// Tree lists every node of the Workspace with its slash path, parents first.
func (s *Store) Tree(ctx context.Context, workspaceID string) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE tree AS (
			SELECT id, parent_id, level, slug, title, sort, slug::text AS path, 0 AS depth
			FROM wiki_nodes WHERE workspace_id = $1 AND parent_id IS NULL
			UNION ALL
			SELECT n.id, n.parent_id, n.level, n.slug, n.title, n.sort, tree.path || '/' || n.slug, tree.depth + 1
			FROM wiki_nodes n JOIN tree ON n.parent_id = tree.id
			WHERE n.workspace_id = $1
		)
		SELECT id, COALESCE(parent_id, ''), level, slug, title, sort, path FROM tree
		ORDER BY depth, sort, title`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("wiki tree: %w", err)
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Level, &n.Slug, &n.Title, &n.Sort, &n.Path); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Create adds a node under a parent of the required level.
func (s *Store) Create(ctx context.Context, workspaceID string, in NodeInput, ev audit.Event) (Node, error) {
	in.Slug, in.Title = NormalizeSlug(in.Slug), strings.TrimSpace(in.Title)
	wantParent, ok := ParentLevel[in.Level]
	if !ok || !ValidSlug(in.Slug) || !validTitle(in.Title) || (wantParent == "") != (in.ParentID == "") {
		return Node{}, ErrInvalid
	}
	n := Node{ParentID: in.ParentID, Level: in.Level, Slug: in.Slug, Title: in.Title, Sort: in.Sort}
	err := s.inTx(ctx, ev, func(tx pgx.Tx) error {
		if in.ParentID != "" {
			var parentLevel string
			err := tx.QueryRow(ctx, `SELECT level FROM wiki_nodes WHERE id = $1 AND workspace_id = $2`, in.ParentID, workspaceID).Scan(&parentLevel)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && parentLevel != wantParent) {
				return ErrInvalid
			}
			if err != nil {
				return fmt.Errorf("load parent: %w", err)
			}
		}
		var parent *string
		if in.ParentID != "" {
			parent = &in.ParentID
		}
		err := tx.QueryRow(ctx, `INSERT INTO wiki_nodes (workspace_id, parent_id, level, slug, title, sort)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, workspaceID, parent, in.Level, in.Slug, in.Title, in.Sort).Scan(&n.ID)
		if pgCode(err) == "23505" {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("insert node: %w", err)
		}
		ev.Type = "wiki.node_created"
		ev.Resource = "wiki-node/" + n.ID
		ev.Detail = map[string]any{"level": in.Level, "slug": in.Slug, "parent_id": in.ParentID}
		return nil
	})
	return n, err
}

// Update changes title, slug and sort order of a node.
func (s *Store) Update(ctx context.Context, workspaceID, id string, in NodeInput, ev audit.Event) error {
	in.Slug, in.Title = NormalizeSlug(in.Slug), strings.TrimSpace(in.Title)
	if !ValidSlug(in.Slug) || !validTitle(in.Title) {
		return ErrInvalid
	}
	ev.Type = "wiki.node_updated"
	ev.Resource = "wiki-node/" + id
	ev.Detail = map[string]any{"slug": in.Slug, "title": in.Title, "sort": in.Sort}
	return s.inTx(ctx, ev, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE wiki_nodes SET slug = $3, title = $4, sort = $5 WHERE id = $1 AND workspace_id = $2`,
			id, workspaceID, in.Slug, in.Title, in.Sort)
		if pgCode(err) == "23505" {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("update node: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Delete removes an empty node.
func (s *Store) Delete(ctx context.Context, workspaceID, id string, ev audit.Event) error {
	ev.Type = "wiki.node_deleted"
	ev.Resource = "wiki-node/" + id
	return s.inTx(ctx, ev, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM wiki_nodes WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
		if pgCode(err) == "23503" {
			return ErrInUse
		}
		if err != nil {
			return fmt.Errorf("delete node: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ChapterInWorkspace reports whether id is a chapter of the Workspace.
func (s *Store) ChapterInWorkspace(ctx context.Context, workspaceID, id string) (bool, error) {
	var level string
	err := s.pool.QueryRow(ctx, `SELECT level FROM wiki_nodes WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&level)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return level == LevelChapter, err
}
