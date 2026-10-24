package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// Profile selects the runtime construction mode. The zero value is invalid;
// a profile is fixed at startup and must never be switched per request.
type Profile string

const (
	// ProfileLegacyEmbedded preserves the pre-managed embedded behavior:
	// raw tools, loose model providers, unscoped lookups. It must be chosen
	// explicitly and must never be treated as production-safe isolation.
	ProfileLegacyEmbedded Profile = "legacy_embedded"
	// ProfileManaged is the production profile: ToolService-governed
	// execution, trusted identity injection, namespace isolation, and
	// fail-closed defaults. Managed builders reject legacy inputs.
	ProfileManaged Profile = "managed"
)

// Valid reports whether the profile is one of the frozen values.
func (p Profile) Valid() bool {
	return p == ProfileLegacyEmbedded || p == ProfileManaged
}

// ErrInvalidProfile is returned when a zero or unknown profile is used.
var ErrInvalidProfile = errors.New("runtime: profile must be legacy_embedded or managed")

// IdentityMapper maps an inbound request to a trusted Principal. It is the
// only way a Principal enters a managed runtime: HTTP bodies/headers can
// never set identity.
type IdentityMapper interface {
	// PrincipalFor returns the trusted principal for an inbound request.
	PrincipalFor(ctx context.Context, inbound any) (execution.Principal, error)
}

// ManagedModelResolver resolves a ProjectSpec ModelRef to a governed model
// instance (RC-09). Only refs are stored in specs; secrets and arbitrary
// base URLs never are.
type ManagedModelResolver interface {
	Resolve(ctx context.Context, modelRef string) (models.Model, error)
}

// ManagedOptions configure a managed build.
type ManagedOptions struct {
	// Profile must be ProfileManaged. The zero value is invalid.
	Profile Profile
	// IdentityMapper injects trusted principals. Required in managed mode.
	IdentityMapper IdentityMapper
	// ModelResolver resolves model refs through the governed endpoint
	// registry. When set, Dependencies.Models must be empty (managed
	// builds reject loose model providers, RC-09 step 3).
	ModelResolver ManagedModelResolver
}

// BuildManaged assembles a project under the managed profile. Managed
// constraints enforced: profile must be explicitly managed, an identity
// mapper is required, and tool execution must flow through a governed
// ToolService (Dependencies.ToolService, RC-05). Dependencies.Tools still
// supplies model-visible schemas but is never executed directly.
func BuildManaged(spec ProjectSpec, deps Dependencies, opts ManagedOptions) (*Project, error) {
	if opts.Profile != ProfileManaged {
		return nil, fmt.Errorf("%w: BuildManaged requires ProfileManaged, got %q", ErrInvalidProfile, opts.Profile)
	}
	if opts.IdentityMapper == nil {
		return nil, errors.New("runtime: managed build requires an IdentityMapper")
	}
	if deps.ToolService == nil {
		return nil, errors.New("runtime: managed build requires a governed ToolService")
	}
	// P1-2/P1-3: a managed deployment must authenticate and isolate. A
	// disabled auth or isolation means every resource is owner-agnostic
	// (IDOR), so managed builds fail closed instead of defaulting open.
	if !deps.Authorization.Enabled {
		return nil, errors.New("runtime: managed build requires authentication (Authorization.Enabled)")
	}
	if deps.Authorization.SecurityKey == "" && deps.Authorization.JWTVerificationKey == "" && deps.SecurityKey == "" {
		return nil, errors.New("runtime: managed build requires a security key or JWT verification key")
	}
	if !deps.Authorization.UserIsolation {
		return nil, errors.New("runtime: managed build requires UserIsolation (owner-agnostic access is not allowed)")
	}
	if opts.ModelResolver != nil {
		if len(deps.Models) > 0 {
			return nil, errors.New("runtime: managed build with ModelResolver rejects Dependencies.Models")
		}
		resolved := make(map[string]models.Model, len(spec.Agents)+len(spec.Teams))
		for _, agent := range spec.Agents {
			if agent.ModelRef == "" {
				continue
			}
			if _, done := resolved[agent.ModelRef]; done {
				continue
			}
			model, err := opts.ModelResolver.Resolve(context.Background(), agent.ModelRef)
			if err != nil {
				return nil, fmt.Errorf("runtime: managed resolve model %q: %w", agent.ModelRef, err)
			}
			resolved[agent.ModelRef] = model
		}
		for _, team := range spec.Teams {
			if team.ModelRef == "" {
				continue
			}
			if _, done := resolved[team.ModelRef]; done {
				continue
			}
			model, err := opts.ModelResolver.Resolve(context.Background(), team.ModelRef)
			if err != nil {
				return nil, fmt.Errorf("runtime: managed resolve model %q: %w", team.ModelRef, err)
			}
			resolved[team.ModelRef] = model
		}
		deps.Models = resolved
	}
	// ToolService wiring is now complete (RC-05): Dependencies.ToolService is
	// threaded into every agent so tool execution is governed. Identity
	// injection remains at the host/HTTP boundary via the IdentityMapper.
	return Build(spec, deps)
}

// BuildLegacy is the explicit legacy-embedded entry point. It preserves the
// pre-managed behavior and must not be used for production isolation.
func BuildLegacy(spec ProjectSpec, deps Dependencies) (*Project, error) {
	return Build(spec, deps)
}
