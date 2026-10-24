import { lstat, mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { dirname } from 'node:path';

type SecretProtector = {
  isEncryptionAvailable(): boolean;
  getSelectedStorageBackend?(): string;
  encryptString(value: string): Buffer;
  decryptString(value: Buffer): string;
};

export class ServerCenterCredentialStore {
  constructor(private readonly path: string, private readonly protector: SecretProtector) {}

  private available(): boolean {
    return this.protector.isEncryptionAvailable()
      && this.protector.getSelectedStorageBackend?.() !== 'basic_text';
  }

  async status(): Promise<{ configured: boolean; storageAvailable: boolean }> {
    let configured = false;
    try {
      const file = await lstat(this.path);
      if (!file.isFile()) throw new Error('知识服务中心凭据配置文件不是普通文件。');
      configured = true;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    }
    return { configured, storageAvailable: this.available() };
  }

  async loadKey(): Promise<string | undefined> {
    const { configured } = await this.status();
    if (!configured) return undefined;
    if (!this.available()) throw new Error('系统安全存储不可用，无法读取知识服务中心 API key。');
    const file = JSON.parse(await readFile(this.path, 'utf8')) as { version?: unknown; encryptedKey?: unknown };
    if (file.version !== 1 || typeof file.encryptedKey !== 'string') throw new Error('知识服务中心凭据配置无效。');
    const key = this.protector.decryptString(Buffer.from(file.encryptedKey, 'base64'));
    if (!/^(?:ypk_[A-Za-z0-9_-]{43}|agk_(?:live|test)_[A-Za-z0-9]{32})$/.test(key)) throw new Error('知识服务中心 API key 格式无效。');
    return key;
  }

  async saveKey(key: string): Promise<void> {
    if (typeof key !== 'string' || !/^(?:ypk_[A-Za-z0-9_-]{43}|agk_(?:live|test)_[A-Za-z0-9]{32})$/.test(key)) throw new Error('知识服务中心 API key 格式无效。');
    if (!this.available()) throw new Error('系统安全存储不可用，无法保存知识服务中心 API key。');
    // Refuse a pre-existing symlink before replacing the credential file.
    await this.status();
    const encryptedKey = this.protector.encryptString(key).toString('base64');
    await mkdir(dirname(this.path), { recursive: true });
    const temporary = `${this.path}.${randomUUID()}.tmp`;
    try {
      await writeFile(temporary, JSON.stringify({ version: 1, encryptedKey }), { mode: 0o600, flag: 'wx' });
      await rename(temporary, this.path);
    } finally {
      await rm(temporary, { force: true });
    }
  }

  async removeKey(): Promise<void> {
    await this.status();
    await rm(this.path, { force: true });
  }
}
