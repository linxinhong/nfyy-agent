# ADR-0027: Usage ledger rows ride the governance audit transaction

Date: 2026-09-03 · Status: accepted · Task: TASK-041

## Context

The concurrency gate (TASK-041) found the gateway's saturation ceiling is
the per-request durable commit count: the audit event set (one batched
INSERT, fail closed) plus the usage ledger row (one INSERT, best-effort)
cost two fsync commits per request. Under 50 concurrent users the commit
serialization dominated latency and left the PRD §9 budget without margin.

Bundling both into one transaction changes one documented contract: today
a usage-ledger failure is logged and the answer is still delivered
("ledger failures do not break the user's answer"), while an audit
failure withholds it. In one transaction, any failure fails both.

## Decision

The usage ledger row of a routed request rides the audit transaction:
`WriteGovernanceBatch` delivers the audit event set and the usage row in
ONE durable commit.

- Usage inherits audit-grade durability. Usage rows are the billing
  source of truth; stronger durability is the safe direction.
- The practical failure envelope is unchanged: a database down fails
  both paths today anyway; a usage-shaped failure (constraint violation)
  that would previously have been logged-and-served now denies the
  request. That class has never been observed in qualification traffic
  and is the price of one-commit governance.
- Sinks that do not implement the batch keep today's separate semantics
  (RequireWriteBatch + recordUsage) — the fallback is contract-identical
  to TASK-038 behavior.

## Consequences

- Per-request durable commits drop from 2-3 to 1 (+ the coalesced async
  key touch), roughly doubling the saturation capacity of the governance
  write path; the 50-user budget profile runs at p95 ≈ 50-90ms on the
  dev box.
- Production capacity planning still keys off the PG commit rate; the
  saturation profile in docs/perf/ records the dev-environment ceiling
  for comparison.

## Revisit triggers

- A hard requirement that billing rows NEVER gate an answer (split back
  out with an explicit async durable queue — architecture work).
- Group-commit/WAL tuning at the infrastructure layer.
