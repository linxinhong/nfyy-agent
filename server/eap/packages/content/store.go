package content

// Persistence for content_resources / content_revisions (migrations 000023,
// 000025). Wiki pages are resources of kind wiki placed under a chapter.
//
// Every mutation writes its audit event through TxAudit inside the same
// transaction, so content and its audit evidence commit or fail together.
// Reads that miss authorization report ErrNotFound, never ErrForbidden, so a
// caller cannot probe for resources it may not see.
//
// Each revision stores the rendered plain text (search), its internal links
// and its source chain; tags are current state, not per revision.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/wiki"
)

var (
	ErrInvalid   = errors.New("invalid resource")
	ErrForbidden = errors.New("resource action not permitted")
	ErrNotFound  = errors.New("resource not found")
	ErrConflict  = errors.New("resource state conflict")
)

const (
	maxTitleRunes   = 160
	maxSummaryRunes = 300
	maxBodyBytes    = 200_000
	maxDetailsBytes = 16_384
	maxQueryBytes   = 120
	listLimit       = 100
	maxTags         = 20
	maxTagRunes     = 40
	maxSources      = 50
	maxAnchorRunes  = 120
)

// TxAudit writes audit events inside a caller-owned transaction.
type TxAudit interface {
	WriteBatchTx(ctx context.Context, tx pgx.Tx, events []audit.Event) error
}

