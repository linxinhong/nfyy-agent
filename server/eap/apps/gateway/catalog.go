package main

// Skill and capability directory (packages/catalog) merged with the Skill
// Registry: Search and Item add the caller Workspace's released registry
// packages to the built-in entries. Session or employee key, like the
// registry catalog; artifact bytes are the signed package the client
// verifies, so they are served only after the same authentication.

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"enterprise-agent-platform/packages/catalog"
	"enterprise-agent-platform/packages/errs"
)

func catalogProblem(err error) error {
	switch {
	case errors.Is(err, catalog.ErrInvalidPath):
		return errs.New(errs.InvalidArgument, "invalid capability artifact path")
	case errors.Is(err, catalog.ErrNotFound):
		return errs.New(errs.NotFound, "catalog entry not found")
	case errors.Is(err, catalog.ErrNotPublished):
		return errs.New(errs.NotFound, "capability artifact not published")
	default:
		return errs.Wrap(err, errs.Internal, "catalog lookup failed")
	}
}

// catalogWorkspace authenticates the caller and returns the selected Workspace.
func (s *Server) catalogWorkspace(w http.ResponseWriter, r *http.Request) (string, bool) {
	auth, ok := s.requireCatalogAuth(w, r)
	if !ok {
		return "", false
	}
	session, err := s.selectWorkspace(r, auth)
	if err != nil {
		writeProblem(w, r, err)
		return "", false
	}
	return session.Workspace.WorkspaceID, true
}

// registryItems lists the newest released version of each registry package.
func (s *Server) registryItems(r *http.Request, workspaceID string) ([]catalog.Item, error) {
	rows, err := s.OIDCStore.Pool().Query(r.Context(), `
		SELECT DISTINCT ON (name) name, version
		FROM skill_packages
		WHERE workspace_id = $1 AND status = 'released' AND visibility = 'organization'
		ORDER BY name, created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []catalog.Item{}
	for rows.Next() {
		var name, version string
		if err := rows.Scan(&name, &version); err != nil {
			return nil, err
		}
		out = append(out, catalog.RegistryItem(name, version, workspaceID))
	}
	return out, rows.Err()
}

// handleCatalogSearch implements GET /enterprise/v1/catalog/search.
func (s *Server) handleCatalogSearch(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.catalogWorkspace(w, r)
	if !ok {
		return
	}
	extra, err := s.registryItems(r, workspaceID)
	if err != nil {
		writeProblem(w, r, catalogProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": catalog.SchemaVersion,
		"items":          s.Catalog.Search(r.URL.Query().Get("q"), extra),
	})
}

// handleCatalogItem implements GET /enterprise/v1/catalog/items/{id}.
func (s *Server) handleCatalogItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.catalogWorkspace(w, r)
	if !ok {
		return
	}
	extra, err := s.registryItems(r, workspaceID)
	if err != nil {
		writeProblem(w, r, catalogProblem(err))
		return
	}
	item, err := s.Catalog.Item(r.PathValue("id"), extra)
	if err != nil {
		writeProblem(w, r, catalogProblem(err))
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// handleCapabilityManifest implements GET /enterprise/v1/capability-packages/{id}/manifest.
func (s *Server) handleCapabilityManifest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.catalogWorkspace(w, r); !ok {
		return
	}
	raw, err := s.Catalog.Manifest(r.PathValue("id"))
	if err != nil {
		writeProblem(w, r, catalogProblem(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handleCapabilityArtifact implements GET /enterprise/v1/capability-packages/{id}/artifacts/{filename}.
func (s *Server) handleCapabilityArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.catalogWorkspace(w, r); !ok {
		return
	}
	f, err := s.Catalog.OpenArtifact(r.PathValue("id"), r.PathValue("filename"))
	if err != nil {
		writeProblem(w, r, catalogProblem(err))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeProblem(w, r, catalogProblem(catalog.ErrNotPublished))
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}
