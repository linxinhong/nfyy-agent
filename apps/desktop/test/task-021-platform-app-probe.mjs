import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

const release = resolve(import.meta.dirname, '../release');
const root = await mkdtemp(join(tmpdir(), 'yuanpu-task-021-platform-app-'));
const home = join(root, 'home');
const userData = join(root, 'user-data');
const workspace = join(root, 'workspace');
const scheduleName = 'TASK-021 platform App probe';
const apps = [];
let providerRequests = 0;

async function packagedExecutable() {
  if (process.platform === 'darwin') {
    return join(release, `mac-${process.arch}`, 'MindLink.app', 'Contents', 'MacOS', 'MindLink');
  }
  const unpacked = join(release, process.platform === 'win32' ? 'win-unpacked' : 'linux-unpacked');
  const candidates = (await readdir(unpacked)).filter((name) =>
    /^mindlink(?:\.exe)?$/i.test(name));
  assert.equal(candidates.length, 1, `Expected one packaged executable in ${unpacked}`);
  return join(unpacked, candidates[0]);
}

async function eventually(check, message, timeoutMs = 30_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const result = await check();
    if (result) return result;
    await new Promise((done) => setTimeout(done, 50));
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

async function cdpSocket(url) {
  const socket = new WebSocket(url);
  await new Promise((done, reject) => {
    socket.addEventListener('open', done, { once: true });
    socket.addEventListener('error', reject, { once: true });
  });
  let sequence = 0;
  const pending = new Map();
  socket.addEventListener('message', (event) => {
    const reply = JSON.parse(event.data);
    const waiter = pending.get(reply.id);
    if (!waiter) return;
    pending.delete(reply.id);
    if (reply.error) waiter.reject(new Error(reply.error.message));
    else waiter.resolve(reply.result);
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

async function evaluate(app, expression) {
  const reply = await app.page.command('Runtime.evaluate', {
    expression, awaitPromise: true, returnByValue: true,
  });
  if (reply.exceptionDetails) throw new Error(`Packaged renderer evaluation failed: ${reply.exceptionDetails.text}`);
  return reply.result.value;
}

async function start(executable) {
  const port = await freePort();
  const child = spawn(executable, [`--user-data-dir=${userData}`, `--remote-debugging-port=${port}`], {
    env: {
      ...process.env,
      YUANPU_HOME: home,
      YUANPU_NOTIFICATIONS_ENABLED: '0',
      TASK_021_PROVIDER_KEY: 'fixture-only',
      ...(process.platform === 'linux' ? { ELECTRON_DISABLE_SANDBOX: '1' } : {}),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const diagnostics = [];
  for (const stream of [child.stdout, child.stderr]) stream.on('data', (chunk) => {
    if (diagnostics.length < 20) diagnostics.push(chunk.toString().slice(0, 500));
  });
  const app = { child, page: undefined, browser: undefined, diagnostics };
  apps.push(app);
  try {
    const target = await eventually(async () => {
      try {
        const response = await fetch(`http://127.0.0.1:${port}/json/list`);
        return (await response.json()).find((item) => item.type === 'page' && item.webSocketDebuggerUrl);
      } catch { return undefined; }
    }, 'Packaged App debugger did not become ready.');
    app.page = await cdpSocket(target.webSocketDebuggerUrl);
    await app.page.command('Runtime.enable');
    const browserInfo = await (await fetch(`http://127.0.0.1:${port}/json/version`)).json();
    app.browser = await cdpSocket(browserInfo.webSocketDebuggerUrl);
    await eventually(async () => {
      try { return (await evaluate(app, 'window.yuanpu.runtimeInfo()'))?.protocolVersion; }
      catch { return false; }
    }, 'Packaged App preload/Runtime did not become ready.');
    return app;
  } catch (error) {
    throw new Error(`${error.message}; diagnostics=${diagnostics.join('').slice(-800)}`);
  }
}

async function stop(app) {
  const exited = new Promise((done, reject) => {
    const timeout = setTimeout(() => reject(new Error('Packaged App did not quit.')), 20_000);
    app.child.once('exit', (code) => {
      clearTimeout(timeout);
      code === 0 ? done() : reject(new Error(`Packaged App exit code ${code}.`));
    });
  });
  void app.browser.command('Browser.close').catch(() => undefined);
  await exited;
  app.page.close();
  app.browser.close();
}

const provider = createServer(async (request, response) => {
  if (request.method !== 'POST' || request.url !== '/v1/chat/completions') {
    response.writeHead(404).end();
    return;
  }
  for await (const _chunk of request) { /* Synthetic prompt only. */ }
  providerRequests += 1;
  response.writeHead(200, { 'content-type': 'text/event-stream' });
  const chunk = (delta, finish_reason = null) => `data: ${JSON.stringify({
    id: 'task-021-platform', object: 'chat.completion.chunk', created: 1,
    model: 'fixture-model', choices: [{ index: 0, delta, finish_reason }],
  })}\n\n`;
  response.write(chunk({ role: 'assistant', content: 'Synthetic platform reply.' }));
  response.end(chunk({}, 'stop') + 'data: [DONE]\n\n');
});

try {
  const executable = await packagedExecutable();
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

  const first = await start(executable);
  assert.equal((await evaluate(first, 'window.yuanpu.runtimeInfo()')).protocolVersion, 7);
  const created = await evaluate(first, `window.yuanpu.createSchedule(${JSON.stringify({
    contractVersion: 1, name: scheduleName, prompt: 'synthetic', workspaceId: workspace,
    timing: { kind: 'once', at: '2099-01-01T00:00:00.000Z' },
    timeZone: 'UTC', delivery: { kind: 'desktop' },
  })})`);
  const chat = await evaluate(first, "window.yuanpu.chat('Synthetic platform conversation')");
  assert.equal(chat.message.includes('Synthetic platform reply.'), true);
  assert.equal(providerRequests, 1);
  await eventually(async () => evaluate(first,
    "Array.from(document.querySelectorAll('.nav-item')).some((button) => button.textContent.includes('定时任务'))"),
  'Packaged renderer navigation did not mount.');
  await evaluate(first,
    "Array.from(document.querySelectorAll('.nav-item')).find((button) => button.textContent.includes('定时任务')).click()");
  await eventually(async () => evaluate(first, `document.body.innerText.includes(${JSON.stringify(scheduleName)})`),
    'Created schedule was not visible in packaged App UI.');
  await stop(first);

  const second = await start(executable);
  assert.equal((await evaluate(second, 'window.yuanpu.listSchedules()'))[0].scheduleId, created.scheduleId);
  await eventually(async () => evaluate(second,
    "Array.from(document.querySelectorAll('.nav-item')).some((button) => button.textContent.includes('定时任务'))"),
  'Restarted packaged renderer navigation did not mount.');
  await evaluate(second,
    "Array.from(document.querySelectorAll('.nav-item')).find((button) => button.textContent.includes('定时任务')).click()");
  await eventually(async () => evaluate(second, `document.body.innerText.includes(${JSON.stringify(scheduleName)})`),
    'Persisted schedule was not visible after App restart.');
  const database = new DatabaseSync(join(home, 'workflows', 'automation.sqlite'), { readOnly: true });
  assert.equal(database.prepare('SELECT COUNT(*) AS count FROM yp_schedules').get().count, 1);
  assert.equal(database.prepare("SELECT COUNT(*) AS count FROM yp_conversation_bindings WHERE entry_point = 'desktop'").get().count, 1);
  assert.equal(database.prepare("SELECT COUNT(*) AS count FROM yp_agent_runs WHERE entry_point = 'desktop' AND status = 'succeeded'").get().count, 1);
  database.close();
  assert.equal(providerRequests, 1);
  await stop(second);
  console.log(JSON.stringify({
    status: 'passed', platform: `${process.platform}-${process.arch}`,
    packagedAppLaunches: 2, scheduleVisibleAfterRestart: true,
    desktopRunAndBindingRetained: true, providerRequests,
  }));
} finally {
  for (const app of apps) {
    app.page?.close();
    app.browser?.close();
    if (app.child.exitCode === null && app.child.signalCode === null) app.child.kill('SIGKILL');
  }
  provider.closeAllConnections();
  await new Promise((done) => provider.close(done));
  await rm(root, { recursive: true, force: true });
}
