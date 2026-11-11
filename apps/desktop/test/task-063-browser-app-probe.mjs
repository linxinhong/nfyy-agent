import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, readFile, readdir, rm, symlink, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const desktopRoot = resolve(import.meta.dirname, '..');
const repositoryRoot = resolve(desktopRoot, '../..');
const electron = createRequire(import.meta.url)('electron');
const root = await mkdtemp(join(tmpdir(), 'yuanpu-browser-probe-'));
const appRoot = join(root, 'apps', 'desktop');
const home = join(root, 'home');
const workspace = join(home, 'workspace');
const userData = join(root, 'electron-user-data');
const externalOpenLog = join(root, 'external-open.txt');
const guestPreferencesLog = join(root, 'guest-preferences.json');
let firstPageHits = 0;
let secondPageHits = 0;
const browserPage = createServer((request, response) => {
  const second = request.url === '/second';
  const slow = request.url === '/slow';
  if (request.url === '/first') firstPageHits += 1;
  if (second) secondPageHits += 1;
  response.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
  const html = `<html><head><title>Browser fixture ${slow ? 'slow' : second ? 'second' : 'first'}</title></head><body><h1>${second ? 'Second' : 'First'} page</h1><a href="/second">Next page</a><button onclick="window.browserClickCount=(window.browserClickCount||0)+1">Count click</button></body></html>`;
  if (slow) setTimeout(() => response.end(html), 500);
  else response.end(html);
});
let browserPort = 0;
let providerRequests = 0;
let browserSkillAdvertised = false;
let sidebarBrowserPromptAdvertised = false;
let providerMode = 'navigate';
let screenshotHasPng = false;
let snapshotHasPageText = false;
let noGuestErrorSeen = false;
let screenshotToolSummary = '';
const providerTrace = [];
const provider = createServer(async (request, response) => {
  let body = '';
  for await (const chunk of request) body += chunk.toString();
  const messages = JSON.parse(body).messages;
  const lastUser = messages.findLastIndex((message) => message.role === 'user'
    && /browser_(navigate|screenshot|click|evaluate|snapshot)|侧边栏浏览器的文本内容/.test(JSON.stringify(message.content)));
  const currentMessages = messages.slice(lastUser);
  browserSkillAdvertised ||= messages.some((message) => message.role === 'system'
    && JSON.stringify(message.content).includes('browser-control'));
  sidebarBrowserPromptAdvertised ||= messages.some((message) => message.role === 'system'
    && String(message.content).includes('For visible page text, search and execute browser_snapshot'));
  providerRequests += 1;
  response.writeHead(200, { 'content-type': 'text/event-stream' });
  const chunk = (delta, finishReason = null) => `data: ${JSON.stringify({ id: `fixture-${providerRequests}`,
    object: 'chat.completion.chunk', created: 1, model: 'fixture-model', choices: [{ index: 0, delta, finish_reason: finishReason }] })}\n\n`;
  if (!messages.some((message) => message.role === 'system' && String(message.content).includes('You are 南医小助'))) {
    response.end(chunk({ role: 'assistant', content: 'Fixture idle.' }, 'stop') + 'data: [DONE]\n\n');
    return;
  }
  const latestTool = [...currentMessages].reverse().find((message) => message.role === 'tool');
  const latestToolCall = [...currentMessages].reverse().find((message) => message.role === 'assistant' && message.tool_calls?.length)?.tool_calls[0]?.function?.name;
  const toolText = latestTool ? JSON.stringify(latestTool.content) : '';
  const capabilityId = /ypcap:[A-Za-z0-9_-]+:[A-Za-z0-9_-]+/.exec(toolText)?.[0];
  const capability = providerMode === 'screenshot' ? 'browser_screenshot'
    : providerMode === 'snapshot' ? 'browser_snapshot'
    : providerMode === 'click-deny' ? 'browser_click'
    : providerMode === 'evaluate-deny' ? 'browser_evaluate'
    : providerMode === 'no-guest' ? 'browser_snapshot' : 'browser_navigate';
  const args = providerMode === 'screenshot' ? { fullPage: false }
    : providerMode === 'click-deny' ? { x: 145, y: 80 }
    : providerMode === 'evaluate-deny' ? { expression: 'window.browserEvaluateCount=(window.browserEvaluateCount||0)+1' }
    : providerMode === 'no-guest' || providerMode === 'snapshot' ? {}
    : { url: `http://127.0.0.1:${browserPort}/first` };
  providerTrace.push({ mode: providerMode, latestToolCall, hasCapabilityId: Boolean(capabilityId),
    lastTool: toolText.slice(0, 120) });
  if (!latestTool) {
    response.write(chunk({ role: 'assistant', tool_calls: [{ index: 0, id: `call-${providerRequests}`, type: 'function',
      function: { name: 'search_capabilities', arguments: JSON.stringify({ query: capability }) } }] }));
    response.end(chunk({}, 'tool_calls') + 'data: [DONE]\n\n');
  } else if (latestToolCall === 'search_capabilities' && capabilityId) {
    response.write(chunk({ role: 'assistant', tool_calls: [{ index: 0, id: `call-${providerRequests}`, type: 'function',
      function: { name: 'execute_capability', arguments: JSON.stringify({ name: capabilityId,
        arguments: args }) } }] }));
    response.end(chunk({}, 'tool_calls') + 'data: [DONE]\n\n');
  } else {
    if (providerMode === 'screenshot' && latestToolCall === 'execute_capability') {
      screenshotHasPng = JSON.stringify(currentMessages).includes('iVBORw0KGgo');
      screenshotToolSummary = JSON.stringify({ lastToolCall: latestToolCall, contentType: typeof latestTool?.content,
        textLength: toolText.length, textStart: toolText.slice(0, 300) });
    }
    if (providerMode === 'snapshot' && latestToolCall === 'execute_capability') {
      snapshotHasPageText = toolText.includes('First page') && toolText.includes('Browser fixture first');
    }
    if (providerMode === 'no-guest' && latestToolCall === 'execute_capability') {
      noGuestErrorSeen = toolText.includes('该会话尚未打开浏览器标签');
    }
    response.write(chunk({ role: 'assistant', content: `Browser capability finished: ${toolText.slice(0, 120)}` }));
    response.end(chunk({}, 'stop') + 'data: [DONE]\n\n');
  }
});

