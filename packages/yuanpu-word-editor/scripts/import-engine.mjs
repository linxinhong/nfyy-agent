// Copies an explicitly supplied engine into Yuanpu's own storage. Does not execute the source.
import { createHash, randomUUID } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { chmod, copyFile, mkdir, realpath, rename, rm, writeFile } from 'node:fs/promises';
import { homedir } from 'node:os';
import { isAbsolute, join, resolve } from 'node:path';

const [source, expectedDigest, requestedDirectory] = process.argv.slice(2);
if (!source || !/^[a-f0-9]{64}$/i.test(expectedDigest ?? '')) {
  throw new Error('Usage: node scripts/import-engine.mjs /absolute/engine/path <pinned-sha256> [/absolute/storage/directory]');
}
if (!isAbsolute(source) || (requestedDirectory && !isAbsolute(requestedDirectory))) throw new Error('Absolute paths are required.');
const sourcePath = await realpath(source);
const engineSha256 = expectedDigest.toLowerCase();
const digest = async (path) => {
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  return hash.digest('hex');
};
if (await digest(sourcePath) !== engineSha256) throw new Error('Source engine SHA-256 does not match the pinned digest.');
const storage = resolve(requestedDirectory ?? join(homedir(), '.yuanpu', 'word-editor'));
const directory = join(storage, 'engines', `${process.platform}-${process.arch}`, engineSha256);
await mkdir(directory, { recursive: true });
const enginePath = join(await realpath(directory), process.platform === 'win32' ? 'editor_sdk.exe' : 'editor_sdk');
const temporary = `${enginePath}.${randomUUID()}.tmp`;
try {
  await copyFile(sourcePath, temporary);
  if (await digest(temporary) !== engineSha256) throw new Error('Copied engine failed SHA-256 verification.');
  await chmod(temporary, 0o755);
  await rename(temporary, enginePath);
} finally { await rm(temporary, { force: true }); }
const configPath = join(storage, 'engine.json');
const config = { platform: process.platform, arch: process.arch, enginePath, engineSha256 };
const temporaryConfig = `${configPath}.${randomUUID()}.tmp`;
await writeFile(temporaryConfig, `${JSON.stringify(config, null, 2)}\n`, { flag: 'wx', mode: 0o600 });
await rename(temporaryConfig, configPath);
console.log(JSON.stringify({ ok: true, configPath, ...config }, null, 2));
