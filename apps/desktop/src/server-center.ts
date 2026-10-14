import { createServerClient, ServerApiError } from '@yuanpu-agent/server-client';
import type {
  ServerCenterCatalogItem, ServerCenterResource, ServerCenterResult, ServerCenterStatus,
  ServerCenterUser, ServerWikiPreview,
} from '@yuanpu-agent/protocol';

export const defaultServerUrl = 'http://127.0.0.1:8788';

function issueCode(error: unknown): string {
  if (error instanceof ServerApiError) return error.code;
  if (error instanceof Error && error.name === 'TimeoutError') return 'TIMEOUT';
  return 'NETWORK_UNAVAILABLE';
}

export async function getServerCenterStatus(baseUrl: string): Promise<ServerCenterStatus> {
  const checkedAt = new Date().toISOString();
  let client: ReturnType<typeof createServerClient>;
  try {
    client = createServerClient({ baseUrl, timeoutMs: 4_000 });
  } catch {
    return { url: '', checkedAt, state: 'incompatible', issueCode: 'INVALID_SERVER_URL' };
  }
  let health;
  try {
    health = await client.health();
  } catch (error) {
    return { url: baseUrl, checkedAt, state: error instanceof ServerApiError ? 'incompatible' : 'offline',
      issueCode: issueCode(error), requestId: error instanceof ServerApiError ? error.requestId : undefined };
  }
  const online: ServerCenterStatus = {
    url: baseUrl, checkedAt, state: 'running', version: health.version, apiVersion: health.apiVersion,
  };
  try {
    const ready = await client.ready();
    return { ...online, state: 'ready', schemaVersion: ready.schemaVersion };
  } catch (error) {
    return { ...online, issueCode: issueCode(error),
      requestId: error instanceof ServerApiError ? error.requestId : undefined };
  }
}

export function getServerWikiPreview(baseUrl: string): Promise<ServerWikiPreview> {
  return createServerClient({ baseUrl, timeoutMs: 4_000 }).wikiPreview();
}

function failure(error: unknown): ServerCenterResult<never> {
  if (error instanceof ServerApiError && error.status === 401) return { ok: false, code: 'UNAUTHORIZED' };
  if (error instanceof ServerApiError && [403, 404].includes(error.status)) return { ok: false, code: 'FORBIDDEN' };
  return { ok: false, code: 'UNAVAILABLE' };
}

function boundedText(value: string, max: number, trim = true): string {
  if (typeof value !== 'string' || value.length > max) throw new TypeError('Invalid center request');
  return trim ? value.trim() : value;
}

/** Keeps the member token in the trusted Electron host, never in renderer state. */
export class ServerCenterSession {
  private token = '';
  private user: ServerCenterUser | null = null;
  private generation = 0;
  private autoLoginSuppressed = false;
  private restoring?: Promise<ServerCenterResult<ServerCenterUser>>;

  constructor(private readonly baseUrl: string, private readonly loadAPIKey?: () => Promise<string | undefined>) {}

  private client(token = '') {
    return createServerClient({ baseUrl: this.baseUrl, token, timeoutMs: 8_000 });
  }

  async searchCatalog(query: string): Promise<ServerCenterCatalogItem[]> {
    return (await this.client().searchCatalog(boundedText(query, 200))).items;
  }

  async useAPIKey(apiKey: string): Promise<ServerCenterResult<ServerCenterUser>> {
    const loginGeneration = ++this.generation;
    try {
      const result = await this.client().loginWithAPIKey(boundedText(apiKey, 256, false));
      if (this.generation !== loginGeneration) return { ok: false, code: 'UNAUTHORIZED' };
      this.token = result.token;
      this.user = result.user;
      this.autoLoginSuppressed = false;
      return { ok: true, value: result.user };
    } catch (error) {
      return failure(error);
    }
  }

  restore(): Promise<ServerCenterResult<ServerCenterUser>> {
    if (this.token && this.user) return Promise.resolve({ ok: true, value: this.user });
    if (this.autoLoginSuppressed || !this.loadAPIKey) return Promise.resolve({ ok: false, code: 'UNAUTHORIZED' });
    if (this.restoring) return this.restoring;
    const generation = this.generation;
    const work = (async (): Promise<ServerCenterResult<ServerCenterUser>> => {
      try {
        const key = await this.loadAPIKey!();
        if (!key) return { ok: false, code: 'UNAUTHORIZED' };
        const result = await this.client().loginWithAPIKey(key);
        if (generation !== this.generation) return { ok: false, code: 'UNAUTHORIZED' };
        this.token = result.token;
        this.user = result.user;
        return { ok: true, value: result.user };
      } catch (error) {
        return failure(error);
      }
    })();
    this.restoring = work;
    void work.finally(() => { if (this.restoring === work) this.restoring = undefined; });
    return work;
  }

  async logout(): Promise<void> {
    const token = this.token;
    this.token = '';
    this.user = null;
    this.autoLoginSuppressed = true;
    this.generation++;
    if (token) {
      try { await this.client(token).logout(); } catch { /* Local logout must always complete. */ }
    }
  }

  private async authorized<T>(request: (token: string) => Promise<T>): Promise<ServerCenterResult<T>> {
    const session = await this.restore();
    if (!session.ok) return session;
    let token = this.token;
    try {
      const value = await request(token);
      if (token !== this.token) return { ok: false, code: 'UNAUTHORIZED' };
      return { ok: true, value };
    } catch (error) {
      if (error instanceof ServerApiError && error.status === 401 && token === this.token) {
        this.token = '';
        this.user = null;
        this.generation++;
        const renewed = await this.restore();
        if (!renewed.ok) return renewed;
        token = this.token;
        try {
          const value = await request(token);
          return token === this.token ? { ok: true, value } : { ok: false, code: 'UNAUTHORIZED' };
        } catch (retryError) {
          return failure(retryError);
        }
      }
      return failure(error);
    }
  }

  listResources(query: string): Promise<ServerCenterResult<ServerCenterResource[]>> {
    const q = boundedText(query, 200);
    return this.authorized((token) => this.client(token).listResources({ q }));
  }

  getResource(id: string): Promise<ServerCenterResult<ServerCenterResource>> {
    const resourceId = boundedText(id, 128);
    if (!resourceId) throw new TypeError('Invalid resource ID');
    return this.authorized((token) => this.client(token).getResource(resourceId));
  }
}
