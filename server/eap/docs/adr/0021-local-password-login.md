# ADR-0021: Built-in local account/password login

Date: 2026-09-02 · Status: accepted · Task: TASK-027

## Context

The primary enterprise identity path is OIDC (Feishu tenant, ADR-0009). Some
operational scenarios — first bootstrap before a tenant is configured, break-
glass access when the IdP is down, small internal deployments — need a login
path that does not depend on an external IdP. The PRD rules out building a
full enterprise IAM.

## Decision

1. A **minimal local credential fallback**: operator-created username/password
   accounts (`local_credentials`, bcrypt-hashed) bound to the canonical
   employee row. Login issues the same control-plane session JWT as every
   other path — one Principal model, one authorization boundary.
2. **Gated by `EAP_LOCAL_AUTH`**; the primary enterprise path remains OIDC.
   Local accounts are created/reset only by an administrator
   (`POST /enterprise/v1/admin/local-accounts`), and every create/reset and
   login success/failure is audited on the reliable path.
3. Unknown user and wrong password are **indistinguishable** (same error,
   same audit shape). Passwords are never stored reversible, never logged,
   and never echoed in responses.
4. Roles/workspaces still come exclusively from `workspace_members`
   (server-side) — the login method grants no authority by itself.

## Consequences

- Not a full IAM: no self-registration, no password self-service recovery,
  no MFA (follow-ups if needed).
- A user may hold both an OIDC subject and a local credential; the identity
  rows reconcile by verified email (ADR-0020).
- Login brute force relies on the gateway's global rate limiter for now;
  per-account throttling is a recorded follow-up.
