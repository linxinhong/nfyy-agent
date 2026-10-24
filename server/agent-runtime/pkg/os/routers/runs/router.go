// Package runs provides the /runs HTTP router for AgentOS.
package runs

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/authz"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/service"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Router is the http.Handler for run lookup routes.
type Router struct {
	db      db.DB
	runs    *runmanager.Manager
	streams *service.StreamHub
	mux     *http.ServeMux
}

// NewRouter creates a router that serves run lookup endpoints.
func NewRouter(store db.DB, managers ...*runmanager.Manager) http.Handler {
	var manager *runmanager.Manager
	if len(managers) > 0 {
		manager = managers[0]
	}
	return NewRouterWithStreams(store, manager, nil)
}

// NewRouterWithStreams also serves GET /{run_id}/events, which replays a
// run's journaled SSE frames after Last-Event-ID and then follows it live.
func NewRouterWithStreams(store db.DB, manager *runmanager.Manager, streams *service.StreamHub) http.Handler {
	r := &Router{
		db:      store,
		runs:    manager,
		streams: streams,
		mux:     http.NewServeMux(),
	}
	r.mux.HandleFunc("GET /{run_id}/events", r.streamEvents)
	r.mux.HandleFunc("GET /{run_id}", r.getRun)
	r.mux.HandleFunc("POST /{run_id}/cancel", r.cancelRun)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

// streamEvents reattaches to a run's SSE stream. Only the identity that
// started the stream may attach; anything else is indistinguishable from an
// unknown run.
func (r *Router) streamEvents(w http.ResponseWriter, req *http.Request) {
	if r.streams == nil {
		respondError(w, http.StatusNotFound, "stream not found")
		return
	}
	err := r.streams.Serve(w, req, req.PathValue("run_id"), service.OwnerFromRequest(req), service.LastEventID(req))
	if errors.Is(err, service.ErrStreamNotFound) {
		// The journal lives on the instance that started the stream and
		// expires after its retention window; GET /runs/{run_id} still
		// returns the durable run.
		respondError(w, http.StatusNotFound, "stream not found")
	}
}

func (r *Router) getRun(w http.ResponseWriter, req *http.Request) {
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}

	runID := req.PathValue("run_id")

	userID, isolated := authz.UserID(req)
	query := db.RunQuery{RunID: runID}
	namespace, tenant := authz.Namespace(req)
	if tenant {
		query.Namespace = namespace
	}
	if isolated {
		query.UserID = userID
	}
	var run *session.RunOutput
	var err error
	var scoped db.ScopedRunStore
	if tenant {
		var ok bool
		scoped, ok = r.db.(db.ScopedRunStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
	}
	if tenant {
		run, err = scoped.GetRunScoped(req.Context(), query)
	} else if r.runs != nil {
		run, err = r.runs.Get(req.Context(), query)
	} else {
		run, err = r.db.GetRun(req.Context(), query)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run == nil {
		respondError(w, http.StatusNotFound, "run not found")
		return
	}
	respondJSON(w, http.StatusOK, run)
}

func (r *Router) cancelRun(w http.ResponseWriter, req *http.Request) {
	if r.runs == nil {
		respondError(w, http.StatusServiceUnavailable, "run manager not configured")
		return
	}
	userID, isolated := authz.UserID(req)
	query := db.RunQuery{RunID: req.PathValue("run_id")}
	namespace, tenant := authz.Namespace(req)
	if tenant {
		query.Namespace = namespace
	}
	if tenant {
		// Manager may expose a process-local pending entry that has not yet
		// been durably namespace-bound. TASK-006 rejects this path rather
		// than falling back to a potentially cross-tenant cancellation.
		respondError(w, http.StatusServiceUnavailable, "tenant cancellation not configured")
		return
	}
	if isolated {
		query.UserID = userID
	}
	output, err := r.runs.Cancel(req.Context(), query)
	switch {
	case errors.Is(err, db.ErrNotFound):
		respondError(w, http.StatusNotFound, "run not found")
	case errors.Is(err, runmanager.ErrNotCancellable):
		respondJSON(w, http.StatusConflict, output)
	case err != nil:
		respondError(w, http.StatusInternalServerError, err.Error())
	default:
		respondJSON(w, http.StatusOK, output)
	}
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
