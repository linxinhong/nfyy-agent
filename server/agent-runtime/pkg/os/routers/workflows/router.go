// Package workflows provides the /workflows HTTP router for AgentOS.
package workflows

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
	toolservice "github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

// Router is the http.Handler for workflow routes.
type Router struct {
	workflows        map[string]*workflow.Workflow
	db               db.DB
	runs             *runmanager.Manager
	requirePrincipal bool
	streams          *service.StreamHub
	runTimeout       time.Duration
	mux              *http.ServeMux
}

type Config struct {
	Workflows        map[string]*workflow.Workflow
	DB               db.DB
	Runs             *runmanager.Manager
	RequirePrincipal bool
	// Streams journals SSE runs for Last-Event-ID reconnects. Nil keeps
	// streams bound to the request connection.
	Streams *service.StreamHub
	// RunTimeout bounds synchronous and streamed runs. Zero selects 120s.
	RunTimeout time.Duration
}

// NewRouter creates a router that serves the registered workflows.
func NewRouter(workflows map[string]*workflow.Workflow, store db.DB, managers ...*runmanager.Manager) http.Handler {
	var manager *runmanager.Manager
	if len(managers) > 0 {
		manager = managers[0]
	}
	return NewRouterWithConfig(Config{Workflows: workflows, DB: store, Runs: manager})
}

