import type {
  CapabilityArtifactTarget,
  CapabilityPackageManifest,
  PluginConfigValidation,
} from '@yuanpu-agent/protocol';
import Ajv from 'ajv';
import {
  createHash,
  createPublicKey,
  randomUUID,
  verify as verifySignature,
} from 'node:crypto';
import {
  access,
  chmod,
  mkdir,
  open,
  readFile,
  rename,
  rm,
  writeFile,
} from 'node:fs/promises';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { createServer as createNetServer } from 'node:net';
import { Readable } from 'node:stream';
import * as tar from 'tar';

const STATE_VERSION = 1;
const DEFAULT_MAX_ARCHIVE_BYTES = 256 * 1024 * 1024;
const DEFAULT_MAX_UNPACKED_BYTES = 768 * 1024 * 1024;
const DEFAULT_MAX_ENTRIES = 20_000;
const LOCK_PORT_BASE = 12_000;
const LOCK_PORT_SPAN = 28_000;
const artifactConfigAjv = new Ajv({ strict: true, allErrors: true });

export function validateCapabilityConfig(
  schema: Record<string, unknown>,
  value: Record<string, unknown>,
): PluginConfigValidation {
  const validate = artifactConfigAjv.compile<Record<string, unknown>>(schema);
  const valid = validate(value);
  return {
    valid: Boolean(valid),
    errors: (validate.errors ?? []).map((error) => (
      `${error.instancePath || '/'} ${error.message ?? '配置无效'}`
    )),
  };
}

export interface ArtifactTrustRoot {
  keyId: string;
  publicKeyPem: string;
}

export interface InstalledArtifactVersion {
  version: string;
  installPath: string;
  entrypoint: string;
  installedAt: string;
  issuedAt: string;
  target: string;
  manifestUrl?: string;
  configSchema?: Record<string, unknown>;
  permissions?: CapabilityPackageManifest['permissions'];
  connections?: string[];
}

export interface InstalledArtifactPackage {
  id: string;
  kind: 'python-mcp';
  activeVersion: string;
  versions: Record<string, InstalledArtifactVersion>;
  lastIssuedAt: string;
}

interface ArtifactState {
  schemaVersion: 1;
  packages: Record<string, InstalledArtifactPackage>;
}

export interface ArtifactInstallOptions {
  allowDowngrade?: boolean;
  healthCheck: (entrypoint: string, installPath: string) => Promise<void>;
  manifestUrl?: string;
  signal?: AbortSignal;
}

export interface CapabilityArtifactManagerOptions {
  runtimeVersion: string;
  capabilityContractVersion?: number;
  trustRoots: readonly ArtifactTrustRoot[];
  platform?: NodeJS.Platform;
  arch?: string;
  fetch?: typeof globalThis.fetch;
  maxArchiveBytes?: number;
  maxUnpackedBytes?: number;
  maxEntries?: number;
}

export function artifactInstallLockPort(packagesRoot: string): number {
  const digest = createHash('sha256').update(resolve(packagesRoot)).digest();
  return LOCK_PORT_BASE + (digest.readUInt32BE(0) % LOCK_PORT_SPAN);
}

function initialState(): ArtifactState {
  return { schemaVersion: STATE_VERSION, packages: {} };
}

function isInside(parent: string, child: string): boolean {
  const value = relative(resolve(parent), resolve(child));
  return value === '' || (value !== '..' && !value.startsWith(`..${sep}`));
}

function safeSegment(value: string, label: string): string {
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/.test(value)) {
    throw new Error(`${label} contains unsafe characters.`);
  }
  return value;
}

