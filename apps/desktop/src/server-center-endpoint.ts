import { randomUUID } from 'node:crypto';
import { lstat, mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';

export function normalizeServerCenterEndpoint(value: string): string {
  if (typeof value !== 'string' || value.length > 2048 || value.trim() !== value) {
    throw new TypeError('中心端点必须是有效的 HTTP(S) 地址。');
  }
  let url: URL;
  try { url = new URL(value); }
  catch { throw new TypeError('中心端点必须是有效的 HTTP(S) 地址。'); }
  if (!['http:', 'https:'].includes(url.protocol) || !url.hostname || url.username || url.password || url.search || url.hash) {
    throw new TypeError('中心端点不能包含账号、密码、查询参数或片段。');
  }
  if (url.protocol === 'http:' && !['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname)) {
    throw new TypeError('远程中心端点必须使用 HTTPS。');
  }
  return url.toString().replace(/\/$/, '');
}

export function isCenterPortalUrl(value: string, endpoint: string): boolean {
  try {
    const target = new URL(value);
    const center = new URL(normalizeServerCenterEndpoint(endpoint));
    return ['http:', 'https:'].includes(target.protocol) && target.origin === center.origin;
  } catch { return false; }
}

export class ServerCenterEndpointStore {
  constructor(private readonly path: string, private readonly fallbackUrl: string) {}

  async load(): Promise<string> {
    let stat;
    try { stat = await lstat(this.path); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return normalizeServerCenterEndpoint(this.fallbackUrl);
      throw error;
    }
    if (!stat.isFile()) throw new Error('知识服务中心端点配置文件不是普通文件。');
    const config = JSON.parse(await readFile(this.path, 'utf8')) as { version?: unknown; url?: unknown };
    if (config.version !== 1 || typeof config.url !== 'string') throw new Error('知识服务中心端点配置无效。');
    const saved = normalizeServerCenterEndpoint(config.url);
    // The former local website used 8789; keep all other explicit endpoints.
    return saved === 'http://127.0.0.1:8789' ? normalizeServerCenterEndpoint(this.fallbackUrl) : saved;
  }

  async save(value: string): Promise<string> {
    const url = normalizeServerCenterEndpoint(value);
    await this.load();
    await mkdir(dirname(this.path), { recursive: true });
    const temporary = `${this.path}.${randomUUID()}.tmp`;
    try {
      await writeFile(temporary, JSON.stringify({ version: 1, url }), { mode: 0o600, flag: 'wx' });
      await rename(temporary, this.path);
    } finally {
      await rm(temporary, { force: true });
    }
    return url;
  }
}
