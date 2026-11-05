import { randomUUID } from 'node:crypto';
import { constants as fsConstants, promises as fs } from 'node:fs';
import { isAbsolute, join, relative, resolve } from 'node:path';

/** Text previews are hard-capped and reported as truncated above this size. */
export const MAX_TEXT_PREVIEW_BYTES = 256 * 1024;
export const MAX_IMAGE_PREVIEW_BYTES = 8 * 1024 * 1024;
export const MAX_PDF_PREVIEW_BYTES = 20 * 1024 * 1024;
/** Flat workspace listings stop growing past this and report `truncated`. */
export const MAX_TREE_ENTRIES = 5000;
const MAX_TREE_DEPTH = 24;

const IMAGE_MEDIA_TYPES: Record<string, string> = {
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.gif': 'image/gif',
  '.webp': 'image/webp',
  '.bmp': 'image/bmp',
  '.svg': 'image/svg+xml',
};

/** Carries an HTTP-ready status so route handlers can map it directly. */
export class WorkspaceFileAccessError extends Error {
  constructor(message: string, readonly statusCode: number) {
    super(message);
    this.name = 'WorkspaceFileAccessError';
  }
}

function toPosixPath(value: string): string {
  return process.platform === 'win32' ? value.replace(/\\/g, '/') : value;
}

function containsReal(root: string, target: string): boolean {
  const rel = relative(root, target);
  return rel === '' || (!rel.startsWith('..') && !isAbsolute(rel));
}

/**
 * Resolves a caller-supplied workspace-relative path against root and refuses
 * escapes. Absolute paths are accepted only when they stay inside the
 * workspace. Symlinked components are resolved via realpath and re-checked so
 * a link cannot smuggle access outside the workspace.
 */
export async function resolveWorkspacePath(root: string, inputPath: string): Promise<string> {
  if (typeof inputPath !== 'string' || inputPath.includes('\0')) {
    throw new WorkspaceFileAccessError('路径不合法，已拒绝访问。', 400);
  }
  const normalized = toPosixPath(inputPath);
  if (isAbsolute(normalized) || normalized.split('/').some((segment) => segment === '..')) {
    throw new WorkspaceFileAccessError('路径越出工作区，已拒绝访问。', 400);
  }
  const realRoot = await fs.realpath(resolve(root));
  const target = resolve(realRoot, normalized);
  if (!containsReal(realRoot, target)) {
    throw new WorkspaceFileAccessError('路径越出工作区，已拒绝访问。', 400);
  }
  const real = await fs.realpath(target)
    .catch(() => { throw new WorkspaceFileAccessError('文件或目录不存在。', 404); });
  if (!containsReal(realRoot, real)) {
    throw new WorkspaceFileAccessError('路径越出工作区，已拒绝访问。', 400);
  }
  return real;
}

/** Resolve only regular files before handing a path to the desktop shell. */
export async function resolveWorkspaceFilePath(root: string, inputPath: string): Promise<string> {
  const path = await resolveWorkspacePath(root, inputPath);
  const stat = await fs.stat(path).catch(() => { throw new WorkspaceFileAccessError('文件不存在。', 404); });
  if (!stat.isFile()) throw new WorkspaceFileAccessError('所选路径不是文件。', 400);
  return path;
}

/** NUL bytes or a dominant share of control characters mark binary content. */
export function isProbablyBinary(buffer: Buffer): boolean {
  const sample = buffer.subarray(0, 8192);
  if (sample.includes(0)) return true;
  let controlChars = 0;
  for (const byte of sample) {
    if (byte < 9 || (byte > 13 && byte < 32)) controlChars += 1;
  }
  return sample.length > 0 && controlChars / sample.length > 0.3;
}

