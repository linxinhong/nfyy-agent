# Security Adversarial Suite — TASK-047 Verification Report

- **Card**: TASK-047 (Security adversarial suite: authz matrix, leakage scan, DLP bypass corpus)
- **Gate**: `make verify-security-adversarial`
- **Environment**: isolated TASK-047 stack (`eap-verify-pg-047` on :15434, `eap-verify-redis-047` on :16381); real PostgreSQL (all migrations) + real Redis; gateway exercised in-process via the production `Server.Handler()`
- **Suite**: `apps/gateway/security_adversarial_integration_test.go` (`//go:build integration`, package `main_test`, reuses `newSliceEnv`/`mcpLogin`/`seedKey`)
- **Result**: PASS — 61 routes × credential classes = 446 matrix cells asserted (zero skipped), 108 corpus subtests + 3 lifecycle/limit tests green, 3 self-verified DLP findings recorded

## 1. Authorization matrix sweep (`TestAuthzMatrix`)

The endpoint inventory is **generated**, not hand-written: `registeredRoutePatterns` parses every `mux.HandleFunc("METHOD /path", …)` registration out of `apps/gateway/server.go` with `go/ast`. The sweep hard-fails when a registered route has no matrix row (or a row matches nothing registered), so any new surface is swept automatically. This caught two real gaps during development (`device/approve`, `device/deny` were initially unrowed).

Dimensions:

- **Routes**: 61 (every `HandleFunc` registration in `server.go`, including conditional surfaces — all wired in the fixture: OIDC+IdP stub, local auth, CLI dist, Portal static, capability registry, routed LLM plane, knowledge, MCP, device flow, skill signing keys)
- **Credential classes (8)**: `no_auth`, valid `agk_*` key + `X-Agent-Workspace`, workspace-bound key **without** the header (ADR-0024), revoked key, expired key, session JWT (control-plane class), valid key + **foreign** workspace header, plus a row-specific `authorized_actor` (admin session / maintainer session / portal cookie / full OIDC flow)
- **Cells**: 446 asserted, zero skipped. Per-cell expected outcomes follow the frozen contract: 401 (unauthenticated/revoked/expired/wrong class), 403 (cross-workspace where membership revalidation denies; role denial for non-admin/maintainer sessions), 404/other only where the endpoint's documented semantics say so, 2xx for the authorized actor.

Extra fail-closed invariants asserted on top of exact status codes:

- No governed surface returns success for `no_auth`, revoked, or expired credentials (matrix-wide, not spot checks).
- No governed surface returns success for a foreign workspace — with one documented, contract-correct exception: `GET /enterprise/v1/me` is identity-informational (lists the caller's own memberships), never establishes workspace authority, and ignores the header (row marked `wsAgnostic`; it leaks nothing beyond the caller's own data).
- The Portal SPA catch-all answers unknown API paths with the JSON problem shape, never HTML.

Per-row authorized outcomes verified end to end: 201 key create / skill publish / knowledge upload, 204 key delete / skill revoke / logouts, 302 redirects (`/`, OIDC login, OIDC callback with a real IdP round trip), 200 everywhere else. Device-token polling asserts the full approve→poll flow for the authorized class and `authorization_pending`/`expired_token` (400) for every other class. Cookie-scoped Portal profile denies every bearer class (authority is the HttpOnly cookie).

## 2. Credential-leakage scan (`TestCredentialLeakage`)

A full key lifecycle runs while the gateway's **real log stream** is captured (`log.SetOutput` swapped in-process, restored on cleanup) and **every response body** is recorded through a wrapping handler; the generated CLI bootstrap config is part of the traffic corpus.

Coverage: dev-login → API key create → use (`/me`, token exchange, bootstrap config, key list) → the created key pasted into a chat request (deny-class DLP, 422, **zero provider egress** verified against the upstream request log) → device flow (code → approve → token → logout) → revoke → revoked/malformed key use (401).

Assertions:

