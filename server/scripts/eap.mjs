// Runner for the EAP-based center (server/eap): the EAP gates (fmt, vet,
// unit, race, integration, perf), the gateway and curator builds, local
// development (`dev` runs the gateway and curation worker together), and
// `runtime`: fmt, vet and unit tests of the vendored agent runtime
// (server/agent-runtime, its own Go module, used by the model curator).
// The website is the EAP Portal (server/eap/apps/web, its own pnpm project
// and lockfile): `portal` installs it frozen, typechecks, lints, unit-tests
// and builds the dist the gateway serves; `web` runs its Vite dev server
// against the dev gateway on 8790.
//
// Why: the root package scripts must work the same on macOS, Linux and
// Windows without make; this wraps the Go commands and the dev defaults.
//
// Pitfalls: `dev` needs PostgreSQL and Redis (EAP_DATABASE_URL /
// EAP_REDIS_ADDR); migrations run on gateway start. `integration` and `perf`
// need EAP_TEST_DATABASE_URL (admin connection; tests recreate their own
// databases) and EAP_TEST_REDIS_ADDR. `perf` holds the concurrency and soak
// gates: they measure p95 against the PRD budget and only mean something on
// a dedicated machine, so they are not part of `integration`. Local login and
// the bootstrap admin are enabled for development only — never reuse these
// defaults in a deployment.
import { spawn } from 'node:child_process';
import { mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const eapRoot = fileURLToPath(new URL('../eap/', import.meta.url));
const runtimeRoot = fileURLToPath(new URL('../agent-runtime/', import.meta.url));
const portalRoot = join(eapRoot, 'apps', 'web');
const pnpm = process.platform === 'win32' ? 'pnpm.cmd' : 'pnpm';
const exe = (name) => join(eapRoot, 'dist', process.platform === 'win32' ? `${name}.exe` : name);
const binary = exe('gateway');
const perfGates = 'TestGatewayConcurrencyGate|TestGatewaySoak';
const commands = {
  // -o to a directory builds both binaries: dist/gateway and dist/curator.
  build: ['build', '-trimpath', '-o', join(eapRoot, 'dist') + '/', './apps/gateway', './apps/curator'],
  vet: ['vet', './...'],
  test: ['test', '-count=1', './...'],
  race: ['test', '-race', '-count=1', './...'],
  integration: ['test', '-tags', 'integration', '-count=1', '-timeout', '40m', '-skip', perfGates, './...'],
  perf: ['test', '-tags', 'integration', '-count=1', '-timeout', '60m', '-run', perfGates, '-v', './apps/gateway'],
};
const command = process.argv[2];
if (!['dev', 'curator', 'fmt', 'runtime', 'portal', 'web'].includes(command) && !Object.hasOwn(commands, command)) {
  throw new Error('Expected build, dev, curator, fmt, vet, test, race, integration, perf, runtime, portal or web');
}
if (command === 'curator' && !process.env.EAP_DATABASE_URL) {
  throw new Error('curator needs EAP_DATABASE_URL (the gateway must have migrated the database)');
}
if ((command === 'integration' || command === 'perf') && !(process.env.EAP_TEST_DATABASE_URL && process.env.EAP_TEST_REDIS_ADDR)) {
  throw new Error(`${command} needs EAP_TEST_DATABASE_URL (PostgreSQL admin connection) and EAP_TEST_REDIS_ADDR`);
}

let interrupted = false;
function run(program, args, env = process.env, cwd = eapRoot, onStart) {
  return new Promise((resolve) => {
    // pnpm is a .cmd shim on Windows, which spawn only runs through a shell.
    const child = spawn(program, args, { cwd, stdio: 'inherit', env, shell: program.endsWith('.cmd') });
    onStart?.(child);
    let force;
    const forward = (signal) => {
      interrupted = true;
      child.kill(signal);
      force ??= setTimeout(() => child.kill('SIGKILL'), 12_000);
    };
    const onInterrupt = () => forward('SIGINT');
    const onTerminate = () => forward('SIGTERM');
    process.on('SIGINT', onInterrupt);
    process.on('SIGTERM', onTerminate);
    child.on('error', (error) => console.error(`Command failed. Install Go 1.27 or newer: ${error.message}`));
    child.on('close', (code) => {
      clearTimeout(force);
      process.off('SIGINT', onInterrupt);
      process.off('SIGTERM', onTerminate);
      resolve(typeof code === 'number' && code >= 0 ? code : 1);
    });
  });
}

// Same rule as EAP's fmt-check: any file gofmt -l lists (outside .tasks/) fails,
// and so does gofmt itself failing.
async function gofmt(cwd) {
  const result = await new Promise((resolve) => {
    const child = spawn('gofmt', ['-l', '.'], { cwd });
    let out = '';
    let err = '';
    child.stdout.on('data', (chunk) => { out += chunk; });
    child.stderr.on('data', (chunk) => { err += chunk; });
    child.on('error', (error) => resolve({ code: 1, out: '', err: `gofmt unavailable: ${error.message}` }));
    child.on('close', (code) => resolve({ code, out, err }));
  });
  const unformatted = result.out.split('\n').filter((line) => line && !line.startsWith('.tasks/'));
  if (result.code !== 0 || result.err.trim() || unformatted.length) {
    console.error(result.err.trim() || `Unformatted Go files:\n${unformatted.join('\n')}`);
    return 1;
  }
  return 0;
}

if (command === 'fmt') process.exit(await gofmt(eapRoot));
if (command === 'portal') {
  let code = 0;
  for (const args of [
    ['install', '--frozen-lockfile'],
    ['exec', 'tsc', '--noEmit'],
    ['exec', 'oxlint'],
    ['exec', 'vitest', 'run'],
    ['exec', 'vite', 'build'],
  ]) {
    if (code === 0 && !interrupted) code = await run(pnpm, args, process.env, portalRoot);
  }
  process.exit(code);
}
if (command === 'web') {
  process.exit(await run(pnpm, ['exec', 'vite'], { VITE_API_TARGET: 'http://127.0.0.1:8790', ...process.env }, portalRoot));
}
if (command === 'runtime') {
  let code = await gofmt(runtimeRoot);
  for (const args of [['vet', './...'], ['test', '-count=1', './...']]) {
    if (code === 0 && !interrupted) code = await run('go', args, process.env, runtimeRoot);
  }
  process.exit(code);
}

await mkdir(join(eapRoot, 'dist'), { recursive: true });
let code = await run('go', commands[command === 'dev' || command === 'curator' ? 'build' : command]);
if (command === 'curator' && code === 0 && !interrupted) {
  code = await run(exe('curator'), [], {
    EAP_CURATOR_LLM_BASE_URL: 'http://127.0.0.1:8790/v1',
    ...process.env,
  });
}
if (command === 'dev' && code === 0 && !interrupted) {
  let gatewayChild;
  let curatorChild;
  const gateway = run(binary, [], {
    EAP_GATEWAY_ADDR: '127.0.0.1:8790',
    EAP_OIDC_REDIRECT_URL: 'http://127.0.0.1:8790/enterprise/v1/auth/oidc/callback',
    EAP_LOCAL_AUTH: '1',
    EAP_MIGRATIONS_DIR: join(eapRoot, 'migrations'),
    EAP_WEB_DIST: join(eapRoot, 'apps', 'web', 'dist'),
    ...process.env,
  }, eapRoot, (child) => { gatewayChild = child; });
  // The worker retries while the gateway applies migrations. If either
  // process exits, stop the other so development cannot silently run without
  // automatic organization.
  const curator = run(exe('curator'), [], {
    EAP_CURATOR_LLM_BASE_URL: 'http://127.0.0.1:8790/v1',
    ...process.env,
  }, eapRoot, (child) => { curatorChild = child; });
  const first = await Promise.race([
    gateway.then((exitCode) => ({ name: 'gateway', exitCode })),
    curator.then((exitCode) => ({ name: 'curator', exitCode })),
  ]);
  if (!interrupted) {
    console.error(`Development ${first.name} stopped; stopping the other process.`);
    (first.name === 'gateway' ? curatorChild : gatewayChild).kill('SIGTERM');
  }
  await Promise.all([gateway, curator]);
  code = interrupted ? first.exitCode : first.exitCode || 1;
}
process.exitCode = code;
