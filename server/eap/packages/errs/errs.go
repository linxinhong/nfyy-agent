// Package errs defines the EAP public error contract: one frozen problem
// schema, the canonical error codes, and their HTTP mapping. Every gateway and
// capability service reports failures with this schema so clients and audit
// pipelines see one vocabulary.
//
// Enforcement layers must fail closed: when the Policy, DLP or Workspace
// deciding engine itself errors, the request is denied using the fail-closed
// constructors below. An engine failure is never an allow.
package errs

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is the canonical, versioned error code (ADR-0001). Values are part of
// the public contract and must not be renamed or removed.
type Code string

const (
	InvalidArgument     Code = "invalid_argument"
	Unauthenticated     Code = "unauthenticated"
	PermissionDenied    Code = "permission_denied"
	WorkspaceForbidden  Code = "workspace_forbidden"
	NotFound            Code = "not_found"
	Conflict            Code = "conflict"
	RateLimited         Code = "rate_limited"
	QuotaExceeded       Code = "quota_exceeded"
	PolicyDenied        Code = "policy_denied"
	DLPBlocked          Code = "dlp_blocked"
	UpstreamUnavailable Code = "upstream_unavailable"
	Internal            Code = "internal"
)

// Problem is the frozen public error body, mirrored by
// contracts/schemas/v1/error/problem.example.json.
type Problem struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	TraceID   string `json:"trace_id,omitempty"`
	Retriable bool   `json:"retriable"`
}

// Error carries a Problem plus an unexported cause chain. The cause is for
// logs and tests only and is never serialized to clients.
type Error struct {
	Problem
	cause error
}

// New builds a terminal *Error with the given code and message.
func New(code Code, message string) *Error {
	return &Error{Problem: Problem{Code: code, Message: message}}
}

// Wrap preserves cause while attaching a Problem. Empty messages fall back to
// the cause text so context is not lost.
func Wrap(cause error, code Code, message string) *Error {
	if message == "" {
		message = "request failed"
	}
	return &Error{Problem: Problem{Code: code, Message: message}, cause: cause}
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.Problem.Message, e.cause)
	}
	return e.Problem.Message
}

func (e *Error) Unwrap() error { return e.cause }

// WithTraceID attaches the request trace id and returns the same error.
func (e *Error) WithTraceID(traceID string) *Error {
	e.Problem.TraceID = traceID
	return e
}

// AsRetriable marks the failure as safe to retry and returns the same error.
func (e *Error) AsRetriable() *Error {
	e.Problem.Retriable = true
	return e
}

// From extracts the Problem carried by an *Error anywhere in the chain.
func From(err error) (Problem, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Problem, true
	}
	return Problem{}, false
}

// HTTPStatus is the frozen code-to-HTTP-status mapping (ADR-0001). Unknown
// codes map to 500 so new codes cannot silently leak as 200.
func (c Code) HTTPStatus() int {
	switch c {
	case InvalidArgument:
		return http.StatusBadRequest
	case Unauthenticated:
		return http.StatusUnauthorized
	case PermissionDenied, WorkspaceForbidden, PolicyDenied:
		return http.StatusForbidden
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case DLPBlocked:
		return http.StatusUnprocessableEntity
	case RateLimited, QuotaExceeded:
		return http.StatusTooManyRequests
	case UpstreamUnavailable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// PolicyFailClosed denies the request because the policy engine could not
// reach a decision. Retriable is false: callers may retry at a higher level,
// but this response must never be interpreted as an allow.
func PolicyFailClosed(reason string) *Error {
	return New(PolicyDenied, "policy decision unavailable, request denied (fail closed): "+reason)
}

// DLPFailClosed denies the request because outbound content could not be
// inspected. Uninspected content never leaves the boundary.
func DLPFailClosed(reason string) *Error {
	return New(DLPBlocked, "DLP scan unavailable, content blocked (fail closed): "+reason)
}

// WorkspaceFailClosed denies the request because workspace authority could not
// be established server-side.
func WorkspaceFailClosed(reason string) *Error {
	return New(WorkspaceForbidden, "workspace authority unavailable, request denied (fail closed): "+reason)
}
