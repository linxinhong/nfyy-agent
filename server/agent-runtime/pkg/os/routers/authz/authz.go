// Package authz contains shared request-level authorization helpers.
package authz

import (
	"context"
	"fmt"
	"net/http"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/middleware"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// UserID returns the authenticated subject when user isolation is enabled.
// The second result reports whether handlers must enforce ownership checks.
func UserID(req *http.Request) (string, bool) {
	if !middleware.UserIsolationFromContext(req.Context()) {
		return "", false
	}
	return middleware.UserIDFromContext(req.Context()), true
}

// MemoryPrincipal returns only a complete principal installed by the verified
// host authentication path. Security-key and legacy-compatible requests have
// no private-memory authority.
func MemoryPrincipal(req *http.Request) (memory.Principal, error) {
	p, err := service.PrincipalFromContext(req.Context())
	if err != nil || p.Subject == "" || p.Namespace == "" || p.Issuer == "" {
		return memory.Principal{}, fmt.Errorf("%w: authenticated memory principal required", memory.ErrUnauthorized)
	}
	return memory.Principal{Subject: p.Subject, Namespace: p.Namespace, Issuer: p.Issuer, AuthzEpoch: p.AuthzEpoch}, nil
}

// Namespace returns the verified tenant namespace. Missing JWT namespace
// claims intentionally use the migration-only legacy partition; callers must
// not treat that fallback as an all-tenant scope.
func Namespace(req *http.Request) (string, bool) {
	namespace, tenant := middleware.NamespaceFromContext(req.Context())
	if !tenant {
		return execution.ReservedNamespace, false
	}
	return namespace, true
}

// Owns reports whether a resource belongs to the isolated request user.
func Owns(req *http.Request, resourceUserID string) bool {
	userID, isolated := UserID(req)
	return !isolated || resourceUserID == userID
}

// DetachedExecutionContext copies only verified principal and namespace into
// a fresh background root. It intentionally drops request cancellation and
// every unrelated HTTP/private context value.
func DetachedExecutionContext(ctx context.Context) context.Context {
	detached := context.Background()
	principal, err := service.PrincipalFromContext(ctx)
	if err != nil {
		return detached
	}
	detached = service.ContextWithPrincipal(detached, principal)
	if execution.ValidNamespace(principal.Namespace) {
		detached = execution.ContextWithNamespace(detached, principal.Namespace)
	}
	return detached
}

// BackgroundExecutionContext narrows strict-memory execution to trusted
// authority only. Legacy routing retains the caller context for compatibility,
// including namespace-only JWTs that predate issuer-bound memory authority.
func BackgroundExecutionContext(ctx context.Context, strictMemory bool) context.Context {
	if !strictMemory {
		return ctx
	}
	return DetachedExecutionContext(ctx)
}
