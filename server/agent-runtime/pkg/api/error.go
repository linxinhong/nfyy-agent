// Package api holds the public, frozen HTTP-facing contracts: stable error
// codes, their status-code mapping, and the sentinel-error classifier. It is
// the single source of truth for translating internal sentinel errors into
// the wire envelope used by the governed REST surface (RC-07).
package api

import (
	"errors"
	"net/http"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// Code is a stable, machine-readable error code. Codes are frozen at
// RC-07: add new codes only by appending, never renumbering or reusing.
type Code string

const (
	CodeNotFound           Code = "not_found"
	CodeUnknownTool        Code = "unknown_tool"
	CodeForbidden          Code = "forbidden"
	CodeStale              Code = "stale"
	CodeExpired            Code = "expired"
	CodeApprovalRequired   Code = "approval_required"
	CodeResultTooLarge     Code = "result_too_large"
	CodeIdentityRequired   Code = "identity_required"
	CodeInvalidNamespace   Code = "invalid_namespace"
	CodeIdentityNotAllowed Code = "identity_not_allowed"
	CodeInvalidJSON        Code = "invalid_json"
	CodeNotImplemented     Code = "not_implemented"
	CodeConflict           Code = "conflict"
	CodeLeaseHeld          Code = "lease_held"
	CodeInternal           Code = "internal"
)

// Status returns the frozen HTTP status for a code.
func Status(code Code) int {
	switch code {
	case CodeApprovalRequired:
		return http.StatusAccepted
	case CodeIdentityRequired, CodeInvalidNamespace:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeUnknownTool, CodeNotFound:
		return http.StatusNotFound
	case CodeStale, CodeConflict, CodeLeaseHeld:
		return http.StatusConflict
	case CodeExpired:
		return http.StatusGone
	case CodeResultTooLarge, CodeInvalidJSON, CodeIdentityNotAllowed:
		return http.StatusUnprocessableEntity
	case CodeNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// Classify maps a known sentinel error to its stable (code, status). The
// default for an unknown error is CodeInternal with a 500 status; the
// message must never carry internal details to the wire.
func Classify(err error) (Code, int) {
	switch {
	case err == nil:
		return CodeInternal, http.StatusInternalServerError
	case errors.Is(err, service.ErrForbidden), errors.Is(err, service.ErrSelfApproval):
		return CodeForbidden, Status(CodeForbidden)
	case errors.Is(err, service.ErrUnknownTool):
		return CodeUnknownTool, Status(CodeUnknownTool)
	case errors.Is(err, service.ErrPlanNotFound), errors.Is(err, db.ErrNotFound), errors.Is(err, run.ErrAttemptNotFound):
		return CodeNotFound, Status(CodeNotFound)
	case errors.Is(err, service.ErrPlanStale), errors.Is(err, service.ErrGrantStale), errors.Is(err, run.ErrStaleFencingToken):
		return CodeStale, Status(CodeStale)
	case errors.Is(err, service.ErrPlanExpired), errors.Is(err, service.ErrGrantExpired):
		return CodeExpired, Status(CodeExpired)
	case errors.Is(err, service.ErrApprovalRequired):
		return CodeApprovalRequired, Status(CodeApprovalRequired)
	case errors.Is(err, service.ErrResultTooLarge):
		return CodeResultTooLarge, Status(CodeResultTooLarge)
	case errors.Is(err, service.ErrApprovalConflict), errors.Is(err, db.ErrConflict),
		errors.Is(err, db.ErrInvalidTransition), errors.Is(err, run.ErrNotCancellable),
		errors.Is(err, run.ErrInvalidAttemptTransition):
		return CodeConflict, Status(CodeConflict)
	case errors.Is(err, service.ErrIdentityRequired):
		return CodeIdentityRequired, Status(CodeIdentityRequired)
	case errors.Is(err, service.ErrPlanNamespaceMismatch):
		return CodeNotFound, Status(CodeNotFound) // cross-namespace unifies to 404
	case errors.Is(err, execution.ErrNamespaceRequired), errors.Is(err, execution.ErrNamespaceReserved):
		return CodeInvalidNamespace, Status(CodeInvalidNamespace)
	case errors.Is(err, run.ErrLeaseHeld):
		return CodeLeaseHeld, Status(CodeLeaseHeld)
	default:
		return CodeInternal, Status(CodeInternal)
	}
}
