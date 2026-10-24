package content

// Wiki reads over content resources: page lookup by path, the readable pages
// of the tree, keyword search, and a revision's links and source chain.
//
// Pitfalls:
//   - Search and tree listing use the same candidate filter as List and then
//     re-check Allowed; a snippet is cut from the published revision's plain
//     text only, so drafts never leak through search.
//   - Keyword search uses ILIKE over title, summary and body text (score_kind
//     keyword); vector retrieval and RRF arrive with the query step.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"enterprise-agent-platform/packages/wiki"
)

const (
	maxSearchResults = 20
	snippetRunes     = 120
	treePageLimit    = 2000
)

// PageEntry is a readable published page. An empty NodeID means unplaced.
type PageEntry struct {
	ID     string `json:"id"`
	NodeID string `json:"node_id"`
	Slug   string `json:"slug"`
	Path   string `json:"path"`
	Title  string `json:"title"`
}

// SearchHit is one authorized search result.
type SearchHit struct {
	PageID    string  `json:"page_id"`
	Path      string  `json:"path"`
	Title     string  `json:"title"`
	Summary   string  `json:"summary"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
	ScoreKind string  `json:"score_kind"`
}

// publishedReadable is the candidate filter shared with List.
const publishedReadable = ` AND v.revision = r.published_revision
	WHERE r.workspace_id = $1 AND r.published_revision IS NOT NULL
	  AND (r.visibility = 'organization' OR (r.visibility = 'team' AND r.group_id = $2) OR $3)`

// ResolvePath returns the id of the page at shelf/book/chapter/page, or of
// the page that last moved away from that path (wiki_path_aliases).
func (s *Store) ResolvePath(ctx context.Context, workspaceID, path string) (string, error) {
	path = wiki.NormalizePath(path)
	if !wiki.ValidPath(path) {
		return "", ErrNotFound
	}
	seg := strings.Split(path, "/")
	var id string
	err := s.pool.QueryRow(ctx, `SELECT r.id FROM content_resources r
		JOIN wiki_nodes ch ON ch.id = r.node_id JOIN wiki_nodes bk ON bk.id = ch.parent_id JOIN wiki_nodes sh ON sh.id = bk.parent_id
		WHERE r.workspace_id = $1 AND sh.slug = $2 AND bk.slug = $3 AND ch.slug = $4 AND r.slug = $5`,
		workspaceID, seg[0], seg[1], seg[2], seg[3]).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// No page has this path now: follow the alias left by a move or rename.
		err = s.pool.QueryRow(ctx, `SELECT resource_id FROM wiki_path_aliases WHERE workspace_id = $1 AND path = $2`,
			workspaceID, path).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// TreePages lists published wiki pages the actor may read, including unplaced
// pages created by the curator before a chapter has been assigned.
func (s *Store) TreePages(ctx context.Context, workspaceID string, a Actor) ([]PageEntry, error) {
	rows, err := s.pool.Query(ctx, resourceSelect+publishedReadable+`
		AND r.kind = 'wiki' ORDER BY v.title LIMIT $4`,
		workspaceID, a.GroupID, a.broad(), treePageLimit)
	if err != nil {
		return nil, fmt.Errorf("tree pages: %w", err)
	}
	items, err := collect(rows, func(r Resource) bool { return Allowed(a, Read, r.publishedFacts()) })
	if err != nil {
		return nil, err
	}
	out := make([]PageEntry, 0, len(items))
	for _, r := range items {
		out = append(out, PageEntry{ID: r.ID, NodeID: r.NodeID, Slug: r.Slug, Path: r.Path, Title: r.Title})
	}
	return out, nil
}

func snippet(text, query string) string {
	runes := []rune(text)
	idx := strings.Index(strings.ToLower(text), strings.ToLower(query))
	start := 0
	if idx >= 0 {
		start = utf8.RuneCountInString(text[:idx]) - snippetRunes/3
		if start < 0 {
			start = 0
		}
	}
	end := start + snippetRunes
	if end > len(runes) {
		end = len(runes)
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

// Search finds readable published wiki pages by keyword.
func (s *Store) Search(ctx context.Context, workspaceID string, a Actor, query string, limit int) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > maxQueryBytes*2 {
		return nil, ErrInvalid
	}
	if limit < 1 || limit > maxSearchResults {
		limit = maxSearchResults
	}
	rows, err := s.pool.Query(ctx, `SELECT q.*, v2.body_text FROM (`+resourceSelect+publishedReadable+`
		AND r.kind = 'wiki'
		AND (v.title ILIKE '%' || $4 || '%' OR v.summary ILIKE '%' || $4 || '%' OR v.body_text ILIKE '%' || $4 || '%')
		ORDER BY (CASE WHEN v.title ILIKE '%' || $4 || '%' THEN 3 ELSE 0 END
		        + CASE WHEN v.summary ILIKE '%' || $4 || '%' THEN 2 ELSE 0 END
		        + CASE WHEN v.body_text ILIKE '%' || $4 || '%' THEN 1 ELSE 0 END) DESC, r.updated_at DESC
		LIMIT $5) q JOIN content_revisions v2 ON v2.resource_id = q.id AND v2.revision = q.revision`,
		workspaceID, a.GroupID, a.broad(), query, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	lower := strings.ToLower(query)
	out := []SearchHit{}
	for rows.Next() {
		var r Resource
		var text string
		if err := rows.Scan(&r.ID, &r.Kind, &r.OwnerID, &r.Visibility, &r.GroupID, &r.PublishedRevision,
			&r.Revision, &r.Status, &r.Title, &r.Summary, &r.Body, &r.Details,
			&r.NodeID, &r.Slug, &r.Path, &r.Tags, &r.UpdatedAt, &text); err != nil {
			return nil, fmt.Errorf("scan hit: %w", err)
		}
		if !Allowed(a, Read, r.publishedFacts()) {
			continue
		}
		score := 0.0
		for weight, field := range map[float64]string{3: r.Title, 2: r.Summary, 1: text} {
			if strings.Contains(strings.ToLower(field), lower) {
				score += weight
			}
		}
		out = append(out, SearchHit{PageID: r.ID, Path: r.Path, Title: r.Title, Summary: r.Summary,
			Snippet: snippet(text, query), Score: score, ScoreKind: "keyword"})
	}
	return out, rows.Err()
}

// RevisionLinks returns the internal link targets of one revision.
func (s *Store) RevisionLinks(ctx context.Context, resourceID string, revision int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT target_path FROM content_revision_links WHERE resource_id = $1 AND revision = $2 ORDER BY target_path`,
		resourceID, revision)
	if err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RevisionSources returns the source chain of one revision.