async function freePort() {
  const server = createNetServer();
  await new Promise((done) => server.listen(0, '127.0.0.1', done));
  const port = server.address().port;
  await new Promise((done) => server.close(done));
  return port;
}

async function listen(server) {
  await new Promise((done) => server.listen(0, '127.0.0.1', done));
  return server.address().port;
}

async function eventually(check, message, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try { const result = await check(); if (result) return result; }
    catch { /* The app may still be starting. */ }
    await new Promise((done) => setTimeout(done, 80));
  }
  throw new Error(message);
}

async function connectRenderer(port, targetFilter = (item) => item.type === 'page') {
  const target = await eventually(async () => {
    const response = await fetch(`http://127.0.0.1:${port}/json/list`);
    return (await response.json()).find((item) => targetFilter(item) && item.webSocketDebuggerUrl);
  }, 'Electron target debugger did not start');
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((done, fail) => {
    socket.addEventListener('open', done, { once: true });
    socket.addEventListener('error', fail, { once: true });
  });
  let nextId = 0;
  const pending = new Map();
  socket.addEventListener('message', (event) => {
    const message = JSON.parse(event.data);
    if (!message.id || !pending.has(message.id)) return;
    const { done, fail } = pending.get(message.id);
    pending.delete(message.id);
    if (message.error) fail(new Error(message.error.message));
    else done(message.result);
  });
  const command = (method, params = {}) => new Promise((done, fail) => {
    const id = ++nextId;
    pending.set(id, { done, fail });
    socket.send(JSON.stringify({ id, method, params }));
  });
  await command('Runtime.enable');
  return {
    async evaluate(expression) {
      const reply = await command('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
      if (reply.exceptionDetails) throw new Error(reply.exceptionDetails.exception?.description ?? reply.exceptionDetails.text);
      return reply.result.value;
    },
    command,
    close: () => socket.close(),
  };
}

async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  child.kill('SIGUSR2');
  await Promise.race([
    new Promise((done) => child.once('exit', done)),
    new Promise((done) => setTimeout(done, 5_000)),
  ]);
  if (child.exitCode === null && child.signalCode === null) child.kill();
}

