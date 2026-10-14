import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';


export async function piMcpScenario(t, createSession) {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-pi-mcp-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const fixture = join(root, 'mcp.mjs');
  const previous = process.env.YUANPU_PI_MCP_TEST_VALUE;
  process.env.YUANPU_PI_MCP_TEST_VALUE = 'migrated-reference';
  t.after(() => {
    if (previous === undefined) delete process.env.YUANPU_PI_MCP_TEST_VALUE;
    else process.env.YUANPU_PI_MCP_TEST_VALUE = previous;
  });
  await writeFile(fixture, `import { createInterface } from 'node:readline';
for await (const line of createInterface({ input: process.stdin })) {
  const request = JSON.parse(line);
  if (request.id === undefined) continue;
  let result;
  if (request.method === 'initialize') result = { protocolVersion: request.params.protocolVersion,
    capabilities: { tools: {} }, serverInfo: { name: 'fixture', version: '1.0.0' } };
  else if (request.method === 'tools/list') result = { tools: [{ name: 'echo', description: 'Echo test text',
    inputSchema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] } }] };
  else if (request.method === 'tools/call') result = { content: [{ type: 'text',
    text: JSON.stringify({ text: request.params.arguments.text, pid: process.pid, marker: process.env.MCP_TEST_MARKER }) }] };
  else result = {};
  process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: request.id, result }) + '\\n');
}`);
  // No adapter installation and no explicit exposure: the host uses Pi's deferred discovery.
  await writeFile(join(root, 'mcp.json'), JSON.stringify({ mcpServers: {
    fixture: { command: process.execPath, args: [fixture], env: { MCP_TEST_MARKER: '$env:YUANPU_PI_MCP_TEST_VALUE' } },
  } }));
  const requests = [];
  let childPid;
  const model = createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    const input = JSON.parse(body);
    requests.push(input);
    const call = requests.length;
    const delta = call <= 2 ? { role: 'assistant', tool_calls: [{ index: 0, id: `call_${call}`,
      type: 'function', function: call === 1
        ? { name: 'tool_search', arguments: '{"query":"echo"}' }
        : { name: 'mcp__fixture__echo', arguments: '{"text":"native MCP works"}' } }] }
      : { role: 'assistant', content: 'MCP completed.' };
    if (call === 3) {
      const toolReply = input.messages.findLast((message) => message.role === 'tool');
      try { childPid = JSON.parse(toolReply.content).pid; } catch { /* Assertion below reports the complete response. */ }
    }
    response.writeHead(200, { 'content-type': 'text/event-stream' });
    response.end(`data: ${JSON.stringify({ id: 'fixture', object: 'chat.completion.chunk', created: 0,
      model: 'fixture', choices: [{ index: 0, delta, finish_reason: call <= 2 ? 'tool_calls' : 'stop' }],
      usage: { prompt_tokens: 10, completion_tokens: 4 } })}\n\ndata: [DONE]\n\n`);
  });
  await new Promise((resolve) => model.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise((resolve) => { model.closeAllConnections(); model.close(resolve); }));
  await writeFile(join(root, 'models.json'), JSON.stringify({ providers: { fixture: {
    baseUrl: `http://127.0.0.1:${model.address().port}/v1`, api: 'openai-completions',
    models: [{ id: 'fixture', name: 'Fixture', reasoning: false, input: ['text'], contextWindow: 128000, maxTokens: 1024 }],
  } } }));
  const cwd = join(root, 'workspace');
  await mkdir(cwd);
  const session = await createSession({ agentDir: root, modelConfigDir: root, cwd,
    provider: 'fixture', model: 'fixture', apiKey: 'test-key',
    capabilityClient: { async search() { throw new Error('Native MCP must not use the Yuanpu capability bridge'); },
      async execute() { throw new Error('Native MCP must not use the Yuanpu capability bridge'); } },
  });
  t.after(() => session.dispose());
  assert.equal((await session.prompt('Use the configured echo MCP service.')).message, 'MCP completed.');
  assert.ok(requests[0].tools.some((tool) => tool.function.name === 'tool_search'));
  assert.ok(!requests[0].tools.some((tool) => tool.function.name === 'mcp__fixture__echo'));
  assert.ok(requests[1].tools.some((tool) => tool.function.name === 'mcp__fixture__echo'));
  assert.ok(childPid > 0, JSON.stringify(requests.at(-1).messages));
  assert.match(JSON.stringify(requests.at(-1).messages), /migrated-reference/);
  await session.dispose();
  assert.throws(() => process.kill(childPid, 0), { code: 'ESRCH' });
}
