import { createHash } from 'node:crypto';
import { isAbsolute, join, resolve, sep } from 'node:path';

/** 审查快照单文件上限：超过即截断存储，前端以占位提示替代 diff。 */
export const WORK_FILE_CHANGE_MAX_BYTES = 256 * 1024;

export interface WorkFileSnapshot {
  content: string;
  sha256: string;
  size: number;
  truncated: boolean;
}

export interface CapturedWorkFileChange {
  toolCallId: string;
  toolName: 'edit' | 'write';
  relativePath: string;
  /** null 表示工具执行前文件不存在（新建）。 */
  before: WorkFileSnapshot | null;
  after: WorkFileSnapshot;
}

/** 返回 null 表示文件不存在（ENOENT）；其余读取错误应抛出。 */
export type WorkFileSnapshotReader = (absolutePath: string) => Promise<Buffer | null>;

function toSnapshot(raw: Buffer): WorkFileSnapshot {
  const truncated = raw.length > WORK_FILE_CHANGE_MAX_BYTES;
  return {
    content: truncated ? raw.subarray(0, WORK_FILE_CHANGE_MAX_BYTES).toString('utf8') : raw.toString('utf8'),
    sha256: createHash('sha256').update(raw).digest('hex'),
    size: raw.length,
    truncated,
  };
}

function readSnapshot(reader: WorkFileSnapshotReader, absolutePath: string): Promise<WorkFileSnapshot | null> {
  return reader(absolutePath).then((raw) => raw === null ? null : toSnapshot(raw));
}

/** 把工具请求路径规约到工作目录内；出界（含工作目录本身）返回 undefined，不捕获。 */
export function resolveWorkspaceRelativePath(rootDir: string, requestedPath: string): string | undefined {
  if (!rootDir || !requestedPath) return undefined;
  const root = resolve(rootDir);
  const absolute = isAbsolute(requestedPath) ? resolve(requestedPath) : resolve(join(root, requestedPath));
  if (absolute === root || !absolute.startsWith(root + sep)) return undefined;
  // Stored and displayed paths use '/' on every platform.
  return absolute.slice(root.length + 1).split(sep).join('/');
}

/** 一次 edit/write 工具调用的捕获会话：start 时读 before，end 后读 after。 */
export interface WorkFileCaptureSession {
  readonly toolCallId: string;
  readonly toolName: 'edit' | 'write';
  readonly relativePath: string;
  readonly absolutePath: string;
  readonly before: Promise<WorkFileSnapshot | null>;
}

export function beginWorkFileCapture(options: {
  rootDir: string;
  requestedPath: string;
  toolCallId: string;
  toolName: string;
  reader: WorkFileSnapshotReader;
}): WorkFileCaptureSession | undefined {
  if (options.toolName !== 'edit' && options.toolName !== 'write') return undefined;
  const relativePath = resolveWorkspaceRelativePath(options.rootDir, options.requestedPath);
  if (!relativePath) return undefined;
  const absolutePath = resolve(join(resolve(options.rootDir), relativePath));
  return {
    toolCallId: options.toolCallId,
    toolName: options.toolName,
    relativePath,
    absolutePath,
    before: readSnapshot(options.reader, absolutePath),
  };
}

/**
 * 工具执行结束后组装变更记录；文件未变化、after 不存在或读取失败时返回
 * undefined。任何失败都只意味着这一笔不进审查数据，不影响会话执行。
 */
export async function completeWorkFileCapture(
  session: WorkFileCaptureSession,
  reader: WorkFileSnapshotReader,
): Promise<CapturedWorkFileChange | undefined> {
  try {
    const [before, after] = await Promise.all([
      session.before,
      readSnapshot(reader, session.absolutePath).catch(() => undefined),
    ]);
    if (!after) return undefined;
    if (before && before.sha256 === after.sha256) return undefined;
    return { toolCallId: session.toolCallId, toolName: session.toolName,
      relativePath: session.relativePath, before, after };
  } catch {
    return undefined;
  }
}