let vite;
let app;
let renderer;
let diagnostics = '';
try {
  await mkdir(appRoot, { recursive: true });
  await mkdir(join(root, 'apps', 'runtime'), { recursive: true });
  await mkdir(join(home, 'app'), { recursive: true });
  await mkdir(workspace, { recursive: true });
  await mkdir(userData, { recursive: true });
  await symlink(resolve(desktopRoot, '../runtime/dist'), join(root, 'apps', 'runtime', 'dist'), 'dir');
  await writeFile(join(appRoot, 'package.json'), JSON.stringify({ name: 'yuanpu-browser-probe', version: '1.0.0', main: 'entry.cjs' }));
  await writeFile(join(appRoot, 'entry.cjs'), `
    const { app, shell, webContents } = require('electron');
    shell.openExternal = async (url) => require('node:fs').writeFileSync(${JSON.stringify(externalOpenLog)}, url);
    app.setPath('userData', ${JSON.stringify(userData)});
    process.on('SIGUSR2', () => app.quit());
    require(${JSON.stringify(join(desktopRoot, 'dist/main.cjs'))});
    setInterval(() => {
      const guest = webContents.getAllWebContents().find((item) => item.getType() === 'webview');
      if (guest) require('node:fs').writeFileSync(${JSON.stringify(guestPreferencesLog)},
        JSON.stringify(guest.getLastWebPreferences()));
    }, 100).unref();
  `);
  browserPort = await listen(browserPage);
  const providerPort = await listen(provider);
  await writeFile(join(home, 'app', 'config.json'), JSON.stringify({
    schemaVersion: 1, provider: 'browser-fixture', model: 'fixture-model', apiKeyEnv: 'TASK_063_PROVIDER_KEY',
    workingDirectory: workspace, baseUrl: `http://127.0.0.1:${providerPort}/v1`, api: 'openai-completions',
  }));
  await writeFile(join(home, 'app', 'models.json'), JSON.stringify({ providers: { 'browser-fixture': {
    baseUrl: `http://127.0.0.1:${providerPort}/v1`, api: 'openai-completions',
    models: [{ id: 'fixture-model', name: 'fixture-model', reasoning: false,
      input: ['text', 'image'], contextWindow: 1_000_000, maxTokens: 1024 }],
  } } }));
  const vitePort = await freePort();
  vite = spawn('pnpm', ['--filter', '@yuanpu-agent/app', 'run', 'dev', '--host', '127.0.0.1', '--port', String(vitePort), '--strictPort'], {
    cwd: repositoryRoot, stdio: ['ignore', 'pipe', 'pipe'],
  });
  await eventually(async () => (await fetch(`http://127.0.0.1:${vitePort}/`)).ok, 'Vite did not start');
  const debuggerPort = await freePort();
  app = spawn(electron, [`--remote-debugging-port=${debuggerPort}`, appRoot], {
    env: { ...process.env, YUANPU_HOME: home, YUANPU_RENDERER_URL: `http://127.0.0.1:${vitePort}/`,
      YUANPU_NODE_BINARY: process.execPath, YUANPU_NOTIFICATIONS_ENABLED: '0', TASK_063_PROVIDER_KEY: 'fixture-only' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  for (const stream of [app.stdout, app.stderr]) stream.on('data', (chunk) => {
    if (diagnostics.length < 16_384) diagnostics += chunk.toString();
  });
  renderer = await connectRenderer(debuggerPort);
  await eventually(() => renderer.evaluate('window.yuanpu?.runtimeInfo?.()'), 'Electron preload or Runtime did not start');
  await renderer.evaluate(`window.location.hash = '#/work'`);
  await eventually(() => renderer.evaluate(`Boolean(document.querySelector('button[aria-label="打开右侧面板"]'))`), 'Work panel did not mount');
  await eventually(() => renderer.evaluate(`document.querySelector('button[aria-label="重命名会话"]')?.disabled === false`),
    'Work conversation did not finish loading');
  await renderer.evaluate(`document.querySelector('button[aria-label="打开右侧面板"]').click()`);
  await eventually(() => renderer.evaluate(`Boolean(document.querySelector('button[aria-label="添加面板标签"]'))`), 'Workspace tabs did not mount');
  await renderer.evaluate(`document.querySelector('button[aria-label="添加面板标签"]').click()`);
  assert.deepEqual(await renderer.evaluate(`Array.from(document.querySelectorAll('.workspace-tab-menu button'))
    .map((button) => button.textContent.trim())`), ['文件列表', '浏览器'],
    'Only user-openable file and browser tabs belong in the new-tab menu');
  await renderer.evaluate(`Array.from(document.querySelectorAll('.workspace-tab-menu button')).find((button) => button.textContent.includes('浏览器')).click()`);
  await eventually(() => renderer.evaluate('document.querySelector("webview")?.getWebContentsId?.()'), 'Browser webview did not attach');
  const guestId = await renderer.evaluate('document.querySelector("webview").getWebContentsId()');
  const guestPreferences = await eventually(async () => {
    const raw = await readFile(guestPreferencesLog, 'utf8').catch(() => '');
    return raw ? JSON.parse(raw) : undefined;
  }, 'Guest preferences were not observable');
  assert.equal(guestPreferences.sandbox, true, 'Guest sandbox is disabled');
  assert.equal(guestPreferences.contextIsolation, true, 'Guest context isolation is disabled');
  assert.equal(guestPreferences.nodeIntegration, false, 'Guest Node integration is enabled');
  assert.notEqual(guestPreferences.webSecurity, false, 'Guest web security is disabled');
  assert.equal(guestPreferences.preload, undefined, 'Guest inherited a privileged preload');
  const firstUrl = `http://127.0.0.1:${browserPort}/first`;
  await renderer.evaluate(`(() => {
    const input = document.querySelector('.browser-address');
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, ${JSON.stringify(firstUrl)});
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  await renderer.evaluate(`document.querySelector('.browser-address').closest('form').requestSubmit()`);
  await eventually(() => renderer.evaluate(`document.querySelector('webview')?.getURL() === ${JSON.stringify(firstUrl)}`), 'Browser did not navigate to the local page');
  await eventually(() => renderer.evaluate('document.querySelector("webview")?.getTitle() === "Browser fixture first"'), 'Browser title did not update');
  await eventually(() => renderer.evaluate('!document.querySelector("webview")?.isLoading()'), 'First page did not finish loading');
  const initialConversation = await renderer.evaluate('window.yuanpu.listWorkConversations().then((items) => items.find((item) => item.current))');
  const hitsBeforeSwitch = firstPageHits;
  await renderer.evaluate(`document.querySelector('button[aria-label="打开工作列表"]').click()`);
  await renderer.evaluate(`document.querySelector('button[aria-label="新建会话"]').click()`);
  await eventually(() => renderer.evaluate(`window.yuanpu.listWorkConversations().then((items) =>
    items.find((item) => item.current)?.id !== ${JSON.stringify(initialConversation.id)})`), 'New Work conversation did not become current');
  assert.equal(await renderer.evaluate('document.querySelector("webview")?.getWebContentsId()'), guestId,
    'Switching Work conversations destroyed the first browser guest');
  await eventually(() => renderer.evaluate(`Boolean(document.querySelector('button[aria-label="打开右侧面板"]'))`),
    'Second Work conversation did not restore its closed sidebar state');
  await renderer.evaluate(`document.querySelector('button[aria-label="打开右侧面板"]').click()`);
  await eventually(() => renderer.evaluate('Boolean(document.querySelector(".file-tab-title"))'), 'Second Work sidebar did not open');
  assert.equal(await renderer.evaluate('document.querySelectorAll(".file-tab-title").length'), 1,
    'The second Work conversation inherited tabs from the first');
  await renderer.evaluate(`document.querySelector('[data-tree-key=${JSON.stringify(`conversation:${initialConversation.id}`)}]').click()`);
  await eventually(() => renderer.evaluate(`window.yuanpu.listWorkConversations().then((items) =>
    items.find((item) => item.current)?.id === ${JSON.stringify(initialConversation.id)})`), 'First Work conversation did not reopen');
  await eventually(() => renderer.evaluate('Boolean(document.querySelector(".file-tab-title"))'), 'First Work sidebar did not reopen');
  assert.equal(await renderer.evaluate('document.querySelector("webview")?.getWebContentsId()'), guestId,
    'Returning to a Work conversation replaced its browser guest');
  assert.equal(firstPageHits, hitsBeforeSwitch, 'Switching Work conversations reloaded the browser page');
  const slowUrl = `http://127.0.0.1:${browserPort}/slow`;
  await renderer.evaluate(`(() => {
    const input = document.querySelector('.browser-address');
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, ${JSON.stringify(slowUrl)});
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.closest('form').requestSubmit();
  })()`);
  await eventually(() => renderer.evaluate('Boolean(document.querySelector(".browser-reloading"))'), 'Browser loading state did not appear');
  await eventually(() => renderer.evaluate('document.querySelector("webview")?.getTitle() === "Browser fixture slow"'), 'Slow page did not load');
  await renderer.evaluate(`(() => {
    const input = document.querySelector('.browser-address');
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, 'http://127.0.0.1:65534/fail');
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.closest('form').requestSubmit();
  })()`);
  await eventually(() => renderer.evaluate('Boolean(document.querySelector(".browser-error")?.textContent.trim())'),
    'Browser navigation error state did not appear');
  await renderer.evaluate(`(() => {
    const input = document.querySelector('.browser-address');
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, ${JSON.stringify(firstUrl)});
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.closest('form').requestSubmit();
  })()`);
  await eventually(() => renderer.evaluate(`document.querySelector('webview')?.getURL() === ${JSON.stringify(firstUrl)} && !document.querySelector('.browser-error')`),
    'Browser did not recover from the navigation error');
  assert.equal(await renderer.evaluate(`document.querySelector('webview').executeJavaScript('typeof process + ":" + typeof require')`),
    'undefined:undefined', 'Guest page can access Node globals');
  const beforeWidth = await renderer.evaluate('Number(document.querySelector(".activity-resize-handle").getAttribute("aria-valuenow"))');
  await renderer.evaluate(`(() => {
    const handle = document.querySelector('.activity-resize-handle');
    const x = handle.getBoundingClientRect().left + 5;
    handle.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, pointerId: 7, clientX: x }));
    window.dispatchEvent(new PointerEvent('pointermove', { bubbles: true, pointerId: 7, clientX: x - 80 }));
    window.dispatchEvent(new PointerEvent('pointerup', { bubbles: true, pointerId: 7, clientX: x - 80 }));
  })()`);
  await eventually(() => renderer.evaluate(`Number(document.querySelector('.activity-resize-handle').getAttribute('aria-valuenow')) > ${beforeWidth}`),
    'Browser panel did not follow the resize drag');
  await renderer.evaluate(`document.querySelector('button[aria-label="在系统浏览器中打开"]').click()`);
  await eventually(async () => (await readFile(externalOpenLog, 'utf8').catch(() => '')) === firstUrl,
    'System browser action did not hand off the current URL');
  await renderer.evaluate(`document.querySelector('webview').executeJavaScript('window.open("https://example.com/popup")', true)`);
  await eventually(async () => (await readFile(externalOpenLog, 'utf8').catch(() => '')) === 'https://example.com/popup',
    'Guest HTTP popup was not handed to the system browser');
  await renderer.evaluate(`document.querySelector('webview').executeJavaScript('window.open("file:///etc/passwd")', true)`);
  assert.equal(await readFile(externalOpenLog, 'utf8'), 'https://example.com/popup', 'Blocked file popup escaped the guest');
  await renderer.evaluate(`document.querySelector('button[aria-label="添加面板标签"]').click()`);
  await renderer.evaluate(`Array.from(document.querySelectorAll('.workspace-tab-menu button')).find((button) => button.textContent.includes('文件列表')).click()`);
  assert.equal(await renderer.evaluate('document.querySelector("webview")?.getWebContentsId()'), guestId, 'Switching tabs destroyed the shared browser guest');
  await new Promise((done) => setTimeout(done, 300));
  await renderer.evaluate(`document.querySelector('webview').executeJavaScript('document.querySelector("a").click()', true)`);
  await eventually(() => renderer.evaluate('document.querySelector("webview")?.getURL().endsWith("/second")'), 'Browser link did not navigate');
  await eventually(() => renderer.evaluate('!document.querySelector("webview")?.isLoading()'), 'Second page did not finish loading');
  await eventually(() => renderer.evaluate('!document.querySelector(\'button[aria-label="后退"]\')?.disabled'), 'Back button stayed disabled', 2_000);
  await renderer.evaluate(`document.querySelector('button[aria-label="后退"]').click()`);
  await eventually(() => renderer.evaluate('document.querySelector("webview")?.getURL().endsWith("/first")'), 'Browser back button failed');
  await eventually(() => renderer.evaluate('!document.querySelector("webview")?.isLoading() && document.querySelector("webview")?.canGoForward()'), 'Back navigation did not finish');
  await renderer.evaluate(`document.querySelector('button[aria-label="前进"]').click()`);
  await eventually(() => renderer.evaluate('document.querySelector("webview")?.getURL().endsWith("/second")'), 'Browser forward button failed');
  const hitsBeforeReload = secondPageHits;
  await eventually(() => renderer.evaluate('!document.querySelector("webview")?.isLoading()'), 'Forward navigation did not finish');
  await renderer.evaluate(`document.querySelector('button[aria-label="重新加载"]').click()`);
  await eventually(() => secondPageHits > hitsBeforeReload, 'Browser reload did not request the page again');
  assert.equal(await renderer.evaluate('document.querySelector("webview")?.getWebContentsId()'), guestId);
  await renderer.evaluate(`document.querySelector('button[aria-label="添加面板标签"]').click()`);
  await renderer.evaluate(`Array.from(document.querySelectorAll('.workspace-tab-menu button')).find((button) => button.textContent.includes('文件列表')).click()`);
  await renderer.evaluate(`(() => {
    const input = document.querySelector('textarea[aria-label="消息"]');
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(input, '请使用 browser_navigate 回到第一页');
    input.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  await renderer.evaluate(`document.querySelector('button[aria-label="发送消息"]').click()`);
  await eventually(() => providerRequests >= 2, 'Agent did not discover the browser capability', 30_000);
  await eventually(() => renderer.evaluate('document.querySelector("webview")?.getURL().endsWith("/first")'),
    'Agent browser_navigate did not control the shared guest', 30_000);
  assert.equal(await renderer.evaluate('document.querySelector("webview")?.getWebContentsId()'), guestId);
  assert.equal(browserSkillAdvertised, true, 'Pi did not discover the bundled browser-control skill');
  assert.equal(sidebarBrowserPromptAdvertised, true, 'Pi was not told how to read the shared sidebar browser');
  await eventually(() => renderer.evaluate('!document.querySelector(".composer-hint")?.textContent.includes("任务执行中")'), 'Navigation run did not finish');
  await renderer.evaluate(`Array.from(document.querySelectorAll('.file-tab-title')).find((button) => button.textContent.includes('Browser fixture first')).click()`);

  async function sendBrowserTask(mode, text) {
    providerMode = mode;
    await renderer.evaluate(`(() => {
      const input = document.querySelector('textarea[aria-label="消息"]');
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(input, ${JSON.stringify(text)});
      input.dispatchEvent(new Event('input', { bubbles: true }));
    })()`);
    await eventually(() => renderer.evaluate('!document.querySelector(\'button[aria-label="发送消息"]\')?.disabled'),
      `${mode} prompt was not ready`);
    await renderer.evaluate(`document.querySelector('button[aria-label="发送消息"]').click()`);
  }

  await sendBrowserTask('snapshot', '侧边栏浏览器的文本内容是什么？');
  await eventually(() => snapshotHasPageText, 'Agent browser_snapshot did not read the visible page text', 30_000);
  await eventually(() => renderer.evaluate('!document.querySelector(".composer-hint")?.textContent.includes("任务执行中")'), 'Snapshot run did not finish');
  await sendBrowserTask('screenshot', '请用 browser_screenshot 截取当前浏览器页面。');
  await eventually(() => renderer.evaluate(`Boolean(Array.from(document.querySelectorAll('.file-tab-title'))
    .find((button) => button.textContent.includes('Browser fixture first'))?.getAttribute('aria-selected') === 'true')`),
  'Screenshot request did not reveal the browser tab', 10_000);
  await eventually(() => screenshotHasPng, 'Agent screenshot did not return a real PNG image', 75_000);
  await eventually(() => renderer.evaluate('!document.querySelector(".composer-hint")?.textContent.includes("任务执行中")'), 'Screenshot run did not finish');
  const activeWork = await renderer.evaluate('window.yuanpu.listWorkConversations().then((items) => items.find((item) => item.current))');
  const screenshotRoot = activeWork?.workingDirectory ?? workspace;
  const screenshotFiles = await readdir(join(screenshotRoot, 'images'));
  assert.equal(screenshotFiles.length, 1, 'Browser screenshot was not saved in this Work conversation');
  assert.match(screenshotFiles[0], /^browser-.*\.png$/);
  assert.deepEqual((await readFile(join(screenshotRoot, 'images', screenshotFiles[0]))).subarray(0, 8),
    Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]));
  await renderer.evaluate(`Array.from(document.querySelectorAll('.file-tab-close')).find((button) => button.getAttribute('aria-label')?.includes('Browser fixture first')).click()`);
  await eventually(() => renderer.evaluate('!document.querySelector("webview")'), 'Browser guest did not detach when its tab closed');
  await renderer.evaluate(`document.querySelector('button[aria-label="添加面板标签"]').click()`);
  await renderer.evaluate(`Array.from(document.querySelectorAll('.workspace-tab-menu button')).find((button) => button.textContent.includes('浏览器')).click()`);
  await eventually(() => renderer.evaluate(`document.querySelector('webview')?.getURL() === ${JSON.stringify(firstUrl)}`),
    'Browser URL was not restored from conversation memory');
  const restoredGuestId = await renderer.evaluate('document.querySelector("webview").getWebContentsId()');
  assert.notEqual(restoredGuestId, guestId, 'Reopening browser tab did not create a fresh guest');

  for (const [mode, command] of [['click-deny', 'browser_click'], ['evaluate-deny', 'browser_evaluate']]) {
    const before = providerRequests;
    await sendBrowserTask(mode, `请使用 ${command} 测试授权，等待我拒绝。`);
    await eventually(() => providerRequests >= before + 2, `${command} discovery did not reach the provider`, 30_000);
    await eventually(() => renderer.evaluate('window.yuanpu.listCapabilityApprovals().then(items => items.length > 0)'),
      `${command} did not request approval`, 30_000);
    await eventually(() => renderer.evaluate('Array.from(document.querySelectorAll(".approval-actions button")).some(button => button.textContent.trim() === "拒绝" && !button.disabled)'),
      `${command} approval card did not render`, 30_000);
    await renderer.evaluate(`Array.from(document.querySelectorAll('.approval-actions button')).find(button => button.textContent.trim() === '拒绝').click()`);
    await eventually(() => renderer.evaluate('window.yuanpu.listCapabilityApprovals().then(items => items.length === 0)'),
      `${command} rejection did not settle`, 30_000);
    await eventually(() => renderer.evaluate('!document.querySelector(".composer-hint")?.textContent.includes("任务执行中")'),
      `${command} run did not settle`, 30_000);
  }
  assert.equal(await renderer.evaluate('document.querySelector("webview").executeJavaScript("window.browserClickCount||0")'), 0);
  assert.equal(await renderer.evaluate('document.querySelector("webview").executeJavaScript("window.browserEvaluateCount||0")'), 0);
  const guestDebugger = await connectRenderer(debuggerPort, (item) => item.url === firstUrl);
  await Promise.race([guestDebugger.command('Page.crash').catch(() => undefined),
    new Promise((done) => setTimeout(done, 3_000))]);
  guestDebugger.close();
  await eventually(() => renderer.evaluate(`document.querySelector('webview')?.getWebContentsId() !== ${restoredGuestId}`),
    'Crashed browser guest was not rebuilt');
  await eventually(() => renderer.evaluate(`document.querySelector('webview')?.getURL() === ${JSON.stringify(firstUrl)}`),
    'Crashed browser guest did not restore its URL');
  const inactiveConversation = await renderer.evaluate('window.yuanpu.createWorkConversation()');
  providerMode = 'no-guest';
  await renderer.evaluate(`window.yuanpu.submitDesktopMessage('请使用 browser_snapshot 测试未激活会话', 'work', ${JSON.stringify(inactiveConversation.id)})`);
  await eventually(() => noGuestErrorSeen, 'Inactive conversation did not receive a clear missing-browser error', 20_000);
  assert.equal(await renderer.evaluate(`document.querySelector('webview')?.getURL() === ${JSON.stringify(firstUrl)}`), true,
    'Inactive browser command disturbed the active conversation');
  console.log(JSON.stringify({ status: 'passed', guestId, restoredGuestId, providerRequests, screenshotHasPng, snapshotHasPageText,
    noGuestErrorSeen, firstUrl, secondUrl: `http://127.0.0.1:${browserPort}/second` }));
} catch (error) {
  const ui = await renderer?.evaluate('({ text: document.body.innerText.slice(-1000), approvals: document.querySelectorAll(".approval-actions").length, toggle: document.querySelector(".panel-toggle")?.getAttribute("aria-expanded"), dialog: document.querySelector(".activity-panel")?.open, instances: document.querySelectorAll(".session-sidebar-instance").length, tabHost: Boolean(document.querySelector(".workspace-tab-host")) })').catch(() => null);
  throw new Error(`${error.message}; screenshotToolSummary=${screenshotToolSummary}; providerTrace=${JSON.stringify(providerTrace.slice(-8))}; ui=${JSON.stringify(ui)}; diagnostics=${diagnostics.slice(-1800)}`);
} finally {
  renderer?.close();
  await stop(app);
  if (vite && vite.exitCode === null) vite.kill();
  browserPage.closeAllConnections();
  provider.closeAllConnections();
  await Promise.all([new Promise((done) => browserPage.close(done)), new Promise((done) => provider.close(done))]);
  await rm(root, { recursive: true, force: true });
}
