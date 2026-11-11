import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { access, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const bundle = resolve(import.meta.dirname, '../release/mac-arm64/南医小助.app');
const executable = join(bundle, 'Contents', 'MacOS', 'nanyi-xiaozhu');
const root = await mkdtemp(join(tmpdir(), 'yuanpu-task-042-app-exit-'));
const home = join(root, 'home');
const userData = join(root, 'user-data');
const workspace = join(root, 'workspace');
let app;
let runtimePid;
let workerPid;

function children(parentPid) {
  const output = execFileSync('ps', ['-axo', 'pid=,ppid=,command='], { encoding: 'utf8' });
  return output.split('\n').flatMap((line) => {
    const match = /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(line);
    return match && Number(match[2]) === parentPid
      ? [{ pid: Number(match[1]), command: match[3] }] : [];
  });
}

function alive(pid) {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

async function eventually(check, message, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const value = await check();
    if (value) return value;
    await new Promise((resolveWait) => setTimeout(resolveWait, 50));
  }
  throw new Error(message);
}

async function freePort() {
  const server = createServer();
  await new Promise((resolveListen) => server.listen(0, '127.0.0.1', resolveListen));
  const port = server.address().port;
  await new Promise((resolveClose) => server.close(resolveClose));
  return port;
}

try {
  await Promise.all([mkdir(join(home, 'app'), { recursive: true }), mkdir(userData), mkdir(workspace)]);
  await writeFile(join(home, 'app', 'config.json'), JSON.stringify({ schemaVersion: 1,
    provider: 'fixture', model: 'fixture-model', workingDirectory: workspace }));
  await assert.rejects(access(join(home, 'app', 'connections', 'wecom.json')));
  assert.equal(children(process.pid).some((item) => item.command.includes('YuanpuAgentRuntime')), false);
  const port = await freePort();
  app = spawn(executable, [`--user-data-dir=${userData}`, `--remote-debugging-port=${port}`], {
    env: { ...process.env, YUANPU_HOME: home, YUANPU_NOTIFICATIONS_ENABLED: '0',
      YUANPU_PYTHON_MCP_EXECUTABLE: '', YUANPU_PYTHON_MCP_ROOT: '' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let diagnostics = '';
  for (const stream of [app.stdout, app.stderr]) stream.on('data', (chunk) => {
    if (diagnostics.length < 1000) diagnostics += chunk.toString().slice(0, 1000 - diagnostics.length);
  });
  runtimePid = await eventually(() => children(app.pid).find((item) =>
    item.command.includes('YuanpuAgentRuntime-darwin-arm64'))?.pid,
  'Packaged App did not start its Runtime').catch((error) => {
    throw new Error(`${error.message}; diagnostic length=${diagnostics.length}`);
  });
  workerPid = await eventually(() => children(runtimePid).find((item) =>
    item.command.includes('YuanpuAgentRuntime-darwin-arm64')
      && item.command.includes('--assistant-worker'))?.pid,
  'Packaged Runtime did not start Assistant Worker');
  assert.equal(alive(app.pid) && alive(runtimePid) && alive(workerPid), true);
  const browserTarget = await eventually(async () => {
    try { return (await (await fetch(`http://127.0.0.1:${port}/json/version`)).json()).webSocketDebuggerUrl; }
    catch { return undefined; }
  }, 'Packaged App CDP endpoint did not appear');
  const browser = new WebSocket(browserTarget);
  await new Promise((resolveOpen, rejectOpen) => {
    browser.addEventListener('open', resolveOpen, { once: true });
    browser.addEventListener('error', rejectOpen, { once: true });
  });
  const pageTarget = await eventually(async () => {
    try { return (await (await fetch(`http://127.0.0.1:${port}/json/list`)).json())
      .find((item) => item.type === 'page' && item.webSocketDebuggerUrl)?.webSocketDebuggerUrl; }
    catch { return undefined; }
  }, 'Packaged App page did not appear');
  const page = new WebSocket(pageTarget);
  await new Promise((resolveOpen, rejectOpen) => {
    page.addEventListener('open', resolveOpen, { once: true });
    page.addEventListener('error', rejectOpen, { once: true });
  });
  let cdpId = 0;
  const command = (socket, method, params = {}) => new Promise((resolveResult, rejectResult) => {
    const id = ++cdpId;
    const timeout = setTimeout(() => rejectResult(new Error(`CDP ${method} timed out`)), 5_000);
    const onMessage = (event) => {
      const message = JSON.parse(event.data);
      if (message.id !== id) return;
      clearTimeout(timeout);
      socket.removeEventListener('message', onMessage);
      if (message.error) rejectResult(new Error(`CDP ${method} failed`));
      else resolveResult(message.result);
    };
    socket.addEventListener('message', onMessage);
    socket.send(JSON.stringify({ id, method, params }));
  });
  await command(page, 'Runtime.enable');
  await eventually(async () => {
    try {
      const reply = await command(page, 'Runtime.evaluate', {
        expression: 'window.yuanpu.runtimeInfo().then((info) => ({ protocolVersion: info.protocolVersion, configRoot: info.configRoot }))',
        awaitPromise: true, returnByValue: true,
      });
      return reply.result.value?.protocolVersion === 7 && reply.result.value?.configRoot === home;
    } catch { return false; }
  }, 'Packaged App preload and Runtime did not become ready');
  const exited = new Promise((resolveExit, rejectExit) => {
    const timeout = setTimeout(() => rejectExit(new Error('Packaged App did not quit normally')), 20_000);
    app.once('exit', (code) => { clearTimeout(timeout); resolveExit(code); });
  });
  void command(browser, 'Browser.close').catch(() => undefined);
  assert.equal(await exited, 0);
  page.close();
  browser.close();
  await eventually(() => !alive(runtimePid) && !alive(workerPid),
    'Runtime or Assistant Worker survived packaged App quit');
  await assert.rejects(access(join(home, 'app', 'connections', 'wecom.json')));
  console.log(JSON.stringify({ status: 'passed', packagedAppExit: 'normal',
    runtimeStopped: true, assistantWorkerStopped: true, isolatedHome: true,
    wecomConnectionConfigPresent: false }));
} finally {
  if (app?.exitCode === null && app?.signalCode === null) app.kill('SIGKILL');
  for (const pid of [workerPid, runtimePid]) {
    if (pid && alive(pid)) process.kill(pid, 'SIGKILL');
  }
  await rm(root, { recursive: true, force: true });
}
