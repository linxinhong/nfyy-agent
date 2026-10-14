import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';

import { executableSuffix, targetTriple } from './lib.mjs';

const appRoot = resolve(import.meta.dirname, '../..');
const target = targetTriple();
const binary = resolve(
  appRoot,
  `dist-native/bin/YuanpuAgentRuntime-${target}${executableSuffix(target)}`,
);
const { version } = JSON.parse(await readFile(resolve(appRoot, 'package.json'), 'utf8'));

const greeting = execFileSync(binary, [], { encoding: 'utf8' }).trim();
const reportedVersion = execFileSync(binary, ['--version'], { encoding: 'utf8' }).trim();
await new Promise((resolveTest, reject) => {
  const check = spawn(process.execPath, ['--test', resolve(appRoot, 'test/pi-mcp-runtime.test.mjs')], {
    env: { ...process.env, YUANPU_TEST_RUNTIME_BINARY: binary }, stdio: 'inherit',
  });
  check.once('error', reject);
  check.once('exit', (code) => code === 0 ? resolveTest() : reject(new Error(`Native Pi MCP smoke failed: ${code}`)));
});
const agentToolsSmoke = JSON.parse(execFileSync(binary, ['--agent-tools-smoke'], { encoding: 'utf8', timeout: 15000 }).trim());
assert.deepEqual(agentToolsSmoke, { workflow: 'completed', result: ['native-worker-ok'], reader: true });
const pythonRoot = resolve(appRoot, '../python-capabilities/dist-artifact/bundle/YuanpuEchoMcp');
const pythonExecutable = join(
  pythonRoot,
  process.platform === 'win32' ? 'YuanpuEchoMcp.exe' : 'YuanpuEchoMcp',
);
const capabilitySmoke = JSON.parse(execFileSync(binary, ['--capability-smoke'], {
  encoding: 'utf8',
  env: {
    ...process.env,
    YUANPU_PYTHON_MCP_EXECUTABLE: pythonExecutable,
    YUANPU_PYTHON_MCP_ROOT: pythonRoot,
    YUANPU_PYTHON_MCP_ARGS: '[]',
    PATH: `${dirname(pythonExecutable)}${process.platform === 'win32' ? ';' : ':'}${process.env.PATH ?? ''}`,
  },
}).trim());
const sqliteRoot = await mkdtemp(join(tmpdir(), 'yuanpu-sea-sqlite-'));
let firstSqliteSmoke;
let secondSqliteSmoke;
let schedulerSmoke;
let assistantWorkerSmoke;
try {
  const sqlitePath = join(sqliteRoot, 'automation.sqlite');
  firstSqliteSmoke = JSON.parse(execFileSync(binary, ['--sqlite-smoke', sqlitePath], {
    encoding: 'utf8',
  }).trim());
  secondSqliteSmoke = JSON.parse(execFileSync(binary, ['--sqlite-smoke', sqlitePath], {
    encoding: 'utf8',
  }).trim());
  schedulerSmoke = JSON.parse(execFileSync(
    binary,
    ['--scheduler-smoke', join(sqliteRoot, 'scheduler.sqlite')],
    { encoding: 'utf8' },
  ).trim());
  const assistantHome = join(sqliteRoot, 'assistant');
  const worker = spawn(binary, ['--assistant-worker'], {
    stdio: ['ignore', 'pipe', 'pipe', 'ipc'],
    env: { ...process.env, PATH: '' },
  });
  let stderr = '';
  worker.stderr.setEncoding('utf8');
  worker.stderr.on('data', (chunk) => { stderr += chunk; });
  const receive = (predicate) => new Promise((resolveMessage, rejectMessage) => {
    const timeout = setTimeout(() => rejectMessage(new Error(`SEA Assistant Worker timed out: ${stderr}`)), 8_000);
    const onMessage = (message) => {
      if (!predicate(message)) return;
      clearTimeout(timeout);
      worker.off('message', onMessage);
      resolveMessage(message);
    };
    worker.on('message', onMessage);
    worker.once('exit', (code) => {
      clearTimeout(timeout);
      worker.off('message', onMessage);
      rejectMessage(new Error(`SEA Assistant Worker exited ${code}: ${stderr}`));
    });
  });
  try {
    const ready = receive((message) => message.kind === 'ready');
    worker.send({ kind: 'bootstrap', home: assistantHome, parentPid: process.pid });
    const started = await ready;
    const queried = receive((message) => message.kind === 'task' && message.correlationId === 'sea-query');
    worker.send({ kind: 'task', id: 'absent', correlationId: 'sea-query' });
    assistantWorkerSmoke = { readyPid: started.pid, missingTask: (await queried).record };
  } finally {
    if (worker.exitCode === null && worker.signalCode === null) {
      worker.send({ kind: 'shutdown' });
      await new Promise((resolveExit) => worker.once('exit', resolveExit));
    }
  }
} finally {
  await rm(sqliteRoot, { recursive: true, force: true });
}

assert.equal(greeting, 'Hello, world!');
assert.equal(reportedVersion, version);
assert.deepEqual(capabilitySmoke.tools, ['search_capabilities', 'execute_capability']);
assert.deepEqual(capabilitySmoke.result.structuredContent, {
  text: 'YuanpuAgent SEA',
  length: 15,
});
assert.equal(capabilitySmoke.errorResult.isError, true);
assert.match(capabilitySmoke.errorResult.content[0].text, /diagnostic error/i);
assert.deepEqual(firstSqliteSmoke, {
  driver: 'node:sqlite',
  schemaVersion: 16,
  persistedCount: 1,
});
assert.deepEqual(secondSqliteSmoke, {
  driver: 'node:sqlite',
  schemaVersion: 16,
  persistedCount: 2,
});
assert.deepEqual(schedulerSmoke, {
  schemaVersion: 16,
  historyCount: 1,
  runStatus: 'succeeded',
  output: 'SEA scheduler persisted output',
  deliveryStatus: 'delivered',
});
assert.ok(Number.isSafeInteger(assistantWorkerSmoke.readyPid));
assert.equal(assistantWorkerSmoke.missingTask, undefined);
console.log(`Smoke test passed for ${target}`);
