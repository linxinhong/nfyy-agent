// Package skillshare is the MindLink layer over the Skill Registry: who may
// see, review, enable or require a skill, and where its protected SKILL.md
// lives.
//
// Why: skills distill senior lawyers' methods for everyone else, so a skill
// is shared at a scope (personal, team, organization) like knowledge, and its
// SKILL.md is the protected asset — stored apart from the downloadable
// package and only ever handed to the model by the gateway, never to a
// client. Resource files (scripts, templates) that must run locally stay in
// the signed, downloadable archive.
//
// Invariants and pitfalls:
//   - A protected SKILL.md is never part of the archive, so no download or
//     EAP sync path can return it; Body is the only reader and it serves the
//     gateway's injection for skills the member has active.
//   - Visibility is per version: a personal skill can later be shared to a
//     team as a new version, which then needs review. Members consume the
//     newest version they can see; a personal version is visible to its
//     author only and never needs review ("private" status, not "released",
//     so Workspace-wide registry readers never see it).
//   - Reviewers act at the skill's scope (skill.review in its team, or
//     Workspace-wide) and never on their own skill.
//   - Requiring a skill for an audience needs skill.require there, and the
//     audience must be able to see a released version.
package skillshare

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/skillreg"
)

const (
	VisibilityPersonal     = "personal"
	VisibilityTeam         = "team"
	VisibilityOrganization = "organization"

	// StatusPrivate marks a personal version: usable by its author, never
	// reviewed, invisible to Workspace-wide registry readers.
	StatusPrivate  = "private"
	StatusRejected = "rejected"

	// SkillFile is the protected entry point.
	SkillFile = "SKILL.md"
	// MarkerFile keeps a protected package's archive non-empty and tells
	// anyone unpacking it where SKILL.md went.
	MarkerFile    = ".mindlink-protected"
	markerContent = "SKILL.md of this skill is protected and served by the MindLink center only.\n"

	maxReasonRunes = 500
)

var (
	ErrInvalid   = errors.New("invalid skill request")
	ErrForbidden = errors.New("skill action is not permitted")
	ErrNotFound  = errors.New("skill not found")
	ErrConflict  = errors.New("skill state has changed")
)

// Viewer is the member a decision is about.
type Viewer struct {
	UserID string
	Grants rbac.Grants
}

// Store persists sharing state next to skill_packages.
type Store struct {
	pool  *pgxpool.Pool
	audit TxAudit
}

// TxAudit writes an audit event inside the caller's transaction.
type TxAudit interface {
	WriteBatchTx(ctx context.Context, tx pgx.Tx, events []audit.Event) error
}

func New(pool *pgxpool.Pool, txAudit TxAudit) *Store { return &Store{pool: pool, audit: txAudit} }

// Publish is one new version.
type Publish struct {
	Name       string
	Version    string
	Visibility string
	GroupID    string // team visibility only
	Protected  bool
	Files      []skillreg.PackageFile
}

// Published is the stored version.
type Published struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Status      string `json:"status"`
	Visibility  string `json:"visibility"`
	GroupID     string `json:"group_id"`
	Protected   bool   `json:"protected"`
	Description string `json:"description"`
	Digest      string `json:"digest"`
}

var frontmatterDescription = regexp.MustCompile(`(?m)^description:\s*(.+?)\s*$`)

// Describe reads the one-line purpose from SKILL.md: the frontmatter
// description, else the first prose line.
func Describe(skill string) string {
	if strings.HasPrefix(skill, "---") {
		if end := strings.Index(skill[3:], "\n---"); end >= 0 {
			if m := frontmatterDescription.FindStringSubmatch(skill[3 : 3+end]); m != nil {
				return clip(strings.Trim(m[1], `"'`), 300)
			}
			skill = skill[3+end+4:]
		}
	}
	for _, line := range strings.Split(skill, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			return clip(line, 300)
		}
	}
	return ""
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}

