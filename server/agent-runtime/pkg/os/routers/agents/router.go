// Package agents provides the /agents HTTP router for AgentOS.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/authz"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/service"
	"github.com/linxinhong/xingding-agent-runtime/pkg/requirement"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	toolservice "github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// Router is the http.Handler for agent routes.
type Router struct {
	agents           map[string]*agent.Agent
	db               db.DB
	runs             *runmanager.Manager
	requirePrincipal bool
	streams          *service.StreamHub
	runTimeout       time.Duration
	mux              *http.ServeMux
}

// Config controls host-only route behavior. RequirePrincipal is enabled when
// strict memory is assembled so run and memory subjects always derive from
// the same verified per-request principal.
type Config struct {
	Agents           map[string]*agent.Agent
	DB               db.DB
	Runs             *runmanager.Manager
	RequirePrincipal bool
	// Streams journals SSE runs for Last-Event-ID reconnects. Nil keeps
	// streams bound to the request connection.
	Streams *service.StreamHub
	// RunTimeout bounds synchronous and streamed runs. Zero selects 120s.
	RunTimeout time.Duration
}

// NewRouter creates a router that serves the registered agents.
func NewRouter(agents map[string]*agent.Agent, store db.DB, managers ...*runmanager.Manager) http.Handler {
	var manager *runmanager.Manager
	if len(managers) > 0 {
		manager = managers[0]
	}
	return NewRouterWithConfig(Config{Agents: agents, DB: store, Runs: manager})
}

// NewRouterWithConfig creates an agent router with explicit strict-memory
// authority requirements while preserving the legacy constructor above.
func NewRouterWithConfig(cfg Config) http.Handler {
	r := &Router{
		agents:           cfg.Agents,
		db:               cfg.DB,
		runs:             cfg.Runs,
		requirePrincipal: cfg.RequirePrincipal,
		streams:          cfg.Streams,
		runTimeout:       cfg.RunTimeout,
		mux:              http.NewServeMux(),
	}
	if r.runTimeout <= 0 {
		r.runTimeout = 120 * time.Second
	}
	r.mux.HandleFunc("GET /", r.listAgents)
	r.mux.HandleFunc("GET /{id}", r.getAgent)
	r.mux.HandleFunc("GET /{id}/runs", r.listAgentRuns)
	r.mux.HandleFunc("POST /{id}/runs", r.runAgent)
	r.mux.HandleFunc("POST /{id}/runs/{run_id}/resume", r.resumeAgent)
	r.mux.HandleFunc("POST /{id}/runs/{run_id}/recover", r.recoverRun)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) listAgents(w http.ResponseWriter, req *http.Request) {
	items := make([]AgentSummary, 0, len(r.agents))
	for id, a := range r.agents {
		items = append(items, AgentSummary{
			ID:   id,
			Name: a.Name(),
		})
	}
	respondJSON(w, http.StatusOK, ListAgentsResponse{Agents: items})
}

func (r *Router) getAgent(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	a, ok := r.agents[id]
	if !ok {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}
	respondJSON(w, http.StatusOK, AgentInfoResponse{
		ID:   id,
		Name: a.Name(),
	})
}

