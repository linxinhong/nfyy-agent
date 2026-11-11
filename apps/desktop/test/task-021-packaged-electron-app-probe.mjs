import assert from 'node:assert/strict';
import { execFile, execFileSync, spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmod, copyFile, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { promisify } from 'node:util';

import { SEA_SENTINEL_FUSE } from '../../runtime/scripts/native/lib.mjs';

const execFileAsync = promisify(execFile);

const appBundle = resolve(
  process.env.TASK_021_PACKAGED_APP_PATH || resolve(import.meta.dirname, '../release/mac-arm64/南医小助.app'),
);
const executable = join(appBundle, 'Contents', 'MacOS', 'nanyi-xiaozhu');
const bundledSea = join(appBundle, 'Contents', 'Resources', 'runtime', 'YuanpuAgentRuntime-darwin-arm64');
const versionedSeaVersion = '0.1.1-test.021';
const root = await mkdtemp(join(tmpdir(), 'yuanpu-task-021-packaged-app-'));
const home = join(root, 'home');
const userData = join(root, 'user-data');
const workspace = join(root, 'workspace');
const apps = [];
let providerRequests = 0;
const workProviderBodies = [];
const provider = createServer(async (request, response) => {
  if (request.method !== 'POST' || request.url !== '/v1/chat/completions') {
    response.writeHead(404).end();
    return;
  }
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
  if (body.tools?.some((tool) => tool.function?.name === 'write')) workProviderBodies.push(body);
  providerRequests += 1;
  response.writeHead(200, { 'content-type': 'text/event-stream' });
  const chunk = (delta, finish_reason = null) => `data: ${JSON.stringify({
    id: 'task-021-packaged', object: 'chat.completion.chunk', created: 1,
    model: 'fixture-model', choices: [{ index: 0, delta, finish_reason }],
  })}\n\n`;
  response.write(chunk({ role: 'assistant', content: 'Synthetic packaged reply.' }));
  response.end(chunk({}, 'stop') + 'data: [DONE]\n\n');
});

async function eventually(check, message, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const result = await check();
    if (result) return result;
    await new Promise((done) => setTimeout(done, 40));
  }
  throw new Error(message);
}

async function freePort() {
  const server = createServer();
  await new Promise((done) => server.listen(0, '127.0.0.1', done));
  const port = server.address().port;
  await new Promise((done) => server.close(done));
  return port;
}

async function buildVersionedSea() {
  const source = await readFile(resolve(import.meta.dirname, '../../runtime/dist/index.cjs'), 'utf8');
  assert.equal(source.split('0.1.0').length - 1, 7,
    'Unexpected built Runtime version literal count before isolated SEA construction.');
  const versionedSource = join(root, 'versioned-runtime.cjs');
  const seaBlob = join(root, 'versioned-runtime.blob');
  const seaConfig = join(root, 'versioned-sea-config.json');
  const executable = join(root, 'versioned-runtime');
  await writeFile(versionedSource, source.replaceAll('0.1.0', versionedSeaVersion));
  await writeFile(seaConfig, JSON.stringify({
    main: versionedSource, output: seaBlob, disableExperimentalSEAWarning: true,
    useCodeCache: false, useSnapshot: false,
  }));
  await execFileAsync(process.execPath, ['--experimental-sea-config', seaConfig]);
  await copyFile(process.execPath, executable);
  await execFileAsync('codesign', ['--remove-signature', executable]);
  const postject = resolve(import.meta.dirname, '../../runtime/node_modules/postject/dist/cli.js');
  await execFileAsync(process.execPath, [postject, executable, 'NODE_SEA_BLOB', seaBlob,
    '--sentinel-fuse', SEA_SENTINEL_FUSE, '--macho-segment-name', 'NODE_SEA']);
  await execFileAsync('codesign', ['--sign', '-', '--force', executable]);
  assert.equal((await execFileAsync(executable, ['--version'])).stdout.trim(), versionedSeaVersion);
  return executable;
}

async function cdpSocket(url) {
  const socket = new WebSocket(url);
  await new Promise((done, reject) => {
    socket.addEventListener('open', done, { once: true });
    socket.addEventListener('error', reject, { once: true });
  });
  let sequence = 0;
  const pending = new Map();
  socket.addEventListener('message', (event) => {
    const response = JSON.parse(event.data);
    const waiter = pending.get(response.id);
    if (!waiter) return;
    pending.delete(response.id);
    if (response.error) waiter.reject(new Error(response.error.message));
    else waiter.resolve(response.result);
  });
  return {
    async command(method, params = {}) {
      return new Promise((done, reject) => {
        const id = ++sequence;
        pending.set(id, { resolve: done, reject });
        socket.send(JSON.stringify({ id, method, params }));
      });
    },
    close() { socket.close(); },
  };
}

