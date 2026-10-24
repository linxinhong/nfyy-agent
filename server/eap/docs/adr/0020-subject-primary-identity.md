# ADR-0020: Subject-primary identity (email optional)

Date: 2026-09-02 · Status: accepted · Task: TASK-023

## Context

The canonical Principal is persisted in `users` with `idp_subject UNIQUE` and
a `NOT NULL` email plus a full unique index on `lower(email)`. Real-IdP
qualification (TASK-013, Feishu) showed that enterprise directories frequently
carry no usable email for a member — the `authen/v1/user_info` email is a
sensitive field gated behind `contact:user.email:readonly`, sourced from
admin-imported directory data, and explicitly not verified by the user.
Requiring it made the real login journey fail closed for perfectly valid
identities.

## Decision

1. The IdP **subject is the primary and unique identity anchor** (`users.idp_subject`).
   For Feishu that is the `union_id` (stable across apps), falling back to
   `open_id` when absent.
2. **Email is an optional profile attribute** (`users.email` nullable; the
   unique index is partial, `WHERE email <> ''`). A first login with no
   directory email JIT-provisions a subject-only employee row.
3. **Email-based linking** (directory reconciliation / IdP migration) applies
   only when the IdP asserts a non-empty email that matches an existing row.
   A provisioned email is never wiped by an empty claim.
4. Workspace authority is unchanged: it flows exclusively through the
   operator-declared IdP-group → workspace mapping (ADR-0009). Availability
   scope of the IdP application remains the onboarding gate.
5. The browser never supplies identity; the subject/email are IdP-asserted
   via server-to-server calls.

## Consequences

- Zero-friction onboarding: any member inside the IdP application's
  availability scope can log in and get a Principal without operator
  pre-binding.
- Admin surfaces must treat an empty email as a display detail (name remains
  the human-readable anchor).
- If a tenant later provisions the email, the next login backfills it; an
  email that matches a different row re-links (one row per human).
- Deletion/GDPR flows key on the subject, not the email.
