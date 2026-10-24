# Pi integration

Yuanpu uses the official **1.4.0 npm packages** (registry `latest` checked on 2026-10-22). `pnpm-lock.yaml` records their integrity; exact direct versions and Pi-family overrides in `pnpm-workspace.yaml` keep the published family on one release. `docs/pi-upstream.json` records the release and the previous 0.86.1 source revision.

The 1.0.4 → 1.4.0 update brings provider and model fixes, more reliable resumed extension tools and MCP handling, and compaction fixes. Durable tool registrations return `output`; the Assistant adapter now forwards its tool content through that field so delegated tool results reach the model. The Assistant uses Durable's JSONL store, so 1.4.0's automatic **SQLite schema 2** migration does not touch its current session files. Durable also introduces sleeping tasks and changes retry behavior; a session written by 1.4.0 must not be assumed readable by 1.0.4 without a rollback test. Yuanpu does not call the removed `TaskRuntime.sleep()` or implement Durable storage itself. Pi 1.3 changes `kimi-coding` to Chat Completions; a user-supplied base URL for that provider must include `/v1`.

## Ownership and builds

`packages/yuanpu-*` contains Yuanpu code. Pi source mirrors, `build:pi`, `sync:pi`, the copied bundle helper and model-data hydration script have been retired. Published Pi packages already contain the compiled code and model data needed by the build. A sibling Pi checkout is unnecessary.

- `packages/yuanpu-runtime` adapts the public `pi-coding-agent` SDK for Work sessions and owns managed capabilities.
- `packages/yuanpu-assistant` adapts `pi-durable` for isolated Assistant sessions. Application tools and skills use Yuanpu-owned contracts.
- `apps/runtime` composes those adapters into a CJS bundle and a Node SEA executable.
- `pi-ai` supplies model/provider APIs; `chord/context` supplies the durable harness context.

Use published package entry points. Pi implementation files and unpublished subpaths are not application APIs. Go `server/` and `yuanpu-server-client` remain independent of Pi's internal session/storage protocol.

## Assistant persistence

Pi 1.0 removed `AgentHarness`, `JsonlSessionRepo`, skills and the `pi-agent-core/node` export. The replacement `pi-durable` is still documented upstream as **experimental**, including breaking changes between patch releases. Keep its exact version pinned behind `AssistantExecutor`.

New durable stores live under `assistantHome/sessions/pi/durable-v1/<sessionId>/`. When an old session is first reopened, `pi-storage.ts` reads its format-4 committed `main` branch, imports messages and compaction context into a temporary store, then publishes that store atomically. The original JSONL and frozen identity snapshot remain available. Off-branch entries and unfinished operation state are not replayed; an unfinished tool call receives an interrupted/unknown result. Invalid formats or broken ancestry fail before publication. Import metadata records the source path and SHA-256.

Identity remains frozen per session. Current personal memory is injected through a request hook and excluded from the persisted conversation. Private reflection uses memory storage. The Assistant and delegated professional executors do not load Work MCP configuration or ambient extensions.

## Built-in MCP

The Skills page's **MCP 服务** entry edits the built-in service configuration without installing a plugin:

- User configuration: `~/.yuanpu/agent/mcp.json`.
- Trusted workspace configuration: `<workspace>/.pi/mcp.json`.
- The existing configuration API uses the virtual name `pi-mcp`.

Yuanpu explicitly registers Pi's public `createMcpExtension()` and `createToolSearchExtension()` factories, binds extension startup, and awaits extension shutdown before disposing a Work session. Pi owns MCP connection, protocol, discovery, execution and shutdown. Yuanpu's small adapter selects configuration scope and preserves legacy `disabled` and `$env:NAME` settings.

MCP tools use Pi's **deferred** exposure by default: the model discovers them through `tool_search`, then calls `mcp__<server>__<tool>`. `direct` and `hidden` are supported. Codemode exposure is mapped to deferred exposure; JavaScript codemode, its Worker and QuickJS WASM are not enabled in the single-file Runtime. This keeps configured services usable without silently depending on files next to an npm installation.

