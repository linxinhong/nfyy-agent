// Package teams provides the /teams HTTP router for AgentOS.
package teams

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
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/team"
	toolservice "github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// Router is the http.Handler for team routes.
type Router struct {
	teams            map[string]*team.Team
	db               db.DB
	runs             *runmanager.Manager
	requirePrincipal bool
	streams          *service.StreamHub
	runTimeout       time.Duration
	mux              *http.ServeMux
}

type Config struct {
	Teams            map[string]*team.Team
	DB               db.DB
	Runs             *runmanager.Manager
	RequirePrincipal bool
	// Streams journals SSE runs for Last-Event-ID reconnects. Nil keeps
	// streams bound to the request connection.
	Streams *service.StreamHub
	// RunTimeout bounds synchronous and streamed runs. Zero selects 120s.
	RunTimeout time.Duration
}

// NewRouter creates a router that serves the registered teams.
func NewRouter(teams map[string]*team.Team, store db.DB, managers ...*runmanager.Manager) http.Handler {
	var manager *runmanager.Manager
	if len(managers) > 0 {
		manager = managers[0]
	}
	return NewRouterWithConfig(Config{Teams: teams, DB: store, Runs: manager})
}

func NewRouterWithConfig(cfg Config) http.Handler {
	r := &Router{
		teams:            cfg.Teams,
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
	r.mux.HandleFunc("GET /", r.listTeams)
	r.mux.HandleFunc("GET /{id}", r.getTeam)
	r.mux.HandleFunc("GET /{id}/runs", r.listTeamRuns)
	r.mux.HandleFunc("POST /{id}/runs", r.runTeam)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) listTeams(w http.ResponseWriter, req *http.Request) {
	items := make([]TeamSummary, 0, len(r.teams))
	for id, t := range r.teams {
		items = append(items, TeamSummary{
			ID:   id,
			Name: t.Name(),
		})
	}
	respondJSON(w, http.StatusOK, ListTeamsResponse{Teams: items})
}

func (r *Router) getTeam(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	t, ok := r.teams[id]
	if !ok {
		respondError(w, http.StatusNotFound, "team not found")
		return
	}
	respondJSON(w, http.StatusOK, TeamInfoResponse{
		ID:   id,
		Name: t.Name(),
	})
}

func (r *Router) listTeamRuns(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if _, ok := r.teams[id]; !ok {
		respondError(w, http.StatusNotFound, "team not found")
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
	sessions, err := r.db.ListTeamSessions(req.Context(), query)
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

func (r *Router) runTeam(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	t, ok := r.teams[id]
	if !ok {
		respondError(w, http.StatusNotFound, "team not found")
		return
	}

	stream, _ := strconv.ParseBool(req.URL.Query().Get("stream"))
	background, _ := strconv.ParseBool(req.URL.Query().Get("background"))
	if stream && background {
		respondError(w, http.StatusBadRequest, "stream and background cannot both be enabled")
		return
	}

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
		sess, err := r.db.ReadTeamSession(req.Context(), sessionID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if sess != nil && (!authz.Owns(req, sess.UserID) || sess.TeamID != id) {
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
		output, err := r.runs.StartContext(authz.BackgroundExecutionContext(req.Context(), r.requirePrincipal), runRequest, t.RunWith)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, runmanager.ErrStoreRequired) {
				status = http.StatusServiceUnavailable
			} else if errors.Is(err, runmanager.ErrStartTimeout) {
				status = http.StatusGatewayTimeout
			} else if errors.Is(err, runmanager.ErrAdmissionQueueFull) || errors.Is(err, runmanager.ErrAdmissionTimeout) {
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
		r.streams.StreamRun(w, req, runRequest.RunID, runCancel, func(pub *service.Publisher) {
			events, err := t.RunStreamWith(runCtx, runRequest)
			if err != nil {
				pub.Publish("run_error", map[string]string{"error": err.Error()})
				return
			}
			service.PublishAgentEvents(pub, runRequest.RunID, events, teamStreamEvent)
		})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), r.runTimeout)
	defer cancel()

	if stream {
		r.runStream(ctx, w, t, runRequest)
		return
	}

	output, err := t.RunWith(ctx, runRequest)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, output)
}

func (r *Router) runStream(ctx context.Context, w http.ResponseWriter, t *team.Team, request session.RunRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	events, err := t.RunStreamWith(ctx, request)
	if err != nil {
		service.WriteSSE(w, flusher, "run_error", map[string]string{"error": err.Error()})
		return
	}

	for ev := range events {
		switch ev.Type {
		case agent.EventTypeRunStarted:
			service.WriteSSE(w, flusher, "run_started", nil)
		case agent.EventTypeMessage:
			service.WriteSSE(w, flusher, "message", map[string]string{"content": ev.Content})
		case agent.EventTypeRunCompleted:
			service.WriteSSE(w, flusher, "run_completed", ev.RunOutput)
		case agent.EventTypeRunError:
			service.WriteSSE(w, flusher, "run_error", map[string]string{"error": ev.Err.Error()})
		}
	}
}

// teamStreamEvent keeps the event types the team SSE stream has always
// exposed.
func teamStreamEvent(ev agent.Event) bool {
	switch ev.Type {
	case agent.EventTypeRunStarted, agent.EventTypeMessage, agent.EventTypeRunCompleted, agent.EventTypeRunError:
		return true
	}
	return false
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

// TeamSummary is a lightweight team description.
type TeamSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListTeamsResponse is the response for GET /.
type ListTeamsResponse struct {
	Teams []TeamSummary `json:"teams"`
}

// TeamInfoResponse is the response for GET /{id}.
type TeamInfoResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
