import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { mkdir, mkdtemp, readdir, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

// A packaged Electron/SEA business probe. All content and model responses are synthetic.
const executable = resolve(import.meta.dirname, '../release/mac-arm64/MindLink.app/Contents/MacOS/MindLink');
const root = await mkdtemp(join(tmpdir(), 'yp-task-050-packaged-'));
const home = join(root, 'home');
const userData = join(root, 'user-data');
const workspace = join(root, 'workspace');
const markerA = 'TASK050_ALPHA_WORK';
const markerB = 'TASK050_BETA_WORK';
const workPrompts = [];
let app;
let runtimePid;
let workerPid;

function children(pid) {
  return execFileSync('ps', ['-axo', 'pid=,ppid=,command='], { encoding: 'utf8' })
    .split('\n').flatMap((line) => {
      const match = /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(line);
      return match && Number(match[2]) === pid ? [{ pid: Number(match[1]), command: match[3] }] : [];
    });
}

function alive(pid) { try { process.kill(pid, 0); return true; } catch { return false; } }

async function eventually(check, label, timeout = 25_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    try { const value = await check(); if (value) return value; } catch { /* transient startup */ }
    await new Promise((done) => setTimeout(done, 80));
  }
  throw new Error(`Timed out waiting for ${label}`);
}

async function freePort() {
  const server = createServer();
  await new Promise((done) => server.listen(0, '127.0.0.1', done));
  const port = server.address().port;
  await new Promise((done) => server.close(done));
  return port;
}

const provider = createServer(async (request, response) => {
  let raw = '';
  for await (const chunk of request) raw += chunk;
  const body = JSON.parse(raw);
  const text = JSON.stringify(body.messages ?? []);
  const tools = (body.tools ?? []).map((tool) => tool.function?.name);
  let content;
  if (text.includes('Review this saved Work snapshot')) {
    content = JSON.stringify({ goal: 'Inspect the synthetic Work outcome', constraints: [],
      judgment: 'unverified', findings: [], unresolved: ['No independently verified outcome.'],
      followUp: ['Check the saved Work result.'], memoryCandidates: [], ledgerCandidates: [] });
  } else if (text.includes('Reflect on these current assistant follow-up candidates')) {
    const candidateId = /\\"candidateId\\":\\"([^\\"]+)\\"/u.exec(text)?.[1];
    content = JSON.stringify({ suggestions: candidateId ? [{ candidateId,
      reason: 'The synthetic Work outcome remains unverified.',
      nextStep: 'Review the saved result.' }] : [] });
  } else if (text.includes('Classify only direct user statements')) {
    content = '{"observations":[]}';
  } else {
    if (tools.includes('write')) workPrompts.push(text);
    content = text.includes(markerA) ? 'Synthetic answer for ALPHA.'
      : text.includes(markerB) ? 'Synthetic answer for BETA.' : 'Synthetic assistant reply.';
  }
  response.writeHead(200, { 'content-type': 'text/event-stream' });
  response.write(`data: ${JSON.stringify({ id: 'task-050-fixture', object: 'chat.completion.chunk',
    created: 1, model: 'fixture-model', choices: [{ index: 0,
      delta: { role: 'assistant', content }, finish_reason: null }] })}\n\n`);
  response.write(`data: ${JSON.stringify({ id: 'task-050-fixture', object: 'chat.completion.chunk',
    created: 1, model: 'fixture-model', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] })}\n\n`);
  response.end('data: [DONE]\n\n');
});

async function socket(url) {
  const connection = new WebSocket(url);
  await new Promise((done, reject) => {
    connection.addEventListener('open', done, { once: true });
    connection.addEventListener('error', reject, { once: true });
  });
  let sequence = 0;
  return { connection, command(method, params = {}) {
    return new Promise((resolveResult, rejectResult) => {
      const id = ++sequence;
      const timer = setTimeout(() => rejectResult(new Error(`CDP ${method} timeout`)), 10_000);
      const listener = (event) => {
        const message = JSON.parse(event.data);
        if (message.id !== id) return;
        clearTimeout(timer);
        connection.removeEventListener('message', listener);
        if (message.error) rejectResult(new Error(message.error.message));
        else resolveResult(message.result);
      };
      connection.addEventListener('message', listener);
      connection.send(JSON.stringify({ id, method, params }));
    });
  } };
}

