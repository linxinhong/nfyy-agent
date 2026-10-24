package curation

// Job leasing, proposal application and human decisions (migration 000027).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/wiki"
)

const (
	maxAttempts    = 5
	curatorSubject = "svc:mindlink-curator"
)

var (
	ErrNotFound  = errors.New("proposal not found")
	ErrForbidden = errors.New("proposal decision not permitted")
	ErrConflict  = errors.New("proposal state does not allow this decision")
	ErrInvalid   = errors.New("invalid decision")
)

// TxAudit writes audit events inside a caller-owned transaction.
type TxAudit interface {
	WriteBatchTx(ctx context.Context, tx pgx.Tx, events []audit.Event) error
}

// Job is one leased unit of curation work.
type Job struct {
	ID                  int64
	WorkspaceID         string
	Kind                string
	ContributionID      string
	ContributionVersion int
	Attempts            int
}

// Contribution is the version a job curates.
type Contribution struct {
	ID       string
	Version  int
	AuthorID string
	Status   string
	Scope    string
	GroupID  string
	Title    string
	Summary  string
	Markdown string
	Details  map[string]any
}

// Proposal is a stored proposal as reviewers see it.
type Proposal struct {
	ID              string          `json:"id"`
	Kind            string          `json:"kind"`
	Risk            string          `json:"risk"`
	Status          string          `json:"status"`
	Target          map[string]any  `json:"target"`
	Payload         json.RawMessage `json:"payload"`
	Sources         []Source        `json:"sources"`
	Rationale       string          `json:"rationale"`
	AppliedRevision *int            `json:"applied_revision,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	DecidedAt       *time.Time      `json:"decided_at,omitempty"`
	DecidedBy       string          `json:"decided_by,omitempty"`
}

// Store persists jobs and proposals and applies them through content.
type Store struct {
	pool    *pgxpool.Pool
	audit   TxAudit
	content *content.Store
}

func New(pool *pgxpool.Pool, txAudit TxAudit, contentStore *content.Store) *Store {
	return &Store{pool: pool, audit: txAudit, content: contentStore}
}

// EnsureCurator returns the curator service user of the Workspace's
// organization. It has no credentials and no Workspace roles: it can author
// drafts and propose revisions, never review or publish.
func (s *Store) EnsureCurator(ctx context.Context, workspaceID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (idp_subject, email, display_name, organization_id, department_id)
		SELECT $1, '', '整理 Agent', organization_id, 'dept_platform' FROM workspaces WHERE id = $2
		ON CONFLICT (idp_subject) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id`, curatorSubject, workspaceID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ensure curator: %w", err)
	}
	return id, nil
}

// Lease claims the next ready job (queued, or running with an expired lease).
func (s *Store) Lease(ctx context.Context, owner string, lease time.Duration) (Job, bool, error) {
	var j Job
	err := s.pool.QueryRow(ctx, `
		UPDATE curation_jobs SET status = 'running', lease_owner = $1, lease_until = now() + $2::interval,
			attempts = attempts + 1, updated_at = now()
		WHERE id = (
			SELECT id FROM curation_jobs
			WHERE available_at <= now() AND (status = 'queued' OR (status = 'running' AND lease_until < now()))
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, workspace_id, kind, COALESCE(contribution_id, ''), COALESCE(contribution_version, 0), attempts`,
		owner, fmt.Sprintf("%d milliseconds", lease.Milliseconds())).
		Scan(&j.ID, &j.WorkspaceID, &j.Kind, &j.ContributionID, &j.ContributionVersion, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("lease job: %w", err)
	}
	return j, true, nil
}

// Complete marks a leased job done.
func (s *Store) Complete(ctx context.Context, j Job, owner string) error {
	_, err := s.pool.Exec(ctx, `UPDATE curation_jobs SET status = 'done', lease_owner = NULL, lease_until = NULL, last_error = NULL,
		updated_at = now() WHERE id = $1 AND lease_owner = $2`, j.ID, owner)
	return err
}

