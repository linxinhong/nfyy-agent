import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { generateKeyPairSync, randomBytes, randomUUID, sign } from 'node:crypto';
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { DatabaseSync } from 'node:sqlite';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { SessionManager } from '@earendil-works/pi-coding-agent';

import {
  AGENT_CONTRACT_VERSION,
  SCHEDULE_CONTRACT_VERSION,
  capabilityApprovalSigningPayload,
} from '@yuanpu-agent/protocol';

const runtimePackage = JSON.parse(await readFile(new URL('../package.json', import.meta.url), 'utf8'));

async function stopChild(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  await new Promise((resolve) => {
    child.once('exit', resolve);
    child.kill();
  });
}

test('runtime CLI prints the default greeting from the Yuanpu runtime kit', () => {
  const output = execFileSync(process.execPath, ['dist/index.cjs'], { encoding: 'utf8' });
  assert.equal(output.trim(), 'Hello, world!');
});

test('runtime CLI accepts a name', () => {
  const output = execFileSync(process.execPath, ['dist/index.cjs', '--name', 'CI'], {
    encoding: 'utf8',
  });
  assert.equal(output.trim(), 'Hello, CI!');
});

test('optional Enterprise WeChat startup failure does not prevent Runtime readiness', async (context) => {
  for (const [name, document, expectedEvent] of [
    ['malformed JSON', '{', 'configuration_invalid'],
    ['legacy unknown field', {
      schemaVersion: 1,
      connections: [{
        enabled: false,
        provider: 'wecom',
        connectionId: 'imc_fixture',
        groupAllowlist: [],
      }],
    }, 'configuration_invalid'],
    ['unavailable Keychain credential', {
      schemaVersion: 1,
      connections: [{
        enabled: true,
        provider: 'wecom',
        connectionId: 'imc_fixture',
        providerAccountRef: 'fixture-bot',
        credentialRefs: { botSecret: 'keychain:yuanpu/im/imc_fixture/bot-secret' },
        directMessagePolicy: 'paired-only',
        groupPolicy: 'allowlist-paired-sender-and-provider-at-mention',
        acceptedMessageTypes: ['text'],
      }],
    }, 'credential_unavailable'],
  ]) {
    await context.test(name, async (scenario) => {
      const home = await mkdtemp(join(tmpdir(), 'yuanpu-optional-wecom-'));
      await mkdir(join(home, 'app', 'connections'), { recursive: true });
      await writeFile(
        join(home, 'app', 'connections', 'wecom.json'),
        typeof document === 'string' ? document : JSON.stringify(document),
      );
      const token = randomBytes(32).toString('hex');
      const approvalPublicKey = generateKeyPairSync('ed25519').publicKey
        .export({ type: 'spki', format: 'der' }).toString('base64');
      const child = spawn(process.execPath, ['dist/index.cjs', '--serve', '--port', '0'], {
        stdio: ['pipe', 'pipe', 'pipe'],
        env: {
          ...process.env,
          YUANPU_HOME: home,
          YUANPU_PYTHON_MCP_EXECUTABLE: '',
          YUANPU_PYTHON_MCP_ROOT: '',
        },
      });
      let stderr = '';
      child.stderr.setEncoding('utf8');
      child.stderr.on('data', (chunk) => { stderr += chunk; });
      child.stdin.end(`${JSON.stringify({ token, approvalPublicKey, parentPid: process.pid })}\n`);
      let assistantWorkerPid;
      scenario.after(async () => {
        await stopChild(child);
        if (assistantWorkerPid) {
          const deadline = Date.now() + 5_000;
          while (Date.now() < deadline) {
            try { process.kill(assistantWorkerPid, 0); }
            catch { break; }
            await new Promise((resolveWait) => setTimeout(resolveWait, 50));
          }
        }
        await rm(home, { recursive: true, force: true });
      });
      const ready = await new Promise((resolve, reject) => {
        let stdout = '';
        const timeout = setTimeout(() => reject(new Error('Runtime did not become ready')), 15_000);
        child.once('error', reject);
        child.once('exit', (code) => reject(new Error(`Runtime exited before ready: ${code}`)));
        child.stdout.setEncoding('utf8');
        child.stdout.on('data', (chunk) => {
          stdout += chunk;
          const newline = stdout.indexOf('\n');
          if (newline < 0) return;
          clearTimeout(timeout);
          resolve(JSON.parse(stdout.slice(0, newline)));
        });
      });
      assert.equal(ready.event, 'ready');
      assistantWorkerPid = ready.assistantWorkerPid;
      const health = await fetch(`http://${ready.host}:${ready.port}/v1/health`, {
        headers: { authorization: `Bearer ${token}` },
      });
      assert.equal(health.status, 200);
      assert.match(stderr, new RegExp(`\\[wecom\\] ${expectedEvent}`));
      assert.doesNotMatch(stderr, /fixture-bot|bot-secret/);
    });
  }
});

