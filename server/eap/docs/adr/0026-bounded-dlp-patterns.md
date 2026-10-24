# ADR-0026: Seeded DLP patterns must have a finite maximum match length

Date: 2026-09-03 · Status: accepted · Task: TASK-039

## Context

The streaming DLP sanitizer releases SSE content only up to the prefix that
can no longer match any rule; the remainder rides a holdback window sized
"longest possible match + margin". That bound was a hardcoded 64 bytes —
any dictionary term or pattern longer than ~80 bytes could straddle a
release boundary and escape scanning. TASK-039 derives the bound from the
compiled rule set instead.

Derivation exposed a mathematical fact: six seeded rules carried open-ended
repetition (`{20,}`, `\s+`, `\S{4,}`, `[^\s"]+`, `[A-Z ]*`), for which the
longest possible match is infinite. For such patterns no finite holdback is
safe: either the window grows without bound (whole-response buffering,
killing streaming liveness) or a straddle window remains.

## Decision

1. **Rule policy: every DLP pattern must have a finite maximum match
   length.** Migration 000020 bounds the six seeded patterns with generous
   caps (e.g. `sk-[A-Za-z0-9]{20,200}`, `\s{1,32}` inside bearer tokens,
   `[^\s"]{1,512}` in connection strings). The caps sit far above any real
   credential's length, so deny/redact behavior for real traffic is
   unchanged; only pathological megabyte runs stop matching. Updates are
   unconditional by rule name so upgrades converge.
2. **Derivation, not configuration.** The scanner computes the bound from
   the parsed pattern AST at compile time (`MaxMatchBytes`); the SSE
   sanitizer sizes its window from that value. Patterns added later that
   are unbounded set `bounded=false`; the sanitizer then applies the
   documented 4096-byte cap and the derivation logs loudly. Authoring-time
   validation (rejecting unbounded patterns at write) remains a follow-up.

## Consequences

- The streaming window is exact by construction: no rule match can straddle
  a release boundary (regression-tested with a 360-byte dictionary term
  split across upstream chunks).
- Holdback is now up to ~1055 bytes for the default rule set (dominated by
  `db_conn_string`), still far below streaming liveness thresholds; SSE
  throughput remains ~10⁵ chunks/sec.
- A rule whose match length genuinely exceeds the cap would reintroduce a
  theoretical straddle window; the cap path is observable via logs.

## Revisit triggers

- Admin-facing rule authoring lands (write-time validation for unbounded
  patterns becomes mandatory).
- The 4096 cap is observed in production logs (someone shipped an
  unbounded pattern).
