import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdir, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { homedir, tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

import { forwardGoCatalog, startGoCatalog } from './go-catalog-fixture.mjs';

const rendererUrl = process.env.TASK_036_RENDERER_URL;
if (!rendererUrl || !/^http:\/\/127\.0\.0\.1:\d+\/$/.test(rendererUrl)) {
  throw new Error('Set TASK_036_RENDERER_URL to the isolated worktree Vite URL.');
}
const require = createRequire(import.meta.url);
const electron = require('electron');
const desktopRoot = resolve(import.meta.dirname, '..');
const runtimeDist = resolve(desktopRoot, '../runtime/dist');
const desktopMain = join(desktopRoot, 'dist/main.cjs');

async function eventually(check, message, timeoutMs = 15_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const result = await check();
    if (result) return result;
    await new Promise((resolveWait) => setTimeout(resolveWait, 40));
  }
  throw new Error(message);
}

async function listen(server) {
  await new Promise((resolveListen) => server.listen(0, '127.0.0.1', resolveListen));
  return server.address().port;
}

async function freePort() {
  const server = createServer();
  const port = await listen(server);
  await new Promise((resolveClose) => server.close(resolveClose));
  return port;
}

async function connectRenderer(port) {
  const target = await eventually(async () => {
    try {
      const response = await fetch(`http://127.0.0.1:${port}/json/list`);
      return (await response.json()).find((item) => item.type === 'page' && item.webSocketDebuggerUrl);
    } catch { return undefined; }
  }, 'Electron renderer debugger did not become ready.');
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolveOpen, rejectOpen) => {
    socket.addEventListener('open', resolveOpen, { once: true });
    socket.addEventListener('error', rejectOpen, { once: true });
  });
  let nextId = 0;
  const pending = new Map();
  socket.addEventListener('message', (event) => {
    const message = JSON.parse(event.data);
    if (!message.id || !pending.has(message.id)) return;
    const { resolveReply, rejectReply } = pending.get(message.id);
    pending.delete(message.id);
    if (message.error) rejectReply(new Error(message.error.message));
    else resolveReply(message.result);
  });
  const command = (method, params = {}) => new Promise((resolveReply, rejectReply) => {
    const id = ++nextId;
    pending.set(id, { resolveReply, rejectReply });
    socket.send(JSON.stringify({ id, method, params }));
  });
  await command('Runtime.enable');
  return {
    async evaluate(expression, awaitPromise = true) {
      const reply = await command('Runtime.evaluate', { expression, awaitPromise, returnByValue: true });
      if (reply.exceptionDetails) throw new Error(`Renderer evaluation failed: ${reply.exceptionDetails.exception?.description ?? reply.exceptionDetails.text}`);
      return reply.result.value;
    },
    command,
    close: () => socket.close(),
  };
}

function childRuntimePid(electronPid) {
  const output = execFileSync('ps', ['-axo', 'pid=,ppid=,command='], { encoding: 'utf8' });
  for (const line of output.split('\n')) {
    const match = /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(line);
    if (match && Number(match[2]) === electronPid && match[3].includes('index.cjs --serve --port 0')) {
      return Number(match[1]);
    }
  }
  return undefined;
}

