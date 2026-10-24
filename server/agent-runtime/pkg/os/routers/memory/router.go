// Package memory exposes the strict project-scoped Memory Module over HTTP.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	mem "github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/authz"
)

const maxMemoryHTTPBody = 64 << 10

type Config struct {
	Module         *mem.Module
	ProjectID      string
	CollectionID   string
	ProposeEnabled bool
	SourceResolver mem.ProposalSourceResolver
}

type Router struct {
	cfg Config
	mux *http.ServeMux
}

// NewRouter never accepts a raw DB. A nil Module is an explicit disabled route
// returning unavailable, preventing a compatibility fallback.
func NewRouter(cfg Config) http.Handler {
	r := &Router{cfg: cfg, mux: http.NewServeMux()}
	r.mux.HandleFunc("GET /{$}", r.list)
	r.mux.HandleFunc("POST /{$}", r.create)
	r.mux.HandleFunc("POST /operations", r.issueToken)
	r.mux.HandleFunc("POST /forget", r.forget)
	r.mux.HandleFunc("POST /propose", r.propose)
	r.mux.HandleFunc("GET /{id}", r.read)
	r.mux.HandleFunc("PATCH /{id}", r.revise)
	r.mux.HandleFunc("DELETE /{id}", r.retract)
	r.mux.HandleFunc("POST /{id}/confirm", r.confirm)
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.cfg.Module == nil {
		respondError(w, http.StatusServiceUnavailable, "unavailable", "strict memory is not configured")
		return
	}
	r.mux.ServeHTTP(w, req)
}

func (r *Router) authority(req *http.Request) (mem.Principal, mem.Scope, error) {
	p, err := authz.MemoryPrincipal(req)
	if err != nil {
		return mem.Principal{}, mem.Scope{}, err
	}
	scope := mem.Scope{Namespace: p.Namespace, ProjectID: r.cfg.ProjectID, SubjectID: p.Subject, CollectionID: r.cfg.CollectionID}
	if err := scope.Validate(); err != nil {
		return mem.Principal{}, mem.Scope{}, fmt.Errorf("%w: server memory scope", mem.ErrUnavailable)
	}
	return p, scope, nil
}

func (r *Router) issueToken(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	var body IssueTokenRequest
	if !decodeStrict(w, req, &body) {
		return
	}
	var token string
	var err error
	switch body.Action {
	case mem.ActionForgetSubject:
		token, err = r.cfg.Module.IssueForgetToken(req.Context(), p, scope.Subject(), time.Hour)
	case mem.ActionCreate, mem.ActionRevise, mem.ActionRetract, mem.ActionPropose, mem.ActionConfirm:
		token, err = r.cfg.Module.IssueOperationToken(req.Context(), p, scope, body.Action, time.Hour)
	default:
		respondError(w, http.StatusUnprocessableEntity, "invalid_request", "unsupported HTTP memory action")
		return
	}
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{"operation_token": token, "expires_in_seconds": 3600})
}

func (r *Router) create(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	var body CreateMemoryRequest
	if !decodeStrict(w, req, &body) {
		return
	}
	retention, err := httpRetention(body.RetentionMS)
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	record, err := r.cfg.Module.Create(req.Context(), p, scope, mem.CreateRequest{
		Content: body.Content, Topics: body.Topics, Kind: body.Kind,
		Retention: retention, OperationToken: req.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondRecord(w, http.StatusCreated, record)
}

func (r *Router) read(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	record, err := r.cfg.Module.Read(req.Context(), p, scope, req.PathValue("id"))
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondRecord(w, http.StatusOK, record)
}

func (r *Router) list(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	for key := range req.URL.Query() {
		if key != "limit" && key != "cursor" {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request", "unsupported memory query field")
			return
		}
	}
	limit := 0
	if raw := req.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			respondError(w, http.StatusUnprocessableEntity, "invalid_request", "invalid list limit")
			return
		}
		limit = parsed
	}
	page, err := r.cfg.Module.ListPage(req.Context(), p, scope, mem.ListPageRequest{Limit: limit, Cursor: req.URL.Query().Get("cursor")})
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, page)
}

func (r *Router) revise(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	var body ReviseMemoryRequest
	if !decodeStrict(w, req, &body) {
		return
	}
	retention, err := httpRetention(body.RetentionMS)
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	record, err := r.cfg.Module.Revise(req.Context(), p, scope, mem.ReviseRequest{
		MemoryID: req.PathValue("id"), ExpectedRevision: body.ExpectedRevision, Content: body.Content, Topics: body.Topics,
		Retention: retention, OperationToken: req.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondRecord(w, http.StatusOK, record)
}

func (r *Router) retract(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	revision, err := expectedRevision(req)
	if err != nil {
		respondError(w, http.StatusPreconditionRequired, "precondition_required", err.Error())
		return
	}
	record, err := r.cfg.Module.RetractRecord(req.Context(), p, scope, mem.RetractRequest{MemoryID: req.PathValue("id"), ExpectedRevision: revision, OperationToken: req.Header.Get("Idempotency-Key")})
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondRecord(w, http.StatusOK, record)
}

func (r *Router) forget(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	if req.ContentLength != 0 {
		var empty struct{}
		if !decodeStrict(w, req, &empty) {
			return
		}
	}
	generation, err := r.cfg.Module.ForgetSubject(req.Context(), p, scope.Subject(), req.Header.Get("Idempotency-Key"))
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"subject_generation": generation})
}

