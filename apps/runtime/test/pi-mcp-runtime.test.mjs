import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { generateKeyPairSync, randomBytes } from 'node:crypto';
import { copyFile, mkdir, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { once } from 'node:events';
import test from 'node:test';
import { piMcpScenario } from '../../../packages/yuanpu-runtime/test/fixtures/pi-mcp-scenario.mjs';

test('standalone Runtime executes native Pi MCP through the authenticated Work API', { timeout: 30_000 }, async (t) => {
  await piMcpScenario(t, async (options) => {
    const home = join(options.agentDir, 'home');
    const app = join(home, 'app');
    const agent = join(home, 'agent');
    await Promise.all([mkdir(app, { recursive: true }), mkdir(agent, { recursive: true })]);
    await copyFile(join(options.agentDir, 'mcp.json'), join(agent, 'mcp.json'));
    await copyFile(join(options.agentDir, 'models.json'), join(app, 'models.json'));
    await writeFile(join(app, 'auth.json'), JSON.stringify({ fixture: { type: 'api_key', key: 'test-key' } }));
    await writeFile(join(app, 'config.json'), JSON.stringify({ schemaVersion: 1,
      provider: 'fixture', model: 'fixture', workingDirectory: options.cwd }));
    const token = randomBytes(32).toString('hex');
    const approvalPublicKey = generateKeyPairSync('ed25519').publicKey
      .export({ type: 'spki', format: 'der' }).toString('base64');
    const binary = process.env.YUANPU_TEST_RUNTIME_BINARY;
    const runtime = spawn(binary ?? process.execPath,
      [...(binary ? [] : [resolve(import.meta.dirname, '../dist/index.cjs')]), '--serve', '--port', '0'], {
        cwd: home, stdio: ['pipe', 'pipe', 'pipe'],
        env: { ...process.env, YUANPU_HOME: home,
          YUANPU_PYTHON_MCP_EXECUTABLE: '', YUANPU_PYTHON_MCP_ROOT: '',
          PI_CODING_AGENT_DIR: '', PI_CODING_AGENT_SESSION_DIR: '' },
      });
    let stderr = '';
    runtime.stderr.on('data', (chunk) => { stderr += chunk; });
    const close = async () => {
      if (runtime.exitCode !== null || runtime.signalCode !== null) return;
      const exited = once(runtime, 'exit');
      runtime.kill('SIGTERM');
      const force = setTimeout(() => runtime.kill('SIGKILL'), 5_000);
      try { await exited; } finally { clearTimeout(force); }
    };
    t.after(close);
    runtime.stdin.end(`${JSON.stringify({ token, approvalPublicKey, parentPid: process.pid })}\n`);
    const ready = await new Promise((resolveReady, reject) => {
      let stdout = '';
      const timer = setTimeout(() => reject(new Error(`Runtime readiness timed out: ${stderr}`)), 10_000);
      runtime.once('error', (error) => { clearTimeout(timer); reject(error); });
      runtime.once('exit', (code) => { clearTimeout(timer); reject(new Error(`Runtime exited ${code}: ${stderr}`)); });
      runtime.stdout.on('data', (chunk) => {
        stdout += chunk;
        if (!stdout.includes('\n')) return;
        clearTimeout(timer);
        try { resolveReady(JSON.parse(stdout.split('\n')[0])); } catch (error) { reject(error); }
      });
    });
    assert.equal(ready.event, 'ready', stderr);
    return {
      async prompt(message) {
        const response = await fetch(`http://${ready.host}:${ready.port}/v1/chat`, {
          method: 'POST', headers: { authorization: `Bearer ${token}`, 'content-type': 'application/json' },
          body: JSON.stringify({ message, surface: 'work' }), signal: AbortSignal.timeout(15_000),
        });
        const body = await response.json();
        assert.equal(response.status, 200, `${JSON.stringify(body)}\n${stderr}`);
        return body;
      },
      dispose: close,
    };
  });
});
