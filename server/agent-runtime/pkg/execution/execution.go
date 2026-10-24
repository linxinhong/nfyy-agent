// Package execution defines the dependency-free identity and scope types
// shared by the runtime core. It intentionally has no imports outside the
// standard library so storage, session, and agent packages can all consume
// it without import cycles.
package execution

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

type namespaceContextKey struct{}

// ContextWithNamespace attaches a namespace resolved by a trusted host
// identity layer. Request bodies, query strings, and headers must never be
// used as its source.
func ContextWithNamespace(ctx context.Context, namespace string) context.Context {
	return context.WithValue(ctx, namespaceContextKey{}, namespace)
}

// NamespaceFromContext returns the namespace installed by a trusted host
// identity layer. The boolean distinguishes a missing namespace (legacy
// compatibility) from an explicitly supplied value.
func NamespaceFromContext(ctx context.Context) (string, bool) {
	namespace, ok := ctx.Value(namespaceContextKey{}).(string)
	return namespace, ok
}

// Principal identifies the actor and the physical partition their work
// belongs to. Namespace is the mandatory partition key; Subject is an
// owner/audit attribute and never becomes a resource key on its own.
// Cross-subject access is decided by an Authorizer, never by omitting the
// namespace.
type Principal struct {
	// Subject is the actor identifier (owner/audit attribute only).
	Subject string `json:"subject,omitempty"`
	// Namespace is the mandatory physical partition key.
	Namespace string `json:"namespace"`
	// Issuer records where the identity came from (audit).
	Issuer string `json:"issuer,omitempty"`
	// AuthzEpoch is a version marker for authorization decisions.
	AuthzEpoch uint64 `json:"authz_epoch,omitempty"`
}

// ExecutionContext correlates a request across runs without carrying
// authority: these IDs only link observability and audit records.
type ExecutionContext struct {
	RequestID     string `json:"request_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	WorkflowRunID string `json:"workflow_run_id,omitempty"`
}

// NamespaceScope explicitly scopes a storage operation to one namespace.
// Managed code paths must never issue unscoped queries.
type NamespaceScope struct {
	Namespace string
}

// ReservedNamespace is the migration-only partition for pre-namespace
// records. Managed principals can never access it.
const ReservedNamespace = "legacy"

// validation limits: control characters and empty values are rejected.
var (
	ErrNamespaceRequired = errors.New("execution: namespace is required")
	ErrNamespaceInvalid  = errors.New("execution: namespace contains control characters")
	ErrNamespaceReserved = errors.New("execution: reserved namespace")
)

// ValidateNamespace canonicalizes nothing: it only rejects empty values,
// control characters, and the reserved "legacy" namespace. It deliberately
// does not case-fold or Unicode-normalize.
func ValidateNamespace(namespace string) error {
	if namespace == "" {
		return ErrNamespaceRequired
	}
	if namespace == ReservedNamespace {
		return ErrNamespaceReserved
	}
	for _, r := range namespace {
		if r < 0x20 || r == 0x7f {
			return ErrNamespaceInvalid
		}
	}
	return nil
}

// ValidNamespace reports whether namespace passes ValidateNamespace.
func ValidNamespace(namespace string) bool {
	return ValidateNamespace(namespace) == nil
}

// String renders the scope for error messages and audit.
func (s NamespaceScope) String() string { return s.Namespace }

// HasNamespace reports whether the scope carries a valid non-reserved
// namespace.
func (s NamespaceScope) HasNamespace() bool { return ValidNamespace(s.Namespace) }

// NormalizedNamespace is a compatibility helper for callers that must
// surface a namespace string; it trims whitespace but performs no
// case/Unicode normalization.
func NormalizedNamespace(namespace string) string {
	return strings.TrimSpace(namespace)
}

// MaxNamespaceRunes bounds namespaces to a sane audit-friendly length.
const MaxNamespaceRunes = 128

// ValidateNamespaceLength rejects over-long namespaces (a cheap guard for
// callers that persist the value).
func ValidateNamespaceLength(namespace string) error {
	if utf8.RuneCountInString(namespace) > MaxNamespaceRunes {
		return errors.New("execution: namespace exceeds max length")
	}
	return nil
}
