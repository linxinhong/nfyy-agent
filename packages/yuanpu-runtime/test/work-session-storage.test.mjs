import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { createYuanpuChatSession } from '../dist/index.mjs';

test('Work adapter keeps its coding-agent SessionManager storage independent from Assistant durable storage', async () => {
  const root = await mkdtemp(join(tmpdir(), 'yp-chat-'));
  try {
    await writeFile(join(root, 'models.json'), JSON.stringify({ providers: { 'probe-fixture': {
      baseUrl: 'http://127.0.0.1:1/v1', api: 'openai-completions',
      models: [{ id: 'fixture', name: 'fixture', reasoning: false, input: ['text'], contextWindow: 8192, maxTokens: 1024 }],
    } } }));
    const chat = await createYuanpuChatSession({
      capabilityClient: { async search() { return { matches: [] }; }, async execute() { throw new Error('unused'); } },
      agentDir: root, modelConfigDir: root, cwd: root,
      provider: 'probe-fixture', model: 'fixture', apiKey: 'fixture-only',
      piSession: { id: 'legacy-chat', directory: join(root, 'legacy-sessions') },
    });
    assert.equal(chat.sessionId, 'legacy-chat');
    await chat.dispose();
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