function canonicalize(value: unknown): string {
  if (value === null || typeof value !== 'object') return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonicalize).join(',')}]`;
  const record = value as Record<string, unknown>;
  return `{${Object.keys(record).sort().map((key) => `${JSON.stringify(key)}:${canonicalize(record[key])}`).join(',')}}`;
}

export function capabilityManifestSigningPayload(manifest: CapabilityPackageManifest): Uint8Array {
  const { signature: _signature, ...unsigned } = manifest;
  return Buffer.from(canonicalize(unsigned), 'utf8');
}

export function capabilityManifestDigest(manifest: CapabilityPackageManifest): string {
  return createHash('sha256').update(capabilityManifestSigningPayload(manifest)).digest('hex');
}

function compareVersions(left: string, right: string): number {
  const parse = (value: string) => {
    const match = /^(\d+)\.(\d+)\.(\d+)$/.exec(value);
    if (!match) throw new Error(`Unsupported semantic version: ${value}`);
    return match.slice(1).map(Number);
  };
  const a = parse(left);
  const b = parse(right);
  for (let index = 0; index < 3; index += 1) {
    if (a[index]! !== b[index]!) return a[index]! < b[index]! ? -1 : 1;
  }
  return 0;
}

function assertRuntimeCompatible(manifest: CapabilityPackageManifest, runtimeVersion: string): void {
  const { minimum, maximumExclusive } = manifest.runtimeCompatibility;
  if (compareVersions(runtimeVersion, minimum) < 0
    || (maximumExclusive && compareVersions(runtimeVersion, maximumExclusive) >= 0)) {
    throw new Error(`Capability ${manifest.id}@${manifest.version} is incompatible with runtime ${runtimeVersion}.`);
  }
}

async function exists(path: string): Promise<boolean> {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
}

async function writeJsonAtomic(path: string, value: unknown): Promise<void> {
  const temporary = `${path}.${randomUUID()}.tmp`;
  await mkdir(dirname(path), { recursive: true });
  await writeFile(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  await rename(temporary, path);
}

async function delay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  await new Promise<void>((resolveDelay, reject) => {
    const timer = setTimeout(resolveDelay, milliseconds);
    signal?.addEventListener('abort', () => {
      clearTimeout(timer);
      reject(signal.reason ?? new Error('Operation aborted.'));
    }, { once: true });
  });
}

export class CapabilityArtifactManager {
  private readonly statePath: string;
  private readonly artifactsRoot: string;
  private readonly stagingRoot: string;
  private readonly lockPort: number;
  private readonly platform: NodeJS.Platform;
  private readonly arch: string;
  private readonly fetcher: typeof globalThis.fetch;
  private readonly trustRoots: ReadonlyMap<string, string>;
  private readonly maxArchiveBytes: number;
  private readonly maxUnpackedBytes: number;
  private readonly maxEntries: number;

  constructor(
    private readonly packagesRoot: string,
    private readonly options: CapabilityArtifactManagerOptions,
  ) {
    this.statePath = join(packagesRoot, 'artifact-state.json');
    this.artifactsRoot = join(packagesRoot, 'artifacts');
    this.stagingRoot = join(packagesRoot, '.staging', 'artifacts');
    this.lockPort = artifactInstallLockPort(packagesRoot);
    this.platform = options.platform ?? process.platform;
    this.arch = options.arch ?? process.arch;
    this.fetcher = options.fetch ?? globalThis.fetch;
    this.maxArchiveBytes = options.maxArchiveBytes ?? DEFAULT_MAX_ARCHIVE_BYTES;
    this.maxUnpackedBytes = options.maxUnpackedBytes ?? DEFAULT_MAX_UNPACKED_BYTES;
    this.maxEntries = options.maxEntries ?? DEFAULT_MAX_ENTRIES;
    this.trustRoots = new Map(options.trustRoots.map((root) => [root.keyId, root.publicKeyPem]));
    if (this.trustRoots.size === 0) throw new Error('At least one local artifact trust root is required.');
  }

  private async ensure(): Promise<void> {
    await Promise.all([
      mkdir(this.artifactsRoot, { recursive: true }),
      mkdir(this.stagingRoot, { recursive: true }),
    ]);
    if (!await exists(this.statePath)) {
      try {
        const handle = await open(this.statePath, 'wx', 0o600);
        try {
          await handle.writeFile(`${JSON.stringify(initialState(), null, 2)}\n`);
          await handle.sync();
        } finally {
          await handle.close();
        }
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error;
      }
    }
  }

  private async readState(): Promise<ArtifactState> {
    await this.ensure();
    let state: ArtifactState | undefined;
    let lastError: unknown;
    for (let attempt = 0; attempt < 20; attempt += 1) {
      try {
        state = JSON.parse(await readFile(this.statePath, 'utf8')) as ArtifactState;
        break;
      } catch (error) {
        lastError = error;
        await delay(10);
      }
    }
    if (!state) throw lastError;
    if (state.schemaVersion !== STATE_VERSION || !state.packages || typeof state.packages !== 'object') {
      throw new Error(`Invalid artifact state: ${this.statePath}`);
    }
    return state;
  }

  private verifyManifest(manifest: CapabilityPackageManifest): void {
    if (manifest.manifestVersion !== 1 || manifest.kind !== 'python-mcp') {
      throw new Error('Unsupported capability manifest kind or version.');
    }
    if (manifest.capabilityContractVersion !== (this.options.capabilityContractVersion ?? 1)) {
      throw new Error(`Unsupported capability contract version: ${manifest.capabilityContractVersion}.`);
    }
    if (manifest.signature.algorithm !== 'ed25519') throw new Error('Unsupported manifest signature algorithm.');
    safeSegment(manifest.id, 'Capability id');
    safeSegment(manifest.version, 'Capability version');
    compareVersions(manifest.version, manifest.version);
    const trustedKey = this.trustRoots.get(manifest.signature.keyId);
    if (!trustedKey) throw new Error(`Untrusted manifest signing key: ${manifest.signature.keyId}`);
    const valid = verifySignature(
      null,
      capabilityManifestSigningPayload(manifest),
      createPublicKey(trustedKey),
      Buffer.from(manifest.signature.value, 'base64'),
    );
    if (!valid) throw new Error('Capability manifest signature is invalid.');
    assertRuntimeCompatible(manifest, this.options.runtimeVersion);
    const issuedAt = Date.parse(manifest.issuedAt);
    if (!Number.isFinite(issuedAt) || issuedAt > Date.now() + 5 * 60 * 1000) {
      throw new Error('Capability manifest issuedAt is invalid.');
    }
  }

  inspectManifest(manifest: CapabilityPackageManifest): CapabilityPackageManifest {
    this.verifyManifest(manifest);
    this.selectTarget(manifest);
    return manifest;
  }

  private selectTarget(manifest: CapabilityPackageManifest): CapabilityArtifactTarget {
    const target = manifest.artifacts.find((candidate) => (
      candidate.platform === this.platform && candidate.arch === this.arch
    ));
    if (!target) throw new Error(`No artifact for ${this.platform}-${this.arch}.`);
    if (target.format !== 'tar.gz') throw new Error(`Unsupported artifact format: ${target.format}`);
    if (!Number.isSafeInteger(target.size) || target.size <= 0 || target.size > this.maxArchiveBytes) {
      throw new Error('Artifact archive size exceeds the configured limit.');
    }
    if (!/^[a-f0-9]{64}$/.test(target.sha256)) throw new Error('Artifact SHA-256 is invalid.');
    return target;
  }

  private async acquireLock(signal?: AbortSignal): Promise<() => Promise<void>> {
    await this.ensure();
    for (let attempt = 0; attempt < 4_800; attempt += 1) {
      const server = createNetServer();
      server.unref();
      try {
        await new Promise<void>((resolveListen, reject) => {
          const onError = (error: Error) => {
            server.off('listening', onListening);
            reject(error);
          };
          const onListening = () => {
            server.off('error', onError);
            resolveListen();
          };
          server.once('error', onError);
          server.once('listening', onListening);
          server.listen({ host: '127.0.0.1', port: this.lockPort, exclusive: true });
        });
        return async () => {
          await new Promise<void>((resolveClose, reject) => {
            server.close((error) => error ? reject(error) : resolveClose());
          });
        };
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== 'EADDRINUSE') throw error;
        await delay(25, signal);
      }
    }
    throw new Error('Timed out waiting for the artifact install lock.');
  }

  private async download(
    target: CapabilityArtifactTarget,
    path: string,
    signal?: AbortSignal,
    manifestUrl?: string,
  ): Promise<void> {
    let artifactUrl = target.url;
    try {
      artifactUrl = new URL(target.url, manifestUrl).toString();
    } catch {
      throw new Error('Relative artifact URL requires the fetched manifest URL.');
    }
    const response = await this.fetcher(artifactUrl, { signal });
    if (!response.ok || !response.body) throw new Error(`Artifact download failed with HTTP ${response.status}.`);
    const declared = Number(response.headers.get('content-length'));
    if (Number.isFinite(declared) && declared > target.size) throw new Error('Artifact response exceeds signed size.');
    const handle = await open(path, 'wx', 0o600);
    let received = 0;
    const digest = createHash('sha256');
    try {
      for await (const chunk of Readable.fromWeb(response.body as never)) {
        const data = Buffer.from(chunk as Uint8Array);
        received += data.length;
        if (received > target.size || received > this.maxArchiveBytes) {
          throw new Error('Artifact download exceeded the signed size.');
        }
        digest.update(data);
        await handle.write(data);
      }
    } finally {
      await handle.close();
    }
    if (received !== target.size) throw new Error('Artifact download size does not match the signed manifest.');
    if (digest.digest('hex') !== target.sha256) throw new Error('Artifact SHA-256 mismatch.');
  }

  private async extract(archive: string, destination: string): Promise<void> {
    let entries = 0;
    let unpackedBytes = 0;
    let rejection: Error | undefined;
    await mkdir(destination, { recursive: true });
    await tar.x({
      cwd: destination,
      file: archive,
      gzip: true,
      preservePaths: false,
      strict: true,
      filter: (entryPath, entry) => {
        const archiveEntry = entry as { size?: number; type?: string };
        entries += 1;
        unpackedBytes += archiveEntry.size ?? 0;
        const normalized = entryPath.replaceAll('\\', '/');
        const unsafePath = normalized.startsWith('/')
          || /^[a-zA-Z]:\//.test(normalized)
          || normalized.split('/').includes('..');
        const allowedType = archiveEntry.type === 'File' || archiveEntry.type === 'Directory';
        if (unsafePath || !allowedType) {
          rejection ??= new Error(`Unsafe archive entry: ${entryPath} (${archiveEntry.type}).`);
          return false;
        }
        if (entries > this.maxEntries || unpackedBytes > this.maxUnpackedBytes) {
          rejection ??= new Error('Artifact extraction exceeded configured limits.');
          return false;
        }
        return true;
      },
    });
    if (rejection) throw rejection;
  }

  async list(): Promise<InstalledArtifactPackage[]> {
    const state = await this.readState();
    return Object.values(state.packages);
  }

  async active(id: string): Promise<InstalledArtifactVersion | undefined> {
    const item = (await this.readState()).packages[id];
    return item?.versions[item.activeVersion];
  }

  async install(
    manifest: CapabilityPackageManifest,
    installOptions: ArtifactInstallOptions,
  ): Promise<InstalledArtifactVersion> {
    this.verifyManifest(manifest);
    const target = this.selectTarget(manifest);
    const release = await this.acquireLock(installOptions.signal);
    const transaction = join(this.stagingRoot, randomUUID());
    const archive = join(transaction, 'artifact.tar.gz');
    const unpacked = join(transaction, 'unpacked');
    try {
      const state = await this.readState();
      const previous = state.packages[manifest.id];
      if (previous) {
        const alreadyInstalled = previous.versions[manifest.version];
        if (alreadyInstalled) return alreadyInstalled;
        if (Date.parse(manifest.issuedAt) <= Date.parse(previous.lastIssuedAt)) {
          throw new Error('Rejected stale capability manifest replay.');
        }
        if (!installOptions.allowDowngrade && compareVersions(manifest.version, previous.activeVersion) < 0) {
          throw new Error('Capability downgrade requires explicit authorization.');
        }
      }

      await mkdir(transaction, { recursive: true });
      await this.download(target, archive, installOptions.signal, installOptions.manifestUrl);
      await this.extract(archive, unpacked);
      const entrypoint = resolve(unpacked, target.entrypoint);
      if (!isInside(unpacked, entrypoint) || !await exists(entrypoint)) {
        throw new Error('Artifact entrypoint is missing or outside the package.');
      }
      if (this.platform !== 'win32') await chmod(entrypoint, 0o755);

      const targetName = `${this.platform}-${this.arch}`;
      const installPath = join(
        this.artifactsRoot,
        safeSegment(manifest.id, 'Capability id'),
        safeSegment(manifest.version, 'Capability version'),
        targetName,
      );
      if (!isInside(this.artifactsRoot, installPath)) throw new Error('Unsafe artifact install path.');
      await mkdir(dirname(installPath), { recursive: true });
      // A directory without a committed state record is an orphan from an interrupted switch.
      // It must never be trusted in place of the just-verified staging tree.
      if (await exists(installPath)) await rm(installPath, { recursive: true });
      await rename(unpacked, installPath);

      // Run only at the immutable destination: Windows may keep executable/DLL
      // handles open briefly after exit, preventing a subsequent directory rename.
      // No state is activated until health succeeds; failure leaves an untrusted
      // orphan which the next install replaces, while the old version stays active.
      await installOptions.healthCheck(join(installPath, target.entrypoint), installPath);

      const installed: InstalledArtifactVersion = {
        version: manifest.version,
        installPath,
        entrypoint: join(installPath, target.entrypoint),
        installedAt: new Date().toISOString(),
        issuedAt: manifest.issuedAt,
        target: targetName,
        ...(installOptions.manifestUrl ? { manifestUrl: installOptions.manifestUrl } : {}),
        ...(manifest.configSchema ? { configSchema: manifest.configSchema } : {}),
        permissions: manifest.permissions,
        ...(manifest.connections ? { connections: manifest.connections } : {}),
      };
      const versions = { ...(previous?.versions ?? {}), [manifest.version]: installed };
      state.packages[manifest.id] = {
        id: manifest.id,
        kind: 'python-mcp',
        activeVersion: manifest.version,
        versions,
        lastIssuedAt: manifest.issuedAt,
      };
      await writeJsonAtomic(this.statePath, state);
      return installed;
    } finally {
      await rm(transaction, { recursive: true, force: true });
      await release();
    }
  }

  async rollback(id: string, version: string): Promise<InstalledArtifactVersion> {
    const release = await this.acquireLock();
    try {
      const state = await this.readState();
      const item = state.packages[id];
      const selected = item?.versions[version];
      if (!item || !selected || !await exists(selected.entrypoint)) {
        throw new Error(`Installed artifact version not found: ${id}@${version}`);
      }
      item.activeVersion = version;
      await writeJsonAtomic(this.statePath, state);
      return selected;
    } finally {
      await release();
    }
  }
}

export interface McpOwnershipConflict {
  name: string;
  owners: Array<'yuanpu' | 'pi-mcp-user' | 'pi-mcp-workspace'>;
}

async function configuredMcpNames(path: string): Promise<string[]> {
  try {
    const value = JSON.parse(await readFile(path, 'utf8')) as { mcpServers?: unknown };
    return value.mcpServers && typeof value.mcpServers === 'object'
      ? Object.entries(value.mcpServers).filter(([, config]) => config && typeof config === 'object'
        && (config as { enabled?: boolean; disabled?: boolean }).enabled !== false
        && (config as { disabled?: boolean }).disabled !== true).map(([name]) => name)
      : [];
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return [];
    throw error;
  }
}

export async function detectMcpOwnershipConflicts(input: {
  yuanpuConnections: readonly string[];
  agentRoot: string;
  workspaceRoot: string;
}): Promise<McpOwnershipConflict[]> {
  const owners = new Map<string, Set<McpOwnershipConflict['owners'][number]>>();
  const add = (name: string, owner: McpOwnershipConflict['owners'][number]) => {
    const set = owners.get(name) ?? new Set();
    set.add(owner);
    owners.set(name, set);
  };
  input.yuanpuConnections.forEach((name) => add(name, 'yuanpu'));
  (await configuredMcpNames(join(input.agentRoot, 'mcp.json')))
    .forEach((name) => add(name, 'pi-mcp-user'));
  (await configuredMcpNames(join(input.workspaceRoot, '.pi', 'mcp.json')))
    .forEach((name) => add(name, 'pi-mcp-workspace'));
  return [...owners.entries()]
    .filter(([, values]) => values.has('yuanpu') && values.size > 1)
    .map(([name, values]) => ({ name, owners: [...values] }));
}