// Input is the writable part of a resource revision.
type Input struct {
	Kind       string          `json:"kind"`
	Visibility string          `json:"visibility"`
	GroupID    string          `json:"group_id,omitempty"`
	Title      string          `json:"title"`
	Summary    string          `json:"summary"`
	Body       string          `json:"body"`
	Details    json.RawMessage `json:"details,omitempty"`
	// NodeID and Slug place a wiki page under a chapter (both or neither).
	NodeID string   `json:"node_id,omitempty"`
	Slug   string   `json:"slug,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	// BaseRevision, when set, must equal the latest revision (optimistic lock).
	BaseRevision *int `json:"base_revision,omitempty"`
	// Sources is the source chain of this revision.
	Sources []SourceRef `json:"sources,omitempty"`
}

// SourceRef points a revision at the contribution version it draws on.
type SourceRef struct {
	ContributionID string `json:"contribution_id"`
	Version        int    `json:"version"`
	Anchor         string `json:"anchor,omitempty"`
}

// Resource is one revision of a resource as returned to callers.
type Resource struct {
	ID                string          `json:"id"`
	Kind              string          `json:"kind"`
	OwnerID           string          `json:"owner_id"`
	Visibility        string          `json:"visibility"`
	GroupID           string          `json:"group_id,omitempty"`
	Revision          int             `json:"revision"`
	PublishedRevision *int            `json:"published_revision,omitempty"`
	Status            string          `json:"status"`
	Title             string          `json:"title"`
	Summary           string          `json:"summary"`
	Body              string          `json:"body"`
	Details           json.RawMessage `json:"details"`
	NodeID            string          `json:"node_id,omitempty"`
	Slug              string          `json:"slug,omitempty"`
	Path              string          `json:"path,omitempty"`
	Tags              []string        `json:"tags"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

func (r Resource) facts() Facts {
	return Facts{OwnerID: r.OwnerID, Visibility: r.Visibility, GroupID: r.GroupID, Published: r.PublishedRevision != nil}
}

// publishedFacts treats the row as the published view a non-owner sees.
func (r Resource) publishedFacts() Facts {
	f := r.facts()
	f.Published = true
	return f
}

// Store is the pgx-backed content store.
type Store struct {
	pool  *pgxpool.Pool
	audit TxAudit
}

// New wraps a pool and the transactional audit writer.
func New(pool *pgxpool.Pool, txAudit TxAudit) *Store {
	return &Store{pool: pool, audit: txAudit}
}

// ActorFor resolves the member's group and permissions inside the Workspace.
// Roles come from the caller's live membership check.
func (s *Store) ActorFor(ctx context.Context, workspaceID, userID string, roles []string) (Actor, error) {
	g, err := rbac.Load(ctx, s.pool, workspaceID, userID, roles)
	if err != nil {
		return Actor{}, err
	}
	return Actor{UserID: userID, Grants: g, GroupID: g.GroupID}, nil
}

func validKind(kind string) bool {
	return kind == "news" || kind == "wiki" || kind == "capability"
}

// normalize validates and trims an input in place.
func normalize(in *Input) error {
	in.Title = strings.TrimSpace(in.Title)
	in.GroupID = strings.TrimSpace(in.GroupID)
	if !validKind(in.Kind) {
		return ErrInvalid
	}
	switch in.Visibility {
	case VisibilityOrganization:
		if in.GroupID != "" {
			return ErrInvalid
		}
	case VisibilityTeam:
		if in.GroupID == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if n := utf8.RuneCountInString(in.Title); n == 0 || n > maxTitleRunes {
		return ErrInvalid
	}
	if utf8.RuneCountInString(in.Summary) > maxSummaryRunes || len(in.Body) > maxBodyBytes {
		return ErrInvalid
	}
	if len(in.Details) == 0 {
		in.Details = json.RawMessage(`{}`)
	}
	if len(in.Details) > maxDetailsBytes {
		return ErrInvalid
	}
	var obj map[string]any
	if json.Unmarshal(in.Details, &obj) != nil || obj == nil {
		return ErrInvalid
	}
	in.NodeID = strings.TrimSpace(in.NodeID)
	in.Slug = wiki.NormalizeSlug(in.Slug)
	if (in.NodeID == "") != (in.Slug == "") || (in.NodeID != "" && (in.Kind != "wiki" || !wiki.ValidSlug(in.Slug))) {
		return ErrInvalid
	}
	tags, seen := []string{}, map[string]bool{}
	for _, tag := range in.Tags {
		tag = strings.TrimSpace(tag)
		if n := utf8.RuneCountInString(tag); n == 0 || n > maxTagRunes {
			return ErrInvalid
		}
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	if len(tags) > maxTags || len(in.Sources) > maxSources {
		return ErrInvalid
	}
	in.Tags = tags
	for _, src := range in.Sources {
		if src.ContributionID == "" || src.Version < 1 || utf8.RuneCountInString(src.Anchor) > maxAnchorRunes {
			return ErrInvalid
		}
	}
	return nil
}

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "res_" + hex.EncodeToString(b[:]), nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// resourceSelect computes the page path and tags with scalar subqueries so
// callers can still lock the resource row with FOR UPDATE OF r.
const resourceSelect = `SELECT r.id, r.kind, r.owner_id, r.visibility, COALESCE(r.group_id, ''), r.published_revision,
	v.revision, v.status, v.title, v.summary, v.body, v.details,
	COALESCE(r.node_id, ''), COALESCE(r.slug, ''),
	COALESCE((SELECT sh.slug || '/' || bk.slug || '/' || ch.slug FROM wiki_nodes ch
		JOIN wiki_nodes bk ON bk.id = ch.parent_id JOIN wiki_nodes sh ON sh.id = bk.parent_id
		WHERE ch.id = r.node_id) || '/' || r.slug, ''),
	ARRAY(SELECT tag FROM content_tags t WHERE t.resource_id = r.id ORDER BY tag), r.updated_at
	FROM content_resources r JOIN content_revisions v ON v.resource_id = r.id`

const latestRevision = ` AND v.revision = (SELECT max(revision) FROM content_revisions WHERE resource_id = r.id)`

func scanResource(row pgx.Row) (Resource, error) {
	var r Resource
	err := row.Scan(&r.ID, &r.Kind, &r.OwnerID, &r.Visibility, &r.GroupID, &r.PublishedRevision,
		&r.Revision, &r.Status, &r.Title, &r.Summary, &r.Body, &r.Details,
		&r.NodeID, &r.Slug, &r.Path, &r.Tags, &r.UpdatedAt)
	return r, err
}

// place checks that a wiki page's chapter belongs to the Workspace.
func place(ctx context.Context, tx pgx.Tx, workspaceID, nodeID string) error {
	if nodeID == "" {
		return nil
	}
	var level string
	err := tx.QueryRow(ctx, `SELECT level FROM wiki_nodes WHERE id = $1 AND workspace_id = $2`, nodeID, workspaceID).Scan(&level)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && level != wiki.LevelChapter) {
		return ErrInvalid
	}
	return err
}

// writeRevisionExtras stores what a revision derives from its body and
// input: plain text, internal links, tags and the source chain. Sources must
// be contribution versions of this Workspace the actor may cite (its own, or
// any for reviewers and admins).
func writeRevisionExtras(ctx context.Context, tx pgx.Tx, workspaceID string, a Actor, id string, revision int, in Input) error {
	rendered, err := wiki.Render(in.Body)
	if err != nil {
		return ErrInvalid
	}
	if _, err := tx.Exec(ctx, `UPDATE content_revisions SET body_text = $3 WHERE resource_id = $1 AND revision = $2`,
		id, revision, rendered.Text); err != nil {
		return fmt.Errorf("store text: %w", err)
	}
	for _, target := range rendered.Links {
		if _, err := tx.Exec(ctx, `INSERT INTO content_revision_links (resource_id, revision, target_path) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, id, revision, target); err != nil {
			return fmt.Errorf("store link: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM content_tags WHERE resource_id = $1`, id); err != nil {
		return fmt.Errorf("clear tags: %w", err)
	}
	for _, tag := range in.Tags {
		if _, err := tx.Exec(ctx, `INSERT INTO content_tags (resource_id, tag) VALUES ($1, $2)`, id, tag); err != nil {
			return fmt.Errorf("store tag: %w", err)
		}
	}
	return writeSources(ctx, tx, workspaceID, a, id, revision, in.Sources)
}

// writeSources appends source-chain entries the actor may cite.
func writeSources(ctx context.Context, tx pgx.Tx, workspaceID string, a Actor, id string, revision int, sources []SourceRef) error {
	for _, src := range sources {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contribution_versions cv JOIN contributions c ON c.id = cv.contribution_id
			WHERE c.id = $1 AND cv.version = $2 AND c.workspace_id = $3 AND (c.author_id = $4 OR $5))`,
			src.ContributionID, src.Version, workspaceID, a.UserID, a.broad() || a.CiteContributions).Scan(&ok); err != nil {
			return fmt.Errorf("check source: %w", err)
		}
		if !ok {
			return ErrInvalid
		}
		if _, err := tx.Exec(ctx, `INSERT INTO content_revision_sources (resource_id, revision, contribution_id, contribution_version, anchor)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, id, revision, src.ContributionID, src.Version, src.Anchor); err != nil {
			return fmt.Errorf("store source: %w", err)
		}
	}
	return nil
}

// slugTaken maps a sibling slug clash on a page to ErrConflict.
func slugTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) commitWithAudit(ctx context.Context, tx pgx.Tx, ev audit.Event, action string, r Resource) error {
	ev.Type = audit.EventType("content." + action)
	ev.Resource = "content/" + r.ID
	ev.Detail = map[string]any{"kind": r.Kind, "revision": r.Revision, "visibility": r.Visibility, "group_id": r.GroupID}
	if err := s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev}); err != nil {
		return fmt.Errorf("content audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Create stores a private draft (revision 1); submission is a separate step.
func (s *Store) Create(ctx context.Context, workspaceID string, a Actor, in Input, ev audit.Event) (Resource, error) {
	if err := normalize(&in); err != nil {
		return Resource{}, err
	}
	if !Allowed(a, Create, Facts{Visibility: in.Visibility, GroupID: in.GroupID}) {
		return Resource{}, ErrForbidden
	}
	id, err := newID()
	if err != nil {
		return Resource{}, fmt.Errorf("resource id: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Resource{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := place(ctx, tx, workspaceID, in.NodeID); err != nil {
		return Resource{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO content_resources (id, workspace_id, kind, owner_id, visibility, group_id, node_id, slug)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, workspaceID, in.Kind, a.UserID, in.Visibility, nullable(in.GroupID),
		nullable(in.NodeID), nullable(in.Slug))
	if slugTaken(err) {
		return Resource{}, ErrConflict
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return Resource{}, ErrInvalid // group is not part of this Workspace
	}
	if err != nil {
		return Resource{}, fmt.Errorf("insert resource: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO content_revisions (resource_id, revision, title, summary, body, details, status, created_by)
		VALUES ($1, 1, $2, $3, $4, $5, 'draft', $6)`, id, in.Title, in.Summary, in.Body, in.Details, a.UserID); err != nil {
		return Resource{}, fmt.Errorf("insert revision: %w", err)
	}
	if err := writeRevisionExtras(ctx, tx, workspaceID, a, id, 1, in); err != nil {
		return Resource{}, err
	}
	r, err := scanResource(tx.QueryRow(ctx, resourceSelect+` AND v.revision = 1 WHERE r.id = $1`, id))
	if err != nil {
		return Resource{}, fmt.Errorf("load created: %w", err)
	}
	if err := s.commitWithAudit(ctx, tx, ev, "create", r); err != nil {
		return Resource{}, err
	}
	return r, nil
}

// Get returns the latest revision to those allowed to see drafts, otherwise
// the currently published revision.
func (s *Store) Get(ctx context.Context, workspaceID string, a Actor, id string) (Resource, error) {
	r, err := scanResource(s.pool.QueryRow(ctx, resourceSelect+latestRevision+` WHERE r.id = $1 AND r.workspace_id = $2`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, fmt.Errorf("load resource: %w", err)
	}
	draftView := r.facts()
	draftView.Published = false
	if Allowed(a, Read, draftView) {
		return r, nil
	}
	if r.PublishedRevision == nil {
		return Resource{}, ErrNotFound
	}
	r, err = scanResource(s.pool.QueryRow(ctx, resourceSelect+` AND v.revision = r.published_revision WHERE r.id = $1 AND r.workspace_id = $2`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, fmt.Errorf("load published: %w", err)
	}
	if !Allowed(a, Read, r.publishedFacts()) {
		return Resource{}, ErrNotFound
	}
	return r, nil
}

// List returns published resources the actor may read (newest first).
func (s *Store) List(ctx context.Context, workspaceID string, a Actor, kind, query string) ([]Resource, error) {
	if kind != "" && !validKind(kind) {
		return nil, ErrInvalid
	}
	if len(query) > maxQueryBytes {
		return nil, ErrInvalid
	}
	broad := a.broad()
	rows, err := s.pool.Query(ctx, resourceSelect+` AND v.revision = r.published_revision
		WHERE r.workspace_id = $1 AND r.published_revision IS NOT NULL
		  AND (r.visibility = 'organization' OR (r.visibility = 'team' AND r.group_id = $2) OR $3)
		  AND ($4 = '' OR r.kind = $4)
		  AND ($5 = '' OR v.title ILIKE '%' || $5 || '%' OR v.summary ILIKE '%' || $5 || '%')
		ORDER BY r.updated_at DESC LIMIT $6`, workspaceID, a.GroupID, broad, kind, query, listLimit)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	return collect(rows, func(r Resource) bool { return Allowed(a, Read, r.publishedFacts()) })
}

// ReviewQueue returns submitted latest revisions the actor may review.
func (s *Store) ReviewQueue(ctx context.Context, workspaceID string, a Actor) ([]Resource, error) {
	if !a.CanReviewAny() {
		return nil, ErrForbidden
	}
	broad := a.broad()
	rows, err := s.pool.Query(ctx, resourceSelect+latestRevision+`
		WHERE r.workspace_id = $1 AND v.status = 'submitted' AND r.owner_id <> $2
		  AND ($3 OR (r.visibility = 'team' AND r.group_id = $4))
		ORDER BY r.updated_at DESC LIMIT $5`, workspaceID, a.UserID, broad, a.GroupID, listLimit)
	if err != nil {
		return nil, fmt.Errorf("review queue: %w", err)
	}
	return collect(rows, func(r Resource) bool {
		f := r.facts()
		f.Published = false
		return Allowed(a, Review, f)
	})
}

func collect(rows pgx.Rows, keep func(Resource) bool) ([]Resource, error) {
	defer rows.Close()
	out := []Resource{}
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan resource: %w", err)
		}
		if keep(r) {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// lockLatest loads the resource row FOR UPDATE with its latest revision.
func lockLatest(ctx context.Context, tx pgx.Tx, workspaceID, id string) (Resource, error) {
	r, err := scanResource(tx.QueryRow(ctx, resourceSelect+latestRevision+
		` WHERE r.id = $1 AND r.workspace_id = $2 FOR UPDATE OF r`, id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, fmt.Errorf("lock resource: %w", err)
	}
	return r, nil
}

// Edit appends a new draft revision. Kind, visibility and group are fixed at
// creation; a submitted revision must be reviewed or withdrawn first.
func (s *Store) Edit(ctx context.Context, workspaceID string, a Actor, id string, in Input, ev audit.Event) (Resource, error) {
	if err := normalize(&in); err != nil {
		return Resource{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Resource{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := lockLatest(ctx, tx, workspaceID, id)
	if err != nil {
		return Resource{}, err
	}
	if !Allowed(a, Edit, r.facts()) {
		if Allowed(a, Read, r.facts()) {
			return Resource{}, ErrForbidden
		}
		return Resource{}, ErrNotFound
	}
	if in.Kind != r.Kind || in.Visibility != r.Visibility || in.GroupID != r.GroupID {
		return Resource{}, ErrInvalid
	}
	if r.Status == "submitted" || (in.BaseRevision != nil && *in.BaseRevision != r.Revision) {
		return Resource{}, ErrConflict
	}
	if in.NodeID != r.NodeID || in.Slug != r.Slug {
		if err := place(ctx, tx, workspaceID, in.NodeID); err != nil {
			return Resource{}, err
		}
		_, err := tx.Exec(ctx, `UPDATE content_resources SET node_id = $2, slug = $3 WHERE id = $1`, id, nullable(in.NodeID), nullable(in.Slug))
		if slugTaken(err) {
			return Resource{}, ErrConflict
		}
		if err != nil {
			return Resource{}, fmt.Errorf("move page: %w", err)
		}
	}
	r.Revision++
	if _, err := tx.Exec(ctx, `
		INSERT INTO content_revisions (resource_id, revision, title, summary, body, details, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'draft', $7)`, id, r.Revision, in.Title, in.Summary, in.Body, in.Details, a.UserID); err != nil {
		return Resource{}, fmt.Errorf("insert revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE content_resources SET updated_at = now() WHERE id = $1`, id); err != nil {
		return Resource{}, fmt.Errorf("touch resource: %w", err)
	}
	if err := writeRevisionExtras(ctx, tx, workspaceID, a, id, r.Revision, in); err != nil {
		return Resource{}, err
	}
	if r, err = scanResource(tx.QueryRow(ctx, resourceSelect+` AND v.revision = $2 WHERE r.id = $1`, id, r.Revision)); err != nil {
		return Resource{}, fmt.Errorf("load edited: %w", err)
	}
	if err := s.commitWithAudit(ctx, tx, ev, "edit", r); err != nil {
		return Resource{}, err
	}
	return r, nil
}

// Transition applies submit, approve, reject or withdraw to the latest revision.
func (s *Store) Transition(ctx context.Context, workspaceID string, a Actor, id, action string, ev audit.Event) (Resource, error) {
	if action != "submit" && action != "approve" && action != "reject" && action != "withdraw" {
		return Resource{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Resource{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := lockLatest(ctx, tx, workspaceID, id)
	if err != nil {
		return Resource{}, err
	}
	denied := func() (Resource, error) {
		if Allowed(a, Read, r.facts()) {
			return Resource{}, ErrForbidden
		}
		return Resource{}, ErrNotFound
	}
	draftView := r.facts()
	draftView.Published = false
	switch action {
	case "submit":
		if !Allowed(a, Submit, draftView) {
			return denied()
		}
		if r.Status != "draft" && r.Status != "rejected" {
			return Resource{}, ErrConflict
		}
		r.Status = "submitted"
	case "approve", "reject":
		if !Allowed(a, Review, draftView) {
			return denied()
		}
		if r.Status != "submitted" {
			return Resource{}, ErrConflict
		}
		r.Status = map[string]string{"approve": "approved", "reject": "rejected"}[action]
	case "withdraw":
		if !Allowed(a, Withdraw, r.facts()) {
			return denied()
		}
		if r.PublishedRevision == nil {
			return Resource{}, ErrConflict
		}
	}
	reviewed := action == "approve" || action == "reject" || (action == "withdraw" && r.Status == "submitted")
	if action == "withdraw" {
		if _, err := tx.Exec(ctx, `UPDATE content_resources SET published_revision = NULL, updated_at = now() WHERE id = $1`, id); err != nil {
			return Resource{}, fmt.Errorf("withdraw: %w", err)
		}
		r.PublishedRevision = nil
		if r.Status == "submitted" {
			r.Status = "rejected"
		}
	}
	if action != "withdraw" || reviewed {
		var reviewer *string
		if reviewed {
			reviewer = &a.UserID
		}
		if _, err := tx.Exec(ctx, `
			UPDATE content_revisions
			SET status = $3, reviewed_by = $4, reviewed_at = CASE WHEN $4::text IS NULL THEN NULL ELSE now() END
			WHERE resource_id = $1 AND revision = $2`, id, r.Revision, r.Status, reviewer); err != nil {
			return Resource{}, fmt.Errorf("update revision: %w", err)
		}
	}
	if action == "approve" {
		if _, err := tx.Exec(ctx, `UPDATE content_resources SET published_revision = $2, updated_at = now() WHERE id = $1`, id, r.Revision); err != nil {
			return Resource{}, fmt.Errorf("publish: %w", err)
		}
		rev := r.Revision
		r.PublishedRevision = &rev
	}
	if err := s.commitWithAudit(ctx, tx, ev, action, r); err != nil {
		return Resource{}, err
	}
	return r, nil
}

// ProposeRevision appends a submitted revision written by a curator to a page
// the curator may read, without taking ownership: the page owner and
// visibility stay, and the revision is published only through review.
// The new revision's source chain is the previous one plus in.Sources.
func (s *Store) ProposeRevision(ctx context.Context, workspaceID string, a Actor, id string, in Input, ev audit.Event) (Resource, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Resource{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := lockLatest(ctx, tx, workspaceID, id)
	if err != nil {
		return Resource{}, err
	}
	if !Allowed(a, Read, r.publishedFacts()) && !Allowed(a, Read, r.facts()) {
		return Resource{}, ErrNotFound
	}
	in.Kind, in.Visibility, in.GroupID, in.NodeID, in.Slug = r.Kind, r.Visibility, r.GroupID, r.NodeID, r.Slug
	if len(in.Tags) == 0 {
		in.Tags = r.Tags
	}
	if err := normalize(&in); err != nil {
		return Resource{}, err
	}
	if r.Status == "submitted" || (in.BaseRevision != nil && *in.BaseRevision != r.Revision) {
		return Resource{}, ErrConflict
	}
	previous, err := revisionSources(ctx, tx, id, r.Revision)
	if err != nil {
		return Resource{}, err
	}
	r.Revision++
	if _, err := tx.Exec(ctx, `
		INSERT INTO content_revisions (resource_id, revision, title, summary, body, details, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'submitted', $7)`, id, r.Revision, in.Title, in.Summary, in.Body, in.Details, a.UserID); err != nil {
		return Resource{}, fmt.Errorf("insert revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE content_resources SET updated_at = now() WHERE id = $1`, id); err != nil {
		return Resource{}, fmt.Errorf("touch resource: %w", err)
	}
	if err := writeRevisionExtras(ctx, tx, workspaceID, a, id, r.Revision, Input{Body: in.Body, Tags: in.Tags}); err != nil {
		return Resource{}, err
	}
	// Prior sources were vetted when their revision was written; new ones are checked for this actor.
	carried := Actor{UserID: a.UserID, Grants: rbac.Of(rbac.KnowledgeReview)}
	if err := writeSources(ctx, tx, workspaceID, carried, id, r.Revision, previous); err != nil {
		return Resource{}, err
	}
	if err := writeSources(ctx, tx, workspaceID, a, id, r.Revision, in.Sources); err != nil {
		return Resource{}, err
	}
	if r, err = scanResource(tx.QueryRow(ctx, resourceSelect+` AND v.revision = $2 WHERE r.id = $1`, id, r.Revision)); err != nil {
		return Resource{}, fmt.Errorf("load proposed: %w", err)
	}
	if err := s.commitWithAudit(ctx, tx, ev, "propose", r); err != nil {
		return Resource{}, err
	}
	return r, nil
}

func revisionSources(ctx context.Context, tx pgx.Tx, id string, revision int) ([]SourceRef, error) {
	rows, err := tx.Query(ctx, `SELECT contribution_id, contribution_version, anchor FROM content_revision_sources
		WHERE resource_id = $1 AND revision = $2`, id, revision)
	if err != nil {
		return nil, fmt.Errorf("previous sources: %w", err)
	}
	defer rows.Close()
	out := []SourceRef{}
	for rows.Next() {
		var src SourceRef
		if err := rows.Scan(&src.ContributionID, &src.Version, &src.Anchor); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}
