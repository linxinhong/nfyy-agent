import { appendFileSync, readFileSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';

const [mode = 'healthy', eventFile, counterFile] = process.argv.slice(2, 5);
let bootstrap = '';
for await (const chunk of process.stdin) {
  bootstrap += chunk.toString();
}
const credentials = JSON.parse(bootstrap);
const record = (event, extra = {}) => {
  if (eventFile) appendFileSync(eventFile, `${JSON.stringify({ event, pid: process.pid, ...extra })}\n`);
};

let startNumber = 1;
let crashScheduled = false;
if (counterFile) {
  try { startNumber = Number.parseInt(readFileSync(counterFile, 'utf8'), 10) + 1; } catch {}
  writeFileSync(counterFile, String(startNumber));
}
record('start', { startNumber, hasCenterKey: Boolean(credentials.centerConnection?.apiKey) });

const server = createServer(async (request, response) => {
  if (request.headers.authorization !== `Bearer ${credentials.token}`) {
    response.writeHead(401).end();
    return;
  }
  if (request.url === '/v1/health') {
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({
      version: '0.1.0',
      protocolVersion: mode === 'health-protocol-mismatch' ? 999 : 9,
      piVersion: 'fixture',
      mcpTools: [],
      configRoot: '/fixture',
      notificationsEnabled: true,
    }));
    if (!crashScheduled && ((mode === 'crash-once' && startNumber === 1) || mode === 'always-crash')) {
      crashScheduled = true;
      setTimeout(() => {
        record('crash', { startNumber });
        process.exit(23);
      }, 500);
    }
    return;
  }
  if (request.url === '/v1/center/connection' && request.method === 'POST') {
    let body = '';
    for await (const chunk of request) body += chunk;
    const connection = JSON.parse(body);
    record('center-connection', { endpoint: connection.endpoint, hasKey: Boolean(connection.apiKey) });
    response.writeHead(204).end();
    return;
  }
  if (request.url === '/v1/agent/runs/run-fixture') {
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({
      runId: 'run-fixture',
      owner: {
        entryPoint: 'desktop',
        identity: {
          kind: 'local_user', subjectId: 'local-user', authorityId: 'local-desktop', authenticatedBy: 'electron',
        },
      },
      context: {
        workspaceId: '/fixture',
        conversation: { namespace: 'desktop', conversationId: 'default' },
        delivery: { kind: 'desktop' },
      },
      requestFingerprint: 'fixture',
      inputDigest: 'fixture',
      status: 'succeeded',
      externalEffectState: 'possible',
      createdAt: '2026-09-22T00:00:00.000Z',
      updatedAt: '2026-09-22T00:00:01.000Z',
    }));
    return;
  }
  if (mode === 'management') {
    const reply = (status, body) => {
      response.writeHead(status, { 'content-type': 'application/json' });
      response.end(JSON.stringify(body));
    };
    if (request.url === '/v1/work/search?query=needle&archive=all&limit=5' && request.method === 'GET') {
      reply(200, { items: [{ kind: 'message', conversationId: 'work:fixture', messageEntryId: 'entry-1' }], contentFailures: [] });
      return;
    }
    if (request.url === '/v1/work/messages/window?conversationId=work%3Afixture&entryId=entry-1&radius=3' && request.method === 'GET') {
      reply(200, { status: 'ok', messages: [{ id: 'entry-1', role: 'user', text: 'needle', at: '2026-09-27T00:00:00Z' }],
        targetIndex: 0, hasBefore: false, hasAfter: false });
      return;
    }
    if (request.url === '/v1/schedules' && request.method === 'GET') {
      reply(200, [{ scheduleId: 'schedule-fixture', enabled: true }]);
      return;
    }
    if (request.url === '/v1/schedules/preview' && request.method === 'POST') {
      reply(200, { nextTriggerAt: '2026-09-23T01:00:00.000Z' });
      return;
    }
    if (request.url === '/v1/connections/wecom' && request.method === 'GET') {
      reply(200, { status: 'ok', connections: [{ connectionId: 'test', enabled: true, status: 'connected' }] });
      return;
    }
    if (request.url === '/v1/connections/wecom' && request.method === 'POST') {
      reply(200, { connectionId: 'test', enabled: false, status: 'disabled', pairedSenderCount: 0, groupEnabled: false });
      return;
    }
    if (request.url === '/v1/chat/submit' && request.method === 'POST') {
      reply(202, { accepted: true, runId: 'run-fixture', status: 'queued', duplicate: false });
      return;
    }
    if (request.url === '/v1/connections/wecom/test/test' && request.method === 'POST') {
      reply(200, { connectionId: 'test', enabled: true, status: 'connected' });
      return;
    }
    if (request.url === '/v1/channels/schedule-targets' && request.method === 'GET') {
      reply(200, [{ contactId: 'contact-fixture', connectionId: 'test', lastSeenAt: '2026-09-23T00:00:00Z' }]);
      return;
    }
    if (request.url === '/v1/schedules/schedule-fixture/history?limit=3' && request.method === 'GET') {
      reply(200, [{ triggerKey: 'trigger-fixture', runId: 'run-fixture' }]);
      return;
    }
    if (request.url === '/v1/agent/runs/run-fixture/cancel' && request.method === 'POST') {
      reply(200, { runId: 'run-fixture', result: 'already_terminal', status: 'succeeded' });
      return;
    }
    if (request.url === '/v1/schedules/schedule-fixture/disable' && request.method === 'POST') {
      reply(200, { scheduleId: 'schedule-fixture', enabled: false });
      return;
    }
    if (request.url === '/v1/channels/schedule-targets/route-fixture' && request.method === 'DELETE') {
      response.writeHead(204).end();
      return;
    }
  }
  response.writeHead(404).end();
});

server.listen(0, '127.0.0.1', () => {
  const address = server.address();
  const protocolVersion = mode === 'ready-protocol-mismatch' ? 999 : 9;
  process.stdout.write(`${JSON.stringify({
    event: 'ready',
    host: '127.0.0.1',
    port: address.port,
    version: '0.1.0',
    protocolVersion,
    piVersion: 'fixture',
    mcpTools: [],
    configRoot: '/fixture',
    notificationsEnabled: true,
  })}\n`);
});

if (mode !== 'ignore-term') {
  process.once('SIGTERM', () => {
    record('term');
    server.close(() => process.exit(0));
  });
}
