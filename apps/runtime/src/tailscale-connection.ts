import { randomUUID } from 'node:crypto';
import { chmod, lstat, mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

import type { SaveTailscaleConnectionInput, TailscaleConnectionSettings, TailscaleNodeStatus } from '@yuanpu-agent/protocol';

interface TailscaleConnectionDocument {
  schemaVersion: 1;
  authKey?: string;
}

const emptyDocument: TailscaleConnectionDocument = { schemaVersion: 1 };
const allowedDocumentKeys = new Set(['schemaVersion', 'authKey']);
const allowedInputKeys = new Set(['authKey', 'removeAuthKey']);

function record(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
}

function validSecret(value: unknown): value is string {
  return typeof value === 'string' && value.length <= 16_384 && value.trim().length > 0
    && !/[\r\n\0]/.test(value);
}

function parseDocument(value: unknown): TailscaleConnectionDocument {
  if (!record(value) || Object.keys(value).some((key) => !allowedDocumentKeys.has(key))
    || value.schemaVersion !== 1 || (value.authKey !== undefined && !validSecret(value.authKey))) {
    throw new Error('Tailscale connection configuration is invalid.');
  }
  return value as unknown as TailscaleConnectionDocument;
}

export type { TailscaleConnectionDocument };

export async function readTailscaleConnectionDocument(appPath: string): Promise<TailscaleConnectionDocument> {
  const path = join(appPath, 'connections', 'tailscale.json');
  try {
    const entry = await lstat(path);
    if (!entry.isFile() || entry.isSymbolicLink()) throw new Error('Tailscale connection configuration is invalid.');
    return parseDocument(JSON.parse(await readFile(path, 'utf8')) as unknown);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return { ...emptyDocument };
    if (error instanceof SyntaxError) throw new Error('Tailscale connection configuration is invalid.');
    throw error;
  }
}

export function summarizeTailscaleConnection(document: TailscaleConnectionDocument,
  node: TailscaleNodeStatus = { state: 'unavailable' }): TailscaleConnectionSettings {
  return {
    hasAuthKey: Boolean(document.authKey),
    node,
  };
}

function applyInput(document: TailscaleConnectionDocument, value: unknown): TailscaleConnectionDocument {
  if (!record(value) || Object.keys(value).some((key) => !allowedInputKeys.has(key))
    || (value.removeAuthKey !== undefined && typeof value.removeAuthKey !== 'boolean')) {
    throw new Error('Invalid Tailscale connection input.');
  }
  const input = value as unknown as SaveTailscaleConnectionInput;
  const next: TailscaleConnectionDocument = { ...document };
  if (input.authKey !== undefined && input.removeAuthKey) throw new Error('Cannot set and remove the same Tailscale key.');
  if (input.authKey !== undefined) {
    if (!validSecret(input.authKey)) throw new Error('Invalid Tailscale key.');
    next.authKey = input.authKey.trim();
  } else if (input.removeAuthKey) {
    delete next.authKey;
  }
  return parseDocument(next);
}

export async function saveTailscaleConnection(
  appPath: string, value: unknown,
): Promise<TailscaleConnectionSettings> {
  const directory = join(appPath, 'connections');
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const directoryEntry = await lstat(directory);
  if (!directoryEntry.isDirectory() || directoryEntry.isSymbolicLink()) {
    throw new Error('Tailscale connection directory is invalid.');
  }
  const next = applyInput(await readTailscaleConnectionDocument(appPath), value);
  const temporary = join(directory, `.tailscale-${randomUUID()}.tmp`);
  try {
    await writeFile(temporary, `${JSON.stringify(next, null, 2)}\n`, { flag: 'wx', mode: 0o600 });
    await rename(temporary, join(directory, 'tailscale.json'));
    await chmod(join(directory, 'tailscale.json'), 0o600);
  } finally {
    await rm(temporary, { force: true });
  }
  return summarizeTailscaleConnection(next);
}
