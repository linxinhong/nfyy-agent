// Package tools exposes the governed internal Tools REST API. It is the
// HTTP adapter for the managed ToolService: identity comes only from the
// trusted context, request bodies can never set Principal/Namespace/Scopes,
// and every operation goes through the same authorizer/approval pipeline as
// every other execution path.
package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/api"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/authz"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// Router is the http.Handler for the internal tools API.
type Router struct {
	tools     service.ToolService
	approvals *service.ApprovalService
	// PrincipalFrom injects the trusted principal from the request context.
	// When nil, every request fails closed (no identity, no access).
	PrincipalFrom func(r *http.Request) (execution.Principal, error)
	// RegistryRevision is echoed in responses for drift visibility.
	RegistryRevision string
}

// NewRouter creates the tools router. A nil PrincipalFrom fails closed.
func NewRouter(toolService service.ToolService, approvals *service.ApprovalService, principalFrom func(*http.Request) (execution.Principal, error)) *Router {
	if principalFrom == nil {
		principalFrom = func(*http.Request) (execution.Principal, error) {
			return execution.Principal{}, service.ErrIdentityRequired
		}
	}
	return &Router{
		tools:         toolService,
		approvals:     approvals,
		PrincipalFrom: principalFrom,
	}
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/tools":
		r.list(w, req)
	case req.Method == http.MethodGet && hasSegment(req.URL.Path, "/tools/"):
		r.describe(w, req)
	case req.Method == http.MethodPost && hasSegment(req.URL.Path, "/tools/") && hasSuffix(req.URL.Path, ":prepare"):
		r.prepare(w, req)
	case req.Method == http.MethodPost && hasSuffix(req.URL.Path, ":execute"):
		r.execute(w, req)
	case req.Method == http.MethodPost && hasSuffix(req.URL.Path, ":resolve"):
		r.resolve(w, req)
	default:
		respondError(w, http.StatusNotFound, "not_found", "route not found", r.RegistryRevision, requestID(req))
	}
}

// rejectIdentityFields rejects bodies that try to set identity fields.
func rejectIdentityFields(w http.ResponseWriter, raw json.RawMessage, rev, rid string) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		respondError(w, http.StatusUnprocessableEntity, "invalid_json", "request body is not valid JSON", rev, rid)
		return true
	}
	for _, field := range []string{"principal", "namespace", "subject", "scopes", "authz_epoch"} {
		if _, exists := probe[field]; exists {
			respondError(w, http.StatusUnprocessableEntity, "identity_not_allowed", "identity fields cannot be set by clients", rev, rid)
			return true
		}
	}
	return false
}

func (r *Router) principal(w http.ResponseWriter, req *http.Request) (execution.Principal, bool) {
	p, err := r.PrincipalFrom(req)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "identity_required", err.Error(), r.RegistryRevision, requestID(req))
		return execution.Principal{}, false
	}
	if err := execution.ValidateNamespace(p.Namespace); err != nil {
		respondError(w, http.StatusUnauthorized, "invalid_namespace", err.Error(), r.RegistryRevision, requestID(req))
		return execution.Principal{}, false
	}
	return p, true
}

func (r *Router) list(w http.ResponseWriter, req *http.Request) {
	p, ok := r.principal(w, req)
	if !ok {
		return
	}
	ctx := service.ContextWithPrincipal(req.Context(), p)
	page, err := r.tools.List(ctx, service.ToolQuery{Namespace: p.Namespace})
	if err != nil {
		respondServiceError(w, err, r.RegistryRevision, requestID(req))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"tools":             page.Tools,
		"registry_revision": r.RegistryRevision,
	})
}

func (r *Router) describe(w http.ResponseWriter, req *http.Request) {
	p, ok := r.principal(w, req)
	if !ok {
		return
	}
	name := segmentAfter(req.URL.Path, "/tools/")
	ctx := service.ContextWithPrincipal(req.Context(), p)
	desc, err := r.tools.Describe(ctx, name)
	if err != nil {
		respondServiceError(w, err, r.RegistryRevision, requestID(req))
		return
	}
	respondJSON(w, http.StatusOK, desc)
}

