import { createHash, randomUUID } from 'node:crypto';
import { lstat, mkdir, readdir, rename, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { createRegistry, Harness, type EntryDraft } from '@earendil-works/pi-durable';
import { openNodeJsonlStorage } from '@earendil-works/pi-durable/storage/jsonl/node';
import type { Message, Models } from '@earendil-works/pi-ai';
import { assertSafeDirectory, readAssistantHomeFile, type AssistantHomePaths } from './home.js';

type RecordValue = Record<string, unknown>;
function record(value: unknown): value is RecordValue {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

async function exists(path: string): Promise<boolean> {
  try {
    const info = await lstat(path);
    if (info.isSymbolicLink()) throw new Error(`Assistant session symlink is forbidden: ${path}`);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return false;
    throw error;
  }
}

/** Refuse nested links before Pi opens its own sidecar files. */
async function checkStore(path: string): Promise<void> {
  await assertSafeDirectory(path);
  for (const entry of await readdir(path, { withFileTypes: true })) {
    if (entry.isSymbolicLink()) throw new Error('Assistant session symlink is forbidden.');
    if (entry.isDirectory()) await checkStore(join(path, entry.name));
    else if (!entry.isFile()) throw new Error('Unsupported assistant session file.');
  }
}

/** Read the old committed main branch only. Never replay old operations or invoke tools. */
function legacyEntries(content: string, sessionId: string): EntryDraft[] {
  // Like the old store, ignore an uncommitted torn final line; retain the original file untouched.
  const lines = content.slice(0, content.lastIndexOf('\n')).split('\n');
  const header: unknown = JSON.parse(lines.shift() ?? 'null');
  if (!record(header) || header.kind !== 'header' || header.v !== 4
    || header.storageVersion !== 1 || header.id !== sessionId) {
    throw new Error('Unsupported legacy Assistant session format.');
  }
  const entries = new Map<string, RecordValue>();
  let tip: string | null | undefined;
  let sequence = 0;
  for (const line of lines) {
    const parsed: unknown = JSON.parse(line);
    for (const write of Array.isArray(parsed) ? parsed : [parsed]) {
      if (!record(write) || typeof write.seq !== 'number' || !Number.isSafeInteger(write.seq)
        || write.seq <= sequence) throw new Error('Invalid legacy Assistant transaction sequence.');
      sequence = write.seq;
      if (write.kind === 'entry') {
        if (typeof write.id !== 'string' || entries.has(write.id)
          || !(write.parentId === null || typeof write.parentId === 'string')) {
          throw new Error('Invalid legacy Assistant entry.');
        }
        entries.set(write.id, write);
      } else if (!['value', 'list', 'usage'].includes(String(write.kind))) {
        throw new Error('Invalid legacy Assistant transaction.');
      }
      if (write.kind === 'value' && write.namespace === 'pi.branch.tip' && write.key === 'main') {
        if (write.op !== 'set' || !(write.value === null || typeof write.value === 'string')) {
          throw new Error('Invalid legacy Assistant branch.');
        }
        tip = write.value;
      }
    }
  }
  if (tip === undefined) throw new Error('Legacy Assistant main branch is missing.');
  const branch: RecordValue[] = [];
  const visited = new Set<string>();
  while (tip !== null) {
    const entry = entries.get(tip);
    if (!entry || visited.has(tip)) throw new Error('Invalid legacy Assistant branch ancestry.');
    visited.add(tip);
    branch.unshift(entry);
    tip = entry.parentId as string | null;
  }
  const result: EntryDraft[] = [];
  const pendingCalls = new Map<string, string>();
  const appendMessage = (value: unknown) => {
    if (!record(value) || !['user', 'assistant', 'toolResult'].includes(String(value.role))
      || !(typeof value.content === 'string' || Array.isArray(value.content))) {
      throw new Error('Unsupported legacy Assistant message.');
    }
    if (value.role === 'assistant' && Array.isArray(value.content)) {
      for (const block of value.content) {
        if (record(block) && block.type === 'toolCall' && typeof block.id === 'string'
          && typeof block.name === 'string') pendingCalls.set(block.id, block.name);
      }
    } else if (value.role === 'toolResult' && typeof value.toolCallId === 'string') {
      pendingCalls.delete(value.toolCallId);
    }
    result.push({ kind: 'yuanpu.legacy.message', model: [value as unknown as Message] });
  };
  for (const entry of branch) {
    if (entry.type === 'message') appendMessage(entry.message);
    else if (entry.type === 'compaction' && typeof entry.summary === 'string'
      && Array.isArray(entry.retainedTail)) {
      result.push({ kind: 'pi.reset', head: 'self', model: [{ role: 'user',
        content: `Summary of the earlier conversation:\n${entry.summary}`, timestamp: 0 }] });
      pendingCalls.clear();
      entry.retainedTail.forEach(appendMessage);
    } else if (entry.type === 'branch_summary' && typeof entry.summary === 'string') {
      appendMessage({ role: 'user', content: entry.summary, timestamp: 0 });
    } else {
      throw new Error(`Unsupported legacy Assistant entry: ${String(entry.type)}`);
    }
  }
  for (const [toolCallId, toolName] of pendingCalls) {
    appendMessage({ role: 'toolResult', toolCallId, toolName, isError: true, timestamp: 0,
      content: [{ type: 'text', text: 'Interrupted before Pi migration. Outcome unknown; this call was not replayed.' }] });
  }
  return result;
}

export async function prepareAssistantStore(paths: AssistantHomePaths, sessionId: string,
  models: Models, createIfMissing: boolean): Promise<{ directory: string; existing: boolean }> {
  if (!/^[a-zA-Z0-9_-]{1,128}$/.test(sessionId)) throw new TypeError('Invalid assistant session ID.');
  const root = join(paths.sessions, 'pi');
  const stores = join(root, 'durable-v1');
  const directory = join(stores, sessionId);
  await assertSafeDirectory(stores);
  if (await exists(directory)) {
    await checkStore(directory);
    const marker = await readAssistantHomeFile(paths, join(directory, 'yuanpu-session.json'));
    const metadata: unknown = marker ? JSON.parse(marker) : undefined;
    if (!record(metadata) || metadata.version !== 1 || metadata.sessionId !== sessionId) {
      throw new Error('Incomplete or unsupported Assistant session store.');
    }
    return { directory, existing: true };
  }
  const matches: string[] = [];
  for (const entry of await readdir(root, { withFileTypes: true })) {
    if (!entry.name.startsWith('--')) continue;
    const legacyRoot = join(root, entry.name);
    await assertSafeDirectory(legacyRoot);
    for (const file of await readdir(legacyRoot)) {
      if (file.endsWith(`_${sessionId}.jsonl`)) matches.push(join(legacyRoot, file));
    }
  }
  if (matches.length > 1) throw new Error('Ambiguous legacy Assistant session ID.');
  const legacy = matches[0] ? await readAssistantHomeFile(paths, matches[0]) : undefined;
  if (!legacy && !createIfMissing) throw new Error(`Unknown assistant Session: ${sessionId}`);
  const drafts = legacy ? legacyEntries(legacy, sessionId) : [];
  const temporary = join(stores, `.${sessionId}.${randomUUID()}.tmp`);
  await mkdir(temporary, { mode: 0o700 });
  try {
    const harness = await Harness.open(await openNodeJsonlStorage(temporary, BACKGROUND_CONTEXT),
      { models, registry: createRegistry() }, BACKGROUND_CONTEXT);
    try {
      const conversation = await harness.root(BACKGROUND_CONTEXT);
      await conversation.commit(async (tx) => {
        for (const draft of drafts) await tx.appendEntry(conversation.id, draft);
      }, BACKGROUND_CONTEXT);
    } finally { await harness.close(BACKGROUND_CONTEXT); }
    await writeFile(join(temporary, 'yuanpu-session.json'), JSON.stringify({ version: 1, sessionId,
      ...(legacy ? { importedFrom: matches[0], sha256: createHash('sha256').update(legacy).digest('hex') } : {}),
    }), { mode: 0o600, flag: 'wx' });
    await rename(temporary, directory);
  } finally { await rm(temporary, { recursive: true, force: true }); }
  return { directory, existing: Boolean(legacy) };
}