function matchesImageFormat(buffer: Buffer, extension: string): boolean {
  switch (extension) {
    case '.png': return buffer.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]));
    case '.jpg':
    case '.jpeg': return buffer.length >= 3 && buffer[0] === 0xff && buffer[1] === 0xd8 && buffer[2] === 0xff;
    case '.gif': return buffer.subarray(0, 6).toString('ascii') === 'GIF87a'
      || buffer.subarray(0, 6).toString('ascii') === 'GIF89a';
    case '.webp': return buffer.subarray(0, 4).toString('ascii') === 'RIFF'
      && buffer.subarray(8, 12).toString('ascii') === 'WEBP';
    case '.bmp': return buffer.length >= 2 && buffer[0] === 0x42 && buffer[1] === 0x4d;
    case '.svg': return !isProbablyBinary(buffer) && /<svg(?:\s|>)/i.test(buffer.subarray(0, 4096).toString('utf8'));
    default: return false;
  }
}

function fileExtension(path: string): string {
  const name = toPosixPath(path).split('/').pop() ?? '';
  const dot = name.lastIndexOf('.');
  return dot <= 0 ? '' : name.slice(dot).toLowerCase();
}

/** Classifies a preview without touching the filesystem. */
export function classifyWorkspaceFile(path: string): 'image' | 'pdf' | 'text' {
  if (fileExtension(path) === '.pdf') return 'pdf';
  if (fileExtension(path) in IMAGE_MEDIA_TYPES) return 'image';
  return 'text';
}