function runtimePid(electronPid) {
  const output = execFileSync('ps', ['-axo', 'pid=,ppid=,command='], { encoding: 'utf8' });
  for (const line of output.split('\n')) {
    const match = /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(line);
    if (match && Number(match[2]) === electronPid && match[3].includes('YuanpuAgentRuntime-darwin-arm64')) {
      return Number(match[1]);
    }
    if (match && Number(match[2]) === electronPid && match[3].includes(join(userData, 'runtime', 'versions'))) {
      return Number(match[1]);
    }
  }
  return undefined;
}

function alive(pid) {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

async function start() {
  const port = await freePort();
  const child = spawn(executable, [`--user-data-dir=${userData}`, `--remote-debugging-port=${port}`], {
    env: { ...process.env, YUANPU_HOME: home, YUANPU_NOTIFICATIONS_ENABLED: '0', TASK_021_PROVIDER_KEY: 'fixture-only' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const diagnostics = [];
  for (const stream of [child.stdout, child.stderr]) stream.on('data', (chunk) => {
    if (diagnostics.length < 20) diagnostics.push(chunk.toString().slice(0, 500));
  });
  const app = { child, page: undefined, browser: undefined, runtimePid: undefined,
    workerPid: undefined, diagnostics };
  apps.push(app);
  try {
    const targets = await eventually(async () => {
      try {
        const response = await fetch(`http://127.0.0.1:${port}/json/list`);
        return (await response.json()).find((item) => item.type === 'page' && item.webSocketDebuggerUrl);
      } catch { return undefined; }
    }, 'Packaged App debugger did not become ready.');
    app.page = await cdpSocket(targets.webSocketDebuggerUrl);
    await app.page.command('Runtime.enable');
    const browserInfo = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
    app.browser = await cdpSocket(browserInfo.webSocketDebuggerUrl);
    await eventually(async () => {
      try { return (await evaluate(app, 'window.yuanpu.runtimeInfo()'))?.protocolVersion; }
      catch { return false; }
    }, 'Packaged App preload/Runtime did not become ready.');
    app.runtimePid = await eventually(() => runtimePid(child.pid), 'Packaged Runtime child was not found.');
    app.workerPid = await eventually(() => {
      const output = execFileSync('ps', ['-axo', 'pid=,ppid=,command='], { encoding: 'utf8' });
      for (const line of output.split('\n')) {
        const match = /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(line);
        if (match && Number(match[2]) === app.runtimePid
          && match[3].includes('--assistant-worker')) return Number(match[1]);
      }
      return undefined;
    }, 'Packaged Assistant Worker child was not found.');
    return app;
  } catch (error) { throw new Error(`${error.message}; diagnostics=${diagnostics.join('').slice(-800)}`); }
}

async function evaluate(app, expression) {
  const reply = await app.page.command('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  if (reply.exceptionDetails) throw new Error(`Packaged renderer evaluation failed: ${reply.exceptionDetails.text}`);
  return reply.result.value;
}

async function stop(app) {
  const exited = new Promise((done, reject) => {
    const timeout = setTimeout(() => reject(new Error('Packaged App did not quit.')), 15_000);
    app.child.once('exit', (code) => {
      clearTimeout(timeout);
      code === 0 ? done() : reject(new Error(`Packaged App exit code ${code}.`));
    });
  });
  void app.browser.command('Browser.close').catch(() => undefined);
  await exited;
  app.page.close();
  app.browser.close();
  await eventually(() => !alive(app.runtimePid), 'Packaged Runtime child survived App quit.');
  await eventually(() => !alive(app.workerPid), 'Packaged Assistant Worker survived App quit.');
}

try {
  const providerPort = await freePort();
  await new Promise((done) => provider.listen(providerPort, '127.0.0.1', done));
  await mkdir(join(home, 'app'), { recursive: true });
  await mkdir(userData, { recursive: true });
  await mkdir(workspace, { recursive: true });
  await writeFile(join(home, 'app', 'config.json'), JSON.stringify({
    schemaVersion: 1, provider: 'task-021-fixture', model: 'fixture-model',
    apiKeyEnv: 'TASK_021_PROVIDER_KEY', workingDirectory: workspace,
    baseUrl: `http://127.0.0.1:${providerPort}/v1`, api: 'openai-completions',
  }));
  const first = await start();
  assert.equal((await evaluate(first, 'window.yuanpu.runtimeInfo()')).protocolVersion, 7);
  assert.equal(await evaluate(first, 'window.yuanpu.runtimeRecoveryNotice().then((notice) => notice === undefined)'), true);
  assert.equal(await evaluate(first, "document.querySelector('.runtime-recovery-notice') === null"), true);
  const input = {
    contractVersion: 1, name: 'Packaged UI fixture', prompt: 'synthetic', workspaceId: workspace,
    timing: { kind: 'once', at: '2099-01-01T00:00:00.000Z' }, timeZone: 'UTC', delivery: { kind: 'desktop' },
  };
  const created = await evaluate(first, `window.yuanpu.createSchedule(${JSON.stringify(input)})`);
  assert.equal((await evaluate(first, 'window.yuanpu.listSchedules()')).length, 1);
  const connection = await evaluate(first, `window.yuanpu.saveWecomConnection(${JSON.stringify({
    connectionId: 'imc_task021_packaged', botId: 'synthetic-bot', enabled: false,
  })})`);
  assert.equal(connection.status, 'disabled');
  const chat = await evaluate(first, "window.yuanpu.chat('Synthetic packaged conversation')");
  assert.equal(chat.message.includes('Synthetic packaged reply.'), true);
  assert.equal(workProviderBodies.length, 1);
  const initialDatabase = new DatabaseSync(join(home, 'workflows', 'automation.sqlite'), { readOnly: true });
  const desktopRun = initialDatabase.prepare("SELECT run_id, status FROM yp_agent_runs WHERE entry_point = 'desktop' ORDER BY created_at DESC LIMIT 1").get();
  assert.equal(desktopRun?.status, 'succeeded');
  assert.equal(initialDatabase.prepare("SELECT COUNT(*) AS count FROM yp_conversation_bindings WHERE entry_point = 'desktop'").get().count, 1);
  initialDatabase.close();
  await eventually(async () => evaluate(first, "Array.from(document.querySelectorAll('.nav-item')).some((button) => button.textContent.includes('定时任务'))"), 'Packaged renderer navigation did not mount.');
  assert.equal(await evaluate(first, "document.querySelector('.runtime-recovery-notice') === null"), true);
  await evaluate(first, "Array.from(document.querySelectorAll('.nav-item')).find((button) => button.textContent.includes('定时任务')).click()");
  await eventually(async () => evaluate(first, "document.body.innerText.includes('Packaged UI fixture')"), 'Schedule missing from packaged App UI.');
  await stop(first);

  const canonicalUserData = await realpath(userData);
  const stagedRoot = join(canonicalUserData, 'runtime', '.staging');
  await mkdir(stagedRoot, { recursive: true });
  const stagedSea = join(stagedRoot, 'runtime');
  await copyFile(await buildVersionedSea(), stagedSea);
  await writeFile(join(stagedRoot, 'staged.json'), JSON.stringify({
    version: versionedSeaVersion, filename: 'runtime',
    sha256: createHash('sha256').update(await readFile(stagedSea)).digest('hex'),
  }));
  const second = await start();
  assert.equal(await evaluate(second, 'window.yuanpu.runtimeRecoveryNotice().then((notice) => notice === undefined)'), true);
  assert.equal(await evaluate(second, "document.querySelector('.runtime-recovery-notice') === null"), true);
  const active = JSON.parse(await readFile(join(canonicalUserData, 'runtime', 'current.json'), 'utf8'));
  assert.equal(active.version, versionedSeaVersion);
  assert.equal(active.executable, join(canonicalUserData, 'runtime', 'versions', versionedSeaVersion, 'YuanpuAgentRuntime'));
  assert.equal((await evaluate(second, 'window.yuanpu.listSchedules()'))[0].scheduleId, created.scheduleId);
  assert.equal((await evaluate(second, `window.yuanpu.getAgentRun(${JSON.stringify(desktopRun.run_id)})`)).status, 'succeeded');
  assert.equal((await evaluate(second, 'window.yuanpu.listWecomConnections()')).connections.length, 1);
  assert.equal(workProviderBodies.length, 1);
  const continuedChat = await evaluate(second, "window.yuanpu.chat('Synthetic packaged continuation')");
  assert.equal(continuedChat.message.includes('Synthetic packaged reply.'), true);
  assert.equal(workProviderBodies.length, 2);
  const continuedContext = JSON.stringify(workProviderBodies[1].messages);
  assert.equal(continuedContext.includes('Synthetic packaged conversation'), true);
  assert.equal(continuedContext.includes('Synthetic packaged reply.'), true);
  await eventually(async () => evaluate(second, "Array.from(document.querySelectorAll('.nav-item')).some((button) => button.textContent.includes('定时任务'))"), 'Restarted packaged navigation did not mount.');
  assert.equal(await evaluate(second, "document.querySelector('.runtime-recovery-notice') === null"), true);
  await evaluate(second, "Array.from(document.querySelectorAll('.nav-item')).find((button) => button.textContent.includes('定时任务')).click()");
  await eventually(async () => evaluate(second, "document.body.innerText.includes('Packaged UI fixture')"), 'Persisted schedule missing from restarted packaged UI.');
  await new Promise((done) => setTimeout(done, 2_500));
  await assert.rejects(readFile(join(canonicalUserData, 'runtime', 'activation-pending.json')), { code: 'ENOENT' });
  const database = new DatabaseSync(join(home, 'workflows', 'automation.sqlite'), { readOnly: true });
  assert.equal(database.prepare('SELECT COUNT(*) AS count FROM yp_schedules').get().count, 1);
  assert.equal(database.prepare("SELECT COUNT(*) AS count FROM yp_conversation_bindings WHERE entry_point = 'desktop'").get().count, 1);
  assert.equal(database.prepare("SELECT COUNT(*) AS count FROM yp_agent_runs WHERE entry_point = 'desktop' AND status = 'succeeded'").get().count, 2);
  database.close();
  await stop(second);

  const incompatible = join(root, 'incompatible-runtime');
  await writeFile(incompatible, '#!/bin/sh\nif [ "$1" = "--version" ]; then echo 9.9.9; else echo \'{"event":"ready","protocolVersion":999,"version":"9.9.9","host":"127.0.0.1","port":0}\'; sleep 10; fi\n');
  await chmod(incompatible, 0o755);
  await mkdir(stagedRoot, { recursive: true });
  const incompatibleStaged = join(stagedRoot, 'runtime');
  await copyFile(incompatible, incompatibleStaged);
  await writeFile(join(stagedRoot, 'staged.json'), JSON.stringify({
    version: '9.9.9', filename: 'runtime',
    sha256: createHash('sha256').update(await readFile(incompatibleStaged)).digest('hex'),
  }));
  const third = await start();
  const rolledBack = JSON.parse(await readFile(join(canonicalUserData, 'runtime', 'current.json'), 'utf8'));
  assert.deepEqual(rolledBack, active);
  assert.equal((await evaluate(third, 'window.yuanpu.listSchedules()'))[0].scheduleId, created.scheduleId);
  assert.equal((await evaluate(third, `window.yuanpu.getAgentRun(${JSON.stringify(desktopRun.run_id)})`)).status, 'succeeded');
  assert.equal(third.diagnostics.join('').includes('Runtime protocol is incompatible'), true);
  assert.equal(await evaluate(third,
    "window.yuanpu.runtimeRecoveryNotice().then((notice) => notice?.kind === 'incompatible_protocol')"), true);
  const visibleIncompatibilityNotice = await eventually(async () => evaluate(third,
    "(() => { const notice = document.querySelector('.runtime-recovery-notice'); return Boolean(notice && notice.innerText.includes('Runtime 协议不兼容') && notice.innerText.includes('已恢复上一版本') && notice.getClientRects().length > 0 && getComputedStyle(notice).visibility === 'visible'); })()"),
  'Recovered incompatible Runtime notice was not visible in packaged UI.');
  assert.equal(await evaluate(third,
    `!document.querySelector('.runtime-recovery-notice').innerText.includes('999') && !document.querySelector('.runtime-recovery-notice').innerText.includes(${JSON.stringify(root)})`), true);
  await evaluate(third, "document.querySelector('.runtime-recovery-notice button[aria-label=\"关闭 Runtime 更新提示\"]').click()");
  await eventually(async () => evaluate(third, "document.querySelector('.runtime-recovery-notice') === null"),
    'Runtime recovery notice could not be dismissed.');
  await stop(third);

  const genericMarker = 'TASK021_SYNTHETIC_DIAGNOSTIC_MARKER';
  const genericFailure = join(root, 'generic-failure-runtime');
  await writeFile(genericFailure,
    `#!/bin/sh\nif [ "$1" = "--version" ]; then echo 9.9.10; else echo ${genericMarker} >&2; exit 9; fi\n`);
  await chmod(genericFailure, 0o755);
  await mkdir(stagedRoot, { recursive: true });
  const genericStaged = join(stagedRoot, 'runtime');
  await copyFile(genericFailure, genericStaged);
  await writeFile(join(stagedRoot, 'staged.json'), JSON.stringify({
    version: '9.9.10', filename: 'runtime',
    sha256: createHash('sha256').update(await readFile(genericStaged)).digest('hex'),
  }));
  const fourth = await start();
  assert.deepEqual(JSON.parse(await readFile(join(canonicalUserData, 'runtime', 'current.json'), 'utf8')), active);
  assert.equal((await evaluate(fourth, 'window.yuanpu.listSchedules()'))[0].scheduleId, created.scheduleId);
  assert.equal((await evaluate(fourth, `window.yuanpu.getAgentRun(${JSON.stringify(desktopRun.run_id)})`)).status, 'succeeded');
  assert.equal(fourth.diagnostics.join('').includes(genericMarker), true);
  assert.equal(await evaluate(fourth,
    "window.yuanpu.runtimeRecoveryNotice().then((notice) => notice?.kind === 'activation_failed')"), true);
  await eventually(async () => evaluate(fourth,
    "(() => { const notice = document.querySelector('.runtime-recovery-notice'); return Boolean(notice && notice.innerText.includes('Runtime 更新失败') && notice.innerText.includes('已恢复上一版本') && notice.getClientRects().length > 0); })()"),
  'Generic update failure notice was not visible in packaged UI.');
  assert.equal(await evaluate(fourth,
    `!document.querySelector('.runtime-recovery-notice').innerText.includes(${JSON.stringify(genericMarker)}) && !document.querySelector('.runtime-recovery-notice').innerText.includes(${JSON.stringify(root)})`), true);
  await evaluate(fourth, "document.querySelector('.runtime-recovery-notice button[aria-label=\"关闭 Runtime 更新提示\"]').click()");
  await eventually(async () => evaluate(fourth, "document.querySelector('.runtime-recovery-notice') === null"),
    'Generic Runtime recovery notice could not be dismissed.');
  const recoveredChat = await evaluate(fourth, "window.yuanpu.chat('Synthetic packaged after rollback')");
  assert.equal(recoveredChat.message.includes('Synthetic packaged reply.'), true);
  assert.equal(workProviderBodies.length, 3);
  const recoveredContext = JSON.stringify(workProviderBodies[2].messages);
  assert.equal(recoveredContext.includes('Synthetic packaged conversation'), true);
  assert.equal(recoveredContext.includes('Synthetic packaged continuation'), true);
  await stop(fourth);
  console.log(JSON.stringify({
    status: 'passed', packagedAppLaunches: 4, persistedScheduleVisible: true,
    stagedSeaConfirmed: true, versionDistinctSeaConfirmed: true,
    incompatibleCandidateRolledBack: true,
    visibleIncompatibilityNotice, genericFailureSanitized: true, runtimeChildrenStopped: true,
    desktopRunAndBindingRetained: true, sessionContextContinued: true,
    rollbackSessionContextContinued: true,
    disabledConnectionRetained: true, providerRequests,
  }));
} finally {
  for (const app of apps) {
    app.page?.close();
    app.browser?.close();
    if (app.child.exitCode === null && app.child.signalCode === null) app.child.kill('SIGKILL');
    if (app.runtimePid && alive(app.runtimePid)) {
      const command = execFileSync('ps', ['-p', String(app.runtimePid), '-o', 'command='], { encoding: 'utf8' });
      if (command.includes(root) || command.includes(bundledSea)) process.kill(app.runtimePid, 'SIGKILL');
    }
    if (app.workerPid && alive(app.workerPid)) process.kill(app.workerPid, 'SIGKILL');
  }
  provider.closeAllConnections();
  await new Promise((done) => provider.close(done));
  await rm(root, { recursive: true, force: true });
}
