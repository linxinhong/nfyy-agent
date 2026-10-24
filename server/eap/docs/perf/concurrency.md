# Gateway concurrency profile

TASK-041 gate · 2026-09-03 · dev box (Apple Silicon, local Docker
PostgreSQL/Redis), full governance ON, provider stubbed at ~1ms.
Reproduce: `make perf-gateway` (budget) and `make perf-concurrency-race`
(race gate). Numbers are environment-relative: the saturation ceiling is
the local PG commit rate, not a production number.

## Budget profile (PRD §9 assertion)

50 concurrent users, 200ms agent-like pacing (~230 rps offered):

| stage | p50 | p95 | p99 |
|---|---|---|---|
| before compaction (est. from TASK-033-era path: ~12 serial round trips, 2-3 audit commits, uncached control data) | n/a — never measured under concurrency; the sequential full-chain p95 was 17.3ms | expected >100ms | n/a |
| after TASK-036..041 | 17-28ms | 51-92ms | 58-115ms |

Hard gate: p95 < 100ms (PRD §9) at 50 concurrent users, two-attempt
semantics for dev-box VM jitter.

## Saturation profiles (recorded, not asserted)

Closed loops with no pacing — these measure the dev environment's
durability ceiling (PG commit rate with one fsync-commit per request
after the TASK-041 governance batch). Test pool aligned to production
sizing (32 connections) from the TASK-040 wiring:

| clients | n | p50 | p95 | p99 |
|---|---|---|---|---|
| 50 | 1000 | ~45ms | ~90ms | ~110ms |
| 200 | 1000 | ~156ms | ~198ms | ~205ms |
| 1000 | 2000 | ~852ms | ~1048ms | ~1096ms |

Little's law check at 1000 in-flight: throughput ≈ 1000/0.852s ≈ **1170
rps**. Latency at saturation scales as concurrency/throughput; the
gateway stays race-clean and goroutine-stable at 1000 in-flight.

### Saturation attribution (2026-09-03 diagnostic)

`GATE_DIAG_SYNC_OFF=1` runs the same profiles with
`synchronous_commit=off` (diagnostic only — audit reliability is a hard
boundary in production):

| profile | fsync on (p50) | fsync off (p50) | commit-share |
|---|---|---|---|
| 50 sat | 36ms | 29ms | ~20% |
| 200 sat | 152ms | 121ms | ~20% |
| 1000 sat | 654ms | 509ms | ~22% |

The durable commit is NOT the dominant cost: ~80% of saturation latency
is read queries (~8 sequential round trips per request) contending for
the 32-connection pool against a virtualized local PostgreSQL. On
production infrastructure (native PG, no VM boundary, NVMe), per-query
latency drops several-fold and single-instance capacity projects to
5-12k rps; the structural levers beyond that are connection-count tuning,
further read-path compaction, and stateless gateway scale-out.

## What 1000 concurrent *users* means (production view)

A closed-loop user is ~1 rps; 1000 users at agent-like pacing offer
~1000 rps. Capacity per gateway instance ≈ PG commit rate (1 commit per
request after ADR-0027) + ~6-8 cached-path reads. A production PG
(Linux/NVMe, group commit) sustains 5-20k commits/s, so 1000 users sit
at 5-20% of a single-instance ceiling and the PRD p95 budget holds with
queue depth near zero. Scale-out is linear: gateway instances are
stateless (process-local warm caches, all control data in PostgreSQL);
PostgreSQL becomes the next ceiling, addressed via commit-rate tuning
or read replicas — an infrastructure decision, out of repo scope.

## Sequential evolution (make perf-gateway, n=200)

| stage | full-chain p50 | full-chain p95 | DLP-added p95 |
|---|---|---|---|
| TASK-033 baseline (2026-09 review) | 9.84ms | 17.34ms | 6.94ms |
| + TASK-036 control-data caches | 7.67ms | 10.45ms | 5.95ms |
| + TASK-037 vault batching | 4.87ms | 7.35ms | 0.45ms |
| + TASK-038 batched audit/usage (single commit) | ~4-5ms | ~7-8ms | 0.45-1.2ms |

SSE window throughput: ~116-131k chunks/sec (unchanged within noise; the
TASK-039 exact holdback is larger but still three orders of magnitude
below real token-stream rates).