// Publish stores a new version. A protected version moves SKILL.md out of the
// archive; team visibility needs the author to belong to the team (or hold
// knowledge.manage, which publishes into any team).
func (s *Store) Publish(ctx context.Context, workspaceID string, author Viewer, in Publish, ev audit.Event) (Published, error) {
	out := Published{Name: in.Name, Version: in.Version, Visibility: in.Visibility, Protected: in.Protected}
	switch in.Visibility {
	case VisibilityPersonal, VisibilityOrganization:
		in.GroupID = ""
	case VisibilityTeam:
		if in.GroupID == "" {
			in.GroupID = author.Grants.GroupID
		}
		if in.GroupID == "" || (in.GroupID != author.Grants.GroupID && !author.Grants.Has(rbac.KnowledgeManage)) {
			return out, fmt.Errorf("%w: team skills go to your own team", ErrForbidden)
		}
	default:
		return out, fmt.Errorf("%w: visibility must be personal, team or organization", ErrInvalid)
	}
	out.GroupID = in.GroupID
	files := in.Files
	var body *string
	for _, f := range files {
		if f.Path == SkillFile {
			out.Description = Describe(f.Content)
		}
	}
	if in.Protected {
		var rest []skillreg.PackageFile
		for _, f := range files {
			if f.Path == SkillFile {
				if err := skillreg.ScanContent(f.Path, f.Content); err != nil {
					return out, err
				}
				if len(f.Content) > skillreg.MaxFileBytes {
					return out, fmt.Errorf("%w: SKILL.md exceeds the size limit", ErrInvalid)
				}
				content := f.Content
				body = &content
				continue
			}
			if f.Path == MarkerFile {
				return out, fmt.Errorf("%w: %s is reserved", ErrInvalid, MarkerFile)
			}
			rest = append(rest, f)
		}
		if body == nil {
			return out, fmt.Errorf("%w: a protected skill needs SKILL.md", ErrInvalid)
		}
		files = append(rest, skillreg.PackageFile{Path: MarkerFile, Content: markerContent})
	}
	archive, digest, err := skillreg.BuildArchive(files)
	if err != nil {
		return out, err
	}
	out.Digest = digest
	out.Status = skillreg.StatusPendingReview
	if in.Visibility == VisibilityPersonal {
		out.Status = StatusPrivate
	}
	var group *string
	if in.GroupID != "" {
		group = &in.GroupID
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO skill_packages (name, version, workspace_id, digest, payload, published_by,
			                            status, visibility, group_id, description, protected_body)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING id`, in.Name, in.Version, workspaceID, digest, archive, author.UserID,
			out.Status, in.Visibility, group, out.Description, body).Scan(&out.ID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: skill version already exists (immutable name@version)", ErrConflict)
		}
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return fmt.Errorf("%w: unknown team", ErrInvalid)
		}
		if err != nil {
			return fmt.Errorf("publish skill: %w", err)
		}
		ev.Resource = "skill/" + in.Name + "@" + in.Version
		ev.Detail = map[string]any{"digest": digest, "files": len(in.Files), "visibility": in.Visibility,
			"group_id": in.GroupID, "protected": in.Protected}
		return s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev})
	})
	return out, err
}

// Version is one stored version's facts.
type Version struct {
	ID          string
	Name        string
	Version     string
	Status      string
	Visibility  string
	GroupID     string
	AuthorID    string
	Digest      string
	Description string
	Protected   bool
}

// GetVersion loads one version of the Workspace.
func (s *Store) GetVersion(ctx context.Context, workspaceID, name, version string) (Version, error) {
	var v Version
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, version, status, visibility, COALESCE(group_id, ''), published_by, digest,
		       description, protected_body IS NOT NULL
		FROM skill_packages WHERE workspace_id = $1 AND name = $2 AND version = $3`,
		workspaceID, name, version).Scan(&v.ID, &v.Name, &v.Version, &v.Status, &v.Visibility, &v.GroupID,
		&v.AuthorID, &v.Digest, &v.Description, &v.Protected)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// MayReview reports whether viewer decides on v: skill.review at v's scope,
// never on their own skill, never on a personal one.
func MayReview(viewer Viewer, v Version) bool {
	if viewer.UserID == "" || viewer.UserID == v.AuthorID {
		return false
	}
	switch v.Visibility {
	case VisibilityOrganization:
		return viewer.Grants.Has(rbac.SkillReview)
	case VisibilityTeam:
		return viewer.Grants.HasIn(rbac.SkillReview, v.GroupID)
	}
	return false
}

// MaySee reports whether viewer may use v (consume its resources and, via the
// gateway, its SKILL.md).
func MaySee(viewer Viewer, v Version) bool {
	switch v.Visibility {
	case VisibilityPersonal:
		return v.AuthorID == viewer.UserID && v.Status == StatusPrivate
	case VisibilityTeam:
		return v.Status == skillreg.StatusReleased && v.GroupID != "" && v.GroupID == viewer.Grants.GroupID
	case VisibilityOrganization:
		return v.Status == skillreg.StatusReleased
	}
	return false
}

// MayRead reports whether viewer may read v's SKILL.md in the Portal: its
// author or a reviewer at its scope.
func MayRead(viewer Viewer, v Version) bool {
	return viewer.UserID == v.AuthorID || MayReview(viewer, Version{Visibility: v.Visibility, GroupID: v.GroupID})
}

// Content is a version's SKILL.md and archive file list for review.
type Content struct {
	SkillMD string   `json:"skill_md"`
	Files   []string `json:"files"`
}

// Body returns a version's SKILL.md (protected or from the archive).
func (s *Store) Content(ctx context.Context, v Version) (Content, error) {
	var body *string
	var payload []byte
	if err := s.pool.QueryRow(ctx, `SELECT protected_body, payload FROM skill_packages WHERE id = $1`, v.ID).Scan(&body, &payload); err != nil {
		return Content{}, err
	}
	files, err := skillreg.ExtractBounded(payload)
	if err != nil {
		return Content{}, err
	}
	out := Content{Files: []string{}}
	if body != nil {
		out.SkillMD = *body
	} else {
		out.SkillMD = files[SkillFile]
	}
	for path := range files {
		if path != MarkerFile && path != SkillFile {
			out.Files = append(out.Files, path)
		}
	}
	slices.Sort(out.Files)
	return out, nil
}

// Reject closes a pending version with a reason the author can act on.
func (s *Store) Reject(ctx context.Context, workspaceID string, viewer Viewer, v Version, reason string, ev audit.Event) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len([]rune(reason)) > maxReasonRunes {
		return fmt.Errorf("%w: a reason of 1-%d characters is required", ErrInvalid, maxReasonRunes)
	}
	if !MayReview(viewer, v) {
		return ErrForbidden
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE skill_packages SET status = 'rejected', reviewed_by = $2, reject_reason = $3
			WHERE id = $1 AND status IN ('pending_review', 'approved')`, v.ID, viewer.UserID, reason)
		if err != nil {
			return fmt.Errorf("reject skill: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrConflict
		}
		ev.Resource = "skill/" + v.Name + "@" + v.Version
		ev.Detail = map[string]any{"reviewer": viewer.UserID}
		return s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev})
	})
}

// Pending lists versions awaiting a decision the viewer may make.
func (s *Store) Pending(ctx context.Context, workspaceID string, viewer Viewer) ([]Listed, error) {
	rows, err := s.pool.Query(ctx, listSelect+`
		WHERE p.workspace_id = $1 AND p.status IN ('pending_review', 'approved')
		ORDER BY p.created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("pending skills: %w", err)
	}
	all, err := scanListed(rows)
	if err != nil {
		return nil, err
	}
	out := []Listed{}
	for _, l := range all {
		if MayReview(viewer, l.version()) {
			out = append(out, l)
		}
	}
	return out, nil
}

