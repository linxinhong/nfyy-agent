# ADR-0025: Tool-calling passthrough on `/v1/chat/completions`

Date: 2026-09-03 · Status: accepted · Task: TASK-036

## Context

External agent clients drive tools through OpenAI function calling: Hermes
registers MCP tools client-side and sends them in the chat request's `tools`
array. The gateway's `/v1/chat/completions` pipeline (protocol-openai →
llmkernel → OpenAICompatClient) is whitelist-shaped and silently drops
`tools`, `tool_calls` messages and `tool_calls` deltas. Tool-using clients
therefore see a model that cannot call anything and hallucinates
conveniences instead — observed live with Hermes against the EAP endpoint.
The routed provider path (TASK-008/010) is EAP's own OpenAI-compatible
client, so passthrough is a bounded contract change, not a Bifrost change.

## Decision

1. **Byte-faithful tool passthrough on the routed path.** `tools` and
   `tool_choice` travel as opaque JSON from client to provider; assistant
   `tool_calls`, `tool` role messages (with `tool_call_id`) and streaming
   `tool_calls` deltas pass through unchanged. EAP governs the conversation,
   not the tool schema.
2. **Governance covers the conversation content, including tool arguments.**
   Request-side DLP scans message content AND `tool_calls[].function.arguments`
   (provider-bound conversation data), with redaction written back into the
   arguments string. Tool *definitions* are client-supplied contract metadata
   and are not scanned — redaction inside a JSON schema would corrupt it.
   Streaming `tool_calls` deltas bypass the SSE content sanitizer (they are
   JSON fragments; text transforms would corrupt them) — the same data was
   already scanned when it entered the conversation.
3. **Fail loud where tools are unsupported.** The direct Bifrost kernel path
   (`Kernel.GenerateChat`/`StreamChat`) rejects requests carrying tools with
   `invalid_argument` instead of silently dropping them. Bifrost-path tool
   support is a follow-up.
4. **Finish reason is the provider's.** The terminal SSE frame carries the
   provider's `finish_reason` (e.g. `tool_calls`), not a hardcoded `stop`.

## Consequences

- Hermes-class agents get native function calling through one employee key.
- Tool execution stays client-side (Hermes runs MCP locally); `/mcp/*`
  governance is unchanged. Provider-bound tool results re-enter as messages
  and are scanned like any other content.
- The frozen public contract grows additively; clients not using tools see
  zero behavior change.

## Follow-ups recorded

Bifrost-kernel tool mapping; Responses/Anthropic surfaces tool parity;
per-tool policy hooks (caller-side tool allowlists ride the existing policy
engine via MCP surfaces, not chat passthrough).