func (r *Router) propose(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	if !r.cfg.ProposeEnabled || !mem.ProposalSourceResolverAvailable(r.cfg.SourceResolver) {
		respondError(w, http.StatusServiceUnavailable, "unavailable", "memory proposal is not configured")
		return
	}
	var body ProposeMemoryRequest
	if !decodeStrict(w, req, &body) {
		return
	}
	result, err := r.cfg.Module.ProposeResolved(req.Context(), p, scope, body.SourceRef, req.Header.Get("Idempotency-Key"), r.cfg.SourceResolver)
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, result)
}

func (r *Router) confirm(w http.ResponseWriter, req *http.Request) {
	p, scope, ok := r.requireAuthority(w, req)
	if !ok {
		return
	}
	var body ConfirmMemoryRequest
	if !decodeStrict(w, req, &body) {
		return
	}
	record, err := r.cfg.Module.Confirm(req.Context(), p, scope, mem.ConfirmRequest{
		MemoryID: req.PathValue("id"), ExpectedRevision: body.ExpectedRevision,
		ReplacedMemoryID: body.ReplacedMemoryID, ExpectedReplacedRevision: body.ExpectedReplacedRevision,
		SourceRef: body.SourceRef, SourceEpoch: body.SourceEpoch, OperationToken: req.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		respondMemoryError(w, err)
		return
	}
	respondRecord(w, http.StatusOK, record)
}

func (r *Router) requireAuthority(w http.ResponseWriter, req *http.Request) (mem.Principal, mem.Scope, bool) {
	p, scope, err := r.authority(req)
	if err != nil {
		if errors.Is(err, mem.ErrUnauthorized) {
			respondError(w, http.StatusUnauthorized, "identity_required", "authenticated memory identity is required")
		} else {
			respondMemoryError(w, err)
		}
		return mem.Principal{}, mem.Scope{}, false
	}
	return p, scope, true
}

func httpRetention(milliseconds int64) (time.Duration, error) {
	if milliseconds < 0 || milliseconds > math.MaxInt64/int64(time.Millisecond) {
		return 0, fmt.Errorf("%w: retention_ms", mem.ErrInvalid)
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func decodeStrict(w http.ResponseWriter, req *http.Request, dst any) bool {
	decoder := json.NewDecoder(io.LimitReader(req.Body, maxMemoryHTTPBody+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		respondError(w, http.StatusUnprocessableEntity, "invalid_json", "request body is invalid")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		respondError(w, http.StatusUnprocessableEntity, "invalid_json", "request body must contain one JSON value")
		return false
	}
	return true
}

func expectedRevision(req *http.Request) (uint64, error) {
	raw := req.Header.Get("If-Match")
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, errors.New("If-Match revision is required")
	}
	revision, err := strconv.ParseUint(raw[1:len(raw)-1], 10, 64)
	if err != nil || revision == 0 {
		return 0, errors.New("If-Match revision is invalid")
	}
	return revision, nil
}

func respondRecord(w http.ResponseWriter, status int, record *mem.Record) {
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", record.Revision))
	respondJSON(w, status, record)
}

func respondMemoryError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, mem.ErrUnauthorized):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, mem.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, mem.ErrInvalid):
		status, code = http.StatusUnprocessableEntity, "invalid_request"
	case errors.Is(err, mem.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, mem.ErrExpired):
		status, code = http.StatusGone, "expired"
	case errors.Is(err, mem.ErrQuota):
		status, code = http.StatusTooManyRequests, "quota_exceeded"
	case errors.Is(err, mem.ErrOverloaded):
		status, code = http.StatusTooManyRequests, "overloaded"
	case errors.Is(err, mem.ErrCommitUnknown):
		status, code = http.StatusServiceUnavailable, "commit_unknown"
	case errors.Is(err, mem.ErrUnavailable), errors.Is(err, mem.ErrAudit), errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusServiceUnavailable, "unavailable"
	}
	message := err.Error()
	if status >= 500 {
		message = "memory service unavailable"
	}
	respondError(w, status, code, message)
}

func respondJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func respondError(w http.ResponseWriter, status int, code, message string) {
	respondJSON(w, status, map[string]string{"code": code, "error": message})
}

var _ http.Handler = (*Router)(nil)
