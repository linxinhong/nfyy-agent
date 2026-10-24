# ADR-0024: Workspace-bound API keys — one-credential agent onboarding

Date: 2026-09-03 · Status: accepted · Task: TASK-035

## Context

Every scoped EAP request needs server-validated Workspace authority. The only
selection mechanism today is the client-sent `X-Agent-Workspace` header; a
missing header fails closed with 403 `workspace_forbidden`
(`identity.SelectWorkspace`).

Real agent clients frequently cannot inject custom headers. A live failure:
the Hermes agent pointed at the gateway with a valid `agk_*` key was denied on
every call because its provider config silently ignores per-provider default
headers — authentication and membership were fine, only the selection was
missing. Minimal OpenAI SDK wrappers and many CI jobs have the same shape.

## Decision

1. **API keys gain an optional server-side Workspace binding** (nullable
   `api_keys.workspace_id`). The binding is established only at creation:
   `POST /enterprise/v1/keys` accepts an optional `workspace`, and the gateway
   verifies the creator's membership server-side before binding. Binding to a
   workspace the creator is not a member of is rejected (fail closed).
2. **Selection precedence: explicit header → key binding → fail closed.** The
   header still selects context when present; the binding is the default when
   it is absent. Unbound keys behave exactly as before.
3. **The binding nominates, membership authorizes.** Every request still runs
   the membership resolver, so removing a user from a Workspace invalidates
   bound keys immediately. `WorkspaceContext.Source` stays
   `server_membership`; the binding is a selection default, not a new
   authority source. "Client headers only select context" is unchanged — the
   binding is server-side data, which is precisely why it may default.
4. **Gateway edge only.** Internal JWT semantics, downstream services, and the
   CLI device credential (which has its own workspace selection flow) are
   unchanged. Audit records carry the effective workspace; key-bound
   selection is recorded with origin `key_binding` so selection stays
   traceable.

## Rejected alternatives

- **User-level default workspace** — changing the default would silently
  re-context every header-less client; key-level binding is deterministic and
  per-credential.
- **Strict pinned keys** (header must equal binding or 403) — valuable for
  single-workspace CI credentials, but unnecessary for onboarding; recorded as
  a follow-up, not V1 behavior.

## Consequences

- Agent clients such as Hermes need only `base_url` + `api_key`.
- CI keys become naturally least-privilege: bound to one Workspace, while any
  header override remains membership-gated.
- Schema change is additive (nullable column); existing keys and upgrade paths
  are unaffected.

## Follow-ups recorded

Pinned-key strict mode; Portal key-creation UI field; exposing binding in key
listing views (done with this ADR) and audit dashboards.
