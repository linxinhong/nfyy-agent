import { spawn, type ChildProcess } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { access, lstat, mkdir, mkdtemp, realpath, rm, stat } from 'node:fs/promises';
import { constants } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { basename, dirname, extname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

import { WordEditorError, type WordEditorDocument, type WordEditorMode,
  type WordEditorStatus, type WordTextLocation } from './types.js';

const WORD_EXTENSIONS = new Set(['.doc', '.docx', '.dot', '.dotx', '.docm', '.dotm', '.wps', '.wpt']);
type JsonObject = Record<string, unknown>;
interface EngineEditor { file_id: string; file_path: string; is_dirty: boolean; last_saved_ms?: number }
interface DocumentSession { document: WordEditorDocument; filePath: string; fileId: string }
export interface WordEditorOpenOptions { mode?: WordEditorMode; load?: 'frame' | 'model' }

export interface WordEditorHostOptions {
  /** External, caller-provided engine. The package contains no vendor binaries. */
  enginePath: string;
  /** Expected digest pinned by the host; never inferred or silently updated by this library. */
  engineSha256: string;
  workspaceRoot: string;
  startupTimeoutMs?: number;
  requestTimeoutMs?: number;
  shutdownTimeoutMs?: number;
  maxFileBytes?: number;
}

function object(value: unknown): JsonObject {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new WordEditorError('invalid_response', 'The editor engine returned an invalid object.');
  }
  return value as JsonObject;
}

function within(root: string, candidate: string): boolean {
  const rel = relative(root, candidate);
  return rel === '' || (!isAbsolute(rel) && rel !== '..' && !rel.startsWith(`..${sep}`));
}

function checkedTimeout(value: number | undefined, fallback: number): number {
  if (value === undefined) return fallback;
  if (!Number.isSafeInteger(value) || value < 1 || value > 300_000) {
    throw new WordEditorError('invalid_options', 'Timeouts must be integers between 1 and 300000 ms.');
  }
  return value;
}

async function sha256(filePath: string): Promise<string> {
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(filePath)) hash.update(chunk);
  return hash.digest('hex');
}

async function freePort(): Promise<number> {
  const server = createServer();
  await new Promise<void>((done, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', done);
  });
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Could not reserve a loopback port.');
  const port = address.port;
  await new Promise<void>((done, reject) => server.close((error) => error ? reject(error) : done()));
  return port;
}

/** Owns one external engine and authorizes document operations under one workspace. */
export class WordEditorHost {
  private readonly startupTimeout: number;
  private readonly requestTimeout: number;
  private readonly shutdownTimeout: number;
  private readonly maxFileBytes: number;
  private rootPromise?: Promise<string>;
  private child?: ChildProcess;
  private engineDirectory?: string;
  private origin?: string;
  private starting?: Promise<void>;
  private stopping?: Promise<void>;
  private disposed = false;
  private closing = false;
  private operations: Promise<unknown> = Promise.resolve();
  private rpcId = 0;
  private mcpSession?: string;
  private readonly documents = new Map<string, DocumentSession>();
  private readonly requests = new Set<AbortController>();

  constructor(private readonly options: WordEditorHostOptions) {
    if (!/^[a-f0-9]{64}$/i.test(options.engineSha256) || !isAbsolute(options.enginePath)
      || !isAbsolute(options.workspaceRoot)) {
      throw new WordEditorError('invalid_options', 'An absolute engine path, workspace root and pinned SHA-256 are required.');
    }
    this.startupTimeout = checkedTimeout(options.startupTimeoutMs, 15_000);
    this.requestTimeout = checkedTimeout(options.requestTimeoutMs, 30_000);
    this.shutdownTimeout = checkedTimeout(options.shutdownTimeoutMs, 2_000);
    this.maxFileBytes = options.maxFileBytes ?? 64 * 1024 * 1024;
    if (!Number.isSafeInteger(this.maxFileBytes) || this.maxFileBytes < 1) {
      throw new WordEditorError('invalid_options', 'maxFileBytes must be a positive integer.');
    }
  }

  private assertActive(): void {
    if (this.disposed) throw new WordEditorError('disposed', 'The editor host is disposed.');
  }

  // Serialize host operations so a save/close cannot overtake a pending replacement.
  private run<T>(operation: () => Promise<T>): Promise<T> {
    if (this.closing || this.disposed) return Promise.reject(new WordEditorError('disposed', 'The editor host is shutting down or disposed.'));
    const result = this.operations.then(operation);
    this.operations = result.catch(() => {});
    return result;
  }

