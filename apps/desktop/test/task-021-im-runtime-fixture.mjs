import { createServer } from 'node:http';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  ChannelRouter, PersistentAgentService, digestChannelValue, openYuanpuMetadataDatabase,
} from '../../../packages/yuanpu-runtime/dist/index.mjs';
import { getDesktopPrivateImRunSummary } from '../../runtime/src/runtime-host.ts';

// This is a test-only Runtime replacement. It never resolves Keychain or starts the WeCom SDK.
const home = process.env.YUANPU_HOME;
const metricsPath = join(home, 'fixture-metrics.json');
const portPath = join(home, 'fixture-port.json');
const incoming = {
  provider: 'wecom', connectionId: 'imc_task021', providerBotId: 'synthetic-bot',
  providerRequestId: 'synthetic-request', providerMessageId: 'synthetic-message',
  senderId: 'synthetic-member', conversationType: 'single', conversationId: 'synthetic-member',
  messageType: 'text', text: 'synthetic input',
};
let bootstrap = '';
for await (const chunk of process.stdin) bootstrap += chunk.toString();
const { token } = JSON.parse(bootstrap.trim());
const metadata = openYuanpuMetadataDatabase(join(home, 'workflows', 'automation.sqlite'));
let metrics;
try { metrics = JSON.parse(readFileSync(metricsPath, 'utf8')); }
catch { metrics = { executions: 0, sends: 0 }; }
const saveMetrics = () => writeFileSync(metricsPath, JSON.stringify(metrics));
const agent = await PersistentAgentService.open({
  store: metadata.agentRuns,
  executor: { async execute() {
    metrics.executions += 1;
    saveMetrics();
    return { kind: 'completed', output: { message: 'synthetic reply', tools: [] } };
  } },
});
let unblockReply;
const replyGate = new Promise((done) => { unblockReply = done; });
const transport = {
  connect() {}, async ready() {},
  async reply() {
    metrics.sends += 1;
    saveMetrics();
    await replyGate;
    return { status: 'accepted' };
  },
  close() { unblockReply(); },
};
const router = new ChannelRouter({
  config: {
    provider: 'wecom', connectionId: 'imc_task021', providerAccountRef: 'synthetic-bot',
    credentialBindingDigest: 'a'.repeat(64), workspaceId: join(home, 'workspace'),
    acceptedMessageTypes: ['text'], groupEnabled: false, groupAllowlistDigests: [],
    pairedSenderDigests: [digestChannelValue('imc_task021', 'sender', 'synthetic-member')],
  },
  store: metadata.channels, agent, transport,
});
router.start();
const info = {
  version: '0.1.0', protocolVersion: 9, piVersion: 'fixture', mcpTools: [],
  configRoot: home, workingDirectory: join(home, 'workspace'), notificationsEnabled: false,
};
const server = createServer(async (request, response) => {
  if (request.url === '/v1/health') {
    if (request.headers.authorization !== `Bearer ${token}`) { response.writeHead(401).end(); return; }
    response.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(info));
    return;
  }
  if (request.url === '/fixture/inbound' && request.method === 'POST') {
    const receipt = await router.handleInbound(incoming);
    response.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(receipt));
    return;
  }
  if (request.url?.startsWith('/v1/agent/runs/')) {
    if (request.headers.authorization !== `Bearer ${token}`) { response.writeHead(401).end(); return; }
    // Production's generic desktop/scheduler run query does not authorize IM runs.
    response.writeHead(404, { 'content-type': 'application/json' }).end('{"error":"not found"}');
    return;
  }
  if (request.url?.startsWith('/fixture/run/')) {
    const runId = decodeURIComponent(request.url.slice('/fixture/run/'.length));
    const run = metadata.agentRuns.get(runId);
    response.writeHead(run ? 200 : 404, { 'content-type': 'application/json' })
      .end(JSON.stringify(run ?? { error: 'not found' }));
    return;
  }
  if (request.url?.startsWith('/v1/im/private-runs/')) {
    if (request.headers.authorization !== `Bearer ${token}`) { response.writeHead(401).end(); return; }
    const runId = decodeURIComponent(request.url.slice('/v1/im/private-runs/'.length));
    const document = JSON.parse(readFileSync(join(home, 'app', 'connections', 'wecom.json'), 'utf8'));
    const summary = getDesktopPrivateImRunSummary(runId, metadata, document, join(home, 'workspace'));
    response.writeHead(summary ? 200 : 404, { 'content-type': 'application/json' })
      .end(JSON.stringify(summary ?? { error: 'not found' }));
    return;
  }
  if (request.url === '/v1/host/events') {
    response.writeHead(200, { 'content-type': 'text/event-stream' });
    request.on('close', () => response.end());
    return;
  }
  response.writeHead(404, { 'content-type': 'application/json' }).end('{}');
});
await new Promise((done) => server.listen(0, '127.0.0.1', done));
const port = server.address().port;
writeFileSync(portPath, JSON.stringify({ port }));
process.stdout.write(`${JSON.stringify({ event: 'ready', host: '127.0.0.1', port, ...info })}\n`);
process.on('SIGTERM', () => process.exit(0));
