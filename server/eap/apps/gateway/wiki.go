package main

// Wiki API (/enterprise/v1/wiki): tree navigation, page reading by id or
// path, keyword search, and admin management of the shelf/book/chapter tree.
//
// Why: pages are content resources, so every read goes through the same
// authorization (content.Allowed) as /resources; this layer adds the tree,
// server-side Markdown rendering and link/source resolution.
//
// Pitfalls:
//   - HTML is rendered and sanitized on every read (wiki.Render); never
//     return stored or client-supplied HTML.
//   - A link target appears in `links` only if the reader may open it; the
//     Markdown itself is the author's text and is returned as written.
//   - Node titles are taxonomy and visible to every member; case-level
//     restrictions arrive with matters and walls, not here.

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/wiki"
)

// pageLink is a resolved internal link the reader may follow.
type pageLink struct {
	PageID string `json:"page_id"`
	Path   string `json:"path"`
	Title  string `json:"title"`
}

// wikiPage is the reader's view of one page revision.
type wikiPage struct {
	content.Resource
	Markdown    string               `json:"markdown"`
	HTML        string               `json:"html"`
	Links       []pageLink           `json:"links"`
	Sources     []content.SourceRef  `json:"sources"`
	Attachments []content.Attachment `json:"attachments"`
}

func wikiProblem(err error) error {
	switch {
	case errors.Is(err, wiki.ErrInvalid):
		return errs.New(errs.InvalidArgument, "invalid node: level, parent, slug (1-80 letters, digits, - _ .) or title (1-120)")
	case errors.Is(err, wiki.ErrNotFound):
		return errs.New(errs.NotFound, "wiki node not found")
	case errors.Is(err, wiki.ErrConflict):
		return errs.New(errs.Conflict, "a sibling already uses this slug")
	case errors.Is(err, wiki.ErrInUse):
		return errs.New(errs.Conflict, "the node still has children or pages")
	default:
		return errs.Wrap(err, errs.Internal, "wiki storage failed")
	}
}

// buildPage renders one readable wiki resource for the actor.
func (s *Server) buildPage(r *http.Request, c contentCaller, item content.Resource) (wikiPage, error) {
	rendered, err := wiki.Render(item.Body)
	if err != nil {
		return wikiPage{}, errs.Wrap(err, errs.Internal, "render failed")
	}
	targets, err := s.Content.RevisionLinks(r.Context(), item.ID, item.Revision)
	if err != nil {
		return wikiPage{}, contentProblem(err)
	}
	links := []pageLink{}
	for _, target := range targets {
		id, err := s.Content.ResolvePath(r.Context(), c.session.Workspace.WorkspaceID, target)
		if err != nil {
			continue
		}
		linked, err := s.Content.Get(r.Context(), c.session.Workspace.WorkspaceID, c.actor, id)
		if err != nil {
			continue // missing or not readable: never named to this reader
		}
		links = append(links, pageLink{PageID: linked.ID, Path: linked.Path, Title: linked.Title})
	}
	sources, err := s.Content.RevisionSources(r.Context(), item.ID, item.Revision)
	if err != nil {
		return wikiPage{}, contentProblem(err)
	}
	attachments, err := s.Content.RevisionAttachments(r.Context(), c.session.Workspace.WorkspaceID, item.ID, item.Revision)
	if err != nil {
		return wikiPage{}, contentProblem(err)
	}
	return wikiPage{Resource: item, Markdown: item.Body, HTML: rendered.HTML, Links: links, Sources: sources, Attachments: attachments}, nil
}