// Listed is one version as the Portal and the client plugin see it.
type Listed struct {
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Status       string    `json:"status"`
	Visibility   string    `json:"visibility"`
	GroupID      string    `json:"group_id"`
	Description  string    `json:"description"`
	AuthorID     string    `json:"author_id"`
	AuthorName   string    `json:"author_name"`
	Protected    bool      `json:"protected"`
	Digest       string    `json:"digest"`
	HasResources bool      `json:"has_resources"`
	RejectReason string    `json:"reject_reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// Member view (Mine) only.
	Required      bool   `json:"required"`
	RequiredScope string `json:"required_scope,omitempty"` // workspace | team
	Enabled       bool   `json:"enabled"`
	// Active: required, enabled, or the member's own personal skill — the
	// skills the client offers to the model.
	Active bool `json:"active"`
}

func (l Listed) version() Version {
	return Version{Name: l.Name, Version: l.Version, Status: l.Status, Visibility: l.Visibility,
		GroupID: l.GroupID, AuthorID: l.AuthorID, Protected: l.Protected}
}

const listSelect = `
	SELECT p.name, p.version, p.status, p.visibility, COALESCE(p.group_id, ''), p.description,
	       p.published_by, COALESCE(u.display_name, ''), p.protected_body IS NOT NULL, p.digest,
	       COALESCE(p.reject_reason, ''), p.created_at
	FROM skill_packages p LEFT JOIN users u ON u.id = p.published_by`

func scanListed(rows pgx.Rows) ([]Listed, error) {
	defer rows.Close()
	var out []Listed
	for rows.Next() {
		var l Listed
		if err := rows.Scan(&l.Name, &l.Version, &l.Status, &l.Visibility, &l.GroupID, &l.Description,
			&l.AuthorID, &l.AuthorName, &l.Protected, &l.Digest, &l.RejectReason, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan skill: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// semverLess orders x.y.z versions numerically.
func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

// Mine is the member's skill shelf: per name, the newest version they may
// use, whether it is required for them and whether they enabled it; plus
// their own versions still in review or rejected, so authors see the state
// of what they shared.
func (s *Store) Mine(ctx context.Context, workspaceID string, viewer Viewer) ([]Listed, error) {
	rows, err := s.pool.Query(ctx, listSelect+`
		WHERE p.workspace_id = $1 AND p.status NOT IN ('revoked')`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	all, err := scanListed(rows)
	if err != nil {
		return nil, err
	}
	required, err := s.requiredFor(ctx, workspaceID, viewer)
	if err != nil {
		return nil, err
	}
	enabled, err := s.enabledBy(ctx, workspaceID, viewer.UserID)
	if err != nil {
		return nil, err
	}
	newest := map[string]Listed{}
	var own []Listed
	for _, l := range all {
		v := l.version()
		if MaySee(viewer, v) {
			if cur, ok := newest[l.Name]; !ok || semverLess(cur.Version, l.Version) {
				newest[l.Name] = l
			}
		} else if l.AuthorID == viewer.UserID && l.Status != skillreg.StatusDeprecated && l.Status != skillreg.StatusReleased {
			own = append(own, l)
		}
	}
	out := []Listed{}
	for name, l := range newest {
		l.RequiredScope, l.Required = required[name], required[name] != ""
		l.Enabled = enabled[name]
		l.Active = l.Required || l.Enabled || l.Visibility == VisibilityPersonal
		l.HasResources, err = s.hasResources(ctx, workspaceID, l.Name, l.Version)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	out = append(out, own...)
	slices.SortFunc(out, func(a, b Listed) int {
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return strings.Compare(b.Version, a.Version)
	})
	return out, nil
}

func (s *Store) hasResources(ctx context.Context, workspaceID, name, version string) (bool, error) {
	var payload []byte
	var protected bool
	if err := s.pool.QueryRow(ctx, `SELECT payload, protected_body IS NOT NULL FROM skill_packages
		WHERE workspace_id = $1 AND name = $2 AND version = $3`, workspaceID, name, version).Scan(&payload, &protected); err != nil {
		return false, err
	}
	files, err := skillreg.ExtractBounded(payload)
	if err != nil {
		return false, err
	}
	for path := range files {
		if path != MarkerFile && (path != SkillFile || protected) {
			return true, nil
		}
	}
	return false, nil
}

// requiredFor maps skill name → "workspace" or "team" for the viewer.
func (s *Store) requiredFor(ctx context.Context, workspaceID string, viewer Viewer) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, group_id IS NULL FROM skill_requirements
		WHERE workspace_id = $1 AND (group_id IS NULL OR group_id = $2)`, workspaceID, viewer.Grants.GroupID)
	if err != nil {
		return nil, fmt.Errorf("skill requirements: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name string
		var whole bool
		if err := rows.Scan(&name, &whole); err != nil {
			return nil, err
		}
		if whole || out[name] == "" {
			out[name] = map[bool]string{true: rbac.ScopeWorkspace, false: rbac.ScopeTeam}[whole]
		}
	}
	return out, rows.Err()
}

