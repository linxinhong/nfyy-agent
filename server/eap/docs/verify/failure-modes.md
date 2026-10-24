# Failure-mode & degradation matrix

TASK-046 (L3) · 2026-09-04 · dev box (Apple Silicon), isolated qualification
containers `eap-verify-pg-046` (PostgreSQL 16, port 15433) and
`eap-verify-redis-046` (Redis 7, port 16380), real handler chain, provider
stubbed. Reproduce: `make verify-failure-modes`.

Isolation: the suite fault-injects ONLY its own containers (docker
stop/start of the two above). The tests refuse to run when
`EAP_TEST_DATABASE_URL` / `EAP_TEST_REDIS_ADDR` do not point at ports
15433/16380, so the shared dev stack (15432/16379) and sibling task
containers can never be fault-injected.

## Degradation matrix

Every scenario is one test in `apps/gateway/failure_modes_integration_test.go`
(build tag `integration`). Faults are injected at the infrastructure boundary
only — container stop/start, connection-killing provider stubs, a failing
audit sink — never by mocking the gateway's own layers. After every scenario
the harness drains and asserts zero goroutine growth and a settled pgxpool
(no acquired connections stuck, no total-connection growth).

| # | Scenario | Fault injection point | Contracted behavior | Observed | Result |
|---|---|---|---|---|---|
| 1 | PostgreSQL unavailable | `docker stop eap-verify-pg-046` mid-traffic | Requests fail CLOSED with a DISTINCT storage-error class (500 `internal` / "credential lookup failed"), never the unknown-key 401 (`unauthenticated` / "invalid or unusable API key"); the security-audit delivery failure is logged loudly | Valid key: 500 + `credential lookup failed`; healthy contrast: unknown key 401 audited with `reason=unknown_key`; log contains `gateway: SECURITY AUDIT delivery failure`; after container restart pgxpool auto-reconnects and serves 200 | PASS |
| 2 | Redis unavailable | `docker stop eap-verify-redis-046` mid-traffic | Rate limiting fails OPEN (abuse protection, not authorization); everything else (auth chain, membership, audit) keeps working; the failure is logged | Healthy: 2x 200 then 429 (limiter enforces). Redis down: 3x 200 beyond the limit + log `gateway: rate limiter unavailable (fail open)`. After restart (+ counter flush): 2x 200 then 429 again | PASS |
| 3 | Provider failover exhausted | Both routed providers fail (accept-then-500 and connection-kill stubs) | Deterministic, EAP-wrapped `upstream_unavailable` error (502), no provider credential or URL material in the error body; failed attempts recorded in audit; no usage row for a request that never selected a route | Both routes walked in plan order on every request; identical code+message on repeat requests; body free of the env-resolved provider credential and of any `127.0.0.1`/stub URL material; `llm.request` audit row with `attempts=2`, `error_code=upstream_unavailable`, `outcome=denied`; `policy.decision` recorded; zero `usage_records` | PASS |
| 4 | Provider dies mid-stream | SSE stub streams two deltas, then aborts the connection (`panic(http.ErrAbortHandler)`) without finish/DONE frames | Client sees a TERMINAL SSE error frame, then `[DONE]`; failure recorded in audit; no fabricated usage for the dead stream | Stream committed (200 + deltas), then chunk with `finish_reason:"error"` carrying `[stream error]`, then `data: [DONE]`; `llm.request` audit row with `error_code=upstream_unavailable` + `policy.decision` recorded; zero `usage_records` (ADR-0027: on a routed error the audit set carries the outcome) | PASS |
| 5 | Audit delivery failure | Reliable audit sink forced to reject deliveries (`WriteGovernanceBatch` fails) | Answer withheld: 500 "request could not be audited" — an unauditable request is never released | 500 + "could not be audited", response carries no provider answer; all-or-nothing batch leaves zero `usage_records`; with delivery recovered the same key is served again and the ledger records exactly one row | PASS |

## Drain & stability evidence

After each scenario the harness samples until the goroutine count settles
(bounded poll, two consecutive non-increasing samples, 8s cap) and asserts:

- no goroutine growth past a +10 tolerance over the post-warmup baseline;
- `pgxpool.Stat().AcquiredConns() == 0` after drain (no stuck checkouts);
- `pgxpool.Stat().TotalConns()` within +4 of baseline (no pool growth).

Observed on all five scenarios: settled, no growth, pool clean. The
PG-unavailable scenario additionally proves pgxpool auto-reconnection: after
the container restart the same pool serves 200 without reconstruction.

## Findings (fix-card candidates, not absorbed here)

1. **Mid-stream death decision race (protocols.go, low likelihood).** In
   `handleChatStream`, when the provider dies mid-stream the router's stream
   goroutine delivers the error on `errCh` (buffered) and then closes BOTH
   `errCh` and `chunks`. If the handler happens to be between chunk
   deliveries (not parked on its `select`) when both become ready, Go picks
   between the ready cases at random: the chunks-closed branch treats the
   death as a clean completion — no terminal error frame, outcome audited as
   `allowed`, and a zero-usage `usage_records` row written for a stream that
   never completed. The suite's stub paces deltas so the handler is parked on
   its select at death time (deterministic happy path of the contract); the
   racy interleaving remains reachable under real timing. Same pattern exists
   in `handleResponsesStream` / `handleMessagesStream` (`!alive` treated as
   completion). Suggested fix direction: after receiving a closed `chunks`,
   drain `errCh` before treating the stream as complete (or close `chunks`
   only after the error is known-consumed). Recommend a fix card.

2. **Usage accounting for interrupted streams (by design, flagged for the
   ledger owners).** Per ADR-0027's documented rule ("on a routed error the
   answer is never written and no usage row exists; the audit set carries the
   outcome"), tokens actually consumed before a mid-stream death are not
   billed and no partial-usage row exists. The suite asserts the
   no-fabrication side of that rule. If the platform ever wants
   partial-stream usage, that is a contract change, not a defect.

## Non-findings verified along the way

- A graceful Redis restart persists an RDB snapshot (SIGTERM save), so a
  rate-limit counter can survive a restart. The limiter contract is about
  availability (fail open), not counter durability; the suite flushes the
  counter after restart to prove the limiter reads Redis again rather than
  being stuck fail-open.
- pgxpool reconnects transparently after a full PostgreSQL container
  restart; no request-path intervention needed.
