# ADR-0023: Capability Gateway — the unified enterprise capability plane

Date: 2026-09-03 · Status: accepted · Task: TASK-034

## Context

EAP V1 exposes governed capabilities through separate surfaces: MCP tools at
`/mcp/{server}` (direct tool mount) and Skills through the registry + local
`eap sync`. The OpenWork Den control plane (studied in source at
`~/git/openwork/ee/apps/den-api/src/mcp/`) validates the alternative: a
single meta-tool surface (`search_capabilities` + `execute_capability`) where
agents discover and invoke every organizational capability through two
constant tools, keeping agent context small and governance centralized.

Den's largest open gap (their issue #2697) is that discovery is NOT
permission-filtered: agents find capabilities that fail at execution time.

## Decision

1. **Capability is the unified abstraction.** MCP tools, Skills (and later
   HTTP connectors, model aliases, workflows) are implementation sources of
   one object: a named, risk-classified, policy-governed enterprise
   capability. Names are colon-segmented: `mcp:<server>:<tool>`,
   `skill:<name>` / `skill:<name>@<version>`.
2. **Composite source registry (code, not a mega-table).** Following Den,
   each source implements Search/Parse/Execute over the existing governed
   tables (`mcp_tools`, `skill_packages`). No new source-of-truth table.
3. **Permission-aware discovery (EAP's differentiation).** search results are
   filtered by workspace membership and annotated with policy/risk status
   (`available | needs_approval | denied_by_policy | disabled`). This is UX
   and planning only — **execution always revalidates everything**; a search
   result is never an authorization token.
4. **Dual-surface coexistence.** `/mcp/{server}` stays for clients that want
   direct tool mounts; `/mcp/agent` is the meta-tool surface. Both share the
   identical governance chain (identity → workspace → policy → risk →
   approval → DLP egress → audit). No capability bypasses either.
5. **Skills execute as governed guidance.** `execute_capability` on a skill
   returns verified install/sync instructions; skill bodies still run only
   locally through `eap sync` (ADR-0017). The platform never executes
   package code.
6. **Token-scored search** (Den's algorithm: camelCase split, name/summary
   weights) — explainable, no vector index dependency for V1.5.

## Not adopted from Den

- OAuth/RFC9728 on the MCP surface (enterprise `agk_*` credentials are the
  frozen contract; device-flow CLI credentials already exist).
- Code Mode, MCP Apps, remote sessions (outside the EAP boundary).
- Den's "execute does not audit" model — EAP writes reliable-path audit for
  every capability execution directly.

## Consequences

- Agents' tool lists stay constant (2 tools) regardless of capability count.
- Adding an HTTP connector or model-capability later is a new Source adapter,
  not a new governance path.
- An empty search query means "list everything visible to me" (refined after
  live agent use: token-scored search alone left agents unable to browse).
- The discovery-status vocabulary must stay in sync with policy effects
   (deny → denied_by_policy; R2+ → needs_approval) or agents will plan on
  stale information.

## Follow-ups recorded

schemaDigest search→execute drift detection; HTTP connector + model-alias
sources; capability usage telemetry; Agent Registry (profiles binding
capabilities to agents) as the next card.