func (s *Store) RevisionSources(ctx context.Context, resourceID string, revision int) ([]SourceRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT contribution_id, contribution_version, anchor FROM content_revision_sources
		WHERE resource_id = $1 AND revision = $2 ORDER BY contribution_id, contribution_version, anchor`, resourceID, revision)
	if err != nil {
		return nil, fmt.Errorf("sources: %w", err)
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

// Attachment is one file a page revision can offer its readers.
type Attachment struct {
	SHA256    string `json:"sha256"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

// RevisionAttachments lists the attachments of the contribution versions a
// revision cites, skipping withdrawn contributions and blocked blobs: a
// page's readers may open exactly these, and nothing else in the blob store.
func (s *Store) RevisionAttachments(ctx context.Context, workspaceID, resourceID string, revision int) ([]Attachment, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (a->>'sha256') a->>'sha256', a->>'name', b.media_type, b.size
		FROM content_revision_sources src
		JOIN contributions c ON c.id = src.contribution_id AND c.workspace_id = $1 AND c.status <> 'withdrawn'
		JOIN contribution_versions cv ON cv.contribution_id = src.contribution_id AND cv.version = src.contribution_version
		CROSS JOIN LATERAL jsonb_array_elements(cv.attachments) a
		JOIN blobs b ON b.workspace_id = $1 AND b.sha256 = a->>'sha256' AND b.scan_status <> 'blocked'
		WHERE src.resource_id = $2 AND src.revision = $3
		ORDER BY a->>'sha256', a->>'name'`, workspaceID, resourceID, revision)
	if err != nil {
		return nil, fmt.Errorf("attachments: %w", err)
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.SHA256, &a.Name, &a.MediaType, &a.Size); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