func (r *Router) listAgentRuns(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if _, ok := r.agents[id]; !ok {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}
	if _, tenant := authz.Namespace(req); tenant {
		if _, ok := r.db.(db.NamespaceScopedStore); !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
	}

	userID, isolated := authz.UserID(req)
	query := db.SessionQuery{ComponentID: id}
	if isolated {
		query.UserID = userID
	}
	sessions, err := r.db.ListSessions(req.Context(), query)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var runs []*session.RunOutput
	for _, s := range sessions {
		runs = append(runs, s.Runs...)
	}
	respondJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (r *Router) runAgent(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	a, ok := r.agents[id]
	if !ok {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}

	stream, _ := strconv.ParseBool(req.URL.Query().Get("stream"))
	background, _ := strconv.ParseBool(req.URL.Query().Get("background"))
	if stream && background {
		respondError(w, http.StatusBadRequest, "stream and background cannot both be enabled")
		return
	}

	// Minimal body parsing: expect JSON {"message":"..."} or plain text body.
	var input string
	var userID, sessionID string
	if req.Header.Get("Content-Type") == "application/json" {
		var body struct {
			Message   string          `json:"message"`
			UserID    string          `json:"user_id"`
			SessionID string          `json:"session_id"`
			Metadata  json.RawMessage `json:"metadata"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err == nil {
			if len(body.Metadata) > 0 {
				respondError(w, http.StatusBadRequest, "run metadata is not accepted from HTTP clients")
				return
			}
			input = body.Message
			userID = body.UserID
			sessionID = body.SessionID
		}
	}
	if input == "" {
		if b, err := io.ReadAll(req.Body); err == nil {
			input = strings.TrimSpace(string(b))
		}
	}
	if principal, err := toolservice.PrincipalFromContext(req.Context()); err == nil {
		userID = principal.Subject
	} else if r.requirePrincipal {
		respondError(w, http.StatusUnauthorized, "trusted principal required")
		return
	} else if authenticatedUserID, isolated := authz.UserID(req); isolated {
		userID = authenticatedUserID
	}
	if sessionID != "" && r.db != nil {
		if _, tenant := authz.Namespace(req); tenant {
			if _, ok := r.db.(db.NamespaceScopedStore); !ok {
				respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
				return
			}
		}
		sess, err := r.db.ReadSession(req.Context(), sessionID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if sess != nil && (!authz.Owns(req, sess.UserID) || sess.AgentID != id) {
			respondError(w, http.StatusNotFound, "session not found")
			return
		}
	}

	runRequest := session.RunRequest{Input: session.RunInput{Message: input}, UserID: userID, SessionID: sessionID}
	if background {
		if r.runs == nil {
			respondError(w, http.StatusServiceUnavailable, "run manager not configured")
			return
		}
		output, err := r.runs.StartContext(authz.BackgroundExecutionContext(req.Context(), r.requirePrincipal), runRequest, a.RunWith)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, runmanager.ErrStoreRequired) {
				status = http.StatusServiceUnavailable
			} else if errors.Is(err, runmanager.ErrStartTimeout) {
				status = http.StatusGatewayTimeout
			} else if errors.Is(err, agent.ErrBusy) || errors.Is(err, runmanager.ErrAdmissionQueueFull) || errors.Is(err, runmanager.ErrAdmissionTimeout) {
				status = http.StatusTooManyRequests
			}
			respondError(w, status, err.Error())
			return
		}
		respondJSON(w, http.StatusAccepted, output)
		return
	}

	if stream && r.streams != nil {
		runRequest.RunID = uuid.NewString()
		runCtx, runCancel := context.WithTimeout(context.WithoutCancel(req.Context()), r.runTimeout)
		events, err := a.RunStreamWith(runCtx, runRequest)
		if err != nil {
			runCancel()
			status := http.StatusInternalServerError
			if errors.Is(err, agent.ErrBusy) || errors.Is(err, runmanager.ErrAdmissionQueueFull) || errors.Is(err, runmanager.ErrAdmissionTimeout) {
				status = http.StatusTooManyRequests
			}
			respondError(w, status, err.Error())
			return
		}
		r.streams.StreamRun(w, req, runRequest.RunID, runCancel, func(pub *service.Publisher) {
			service.PublishAgentEvents(pub, runRequest.RunID, events, nil)
		})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), r.runTimeout)
	defer cancel()

	if stream {
		events, err := a.RunStreamWith(ctx, runRequest)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, agent.ErrBusy) || errors.Is(err, runmanager.ErrAdmissionQueueFull) || errors.Is(err, runmanager.ErrAdmissionTimeout) {
				status = http.StatusTooManyRequests
			}
			respondError(w, status, err.Error())
			return
		}
		r.runStream(w, events)
		return
	}

	output, err := a.RunWith(ctx, runRequest)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, agent.ErrBusy), errors.Is(err, runmanager.ErrAdmissionQueueFull), errors.Is(err, runmanager.ErrAdmissionTimeout), errors.Is(err, agent.ErrQuotaExceeded):
			status = http.StatusTooManyRequests
		case errors.Is(err, agent.ErrSessionBusy):
			status = http.StatusConflict
		}
		respondError(w, status, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, output)
}

func (r *Router) runStream(w http.ResponseWriter, events <-chan agent.Event) {
	service.StreamEvents(w, events)
}

func (r *Router) resumeAgent(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	a, ok := r.agents[id]
	if !ok {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}
	var body struct {
		RequirementID string `json:"requirement_id"`
		Action        string `json:"action"`
		Input         any    `json:"input,omitempty"`
		UserID        string `json:"user_id,omitempty"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid resume request")
		return
	}
	userID := body.UserID
	actorID := ""
	if principal, err := toolservice.PrincipalFromContext(req.Context()); err == nil {
		userID = principal.Subject
		actorID = principal.Subject
	} else if r.requirePrincipal {
		respondError(w, http.StatusUnauthorized, "trusted principal required")
		return
	} else if authenticatedUserID, isolated := authz.UserID(req); isolated {
		userID = authenticatedUserID
		actorID = authenticatedUserID
	}
	resumeReq := session.ResumeRequest{
		RunID:         req.PathValue("run_id"),
		RequirementID: body.RequirementID,
		Action:        body.Action,
		ActorID:       actorID,
		UserID:        userID,
		Input:         body.Input,
	}
	if _, ok := requirement.ResolveAction(body.Action); !ok {
		r.respondResumeError(w, agent.ErrInvalidResume)
		return
	}
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}
	query := db.RunQuery{RunID: resumeReq.RunID, UserID: userID}
	namespace, tenant := authz.Namespace(req)
	var stored *session.RunOutput
	var err error
	if tenant {
		query.Namespace = namespace
		scoped, ok := r.db.(db.ScopedRunStore)
		if !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
		stored, err = scoped.GetRunScoped(req.Context(), query)
	} else {
		stored, err = r.db.GetRun(req.Context(), query)
	}
	if err != nil {
		r.respondResumeError(w, err)
		return
	}
	if stored == nil || stored.AgentID != id || !hasRequirement(stored, body.RequirementID) {
		r.respondResumeError(w, agent.ErrResumeNotFound)
		return
	}
	stream, _ := strconv.ParseBool(req.URL.Query().Get("stream"))
	if stream && r.streams != nil {
		runCtx, runCancel := context.WithTimeout(context.WithoutCancel(req.Context()), r.runTimeout)
		events, err := a.ResumeStream(runCtx, resumeReq)
		if err != nil {
			runCancel()
			r.respondResumeError(w, err)
			return
		}
		r.streams.StreamRun(w, req, resumeReq.RunID, runCancel, func(pub *service.Publisher) {
			service.PublishAgentEvents(pub, resumeReq.RunID, events, nil)
		})
		return
	}
	if stream {
		events, err := a.ResumeStream(req.Context(), resumeReq)
		if err != nil {
			r.respondResumeError(w, err)
			return
		}
		r.runStream(w, events)
		return
	}
	output, err := a.Resume(req.Context(), resumeReq)
	if err != nil {
		r.respondResumeError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, output)
}

// recoverRun continues a run whose process crashed from its last
// checkpoint (agent.Agent.Recover). The user comes from verified identity
// only.
func (r *Router) recoverRun(w http.ResponseWriter, req *http.Request) {
	a, ok := r.agents[req.PathValue("id")]
	if !ok {
		respondError(w, http.StatusNotFound, "agent not found")
		return
	}
	userID := ""
	if principal, err := toolservice.PrincipalFromContext(req.Context()); err == nil {
		userID = principal.Subject
	} else if r.requirePrincipal {
		respondError(w, http.StatusUnauthorized, "trusted principal required")
		return
	} else if authenticatedUserID, isolated := authz.UserID(req); isolated {
		userID = authenticatedUserID
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), r.runTimeout)
	defer cancel()
	output, err := a.Recover(ctx, agent.RecoverRequest{RunID: req.PathValue("run_id"), UserID: userID})
	switch {
	case errors.Is(err, agent.ErrRecoverNotFound):
		respondError(w, http.StatusNotFound, "run not found")
	case errors.Is(err, agent.ErrRecoverNotAllowed), errors.Is(err, agent.ErrRecoverConflict), errors.Is(err, agent.ErrSessionBusy):
		respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, agent.ErrBusy), errors.Is(err, runmanager.ErrAdmissionQueueFull), errors.Is(err, runmanager.ErrAdmissionTimeout), errors.Is(err, agent.ErrQuotaExceeded):
		respondError(w, http.StatusTooManyRequests, err.Error())
	case err != nil && output == nil:
		respondError(w, http.StatusInternalServerError, err.Error())
	default:
		// A recovered run that then fails still returns its record.
		respondJSON(w, http.StatusOK, output)
	}
}

func hasRequirement(run *session.RunOutput, id string) bool {
	for i := range run.Requirements {
		if run.Requirements[i].ID == id {
			return true
		}
	}
	return false
}

func (r *Router) respondResumeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, agent.ErrResumeNotFound), errors.Is(err, db.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, agent.ErrResumeConflict), errors.Is(err, db.ErrConflict), errors.Is(err, db.ErrInvalidTransition):
		status = http.StatusConflict
	case errors.Is(err, agent.ErrInvalidResume):
		status = http.StatusBadRequest
	case errors.Is(err, agent.ErrBusy), errors.Is(err, runmanager.ErrAdmissionQueueFull), errors.Is(err, runmanager.ErrAdmissionTimeout):
		status = http.StatusTooManyRequests
	}
	respondError(w, status, err.Error())
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

// ensure RunOutput is referenced for SSE completion payload.
var _ *session.RunOutput