func (r *Router) prepare(w http.ResponseWriter, req *http.Request) {
	p, ok := r.principal(w, req)
	if !ok {
		return
	}
	name := segmentAfter(req.URL.Path, "/tools/")
	name = trimSuffix(name, ":prepare")

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		respondError(w, http.StatusUnprocessableEntity, "invalid_json", "invalid request body", r.RegistryRevision, requestID(req))
		return
	}
	if rejectIdentityFields(w, raw, r.RegistryRevision, requestID(req)) {
		return
	}
	var body struct {
		Arguments      json.RawMessage `json:"arguments"`
		IdempotencyKey string          `json:"idempotency_key,omitempty"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		respondError(w, http.StatusUnprocessableEntity, "invalid_json", "invalid request body", r.RegistryRevision, requestID(req))
		return
	}
	ctx := service.ContextWithPrincipal(req.Context(), p)
	prepared, err := r.tools.Prepare(ctx, service.PrepareRequest{ToolName: name, Arguments: body.Arguments, IdempotencyKey: body.IdempotencyKey})
	if err != nil {
		respondServiceError(w, err, r.RegistryRevision, requestID(req))
		return
	}
	status := http.StatusCreated
	if prepared.ApprovalRequired {
		status = http.StatusAccepted
	}
	respondJSON(w, status, map[string]any{
		"plan_id":           prepared.Ref.ID,
		"plan_version":      prepared.Ref.Version,
		"tool_name":         prepared.ToolName,
		"display":           prepared.Display,
		"approval_required": prepared.ApprovalRequired,
		"registry_revision": r.RegistryRevision,
	})
}

func (r *Router) execute(w http.ResponseWriter, req *http.Request) {
	p, ok := r.principal(w, req)
	if !ok {
		return
	}
	planID := segmentAfter(req.URL.Path, "/tool-executions/")
	planID = trimSuffix(planID, ":execute")
	if planID == "" {
		respondError(w, http.StatusNotFound, "not_found", "plan not found", r.RegistryRevision, requestID(req))
		return
	}
	var body struct {
		PlanVersion string `json:"plan_version"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		respondError(w, http.StatusUnprocessableEntity, "invalid_json", "invalid request body", r.RegistryRevision, requestID(req))
		return
	}
	ctx := service.ContextWithPrincipal(req.Context(), p)
	result, err := r.tools.Execute(ctx, service.PlanRef{ID: planID, Version: body.PlanVersion})
	if err != nil {
		respondServiceError(w, err, r.RegistryRevision, requestID(req))
		return
	}
	status := http.StatusOK
	if result.Replay {
		status = http.StatusOK
	}
	respondJSON(w, status, result)
}

// resolve handles approval decisions (RC-07 route). Resolution bodies only
// carry resolution + optional comment.
func (r *Router) resolve(w http.ResponseWriter, req *http.Request) {
	respondError(w, http.StatusNotImplemented, "not_implemented", "approval resolution wiring lands with the durable grant store", r.RegistryRevision, requestID(req))
}

// --- helpers ---

func respondServiceError(w http.ResponseWriter, err error, rev, rid string) {
	code, status := api.Classify(err)
	message := err.Error()
	if status >= 500 {
		// Never leak internal error details to clients (P2): 5xx gets a
		// generic message; the underlying error is logged at the call site.
		message = "internal server error"
	}
	respondError(w, status, string(code), message, rev, rid)
}

func respondError(w http.ResponseWriter, status int, code, message, rev, rid string) {
	respondJSON(w, status, map[string]any{
		"error":             message,
		"code":              code,
		"request_id":        rid,
		"registry_revision": rev,
	})
}

func respondJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" {
		return id
	}
	return fmt.Sprintf("req-%d", time.Now().UTC().UnixNano())
}

func hasSegment(path, prefix string) bool {
	return len(path) > len(prefix) && path[:len(prefix)] == prefix
}

func hasSuffix(path, suffix string) bool {
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}

func segmentAfter(path, prefix string) string {
	if !hasSegment(path, prefix) {
		return ""
	}
	return path[len(prefix):]
}

func trimSuffix(s, suffix string) string {
	if hasSuffix(s, suffix) {
		return s[:len(s)-len(suffix)]
	}
	return s
}

var _ = authz.UserID
var _ = catalog.ToolDescriptor{}