At Runtime startup, managed `pi-mcp-adapter` installation records and their package directories are retired, and their entries are removed from Pi's package settings. MCP configuration is retained. If the old plugin was disabled, its connections stay disabled and a `.pre-pi-1.bak` configuration backup is retained. Installation/search no longer offers the old adapter. Manually installed extensions outside Yuanpu's managed package directories need separate removal if they implement their own MCP handler.

Signed Python capability artifacts retain Yuanpu's installation, update, permission and process supervision. They and the browser/web/notification providers remain behind `search_capabilities` and `execute_capability`. These tools are separate from Pi's built-in MCP services; a configured trusted MCP service has the same extension trust boundary as the old adapter, not the signed capability approval policy. Duplicate named connections are reported before installing a managed artifact.

## Model compatibility and slow requests

Yuanpu applies endpoint defaults for `api.longcat.chat` using the OpenAI completions API: `max_tokens` and explicit `thinking.type`, without `reasoning_effort`. Explicit `models.json` compatibility overrides win. Work and Assistant use the same defaults; model limits and the user's thinking selection are retained. These fields follow the [LongCat API documentation](https://longcat.chat/platform/docs/api/chat.html) and [configuration example](https://longcat.chat/platform/docs/OpenClaw.html). Correct compatibility alone does not guarantee provider latency.

Work session JSONL files include `customType: "yuanpu_model_timing"` records for request start, response headers, first parsed provider event, first text and completion. Records share a request ID and host run ID, and report elapsed milliseconds. They contain no prompt, response text, reasoning text, headers or credentials. Compare their timestamps with the host run's `created_at` to distinguish local preparation from waiting on the model. A first event may be reasoning or protocol metadata; it is not necessarily visible text. An interrupted process may leave an incomplete trace, which must not be treated as successful completion. Assistant uses a separate durable engine and does not emit these Work timing entries.

The desktop investigation and real-call measurements are recorded in [.tasks/verification/pi-1-desktop-2026-10-11/report.md](../.tasks/verification/pi-1-desktop-2026-10-11/report.md).

## Updating and verification

1. Inspect official changelogs and published exports, especially `pi-durable`, model-provider names and MCP tool exposure.
2. Update exact direct versions, all Pi-family overrides, `PI_UPSTREAM_VERSION`, built-in MCP display version and `docs/pi-upstream.json` together.
3. Install with `pnpm install --ignore-pnpmfile`; review lockfile changes. Verify a fresh `pnpm install --frozen-lockfile --ignore-pnpmfile`.
4. Run `pnpm check`, `pnpm build:native` and `pnpm smoke:native`. The native smoke includes a real loopback model → Work HTTP API → Pi tool search → stdio MCP call, with process cleanup.
5. Verify migration with old-session fixtures, reopened histories, memory isolation, cancellation, package retirement and existing signed Python artifact checks. Hosted Windows/Linux release checks remain necessary.

Pi 1.0.3 renamed the built-in Azure provider to `azure`. Existing Azure users must update their provider keys in auth/model/settings files according to the upstream changelog; the upstream rename is not an automatic credential migration here. Keep original session archives when rolling back; a 0.86 runtime cannot read new durable stores.

## Upstream references

- [Pi 1.4.0 release](https://github.com/earendil-works/pi/releases/tag/v1.4.0)
- [Durable changelog and storage changes](https://github.com/earendil-works/pi/blob/v1.4.0/packages/durable/CHANGELOG.md)
- [Coding Agent changelog](https://github.com/earendil-works/pi/blob/v1.4.0/packages/coding-agent/CHANGELOG.md)
- [AI provider changelog](https://github.com/earendil-works/pi/blob/v1.4.0/packages/ai/CHANGELOG.md)
- [MCP configuration](https://github.com/earendil-works/pi/blob/v1.4.0/packages/coding-agent/docs/mcp.md)