function exists(pid) {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

const root = await mkdtemp(join(tmpdir(), 'yuanpu-task-036-electron-'));
const appRoot = join(root, 'apps', 'desktop');
const home = join(root, 'home');
const workspace = join(root, 'workspace');
const userData = join(root, 'desktop-user-data');
let providerRequests = 0;
const live = process.env.TASK_036_LIVE === '1';
const fullUiFixture = process.env.TASK_008_FULL_UI_FIXTURE;
const approvalFixture = process.env.TASK_008_APPROVAL_FIXTURE === '1' || Boolean(fullUiFixture);
const screenshots = approvalFixture ? join(root, 'screenshots') : resolve(desktopRoot, '../../docs/frontend/evidence/task-036');
let mode = 'success';
let approvalDecision = 'approved';
const catalogOptions = fullUiFixture ? { artifactRoot: join(fullUiFixture, '0.1.0') } : undefined;
let catalog;
let catalogFault;
const catalogServer = catalogOptions ? createServer(async (request, response) => {
  if (catalogFault === 'signature' && request.url?.endsWith('/manifest')) {
    const manifest = JSON.parse(await readFile(join(catalogOptions.artifactRoot, 'manifest.json'), 'utf8'));
    manifest.signature.value = Buffer.alloc(64).toString('base64');
    response.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(manifest));
    return;
  }
  await forwardGoCatalog(catalog.origin, request, response);
}) : undefined;
const provider = createServer(async (request, response) => {
  if (request.method !== 'POST' || request.url !== '/v1/chat/completions') {
    response.writeHead(404).end(); return;
  }
  let requestBody = '';
  for await (const chunk of request) {
    requestBody += chunk.toString();
    if (requestBody.length > 256_000) { response.writeHead(413).end(); return; }
  }
  providerRequests += 1;
  if (mode === 'hold') return;
  if (mode === 'failure') { response.writeHead(400).end(JSON.stringify({ error: { message: 'Controlled provider failure' } })); return; }
  response.writeHead(200, { 'content-type': 'text/event-stream' });
  const chunk = (delta, finish_reason = null) => `data: ${JSON.stringify({ id: 'fixture', object: 'chat.completion.chunk', created: 1, model: 'fixture-model', choices: [{ index: 0, delta, finish_reason }] })}\n\n`;
  if (mode === 'approval') {
    const messages = JSON.parse(requestBody).messages;
    const lastUserIndex = messages.findLastIndex((message) => message.role === 'user');
    const currentMessages = messages.slice(lastUserIndex);
    const latestTool = [...currentMessages].reverse().find((message) => message.role === 'tool');
    const toolText = latestTool ? JSON.stringify(latestTool.content) : '';
    const capabilityId = /ypcap:[A-Za-z0-9_-]+:[A-Za-z0-9_-]+/.exec(toolText)?.[0];
    const latestToolCall = [...currentMessages].reverse().find((message) => message.role === 'assistant' && message.tool_calls?.length)?.tool_calls[0]?.function?.name;
    const toolCall = (name, args) => {
      response.write(chunk({ role: 'assistant', tool_calls: [{ index: 0, id: `call_${providerRequests}`, type: 'function', function: { name, arguments: JSON.stringify(args) } }] }));
      response.end(chunk({}, 'tool_calls') + 'data: [DONE]\n\n');
    };
    if (!latestTool) { toolCall('search_capabilities', { query: 'yuanpu_approved_echo' }); return; }
    if (latestToolCall === 'search_capabilities' && capabilityId) {
      toolCall('execute_capability', { name: capabilityId, arguments: { text: `TASK008-${approvalDecision}` } });
      return;
    }
    response.write(chunk({ role: 'assistant', content: `TASK008-${approvalDecision}` }));
    response.end(chunk({}, 'stop') + 'data: [DONE]\n\n');
    return;
  }
  response.write(chunk({ role: 'assistant', content: 'UI fixture reply completed.' }));
  response.end(chunk({}, 'stop') + 'data: [DONE]\n\n');
});
let child;
let renderer;
let runtimePid;
try {
  if (fullUiFixture) {
    const fixture = JSON.parse(await readFile(join(fullUiFixture, 'metadata/fixture.json'), 'utf8'));
    assert.equal(fixture.development, true);
    assert.equal(fixture.target, `${process.platform}-${process.arch}`);
    assert.equal(fixture.sourceCommit, execFileSync('git', ['rev-parse', 'HEAD'], { cwd: resolve(desktopRoot, '../..'), encoding: 'utf8' }).trim());
  }
  await mkdir(appRoot, { recursive: true });
  await mkdir(screenshots, { recursive: true });
  await symlink(resolve(desktopRoot, '../python-capabilities'), join(root, 'apps/python-capabilities'), 'dir');
  await mkdir(join(home, 'app'), { recursive: true });
  await mkdir(userData, { recursive: true });
  await mkdir(workspace, { recursive: true });
  await mkdir(dirname(join(root, 'apps', 'runtime', 'dist')), { recursive: true });
  await symlink(runtimeDist, join(root, 'apps', 'runtime', 'dist'), 'dir');
  await writeFile(join(appRoot, 'package.json'), JSON.stringify({
    name: 'yuanpu-task-036-isolated-app', version: '0.1.0', main: 'entry.cjs',
  }));
  await writeFile(join(appRoot, 'entry.cjs'), `
    const { app } = require('electron');
    app.setPath('userData', ${JSON.stringify(userData)});
    process.on('SIGUSR2', () => app.quit());
    require(${JSON.stringify(desktopMain)});
  `);
  const providerPort = await listen(provider);
  if (catalogOptions) catalog = await startGoCatalog(catalogOptions.artifactRoot);
  const catalogPort = catalogServer ? await listen(catalogServer) : undefined;
  const config = live
    ? JSON.parse(await readFile(join(homedir(), '.yuanpu/app/config.json'), 'utf8'))
    : { schemaVersion: 1, provider: 'task-036-fixture', model: 'fixture-model',
      apiKeyEnv: 'TASK_036_PROVIDER_KEY', baseUrl: `http://127.0.0.1:${providerPort}/v1`, api: 'openai-completions' };
  if (live && !process.env[config.apiKeyEnv]) throw new Error('Configured live model credential is unavailable.');
  await writeFile(join(home, 'app', 'config.json'), JSON.stringify({ ...config, workingDirectory: workspace }));
  const debuggerPort = await freePort();
  child = spawn(electron, [`--remote-debugging-port=${debuggerPort}`, appRoot], {
    env: {
      ...process.env,
      YUANPU_HOME: home,
      YUANPU_RENDERER_URL: rendererUrl,
      YUANPU_NODE_BINARY: process.execPath,
      YUANPU_NOTIFICATIONS_ENABLED: '0',
      TASK_036_PROVIDER_KEY: 'fixture-only',
      ...(catalogPort ? {
        YUANPU_CATALOG_URL: `http://127.0.0.1:${catalogPort}`,
        YUANPU_CAPABILITY_TRUST_ROOT_FILE: join(fullUiFixture, '0.1.0/bundle/trust-root.json'),
      } : {}),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let diagnostics = '';
  for (const stream of [child.stdout, child.stderr]) {
    stream.on('data', (chunk) => { if (diagnostics.length < 8_192) diagnostics += chunk.toString(); });
  }
  try {
    renderer = await connectRenderer(debuggerPort);
    await eventually(async () => {
      try { return (await renderer.evaluate('window.yuanpu.runtimeInfo()')).workingDirectory === workspace; }
      catch { return false; }
    }, 'Isolated Electron bridge did not become ready.');
  } catch (error) {
    throw new Error(`${error.message}; diagnostics=${diagnostics.slice(-700)}`);
  }
  await eventually(() => renderer.evaluate('document.querySelectorAll(".nav-item").length >= 4'), 'Renderer UI did not mount.');
  runtimePid = await eventually(() => childRuntimePid(child.pid), 'Runtime child missing.');
  const connectionId = `imc_${randomUUID()}`;
  const connection = await renderer.evaluate(`window.yuanpu.saveWecomConnection(${JSON.stringify({
    connectionId, botId: 'fixture-bot', enabled: false,
  })})`);
  assert.equal(connection.status, 'disabled');
  assert.equal((await renderer.evaluate('window.yuanpu.listWecomConnections()')).connections.length, 1);
  const input = {
    contractVersion: 1,
    name: 'Fixture scheduled note',
    prompt: 'Summarize the synthetic workspace.',
    workspaceId: workspace,
    timing: { kind: 'once', at: '2099-01-01T00:00:00.000Z' },
    timeZone: 'Asia/Shanghai',
    delivery: { kind: 'desktop' },
  };
  const preview = await renderer.evaluate(`window.yuanpu.previewSchedule(${JSON.stringify(input)})`);
  assert.equal(preview.nextTriggerAt, '2099-01-01T00:00:00.000Z');
  const created = await renderer.evaluate(`window.yuanpu.createSchedule(${JSON.stringify(input)})`);
  assert.equal(created.nextTriggerAt, preview.nextTriggerAt);
  await renderer.evaluate(`window.yuanpu.setScheduleEnabled(${JSON.stringify(created.scheduleId)}, false)`);
  assert.equal((await renderer.evaluate('window.yuanpu.listSchedules()'))[0].enabled, false);
  if (approvalFixture) {
    await renderer.evaluate(`document.querySelector('.nav-item[aria-label="工作"]').click()`);
  } else {
    await renderer.evaluate(`Array.from(document.querySelectorAll('.nav-item')).find((button) => button.textContent.includes('定时任务')).click()`);
    await eventually(async () => (await renderer.evaluate('document.body.innerText')).includes('Fixture scheduled note'), 'Real schedule did not appear in the Electron UI.');
    await renderer.evaluate(`Array.from(document.querySelectorAll('.nav-item')).find((button) => button.textContent.includes('连接')).click()`);
    await eventually(async () => (await renderer.evaluate('document.body.innerText')).includes(connectionId), 'Real connection did not appear in the Electron UI.');
    assert.equal((await renderer.evaluate('document.body.innerText')).includes('fixture-bot'), false);
    await renderer.evaluate(`Array.from(document.querySelectorAll('.nav-item')).find((button) => button.textContent.trim() === '聊天').click()`);
  }
  const body = () => renderer.evaluate('document.body.innerText');
  const click = (selector) => renderer.evaluate(`document.querySelector(${JSON.stringify(selector)}).click()`);
  const fill = async (text) => {
    await renderer.evaluate(`(() => {
      const field = document.querySelector('textarea[aria-label="消息"]');
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(field, ${JSON.stringify(text)});
      field.dispatchEvent(new Event('input', { bubbles: true }));
    })()`);
  };
  const latestRun = async () => {
    const database = new DatabaseSync(join(home, 'workflows/automation.sqlite'), { readOnly: true });
    const row = database.prepare("SELECT run_id FROM yp_agent_runs WHERE entry_point = 'desktop' ORDER BY created_at DESC LIMIT 1").get();
    database.close();
    return row ? renderer.evaluate(`window.yuanpu.getAgentRun(${JSON.stringify(row.run_id)})`) : undefined;
  };
  const runCount = async () => {
    const database = new DatabaseSync(join(home, 'workflows/automation.sqlite'), { readOnly: true });
    const row = database.prepare("SELECT count(*) AS n FROM yp_agent_runs WHERE entry_point = 'desktop'").get();
    database.close(); return row.n;
  };
  const send = async (text) => {
    const before = await runCount();
    await fill(text);
    await click('[aria-label="发送消息"]');
    await eventually(async () => (await runCount()) === before + 1, 'Message was not submitted exactly once.');
  };
  const waitTerminal = async () => {
    const run = await eventually(async () => {
      const current = await latestRun();
      return current && ['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown'].includes(current.status) ? current : undefined;
    }, 'Run did not finish.', live || approvalFixture ? 120_000 : 20_000);
    await eventually(async () => !(await renderer.evaluate('Boolean(document.querySelector(".thinking"))')), 'UI stayed busy after terminal state.');
    return run;
  };
  const shot = async (name) => {
    const image = await renderer.command('Page.captureScreenshot', { format: 'png' });
    await writeFile(join(screenshots, name + '.png'), Buffer.from(image.data, 'base64'));
  };

  await send(live ? '请只回复 UI 验证通过。不要调用任何工具。' : 'Complete a synthetic UI message.');
  const first = await waitTerminal();
  assert.equal(first.status, 'succeeded');
  assert.ok(first.output?.message);
  await eventually(async () => (await body()).includes(first.output.message), 'Model response missing in UI.');

  // Native modal keyboard behavior and all supported viewport bounds.
  for (const [width, height] of approvalFixture ? [] : [[1440, 900], [1280, 800], [1024, 768], [390, 844]]) {
    await renderer.command('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });
    await eventually(async () => await renderer.evaluate(`window.innerWidth === ${width}`), 'Viewport did not update.');
    if (width <= 1100) {
      await eventually(async () => !(await renderer.evaluate('Boolean(document.querySelector(".activity-panel"))')), 'Narrow panel did not collapse.');
      await click('[aria-label="打开会话动态"]');
      await eventually(async () => await renderer.evaluate('Boolean(document.querySelector("dialog:modal"))'), 'Drawer is not modal.');
      await renderer.command('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
      await renderer.command('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
      await eventually(async () => await renderer.evaluate('document.activeElement?.getAttribute("aria-label") === "打开会话动态"'), 'Escape did not restore trigger focus.');
    }
    assert.equal(await renderer.evaluate('document.documentElement.scrollWidth > innerWidth'), false);
    assert.equal(await renderer.evaluate('document.querySelector(".composer").getBoundingClientRect().bottom <= innerHeight'), true);
    await shot(`${live ? 'live' : 'fixture'}-chat-${width}`);
  }
  await renderer.command('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false });
  if (!approvalFixture) await click('[aria-label="打开会话动态"]');

  if (live || approvalFixture) {
    for (const decision of ['approved', 'denied']) {
      approvalDecision = decision;
      if (approvalFixture) mode = 'approval';
      await send('请查找并调用 yuanpu_approved_echo，参数 text 为 ' + (approvalFixture ? 'TASK008-' : 'UI-036-') + decision + '。这是需要宿主一次授权的无副作用回显工具，请只调用一次，等待用户决定，不改用其他工具。');
      await eventually(async () => (await renderer.evaluate('window.yuanpu.listCapabilityApprovals()')).length > 0,
        'Live model did not request the controlled echo approval.', 120_000);
      await eventually(() => renderer.evaluate('Array.from(document.querySelectorAll(".approval-actions button")).some(button => button.textContent.trim() === "允许一次" && !button.disabled)'), 'Approval controls absent.', 120_000).catch(async (error) => {
        await shot('live-approval-failure');
        const run = await latestRun();
        console.log({ runStatus: run?.status, hasBinding: Boolean(run?.pendingApproval), pendingCount: (await renderer.evaluate('window.yuanpu.listCapabilityApprovals()')).length });
        throw error;
      });
      await renderer.evaluate(`Array.from(document.querySelectorAll('.approval-actions button')).find((button) => button.textContent.trim() === ${JSON.stringify(decision === 'approved' ? '允许一次' : '拒绝')}).click()`);
      await eventually(async () => (await renderer.evaluate('window.yuanpu.listCapabilityApprovals()')).length === 0, 'Host did not resolve approval.').catch(async (error) => {
        await shot('live-approval-failure');
        console.log(await renderer.evaluate('Array.from(document.querySelectorAll(".message.error p")).map(p => p.textContent)'));
        throw error;
      });
      const run = await waitTerminal();
      assert.equal(run.status, decision === 'approved' ? 'succeeded' : 'failed');
      if (decision === 'approved') {
        assert.ok(run.output?.message.includes(approvalFixture ? 'TASK008-approved' : 'UI-036-approved'));
        assert.equal(await renderer.evaluate(`Array.from(document.querySelectorAll('.message.assistant .message-body p')).filter(p => p.textContent.includes(${JSON.stringify(approvalFixture ? 'TASK008-approved' : 'UI-036-approved')})).length`), 1);
        if (approvalFixture) {
          assert.equal(run.output?.tools?.some((tool) => tool.name.startsWith('ypcap:') && tool.status === 'completed'), true);
        }
      }
      if (decision === 'denied') assert.equal(run.output?.tools.some(tool => (approvalFixture ? tool.name.startsWith('ypcap:') : tool.name === 'execute_capability') && tool.status === 'completed'), false);
      if (!approvalFixture) assert.ok((await body()).includes(decision === 'approved' ? '已允许一次' : '已拒绝授权'));
      await shot((approvalFixture ? 'task008' : 'live') + '-approval-' + decision);
    }
  } else {
    mode = 'failure';
    await send('Retain this failed prompt.');
    assert.equal((await waitTerminal()).status, 'failed');
    assert.equal(await renderer.evaluate('document.querySelector("textarea").value'), 'Retain this failed prompt.');
    mode = 'hold';
    const before = providerRequests;
    await send('Hold this controlled request until cancelled.');
    await eventually(() => providerRequests === before + 1, 'Provider did not receive the pending request.');
    await fill('A next draft must not be submitted while busy.');
    assert.equal(await renderer.evaluate('document.querySelector(".composer button").disabled'), true);
    const count = await runCount();
    await click('[aria-label="发送消息"]');
    assert.equal(await runCount(), count);
    await eventually(async () => (await body()).includes('取消任务'), 'Cancellation missing.');
    await renderer.evaluate(`window.confirm = () => true; Array.from(document.querySelectorAll('button')).find((button) => button.textContent === '取消任务').click()`);
    assert.equal((await waitTerminal()).status, 'cancelled');
    assert.equal(await renderer.evaluate('document.querySelector("textarea").value'), 'A next draft must not be submitted while busy.');
    await shot('fixture-cancelled');
  }
  if (fullUiFixture) {
    const activeVersion = async () => {
      const state = JSON.parse(await readFile(join(home, 'packages/artifact-state.json'), 'utf8'));
      return state.packages['builtin.python.echo']?.activeVersion;
    };
    const installedCard = () => renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card.installed')).find(card => card.textContent.includes('builtin.python.echo'))?.textContent`);
    await renderer.evaluate(`document.querySelector('.nav-item[aria-label="技能"]').click()`);
    await eventually(() => renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card')).some(card => card.textContent.includes('Python 示例能力') && card.querySelector('.install-button'))`), 'Signed Python skill missing from current marketplace.', 30_000).catch(async (error) => {
      console.log(JSON.stringify({ skillsPage: (await body()).slice(-1800), catalogPort, search: await renderer.evaluate(`window.yuanpu.searchPlugins('Python').then(items => items.map(item => ({ name: item.name, version: item.version }))).catch(error => error.message)`) }));
      throw error;
    });
    await renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card')).find(card => card.textContent.includes('Python 示例能力')).querySelector('.install-button').click()`);
    await eventually(() => renderer.evaluate(`Boolean(document.querySelector('.trust-dialog'))`), 'Signed install trust dialog missing.');
    assert.ok((await body()).includes('清单摘要'));
    await renderer.evaluate(`Array.from(document.querySelectorAll('.trust-dialog button')).find(button => button.textContent === '信任并安装').click()`);
    await eventually(async () => (await activeVersion()) === '0.1.0' && (await installedCard())?.includes('v0.1.0'), 'UI install and durable v0.1.0 state diverged.', 60_000);
    await shot('task008-current-ui-installed');

    await renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card.installed')).find(card => card.textContent.includes('builtin.python.echo')).querySelector('.plugin-actions button').click()`);
    await eventually(() => renderer.evaluate(`Boolean(document.querySelector('.raw-config-field textarea'))`), 'Installed skill configuration page missing.');
    await renderer.evaluate(`(() => { const field = document.querySelector('.raw-config-field textarea'); Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(field, ${JSON.stringify(JSON.stringify({ responsePrefix: 'TASK008 UI: ' }))}); field.dispatchEvent(new Event('input', { bubbles: true })); })()`);
    await renderer.evaluate(`Array.from(document.querySelectorAll('.config-actions button')).find(button => button.textContent === '保存并应用').click()`);
    await eventually(() => renderer.evaluate(`Boolean(document.querySelector('.config-success'))`), 'Current UI did not confirm configuration save.');
    assert.equal(JSON.parse(await readFile(join(home, 'packages/config/builtin.python.echo/user.json'), 'utf8')).responsePrefix, 'TASK008 UI: ');
    await renderer.evaluate(`document.querySelector('.back-button').click()`);
    await shot('task008-current-ui-configured');

    catalogOptions.artifactRoot = join(fullUiFixture, '0.2.0');
    await catalog.close();
    catalog = await startGoCatalog(catalogOptions.artifactRoot);
    await renderer.evaluate(`Array.from(document.querySelectorAll('[role="tab"]')).find(button => button.textContent === '技能市场').click()`);
    await renderer.evaluate(`document.querySelector('form.plugin-search').requestSubmit()`);
    await eventually(() => renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card')).some(card => card.textContent.includes('Python 示例能力') && card.textContent.includes('v0.2.0'))`), 'Updated signed version missing from current marketplace.', 30_000);
    await renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card')).find(card => card.textContent.includes('Python 示例能力')).querySelector('.install-button').click()`);
    await eventually(() => renderer.evaluate(`Boolean(document.querySelector('.trust-dialog'))`), 'Update trust dialog missing.');
    catalogFault = 'signature';
    await renderer.evaluate(`Array.from(document.querySelectorAll('.trust-dialog button')).find(button => button.textContent === '信任并安装').click()`);
    await eventually(() => renderer.evaluate(`Boolean(document.querySelector('.install-recovery'))`), 'Bad signature did not surface recoverable UI failure.', 30_000);
    assert.equal(await activeVersion(), '0.1.0');
    assert.ok((await body()).includes('仍在使用 v0.1.0'));
    await shot('task008-current-ui-bad-signature');
    catalogFault = undefined;
    await renderer.evaluate(`document.querySelector('.install-recovery button').click()`);
    await eventually(() => renderer.evaluate(`Boolean(document.querySelector('.trust-dialog'))`), 'Retry trust dialog missing.');
    await renderer.evaluate(`Array.from(document.querySelectorAll('.trust-dialog button')).find(button => button.textContent === '信任并安装').click()`);
    await eventually(async () => (await activeVersion()) === '0.2.0' && (await installedCard())?.includes('v0.2.0'), 'UI upgrade and durable v0.2.0 state diverged.', 60_000);
    assert.equal(JSON.parse(await readFile(join(home, 'packages/config/builtin.python.echo/user.json'), 'utf8')).responsePrefix, 'TASK008 UI: ');
    await shot('task008-current-ui-upgraded');
    await renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card.installed .plugin-actions button')).find(button => button.textContent === '回滚到 v0.1.0').click()`);
    await eventually(async () => (await activeVersion()) === '0.1.0' && (await installedCard())?.includes('v0.1.0'), 'UI rollback and durable v0.1.0 state diverged.', 60_000);
    await renderer.command('Page.reload', { ignoreCache: true });
    await eventually(async () => {
      try { return await renderer.evaluate(`(() => { const button = document.querySelector('.nav-item[aria-label="技能"]'); if (!button) return false; button.click(); return true; })()`); }
      catch { return false; }
    }, 'Renderer did not return after reload.', 30_000);
    await eventually(async () => {
      try { return await renderer.evaluate(`(() => { const button = Array.from(document.querySelectorAll('[role="tab"]')).find(item => item.textContent === '已安装'); if (!button) return false; button.click(); return true; })()`); }
      catch { return false; }
    }, 'Installed tab did not return after reload.', 30_000);
    await eventually(async () => (await renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card.installed')).some(card => card.textContent.includes('builtin.python.echo') && card.textContent.includes('v0.1.0'))`)), 'Installed state not restored after renderer reload.', 30_000);
    await renderer.evaluate(`Array.from(document.querySelectorAll('.plugin-card.installed')).find(card => card.textContent.includes('builtin.python.echo')).querySelector('.plugin-actions button').click()`);
    await eventually(() => renderer.evaluate(`document.querySelector('.raw-config-field textarea')?.value.includes('TASK008 UI: ')`), 'Saved configuration not restored in current UI after reload.', 30_000);
    await shot('task008-current-ui-rollback-reload');
  }
  if (process.env.TASK_036_INSPECT === '1') {
    await renderer.command('Emulation.clearDeviceMetricsOverride');
    console.log('Native inspection window ready (45 seconds).');
    await new Promise((resolvePause) => setTimeout(resolvePause, 45_000));
  }
  await new Promise((resolveExit, rejectExit) => {
    const timeout = setTimeout(() => rejectExit(new Error('Electron quit timed out.')), 15_000);
    child.once('exit', (code) => { clearTimeout(timeout); code === 0 ? resolveExit() : rejectExit(new Error(`Electron exited ${code}`)); });
    child.kill('SIGUSR2');
  });
  await eventually(() => !exists(runtimePid), 'Runtime child survived App quit.');
  console.log(JSON.stringify({
    status: 'passed',
    schedulePersisted: true,
    connectionStatus: connection.status,
    mode: live ? 'live-provider' : approvalFixture ? 'controlled-approval-provider' : 'controlled-provider',
    viewportAndKeyboard: true,
    providerRequests,
    skillsUiJourney: Boolean(fullUiFixture),
    runtimeChildrenStopped: true,
  }));
} finally {
  renderer?.close();
  if (child && child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
  provider.closeAllConnections();
  await new Promise((resolveClose) => provider.close(resolveClose));
  if (catalogServer) {
    catalogServer.closeAllConnections();
    await new Promise((resolveClose) => catalogServer.close(resolveClose));
  }
  await catalog?.close();
  if (runtimePid && exists(runtimePid)) {
    const command = execFileSync('ps', ['-p', String(runtimePid), '-o', 'command='], { encoding: 'utf8' });
    if (command.includes(join(root, 'apps', 'runtime', 'dist', 'index.cjs'))) process.kill(runtimePid, 'SIGKILL');
  }
  await rm(root, { recursive: true, force: true });
}
