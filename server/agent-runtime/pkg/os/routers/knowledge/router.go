// Package knowledge provides the /knowledge HTTP router for AgentOS.
package knowledge

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
)

// Router is the http.Handler for knowledge routes.
type Router struct {
	knowledges map[string]knowledge.Knowledge
	mux        *http.ServeMux
}

// NewRouter creates a router that serves knowledge endpoints.
func NewRouter(knowledges map[string]knowledge.Knowledge) http.Handler {
	r := &Router{
		knowledges: knowledges,
		mux:        http.NewServeMux(),
	}
	r.mux.HandleFunc("GET /{$}", r.listKnowledges)
	r.mux.HandleFunc("POST /{id}/search", r.searchKnowledge)
	r.mux.HandleFunc("GET /{id}/content/{doc_id}", r.getContent)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) listKnowledges(w http.ResponseWriter, req *http.Request) {
	items := make([]KnowledgeSummary, 0, len(r.knowledges))
	for id := range r.knowledges {
		items = append(items, KnowledgeSummary{ID: id, Name: id})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	respondJSON(w, http.StatusOK, ListKnowledgeResponse{Knowledges: items})
}

func (r *Router) searchKnowledge(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	kb, ok := r.knowledges[id]
	if !ok {
		respondError(w, http.StatusNotFound, "knowledge base not found")
		return
	}

	var body SearchRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.Query == "" {
		respondError(w, http.StatusBadRequest, "query is required")
		return
	}
	if body.Limit == 0 {
		body.Limit = 5
	}

	docs, err := kb.Search(req.Context(), body.Query, body.Limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]DocumentResponse, 0, len(docs))
	for _, d := range docs {
		items = append(items, DocumentResponse{
			ID:      d.ID,
			Content: d.Content,
		})
	}
	respondJSON(w, http.StatusOK, SearchResponse{Documents: items})
}

func (r *Router) getContent(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	kb, ok := r.knowledges[id]
	if !ok {
		respondError(w, http.StatusNotFound, "knowledge base not found")
		return
	}

	docID := req.PathValue("doc_id")
	content, err := kb.GetContent(req.Context(), docID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"content": content})
}

func respondJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func respondError(w http.ResponseWriter, status int, msg string) {
	// Never leak internal details (DB/file/runtime errors) to clients:
	// 5xx responses always get a generic message. The underlying error is
	// logged at the call site via observability, not echoed to HTTP.
	if status >= 500 {
		msg = "internal server error"
	}
	respondJSON(w, status, map[string]string{"error": msg})
}

// compile-time interface check.
var _ http.Handler = (*Router)(nil)