func (s *Store) enabledBy(ctx context.Context, workspaceID, userID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM skill_enablements WHERE workspace_id = $1 AND user_id = $2`, workspaceID, userID)
	if err != nil {
		return nil, fmt.Errorf("skill enablements: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// newestVisible returns the newest version of name viewer may use.
func (s *Store) newestVisible(ctx context.Context, workspaceID string, viewer Viewer, name string) (Version, error) {
	mine, err := s.Mine(ctx, workspaceID, viewer)
	if err != nil {
		return Version{}, err
	}
	for _, l := range mine {
		if l.Name == name && MaySee(viewer, l.version()) {
			v, err := s.GetVersion(ctx, workspaceID, l.Name, l.Version)
			return v, err
		}
	}
	return Version{}, ErrNotFound
}

// SetEnabled turns a skill on or off for the member.
func (s *Store) SetEnabled(ctx context.Context, workspaceID string, viewer Viewer, name string, on bool, ev audit.Event) error {
	if on {
		if _, err := s.newestVisible(ctx, workspaceID, viewer, name); err != nil {
			return err
		}
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if on {
			_, err = tx.Exec(ctx, `INSERT INTO skill_enablements (workspace_id, user_id, name) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, workspaceID, viewer.UserID, name)
		} else {
			_, err = tx.Exec(ctx, `DELETE FROM skill_enablements WHERE workspace_id = $1 AND user_id = $2 AND name = $3`,
				workspaceID, viewer.UserID, name)
		}
		if err != nil {
			return fmt.Errorf("set enablement: %w", err)
		}
		ev.Resource = "skill/" + name
		ev.Detail = map[string]any{"enabled": on}
		return s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev})
	})
}