  private root(): Promise<string> {
    return this.rootPromise ??= realpath(this.options.workspaceRoot);
  }

  private async documentPath(filePath: string, destination = false): Promise<string> {
    this.assertActive();
    if (!filePath || isAbsolute(filePath) || filePath.includes('\0') || filePath.includes('\\')) {
      throw new WordEditorError('path_outside_workspace', 'Document paths must be workspace-relative paths using forward slashes.');
    }
    if (!WORD_EXTENSIONS.has(extname(filePath).toLowerCase())) {
      throw new WordEditorError('unsupported_format', 'The Word editor requires a supported Word document extension.');
    }
    const root = await this.root();
    const candidate = resolve(root, filePath);
    if (!within(root, candidate)) throw new WordEditorError('path_outside_workspace', 'The document path escapes the workspace.');
    let canonical: string;
    if (destination) {
      const parent = await realpath(dirname(candidate));
      canonical = join(parent, basename(candidate));
      // Save As always creates a new file; an existing file or dangling symlink is never replaced.
      try {
        await lstat(canonical);
        throw new WordEditorError('destination_exists', 'Save As requires a new destination file.');
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
      }
    } else {
      canonical = await realpath(candidate);
      const metadata = await stat(canonical);
      if (!metadata.isFile()) throw new WordEditorError('invalid_document', 'The document is not a regular file.');
      if (metadata.size > this.maxFileBytes) throw new WordEditorError('file_too_large', 'The document exceeds the configured editor size limit.');
    }
    if (!within(root, canonical)) throw new WordEditorError('path_outside_workspace', 'The document resolves outside the workspace.');
    return canonical;
  }

  private async start(): Promise<void> {
    this.assertActive();
    if (this.origin && this.child?.exitCode === null && this.child.signalCode === null) return;
    if (this.starting) return this.starting;
    const starting = this.startEngine();
    this.starting = starting;
    try { await starting; } finally { if (this.starting === starting) this.starting = undefined; }
  }

  private async startEngine(): Promise<void> {
    const enginePath = await realpath(this.options.enginePath);
    await access(enginePath, constants.X_OK);
    if ((await sha256(enginePath)) !== this.options.engineSha256.toLowerCase()) {
      throw new WordEditorError('engine_integrity', 'The editor engine SHA-256 does not match the configured digest.');
    }
    this.assertActive();
    const directory = await mkdtemp(join(tmpdir(), 'yuanpu-word-editor-'));
    this.engineDirectory = directory;
    try {
      await mkdir(join(directory, 'logs'));
      await mkdir(join(directory, 'tmp'));
      const port = await freePort();
      this.assertActive();
      const child = spawn(enginePath, ['--port', String(port), '--log_dir', join(directory, 'logs'),
        '--tmp_dir', join(directory, 'tmp')], { cwd: directory, stdio: ['ignore', 'ignore', 'ignore'], windowsHide: true });
      this.child = child;
      let launchError: Error | undefined;
      child.on('error', (error) => { launchError = error; });
      child.once('exit', () => {
        if (this.child === child) {
          this.origin = undefined;
          this.mcpSession = undefined;
          this.documents.clear();
        }
      });
      const origin = `http://127.0.0.1:${port}`;
      const deadline = Date.now() + this.startupTimeout;
      while (Date.now() < deadline) {
        this.assertActive();
        if (launchError || child.exitCode !== null || child.signalCode !== null) {
          throw new WordEditorError('engine_start_failed', launchError?.message ?? 'The editor engine exited before becoming ready.');
        }
        try {
          const response = await fetch(`${origin}/localapi/editor/status`, { signal: AbortSignal.timeout(500), redirect: 'error' });
          if (response.ok && object(await response.json()).code === 0) {
            if (child.exitCode !== null || child.signalCode !== null) continue;
            this.origin = origin;
            await this.rpc('initialize', { protocolVersion: '2024-11-05', capabilities: {},
              clientInfo: { name: '@yuanpu-agent/word-editor', version: '0.1.0' } });
            await this.notification('notifications/initialized');
            return;
          }
        } catch (error) {
          if (error instanceof WordEditorError || this.disposed) throw error;
        }
        await delay(50);
      }
      throw new WordEditorError('engine_start_timeout', 'The editor engine did not become ready before the startup deadline.');
    } catch (error) {
      await this.stopEngine();
      throw error;
    }
  }