export async function listWorkspaceFiles(root: string, dirPath: string) {
  const realDirectory = await resolveWorkspacePath(root, dirPath);
  const stat = await fs.stat(realDirectory)
    .catch(() => { throw new WorkspaceFileAccessError('文件或目录不存在。', 404); });
  if (!stat.isDirectory()) {
    throw new WorkspaceFileAccessError('所选路径不是目录。', 400);
  }
  const dirents = await fs.readdir(realDirectory, { withFileTypes: true });
  const parent = toPosixPath(dirPath).replace(/^\.\//, '').replace(/\/+$/, '');
  const entries = await Promise.all(dirents.map(async (dirent) => {
    const entryPath = parent ? `${parent}/${dirent.name}` : dirent.name;
    try {
      const entryStat = await fs.stat(resolve(realDirectory, dirent.name));
      return {
        name: dirent.name,
        path: entryPath,
        kind: entryStat.isDirectory() ? 'directory' as const : 'file' as const,
        size: entryStat.isFile() ? entryStat.size : undefined,
        updatedAt: entryStat.mtime.toISOString(),
      };
    } catch {
      // Broken symlinks and vanished entries are skipped, never leaked.
      return undefined;
    }
  }));
  const visible = entries.filter((entry) => entry !== undefined) as NonNullable<(typeof entries)[number]>[];
  visible.sort((left, right) => {
    if (left.kind !== right.kind) return left.kind === 'directory' ? -1 : 1;
    return left.name.localeCompare(right.name, 'zh-Hans-CN');
  });
  return { path: toPosixPath(dirPath).replace(/^\.\//, '').replace(/\/+$/, ''), entries: visible };
}

/**
 * Flattened recursive listing for path-first tree renderers. Symlinked
 * entries are only included when their realpath stays inside the workspace;
 * symlink cycles are cut by visited-set, and growth is capped by
 * MAX_TREE_ENTRIES (reported via `truncated`).
 */
export async function listWorkspaceTree(root: string, dirPath: string) {
  const realRoot = await fs.realpath(resolve(root));
  const realDirectory = await resolveWorkspacePath(root, dirPath);
  const stat = await fs.stat(realDirectory)
    .catch(() => { throw new WorkspaceFileAccessError('文件或目录不存在。', 404); });
  if (!stat.isDirectory()) {
    throw new WorkspaceFileAccessError('所选路径不是目录。', 400);
  }
  const base = toPosixPath(dirPath).replace(/^\.\//, '').replace(/\/+$/, '');
  const entries: Array<{ name: string; path: string; kind: 'file' | 'directory'; size?: number; updatedAt: string }> = [];
  let truncated = false;
  const visitedDirectories = new Set<string>([realDirectory]);
  const walk = async (absDir: string, relDir: string, depth: number): Promise<void> => {
    if (depth > MAX_TREE_DEPTH) {
      truncated = true;
      return;
    }
    let dirents;
    try { dirents = await fs.readdir(absDir, { withFileTypes: true }); } catch { return; }
    dirents.sort((left, right) => left.name.localeCompare(right.name, 'zh-Hans-CN'));
    for (const dirent of dirents) {
      if (entries.length >= MAX_TREE_ENTRIES) {
        truncated = true;
        return;
      }
      const childAbs = resolve(absDir, dirent.name);
      const childRel = relDir ? `${relDir}/${dirent.name}` : dirent.name;
      if (dirent.isSymbolicLink()) {
        let real: string;
        try { real = await fs.realpath(childAbs); } catch { continue; }
        if (!containsReal(realRoot, real)) continue;
      }
      let childStat;
      try { childStat = await fs.stat(childAbs); } catch { continue; }
      if (childStat.isDirectory()) {
        if (dirent.isSymbolicLink()) {
          const real = await fs.realpath(childAbs);
          if (visitedDirectories.has(real)) continue;
          visitedDirectories.add(real);
        }
        entries.push({ name: dirent.name, path: childRel, kind: 'directory', updatedAt: childStat.mtime.toISOString() });
        await walk(childAbs, childRel, depth + 1);
      } else if (childStat.isFile()) {
        entries.push({ name: dirent.name, path: childRel, kind: 'file', size: childStat.size, updatedAt: childStat.mtime.toISOString() });
      }
    }
  };
  await walk(realDirectory, base, 0);
  return { path: base, entries, truncated };
}

export async function readWorkspaceFile(root: string, filePath: string) {
  const realFile = await resolveWorkspacePath(root, filePath);
  const stat = await fs.stat(realFile)
    .catch(() => { throw new WorkspaceFileAccessError('文件或目录不存在。', 404); });
  if (!stat.isFile()) {
    throw new WorkspaceFileAccessError('目录不能作为文件预览。', 400);
  }
  const updatedAt = stat.mtime.toISOString();
  const relativePath = toPosixPath(filePath).replace(/^\.\//, '').replace(/\/+$/, '');
  const kind = classifyWorkspaceFile(relativePath);
  if (kind === 'pdf') {
    if (stat.size > MAX_PDF_PREVIEW_BYTES) {
      return { kind: 'unsupported' as const, path: relativePath,
        reason: `PDF 超过 ${Math.round(MAX_PDF_PREVIEW_BYTES / 1024 / 1024)} MB，暂不支持预览。`, size: stat.size };
    }
    const buffer = await fs.readFile(realFile);
    return { kind: 'pdf' as const, path: relativePath, base64: buffer.toString('base64'),
      size: stat.size, updatedAt };
  }
  if (kind === 'image') {
    if (stat.size > MAX_IMAGE_PREVIEW_BYTES) {
      return { kind: 'unsupported' as const, path: relativePath,
        reason: `图片超过 ${Math.round(MAX_IMAGE_PREVIEW_BYTES / 1024 / 1024)} MB，暂不支持预览。`, size: stat.size };
    }
    const buffer = await fs.readFile(realFile);
    if (!matchesImageFormat(buffer, fileExtension(relativePath))) {
      return { kind: 'unsupported' as const, path: relativePath, reason: '文件内容不是有效图片。', size: stat.size };
    }
    return { kind: 'image' as const, path: relativePath, mediaType: IMAGE_MEDIA_TYPES[fileExtension(relativePath)]!,
      base64: buffer.toString('base64'), size: stat.size, updatedAt };
  }
  const limit = Math.min(MAX_TEXT_PREVIEW_BYTES, Math.max(stat.size, 1));
  const handle = await fs.open(realFile, 'r');
  let buffer: Buffer;
  try {
    buffer = await handle.read(Buffer.alloc(limit), 0, limit, 0).then((read) => read.buffer.subarray(0, read.bytesRead));
  } finally {
    await handle.close();
  }
  if (isProbablyBinary(buffer)) {
    return { kind: 'unsupported' as const, path: relativePath, reason: '二进制文件暂不支持预览。', size: stat.size };
  }
  return {
    kind: 'text' as const,
    path: relativePath,
    content: buffer.toString('utf-8'),
    truncated: stat.size > MAX_TEXT_PREVIEW_BYTES,
    size: stat.size,
    updatedAt,
  };
}

/** Files the user drops into the composer are saved here, relative to the workspace. */
export const IMPORT_DIRECTORY = '附件';
export const MAX_IMPORT_BYTES = 50 * 1024 * 1024;

/** Keeps only a safe final path segment; the extension survives truncation. */
export function safeImportName(name: string): string {
  const base = String(name).split(/[\\/]/).at(-1) ?? '';
  const cleaned = base.replace(/[\u0000-\u001f\u007f:*?"<>|]/g, '_').trim().replace(/^\.+/, '').replace(/[. ]+$/, '');
  if (!cleaned) return 'attachment';
  const dot = cleaned.lastIndexOf('.');
  const extension = dot > 0 && cleaned.length - dot <= 16 ? cleaned.slice(dot) : '';
  const stem = extension ? cleaned.slice(0, dot) : cleaned;
  return `${Array.from(stem).slice(0, 100).join('')}${extension}`;
}

function candidateName(name: string, attempt: number): string {
  if (attempt === 1) return name;
  const dot = name.lastIndexOf('.');
  return dot > 0 ? `${name.slice(0, dot)} (${attempt})${name.slice(dot)}` : `${name} (${attempt})`;
}

/**
 * Streams one dropped file into `<workspace>/附件/` without ever overwriting an
 * existing file. The directory is re-checked after realpath so a symlinked 附件
 * cannot redirect the write outside the workspace.
 */
export async function importWorkspaceFile(root: string, name: string, chunks: AsyncIterable<Uint8Array>,
  maxBytes = MAX_IMPORT_BYTES): Promise<{ path: string; size: number }> {
  const realRoot = await fs.realpath(resolve(root))
    .catch(() => { throw new WorkspaceFileAccessError('工作目录不存在。', 404); });
  const directory = join(realRoot, IMPORT_DIRECTORY);
  await fs.mkdir(directory, { recursive: true });
  const realDirectory = await fs.realpath(directory);
  if (!containsReal(realRoot, realDirectory) || !(await fs.lstat(realDirectory)).isDirectory()) {
    throw new WorkspaceFileAccessError('附件目录越出工作区，已拒绝写入。', 400);
  }
  const temporary = join(realDirectory, `.import-${randomUUID()}.part`);
  const handle = await fs.open(temporary, 'wx', 0o644);
  let size = 0;
  try {
    try {
      for await (const chunk of chunks) {
        size += chunk.byteLength;
        if (size > maxBytes) throw new WorkspaceFileAccessError(`单个附件不能超过 ${Math.floor(maxBytes / 1024 / 1024)} MB。`, 413);
        await handle.write(chunk);
      }
    } finally {
      await handle.close();
    }
    const safe = safeImportName(name);
    for (let attempt = 1; attempt <= 1000; attempt++) {
      const finalName = candidateName(safe, attempt);
      const target = join(realDirectory, finalName);
      try {
        // link() and COPYFILE_EXCL both fail instead of replacing an existing file.
        await fs.link(temporary, target).catch(async (error: NodeJS.ErrnoException) => {
          if (error.code === 'EEXIST') throw error;
          await fs.copyFile(temporary, target, fsConstants.COPYFILE_EXCL);
        });
        return { path: `${IMPORT_DIRECTORY}/${finalName}`, size };
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error;
      }
    }
    throw new WorkspaceFileAccessError('同名附件过多，请先整理附件目录。', 409);
  } finally {
    await fs.rm(temporary, { force: true });
  }
}