// SetRequired marks a skill required (or not) for the whole Workspace
// (groupID "") or one team. The audience must be able to see a released
// version: a team skill can only be required for its own team.
func (s *Store) SetRequired(ctx context.Context, workspaceID string, viewer Viewer, name, groupID string, on bool, ev audit.Event) error {
	if groupID == "" && !viewer.Grants.Has(rbac.SkillRequire) {
		return ErrForbidden
	}
	if groupID != "" && !viewer.Grants.HasIn(rbac.SkillRequire, groupID) {
		return ErrForbidden
	}
	if on {
		audience := Viewer{Grants: rbac.Grants{GroupID: groupID}}
		ok, err := s.hasReleasedFor(ctx, workspaceID, audience, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no released version of this skill reaches that audience", ErrInvalid)
		}
	}
	var group *string
	if groupID != "" {
		group = &groupID
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if on {
			_, err = tx.Exec(ctx, `INSERT INTO skill_requirements (workspace_id, name, group_id, set_by)
				VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, workspaceID, name, group, viewer.UserID)
		} else {
			_, err = tx.Exec(ctx, `DELETE FROM skill_requirements
				WHERE workspace_id = $1 AND name = $2 AND group_id IS NOT DISTINCT FROM $3`, workspaceID, name, group)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return fmt.Errorf("%w: unknown team", ErrInvalid)
		}
		if err != nil {
			return fmt.Errorf("set requirement: %w", err)
		}
		ev.Resource = "skill/" + name
		ev.Detail = map[string]any{"required": on, "group_id": groupID}
		return s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev})
	})
}

func (s *Store) hasReleasedFor(ctx context.Context, workspaceID string, audience Viewer, name string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM skill_packages
		WHERE workspace_id = $1 AND name = $2 AND status = 'released'
		  AND (visibility = 'organization' OR (visibility = 'team' AND group_id = $3))`,
		workspaceID, name, audience.Grants.GroupID).Scan(&n)
	return n > 0, err
}

// Loaded is one protected SKILL.md handed to the gateway for injection.
type Loaded struct {
	Name    string
	Version string
	Body    string
}

// Bodies returns the protected SKILL.md of each named skill that is active
// for the member (required for them or enabled by them) and visible to them.
// Names that do not qualify are reported in skipped, not as an error: a
// stale client list must not fail the model call.
func (s *Store) Bodies(ctx context.Context, workspaceID string, viewer Viewer, names []string) (loaded []Loaded, skipped []string, err error) {
	mine, err := s.Mine(ctx, workspaceID, viewer)
	if err != nil {
		return nil, nil, err
	}
	active := map[string]Listed{}
	for _, l := range mine {
		if l.Active && l.Protected && MaySee(viewer, l.version()) {
			active[l.Name] = l
		}
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		l, ok := active[name]
		if !ok {
			skipped = append(skipped, name)
			continue
		}
		var body string
		if err := s.pool.QueryRow(ctx, `SELECT protected_body FROM skill_packages
			WHERE workspace_id = $1 AND name = $2 AND version = $3`, workspaceID, l.Name, l.Version).Scan(&body); err != nil {
			return nil, nil, fmt.Errorf("load skill body: %w", err)
		}
		loaded = append(loaded, Loaded{Name: l.Name, Version: l.Version, Body: body})
	}
	return loaded, skipped, nil
}

func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