  private async request(endpoint: string, init: RequestInit = {}): Promise<Response> {
    this.assertActive();
    if (!this.origin) throw new WordEditorError('engine_unavailable', 'The editor engine is not running.');
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.requestTimeout);
    this.requests.add(controller);
    try {
      const response = await fetch(`${this.origin}${endpoint}`, { ...init, signal: controller.signal, redirect: 'error' });
      // Read the response under the same deadline; a stalled SSE/body must also be cancellable.
      const text = await response.text();
      if (!response.ok) {
        throw new WordEditorError(response.status === 409 ? 'unsaved_changes' : 'engine_http_error',
          `Editor request failed with HTTP ${response.status}: ${text.slice(0, 300)}`);
      }
      return new Response([204, 205].includes(response.status) ? null : text, { status: response.status, headers: response.headers });
    } catch (error) {
      if (controller.signal.aborted) throw new WordEditorError('request_cancelled', 'The editor request was cancelled or timed out.');
      throw error;
    } finally {
      clearTimeout(timer);
      this.requests.delete(controller);
    }
  }

  private mcpHeaders(): Record<string, string> {
    return { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream',
      ...(this.mcpSession ? { 'Mcp-Session-Id': this.mcpSession } : {}) };
  }

  private async rpc(method: string, params: JsonObject): Promise<JsonObject> {
    const id = ++this.rpcId;
    const response = await this.request('/mcp', { method: 'POST', headers: this.mcpHeaders(),
      body: JSON.stringify({ jsonrpc: '2.0', id, method, params }) });
    const session = response.headers.get('mcp-session-id');
    if (session) this.mcpSession = session;
    const text = await response.text();
    const values: unknown[] = response.headers.get('content-type')?.includes('text/event-stream')
      ? text.replace(/\r\n/g, '\n').split('\n\n').flatMap((event) => {
          const data = event.split('\n').filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trimStart()).join('\n');
          return data ? [JSON.parse(data)] : [];
        })
      : [JSON.parse(text)];
    const result = values.map(object).find((value) => value.id === id);
    if (!result) throw new WordEditorError('invalid_response', 'The editor returned no matching RPC response.');
    if (result.error) throw new WordEditorError('engine_rpc_error', JSON.stringify(result.error));
    return object(result.result);
  }

  private async notification(method: string): Promise<void> {
    await this.request('/mcp', { method: 'POST', headers: this.mcpHeaders(), body: JSON.stringify({ jsonrpc: '2.0', method }) });
  }

  private async tool(name: string, args: JsonObject): Promise<JsonObject> {
    const result = await this.rpc('tools/call', { name, arguments: args });
    if (result.isError === true) throw new WordEditorError('engine_tool_error', JSON.stringify(result.content));
    return result;
  }

  private session(id: string, writable = false): DocumentSession {
    this.assertActive();
    const session = this.documents.get(id);
    if (!session) throw new WordEditorError('unknown_document', 'The document is closed or belongs to another editor host.');
    if (writable && session.document.mode !== 'edit') throw new WordEditorError('readonly_document', 'This document was opened read-only.');
    return session;
  }

  private async editors(): Promise<EngineEditor[]> {
    const response = await this.request('/localapi/editor/status');
    const body = object(await response.json());
    if (body.code !== 0 || !Array.isArray(body.open_editors)) throw new WordEditorError('invalid_response', 'The editor pool status is invalid.');
    return body.open_editors.map((entry) => {
      const value = object(entry);
      if (typeof value.file_id !== 'string' || typeof value.file_path !== 'string' || typeof value.is_dirty !== 'boolean') {
        throw new WordEditorError('invalid_response', 'The editor pool entry is invalid.');
      }
      return value as unknown as EngineEditor;
    });
  }

  open(filePath: string, options: WordEditorOpenOptions = {}): Promise<WordEditorDocument> {
    return this.run(() => this.openScoped(filePath, options));
  }

  private async openScoped(filePath: string, options: WordEditorOpenOptions): Promise<WordEditorDocument> {
    const canonical = await this.documentPath(filePath);
    const mode = options.mode ?? 'edit';
    const load = options.load ?? 'frame';
    if (mode !== 'edit' && mode !== 'readonly') throw new WordEditorError('invalid_options', 'Unknown editor mode.');
    if (load !== 'frame' && load !== 'model') throw new WordEditorError('invalid_options', 'Unknown document loading mode.');
    const existing = [...this.documents.values()].find((item) => item.filePath === canonical);
    if (existing) {
      if (existing.document.mode !== mode || existing.document.load !== load) {
        throw new WordEditorError('document_already_open', 'Close the existing editor before changing its mode or loading method.');
      }
      return { ...existing.document };
    }
    if ([...this.documents.values()].some((item) => item.fileId === canonical
      || new URL(item.document.url).searchParams.get('localFilePath') === canonical)) {
      throw new WordEditorError('document_already_open', 'Close the Save As session before reopening its original file.');
    }
    return this.openDocument(canonical, mode, load);
  }

  private async openDocument(filePath: string, mode: WordEditorMode, load: 'frame' | 'model'): Promise<WordEditorDocument> {
    await this.start();
    let fileId = filePath;
    if (load === 'model') {
      await this.tool('open_file', { file_path: filePath, file_type: 'doc', wait: true });
      const editor = (await this.editors()).find((item) => item.file_path === filePath);
      if (!editor) throw new WordEditorError('document_not_ready', 'The document did not appear in the editor pool.');
      fileId = editor.file_id;
    }
    const root = await this.root();
    const origin = this.origin!;
    const url = new URL('/static/doc/pc.html', origin);
    url.search = new URLSearchParams({ title: basename(filePath), localFilePath: filePath,
      globalPadId: createHash('md5').update(filePath).digest('hex'), local_edit: '1', client: 'sdk_local',
      mode, toolbar: mode === 'edit' ? 'show' : 'hide', outline: 'show',
      statusbar: mode === 'edit' ? 'show' : 'hide', editorSdkUrl: origin }).toString();
    const document: WordEditorDocument = { id: randomUUID(), path: relative(root, filePath).split(sep).join('/'),
      title: basename(filePath), mode, load, origin, url: url.toString() };
    this.documents.set(document.id, { document, filePath, fileId });
    return { ...document };
  }

  status(id: string): Promise<WordEditorStatus> {
    return this.run(() => this.statusScoped(id));
  }

  private async statusScoped(id: string): Promise<WordEditorStatus> {
    const session = this.session(id);
    const editor = await this.resolveEditor(session);
    return { dirty: editor.is_dirty, ...(typeof editor.last_saved_ms === 'number' ? { lastSavedAt: editor.last_saved_ms } : {}) };
  }

  private async resolveEditor(session: DocumentSession): Promise<EngineEditor> {
    const matching = (await this.editors()).filter((item) => item.file_path === session.filePath);
    if (matching.length !== 1) {
      throw new WordEditorError(matching.length ? 'document_conflict' : 'document_not_ready',
        'The document must have exactly one ready editor in the owned engine.');
    }
    const editor = matching[0]!;
    // The UI creates its own file_id; adopt only a pool entry bound to our authorized canonical path.
    session.fileId = editor.file_id;
    return editor;
  }

  findText(id: string, text: string): Promise<WordTextLocation[]> {
    return this.run(() => this.findTextScoped(id, text));
  }

  private async findTextScoped(id: string, text: string): Promise<WordTextLocation[]> {
    const session = this.session(id);
    if (!text) throw new WordEditorError('invalid_search', 'The search text must not be empty.');
    await this.resolveEditor(session);
    const result = await this.tool('doc_find', { file_id: session.fileId, text });
    const content = Array.isArray(result.content) ? result.content.map(object) : [];
    const item = content.find((entry) => entry.type === 'text' && typeof entry.text === 'string');
    const data = object(JSON.parse(String(item?.text)));
    if (!Array.isArray(data.locations)) throw new WordEditorError('invalid_response', 'The editor text search response is invalid.');
    return data.locations.map((location) => {
      const value = object(location);
      if (!Number.isSafeInteger(value.begin) || !Number.isSafeInteger(value.end)
        || Number(value.begin) < 0 || Number(value.end) <= Number(value.begin)) {
        throw new WordEditorError('invalid_response', 'The editor returned an invalid document coordinate.');
      }
      return { begin: Number(value.begin), end: Number(value.end) };
    });
  }

  /** Re-query real engine coordinates before each replacement; no synthetic character offsets. */
  replaceText(id: string, from: string, to: string): Promise<number> {
    return this.run(() => this.replaceTextScoped(id, from, to));
  }

  private async replaceTextScoped(id: string, from: string, to: string): Promise<number> {
    const session = this.session(id, true);
    if (!from) throw new WordEditorError('invalid_search', 'The search text must not be empty.');
    const ranges = await this.findTextScoped(id, from);
    if (ranges.length) await this.tool('doc_replace_text', { file_id: session.fileId, ranges, text: to });
    return ranges.length;
  }

  private async localAction(endpoint: string, body: JsonObject): Promise<JsonObject> {
    const response = await this.request(endpoint, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    const result = object(await response.json());
    if (result.code !== 0) throw new WordEditorError('engine_action_failed', String(result.message ?? 'The editor action failed.'));
    return result;
  }

  save(id: string): Promise<void> {
    return this.run(() => this.saveScoped(id));
  }

  private async saveScoped(id: string): Promise<void> {
    const session = this.session(id, true);
    const canonical = await this.documentPath(session.document.path);
    if (canonical !== session.filePath) throw new WordEditorError('document_path_changed', 'The document path changed since it was opened.');
    await this.resolveEditor(session);
    await this.localAction('/localapi/save', { file_id: session.fileId, file_path: session.filePath });
  }

  saveAs(id: string, destination: string): Promise<WordEditorDocument> {
    return this.run(() => this.saveAsScoped(id, destination));
  }

  private async saveAsScoped(id: string, destination: string): Promise<WordEditorDocument> {
    const session = this.session(id, true);
    const filePath = await this.documentPath(destination, true);
    await this.resolveEditor(session);
    await this.localAction('/localapi/save', { file_id: session.fileId, file_path: filePath });
    // The observed engine retains file_id while changing file_path and future save targets.
    const editor = (await this.editors()).find((item) => item.file_id === session.fileId);
    if (!editor || editor.file_path !== filePath) {
      throw new WordEditorError('invalid_response', 'Save As did not switch the editor to the requested destination.');
    }
    session.filePath = filePath;
    const url = new URL(session.document.url);
    url.searchParams.set('title', basename(filePath));
    session.document = { ...session.document, path: relative(await this.root(), filePath).split(sep).join('/'),
      title: basename(filePath), url: url.toString() };
    return { ...session.document };
  }

  close(id: string, options: { discardChanges?: boolean } = {}): Promise<void> {
    return this.run(() => this.closeScoped(id, options));
  }

  private async closeScoped(id: string, options: { discardChanges?: boolean }): Promise<void> {
    const session = this.session(id);
    let editor: EngineEditor;
    try { editor = await this.resolveEditor(session); } catch (error) {
      if (error instanceof WordEditorError && error.code === 'document_not_ready') { this.documents.delete(id); return; }
      throw error;
    }
    if (!options.discardChanges && editor.is_dirty) {
      throw new WordEditorError('unsaved_changes', 'Save or explicitly discard the document changes before closing.');
    }
    await this.localAction('/localapi/editor/close', { file_id: session.fileId, ...(options.discardChanges ? { force: true } : {}) });
    this.documents.delete(id);
  }

  async dispose(options: { discardChanges?: boolean } = {}): Promise<void> {
    if (this.stopping) return this.stopping;
    if (this.disposed) return;
    this.closing = true;
    const stopping = this.operations.then(async () => {
      if (!options.discardChanges && this.origin && (await this.editors()).some((editor) => editor.is_dirty)) {
        throw new WordEditorError('unsaved_changes', 'Save or explicitly discard document changes before disposing the host.');
      }
      this.disposed = true;
      for (const request of this.requests) request.abort();
      await this.stopEngine();
    });
    this.stopping = stopping;
    try { await stopping; } finally { this.stopping = undefined; this.closing = false; }
  }

  private async stopEngine(): Promise<void> {
    const child = this.child;
    this.child = undefined;
    this.origin = undefined;
    this.mcpSession = undefined;
    this.documents.clear();
    if (child && child.exitCode === null && child.signalCode === null && child.pid) {
      await new Promise<void>((done) => {
        const timer = setTimeout(() => child.kill('SIGKILL'), this.shutdownTimeout);
        child.once('exit', () => { clearTimeout(timer); done(); });
        child.kill('SIGTERM');
      });
    }
    const directory = this.engineDirectory;
    this.engineDirectory = undefined;
    if (directory) await rm(directory, { recursive: true, force: true });
  }
}