test('connection management preserves the previous config when an enabled credential is unavailable', async (context) => {
  const home = await mkdtemp(join(tmpdir(), 'yuanpu-connection-management-'));
  const token = randomBytes(32).toString('hex');
  const connectionId = `imc_${randomUUID()}`;
  const approvalPublicKey = generateKeyPairSync('ed25519').publicKey
    .export({ type: 'spki', format: 'der' }).toString('base64');
  const child = spawn(process.execPath, ['dist/index.cjs', '--serve', '--port', '0'], {
    stdio: ['pipe', 'pipe', 'pipe'],
    env: { ...process.env, YUANPU_HOME: home, YUANPU_PYTHON_MCP_EXECUTABLE: '', YUANPU_PYTHON_MCP_ROOT: '' },
  });
  child.stdin.end(`${JSON.stringify({ token, approvalPublicKey, parentPid: process.pid })}\n`);
  context.after(async () => {
    await stopChild(child);
    await rm(home, { recursive: true, force: true });
  });
  const ready = await new Promise((resolve, reject) => {
    let stdout = '';
    const timeout = setTimeout(() => reject(new Error('Runtime did not become ready')), 15_000);
    child.once('error', reject);
    child.once('exit', (code) => reject(new Error(`Runtime exited before ready: ${code}`)));
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk) => {
      stdout += chunk;
      const newline = stdout.indexOf('\n');
      if (newline < 0) return;
      clearTimeout(timeout);
      resolve(JSON.parse(stdout.slice(0, newline)));
    });
  });
  const root = `http://${ready.host}:${ready.port}`;
  const send = async (path, method = 'GET', body) => fetch(`${root}${path}`, {
    method,
    headers: { authorization: `Bearer ${token}`, ...(body ? { 'content-type': 'application/json' } : {}) },
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
  const created = await send('/v1/connections/wecom', 'POST', {
    connectionId, botId: 'fixture-bot', enabled: false,
  });
  assert.equal(created.status, 200);
  const createdBody = await created.text();
  assert.doesNotMatch(createdBody, /fixture-bot|bot-secret/);
  assert.equal(JSON.parse(createdBody).status, 'disabled');
  const checked = await send(`/v1/connections/wecom/${connectionId}/test`, 'POST');
  assert.equal((await checked.json()).status, 'disabled');
  const failed = await send('/v1/connections/wecom', 'POST', { connectionId, enabled: true });
  assert.equal(failed.status, 400);
  assert.match((await failed.json()).error, /previous configuration was restored/);
  const persisted = JSON.parse(await readFile(join(home, 'app', 'connections', 'wecom.json'), 'utf8'));
  assert.equal(persisted.connections[0].enabled, false);
  const listed = await send('/v1/connections/wecom');
  assert.equal((await listed.json()).connections[0].status, 'disabled');
  const unconfigured = await send('/v1/connections/tailscale');
  assert.deepEqual(await unconfigured.json(), { hasAuthKey: false, node: { state: 'unavailable' } });
  const tailscale = await send('/v1/connections/tailscale', 'POST', { authKey: 'tskey-auth-fixture' });
  assert.equal(tailscale.status, 200);
  const tailscaleBody = await tailscale.text();
  assert.doesNotMatch(tailscaleBody, /tskey-auth-fixture/);
  assert.deepEqual(JSON.parse(tailscaleBody), { hasAuthKey: true, node: { state: 'unavailable' } });
  const tailscalePath = join(home, 'app', 'connections', 'tailscale.json');
  if (process.platform !== 'win32') assert.equal((await stat(tailscalePath)).mode & 0o777, 0o600);
  const retained = await send('/v1/connections/tailscale', 'POST', {});
  assert.equal(retained.status, 200);
  assert.equal((await retained.json()).hasAuthKey, true);
  const persistedTailscale = JSON.parse(await readFile(tailscalePath, 'utf8'));
  assert.equal(persistedTailscale.authKey, 'tskey-auth-fixture');
  const tailscaleListed = await send('/v1/connections/tailscale');
  assert.equal((await tailscaleListed.json()).hasAuthKey, true);
  const extraField = await send('/v1/connections/tailscale', 'POST', { apiKey: 'unsupported' });
  assert.notEqual(extraField.status, 200);
  assert.equal(JSON.parse(await readFile(tailscalePath, 'utf8')).authKey, 'tskey-auth-fixture');
  const removed = await send('/v1/connections/tailscale', 'POST', { removeAuthKey: true });
  assert.deepEqual(await removed.json(), { hasAuthKey: false, node: { state: 'unavailable' } });
  assert.equal((await send('/v1/chat/submit', 'POST', { message: '' })).status, 400);
  assert.equal((await send('/v1/health')).status, 200);
});

