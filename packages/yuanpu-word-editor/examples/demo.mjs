import { createServer } from 'node:http';
import { randomBytes } from 'node:crypto';
import { copyFile, mkdtemp, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { WordEditorHost } from '../dist/index.js';
import { engineConfig } from '../scripts/engine-config.mjs';

const { enginePath, engineSha256 } = await engineConfig();
const root = await mkdtemp(join(tmpdir(), 'yuanpu-word-demo-'));
await copyFile(new URL('../test/fixtures/sample.docx', import.meta.url), join(root, 'sample.docx'));
const host = new WordEditorHost({ enginePath, engineSha256, workspaceRoot: root });
let document = await host.open('sample.docx');
const token = randomBytes(32).toString('hex');
const html = await readFile(new URL('./demo.html', import.meta.url), 'utf8');
const renderer = await readFile(new URL('../dist/renderer.js', import.meta.url));
let origin;
const server = createServer(async (request, response) => {
  const json = (value, status = 200) => { response.writeHead(status, { 'Content-Type': 'application/json' }); response.end(JSON.stringify(value)); };
  try {
    if (request.headers.host !== new URL(origin).host) return json({ error: 'invalid host' }, 403);
    if (request.url === '/' && request.method === 'GET') {
      response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' });
      response.end(html.replace('__TOKEN__', token));
      return;
    }
    if (request.url === '/renderer.js' && request.method === 'GET') {
      response.writeHead(200, { 'Content-Type': 'text/javascript' }); response.end(renderer); return;
    }
    if (request.headers['x-editor-token'] !== token || (request.headers.origin && request.headers.origin !== origin)) {
      return json({ error: 'invalid demo session' }, 403);
    }
    if (request.url === '/api/document' && request.method === 'GET') return json(document);
    if (request.url === '/api/status' && request.method === 'GET') return json(await host.status(document.id));
    if (request.method !== 'POST') return json({ error: 'unknown route' }, 404);
    if (request.headers['content-type'] !== 'application/json') return json({ error: 'JSON required' }, 415);
    let raw = '';
    for await (const chunk of request) { raw += chunk; if (raw.length > 4096) return json({ error: 'request too large' }, 413); }
    const body = JSON.parse(raw);
    if (request.url === '/api/save') { await host.save(document.id); return json({ ok: true }); }
    if (request.url === '/api/save-as') { document = await host.saveAs(document.id, body.path); return json(document); }
    json({ error: 'unknown route' }, 404);
  } catch (error) { json({ error: error.message, code: error.code }, 400); }
});
server.listen(0, '127.0.0.1', () => {
  origin = `http://127.0.0.1:${server.address().port}`;
  console.log(JSON.stringify({ url: origin, workspace: root, editorOrigin: document.origin }));
});
let shuttingDown = false;
async function shutdown() {
  if (shuttingDown) return;
  shuttingDown = true;
  server.closeAllConnections();
  server.close();
  // Demo workspaces are retained so unsaved tests never affect a user's existing files.
  await host.dispose({ discardChanges: true });
  process.exit(0);
}
process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);
