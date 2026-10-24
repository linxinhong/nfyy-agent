package service

import (
	"context"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
)

// AuthorizeRequest carries the fresh context of one authorization decision.
// ArgumentsHash is the canonical-JSON hash; the raw arguments never reach
// the authorizer decision path.
type AuthorizeRequest struct {
	Principal        execution.Principal
	Descriptor       catalog.ToolDescriptor
	ArgumentsHash    string
	ExecutionContext execution.ExecutionContext
	IdempotencyKey   string
}

// AuthorizationDecision is the per-request decision produced by a
// ToolAuthorizer. Decisions are never cached: every Prepare and Execute
// re-authorizes (D-5, RC-05 step 5).
type AuthorizationDecision struct {
	Allowed    bool
	ReasonCode string

	// AuthorizationPolicyRevision identifies the policy version that
	// produced this decision; it is frozen into plans and grants.
	AuthorizationPolicyRevision string

	// ApprovalRequired/ApprovalKind declare the approval posture.
	ApprovalRequired bool
	ApprovalKind     string

	// MaxTimeoutMS/MaxResultBytes tighten execution limits for this call.
	// Zero means no additional authorization-specific limit; negative values
	// are invalid and fail closed before the executor is invoked.
	MaxTimeoutMS   int64
	MaxResultBytes int64
}

// ToolAuthorizer decides discovery and execution per request. The zero
// decision (Allowed=false) fails closed. Authorizer errors abort the
// request before any side effect.
type ToolAuthorizer interface {
	// AuthorizeDiscovery gates catalog visibility. It must not be replaced
	// by an execution authorization with empty arguments.
	AuthorizeDiscovery(ctx context.Context, principal execution.Principal, descriptor catalog.ToolDescriptor) (AuthorizationDecision, error)
	// Authorize gates one Prepare or Execute.
	Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizationDecision, error)
}

// DenyAllAuthorizer fails closed by default.
type DenyAllAuthorizer struct{}

func (DenyAllAuthorizer) AuthorizeDiscovery(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: false, ReasonCode: "deny_all"}, nil
}
func (DenyAllAuthorizer) Authorize(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: false, ReasonCode: "deny_all"}, nil
}

// ErrApprovalRequired is returned when a plan needs approval before
// execution and none has been granted (RC-06 wires grants; until then
// Execute refuses).
var ErrApprovalRequired = errors.New("service: approval required")

// ErrIdentityRequired is returned when no trusted principal is present.
var ErrIdentityRequired = errors.New("service: trusted principal is required")

// ErrResultTooLarge is returned when a result exceeds the configured limit.
var ErrResultTooLarge = errors.New("service: tool result exceeds size limit")

// ErrInvalidExecutionLimit is returned when an authorizer or service
// supplies a negative or otherwise unrepresentable execution limit.
var ErrInvalidExecutionLimit = errors.New("service: invalid tool execution limit")

type principalKey struct{}

// ContextWithPrincipal injects the trusted principal into a context. Only
// the host identity layer may call this; HTTP bodies can never set it.
func ContextWithPrincipal(ctx context.Context, principal execution.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalFromContext returns the trusted principal, or an error when
// absent or when the namespace is invalid for managed execution.
func PrincipalFromContext(ctx context.Context) (execution.Principal, error) {
	p, ok := ctx.Value(principalKey{}).(execution.Principal)
	if !ok {
		return execution.Principal{}, ErrIdentityRequired
	}
	if err := execution.ValidateNamespace(p.Namespace); err != nil {
		return execution.Principal{}, err
	}
	return p, nil
}

// Clock abstracts time for expiry testing.
type Clock func() time.Time

// Now returns the current UTC time.
func Now() time.Time { return time.Now().UTC() }

// ResultLimits caps tool result sizes (RC-03A step 6).
type ResultLimits struct {
	// MaxBytes limits the serialized result; zero uses the default 4 MiB.
	MaxBytes int64
	// MaxDepth limits nested structure depth; zero uses the default 16.
	MaxDepth int
}

func (l ResultLimits) withDefaults() ResultLimits {
	if l.MaxBytes == 0 {
		l.MaxBytes = 4 << 20
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = 16
	}
	return l
}