test('runtime server exposes its protocol and greeting', async (context) => {
  const home = await mkdtemp(join(tmpdir(), 'yuanpu-runtime-test-'));
  const pluginsRoot = join(home, 'plugins');
  const installPath = join(pluginsRoot, 'installed', 'fixture', 'node_modules', 'pi-mcp-adapter');
  await mkdir(installPath, { recursive: true });
  await writeFile(join(installPath, 'package.json'), JSON.stringify({
    name: 'pi-mcp-adapter',
    version: '1.0.0',
    pi: { extensions: ['./index.ts'] },
  }));
  await writeFile(
    join(installPath, 'index.ts'),
    'export default function (pi) { pi.registerCommand("fixture", { description: "fixture", handler: () => {} }); }\n',
  );
  await writeFile(join(pluginsRoot, 'state.json'), JSON.stringify({
    schemaVersion: 1,
    plugins: {
      'pi-mcp-adapter': {
        name: 'pi-mcp-adapter',
        version: '1.0.0',
        description: 'Runtime bundle fixture',
        source: 'npm:pi-mcp-adapter@1.0.0',
        installPath,
        enabled: false,
        installedAt: '2026-09-21T00:00:00.000Z',
      },
    },
  }));
  const localSkillRoot = join(home, 'skills', 'runtime-fixture');
  await mkdir(localSkillRoot, { recursive: true });
  await writeFile(join(localSkillRoot, 'SKILL.md'), [
    '---',
    'name: runtime-fixture',
    'description: Runtime skill fixture.',
    '---',
  ].join('\n'));
  const token = randomBytes(32).toString('hex');
  const approvalKeyPair = generateKeyPairSync('ed25519');
  const approvalPublicKey = approvalKeyPair.publicKey.export({ type: 'spki', format: 'der' })
    .toString('base64');
  const capabilityResourceRoot = join(home, 'capability-resource');
  const capabilityTrustRoot = join(capabilityResourceRoot, 'trust-root.json');
  await mkdir(capabilityResourceRoot, { recursive: true });
  await writeFile(capabilityTrustRoot, JSON.stringify({
    keyId: 'runtime-test',
    publicKeyPem: approvalKeyPair.publicKey.export({ type: 'spki', format: 'pem' }).toString(),
  }));
  await writeFile(join(capabilityResourceRoot, 'manifest.json'), JSON.stringify({ version: '0.1.0' }));
  const child = spawn(process.execPath, [
    'dist/index.cjs', '--serve', '--port', '0',
  ], {
    stdio: ['pipe', 'pipe', 'inherit'],
    env: {
      ...process.env,
      YUANPU_HOME: home,
      YUANPU_CAPABILITY_TRUST_ROOT_FILE: capabilityTrustRoot,
      YUANPU_PYTHON_MCP_EXECUTABLE: '',
      YUANPU_PYTHON_MCP_ROOT: '',
    },
  });
  child.stdin.end(`${JSON.stringify({ token, approvalPublicKey, parentPid: process.pid })}\n`);
  context.after(async () => {
    await stopChild(child);
    await rm(home, { recursive: true, force: true });
  });

  const ready = await new Promise((resolve, reject) => {
    let output = '';
    const timeout = setTimeout(() => reject(new Error('runtime server did not start')), 15_000);
    child.once('error', reject);
    child.stdout.on('data', (chunk) => {
      output += chunk.toString();
      const newline = output.indexOf('\n');
      if (newline < 0) return;
      clearTimeout(timeout);
      resolve(JSON.parse(output.slice(0, newline)));
    });
  });

  const headers = { authorization: `Bearer ${token}` };
  const workEndpoint = `http://${ready.host}:${ready.port}/v1/work/conversations`;
  const initialWork = await fetch(workEndpoint, { headers }).then((response) => response.json());
  assert.equal(initialWork.length, 1);
  assert.equal(initialWork[0].current, true);
  assert.match(initialWork[0].workingDirectory, new RegExp(`^${home}/workspace/c-[a-f0-9-]{36}$`));
  assert.equal((await stat(initialWork[0].workingDirectory)).isDirectory(), true);
  const createdWork = await fetch(workEndpoint, { method: 'POST', headers }).then((response) => response.json());
  assert.notEqual(createdWork.id, initialWork[0].id);
  assert.notEqual(createdWork.workingDirectory, initialWork[0].workingDirectory);
  const workHeaders = { ...headers, 'content-type': 'application/json' };
  const folderEndpoint = `http://${ready.host}:${ready.port}/v1/work/folders`;
  const parent = await fetch(folderEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ parentId: null, name: 'Research', iconId: 'folder' }) }).then((response) => response.json());
  const childFolder = await fetch(folderEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ parentId: parent.id, name: 'Models', iconId: 'code',
      requestId: '11111111-1111-4111-8111-111111111111' }) }).then((response) => response.json());
  const childRetry = await fetch(folderEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ parentId: parent.id, name: 'Models', iconId: 'code',
      requestId: '11111111-1111-4111-8111-111111111111' }) }).then((response) => response.json());
  assert.equal(childRetry.id, childFolder.id);
  assert.equal((await fetch(folderEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ parentId: parent.id, name: 'Different', iconId: 'code',
      requestId: '11111111-1111-4111-8111-111111111111' }) })).status, 400);
  const nested = await fetch(workEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ folderId: childFolder.id,
      requestId: '22222222-2222-4222-8222-222222222222' }) }).then((response) => response.json());
  const nestedRetry = await fetch(workEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ folderId: childFolder.id,
      requestId: '22222222-2222-4222-8222-222222222222' }) }).then((response) => response.json());
  assert.equal(nestedRetry.id, nested.id);
  assert.equal(nested.folderId, childFolder.id);
  assert.equal((await stat(nested.workingDirectory)).isDirectory(), true);
  assert.match(nested.workingDirectory, /\/f-[a-f0-9-]{36}\/f-[a-f0-9-]{36}\/c-[a-f0-9-]{36}$/);
  assert.equal((await fetch(workEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ workingDirectory: '/tmp/arbitrary' }) })).status, 400);
  const renamed = await fetch(folderEndpoint, { method: 'PATCH', headers: workHeaders,
    body: JSON.stringify({ folderId: parent.id, name: 'Renamed' }) }).then((response) => response.json());
  assert.equal(renamed.relativeDirectory, parent.relativeDirectory);
  assert.equal((await fetch(folderEndpoint, { method: 'PATCH', headers: workHeaders,
    body: JSON.stringify({ folderId: parent.id, parentId: childFolder.id }) })).status, 400);
  assert.equal((await fetch(folderEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ parentId: 'folder:foreign', name: 'bad' }) })).status, 400);
  assert.equal((await fetch(folderEndpoint, { method: 'PATCH', headers: workHeaders,
    body: JSON.stringify({ folderId: parent.id, iconId: 'execute-file' }) })).status, 400);
  const tagEndpoint = `http://${ready.host}:${ready.port}/v1/work/tags`;
  const tag = await fetch(tagEndpoint, { method: 'POST',
    headers: workHeaders, body: JSON.stringify({ name: 'review', color: 'blue',
      requestId: '33333333-3333-4333-8333-333333333333' }) }).then((response) => response.json());
  assert.equal((await fetch(tagEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ name: 'review', color: 'red',
      requestId: '33333333-3333-4333-8333-333333333333' }) })).status, 400);
  const edited = await fetch(workEndpoint, { method: 'PATCH', headers: workHeaders,
    body: JSON.stringify({ conversationId: nested.id, title: 'Plan', tagIds: [tag.id], archived: true }) })
    .then((response) => response.json());
  assert.equal(edited.archived, true);
  assert.deepEqual(edited.tagIds, [tag.id]);
  assert.equal((await stat(nested.workingDirectory)).isDirectory(), true);
  assert.equal((await fetch(workEndpoint, { method: 'PUT', headers: workHeaders,
    body: JSON.stringify({ conversationId: nested.id }) })).status, 404);
  const restored = await fetch(workEndpoint, { method: 'PATCH', headers: workHeaders,
    body: JSON.stringify({ conversationId: nested.id, archived: false }) }).then((response) => response.json());
  assert.equal(restored.workingDirectory, nested.workingDirectory);
  const searchDb = new DatabaseSync(join(home, 'workflows', 'automation.sqlite'));
  const piSessionId = searchDb.prepare('SELECT pi_session_id FROM yp_work_conversations WHERE conversation_id=?')
    .get(nested.id).pi_session_id;
  searchDb.close();
  const savedSession = SessionManager.create(nested.workingDirectory, join(home, 'agent', 'sessions'), { id: piSessionId });
  const searchEntryId = savedSession.appendMessage({ role: 'user', content: 'searchable needle', timestamp: Date.now() });
  savedSession.appendMessage({ role: 'assistant', content: [{ type: 'text', text: 'visible answer' }],
    api: 'fixture', provider: 'fixture', model: 'fixture', stopReason: 'stop', timestamp: Date.now(),
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } });
  const trajectoryEndpoint = `http://${ready.host}:${ready.port}/v1/work/trajectory?conversationId=${encodeURIComponent(nested.id)}`;
  const trajectoryBeforeMove = await fetch(trajectoryEndpoint, { headers }).then((response) => response.json());
  assert.equal(trajectoryBeforeMove.conversationId, nested.id);
  assert.equal(trajectoryBeforeMove.rounds, 1);
  assert.deepEqual(trajectoryBeforeMove.rows.map((row) => row.kind), ['user', 'assistant']);
  assert.equal((await fetch(`http://${ready.host}:${ready.port}/v1/work/trajectory?conversationId=work%3Aforeign`,
    { headers })).status, 404);
  assert.equal((await fetch(trajectoryEndpoint)).status, 401);
  const cancelTicketEndpoint = `http://${ready.host}:${ready.port}/v1/work/subagents/cancel`;
  assert.equal((await fetch(cancelTicketEndpoint, { method: 'POST',
    headers: workHeaders, body: JSON.stringify({ conversationId: nested.id, runId: '../other' }) })).status, 400);
  assert.equal((await fetch(cancelTicketEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ conversationId: 'work:foreign', runId: randomUUID() }) })).status, 404);
  assert.equal((await fetch(cancelTicketEndpoint, { method: 'POST', headers: workHeaders,
    body: JSON.stringify({ conversationId: nested.id, runId: randomUUID() }) })).status, 409);
  assert.equal((await fetch(cancelTicketEndpoint, { method: 'POST',
    body: JSON.stringify({ conversationId: nested.id, runId: randomUUID() }) })).status, 401);
  const searchEndpoint = `http://${ready.host}:${ready.port}/v1/work/search?query=needle`;
  const searchBeforeMove = await fetch(searchEndpoint, { headers }).then((response) => response.json());
  assert.equal(searchBeforeMove.items[0].messageEntryId, searchEntryId);
  assert.deepEqual(searchBeforeMove.items[0].folderPath.map((part) => part.name), ['Renamed', 'Models']);
  assert.equal((await fetch(`http://${ready.host}:${ready.port}/v1/work/search?query=`, { headers })).status, 400);
  assert.equal((await fetch(`http://${ready.host}:${ready.port}/v1/work/search?query=needle&limit=51`, { headers })).status, 400);
  const windowEndpoint = `http://${ready.host}:${ready.port}/v1/work/messages/window?conversationId=${encodeURIComponent(nested.id)}&entryId=${searchEntryId}`;
  const windowBeforeMove = await fetch(windowEndpoint, { headers }).then((response) => response.json());
  assert.equal(windowBeforeMove.messages[windowBeforeMove.targetIndex].id, searchEntryId);
  assert.equal((await fetch(`http://${ready.host}:${ready.port}/v1/work/messages/window?conversationId=work%3Aforeign&entryId=${searchEntryId}`,
    { headers })).status, 404);
  await writeFile(join(nested.workingDirectory, 'moved-preview.txt'), 'preview survives move');
  const moveRequest = { requestId: randomUUID(), kind: 'conversation', id: nested.id, targetFolderId: parent.id };
  const moveResponse = await fetch(`http://${ready.host}:${ready.port}/v1/work/move`, {
    method: 'POST', headers: workHeaders, body: JSON.stringify(moveRequest),
  });
  assert.equal(moveResponse.status, 200);
  const moveResult = await moveResponse.json();
  assert.deepEqual(moveResult.conversationIds, [nested.id]);
  assert.match(moveResult.warning, /旧绝对路径/);
  await assert.rejects(stat(nested.workingDirectory), { code: 'ENOENT' });
  const relocated = (await fetch(workEndpoint, { headers }).then((response) => response.json()))
    .find((item) => item.id === nested.id);
  assert.equal(relocated.folderId, parent.id);
  assert.deepEqual(relocated.previousWorkingDirectories, [nested.workingDirectory]);
  const searchAfterMove = await fetch(searchEndpoint, { headers }).then((response) => response.json());
  const trajectoryAfterMove = await fetch(trajectoryEndpoint, { headers }).then((response) => response.json());
  assert.deepEqual(trajectoryAfterMove.rows.map((row) => row.kind), ['user', 'assistant']);
  assert.equal(searchAfterMove.items[0].messageEntryId, searchEntryId);
  assert.deepEqual(searchAfterMove.items[0].folderPath.map((part) => part.name), ['Renamed']);
  assert.equal((await fetch(windowEndpoint, { headers }).then((response) => response.json())).status, 'ok');
  const movedPreview = await fetch(`http://${ready.host}:${ready.port}/v1/work/files/content?conversationId=${nested.id}&path=moved-preview.txt`, { headers }).then((response) => response.json());
  assert.equal(movedPreview.content, 'preview survives move');
  const retryMove = await fetch(`http://${ready.host}:${ready.port}/v1/work/move`, {
    method: 'POST', headers: workHeaders, body: JSON.stringify(moveRequest),
  });
  assert.deepEqual(await retryMove.json(), moveResult);

  assert.equal((await fetch(workEndpoint, { method: 'PATCH', headers: workHeaders,
    body: JSON.stringify({ conversationId: 'default', archived: false }) })).status, 400);
  const switchedWork = await fetch(workEndpoint, { method: 'PUT',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ conversationId: initialWork[0].id }),
  }).then((response) => response.json());
  assert.equal(switchedWork.id, initialWork[0].id);
  assert.equal((await fetch(`${workEndpoint}?ignored=1`, { headers }).then((response) => response.json()))
    .find((item) => item.current).id, initialWork[0].id);
  const archivedSubmit = await fetch(`http://${ready.host}:${ready.port}/v1/chat/submit`, {
    method: 'POST', headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ message: 'must not enter legacy default', surface: 'work', conversationId: 'default' }),
  });
  assert.equal(archivedSubmit.status, 400);
  assert.equal((await fetch(`http://${ready.host}:${ready.port}/v1/desktop/transcript?surface=work&conversationId=${createdWork.id}`,
    { headers }).then((response) => response.json())).length, 0);
  const unauthorized = await fetch(`http://${ready.host}:${ready.port}/v1/health`);
  const badToken = await fetch(`http://${ready.host}:${ready.port}/v1/health`, {
    headers: { authorization: 'Bearer invalid-stage-verification-token' },
  });
  const health = await fetch(`http://${ready.host}:${ready.port}/v1/health`, { headers }).then((response) =>
    response.json(),
  );
  const shortcutUrl = `http://${ready.host}:${ready.port}/v1/settings/hotkeys`;
  assert.equal((await fetch(shortcutUrl)).status, 401);
  assert.deepEqual((await fetch(shortcutUrl, { headers }).then((response) => response.json())).bindings, {});
  const savedShortcut = await fetch(shortcutUrl, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ id: 'conversation.interrupt', binding: null }),
  });
  assert.equal(savedShortcut.status, 200);
  assert.equal((await savedShortcut.json()).bindings['conversation.interrupt'], null);
  assert.equal(JSON.parse(await readFile(join(home, 'app', 'config.json'), 'utf8')).hotkeys['conversation.interrupt'], null);
  const greeting = await fetch(
    `http://${ready.host}:${ready.port}/v1/greeting?name=Integration`,
    { headers },
  ).then((response) => response.json());
  const plugins = await fetch(`http://${ready.host}:${ready.port}/v1/plugins`, { headers })
    .then((response) => response.json());
  const localSkills = await fetch(`http://${ready.host}:${ready.port}/v1/skills/local`, { headers })
    .then((response) => response.json());
  const unauthorizedApprovals = await fetch(
    `http://${ready.host}:${ready.port}/v1/capabilities/approvals`,
  );
  const approvals = await fetch(
    `http://${ready.host}:${ready.port}/v1/capabilities/approvals`,
    { headers },
  ).then((response) => response.json());
  const invalidApprovalDecision = await fetch(
    `http://${ready.host}:${ready.port}/v1/capabilities/approvals/decision`,
    {
      method: 'POST',
      headers: { ...headers, 'content-type': 'application/json' },
      body: JSON.stringify({ requestId: '', decision: 'approved' }),
    },
  );
  const forgedApprovalDecision = await fetch(
    `http://${ready.host}:${ready.port}/v1/capabilities/approvals/decision`,
    {
      method: 'POST',
      headers: { ...headers, 'content-type': 'application/json' },
      body: JSON.stringify({
        requestId: 'unknown',
        decision: 'approved',
        issuedAt: Date.now(),
        nonce: randomBytes(16).toString('base64url'),
        signature: randomBytes(64).toString('base64url'),
      }),
    },
  );
  const signedUnknown = {
    requestId: 'unknown',
    decision: 'approved',
    issuedAt: Date.now(),
    nonce: randomBytes(16).toString('base64url'),
  };
  const signedUnknownBody = JSON.stringify({
    ...signedUnknown,
    signature: sign(
      null,
      capabilityApprovalSigningPayload(signedUnknown),
      approvalKeyPair.privateKey,
    ).toString('base64url'),
  });
  const signedUnknownDecision = await fetch(
    `http://${ready.host}:${ready.port}/v1/capabilities/approvals/decision`,
    {
      method: 'POST',
      headers: { ...headers, 'content-type': 'application/json' },
      body: signedUnknownBody,
    },
  );
  const replayedSignedDecision = await fetch(
    `http://${ready.host}:${ready.port}/v1/capabilities/approvals/decision`,
    {
      method: 'POST',
      headers: { ...headers, 'content-type': 'application/json' },
      body: signedUnknownBody,
    },
  );
  const invalidPluginInstall = await fetch(`http://${ready.host}:${ready.port}/v1/plugins/install`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ source: '' }),
  });
  const floatingPluginInstall = await fetch(`http://${ready.host}:${ready.port}/v1/plugins/install`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ source: 'npm:pi-example@latest' }),
  });
  const floatingPluginError = await floatingPluginInstall.json();
  const oversizedManifest = createServer((_request, response) => {
    const body = JSON.stringify({ padding: 'x'.repeat(300 * 1024) });
    response.writeHead(200, {
      'content-type': 'application/json',
      'content-length': Buffer.byteLength(body),
    });
    response.end(body);
  });
  await new Promise((resolve, reject) => {
    oversizedManifest.once('error', reject);
    oversizedManifest.listen(0, '127.0.0.1', resolve);
  });
  context.after(() => oversizedManifest.close());
  const manifestAddress = oversizedManifest.address();
  assert.notEqual(manifestAddress, null);
  assert.equal(typeof manifestAddress, 'object');
  const artifactInstall = await fetch(`http://${ready.host}:${ready.port}/v1/plugins/install`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({
      source: `artifact:http://127.0.0.1:${manifestAddress.port}/manifest.json`,
    }),
  });
  const artifactInstallError = await artifactInstall.json();
  const enablePlugin = await fetch(`http://${ready.host}:${ready.port}/v1/plugins/state`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ name: 'pi-mcp-adapter', enabled: true }),
  });
  const pluginConfig = await fetch(
    `http://${ready.host}:${ready.port}/v1/plugins/config?name=pi-mcp&scope=user`,
    { headers },
  ).then((response) => response.json());
  const savePluginConfig = await fetch(`http://${ready.host}:${ready.port}/v1/plugins/config/save`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({
      name: 'pi-mcp',
      scope: 'user',
      value: { mcpServers: { docs: { url: 'https://example.test/mcp' } } },
    }),
  });
  const invalidChat = await fetch(`http://${ready.host}:${ready.port}/v1/chat`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ message: '' }),
  });
  const unconfiguredChat = await fetch(`http://${ready.host}:${ready.port}/v1/chat`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ message: 'Hello' }),
  });
  const unconfiguredError = await unconfiguredChat.json();
  const runtimeConfig = JSON.parse(await readFile(join(home, 'app', 'config.json'), 'utf8'));
  const agentRequest = {
    contractVersion: AGENT_CONTRACT_VERSION,
    entryPoint: 'desktop',
    identity: {
      kind: 'local_user',
      subjectId: 'local-user',
      authorityId: 'local-desktop',
      authenticatedBy: 'electron',
    },
    workspaceId: runtimeConfig.workingDirectory,
    conversation: { namespace: 'desktop', conversationId: 'runtime-test' },
    input: { type: 'text', text: 'Hello from AgentService' },
    idempotencyKey: 'runtime-test-request',
    delivery: { kind: 'desktop' },
  };
  const agentSubmissionResponse = await fetch(`http://${ready.host}:${ready.port}/v1/agent/runs`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify(agentRequest),
  });
  const agentSubmission = await agentSubmissionResponse.json();
  const legacyAgentSubmission = await fetch(`http://${ready.host}:${ready.port}/v1/agent/runs`, {
    method: 'POST', headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ ...agentRequest, conversation: { namespace: 'desktop', conversationId: 'default' },
      idempotencyKey: 'legacy-default-rejected' }),
  });
  assert.equal(legacyAgentSubmission.status, 403);
  for (const conversationId of ['assistant', 'assistant:desktop:forged', 'assistant:wecom:forged', 'asst_forged']) {
    const bypass = await fetch(`http://${ready.host}:${ready.port}/v1/agent/runs`, {
      method: 'POST', headers: { ...headers, 'content-type': 'application/json' },
      body: JSON.stringify({ ...agentRequest, conversation: { namespace: 'desktop', conversationId },
        idempotencyKey: `reject-${conversationId}` }),
    });
    assert.equal(bypass.status, 403, conversationId);
  }
  const duplicateAgentSubmission = await fetch(`http://${ready.host}:${ready.port}/v1/agent/runs`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify(agentRequest),
  }).then((response) => response.json());
  const agentConflict = await fetch(`http://${ready.host}:${ready.port}/v1/agent/runs`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({ ...agentRequest, input: { type: 'text', text: 'Changed' } }),
  });
  let agentRun;
  for (let attempt = 0; attempt < 100; attempt += 1) {
    agentRun = await fetch(
      `http://${ready.host}:${ready.port}/v1/agent/runs/${agentSubmission.runId}`,
      { headers },
    ).then((response) => response.json());
    if (agentRun.status === 'failed') break;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  const spoofedAgentSubmission = await fetch(`http://${ready.host}:${ready.port}/v1/agent/runs`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({
      ...agentRequest,
      idempotencyKey: 'spoofed',
      identity: { ...agentRequest.identity, subjectId: 'other-user' },
    }),
  });
  const createScheduleResponse = await fetch(`http://${ready.host}:${ready.port}/v1/schedules`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({
      contractVersion: SCHEDULE_CONTRACT_VERSION,
      name: 'Runtime schedule',
      prompt: 'Run later',
      workspaceId: runtimeConfig.workingDirectory,
      timing: { kind: 'once', at: '2099-01-01T00:00:00.000Z' },
      timeZone: 'UTC',
      delivery: { kind: 'desktop' },
    }),
  });
  const createdSchedule = await createScheduleResponse.json();
  const unauthorizedSchedule = await fetch(`http://${ready.host}:${ready.port}/v1/schedules`, {
    method: 'POST',
    headers: { ...headers, 'content-type': 'application/json' },
    body: JSON.stringify({
      contractVersion: SCHEDULE_CONTRACT_VERSION,
      name: 'Unauthorized schedule',
      prompt: 'Must not run',
      workspaceId: join(home, 'other-workspace'),
      timing: { kind: 'cron', expression: '* * * * *' },
      timeZone: 'UTC',
      delivery: { kind: 'desktop' },
    }),
  });
  const listedSchedules = await fetch(`http://${ready.host}:${ready.port}/v1/schedules`, { headers })
    .then((response) => response.json());
  const disabledSchedule = await fetch(
    `http://${ready.host}:${ready.port}/v1/schedules/${createdSchedule.scheduleId}/disable`,
    { method: 'POST', headers },
  ).then((response) => response.json());
  const scheduleHistory = await fetch(
    `http://${ready.host}:${ready.port}/v1/schedules/${createdSchedule.scheduleId}/history`,
    { headers },
  ).then((response) => response.json());

  assert.equal(unauthorized.status, 401);
  assert.equal(badToken.status, 401);
  assert.equal(unauthorizedApprovals.status, 401);
  assert.deepEqual(approvals, []);
  assert.equal(invalidApprovalDecision.status, 400);
  assert.equal(forgedApprovalDecision.status, 403);
  assert.equal(signedUnknownDecision.status, 409);
  assert.equal(replayedSignedDecision.status, 403);
  assert.equal(child.spawnargs.includes(token), false);
  assert.equal(invalidChat.status, 400);
  assert.equal(invalidPluginInstall.status, 400);
  assert.equal(floatingPluginInstall.status, 500);
  assert.match(floatingPluginError.error, /精确版本/);
  assert.equal(artifactInstall.status, 500);
  assert.match(artifactInstallError.error, /超过 262144 字节限制/);
  assert.equal('hint' in floatingPluginError, false);
  assert.equal(enablePlugin.status, 410);
  assert.equal(pluginConfig.kind, 'mcp');
  assert.equal(pluginConfig.path, join(home, 'agent', 'mcp.json'));
  assert.equal(savePluginConfig.status, 200);
  assert.deepEqual(JSON.parse(await readFile(join(home, 'agent', 'mcp.json'), 'utf8')), {
    mcpServers: { docs: { url: 'https://example.test/mcp' } },
  });
  assert.equal(unconfiguredChat.status, 500);
  assert.match(unconfiguredError.hint, /auth\.json/);
  assert.equal(agentSubmissionResponse.status, 200);
  assert.equal(agentSubmission.accepted, true);
  assert.equal(duplicateAgentSubmission.runId, agentSubmission.runId);
  assert.equal(duplicateAgentSubmission.duplicate, true);
  assert.equal(agentConflict.status, 409);
  assert.equal(agentRun.status, 'failed');
  assert.match(agentRun.failure.message, /API key/);
  assert.equal(spoofedAgentSubmission.status, 403);
  assert.equal(createScheduleResponse.status, 201);
  assert.equal(unauthorizedSchedule.status, 400);
  assert.equal(createdSchedule.revision, 1);
  assert.equal(createdSchedule.nextTriggerAt, '2099-01-01T00:00:00.000Z');
  assert.equal(listedSchedules.length, 1);
  assert.equal(disabledSchedule.enabled, false);
  assert.equal(disabledSchedule.revision, 2);
  assert.deepEqual(scheduleHistory, []);
  assert.deepEqual(health, {
    version: runtimePackage.version,
    protocolVersion: 9,
    piVersion: '1.4.0',
    mcpTools: ['search_capabilities', 'execute_capability'],
    configRoot: home,
    workingDirectory: runtimeConfig.workingDirectory,
    notificationsEnabled: true,
  });
  assert.deepEqual(greeting, { message: 'Hello, Integration!' });
  assert.deepEqual(plugins, []);
  assert.equal(localSkills.diagnostics.length, 0);
  assert.equal(localSkills.skills.length, 1);
  assert.equal(localSkills.skills[0].name, 'runtime-fixture');
});
