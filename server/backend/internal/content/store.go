package content

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuanpu.dev/server/internal/authorization"
	"yuanpu.dev/server/internal/identity"
)

var (
	ErrInvalid     = errors.New("invalid resource")
	ErrForbidden   = errors.New("resource access denied")
	ErrNotFound    = errors.New("resource not found")
	ErrConflict    = errors.New("resource state conflict")
	ErrUnavailable = errors.New("content database unavailable")
)

type Input struct {
	Kind       string          `json:"kind"`
	Visibility string          `json:"visibility"`
	TeamID     *string         `json:"teamId"`
	Title      string          `json:"title"`
	Summary    string          `json:"summary"`
	Body       string          `json:"body"`
	Details    json.RawMessage `json:"details"`
}

type Resource struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	OwnerID    string          `json:"ownerId"`
	Visibility string          `json:"visibility"`
	TeamID     *string         `json:"teamId"`
	Revision   int             `json:"revision"`
	Status     string          `json:"status"`
	Title      string          `json:"title"`
	Summary    string          `json:"summary"`
	Body       string          `json:"body"`
	Details    json.RawMessage `json:"details"`
}

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func newID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func validInput(in Input) bool {
	if in.Kind != "news" && in.Kind != "wiki" && in.Kind != "capability" {
		return false
	}
	if in.Visibility != "team" && in.Visibility != "organization" {
		return false
	}
	if in.Visibility == "team" && (in.TeamID == nil || *in.TeamID == "") {
		return false
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(in.Title)); n == 0 || n > 160 {
		return false
	}
	if utf8.RuneCountInString(in.Summary) > 300 || len(in.Body) > 200_000 {
		return false
	}
	if in.Kind == "wiki" && (utf8.RuneCountInString(in.Body) == 0 || utf8.RuneCountInString(in.Body) > 800) {
		return false
	}
	if len(in.Details) > 16_384 {
		return false
	}
	if len(in.Details) == 0 {
		return true
	}
	var details map[string]any
	return json.Unmarshal(in.Details, &details) == nil && details != nil
}

func resourceFacts(resource Resource, published bool) authorization.ResourceFacts {
	return authorization.ResourceFacts{
		OwnerID: resource.OwnerID, Visibility: resource.Visibility,
		TeamID: resource.TeamID, Published: published, Status: resource.Status,
	}
}

func scanResource(row pgx.Row) (Resource, *int, error) {
	var resource Resource
	var published *int
	err := row.Scan(&resource.ID, &resource.Kind, &resource.OwnerID, &resource.Visibility, &resource.TeamID, &published, &resource.Revision, &resource.Status, &resource.Title, &resource.Summary, &resource.Body, &resource.Details)
	return resource, published, err
}

const resourceSelect = `SELECT r.id,r.kind,r.owner_id,r.visibility,r.team_id,r.published_revision,v.revision,v.status,v.title,v.summary,v.body,v.details FROM yuanpu_resources r JOIN yuanpu_resource_revisions v ON v.resource_id=r.id`

