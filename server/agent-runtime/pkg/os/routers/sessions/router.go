// Package sessions provides the /sessions HTTP router for AgentOS.
package sessions

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/authz"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Router is the http.Handler for agent session routes.
type Router struct {
	db  db.DB
	mux *http.ServeMux
}

// NewRouter creates a router that serves session CRUD endpoints.
func NewRouter(store db.DB) http.Handler {
	r := &Router{
		db:  store,
		mux: http.NewServeMux(),
	}
	r.mux.HandleFunc("GET /", r.listSessions)
	r.mux.HandleFunc("POST /", r.createSession)
	r.mux.HandleFunc("GET /{id}", r.getSession)
	r.mux.HandleFunc("PATCH /{id}", r.updateSession)
	r.mux.HandleFunc("DELETE /{id}", r.deleteSession)
	r.mux.HandleFunc("GET /{id}/runs", r.listSessionRuns)
	r.mux.HandleFunc("GET /{id}/runs/{run_id}", r.getSessionRun)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) listSessions(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}

	agentID := req.URL.Query().Get("agent_id")
	limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(req.URL.Query().Get("offset"))

	userID, isolated := authz.UserID(req)
	query := db.SessionQuery{ComponentID: agentID, Limit: limit, Offset: offset}
	namespace, tenant := authz.Namespace(req)
	if tenant {
		query.Namespace = namespace
	}
	if isolated {
		query.UserID = userID
	}
	var sessions []*session.AgentSession
	var err error
	if tenant {
		store, ok := r.db.(db.ScopedAgentSessionStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
		sessions, err = store.ListSessionsScoped(req.Context(), query)
	} else {
		sessions, err = r.db.ListSessions(req.Context(), query)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	items := make([]SessionResponse, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, toSessionResponse(s))
	}
	respondJSON(w, http.StatusOK, map[string]any{"sessions": items})
}

func (r *Router) createSession(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}

	var body CreateSessionRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if userID, isolated := authz.UserID(req); isolated {
		body.UserID = userID
	}
	namespace, tenant := authz.Namespace(req)

	now := time.Now().UTC()
	s := &session.AgentSession{
		SessionID:    uuid.NewString(),
		AgentID:      body.AgentID,
		UserID:       body.UserID,
		Metadata:     body.Metadata,
		SessionState: body.SessionState,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if tenant {
		s.Namespace = namespace
	}

	var err error
	if tenant {
		store, ok := r.db.(db.ScopedAgentSessionStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
		err = store.CreateSessionScoped(req.Context(), s)
	} else {
		err = r.db.CreateSession(req.Context(), s)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, toSessionResponse(s))
}

func (r *Router) getSession(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}

	id := req.PathValue("id")
	namespace, tenant := authz.Namespace(req)
	var s *session.AgentSession
	var err error
	if tenant {
		store, ok := r.db.(db.ScopedAgentSessionStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
		s, err = store.ReadSessionScoped(req.Context(), namespace, id)
	} else {
		s, err = r.db.ReadSession(req.Context(), id)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s == nil {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	if !authz.Owns(req, s.UserID) {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	respondJSON(w, http.StatusOK, toSessionResponse(s))
}

func (r *Router) updateSession(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}

	id := req.PathValue("id")
	namespace, tenant := authz.Namespace(req)
	var s *session.AgentSession
	var err error
	var scoped db.ScopedAgentSessionStore
	if tenant {
		var ok bool
		scoped, ok = r.db.(db.ScopedAgentSessionStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
		s, err = scoped.ReadSessionScoped(req.Context(), namespace, id)
	} else {
		s, err = r.db.ReadSession(req.Context(), id)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s == nil {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	if !authz.Owns(req, s.UserID) {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}

	var body UpdateSessionRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.Revision == 0 {
		respondError(w, http.StatusBadRequest, "revision is required")
		return
	}

	if body.Metadata != nil {
		s.Metadata = body.Metadata
	}
	s.Revision = body.Revision
	if body.SessionState != nil {
		s.SessionState = body.SessionState
	}
	if body.Summary != nil {
		s.Summary = *body.Summary
	}
	s.UpdatedAt = time.Now().UTC()

	update := db.SessionFieldsUpdate{SessionID: id, Namespace: namespace, ExpectedRevision: body.Revision, Metadata: body.Metadata, SessionState: body.SessionState, Summary: body.Summary}
	var revision uint64
	if tenant {
		revision, err = scoped.UpdateSessionFieldsScoped(req.Context(), update)
	} else {
		revision, err = r.db.UpdateSessionFields(req.Context(), update)
	}
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, db.ErrNotFound) {
			respondError(w, http.StatusNotFound, "session not found")
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Revision = revision
	respondJSON(w, http.StatusOK, toSessionResponse(s))
}

func (r *Router) deleteSession(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}

	id := req.PathValue("id")
	namespace, tenant := authz.Namespace(req)
	var s *session.AgentSession
	var err error
	var scoped db.ScopedAgentSessionStore
	if tenant {
		var ok bool
		scoped, ok = r.db.(db.ScopedAgentSessionStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
		s, err = scoped.ReadSessionScoped(req.Context(), namespace, id)
	} else {
		s, err = r.db.ReadSession(req.Context(), id)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s == nil || !authz.Owns(req, s.UserID) {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	if tenant {
		err = scoped.DeleteSessionScoped(req.Context(), namespace, id)
	} else {
		err = r.db.DeleteSession(req.Context(), id)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (r *Router) listSessionRuns(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}

	id := req.PathValue("id")
	namespace, tenant := authz.Namespace(req)
	var s *session.AgentSession
	var err error
	if tenant {
		store, ok := r.db.(db.ScopedAgentSessionStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
		s, err = store.ReadSessionScoped(req.Context(), namespace, id)
	} else {
		s, err = r.db.ReadSession(req.Context(), id)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s == nil {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	if !authz.Owns(req, s.UserID) {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"runs": s.Runs})
}

func (r *Router) getSessionRun(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}

	id := req.PathValue("id")
	runID := req.PathValue("run_id")

	namespace, tenant := authz.Namespace(req)
	var s *session.AgentSession
	var err error
	if tenant {
		store, ok := r.db.(db.ScopedAgentSessionStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
		s, err = store.ReadSessionScoped(req.Context(), namespace, id)
	} else {
		s, err = r.db.ReadSession(req.Context(), id)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s == nil {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}
	if !authz.Owns(req, s.UserID) {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}

	for _, run := range s.Runs {
		if run.RunID == runID {
			respondJSON(w, http.StatusOK, run)
			return
		}
	}
	respondError(w, http.StatusNotFound, "run not found")
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