async function evaluate(page, expression) {
  const result = await page.command('Runtime.evaluate', {
    expression, awaitPromise: true, returnByValue: true,
  });
  if (result.exceptionDetails) throw new Error(result.exceptionDetails.text);
  return result.result.value;
}

async function startApp() {
  const port = await freePort();
  app = spawn(executable, [`--user-data-dir=${userData}`, `--remote-debugging-port=${port}`], {
    env: { ...process.env, YUANPU_HOME: home, YUANPU_NOTIFICATIONS_ENABLED: '0',
      YUANPU_PYTHON_MCP_EXECUTABLE: '', YUANPU_PYTHON_MCP_ROOT: '' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let diagnostics = '';
  for (const stream of [app.stdout, app.stderr]) stream.on('data', (chunk) => {
    if (diagnostics.length < 1200) diagnostics += chunk.toString().slice(0, 1200 - diagnostics.length);
  });
  runtimePid = await eventually(() => children(app.pid).find((child) =>
    child.command.includes('YuanpuAgentRuntime-darwin-arm64'))?.pid,
  `packaged Runtime: ${diagnostics}`);
  workerPid = await eventually(() => children(runtimePid).find((child) =>
    child.command.includes('--assistant-worker'))?.pid, 'Assistant Worker');
  const target = await eventually(async () => (await (await fetch(`http://127.0.0.1:${port}/json/list`)).json())
    .find((item) => item.type === 'page' && item.webSocketDebuggerUrl), 'packaged page');
  const page = await socket(target.webSocketDebuggerUrl);
  await page.command('Runtime.enable');
  await eventually(() => evaluate(page, 'window.yuanpu.runtimeInfo().then((info) => info.protocolVersion === 7)'),
    'preload/Runtime readiness');
  const browser = await socket((await (await fetch(`http://127.0.0.1:${port}/json/version`)).json())
    .webSocketDebuggerUrl);
  return { page, browser };
}

async function closeApp({ page, browser }) {
  const stopped = new Promise((done, reject) => {
    const timer = setTimeout(() => reject(new Error('Packaged App did not exit')), 20_000);
    app.once('exit', (code) => { clearTimeout(timer); code === 0 ? done() : reject(new Error(`App exit ${code}`)); });
  });
  void browser.command('Browser.close').catch(() => undefined);
  await stopped;
  page.connection.close();
  browser.connection.close();
  await eventually(() => !alive(runtimePid) && !alive(workerPid), 'Runtime/Worker cleanup');
  app = undefined;
}

try {
  await Promise.all([mkdir(join(home, 'app'), { recursive: true }), mkdir(userData), mkdir(workspace)]);
  await new Promise((done) => provider.listen(0, '127.0.0.1', done));
  await writeFile(join(home, 'app', 'config.json'), JSON.stringify({ schemaVersion: 1,
    provider: 'fixture', model: 'fixture-model', workingDirectory: workspace,
    baseUrl: `http://127.0.0.1:${provider.address().port}/v1`, api: 'openai-completions',
    apiKeyEnv: 'TASK050_FIXTURE_KEY' }));
  process.env.TASK050_FIXTURE_KEY = 'synthetic-only';
  const first = await startApp();
  assert.deepEqual((await readdir(join(home, 'assistant', 'skills'))).sort(),
    ['delegate-and-verify', 'follow-up', 'maintain-memory', 'organize-work',
      'reflect-and-suggest', 'review-work', 'understand-user']);
  const workA = await evaluate(first.page, 'window.yuanpu.createWorkConversation()');
  const workB = await evaluate(first.page, 'window.yuanpu.createWorkConversation()');
  assert.notEqual(workA.id, workB.id);
  for (const [work, marker, clientMessageId] of [[workA, markerA, 'task050-a'],
    [workB, markerB, 'task050-b']]) {
    const receipt = await evaluate(first.page, `window.yuanpu.submitDesktopMessage(${JSON.stringify(
      `Please consider ${marker} as a separate Work.`)}, 'work', ${JSON.stringify(work.id)},
      ${JSON.stringify(clientMessageId)})`);
    assert.ok(receipt.runId);
    await eventually(async () => (await evaluate(first.page,
      `window.yuanpu.getAgentRun(${JSON.stringify(receipt.runId)}).then((run) => run.status)`)) === 'succeeded',
    `Work run ${clientMessageId}`);
  }
  assert.ok(workPrompts.some((text) => text.includes(markerA) && !text.includes(markerB)));
  assert.ok(workPrompts.some((text) => text.includes(markerB) && !text.includes(markerA)),
    'a fresh Work must not inherit the other Work transcript');
  const reviews = await eventually(async () => {
    const snapshot = await evaluate(first.page, 'window.yuanpu.getAssistantWorkspace()');
    return [workA, workB].every((work) => snapshot.reviews.some((review) => review.workId === work.id))
      ? snapshot.reviews : undefined;
  }, 'two independent Work reviews', 40_000);
  assert.equal(new Set(reviews.map((review) => review.workId)).size >= 2, true);
  const imported = await evaluate(first.page,
    "window.yuanpu.importAssistantSavedMemory('task050-controlled', 'work', 'The user prefers concise reports.', '2026-09-27T00:00:00.000Z')");
  await evaluate(first.page, "Array.from(document.querySelectorAll('.nav-item')).find((item) => item.textContent.includes('助理')).click()");
  await eventually(() => evaluate(first.page,
    "Boolean(document.querySelector('.assistant-home-tabs') && document.body.innerText.includes('先把重要的事'))"),
  'rendered assistant Home');
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-home-tabs button')).find((item) => item.textContent === '记忆').click()");
  await eventually(() => evaluate(first.page,
    `Array.from(document.querySelectorAll('.assistant-card.assistant-item')).some((item) =>
      item.textContent.includes(${JSON.stringify(imported.context)}))`), 'imported memory card');
  await evaluate(first.page,
    `Array.from(document.querySelectorAll('.assistant-card.assistant-item')).find((item) =>
      item.textContent.includes(${JSON.stringify(imported.context)})).click()`);
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-actions button')).find((item) => item.textContent === '纠正').click()");
  await evaluate(first.page, "document.querySelector('#assistant-memory-correction').focus(); document.querySelector('#assistant-memory-correction').select()");
  await first.page.command('Input.insertText', { text: 'The user prefers a concise written report.' });
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-actions button')).find((item) => item.textContent === '保存纠正').click()");
  const corrected = await eventually(async () => (await evaluate(first.page,
    'window.yuanpu.getAssistantWorkspace()')).memories.find((item) => item.id === imported.id
      && item.version === imported.version + 1 && item.text === 'The user prefers a concise written report.'),
  'renderer memory correction');
  assert.equal(corrected.version, imported.version + 1);
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-section-title button')).find((item) => item.textContent === '返回全部').click()");
  const source = (await evaluate(first.page, 'window.yuanpu.getAssistantWorkspace()'))
    .sources.find((item) => item.sourceId.startsWith('work-turn:') && item.current);
  assert.ok(source);
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('details.assistant-card summary')).find((item) => item.textContent.includes('助理来源')).click()");
  await eventually(() => evaluate(first.page,
    `Array.from(document.querySelectorAll('.assistant-source-list li')).some((item) =>
      item.textContent.includes(${JSON.stringify(source.sourceId)}))`), 'Work source in memory page');
  await evaluate(first.page,
    `Array.from(document.querySelectorAll('.assistant-source-list li')).find((item) =>
      item.textContent.includes(${JSON.stringify(source.sourceId)}))
      .querySelector('button:last-of-type').click()`);
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-source-detail button')).find((item) => item.textContent === '撤销助理读取').click()");
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-source-detail button')).find((item) => item.textContent === '确认撤销').click()");
  await eventually(async () => (await evaluate(first.page, 'window.yuanpu.getAssistantWorkspace()'))
    .sources.some((item) => item.sourceId === source.sourceId && item.availability === 'deleted'),
  'Worker source withdrawal');
  await eventually(() => evaluate(first.page,
    "document.body.innerText.includes('撤销已由宿主登记')"), 'renderer host revocation receipt');
  const suggestion = await eventually(async () => (await evaluate(first.page,
    'window.yuanpu.listAssistantSuggestions()')).items.find((item) => item.feedback === 'none'),
  'proactive suggestion', 40_000);
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-home-tabs button')).find((item) => item.textContent === '今日').click()");
  await eventually(() => evaluate(first.page,
    `Array.from(document.querySelectorAll('.assistant-card.assistant-item')).some((item) =>
      item.textContent.includes(${JSON.stringify(suggestion.reason)}))`), 'rendered suggestion');
  await evaluate(first.page,
    `Array.from(document.querySelectorAll('.assistant-card.assistant-item')).find((item) =>
      item.textContent.includes(${JSON.stringify(suggestion.reason)})).click()`);
  await eventually(() => evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-actions button')).some((item) => item.textContent === '忽略')"),
  'suggestion detail');
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-actions button')).find((item) => item.textContent === '忽略').click()");
  await eventually(async () => (await evaluate(first.page, 'window.yuanpu.listAssistantSuggestions()')).items
    .find((item) => item.suggestionId === suggestion.suggestionId)?.feedback === 'ignored',
  'ignored suggestion persistence');
  await evaluate(first.page,
    "Array.from(document.querySelectorAll('.assistant-home-tabs button')).find((item) => item.textContent === '记忆').click()");
  await eventually(() => evaluate(first.page,
    `Array.from(document.querySelectorAll('.assistant-card.assistant-item')).some((item) =>
      item.textContent.includes(${JSON.stringify(imported.context)}))`), 'rendered corrected memory');
  await evaluate(first.page,
    `Array.from(document.querySelectorAll('.assistant-card.assistant-item')).find((item) =>
      item.textContent.includes(${JSON.stringify(imported.context)})).click()`);
  assert.equal(await evaluate(first.page,
    "document.body.innerText.includes('The user prefers a concise written report.')"), true);
  const capture = await first.page.command('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
  const screenshot = resolve(import.meta.dirname, '../../../.tasks/verification/TASK-050/assistant-desktop.png');
  await mkdir(dirname(screenshot), { recursive: true });
  await writeFile(screenshot, Buffer.from(capture.data, 'base64'));
  await closeApp(first);
  const second = await startApp();
  const restored = await evaluate(second.page, 'window.yuanpu.getAssistantWorkspace()');
  assert.equal(restored.memories.find((item) => item.id === imported.id)?.text,
    'The user prefers a concise written report.');
  assert.equal(restored.sources.find((item) => item.sourceId === source.sourceId)?.availability, 'deleted');
  assert.equal((await evaluate(second.page, 'window.yuanpu.listAssistantSuggestions()')).items
    .find((item) => item.suggestionId === suggestion.suggestionId)?.feedback, 'ignored');
  const db = new DatabaseSync(join(home, 'workflows', 'automation.sqlite'), { readOnly: true });
  try {
    assert.equal(db.prepare('SELECT COUNT(*) AS n FROM yp_work_turn_sources').get().n >= 2, true);
    assert.equal(db.prepare('SELECT COUNT(*) AS n FROM yp_assistant_source_deletions WHERE source_id=?')
      .get(source.sourceId).n, 1);
  } finally { db.close(); }
  await closeApp(second);
  console.log(JSON.stringify({ status: 'passed', mode: 'packaged-electron-sea-loopback',
    twoWorkReviews: true, isolatedWorkContext: true, correctedMemoryRestored: true,
    sourceTombstoneRestored: true, ignoredSuggestionRestored: true,
    workerStoppedWithApp: true, screenshot: 'synthetic-tracked' }));
} finally {
  if (app && app.exitCode === null && app.signalCode === null) app.kill('SIGKILL');
  for (const pid of [workerPid, runtimePid]) if (pid && alive(pid)) process.kill(pid, 'SIGKILL');
  provider.closeAllConnections();
  await new Promise((done) => provider.close(done));
  await rm(root, { recursive: true, force: true });
}