- `agk_(live|test)_[A-Za-z0-9]{16,}` matches **nowhere** in the log stream; none of the tracked literals (session JWT, created key, planted-never-displayed key, device refresh token, device access JWT) appear in logs, any response body, the bootstrap config, or the provider-bound payload.
- The three documented one-time displays appear **exactly once** each: `key` in the key-create response, `refresh_token` in the device-token response, `session_token` in the dev-login response. After stripping those single fields, the entire body corpus is clean.
- All error bodies carry the frozen Problem shape (`code`+`message`) — or the OpenAI error envelope on `/v1/*` and the OAuth error shape on the device grant endpoint (both frozen client contracts) — and no credential-shaped fields anywhere.
- The durable audit trail stores no credential values (checked over `audit_events.detail` and `resource`).

## 3. DLP bypass corpus (`TestDLPBypassCorpus`)

108 subtests over the seeded rule set (migrations 000006 + 000017 + 000020; the test first re-loads the live rules and hard-fails on name/action drift, so the corpus is derived from real control data). Every case drives the full pipeline (`POST /v1/chat/completions` with a routed provider stub) and asserts on the **provider-bound payload** (egress truth) and the **released response**.

Per rule (`openai_key`, `eap_api_key`, `bearer_token`, `password_field`, `private_key`, `db_conn_string`, `internal_phone`, `internal_ipv4`, `cn_resident_id`, `cn_bank_card`, `cn_uscc`):

- **Canonical + evasive variants the pattern still matches**: minimum/maximum length bounds (20/200 for `sk-`, 16/40 for `agk_`), two occurrences in one message, JSON-embedded values, case variations for `(?i)` rules (`BEARER`, `PASSWD`, `POSTGRESQL://`, `MySQL://`), separator runs inside classes (tab, 8-space run inside `bearer\s{1,32}`), multiline `(?s)` private-key headers (RSA / ENCRYPTED / OPENSSH with surrounding newlines), scheme variants (postgres/postgresql/mysql/mongodb). Deny-class: 422 `dlp_blocked` + **zero** provider requests. Redact-class: 200 + provider sees only `[REDACTED:*]`, never the raw value or any ≥½-length prefix of it.
- **Response side** (provider echoes the matching shape): deny rules block the released response; redact rules rewrite it; zero raw secret or long prefix survives.
- **Chunk-boundary splits through the live SSE sanitizer**: the secret is streamed split at **every byte position** (e.g. `eap_api_key`, case/separator `bearer_token`, checksum-valid `cn_resident_id`), with >holdback padding so the sanitizer releases text between the two upstream chunks. Reassembled deltas + raw SSE bytes contain no secret and no ≥½-length prefix; deny aborts with `[withheld:dlp]`, redact releases only the rewritten form.
- **Tokenize action** (internal_phone toggled via the admin API, then restored): provider receives stable `[internal_phone:<token>]` pseudonyms (vault-backed, batch seam), the audited reveal API restores the original for the owning workspace, and the redact baseline is verified after restore.
- **Checksum negative space (documented design, asserted as design)**: GB 11643 / Luhn / GB 32100 candidates that fail their checksum stay untouched on provider-bound payloads. That is the TASK-030 false-positive contract, not a leak.

## 4. Rate-limit effectiveness (`TestRateLimitEffectiveness`)

At the configured limit (3/min, real Redis counter with the production TTL semantics and a controllable window clock): burst of `limit` requests succeeds, the next request trips **429** with the frozen Problem shape (`code=rate_limited`, `retriable=true`, `trace_id` present); a second key keeps its own window (per-subject enforcement); advancing past the window boundary recovers immediately and the new window enforces again.

## Findings (fix-card candidates — no rules were changed here)

Self-verifying subtests in `TestDLPRuleFindings` (skipped while unfixed; they flip to failing if a fix lands without updating the corpus). **All three fixable findings were closed by TASK-054 — see the addendum below; the subtests are positive coverage now.**

1. **`internal_phone` misses the 3-4-4 separator form** — `138 0013 8000` passes raw; the rule matches only 11 contiguous digits. Common CN formatting makes this a realistic egress shape.
2. **`password_field` misses the JSON quoted-key form** — `"password": "…"` passes raw; the double quote after the key breaks `(password|passwd|pwd)\s{0,32}[:=]`. JSON is the dominant API payload shape, so this is the highest-priority gap.
3. **`openai_key` is case-sensitive on the prefix** — an uppercase `SK-…` (formatter mangling) matches nothing.