// Fail re-queues with exponential backoff, or gives up after maxAttempts.
func (s *Store) Fail(ctx context.Context, j Job, owner string, cause error) error {
	status := "queued"
	if j.Attempts >= maxAttempts {
		status = "failed"
	}
	backoff := time.Duration(1<<j.Attempts) * 30 * time.Second
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	_, err := s.pool.Exec(ctx, `UPDATE curation_jobs SET status = $3, lease_owner = NULL, lease_until = NULL, last_error = $4,
		available_at = now() + $5::interval, updated_at = now() WHERE id = $1 AND lease_owner = $2`,
		j.ID, owner, status, msg, fmt.Sprintf("%d milliseconds", backoff.Milliseconds()))
	return err
}

// LoadContribution reads the version a job curates.
func (s *Store) LoadContribution(ctx context.Context, j Job) (Contribution, error) {
	var c Contribution
	var details []byte
	err := s.pool.QueryRow(ctx, `SELECT c.id, v.version, c.author_id, c.status, v.scope, COALESCE(v.group_id, ''),
			v.title, v.summary, v.markdown, v.details
		FROM contributions c JOIN contribution_versions v ON v.contribution_id = c.id
		WHERE c.id = $1 AND v.version = $2 AND c.workspace_id = $3`, j.ContributionID, j.ContributionVersion, j.WorkspaceID).
		Scan(&c.ID, &c.Version, &c.AuthorID, &c.Status, &c.Scope, &c.GroupID, &c.Title, &c.Summary, &c.Markdown, &details)
	if err != nil {
		return Contribution{}, fmt.Errorf("load contribution: %w", err)
	}
	_ = json.Unmarshal(details, &c.Details)
	return c, nil
}

