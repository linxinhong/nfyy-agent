# ADR-0028: /v1 request passthrough stance, multimodal contract, and Bifrost adoption criteria

Date: 2026-09-04 · Status: accepted · Tasks: TASK-051 / TASK-052 / TASK-053

## Context

The routed provider path (LLMRouter → OpenAICompatClient) deliberately bypasses
Bifrost: the pinned v1.8.4 carries a documented stream-cancellation data race
that architecture forbids on the production streaming path, and its
warning-oriented plugin semantics are wrong for the DLP/policy security chain.
The cost is interface compatibility: our request surface is a hand-modeled
whitelist, so anything we have not typed is silently dropped. A field diff
against Bifrost's neutral schemas (the TASK-042 baseline, `~/projects/bifrost`
dev is far ahead of the v1.8.4 pin) shows large gaps: multimodal content parts
(flattened to text today), n, logprobs/top_logprobs, penalties,
response_format, seed, user, metadata, reasoning/refusal/annotations, the
Anthropic-native surface, and more. Each gap is a future real-client failure
of the kind TASK-042 was opened to stop.

## Decision

1. **Governance-scoped transparent passthrough (TASK-051).** EAP governs a
   frozen set of request fields (model, messages incl. tool fields, tools,
   tool_choice, sampling fields already forwarded). Every OTHER request field
   is captured opaquely and forwarded verbatim to the provider, after the
   governed fields. The provider remains the validator of its own surface;
   provider 400s surface unchanged. The governed set is the security
   boundary — DLP/policy/audit semantics never depend on opaque fields.
2. **Multimodal content parts are preserved, text-governed (TASK-052).**
   Inbound content-part arrays keep their original structure toward the
   provider; text parts are flattened for the DLP request scan exactly as
   today; non-text parts pass unscanned in V1 (documented: no provider-bound
   egress defense applies to binary parts, and employee-facing surfaces keep
   the existing egress scan). Assistant deltas that carry non-text parts are
   forwarded structurally — never text-transformed.
3. **Bifrost adoption criteria for the routed path (TASK-053).** The routed
   streaming path may adopt Bifrost only when ALL hold: (a) the pin is
   upgraded past the stream-cancellation race AND a reproduction harness
   proves cancellation is clean on the pinned commit; (b) a security review
   confirms plugin failures still fail closed under EAP's governance chain;
   (c) an ADR amendment records the swap. Until then Bifrost stays
   schemas-as-baseline + legacy-path-only. The verification harness is a gate
   card, not a feature card.

## Consequences

- Unknown-field gaps close in one move instead of one card per client bug.
- The governed-field list becomes a frozen contract artifact (docs/
  protocol-conformance.md) — adding a field to it is a governance decision.
- Non-text parts ride unscanned in V1: recorded, revisited when a real
  modality-carrying client lands.

## Follow-ups recorded

Embeddings/Responses/Anthropic-Messages parity against Bifrost schemas;
prompt-cache fields; reasoning-model output surface.
