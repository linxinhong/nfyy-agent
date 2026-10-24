# xingding-agent-runtime

Go-native core framework for delivering customized agent projects. It takes architectural inspiration from [Agno](https://github.com/agno-agi/agno) while keeping Go-first interfaces, pluggable storage, and a headless AgentOS runtime.

Module path: `github.com/linxinhong/xingding-agent-runtime`

## Status

This is a work in progress. The following core components are implemented:

- **Agent** — run loop, streaming, tool calls, structured output, memory, knowledge, session state.
- **Team** — multi-agent coordination with a leader model and member delegation.
- **Workflow** — code-first, serializable execution trees with sequence, parallel, condition, structured node results, and durable human review.
- **Project Runtime** — serializable project specifications and validated assembly of Agent, Team, Workflow, dependencies, and AgentOS.
- **Model** — provider-agnostic interface; OpenAI-compatible and Anthropic dialects, with DeepSeek V3/R1 support across hosts.
- **Tools** — function tools, toolkits, built-in tools (calculator, chat history, knowledge search, memory update), and MCP client/server support.
- **Memory / Knowledge** — memory manager and vector-DB-backed knowledge bases.
- **Storage** — in-memory, SQLite, PostgreSQL implementations of the `db.DB` interface.
- **AgentOS** — HTTP runtime with health, config, models, agents, teams, workflows, sessions, runs, memory, knowledge, MCP, endpoints, plus a built-in Playground UI.
- **MCP** — Model Context Protocol server (stdio + Streamable HTTP) and client (subprocess + HTTP) with seamless `tools.Tool` integration.

**Release status (WIP, pre-v1):** this repository is a general-purpose library, not a turnkey SaaS product. `AgentOS` is a development/internal serving surface, not a default public-facing product tier — production hosts embed `pkg/runtime` behind their own API, identity, and domain policy. The hardening plan ([docs/tasks/runtime-core-production-hardening.md](docs/tasks/runtime-core-production-hardening.md)) tracks the path to a production-grade managed execution core (RC-00 → RC-11). Baseline commit for the RC series: `bd51a4f`.

## Requirements

- Go 1.23+
- An API key for the model provider you want to use (OpenAI, DeepSeek, or any OpenAI-compatible endpoint).

## Installation

```bash
go get github.com/linxinhong/xingding-agent-runtime/pkg/agent
```

## Environment Variables

| Variable | Description | Default |
|---|---|---|
| `OPENAI_API_KEY` | API key for the official OpenAI endpoint. | — |
| `OPENAI_BASE_URL` | Custom base URL for OpenAI-compatible APIs. | `https://api.openai.com/v1` |
| `MODEL` | Model ID used by the basic examples. | `gpt-4o-mini` |
| `DEEPSEEK_API_KEY` | API key for DeepSeek (used by `e2e/llm`). | — |
| `LLM_BASE_URL` | Base URL for LLM e2e tests. | `https://api.deepseek.com/v1` |
| `LLM_MODEL` | Model ID for LLM e2e tests. | `deepseek-chat` |
| `PORT` | Port for `examples/server`. | `8080` |

## Quick Start

### Run a single agent

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/basic
```

### Start AgentOS

```bash
export OPENAI_API_KEY=sk-...
go run ./examples/server
```

Open `http://localhost:8080/playground` to use the built-in UI, or call the API directly:

```bash
curl -X POST http://localhost:8080/agents/openai-agent/runs \
  -H 'Content-Type: application/json' \
  -d '{"message":"Say hello","user_id":"u1","session_id":"sess-1"}'
```

### DeepSeek V3 / R1

DeepSeek models resolve through the provider registry:

```go
reg := providers.NewFromEnv()                        // reads DEEPSEEK_API_KEY
v3, _ := reg.Resolve("deepseek:deepseek-chat")       // DeepSeek-V3 series
r1, _ := reg.Resolve("deepseek:deepseek-reasoner")   // R1 / V3 thinking mode
```

- `reasoning_content` is kept separate from the answer, streamed as `reasoning` events, and replayed on thinking-mode tool-call turns as the official API requires.
- DeepSeek weights on other hosts are detected by model ID: `siliconflow:deepseek-ai/DeepSeek-R1`, `volcengine:deepseek-r1-250528` (`ARK_API_KEY`), `qwen:deepseek-r1` (DashScope), or a self-hosted OpenAI-compatible endpoint. On R1-family models, `<think>…</think>` blocks in the content (including templates that pre-fill the opening tag) are split into reasoning, and `reasoning_content` is not sent back to third-party R1 hosts. Override both with `providers.OpenAICompletionsCompat{ReasoningReplay, ParseThinkTags}`. When streaming from a template that pre-fills `<think>`, untagged content is held until `</think>` (at most 32 KiB); set `PrefilledThink` for such deployments so reasoning streams live.
- `max_tokens` is clamped to the catalog ceiling. `insufficient_system_resource` becomes a retryable `models.ErrResourceExhausted`. Cache-hit and reasoning token counts appear in `MessageMetrics`.
- Thinking can be enabled per call on `deepseek-chat` with `models.WithReasoningEffort(ctx, models.ReasoningEffortHigh)`.
- `e2e/deepseekmock` emulates the V3.2 and R1 APIs, including the rules they reject with HTTP 400 (missing reasoning in thinking-mode tool loops, replayed reasoning on R1, `max_tokens` ranges, tool message pairing), keep-alives, prefix-cache accounting, and faults. `go test ./e2e/deepseekmock/` runs the Agent and Team against it without an API key.

### Run loop guarantees

- A run whose model output hit the token limit fails with `agent.ErrOutputTruncated` (partial `Content` kept, truncated tool calls never executed); `content_filter` fails with `agent.ErrContentFiltered`. A stream that ends without a completion signal is `models.ErrStreamInterrupted`.
- Retries use typed errors only (`models.ProviderError`, `models.RetryableError`), honor `Retry-After`, and add jitter. A stream that fails before any delta is forwarded is retried transparently.
- `MaxParallelTools` runs consecutive `tools.WithReadOnly` tools concurrently; `MaxToolResultBytes` (default 256 KiB) bounds results; `ToolErrorFormatter` redacts tool errors. Streams emit `tool_started` / `tool_result` per tool.

### Server deployment

- **Sessions** — runs and resumes on the same session are serialized (`SessionLocker`; in-process by default, share `agent.NewSessionLocker()` across Agents, or plug in a distributed lock for multiple replicas). `SessionLockTimeout` turns a long wait into `agent.ErrSessionBusy`. `MaxHistoryRuns` bounds history, and stores implementing `db.RecentSessionReader` (Postgres, SQLite, in-memory) then load only those runs.
- **Prompt cache** — instructions and output schema form a stable leading system message; session state, memories, knowledge, and the current time go in a separate system message after the history, so provider prefix caches (DeepSeek, OpenAI, Anthropic) hit across turns. `LegacySystemContext` restores the single-message layout. Memory and knowledge lookups run concurrently, and reasoning from earlier runs is not replayed.
- **Context budget** — without an explicit `CompactionContextProvider`, a budget is derived from the smallest advertised context window of the primary and fallback models (`models.ContextLimits`, implemented by `providers` models), reserving `MaxTokens` or a quarter of the window for the response. Its token estimates (plus tool schemas) are calibrated against the prompt tokens providers report, and `ContextSummarizer: agent.NewModelSummarizer(cheapModel)` summarizes old turns instead of only dropping them. `ContextBudget` overrides it; `&agent.ContextWindowPolicy{}` disables it.
- **Crash recovery** — with a store implementing `db.RunLeaseStore` (Postgres, in-memory) runs carry a lease (`RunLeaseTimeout`, default 2m) and heartbeat; AgentOS fails runs whose process died once a minute (`AgentOSConfig.Maintenance`; hosts without AgentOS run `run.RunStaleRunReaper`). They are never re-executed automatically: the transcript is checkpointed before and after each tool round (`DisableRoundCheckpoints`), and `Agent.Recover` / `POST /agents/{id}/runs/{run_id}/recover` continues a reaped run from its checkpoint as a new run. Interrupted calls to tools marked `tools.WithIdempotent` (or read-only) run again; others are reported to the model as "outcome unknown" so no side effect repeats blindly.
- **Cross-replica cancel** — `run.Manager.Cancel` on any replica marks the run cancelled; the owning replica's heartbeat or next checkpoint cancels the local run with `agent.ErrRunCancelledExternally`.
- **Provider connections and caching** — model clients share a pooled HTTP transport (64 idle connections per host). Anthropic requests carry cache breakpoints on tools, system, the end of history, and the last block (`Model.DisablePromptCache` turns them off); cache writes and reads are priced separately. `AgentOSConfig.RunTimeout` (default 120s) bounds runs started over HTTP.
- **Performance** — measured overhead of these features and how to reproduce it: [docs/performance.md](docs/performance.md).
- **Metrics** — runs, tokens and cache hits, cost, retries, circuit breakers, quotas, session locks, admission, reaping, streams, and provider latency are exported as OpenTelemetry metrics once the host installs a `MeterProvider`; see [docs/metrics.md](docs/metrics.md).
- **Fallback** — with `FallbackModels`, a model that fails 5 consecutive times with retryable errors is skipped for 30s (`CircuitBreaker`), then probed once.
- **Quotas** — `MaxRunTokens` caps one run; a `UsageGuard` admits every model call and records its usage and cost (charged to the model that served it, cache hits at the cache price). `agent.NewLocalUsageQuota` gives per-user and per-namespace windows; implement the interface over Redis to share it. HTTP answers 429.
- **Admission** — `run.AdmissionConfig.Cluster` adds limits shared by all replicas; `postgres.DB.ClusterAdmission` provides leased slots that expire with a crashed replica.
- **Retention** — `Maintenance.SessionRetention` makes AgentOS delete sessions idle longer than the period (never ones with pending, running, or paused runs); `run.RunSessionJanitor` does the same outside AgentOS.
- **Sessions across replicas** — `AgentConfig.SessionLocker: store.SessionLocker(postgres.SessionLockConfig{})` serializes a session's runs cluster-wide.
- **Graceful shutdown** — `AgentOS.Shutdown(ctx)` reports `draining` on `/health`, stops accepting connections, lets in-flight requests, streams, and background runs finish until the deadline, then cancels the rest. `Serve` starts maintenance; hosts mounting `Handler()` call `Start(ctx)`.
- **Resumable streams** — streamed runs survive a client disconnect for 30s; reconnect with `GET /runs/{run_id}/events` and `Last-Event-ID` (the run ID is in `run_started` and `X-Run-ID`). With `Streams.Journal: store.StreamJournal()` frames are also written (in 100ms batches) to Postgres and any replica can serve the reconnect; a subscriber on another replica keeps the run from being cancelled as abandoned. Without it, route reconnects to the same replica (`DisableStreamResume` restores connection-bound runs).
- **Teams** — the coordinator runs on the Agent loop (retries, fallbacks, finish reasons, reasoning replay, quotas, context budget) and team runs carry leases like agent runs (as do workflow runs, `workflow.Config.RunLeaseTimeout`). `DelegationTimeout` (default 30m) bounds each delegation.

## AgentOS HTTP API

| Method | Path | Description |
|---|---|---|
| `GET` | `/info` | Runtime name and version. |
| `GET` | `/health` | Health check, including DB connectivity. |
| `GET` | `/config` | Lists registered agents, teams, and workflows. |
| `GET` | `/models` | Lists built-in model identifiers. |
| `GET` | `/agents` | List agents. |
| `GET` | `/agents/{id}` | Get agent metadata. |
| `POST` | `/agents/{id}/runs` | Run an agent. Add `?stream=true` for SSE. |
| `GET` | `/agents/{id}/runs` | List runs for an agent. |
| `GET` / `POST` / `PATCH` / `DELETE` | `/sessions` / `/sessions/{id}` | Session CRUD. |
| `GET` | `/runs/{run_id}` | Lookup a run across agent/team/workflow sessions. |
| `GET` / `POST` / `PATCH` / `DELETE` | `/memories` / `/memories/{id}` | Memory CRUD. |
| `GET` / `POST` | `/knowledge` / `/knowledge/{id}/search` | Knowledge base list and search. |
| `GET` / `POST` | `/teams` / `/teams/{id}/runs` | Team list and run. |
| `GET` / `POST` | `/workflows` / `/workflows/{id}/runs` | Workflow list and run. |
| `GET` / `POST` | `/workflows/{id}/runs/{run_id}` / `/workflows/{id}/runs/{run_id}/resume` | Workflow details and durable resume. |
| `GET` / `POST` / `DELETE` | `/mcp/{name}` | MCP server endpoint (Streamable HTTP transport). |

Run bodies accept JSON:

```json
{
  "message": "user message",
  "user_id": "optional-user-id",
  "session_id": "optional-session-id"
}
```

`user_id` and `session_id` are applied per-request without mutating the shared agent/team/workflow instance.

## Project Layout

- `pkg/agent` — Agent run loop and lifecycle.
- `pkg/team` — Multi-agent team coordination.
- `pkg/workflow` — Workflow step orchestration.
- `pkg/runtime` — customized project specification and runtime assembly.
- `pkg/models` — Model interface and provider implementations (`openai`).
- `pkg/tools` — Tool/Toolkit/Function schema and built-in tools.
- `pkg/session` — Session, RunOutput, and related domain models.
- `pkg/db` — Storage interface and implementations (`in_memory`, `sqlite`).
- `pkg/memory` — Memory manager.
- `pkg/knowledge` — Knowledge base backed by vector DB.
- `pkg/vectordb` — Vector database interface and implementations.
- `pkg/mcp` — MCP protocol implementation: server (stdio + Streamable HTTP) and client (subprocess + HTTP).
- `pkg/os` — AgentOS runtime (HTTP/SSE, routers, middleware, playground).
- `examples` — Go cookbook examples (including `mcp-server` and `mcp-client`).
- `e2e` — End-to-end tests, including LLM-backed tests under `e2e/llm`.

## Development

```bash
make build
make test
make vet
make fmt
```

Run LLM-backed end-to-end tests:

```bash
export DEEPSEEK_API_KEY=sk-...
make e2e-llm
```

## Security red lines

Architectural security constraints that must never be violated (see [docs/security-redlines.md](docs/security-redlines.md)):

1. **Authn/authz config is startup-only.** The auth mode, tenant isolation, scopes, and security key are set once at process startup and must never enter a runtime-mutable configuration surface — a compromised admin session must not be able to change authentication itself.
2. **Effective permissions are always computed fresh**, never materialized into cached snapshots; revocation means deleting the source of truth.
3. **Shareable credentials are stored hashed only** (HMAC-SHA-256 with domain separation), never in plaintext.
4. **Restricted views use default-deny wrappers**: embed the full interface and override only allowlisted methods (compile-time static assertion pattern).

## Design and roadmap

- [Agent application runtime roadmap](docs/agent-application-roadmap.md)
- [Agno and Agent Framework Go gap analysis](docs/research/agno-gap-analysis.md)
- [ContextProvider and HistoryPolicy](docs/context-provider.md)
- [Workflow Core](docs/workflow-core.md)
- [Run lifecycle](docs/run-lifecycle.md)
- [Project Runtime](docs/project-runtime.md)
- [Skills（渐进式披露）](docs/skills.md)
- [MCP](docs/mcp.md)

## License

MIT — Copyright (c) 2026 XingDing. See [LICENSE](LICENSE).