Additional observations (lower priority, recorded here only): `db_conn_string` values containing spaces in either credential segment escape `[^\s"]`; any secret split across lines/chunks with inserted characters is out of scope for content rules by design (the SSE sanitizer covers faithful chunk splits, which is the threat the streaming path actually has).

## TASK-054 addendum — evasive-form coverage closed

TASK-054 (migration `000021_dlp_evasive_coverage.sql`, unconditional by-rule-name updates per ADR-0026) fixed the three fixable findings above and converted their subtests from skip-while-unfixed into positive end-to-end coverage. The bypass corpus absorbs the shapes as stricter cases, including chunk-boundary SSE splits for the widened separator phone form. Every changed pattern keeps a finite derived max match; the rule-set maximum stays dominated by the unchanged `db_conn_string` (~1039 bytes), so the SSE holdback and the perf gate are unaffected.

1. **`internal_phone` 3-4-4 separated form** — pattern `\b1[3-9]\d[\s-]?\d{4}[\s-]?\d{4}\b` (max match 11 → **13 bytes**): single space/hyphen separators allowed ONLY at the two fixed digit-group boundaries (`138 0013 8000`, `138-0013-8000`); no free-running digit-space class, so runs split at arbitrary positions (`1 3800138000`) still pass, and the `\b` anchors are preserved. Newly matched benign text: 3-4-4-grouped numbers starting 13-19 (order/reference IDs) — rare, and the action is redact with reversible tokenize/reveal.
2. **`password_field` JSON quoted keys** — pattern `(?i)["']?(password|passwd|pwd)["']?\s{0,32}[:=]\s{0,32}["']?[^\s"']{4,256}` (max match 329 → **332 bytes**): tolerates quoting around the key and an opening quote before the value; the value class excludes quotes so redacted JSON stays well-formed (`{"password": "[REDACTED:password]"}`). Newly matched benign text: quoted or suffixed keys (`"db_password": …`) — semantically password assignments; the action is redact, so a false hit rewrites one token, never blocks.
3. **`openai_key` case-insensitive prefix** — pattern `(?i:sk-)[A-Za-z0-9]{20,200}` (max match **203 bytes**, unchanged): case-insensitivity scoped to the prefix so the bounded body is byte-for-byte unaffected. Newly matched benign text: case-variant `sk-` tokens with ≥20 alphanumerics (e.g. `TASK-<20 alnum>` ticket keys) — rare, deny hits audited; the same collision surface already existed for lowercase input.
4. **`db_conn_string` space-bearing password segment — ACCEPTED LIMITATION** (pattern unchanged). A raw-space password escapes `[^\s"]{1,512}`, but no bounded "word + optional space-run" repetition separates the evasive shape from prose after a `scheme://user:` prefix (`postgres://db:5432 then ping alice@example.com` would become a deny-class hit and hard-block benign operations chat), while the **functional** percent-encoded form (`my%20s3cret`) is already covered (`%` is inside the class) and a raw space is not valid URI userinfo (RFC 3986). A generous bound would also multiply the derived SSE holdback several-fold, against ADR-0026's streaming-liveness rationale. The limitation is self-verifying: `TestDLPRuleFindings/db-conn-password-space-accepted-limitation` skips with the rationale today and flips positive automatically if a future fix lands. Full analysis in `migrations/000021_dlp_evasive_coverage.sql`.

## How to run

```sh
docker run -d --name eap-verify-pg-047 -e POSTGRES_USER=eap -e POSTGRES_PASSWORD=eap -p 15434:5432 postgres:16-alpine
docker run -d --name eap-verify-redis-047 -p 16381:6379 redis:7-alpine
make verify-security-adversarial
```

`EAP_MATRIX_DUMP=<file>` additionally writes the full expected/got cell table as CSV.

## Scope notes

- No real credential material appears in this suite, its output, or this report; all corpus values are synthetic shapes and evidence above masks values to prefixes.
- Exemptions from the matrix: none. Every `HandleFunc` registration is rowed; the static catch-all (`mux.Handle("/")`) is the SPA fallback surface and is asserted via `GET /{$}` plus the JSON-404 API check.
- Infrastructure penetration testing (Docker host, PG auth, TLS) and protocol conformance are out of scope per the card (TASK-042 owns conformance).
