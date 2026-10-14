# Repository Guidelines

## Project Structure & Module Organization

This pnpm monorepo targets Node.js 24. `apps/app` contains the React/Vite renderer; `apps/desktop` owns Electron main/preload code and Runtime updates; `apps/runtime` builds the Node SEA sidecar; and `apps/python-capabilities` contains the managed Python MCP example. Yuanpu-owned shared code lives in `packages/yuanpu-*`. Pi comes from pinned npm packages; when upgrading Pi or changing its adapters, read `docs/pi-integration.md` for persistence, MCP and SEA compatibility checks. `server/backend` is the Go center and skill catalog; `server/web` is its independent website. Architecture notes live in `docs/`; task evidence lives in `.tasks/`.

Keep tests beside their module in `test/` directories. Generated `dist/`, `dist-native/`, `dist-artifact/`, and `release/` directories are build output, not source.

## Build, Test, and Development Commands

- `pnpm install --frozen-lockfile --ignore-pnpmfile`: install the pinned workspace dependencies.
- `pnpm dev`: build Runtime and desktop code, then launch the catalog, Vite renderer, and Electron.
- `pnpm check`: run the full build, Yuanpu TypeScript checks, and all tests. Use this before submitting changes.
- `pnpm build:native && pnpm smoke:native`: build the current-platform SEA and exercise the frozen Python MCP plus staged Runtime update.
- `pnpm package:desktop`: produce the current-platform Electron installer/archive under `apps/desktop/release/`.
- `pnpm dev:server`: run the Go center and skill catalog.

## Coding Style & Naming Conventions

TypeScript is strict and uses ES modules, two-space indentation, single quotes, and semicolons. Use `camelCase` for values/functions, `PascalCase` for classes and exported types, and kebab-case filenames such as `runtime-updater.ts`. Prefer narrow host interfaces and keep Electron renderer access behind preload IPC. Python packages and functions use `snake_case`; pin Python dependencies in `pyproject.toml` and `uv.lock`.

## Testing Guidelines

JavaScript tests use Node’s built-in `node:test` with `*.test.mjs` names. Add behavioral tests in the affected package and use loopback servers or temporary directories instead of external services. Run the focused package test first, then `pnpm check`. Update native/update paths with bounded smoke tests and verify child-process cleanup.

## Commit & Pull Request Guidelines

Use concise conventional prefixes seen in history: `feat:`, `fix:`, `test:`, `docs:`, `ci:`, or `tasks:`. Keep commits focused. Pull requests should explain behavior and risk, list verification commands, link the task/issue, and include screenshots for renderer changes. Call out unverified platforms or signing requirements explicitly; green local builds do not replace hosted platform evidence.

## Security & Configuration

Never commit API keys, signing keys, or generated user data. Runtime configuration belongs under `~/.yuanpu` (or `%USERPROFILE%\.yuanpu`). Preserve checksum, signature, approval, path-containment, and process-lifecycle checks when changing installers, plugins, MCP sources, or update code.
