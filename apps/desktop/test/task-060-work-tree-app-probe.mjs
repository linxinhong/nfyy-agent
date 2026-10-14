import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rm, stat, symlink, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { chromium } from 'playwright';

const rendererUrl = process.env.TASK_060_RENDERER_URL;
assert.match(rendererUrl ?? '', /^http:\/\/127\.0\.0\.1:\d+\/$/);
const require = createRequire(import.meta.url);
const electron = require('electron');
const desktopRoot = resolve(import.meta.dirname, '..');
const runtimeDist = resolve(desktopRoot, '../runtime/dist');
const desktopMain = join(desktopRoot, 'dist/main.cjs');
const evidence = resolve(desktopRoot, '../../.tasks/verification/TASK-060/screenshots');

async function eventually(check, message, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try { const result = await check(); if (result) return result; }
    catch { /* renderer/runtime may still be starting */ }
    await new Promise((resolveWait) => setTimeout(resolveWait, 80));
  }
  throw new Error(message);
}

async function freePort() {
  const server = (await import('node:net')).createServer();
  await new Promise((resolveListen) => server.listen(0, '127.0.0.1', resolveListen));
  const port = server.address().port;
  await new Promise((resolveClose) => server.close(resolveClose));
  return port;
}

async function launch(appRoot, home, userData) {
  const debuggerPort = await freePort();
  const child = spawn(electron, [`--remote-debugging-port=${debuggerPort}`, appRoot], {
    env: { ...process.env, YUANPU_HOME: home, YUANPU_RENDERER_URL: rendererUrl,
      YUANPU_NODE_BINARY: process.execPath, YUANPU_NOTIFICATIONS_ENABLED: '0',
      TASK_060_PROVIDER_KEY: 'fixture-only' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let diagnostics = '';
  for (const stream of [child.stdout, child.stderr]) {
    stream.on('data', (chunk) => { if (diagnostics.length < 16_384) diagnostics += chunk.toString(); });
  }
  try {
    const endpoint = await eventually(async () => {
      const response = await fetch(`http://127.0.0.1:${debuggerPort}/json/version`);
      return (await response.json()).webSocketDebuggerUrl;
    }, `Electron debugger did not start: ${diagnostics}`);
    const browser = await chromium.connectOverCDP(endpoint);
    const page = await eventually(() => browser.contexts()[0]?.pages()[0], 'Electron page missing');
    await eventually(() => page.evaluate(() => Boolean(window.yuanpu?.runtimeInfo)), 'Electron preload missing');
    await eventually(async () => (await page.evaluate(() => window.yuanpu.runtimeInfo())).workingDirectory,
      'Electron Runtime did not become ready');
    return { child, browser, page, diagnostics: () => diagnostics, async close() {
      await browser.close();
      if (child.exitCode === null && child.signalCode === null) {
        child.kill('SIGUSR2');
        await Promise.race([new Promise((resolveExit) => child.once('exit', resolveExit)),
          new Promise((resolveWait) => setTimeout(resolveWait, 10_000))]);
        if (child.exitCode === null && child.signalCode === null) child.kill();
      }
    } };
  } catch (error) {
    child.kill();
    throw new Error(`${error.message}; diagnostics=${diagnostics.slice(-1000)}`);
  }
}

const root = await mkdtemp(join(tmpdir(), 'yuanpu-task060-electron-'));
const appRoot = join(root, 'apps', 'desktop');
const home = join(root, 'home');
const workspace = join(home, 'workspace');
const userData = join(root, 'desktop-user-data');
let providerRequests = 0;
const prompts = [];
const provider = createServer(async (request, response) => {
  if (request.method !== 'POST' || request.url !== '/v1/chat/completions') {
    response.writeHead(404).end(); return;
  }
  let body = '';
  for await (const chunk of request) body += chunk.toString();
  prompts.push(JSON.parse(body));
  providerRequests += 1;
  response.writeHead(200, { 'content-type': 'text/event-stream' });
  response.write(`data: ${JSON.stringify({ id: `fixture-${providerRequests}`, object: 'chat.completion.chunk', created: 1,
    model: 'fixture-model', choices: [{ index: 0, delta: { role: 'assistant', content: `Saved reply ${providerRequests}.` }, finish_reason: null }] })}\n\n`);
  response.end(`data: ${JSON.stringify({ id: `fixture-${providerRequests}`, object: 'chat.completion.chunk', created: 1,
    model: 'fixture-model', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] })}\n\ndata: [DONE]\n\n`);
});
let instance;
try {
  await mkdir(appRoot, { recursive: true });
  await mkdir(join(root, 'apps', 'runtime'), { recursive: true });
  await mkdir(join(home, 'app'), { recursive: true });
  await mkdir(workspace, { recursive: true });
  await mkdir(userData, { recursive: true });
  await mkdir(evidence, { recursive: true });
  await new Promise((resolveListen) => provider.listen(0, '127.0.0.1', resolveListen));
  await writeFile(join(home, 'app', 'config.json'), JSON.stringify({ schemaVersion: 1,
    provider: 'task-060-fixture', model: 'fixture-model', apiKeyEnv: 'TASK_060_PROVIDER_KEY',
    workingDirectory: workspace, baseUrl: `http://127.0.0.1:${provider.address().port}/v1`, api: 'openai-completions' }));
  await symlink(runtimeDist, join(root, 'apps', 'runtime', 'dist'), 'dir');
  await writeFile(join(appRoot, 'package.json'), JSON.stringify({ name: 'task060-isolated', version: '1.0.0', main: 'entry.cjs' }));
  await writeFile(join(appRoot, 'entry.cjs'), `
    const { app } = require('electron');
    app.setPath('userData', ${JSON.stringify(userData)});
    process.on('SIGUSR2', () => app.quit());
    require(${JSON.stringify(desktopMain)});
  `);
  instance = await launch(appRoot, home, userData);
  let { page } = instance;
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(rendererUrl);
  await page.evaluate(() => window.localStorage.setItem('yuanpu:right-panel-width', '1600'));
  await page.reload();
  await page.goto(`${rendererUrl}#/work`);
  await page.getByRole('button', { name: '打开工作列表' }).click();
  await page.locator('.work-tree-shell').waitFor();
  const folderRow = (name) => page.getByRole('treeitem', { name: new RegExp(name) });
  const addFolder = async (name) => {
    await page.getByRole('button', { name: '新建文件夹' }).click();
    await page.getByRole('textbox', { name: '新文件夹名称' }).fill(name);
    await page.getByRole('textbox', { name: '新文件夹名称' }).press('Enter');
    await folderRow(name).waitFor();
  };
  await addFolder('Acme');
  await folderRow('Acme').click();
  await addFolder('Platform');
  await folderRow('Platform').click();
  await addFolder('Milestones');
  await folderRow('Milestones').click();
  await page.getByRole('button', { name: '新建文件夹' }).click();
  const folderPlacementSize = await page.getByRole('radio', { name: /同级/ }).evaluate((radio) => ({
    radioWidth: radio.getBoundingClientRect().width,
    optionHeight: radio.closest('label').getBoundingClientRect().height,
  }));
  assert.equal(folderPlacementSize.radioWidth <= 24 && folderPlacementSize.optionHeight < 70, true,
    `Folder placement controls expanded unexpectedly: ${JSON.stringify(folderPlacementSize)}`);
  await page.getByRole('radio', { name: /同级/ }).check();
  await page.getByRole('textbox', { name: '新文件夹名称' }).fill('Sibling');
  await page.getByRole('dialog', { name: '新建文件夹' }).getByRole('button', { name: '创建' }).click();
  await folderRow('Sibling').waitFor();
  const sibling = (await page.evaluate(() => window.yuanpu.listWorkFolders())).find((item) => item.name === 'Sibling');
  const platform = (await page.evaluate(() => window.yuanpu.listWorkFolders())).find((item) => item.name === 'Platform');
  assert.equal(sibling.parentId, platform.id, 'Sibling creation ignored the explicit placement');
  await folderRow('Milestones').click();
  await addFolder('Level4');
  await folderRow('Level4').click();
  await addFolder('Level5');
  await folderRow('Level5').click();
  await page.getByRole('button', { name: '新建文件夹' }).click();
  assert.equal(await page.getByRole('radio', { name: /子级/ }).isDisabled(), true, 'A sixth folder level was offered');
  await page.getByRole('dialog', { name: '新建文件夹' }).getByRole('button', { name: '取消' }).click();
  await folderRow('Milestones').click();
  await page.getByRole('button', { name: '切换到文件管理器视图' }).click();
  await page.getByRole('button', { name: 'Acme', exact: true }).click();
  await page.getByRole('button', { name: 'Platform', exact: true }).click();
  await page.getByRole('button', { name: 'Milestones', exact: true }).click();
  await page.getByRole('button', { name: '切换到树状列表视图' }).click();
  await page.getByRole('button', { name: '工作列表全屏' }).click();
  assert.equal(await page.locator('.chat-panel.work-mode .chat-main').evaluate((item) => getComputedStyle(item).display), 'none');
  await page.getByRole('button', { name: '退出工作列表全屏' }).click();
  const widthBefore = Number(await page.getByRole('separator', { name: '调整工作列表宽度' }).getAttribute('aria-valuenow'));
  await page.getByRole('separator', { name: '调整工作列表宽度' }).focus();
  await page.keyboard.press('ArrowRight');
  assert.equal(Number(await page.getByRole('separator', { name: '调整工作列表宽度' }).getAttribute('aria-valuenow')), widthBefore + 20);
  await page.getByRole('button', { name: '新建会话' }).click();
  await eventually(async () => (await page.evaluate(() => window.yuanpu.listWorkConversations())).length >= 2,
    'First nested conversation missing');
  const first = (await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.current);
  assert.equal(first.folderId !== null, true);
  const firstRow = page.locator(`[data-tree-key="conversation:${first.id}"]`);
  await firstRow.press('F2');
  await page.getByRole('textbox', { name: '重命名' }).fill('Roadmap');
  await page.getByRole('textbox', { name: '重命名' }).press('Enter');
  await firstRow.getByText('Roadmap').waitFor();
  await page.getByRole('button', { name: '新建会话' }).click();
  await eventually(async () => (await page.evaluate(() => window.yuanpu.listWorkConversations())).length >= 3,
    'Second nested conversation missing');
  const second = (await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.current);
  assert.notEqual(first.id, second.id);
  assert.equal(first.folderId, second.folderId);
  await page.screenshot({ path: join(evidence, 'tree-created-light.png') });
  await page.getByRole('button', { name: '打开右侧面板' }).click();
  const emptyPanelWidth = await page.locator('.chat-panel.work-mode .activity-panel').evaluate((panel) => panel.getBoundingClientRect().width);
  assert.equal(emptyPanelWidth, 320, 'A new Work conversation inherited the previous global right-panel width');
  await page.getByRole('button', { name: '收起右侧面板' }).click();
  await page.getByRole('button', { name: 'Roadmap的操作' }).click();
  await page.getByRole('menuitem', { name: '修改图标' }).click();
  assert.equal(await page.getByRole('group', { name: '选择图标' }).getByRole('button').count(), 36);
  await page.getByRole('button', { name: '下一页' }).click();
  assert.equal(await page.getByRole('group', { name: '选择图标' }).getByRole('button').count(), 36);
  await page.getByRole('dialog', { name: '修改图标' }).getByRole('button', { name: '成就' }).click();
  await page.getByRole('dialog', { name: '修改图标' }).getByRole('button', { name: '保存' }).click();
  await page.getByRole('button', { name: 'Roadmap的操作' }).click();
  await page.getByRole('menuitem', { name: '修改标签' }).click();
  await page.locator('.work-tree-dialog input[placeholder="输入新标签名称（可选）"]').fill('Priority');
  await page.getByRole('dialog', { name: '修改标签' }).getByRole('button', { name: '保存' }).click();
  await eventually(async () => (await page.evaluate(() => window.yuanpu.listWorkConversations()))
    .find((item) => item.id === first.id)?.tagIds.length === 1, 'Icon/tag edit did not persist');
  await firstRow.click();
  await page.getByRole('textbox', { name: '消息' }).fill('Remember stage needle');
  await page.getByRole('button', { name: '发送消息' }).click();
  await eventually(() => providerRequests === 1, 'First UI model request was not received');
  await page.getByText('Saved reply 1.').waitFor({ timeout: 20_000 });
  const original = (await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.id === first.id);
  await writeFile(join(original.workingDirectory, 'preview.txt'), 'synthetic preview survives');
  await page.getByRole('button', { name: 'Roadmap的操作' }).click();
  await page.getByRole('menuitem', { name: '移动到…' }).click();
  await page.getByRole('dialog', { name: '移动到文件夹' }).getByRole('combobox').selectOption({ label: 'Acme' });
  await page.getByRole('dialog', { name: '移动到文件夹' }).getByRole('button', { name: '移动' }).click();
  const moved = await eventually(async () => {
    const item = (await page.evaluate(() => window.yuanpu.listWorkConversations())).find((row) => row.id === first.id);
    return item?.workingDirectory !== original.workingDirectory ? item : undefined;
  }, 'UI move did not change Work cwd');
  await assert.rejects(stat(original.workingDirectory), { code: 'ENOENT' });
  assert.equal(await readFile(join(moved.workingDirectory, 'preview.txt'), 'utf8'), 'synthetic preview survives');
  assert.equal((await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.id === first.id).title, 'Roadmap');
  assert.equal((await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.id === first.id).iconId, 'trophy');
  await page.screenshot({ path: join(evidence, 'electron-moved.png') });
  await page.getByRole('button', { name: '打开右侧面板' }).click();
  const resizeHandles = await page.evaluate(() => {
    const left = document.querySelector('.left-panel-resize-handle');
    const right = document.querySelector('.activity-resize-handle');
    return {
      leftWidth: getComputedStyle(left).width, rightWidth: getComputedStyle(right).width,
      leftLine: getComputedStyle(left, '::after').width, rightLine: getComputedStyle(right, '::after').width,
    };
  });
  assert.equal(resizeHandles.leftWidth, resizeHandles.rightWidth);
  assert.equal(resizeHandles.leftLine, resizeHandles.rightLine);
  await page.getByRole('button', { name: '铺满右侧面板' }).click();
  const expandedBounds = await page.locator('.chat-panel.work-mode .activity-panel').evaluate((item) => {
    const panel = item.getBoundingClientRect();
    const chat = item.closest('.chat-panel').getBoundingClientRect();
    return { panelWidth: panel.width, chatWidth: chat.width, panelLeft: panel.left, chatLeft: chat.left,
      classes: item.closest('.chat-panel').className,
      gridColumns: getComputedStyle(item.closest('.chat-panel')).gridTemplateColumns,
      panelCssWidth: getComputedStyle(item).width,
      panelGridColumn: getComputedStyle(item).gridColumn,
      panelPosition: getComputedStyle(item).position };
  });
  assert.equal(Math.abs(expandedBounds.panelWidth - expandedBounds.chatWidth) < 2, true,
    `Expanded right panel did not fill the Work view: ${JSON.stringify(expandedBounds)}`);
  await page.getByRole('button', { name: '还原右侧面板' }).click();
  await page.getByRole('button', { name: '打开工作列表' }).click();
  const panelWidths = () => page.locator('.chat-panel.work-mode').evaluate((panel) => ({
    chat: panel.getBoundingClientRect().width,
    left: panel.querySelector('.conversation-list-preview')?.getBoundingClientRect().width ?? 0,
    right: panel.querySelector('.activity-panel').getBoundingClientRect().width,
    classes: panel.className,
    columns: getComputedStyle(panel).gridTemplateColumns,
  }));
  const assertHalfPanels = async (stage) => {
    const widths = await panelWidths();
    assert.equal(Math.abs(widths.left - widths.chat / 2) < 2 && Math.abs(widths.right - widths.chat / 2) < 2, true,
      `${stage} did not split the two panels equally: ${JSON.stringify(widths)}`);
  };
  await page.getByRole('button', { name: '收起右侧面板' }).click();
  await page.getByRole('button', { name: '工作列表全屏' }).click();
  await page.getByRole('button', { name: '打开右侧面板' }).click();
  await assertHalfPanels('Opening right from full left');
  await page.locator('.activity-panel .session-sidebar-instance[aria-hidden="false"] .file-workspace').waitFor();
  await page.getByRole('button', { name: '收起右侧面板' }).click();
  await page.getByRole('button', { name: '退出工作列表全屏' }).click();
  await page.getByRole('button', { name: '打开右侧面板' }).click();
  await page.getByRole('button', { name: '铺满右侧面板' }).click();
  await page.getByRole('button', { name: '打开工作列表' }).click();
  await assertHalfPanels('Opening left from full right');
  await page.locator('.activity-panel .session-sidebar-instance[aria-hidden="false"] .file-workspace').waitFor();
  await page.getByRole('button', { name: '收起工作列表' }).click();
  const rightOnly = await panelWidths();
  assert.equal(Math.abs(rightOnly.right - rightOnly.chat) < 2, true,
    `Right panel did not fill after closing left: ${JSON.stringify(rightOnly)}`);
  await page.getByRole('button', { name: '还原右侧面板' }).click();
  await page.getByRole('button', { name: '打开工作列表' }).click();
  await page.screenshot({ path: join(evidence, 'electron-files-open.png') });
  // The file tree uses a separate renderer surface; click its visible row in Electron.
  await page.mouse.click(1209, 214);
  await page.getByText('synthetic preview survives').waitFor();
  await page.screenshot({ path: join(evidence, 'electron-file-preview.png') });
  await page.getByRole('textbox', { name: '消息' }).fill('Continue after move');
  await page.getByRole('button', { name: '发送消息' }).click();
  await eventually(() => providerRequests === 2, 'Second UI model request was not received');
  await page.getByText('Saved reply 2.').waitFor({ timeout: 20_000 });
  assert.match(JSON.stringify(prompts[1].messages), /Remember stage needle/);
  const database = new DatabaseSync(join(home, 'workflows', 'automation.sqlite'), { readOnly: true });
  const persisted = database.prepare('SELECT pi_session_id, working_directory, folder_id, title, icon_id, archived_at FROM yp_work_conversations WHERE conversation_id=?').get(first.id);
  const binding = database.prepare('SELECT workspace_id FROM yp_conversation_bindings WHERE conversation_id=?').get(first.id);
  database.close();
  assert.equal(persisted.working_directory, moved.workingDirectory);
  assert.equal(binding.workspace_id, moved.workingDirectory);
  assert.equal(persisted.pi_session_id.length > 0, true);
  const { SessionManager } = await import('@earendil-works/pi-coding-agent');
  const jsonl = SessionManager.findById(moved.workingDirectory, persisted.pi_session_id, join(home, 'agent', 'sessions'));
  assert.ok(jsonl);
  const saved = await readFile(jsonl, 'utf8');
  assert.match(saved, /Remember stage needle/);
  assert.match(saved, /Continue after move/);
  assert.equal(JSON.parse(saved.split('\n')[0]).cwd, moved.workingDirectory);
  await page.getByRole('button', { name: 'Roadmap的操作' }).click();
  await page.getByRole('menuitem', { name: '移动到…' }).click();
  await page.getByRole('dialog', { name: '移动到文件夹' }).getByRole('combobox')
    .selectOption({ label: 'Acme / Platform' });
  await symlink(join(home, 'outside'), join(moved.workingDirectory, 'escape'));
  await page.getByRole('dialog', { name: '移动到文件夹' }).getByRole('button', { name: '移动' }).click();
  await page.locator('.work-tree-error').waitFor();
  assert.match(await page.locator('.work-tree-error').innerText(), /符号链接|link/i);
  assert.equal((await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.id === first.id).workingDirectory,
    moved.workingDirectory);
  assert.equal(await readFile(join(moved.workingDirectory, 'preview.txt'), 'utf8'), 'synthetic preview survives');
  await page.screenshot({ path: join(evidence, 'electron-move-rejected.png') });
  await rm(join(moved.workingDirectory, 'escape'));
  await page.getByRole('dialog', { name: '移动到文件夹' }).getByRole('button', { name: '取消' }).click();
  await page.locator('.work-tree-error').getByRole('button', { name: '关闭' }).click();
  const searchBox = page.getByRole('searchbox', { name: '搜索工作会话' });
  for (const [term, field] of [['Roadmap', '标题'], ['Priority', '标签'], ['Remember stage needle', '消息']]) {
    await searchBox.fill(term);
    await eventually(async () => (await page.locator('.work-tree-result').allInnerTexts()).some((result) => result.includes(`${field} ·`)),
      `${field} search result missing`);
  }
  await page.screenshot({ path: join(evidence, 'electron-message-search.png') });
  await searchBox.fill('');
  await page.getByRole('button', { name: 'Roadmap的操作' }).click();
  await page.getByRole('menuitem', { name: '归档会话' }).click();
  await eventually(async () => (await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.id === first.id)?.archived,
    'Archive did not persist');
  await page.getByRole('button', { name: '查看已归档' }).click();
  await page.getByRole('button', { name: 'Roadmap的操作' }).click();
  await page.getByRole('menuitem', { name: '恢复会话' }).click();
  await eventually(async () => !(await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.id === first.id)?.archived,
    'Restore did not persist');
  await page.getByRole('button', { name: '查看工作' }).click();
  await instance.close();
  if (process.env.TASK_060_LEGACY === '1') {
    const legacy = SessionManager.create(workspace, join(home, 'agent', 'sessions'), { id: 'legacy-pi-session' });
    legacy.appendMessage({ role: 'user', content: 'Legacy fixture question', timestamp: Date.now() });
    legacy.appendMessage({ role: 'assistant', content: [{ type: 'text', text: 'Legacy fixture answer' }],
      api: 'anthropic-messages', provider: 'fixture', model: 'fixture-model', stopReason: 'stop',
      usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, timestamp: Date.now() });
    const legacyFile = SessionManager.findById(workspace, 'legacy-pi-session', join(home, 'agent', 'sessions'));
    assert.ok(legacyFile, 'Synthetic legacy JSONL was not discoverable');
    const { readYuanpuChatTranscript } = await import('../../../packages/yuanpu-runtime/dist/index.mjs');
    assert.match(JSON.stringify(readYuanpuChatTranscript(workspace, 'legacy-pi-session', join(home, 'agent', 'sessions'))),
      /Legacy fixture question/, 'Synthetic legacy transcript was not readable before restart');
    const legacyDb = new DatabaseSync(join(home, 'workflows', 'automation.sqlite'));
    legacyDb.prepare(`INSERT INTO yp_conversation_bindings
      (binding_id, entry_point, authority_id, subject_id, namespace, conversation_id,
        thread_id, pi_session_id, workspace_id, created_at, updated_at)
      VALUES ('legacy-binding', 'desktop', 'local-desktop', 'local-user', 'desktop', 'default',
        '', 'legacy-pi-session', ?, '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`).run(workspace);
    legacyDb.close();
  }
  instance = await launch(appRoot, home, userData);
  page = instance.page;
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${rendererUrl}#/work`);
  await page.getByRole('button', { name: '打开工作列表' }).click();
  await page.getByRole('button', { name: '展开Acme' }).click();
  await page.getByRole('button', { name: 'Roadmap的操作' }).waitFor();
  const restarted = (await page.evaluate(() => window.yuanpu.listWorkConversations())).find((item) => item.id === first.id);
  assert.equal(restarted.workingDirectory, moved.workingDirectory);
  assert.equal(restarted.iconId, 'trophy');
  assert.equal(restarted.tagIds.length, 1);
  await page.locator(`[data-tree-key="conversation:${first.id}"]`).click();
  await page.getByText('Saved reply 2.').waitFor();
  await page.getByRole('textbox', { name: '消息' }).fill('Continue after restart');
  await page.getByRole('button', { name: '发送消息' }).click();
  const resumedPrompt = await eventually(() => prompts.find((prompt) => JSON.stringify(prompt.messages).includes('Continue after restart')),
    'Restart continuation request missing');
  await page.getByText(`Saved reply ${prompts.indexOf(resumedPrompt) + 1}.`).waitFor({ timeout: 20_000 });
  await page.getByText('任务执行中').waitFor({ state: 'hidden' });
  assert.match(JSON.stringify(resumedPrompt.messages), /Remember stage needle/);
  assert.match(JSON.stringify(resumedPrompt.messages), /Continue after move/);
  await page.screenshot({ path: join(evidence, 'electron-restarted.png') });
  if (process.env.TASK_060_LEGACY === '1') {
    await page.getByRole('button', { name: '查看已归档' }).click();
    await page.getByRole('treeitem', { name: '旧工作（只读）' }).click();
    await page.getByText('Legacy fixture question').waitFor();
    assert.equal(await page.getByRole('textbox', { name: '消息' }).isDisabled(), true);
    await page.screenshot({ path: join(evidence, 'electron-legacy-readonly.png') });
  }
  await page.goto(`${rendererUrl}#/settings`);
  for (const [name, value] of [['浅色', 'yuanpu-light'], ['深色', 'yuanpu-dark'], ['MindLink 测试主题', 'mindlink']]) {
    await page.getByRole('group', { name: '界面主题' }).getByRole('button', { name }).click();
    assert.equal(await page.evaluate(() => document.documentElement.dataset.yuanpuTheme), value);
    await page.goto(`${rendererUrl}#/work`);
    if (await page.getByRole('button', { name: '打开工作列表' }).isVisible().catch(() => false)) {
      await page.getByRole('button', { name: '打开工作列表' }).click();
    }
    await page.locator('.work-tree-shell').waitFor();
    await page.screenshot({ path: join(evidence, `electron-theme-${value}.png`) });
    await page.goto(`${rendererUrl}#/settings`);
  }
  console.log(JSON.stringify({ status: 'core-pass', providerRequests, savedSession: Boolean(jsonl) }));
} finally {
  await instance?.close();
  provider.closeAllConnections();
  await new Promise((resolveClose) => provider.close(resolveClose));
  await rm(root, { recursive: true, force: true });
}