func (s *Store) Create(ctx context.Context, user identity.User, in Input) (Resource, error) {
	if !validInput(in) {
		return Resource{}, ErrInvalid
	}
	if !authorization.Allowed(user, authorization.Create, authorization.ResourceFacts{OwnerID: user.ID, Visibility: in.Visibility, TeamID: in.TeamID}) {
		return Resource{}, ErrForbidden
	}
	if s.pool == nil {
		return Resource{}, ErrUnavailable
	}
	id, err := newID()
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	if len(in.Details) == 0 {
		in.Details = json.RawMessage(`{}`)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_resources (id,kind,owner_id,team_id,visibility) VALUES ($1,$2,$3,$4,$5)`, id, in.Kind, user.ID, in.TeamID, in.Visibility); err != nil {
		return Resource{}, ErrInvalid
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_resource_revisions (resource_id,revision,title,summary,body,details,status,created_by) VALUES ($1,1,$2,$3,$4,$5,'draft',$6)`, id, strings.TrimSpace(in.Title), in.Summary, in.Body, in.Details, user.ID); err != nil {
		return Resource{}, ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_resource_audit (resource_id,revision,actor_id,action) VALUES ($1,1,$2,'create')`, id, user.ID); err != nil {
		return Resource{}, ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return Resource{}, ErrUnavailable
	}
	return Resource{ID: id, Kind: in.Kind, OwnerID: user.ID, Visibility: in.Visibility, TeamID: in.TeamID, Revision: 1, Status: "draft", Title: strings.TrimSpace(in.Title), Summary: in.Summary, Body: in.Body, Details: in.Details}, nil
}

func (s *Store) Get(ctx context.Context, user identity.User, id string) (Resource, error) {
	if s.pool == nil {
		return Resource{}, ErrUnavailable
	}
	row := s.pool.QueryRow(ctx, resourceSelect+` AND v.revision=(SELECT max(revision) FROM yuanpu_resource_revisions WHERE resource_id=r.id) WHERE r.id=$1`, id)
	resource, published, err := scanResource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	if authorization.Allowed(user, authorization.Read, resourceFacts(resource, false)) {
		return resource, nil
	}
	if published == nil {
		return Resource{}, ErrNotFound
	}
	row = s.pool.QueryRow(ctx, resourceSelect+` AND v.revision=r.published_revision WHERE r.id=$1 AND r.published_revision IS NOT NULL`, id)
	resource, _, err = scanResource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	if !authorization.Allowed(user, authorization.Read, resourceFacts(resource, true)) {
		return Resource{}, ErrNotFound
	}
	return resource, nil
}

func (s *Store) List(ctx context.Context, user identity.User, kind, query string) ([]Resource, error) {
	if !user.Active {
		return nil, ErrForbidden
	}
	if kind != "" && kind != "news" && kind != "wiki" && kind != "capability" {
		return nil, ErrInvalid
	}
	if len(query) > 120 {
		return nil, ErrInvalid
	}
	if s.pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, resourceSelect+` AND v.revision=r.published_revision WHERE r.published_revision IS NOT NULL AND ($1='' OR r.kind=$1) AND ($2='' OR v.title ILIKE '%'||$2||'%' OR v.summary ILIKE '%'||$2||'%') ORDER BY r.updated_at DESC LIMIT 500`, kind, query)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	items := make([]Resource, 0)
	for rows.Next() {
		resource, _, err := scanResource(rows)
		if err != nil {
			return nil, ErrUnavailable
		}
		if authorization.Allowed(user, authorization.Read, resourceFacts(resource, true)) && len(items) < 100 {
			items = append(items, resource)
		}
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return items, nil
}

func (s *Store) ReviewQueue(ctx context.Context, user identity.User) ([]Resource, error) {
	if !authorization.CanReviewAny(user) {
		return nil, ErrForbidden
	}
	if s.pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, resourceSelect+` AND v.revision=(SELECT max(revision) FROM yuanpu_resource_revisions WHERE resource_id=r.id) WHERE v.status='submitted' AND r.owner_id<>$1 ORDER BY r.updated_at DESC LIMIT 500`, user.ID)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	items := make([]Resource, 0)
	for rows.Next() {
		resource, _, err := scanResource(rows)
		if err != nil {
			return nil, ErrUnavailable
		}
		if authorization.Allowed(user, authorization.Review, resourceFacts(resource, false)) && len(items) < 100 {
			items = append(items, resource)
		}
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return items, nil
}

func (s *Store) Mine(ctx context.Context, user identity.User) ([]Resource, error) {
	if !user.Active {
		return nil, ErrForbidden
	}
	if s.pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, resourceSelect+` AND v.revision=(SELECT max(revision) FROM yuanpu_resource_revisions WHERE resource_id=r.id) WHERE r.owner_id=$1 ORDER BY r.updated_at DESC LIMIT 100`, user.ID)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	items := make([]Resource, 0)
	for rows.Next() {
		resource, _, err := scanResource(rows)
		if err != nil {
			return nil, ErrUnavailable
		}
		if authorization.Allowed(user, authorization.Read, resourceFacts(resource, false)) {
			items = append(items, resource)
		}
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return items, nil
}

func (s *Store) Edit(ctx context.Context, user identity.User, id string, in Input) (Resource, error) {
	if !validInput(in) {
		return Resource{}, ErrInvalid
	}
	if s.pool == nil {
		return Resource{}, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	var resource Resource
	var previousStatus string
	err = tx.QueryRow(ctx, `SELECT id,kind,owner_id,visibility,team_id FROM yuanpu_resources WHERE id=$1 FOR UPDATE`, id).Scan(&resource.ID, &resource.Kind, &resource.OwnerID, &resource.Visibility, &resource.TeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	if !authorization.Allowed(user, authorization.Edit, resourceFacts(resource, false)) {
		return Resource{}, ErrForbidden
	}
	if in.Kind != resource.Kind || in.Visibility != resource.Visibility || !sameTeam(in.TeamID, resource.TeamID) {
		return Resource{}, ErrInvalid
	}
	var revision int
	if err := tx.QueryRow(ctx, `SELECT revision,status FROM yuanpu_resource_revisions WHERE resource_id=$1 ORDER BY revision DESC LIMIT 1`, id).Scan(&revision, &previousStatus); err != nil {
		return Resource{}, ErrUnavailable
	}
	if previousStatus == "submitted" {
		return Resource{}, ErrConflict
	}
	revision++
	if len(in.Details) == 0 {
		in.Details = json.RawMessage(`{}`)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_resource_revisions (resource_id,revision,title,summary,body,details,status,created_by) VALUES ($1,$2,$3,$4,$5,$6,'draft',$7)`, id, revision, strings.TrimSpace(in.Title), in.Summary, in.Body, in.Details, user.ID); err != nil {
		return Resource{}, ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `UPDATE yuanpu_resources SET updated_at=now() WHERE id=$1`, id); err != nil {
		return Resource{}, ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_resource_audit (resource_id,revision,actor_id,action) VALUES ($1,$2,$3,'edit')`, id, revision, user.ID); err != nil {
		return Resource{}, ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return Resource{}, ErrUnavailable
	}
	resource.Revision = revision
	resource.Status = "draft"
	resource.Title = strings.TrimSpace(in.Title)
	resource.Summary = in.Summary
	resource.Body = in.Body
	resource.Details = in.Details
	return resource, nil
}

func sameTeam(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func (s *Store) Transition(ctx context.Context, user identity.User, id, action string) (Resource, error) {
	if action != "submit" && action != "approve" && action != "reject" && action != "withdraw" {
		return Resource{}, ErrInvalid
	}
	if s.pool == nil {
		return Resource{}, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	row := tx.QueryRow(ctx, resourceSelect+` AND v.revision=(SELECT max(revision) FROM yuanpu_resource_revisions WHERE resource_id=r.id) WHERE r.id=$1 FOR UPDATE OF r`, id)
	resource, published, err := scanResource(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Resource{}, ErrNotFound
	}
	if err != nil {
		return Resource{}, ErrUnavailable
	}
	switch action {
	case "submit":
		if !authorization.Allowed(user, authorization.Submit, resourceFacts(resource, false)) {
			return Resource{}, ErrForbidden
		}
		if resource.Status != "draft" && resource.Status != "rejected" {
			return Resource{}, ErrConflict
		}
		resource.Status = "submitted"
	case "approve", "reject":
		if !authorization.Allowed(user, authorization.Review, resourceFacts(resource, false)) {
			return Resource{}, ErrForbidden
		}
		if resource.Status != "submitted" {
			return Resource{}, ErrConflict
		}
		var confirmed bool
		if err := tx.QueryRow(ctx, `SELECT author_confirmed_at IS NOT NULL FROM yuanpu_resource_revisions WHERE resource_id=$1 AND revision=$2`, id, resource.Revision).Scan(&confirmed); err != nil {
			return Resource{}, ErrUnavailable
		}
		if !confirmed {
			return Resource{}, ErrConflict
		}
		if action == "approve" {
			resource.Status = "approved"
		} else {
			resource.Status = "rejected"
		}
	case "withdraw":
		if !authorization.Allowed(user, authorization.Withdraw, resourceFacts(resource, published != nil)) {
			return Resource{}, ErrForbidden
		}
		if published == nil {
			return Resource{}, ErrConflict
		}
	}
	if action == "withdraw" {
		if _, err := tx.Exec(ctx, `UPDATE yuanpu_resources SET published_revision=NULL,updated_at=now() WHERE id=$1`, id); err != nil {
			return Resource{}, ErrUnavailable
		}
		if resource.Status == "submitted" {
			if _, err := tx.Exec(ctx, `UPDATE yuanpu_resource_revisions SET status='rejected',reviewed_by=$3,reviewed_at=now() WHERE resource_id=$1 AND revision=$2`, id, resource.Revision, user.ID); err != nil {
				return Resource{}, ErrUnavailable
			}
			resource.Status = "rejected"
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE yuanpu_resource_revisions SET status=$3,author_confirmed_at=CASE WHEN $3='submitted' THEN now() ELSE author_confirmed_at END,reviewed_by=CASE WHEN $3 IN ('approved','rejected') THEN $4::text ELSE NULL END,reviewed_at=CASE WHEN $3 IN ('approved','rejected') THEN now() ELSE NULL END WHERE resource_id=$1 AND revision=$2`, id, resource.Revision, resource.Status, user.ID); err != nil {
			return Resource{}, ErrUnavailable
		}
		if action == "approve" {
			if _, err := tx.Exec(ctx, `UPDATE yuanpu_resources SET published_revision=$2,updated_at=now() WHERE id=$1`, id, resource.Revision); err != nil {
				return Resource{}, ErrUnavailable
			}
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_resource_audit (resource_id,revision,actor_id,action) VALUES ($1,$2,$3,$4)`, id, resource.Revision, user.ID, action); err != nil {
		return Resource{}, ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return Resource{}, ErrUnavailable
	}
	return resource, nil
}
