# TASK-008 real-artifact verification

This is an opt-in test workflow, not a release or a clean-target/offline desktop
acceptance substitute. It uses disposable development signing keys, never production
credentials. Do not run the fixture builder concurrently with another artifact build
in the same worktree: it temporarily backs up and restores `dist-artifact`.

## Hosted / source-checkout reproduction

Use the exact tested commit (including the test scripts) and the matching OS/CPU,
Node and pnpm versions pinned by the repository. With Python/uv on the **build host**:

```sh
pnpm install --frozen-lockfile --ignore-pnpmfile
pnpm build
pnpm build:native
node apps/desktop/test/task-008-build-fixtures.mjs /absolute/new-fixtures
node apps/desktop/test/task-008-real-artifact-journey.mjs /absolute/new-fixtures /absolute/new-results
```

Both explicit output directories must be absent before running; existing directories
are rejected, not overwritten. Windows paths may be supplied instead. Every run uses
a new isolated Yuanpu home and workspace. Child PATH excludes Python (but keeps
System32 on Windows for lifecycle cleanup); the host can still have Python installed.
Stopping the catalog is not a network-isolation test.

The manual `TASK-008 Artifact Verification` workflow executes this journey on the
three supported targets. Uploaded fixtures contain public signed manifests, archives,
public trust metadata, the 0.1.0 bootstrap bundle and a sanitized result. They contain
**no standalone host runner or SEA**. To replay a downloaded fixture, use the exact
source checkout and the first three build commands above; extract the artifact, then
pass the directory containing `metadata/fixture.json` to the journey command. The
journey needs `apps/runtime/dist-native/bin/YuanpuAgentRuntime-<target>[.exe]`,
`apps/desktop/dist/runtime-manager.cjs`, the Go server built from `server/backend/`, and
`packages/yuanpu-runtime/dist/index.mjs` from those matching builds. Check the fixture
source commit/target against the CI run before reusing it. GitHub's ZIP upload may not
preserve executable mode on POSIX; restore executable mode on the extracted 0.1.0
bootstrap executable before replay. Do not treat this replay as no-system-Python
target acceptance.

For real desktop target testing, the two version directories can instead seed the
controlled catalog, with the matching public test trust root injected only into an
isolated test App. A matching installed App/SEA, UI approval and OS-specific
installation observations are separate checks. The TASK-008 acceptance scope does
not require disconnecting external networking or formal platform code signing;
neither is implied by this CI. Do not replace the user's trust root or weaken
capability-manifest signature verification.

## Current macOS desktop approval probe

After building the desktop and Runtime, start the renderer on a private loopback
port, then run `apps/desktop/test/task-036-ui-app-probe.mjs` with
`TASK_008_APPROVAL_FIXTURE=1` and `TASK_036_RENDERER_URL` set to that renderer URL.
This starts a real isolated Electron/Runtime/MCP stack and drives the actual
one-time approval controls for allow and deny. Its OpenAI-compatible model
responses are deterministic local fixtures, **not** evidence of a live model
provider. It uses a temporary Yuanpu home and does not read the user's provider
credentials. A previous live-provider TASK-036 run is historical supporting
evidence only, not a substitute for this revision's desktop probe.

For the complete current skills-page journey, first generate both signed versions
with `task-008-build-fixtures.mjs` in a new temporary directory, then use
`TASK_008_FULL_UI_FIXTURE=/absolute/fixture-root` instead of
`TASK_008_APPROVAL_FIXTURE=1` with the same renderer URL. The probe serves the
two versions from a private loopback catalog, installs v0.1.0 through the trust
dialog, saves configuration, injects a bad v0.2.0 manifest signature and checks
v0.1.0 remains active, retries and upgrades, rolls back, reloads the renderer,
and compares page state to the isolated durable state after each transition.
The resulting log reports `skillsUiJourney: true`; it also runs the allow/deny
approval checks. Neither this mode nor the hosted artifact workflow touches the
user's normal Yuanpu home.

## Existing Windows installation smoke

Run `task-008-installed-windows-probe.cmd` (or the adjacent PS1) from PowerShell/CMD.
It uses the normal PowerShell execution policy, the existing installed binaries, a
fresh temp home and a minimal environment. It does not install or update capabilities.
When the Mac user directory is mapped as `G:`, the task worktree scripts are under
`G:\projects\yuanpu-agent\.worktrees\codex-yuanpu\apps\desktop\test`.