func NewRouterWithConfig(cfg Config) http.Handler {
	r := &Router{
		workflows:        cfg.Workflows,
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
	r.mux.HandleFunc("GET /", r.listWorkflows)
	r.mux.HandleFunc("GET /{id}", r.getWorkflow)
	r.mux.HandleFunc("GET /{id}/runs", r.listWorkflowRuns)
	r.mux.HandleFunc("POST /{id}/runs", r.runWorkflow)
	r.mux.HandleFunc("GET /{id}/runs/{run_id}", r.getWorkflowRun)
	r.mux.HandleFunc("POST /{id}/runs/{run_id}/resume", r.resumeWorkflow)
	return r
}

func (r *Router) getWorkflowRun(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if _, ok := r.workflows[id]; !ok {
		respondError(w, http.StatusNotFound, "workflow not found")
		return
	}
	if r.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}
	namespace, tenant := authz.Namespace(req)
	if tenant {
		if _, ok := r.db.(db.ScopedRunStore); !ok {
			respondError(w, http.StatusServiceUnavailable, "tenant storage not configured")
			return
		}
	}
	userID, isolated := authz.UserID(req)
	query := db.RunQuery{RunID: req.PathValue("run_id")}
	if tenant {
		query.Namespace = namespace
	}
	if isolated {
		query.UserID = userID
	}
	var output *session.RunOutput
	var err error
	if tenant {
		output, err = r.db.(db.ScopedRunStore).GetRunScoped(req.Context(), query)
	} else {
		output, err = r.db.GetRun(req.Context(), query)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if output == nil || output.WorkflowID != id {
		respondError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	respondJSON(w, http.StatusOK, output)
}

func (r *Router) resumeWorkflow(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	wf, ok := r.workflows[id]
	if !ok {
		respondError(w, http.StatusNotFound, "workflow not found")
		return
	}
	var body struct {
		RequirementID string `json:"requirement_id"`
		Action        string `json:"action"`
		Input         any    `json:"input"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid resume request")
		return
	}
	userID, isolated := authz.UserID(req)
	if principal, err := toolservice.PrincipalFromContext(req.Context()); err == nil {
		userID, isolated = principal.Subject, true
	} else if r.requirePrincipal {
		respondError(w, http.StatusUnauthorized, "trusted principal required")
		return
	}
	actorID := ""
	if isolated {
		actorID = userID
	}
	request := workflow.ResumeRequest{
		RunID: req.PathValue("run_id"), RequirementID: body.RequirementID, Action: workflow.ResumeAction(body.Action),
		Input: body.Input, ActorID: actorID, UserID: userID,
	}
	stream, _ := strconv.ParseBool(req.URL.Query().Get("stream"))
	if stream && r.streams != nil {
		runCtx, runCancel := context.WithTimeout(context.WithoutCancel(req.Context()), r.runTimeout)
		r.streams.StreamRun(w, req, request.RunID, runCancel, func(pub *service.Publisher) {
			events, err := wf.ResumeEvents(runCtx, request)
			if err != nil {
				pub.Publish("run_error", map[string]string{"error": err.Error()})
				return
			}
			for event := range events {
				pub.Publish(string(event.Type), event)
			}
		})
		return
	}
	if stream {
		r.resumeStream(req.Context(), w, wf, request)
		return
	}
	output, err := wf.Resume(req.Context(), request)
	if err != nil {
		if output != nil && workflow.IsExecutionError(err) {
			respondJSON(w, http.StatusOK, output)
			return
		}
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, db.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, db.ErrConflict), errors.Is(err, db.ErrInvalidTransition):
			status = http.StatusConflict
		case strings.Contains(err.Error(), "invalid resume action"):
			status = http.StatusBadRequest
		}
		respondError(w, status, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, output)
}

func (r *Router) resumeStream(ctx context.Context, w http.ResponseWriter, wf *workflow.Workflow, request workflow.ResumeRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	events, err := wf.ResumeEvents(ctx, request)
	if err != nil {
		service.WriteSSE(w, flusher, "run_error", map[string]string{"error": err.Error()})
		return
	}
	for event := range events {
		service.WriteSSE(w, flusher, string(event.Type), event)
	}
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *Router) listWorkflows(w http.ResponseWriter, req *http.Request) {
	items := make([]WorkflowSummary, 0, len(r.workflows))
	for id, wf := range r.workflows {
		items = append(items, WorkflowSummary{
			ID:   id,
			Name: wf.Name(),
		})
	}
	respondJSON(w, http.StatusOK, ListWorkflowsResponse{Workflows: items})
}

func (r *Router) getWorkflow(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	wf, ok := r.workflows[id]
	if !ok {
		respondError(w, http.StatusNotFound, "workflow not found")
		return
	}
	respondJSON(w, http.StatusOK, WorkflowInfoResponse{
		ID:   id,
		Name: wf.Name(),
	})
}

func (r *Router) listWorkflowRuns(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if _, ok := r.workflows[id]; !ok {
		respondError(w, http.StatusNotFound, "workflow not found")
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
	sessions, err := r.db.ListWorkflowSessions(req.Context(), query)
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

func (r *Router) runWorkflow(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	wf, ok := r.workflows[id]
	if !ok {
		respondError(w, http.StatusNotFound, "workflow not found")
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
		sess, err := r.db.ReadWorkflowSession(req.Context(), sessionID)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if sess != nil && (!authz.Owns(req, sess.UserID) || sess.WorkflowID != id) {
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
		output, err := r.runs.StartContext(authz.BackgroundExecutionContext(req.Context(), r.requirePrincipal), runRequest, wf.RunWith)
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
			r.publishRun(runCtx, pub, wf, runRequest)
		})
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), r.runTimeout)
	defer cancel()

	if stream {
		r.runStream(ctx, w, wf, runRequest)
		return
	}

	output, err := wf.RunWith(ctx, runRequest)
	if err != nil {
		if output != nil && workflow.IsExecutionError(err) {
			respondJSON(w, http.StatusOK, output)
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, output)
}

func (r *Router) runStream(ctx context.Context, w http.ResponseWriter, wf *workflow.Workflow, request session.RunRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	if wf.Structured() {
		events, err := wf.RunEvents(ctx, workflow.RunRequest{Input: request.Input.Message, RunID: request.RunID, UserID: request.UserID, SessionID: request.SessionID, Metadata: request.Metadata})
		if err != nil {
			service.WriteSSE(w, flusher, "run_error", map[string]string{"error": err.Error()})
			return
		}
		for event := range events {
			service.WriteSSE(w, flusher, string(event.Type), event)
		}
		return
	}

	events, err := wf.RunStreamWith(ctx, request)
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

// publishRun is the journaled equivalent of runStream.
func (r *Router) publishRun(ctx context.Context, pub *service.Publisher, wf *workflow.Workflow, request session.RunRequest) {
	if wf.Structured() {
		events, err := wf.RunEvents(ctx, workflow.RunRequest{Input: request.Input.Message, RunID: request.RunID, UserID: request.UserID, SessionID: request.SessionID, Metadata: request.Metadata})
		if err != nil {
			pub.Publish("run_error", map[string]string{"error": err.Error()})
			return
		}
		for event := range events {
			pub.Publish(string(event.Type), event)
		}
		return
	}
	events, err := wf.RunStreamWith(ctx, request)
	if err != nil {
		pub.Publish("run_error", map[string]string{"error": err.Error()})
		return
	}
	service.PublishAgentEvents(pub, request.RunID, events, func(ev agent.Event) bool {
		switch ev.Type {
		case agent.EventTypeRunStarted, agent.EventTypeMessage, agent.EventTypeRunCompleted, agent.EventTypeRunError:
			return true
		}
		return false
	})
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

// WorkflowSummary is a lightweight workflow description.
type WorkflowSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListWorkflowsResponse is the response for GET /.
type ListWorkflowsResponse struct {
	Workflows []WorkflowSummary `json:"workflows"`
}

// WorkflowInfoResponse is the response for GET /{id}.
type WorkflowInfoResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
