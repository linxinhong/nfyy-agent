# ADR-0025: Bounded process-local caches for LLM-path control data

Date: 2026-09-03 · Status: accepted · Task: TASK-036

## Context

The 2026-09 performance review measured the gateway's added chat latency at
p95 17.3ms — well inside the PRD §9 100ms budget — but ~12 serial
PostgreSQL round trips per request, several of them re-reading control
data that changes at most a few times a day: `TablePolicy.Decide` reloaded
the full `policy_rules` set on every decision (no cache at all), and route
plans, provider targets and pricing were queried per request. The DLP
scanner did cache its compiled rule set, but held its mutex across the
reload, so every TTL expiry serialized all concurrent scans behind one
database round trip (a recurring p99 spike).

## Decision

Read-mostly control data on the LLM request path — policy rules, route
plans, provider targets, pricing, and the DLP compiled rule set — is
served from one bounded, process-local, derived cache set with a
**60-second staleness bound**:

- **Fail closed.** A cache load error returns an error to the caller; the
  stale value is never served after a failed load, and the failure is not
  cached — the next request retries. Semantics are identical to the
  pre-cache engines.
- **Single-flight loads.** The cache never holds a lock across a load;
  concurrent requests for the same expired key share one load instead of
  stampeding the database.
- **No per-subject state.** Caches hold only global control data. DLP
  subject/role exemptions are applied per call after the cache, never
  cached (the privilege-leak lesson from the TASK-029 cache poisoning).
- **Derived state only.** Caches are rebuilt from PostgreSQL; there is no
  second source of truth, no persistence, and no admin invalidation API —
  control-plane writes propagate within the TTL bound (≤60s plus one
  in-flight load).
- **Authorization facts stay live.** API-key validity, revocation/expiry
  and workspace membership are resolved on every request and are never
  cached; the staleness bound applies to rule/route/pricing control data
  only.

Operators should expect an admin policy or routing change to reach every
gateway instance within 60 seconds of the write.

## Consequences

- Steady-state routed chat drops ~4 control-data round trips; the 200-load
  counting contract is asserted by `TestControlCachesSteadyStateLoadCounts`
  and TTL-bound propagation by `TestControlCacheRuleChangePropagatesWithinBound`.
- Cached values are shared between requests and treated as immutable; new
  consumers must not mutate returned slices/structs.
- Memory is bounded by control-data size (rules + routes + targets +
  pricing pairs) — trivial against request payloads.

## Revisit triggers

- A compliance requirement demands policy-deny propagation faster than
  ~1s (needs explicit invalidation or a watch mechanism — return to
  architecture).
- Multi-instance deployments need a shared invalidation story beyond TTL.
- Control data grows large enough that full-set reloads are measurable.