// PagesOnlyWithdrawn returns the published pages among ids whose published
// revision cites sources and every one of them has been withdrawn.
func (s *Store) PagesOnlyWithdrawn(ctx context.Context, workspaceID string, ids []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.id FROM content_resources r
		WHERE r.workspace_id = $1 AND r.id = ANY($2) AND r.published_revision IS NOT NULL
		  AND EXISTS (SELECT 1 FROM content_revision_sources src
		      WHERE src.resource_id = r.id AND src.revision = r.published_revision)
		  AND NOT EXISTS (SELECT 1 FROM content_revision_sources src JOIN contributions c ON c.id = src.contribution_id
		      WHERE src.resource_id = r.id AND src.revision = r.published_revision AND c.status <> 'withdrawn')`,
		workspaceID, ids)
	if err != nil {
		return nil, fmt.Errorf("withdrawn-only pages: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PagesCiting lists pages whose latest or published revision cites the contribution.
func (s *Store) PagesCiting(ctx context.Context, workspaceID, contributionID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT r.id FROM content_resources r
		JOIN content_revision_sources src ON src.resource_id = r.id
		WHERE r.workspace_id = $1 AND src.contribution_id = $2
		  AND (src.revision = r.published_revision OR src.revision = (SELECT max(revision) FROM content_revisions WHERE resource_id = r.id))`,
		workspaceID, contributionID)
	if err != nil {
		return nil, fmt.Errorf("citing pages: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// EnqueueSweeps queues a sweep for every Workspace with at least two
// published wiki pages whose last sweep is older than every (or never ran).
// It returns how many were queued; concurrent callers queue each once.
func (s *Store) EnqueueSweeps(ctx context.Context, every time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO curation_jobs (workspace_id, kind)
		SELECT w.id, 'sweep' FROM workspaces w
		WHERE (SELECT count(*) FROM content_resources r
		       WHERE r.workspace_id = w.id AND r.kind = 'wiki' AND r.published_revision IS NOT NULL) >= 2
		  AND NOT EXISTS (SELECT 1 FROM curation_jobs j WHERE j.workspace_id = w.id AND j.kind = 'sweep'
		      AND (j.status IN ('queued', 'running') OR j.created_at > now() - $1::interval))
		ON CONFLICT DO NOTHING`, fmt.Sprintf("%d milliseconds", every.Milliseconds()))
	if err != nil {
		return 0, fmt.Errorf("enqueue sweeps: %w", err)
	}
	return tag.RowsAffected(), nil
}

// EnqueueSweep queues a sweep of one Workspace now; queued reports false
// when one is already open.
func (s *Store) EnqueueSweep(ctx context.Context, workspaceID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO curation_jobs (workspace_id, kind) VALUES ($1, 'sweep') ON CONFLICT DO NOTHING`, workspaceID)
	if err != nil {
		return false, fmt.Errorf("enqueue sweep: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SweepPage is one published page as a sweep sees it.
type SweepPage struct {
	ID         string
	Path       string
	Title      string
	Summary    string
	Body       string
	Tags       []string
	Visibility string
	GroupID    string
	Revision   int
	Related    []string // pages already related to this one
	Duplicate  bool     // an open duplicate flag exists
}

// SweepPages returns up to limit published wiki pages, most recently
// updated first. The sweep reads across teams; what it may change is
// decided per proposal from the pages' sources.
func (s *Store) SweepPages(ctx context.Context, workspaceID string, limit int) ([]SweepPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, COALESCE(sh.slug || '/' || bk.slug || '/' || ch.slug || '/' || r.slug, ''), v.title, v.summary, v.body,
		       COALESCE((SELECT array_agg(tag ORDER BY tag) FROM content_tags t WHERE t.resource_id = r.id), '{}'),
		       r.visibility, COALESCE(r.group_id, ''), r.published_revision,
		       COALESCE((SELECT array_agg(target_resource_id) FROM content_relations cr WHERE cr.resource_id = r.id), '{}'),
		       EXISTS (SELECT 1 FROM content_flags f WHERE f.resource_id = r.id AND f.kind = 'duplicate' AND f.cleared_at IS NULL)
		FROM content_resources r
		JOIN content_revisions v ON v.resource_id = r.id AND v.revision = r.published_revision
		LEFT JOIN wiki_nodes ch ON ch.id = r.node_id
		LEFT JOIN wiki_nodes bk ON bk.id = ch.parent_id
		LEFT JOIN wiki_nodes sh ON sh.id = bk.parent_id
		WHERE r.workspace_id = $1 AND r.kind = 'wiki' AND r.published_revision IS NOT NULL
		ORDER BY r.updated_at DESC, r.id LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("sweep pages: %w", err)
	}
	defer rows.Close()
	var out []SweepPage
	for rows.Next() {
		var p SweepPage
		if err := rows.Scan(&p.ID, &p.Path, &p.Title, &p.Summary, &p.Body, &p.Tags, &p.Visibility, &p.GroupID,
			&p.Revision, &p.Related, &p.Duplicate); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// publishedSources returns the source chain of a page's published revision.
func (s *Store) publishedSources(ctx context.Context, workspaceID, pageID string) ([]Source, error) {
	rows, err := s.pool.Query(ctx, `SELECT src.contribution_id, src.contribution_version, src.anchor
		FROM content_resources r JOIN content_revision_sources src ON src.resource_id = r.id AND src.revision = r.published_revision
		WHERE r.id = $1 AND r.workspace_id = $2`, pageID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("page sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.ContributionID, &src.Version, &src.Anchor); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

var errSweepSource = errors.New("a sweep may only cite contributions already cited by a page")

// sweepSources gives a sweep proposal the sources of the pages it draws on
// (target, and absorbed pages of a merge), so the scope checks see exactly
// what material the change moves. Sources the agent named itself must
// already be cited by some page: a sweep only rearranges curated material.
func (s *Store) sweepSources(ctx context.Context, j Job, p Proposed) ([]Source, error) {
	for _, src := range p.Sources {
		var cited bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM content_revision_sources src
			JOIN content_resources r ON r.id = src.resource_id AND r.workspace_id = $1
			WHERE src.contribution_id = $2 AND src.contribution_version = $3)`, j.WorkspaceID, src.ContributionID, src.Version).Scan(&cited); err != nil {
			return nil, err
		}
		if !cited {
			return nil, errSweepSource
		}
	}
	pages := []string{}
	if p.TargetPageID != "" {
		pages = append(pages, p.TargetPageID)
	}
	if p.Kind == KindMergePages {
		var mp MergePagesPayload
		if json.Unmarshal(p.Payload, &mp) == nil {
			pages = append(pages, mp.Absorbed...)
		}
	}
	out := append([]Source(nil), p.Sources...)
	seen := map[Source]bool{}
	for _, src := range out {
		seen[src] = true
	}
	for _, id := range pages {
		more, err := s.publishedSources(ctx, j.WorkspaceID, id)
		if err != nil {
			return nil, err
		}
		for _, src := range more {
			if !seen[src] {
				seen[src] = true
				out = append(out, src)
			}
		}
	}
	return out, nil
}

// scopeOf intersects the sources' scopes: (visibility, group, ok).
func (s *Store) scopeOf(ctx context.Context, workspaceID string, sources []Source) (string, string, bool, error) {
	visibility, group := content.VisibilityOrganization, ""
	for _, src := range sources {
		var scope, g string
		err := s.pool.QueryRow(ctx, `SELECT v.scope, COALESCE(v.group_id, '') FROM contributions c
			JOIN contribution_versions v ON v.contribution_id = c.id
			WHERE c.id = $1 AND v.version = $2 AND c.workspace_id = $3`, src.ContributionID, src.Version, workspaceID).Scan(&scope, &g)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, nil
		}
		if err != nil {
			return "", "", false, fmt.Errorf("source scope: %w", err)
		}
		switch scope {
		case "personal":
			return "", "", false, nil
		case "team":
			if group != "" && group != g {
				return "", "", false, nil
			}
			visibility, group = content.VisibilityTeam, g
		}
	}
	return visibility, group, true, nil
}

// fits reports whether content of (visibility, group) may enter a page with the target's visibility.
func fits(visibility, group string, target content.Resource) bool {
	if visibility == content.VisibilityOrganization {
		return true
	}
	return target.Visibility == content.VisibilityTeam && target.GroupID == group
}

func toContentSources(sources []Source) []content.SourceRef {
	out := make([]content.SourceRef, 0, len(sources))
	for _, s := range sources {
		out = append(out, content.SourceRef{ContributionID: s.ContributionID, Version: s.Version, Anchor: s.Anchor})
	}
	return out
}

// record writes the proposal row and its audit event.
func (s *Store) record(ctx context.Context, tx pgx.Tx, id string, j Job, p Proposed, status, target, targetPath string, applied *int, revert any, ev audit.Event) error {
	var revertJSON []byte
	if revert != nil {
		revertJSON, _ = json.Marshal(revert)
	}
	sources, _ := json.Marshal(p.Sources)
	payload := p.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	var targetID *string
	if target != "" {
		targetID = &target
	}
	if _, err := tx.Exec(ctx, `INSERT INTO curation_proposals (id, workspace_id, job_id, kind, risk, status, target_resource_id, target_path,
			payload, sources, rationale, applied_revision, revert)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		id, j.WorkspaceID, j.ID, p.Kind, Risk(p.Kind), status, targetID, targetPath, payload, sources,
		strings.TrimSpace(p.Rationale), applied, revertJSON); err != nil {
		return fmt.Errorf("record proposal: %w", err)
	}
	ev = fresh(ev)
	ev.Type = audit.EventType("curation.proposal_" + status)
	ev.Resource = "curation-proposal/" + id
	ev.Detail = map[string]any{"kind": p.Kind, "target": target, "job": j.ID}
	return s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev})
}

// fresh gives one more audit write its own event id: a job or decision
// writes several audited rows, and event ids are primary keys.
func fresh(ev audit.Event) audit.Event {
	n := audit.NewEvent(ev.Type, ev.Outcome)
	ev.ID, ev.Time = n.ID, n.Time
	return ev
}

// Submit validates and applies a job's proposals. Proposals the server cannot
// accept are recorded as rejected with the reason appended to the rationale.
func (s *Store) Submit(ctx context.Context, j Job, curatorID string, proposals []Proposed, ev audit.Event) error {
	type item struct {
		p       Proposed
		invalid error
	}
	var flat []item
	for _, p := range proposals {
		if j.Kind == "sweep" {
			sources, err := s.sweepSources(ctx, j, p)
			if err != nil {
				if errors.Is(err, errSweepSource) {
					flat = append(flat, item{p: p, invalid: err})
					continue
				}
				return err
			}
			p.Sources = sources
		}
		pieces, err := expand(p)
		if err != nil {
			flat = append(flat, item{p: p, invalid: err})
			continue
		}
		for _, piece := range pieces {
			flat = append(flat, item{p: piece})
		}
	}
	for i, it := range flat {
		p := it.p
		id := fmt.Sprintf("prp_%d_%d", j.ID, i)
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM curation_proposals WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("proposal lookup: %w", err)
		}
		if exists {
			continue // retried job: already applied
		}
		var err error
		if it.invalid != nil {
			err = s.reject(ctx, j, id, p, it.invalid.Error(), ev)
		} else {
			err = s.apply(ctx, j, curatorID, id, p, ev)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) reject(ctx context.Context, j Job, id string, p Proposed, reason string, ev audit.Event) error {
	p.Rationale = strings.TrimSpace(p.Rationale) + " [server: " + reason + "]"
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.record(ctx, tx, id, j, p, "rejected", "", "", nil, nil, ev); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) apply(ctx context.Context, j Job, curatorID, id string, p Proposed, ev audit.Event) error {
	if p.Kind == KindWidenVisibility {
		return s.reject(ctx, j, id, p, "agents may not widen visibility; an admin changes it on the page", ev)
	}
	if !supported[p.Kind] {
		return s.reject(ctx, j, id, p, "unknown kind", ev)
	}
	if !validRationale(p.Rationale) {
		p.Rationale = "(no rationale)"
	}
	for _, src := range p.Sources {
		if j.Kind != "sweep" && src.ContributionID != j.ContributionID {
			return s.reject(ctx, j, id, p, "sources must be the contribution this job is about", ev)
		}
	}
	visibility, group, ok, err := s.scopeOf(ctx, j.WorkspaceID, p.Sources)
	if err != nil {
		return err
	}
	if !ok {
		return s.reject(ctx, j, id, p, "sources are personal, missing, or from different teams", ev)
	}
	actor := content.Actor{UserID: curatorID, GroupID: group, CiteContributions: true}
	if Risk(p.Kind) == "review" && len(p.Sources) == 0 {
		return s.reject(ctx, j, id, p, "review proposals must cite their sources", ev)
	}

	var target content.Resource
	if p.TargetPageID != "" {
		if target, err = s.content.Get(ctx, j.WorkspaceID, actor, p.TargetPageID); err != nil {
			return s.reject(ctx, j, id, p, "target page not readable in the sources' scope", ev)
		}
		if !fits(visibility, group, target) {
			return s.reject(ctx, j, id, p, "content would reach a wider audience than its sources", ev)
		}
	} else if p.Kind != KindCreatePage {
		return s.reject(ctx, j, id, p, "target_page_id is required", ev)
	}

	if Risk(p.Kind) == "review" {
		var page PagePayload
		if json.Unmarshal(p.Payload, &page) != nil {
			return s.reject(ctx, j, id, p, "invalid page payload", ev)
		}
		in := content.Input{Kind: "wiki", Visibility: visibility, GroupID: group, Title: page.Title, Summary: page.Summary,
			Body: page.Body, NodeID: page.NodeID, Slug: page.Slug, Tags: page.Tags, BaseRevision: page.BaseRevision,
			Sources: toContentSources(p.Sources)}
		var made content.Resource
		if p.Kind == KindCreatePage {
			made, err = s.content.Create(ctx, j.WorkspaceID, actor, in, fresh(ev))
			if err == nil {
				made, err = s.content.Transition(ctx, j.WorkspaceID, actor, made.ID, "submit", fresh(ev))
			}
		} else {
			made, err = s.content.ProposeRevision(ctx, j.WorkspaceID, actor, target.ID, in, fresh(ev))
		}
		if errors.Is(err, content.ErrInvalid) || errors.Is(err, content.ErrConflict) || errors.Is(err, content.ErrForbidden) {
			return s.reject(ctx, j, id, p, "content refused the change: "+err.Error(), ev)
		}
		if err != nil {
			return err
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		revision := made.Revision
		if err := s.record(ctx, tx, id, j, p, "pending", made.ID, made.Path, &revision, nil, ev); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	return s.applyAuto(ctx, j, id, p, target, ev)
}

// applyAuto performs an auto proposal and records how to undo it.
func (s *Store) applyAuto(ctx context.Context, j Job, id string, p Proposed, target content.Resource, ev audit.Event) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var revert map[string]any
	switch p.Kind {
	case KindTag:
		var tp TagPayload
		if json.Unmarshal(p.Payload, &tp) != nil || len(tp.Tags) == 0 || len(tp.Tags)+len(target.Tags) > 20 {
			return s.reject(ctx, j, id, p, "invalid tags", ev)
		}
		added := []string{}
		for _, tag := range tp.Tags {
			tag = strings.TrimSpace(tag)
			if tag == "" || len([]rune(tag)) > 40 {
				return s.reject(ctx, j, id, p, "invalid tag", ev)
			}
			tag2, err := tx.Exec(ctx, `INSERT INTO content_tags (resource_id, tag) VALUES ($1, $2) ON CONFLICT DO NOTHING`, target.ID, tag)
			if err != nil {
				return fmt.Errorf("add tag: %w", err)
			}
			if tag2.RowsAffected() == 1 {
				added = append(added, tag)
			}
		}
		revert = map[string]any{"remove_tags": added}
	case KindLink:
		var lp LinkPayload
		if json.Unmarshal(p.Payload, &lp) != nil || lp.RelatedPageID == "" || lp.RelatedPageID == target.ID {
			return s.reject(ctx, j, id, p, "invalid related page", ev)
		}
		var sameWorkspace bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM content_resources WHERE id = $1 AND workspace_id = $2)`,
			lp.RelatedPageID, j.WorkspaceID).Scan(&sameWorkspace); err != nil || !sameWorkspace {
			return s.reject(ctx, j, id, p, "related page not found", ev)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO content_relations (resource_id, target_resource_id, proposal_id) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, target.ID, lp.RelatedPageID, nil); err != nil {
			return fmt.Errorf("relate: %w", err)
		}
		revert = map[string]any{"remove_relation": lp.RelatedPageID}
	case KindClassifyDraft:
		var cp ClassifyPayload
		if json.Unmarshal(p.Payload, &cp) != nil || target.PublishedRevision != nil {
			return s.reject(ctx, j, id, p, "only unpublished pages can be classified automatically", ev)
		}
		cp.Slug = wiki.NormalizeSlug(cp.Slug)
		var level string
		err := tx.QueryRow(ctx, `SELECT level FROM wiki_nodes WHERE id = $1 AND workspace_id = $2`, cp.NodeID, j.WorkspaceID).Scan(&level)
		if err != nil || level != wiki.LevelChapter || !wiki.ValidSlug(cp.Slug) {
			return s.reject(ctx, j, id, p, "classification needs a chapter and a valid slug", ev)
		}
		if _, err := tx.Exec(ctx, `UPDATE content_resources SET node_id = $2, slug = $3 WHERE id = $1`, target.ID, cp.NodeID, cp.Slug); err != nil {
			return s.reject(ctx, j, id, p, "slug already used in that chapter", ev)
		}
		revert = map[string]any{"node_id": target.NodeID, "slug": target.Slug}
	case KindRestrictVisibility:
		if target.PublishedRevision == nil {
			return s.reject(ctx, j, id, p, "page is not published", ev)
		}
		if _, err := tx.Exec(ctx, `UPDATE content_resources SET published_revision = NULL, updated_at = now() WHERE id = $1`, target.ID); err != nil {
			return fmt.Errorf("restrict: %w", err)
		}
		revert = map[string]any{"published_revision": *target.PublishedRevision}
	default: // flags
		var fp FlagPayload
		_ = json.Unmarshal(p.Payload, &fp)
		detail, _ := json.Marshal(map[string]any{"reason": fp.Reason})
		var flagID int64
		if err := tx.QueryRow(ctx, `INSERT INTO content_flags (resource_id, kind, detail) VALUES ($1, $2, $3) RETURNING id`,
			target.ID, flagKinds[p.Kind], detail).Scan(&flagID); err != nil {
			return fmt.Errorf("flag: %w", err)
		}
		revert = map[string]any{"clear_flag": flagID}
	}
	if err := s.record(ctx, tx, id, j, p, "auto_applied", target.ID, target.Path, nil, revert, ev); err != nil {
		return err
	}
	if flagID, ok := revert["clear_flag"]; ok {
		if _, err := tx.Exec(ctx, `UPDATE content_flags SET proposal_id = $1 WHERE id = $2`, id, flagID); err != nil {
			return fmt.Errorf("link flag: %w", err)
		}
	}
	if p.Kind == KindLink {
		if _, err := tx.Exec(ctx, `UPDATE content_relations SET proposal_id = $1 WHERE resource_id = $2 AND target_resource_id = $3 AND proposal_id IS NULL`,
			id, target.ID, revert["remove_relation"]); err != nil {
			return fmt.Errorf("link relation: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// List returns proposals the actor may decide, newest first. A review
// proposal's status follows its revision once reviewed in the queue.
func (s *Store) List(ctx context.Context, workspaceID string, a content.Actor, status string) ([]Proposal, error) {
	if !a.CanReviewAny() {
		return nil, ErrForbidden
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id, p.kind, p.risk,
			CASE WHEN p.status = 'pending' AND v.status = 'approved' THEN 'accepted'
			     WHEN p.status = 'pending' AND v.status = 'rejected' THEN 'rejected' ELSE p.status END,
			COALESCE(p.target_resource_id, ''), p.target_path, p.payload, p.sources, p.rationale, p.applied_revision,
			p.created_at, p.decided_at, COALESCE(p.decided_by, '')
		FROM curation_proposals p
		LEFT JOIN content_revisions v ON v.resource_id = p.target_resource_id AND v.revision = p.applied_revision
		WHERE p.workspace_id = $1
		ORDER BY p.created_at DESC LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		var p Proposal
		var target, path string
		var sources []byte
		if err := rows.Scan(&p.ID, &p.Kind, &p.Risk, &p.Status, &target, &path, &p.Payload, &sources, &p.Rationale,
			&p.AppliedRevision, &p.CreatedAt, &p.DecidedAt, &p.DecidedBy); err != nil {
			return nil, fmt.Errorf("scan proposal: %w", err)
		}
		if status != "" && p.Status != status {
			continue
		}
		_ = json.Unmarshal(sources, &p.Sources)
		p.Target = map[string]any{"page_id": target, "path": path}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Leads only see proposals on pages they may review.
	visible := out[:0]
	for _, p := range out {
		if s.mayDecide(ctx, workspaceID, a, p.Target["page_id"].(string)) {
			visible = append(visible, p)
		}
	}
	return visible, nil
}

func (s *Store) mayDecide(ctx context.Context, workspaceID string, a content.Actor, pageID string) bool {
	if pageID == "" {
		return a.Grants.Has(rbac.KnowledgeReview)
	}
	r, err := s.content.Get(ctx, workspaceID, a, pageID)
	if err != nil {
		return false
	}
	return content.Allowed(a, content.Review, content.Facts{Visibility: r.Visibility, GroupID: r.GroupID})
}

// Decide accepts, rejects or reverts one proposal.
func (s *Store) Decide(ctx context.Context, workspaceID string, a content.Actor, id, decision string, ev audit.Event) (Proposal, error) {
	if decision != "accept" && decision != "reject" && decision != "revert" {
		return Proposal{}, ErrInvalid
	}
	var kind, status, target string
	var applied *int
	var revertJSON []byte
	err := s.pool.QueryRow(ctx, `SELECT kind, status, COALESCE(target_resource_id, ''), applied_revision, revert
		FROM curation_proposals WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&kind, &status, &target, &applied, &revertJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("load proposal: %w", err)
	}
	if !s.mayDecide(ctx, workspaceID, a, target) {
		return Proposal{}, ErrNotFound
	}
	next := ""
	switch {
	case status == "pending" && decision != "revert":
		action := map[string]string{"accept": "approve", "reject": "reject"}[decision]
		if _, err := s.content.Transition(ctx, workspaceID, a, target, action, fresh(ev)); err != nil {
			if errors.Is(err, content.ErrForbidden) || errors.Is(err, content.ErrNotFound) {
				return Proposal{}, ErrForbidden
			}
			return Proposal{}, ErrConflict
		}
		next = map[string]string{"accept": "accepted", "reject": "rejected"}[decision]
	case status == "auto_applied" && decision == "accept":
		next = "accepted"
	case status == "auto_applied" && (decision == "revert" || decision == "reject"):
		if err := s.undo(ctx, kind, target, revertJSON); err != nil {
			return Proposal{}, err
		}
		next = "reverted"
	default:
		return Proposal{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE curation_proposals SET status = $2, decided_at = now(), decided_by = $3 WHERE id = $1`,
		id, next, a.UserID); err != nil {
		return Proposal{}, fmt.Errorf("decide: %w", err)
	}
	ev = fresh(ev)
	ev.Type = audit.EventType("curation.proposal_" + next)
	ev.Resource = "curation-proposal/" + id
	ev.Detail = map[string]any{"kind": kind, "decision": decision}
	if err := s.audit.WriteBatchTx(ctx, tx, []audit.Event{ev}); err != nil {
		return Proposal{}, fmt.Errorf("decision audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Proposal{}, err
	}
	for _, p := range mustList(s.List(ctx, workspaceID, a, "")) {
		if p.ID == id {
			return p, nil
		}
	}
	return Proposal{ID: id, Kind: kind, Status: next}, nil
}

func mustList(items []Proposal, err error) []Proposal {
	if err != nil {
		return nil
	}
	return items
}

// undo reverses an auto proposal from its stored revert state.
func (s *Store) undo(ctx context.Context, kind, target string, revertJSON []byte) error {
	var revert map[string]any
	_ = json.Unmarshal(revertJSON, &revert)
	var err error
	switch kind {
	case KindTag:
		tags, _ := revert["remove_tags"].([]any)
		for _, t := range tags {
			if _, err = s.pool.Exec(ctx, `DELETE FROM content_tags WHERE resource_id = $1 AND tag = $2`, target, t); err != nil {
				break
			}
		}
	case KindLink:
		_, err = s.pool.Exec(ctx, `DELETE FROM content_relations WHERE resource_id = $1 AND target_resource_id = $2`, target, revert["remove_relation"])
	case KindClassifyDraft:
		node, _ := revert["node_id"].(string)
		slug, _ := revert["slug"].(string)
		var n, sl *string
		if node != "" {
			n, sl = &node, &slug
		}
		_, err = s.pool.Exec(ctx, `UPDATE content_resources SET node_id = $2, slug = $3 WHERE id = $1`, target, n, sl)
	case KindRestrictVisibility:
		_, err = s.pool.Exec(ctx, `UPDATE content_resources SET published_revision = $2, updated_at = now() WHERE id = $1 AND published_revision IS NULL`,
			target, revert["published_revision"])
	default:
		_, err = s.pool.Exec(ctx, `UPDATE content_flags SET cleared_at = now() WHERE id = $1`, revert["clear_flag"])
	}
	if err != nil {
		return fmt.Errorf("revert: %w", err)
	}
	return nil
}
