package main

// Control-data caches for the LLM request path (TASK-036, ADR-0025):
// policy rules, route plans, provider targets and pricing are read-mostly
// control tables that were re-queried on every request (~4 round trips of
// the gateway-added p95). They are served here from one bounded process-
// local cache set with a 60s staleness bound; admin control-plane writes
// propagate within that bound. The caches are derived state — never a
// second source of truth — and a load failure fails closed and is retried
// on the next request. Per-request authorization facts (API-key validity,
// revocation, workspace membership) are deliberately NOT cached.
//
// The wiring surface is exported so main_test can mirror the production
// wiring exactly (perf-gate honesty) and wrap the loaders with counters.

import (
	"context"
	"time"

	"enterprise-agent-platform/packages/controlcache"
	"enterprise-agent-platform/packages/governance"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/store"
)

// ControlCacheTTL is the documented staleness bound for every control-data
// cache (ADR-0025).
const ControlCacheTTL = time.Minute

// ControlSources names the loadable control-data reads so tests can wrap
// them with counters while production wires them straight to the store.
type ControlSources struct {
	PolicyRules policy.RulesLoader
	Plan        func(ctx context.Context, alias string) (llmkernel.RoutePlanView, error)
	Target      func(ctx context.Context, provider string) (llmkernel.ProviderTarget, error)
	Pricing     func(ctx context.Context, alias, provider string) (store.PricingLookup, error)
}

// ControlSourcesFromStore wires the loads straight to the control tables.
func ControlSourcesFromStore(st *store.Store) ControlSources {
	return ControlSources{
		PolicyRules: st.LoadPolicyRules,
		Plan:        st.RoutePlanFor,
		Target:      st.ResolveProviderTarget,
		Pricing:     st.PricingLookupFor,
	}
}

// ControlCaches owns one cache per control-data shape. Cached values are
// shared between requests and treated as immutable.
type ControlCaches struct {
	sources ControlSources

	policy  *controlcache.Cache[[]policy.TableRule]
	plans   *controlcache.Cache[llmkernel.RoutePlanView]
	targets *controlcache.Cache[llmkernel.ProviderTarget]
	pricing *controlcache.Cache[store.PricingLookup]
}

func NewControlCaches(ttl time.Duration, src ControlSources) *ControlCaches {
	ttlFn := func() time.Duration { return ttl }
	return &ControlCaches{
		sources: src,
		policy:  controlcache.New[[]policy.TableRule](ttlFn),
		plans:   controlcache.New[llmkernel.RoutePlanView](ttlFn),
		targets: controlcache.New[llmkernel.ProviderTarget](ttlFn),
		pricing: controlcache.New[store.PricingLookup](ttlFn),
	}
}

// PolicyRules returns the cached loader for policy.TablePolicy.
func (c *ControlCaches) PolicyRules() policy.RulesLoader {
	return func(ctx context.Context) ([]policy.TableRule, error) {
		return c.policy.Get(ctx, "all", c.sources.PolicyRules)
	}
}

// ResolvePlan returns the cached router plan seam.
func (c *ControlCaches) ResolvePlan() func(ctx context.Context, alias string) (llmkernel.RoutePlanView, error) {
	return func(ctx context.Context, alias string) (llmkernel.RoutePlanView, error) {
		return c.plans.Get(ctx, alias, func(ctx context.Context) (llmkernel.RoutePlanView, error) {
			return c.sources.Plan(ctx, alias)
		})
	}
}

// ResolveTarget returns the cached provider target seam.
func (c *ControlCaches) ResolveTarget() func(ctx context.Context, provider string) (llmkernel.ProviderTarget, error) {
	return func(ctx context.Context, provider string) (llmkernel.ProviderTarget, error) {
		return c.targets.Get(ctx, provider, func(ctx context.Context) (llmkernel.ProviderTarget, error) {
			return c.sources.Target(ctx, provider)
		})
	}
}

// PricingKnown returns the cached pricing seam in the ledger's shape.
func (c *ControlCaches) PricingKnown() func(ctx context.Context, alias, provider string) (governance.Pricing, bool, int, error) {
	return func(ctx context.Context, alias, provider string) (governance.Pricing, bool, int, error) {
		lookup, err := c.pricing.Get(ctx, alias+"@"+provider, func(ctx context.Context) (store.PricingLookup, error) {
			return c.sources.Pricing(ctx, alias, provider)
		})
		if err != nil {
			return governance.Pricing{}, false, 0, err
		}
		return lookup.Pricing, lookup.Known, lookup.Version, nil
	}
}
