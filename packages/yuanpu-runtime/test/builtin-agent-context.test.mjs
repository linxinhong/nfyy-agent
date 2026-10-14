import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

import { createYuanpuChatSession } from '../dist/index.mjs';

const builtinAgentRoot = fileURLToPath(new URL('../../../apps/app/agents/', import.meta.url));

test('Work Pi session loads bundled AGENTS.md before workspace context and discovers bundled skills', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-builtin-agent-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const workspace = join(root, 'workspace');
  await mkdir(workspace);
  await writeFile(join(workspace, 'AGENTS.md'), '# Workspace instructions\n\nPROJECT_CONTEXT_MARKER\n');
  const workspaceSkill = join(workspace, '.pi', 'skills', 'low-ai-ui-design');
  await mkdir(workspaceSkill, { recursive: true });
  await writeFile(join(workspaceSkill, 'SKILL.md'), '---\nname: low-ai-ui-design\ndescription: Workspace UI fixture\n---\nWorkspace UI rules.\n');

  let systemPrompt = '';
  const requests = [];
  const server = createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    const payload = JSON.parse(body);
    requests.push(payload);
    const messages = payload.messages;
    systemPrompt = String(messages.find((message) => message.role === 'system')?.content ?? '');
    response.writeHead(200, { 'content-type': 'text/event-stream' });
    response.end('data: {"id":"fixture","object":"chat.completion.chunk","created":1,"model":"fixture","choices":[{"index":0,"delta":{"role":"assistant","content":"Ready."},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n');
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  context.after(() => new Promise((resolve) => server.close(resolve)));
  const baseUrl = `http://127.0.0.1:${server.address().port}/v1`;
  await writeFile(join(root, 'models.json'), JSON.stringify({ providers: { fixture: {
    baseUrl, api: 'openai-completions',
    models: [{ id: 'fixture', name: 'Fixture', reasoning: false, input: ['text'],
      contextWindow: 128000, maxTokens: 1024 }],
  } } }));

  const chat = await createYuanpuChatSession({
    capabilityClient: { async search() { return { matches: [] }; }, async execute() { throw new Error('not used'); } },
    agentDir: root, modelConfigDir: root, cwd: workspace, provider: 'fixture', model: 'fixture',
    apiKey: 'fixture-only', builtinAgentRoot,
    browserControlAvailable: true,
  });
  context.after(() => chat.dispose());
  await chat.prompt('Reply briefly.');

  assert.match(systemPrompt, /Yuanpu Agent conversation guide/);
  assert.match(systemPrompt, /html-preview/);
  assert.match(systemPrompt, /markdown-visuals/);
  assert.match(systemPrompt, /browser-control/);
  assert.match(systemPrompt, /<name>tailscale<\/name>/);
  assert.match(systemPrompt, /<name>low-ai-ui-design<\/name>/);
  assert.match(systemPrompt, /Save user-facing artifacts with relative paths under cwd, not \/tmp/);
  assert.match(systemPrompt, /Do not create a file, start a server, or navigate the browser merely to display a diagram/);
  assert.ok(systemPrompt.indexOf('Yuanpu Agent conversation guide') < systemPrompt.indexOf('PROJECT_CONTEXT_MARKER'));
  assert.ok(systemPrompt.indexOf('<name>markdown-visuals</name>') < systemPrompt.indexOf('<name>low-ai-ui-design</name>'));

  await chat.prompt('你生成一个架构图给我看下吧');
  const visualRequest = requests.at(-1);
  assert.match(JSON.stringify(visualRequest.messages.at(-1)), /yuanpu_visual_reply_contract/);
  assert.match(JSON.stringify(visualRequest.messages.at(-1)), /do not inspect the workspace or call tools/);
  assert.equal(visualRequest.tools?.length ?? 0, 0);

  await chat.prompt('请根据当前项目代码生成架构图');
  const projectRequest = requests.at(-1);
  assert.ok(projectRequest.tools.length > 0);
  assert.match(JSON.stringify(projectRequest.messages.at(-1)), /If the user names a specific project/);
});
