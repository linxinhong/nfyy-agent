#!/usr/bin/env node
// A transport/process fixture, not a document renderer. Native DOCX round trips use smoke-native.mjs.
import { createServer } from 'node:http';
import { readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

const args = process.argv.slice(2);
const port = Number(args[args.indexOf('--port') + 1]);
await writeFile(join(process.cwd(), 'pid'), String(process.pid));
const editors = new Map();
const content = (text) => ({ content: [{ type: 'text', text: JSON.stringify(text) }] });
const server = createServer(async (request, response) => {
  const json = (value, status = 200) => {
    response.writeHead(status, { 'Content-Type': 'application/json' });
    response.end(JSON.stringify(value));
  };
  try {
    if (request.url === '/localapi/editor/status') return json({ code: 0, open_editors: [...editors.values()] });
    let raw = '';
    for await (const chunk of request) raw += chunk;
    const body = JSON.parse(raw);
    if (request.url === '/localapi/open') {
      editors.set(body.file_id, { file_id: body.file_id, file_path: body.file_path, is_dirty: false,
        text: await readFile(body.file_path, 'utf8'), last_saved_ms: 0 });
      return json({ code: 0 });
    }
    if (request.url === '/mcp') {
      if (!body.id) { response.writeHead(204); response.end(); return; }
      let result;
      if (body.method === 'initialize') {
        response.setHeader('Mcp-Session-Id', 'fixture-session');
        result = { protocolVersion: '2024-11-05', capabilities: {} };
      } else {
        if (request.headers['mcp-session-id'] !== 'fixture-session') return json({ error: 'missing session' }, 400);
        const { name, arguments: arg } = body.params;
        if (name === 'open_file') {
          if (arg.file_path.endsWith('hang.docx')) return;
          if (!editors.has(arg.file_path)) editors.set(arg.file_path, {
            file_id: arg.file_path, file_path: arg.file_path, is_dirty: false,
            text: await readFile(arg.file_path, 'utf8'), last_saved_ms: 0,
          });
          result = content('opened');
        } else {
          const editor = editors.get(arg.file_id);
          if (!editor) throw new Error('unknown engine document');
          if (name === 'doc_find') {
            const start = editor.text.indexOf(arg.text);
            result = content({ locations: start < 0 ? [] : [{ begin: start, end: start + arg.text.length }] });
          } else if (name === 'doc_replace_text') {
            await new Promise((done) => setTimeout(done, 30));
            for (const range of [...arg.ranges].reverse()) {
              editor.text = editor.text.slice(0, range.begin) + arg.text + editor.text.slice(range.end);
            }
            editor.is_dirty = true;
            result = content('replaced');
          } else throw new Error('unsupported tool');
        }
      }
      response.writeHead(200, { 'Content-Type': 'text/event-stream' });
      response.end(`event: message\ndata: ${JSON.stringify({ jsonrpc: '2.0', id: body.id, result })}\n\n`);
      return;
    }
    const editor = editors.get(body.file_id);
    if (!editor) throw new Error('unknown document');
    if (request.url === '/localapi/save') {
      editor.file_path = body.file_path ?? editor.file_path;
      await writeFile(editor.file_path, editor.text);
      editor.is_dirty = false;
      editor.last_saved_ms = Date.now();
      return json({ code: 0, file_id: editor.file_id, file_path: editor.file_path });
    }
    if (request.url === '/localapi/editor/close') {
      if (editor.is_dirty && !body.force) return json({ code: -1 }, 409);
      editors.delete(body.file_id);
      return json({ code: 0 });
    }
    json({ error: 'unknown route' }, 404);
  } catch (error) { json({ error: error.message }, 500); }
});
server.listen(port, '127.0.0.1');
process.on('SIGTERM', () => { server.closeAllConnections(); server.close(() => process.exit(0)); });