func (s *Server) writePage(w http.ResponseWriter, r *http.Request, c contentCaller, id string) {
	item, err := s.Content.Get(r.Context(), c.session.Workspace.WorkspaceID, c.actor, id)
	if err == nil && item.Kind != "wiki" {
		err = content.ErrNotFound
	}
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	page, err := s.buildPage(r, c, item)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// handleWikiPage implements GET /enterprise/v1/wiki/pages/{id}.
func (s *Server) handleWikiPage(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	s.writePage(w, r, c, r.PathValue("id"))
}

// handleWikiPageByPath implements GET /enterprise/v1/wiki/by-path/{path...}.
func (s *Server) handleWikiPageByPath(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	id, err := s.Content.ResolvePath(r.Context(), c.session.Workspace.WorkspaceID, r.PathValue("path"))
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	s.writePage(w, r, c, id)
}

// handleWikiAttachment implements GET /enterprise/v1/wiki/pages/{id}/attachments/{sha256}:
// the bytes of an attachment the readable page revision offers (see
// content.RevisionAttachments), never a blob looked up by hash alone.
func (s *Server) handleWikiAttachment(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	ws := c.session.Workspace.WorkspaceID
	item, err := s.Content.Get(r.Context(), ws, c.actor, r.PathValue("id"))
	if err == nil && item.Kind != "wiki" {
		err = content.ErrNotFound
	}
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	offered, err := s.Content.RevisionAttachments(r.Context(), ws, item.ID, item.Revision)
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	var found *content.Attachment
	for i := range offered {
		if offered[i].SHA256 == r.PathValue("sha256") {
			found = &offered[i]
		}
	}
	if found == nil {
		writeProblem(w, r, errs.New(errs.NotFound, "attachment not found"))
		return
	}
	f, stored, err := s.Blobs.Open(r.Context(), ws, found.SHA256)
	if err != nil {
		writeProblem(w, r, errs.New(errs.NotFound, "attachment not found"))
		return
	}
	defer func() { _ = f.Close() }()
	disposition := "attachment"
	if strings.HasPrefix(stored.MediaType, "image/") {
		disposition = "inline"
	}
	h := w.Header()
	h.Set("Content-Type", stored.MediaType)
	h.Set("Content-Length", strconv.FormatInt(stored.Size, 10))
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": found.Name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// handleWikiTree implements GET /enterprise/v1/wiki/tree.
func (s *Server) handleWikiTree(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	nodes, err := s.Wiki.Tree(r.Context(), c.session.Workspace.WorkspaceID)
	if err != nil {
		writeProblem(w, r, wikiProblem(err))
		return
	}
	pages, err := s.Content.TreePages(r.Context(), c.session.Workspace.WorkspaceID, c.actor)
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "pages": pages})
}

// handleWikiSearch implements GET /enterprise/v1/wiki/search?q=&limit=.
func (s *Server) handleWikiSearch(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 20 {
			writeProblem(w, r, errs.New(errs.InvalidArgument, "limit must be 1-20"))
			return
		}
		limit = n
	}
	hits, err := s.Content.Search(r.Context(), c.session.Workspace.WorkspaceID, c.actor, r.URL.Query().Get("q"), limit)
	if err != nil {
		writeProblem(w, r, contentProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": hits})
}

func (s *Server) wikiEvent(r *http.Request, session gatewayIdentities) audit.Event {
	ev := audit.NewEvent("wiki", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: session.userID}
	ev.WorkspaceID = session.workspace
	ev.RequestID = requestIDOf(r)
	ev.TraceID = traceIDOf(r)
	return ev
}

func decodeNode(w http.ResponseWriter, r *http.Request) (wiki.NodeInput, bool) {
	var in wiki.NodeInput
	if err := decodeJSONBody(w, r, &in); err != nil {
		writeProblem(w, r, err)
		return wiki.NodeInput{}, false
	}
	return in, true
}

// handleCreateWikiNode implements POST /enterprise/v1/wiki/nodes (admin).
func (s *Server) handleCreateWikiNode(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.KnowledgeManage)
	if !ok {
		return
	}
	in, ok := decodeNode(w, r)
	if !ok {
		return
	}
	node, err := s.Wiki.Create(r.Context(), session.workspace, in, s.wikiEvent(r, session))
	if err != nil {
		writeProblem(w, r, wikiProblem(err))
		return
	}
	writeJSON(w, http.StatusCreated, node)
}

// handleUpdateWikiNode implements PATCH /enterprise/v1/wiki/nodes/{id} (admin).
func (s *Server) handleUpdateWikiNode(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.KnowledgeManage)
	if !ok {
		return
	}
	in, ok := decodeNode(w, r)
	if !ok {
		return
	}
	if err := s.Wiki.Update(r.Context(), session.workspace, r.PathValue("id"), in, s.wikiEvent(r, session)); err != nil {
		writeProblem(w, r, wikiProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "slug": wiki.NormalizeSlug(in.Slug), "title": in.Title, "sort": in.Sort})
}

// handleDeleteWikiNode implements DELETE /enterprise/v1/wiki/nodes/{id} (admin).
func (s *Server) handleDeleteWikiNode(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requirePermission(w, r, rbac.KnowledgeManage)
	if !ok {
		return
	}
	if err := s.Wiki.Delete(r.Context(), session.workspace, r.PathValue("id"), s.wikiEvent(r, session)); err != nil {
		writeProblem(w, r, wikiProblem(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
