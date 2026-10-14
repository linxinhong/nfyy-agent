import createClient from 'openapi-fetch';
import type { components, paths } from './schema.gen.js';

export type { components, paths } from './schema.gen.js';
export type HealthResponse = components['schemas']['HealthResponse'];
export type ReadyResponse = components['schemas']['ReadyResponse'];
export type WikiPreviewResponse = components['schemas']['WikiPreviewResponse'];
export type ErrorResponse = components['schemas']['ErrorResponse'];
export type ServerUser = components['schemas']['User'];
export type ServerTeam = components['schemas']['Team'];
export type LoginResponse = components['schemas']['LoginResponse'];
export type ServerResource = components['schemas']['Resource'];
export type ResourceInput = components['schemas']['ResourceInput'];
export type CatalogItem = components['schemas']['CatalogItem'];
export type CatalogSearchResponse = components['schemas']['CatalogSearchResponse'];
export type ServerAPIKey = components['schemas']['ApiKey'];
export type IssuedAPIKey = components['schemas']['IssuedApiKey'];

export interface ServerClientOptions {
  baseUrl: string;
  timeoutMs?: number;
  fetch?: typeof globalThis.fetch;
  token?: string;
}

export interface RequestOptions {
  signal?: AbortSignal;
}

export class ServerApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.name = 'ServerApiError';
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

/** Typed client for public status, account, and knowledge-center operations. */
export function createServerClient(options: ServerClientOptions) {
  const url = new URL(options.baseUrl);
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    throw new TypeError('baseUrl must be an HTTP(S) URL without credentials, query, or fragment');
  }
  const timeoutMs = options.timeoutMs ?? 10_000;
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs <= 0 || timeoutMs > 300_000) {
    throw new RangeError('timeoutMs must be an integer between 1 and 300000');
  }
  const client = createClient<paths>({
    baseUrl: url.toString().replace(/\/$/, ''),
    fetch: options.fetch,
  });
  const requestSignal = (signal?: AbortSignal) => {
    const timeout = AbortSignal.timeout(timeoutMs);
    return signal ? AbortSignal.any([signal, timeout]) : timeout;
  };
  const headers: Record<string, string> = { accept: 'application/json' };
  if (options.token) headers.authorization = `Bearer ${options.token}`;
  const responseError = (response: Response, error: ErrorResponse | undefined, fallback: string) => {
    const detail = error?.error;
    return new ServerApiError(response.status, detail?.code ?? 'HTTP_ERROR', detail?.message ?? fallback, response.headers.get('x-request-id') ?? undefined);
  };

  return {
    async health(request: RequestOptions = {}): Promise<HealthResponse> {
      const { data, response } = await client.GET('/v1/health', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok) throw new ServerApiError(response.status, 'HTTP_ERROR', 'Health request failed', response.headers.get('x-request-id') ?? undefined);
      if (!data || data.status !== 'ok' || data.service !== 'yuanpu-server' || data.apiVersion !== 1 || typeof data.version !== 'string') {
        throw new ServerApiError(response.status, 'INVALID_RESPONSE', 'Server returned an incompatible health response');
      }
      return data;
    },
    async ready(request: RequestOptions = {}): Promise<ReadyResponse> {
      const { data, error, response } = await client.GET('/v1/ready', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok) {
        const detail = error?.error;
        throw new ServerApiError(
          response.status,
          typeof detail?.code === 'string' ? detail.code : 'HTTP_ERROR',
          typeof detail?.message === 'string' ? detail.message : 'Readiness request failed',
          response.headers.get('x-request-id') ?? undefined,
        );
      }
      if (!data || data.status !== 'ready' || !Number.isInteger(data.schemaVersion) || data.schemaVersion < 1) {
        throw new ServerApiError(response.status, 'INVALID_RESPONSE', 'Server returned an incompatible readiness response');
      }
      return data;
    },
    async wikiPreview(request: RequestOptions = {}): Promise<WikiPreviewResponse> {
      const { data, response } = await client.GET('/v1/preview/wiki', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok) throw new ServerApiError(response.status, 'HTTP_ERROR', 'Wiki preview request failed', response.headers.get('x-request-id') ?? undefined);
      if (!data || data.preview !== true || !Array.isArray(data.entries) || !data.entries.every((entry) => (
        entry && typeof entry.id === 'string' && typeof entry.title === 'string'
        && typeof entry.summary === 'string' && typeof entry.body === 'string'
        && typeof entry.level === 'string' && Number.isSafeInteger(entry.experience)
        && entry.experience >= 0 && typeof entry.updatedAt === 'string'
        && Array.isArray(entry.links) && entry.links.every((link) => typeof link === 'string')
      ))) {
        throw new ServerApiError(response.status, 'INVALID_RESPONSE', 'Server returned an incompatible wiki preview response');
      }
      return data;
    },
    async searchCatalog(query = '', request: RequestOptions = {}): Promise<CatalogSearchResponse> {
      const { data, error, response } = await client.GET('/v1/catalog/search', {
        params: { query: { q: query } }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Catalog search failed');
      if (data.schemaVersion !== 1 || !Array.isArray(data.items) || !data.items.every((item) =>
        typeof item.id === 'string' && typeof item.name === 'string' && typeof item.displayName === 'string'
        && typeof item.description === 'string' && typeof item.version === 'string'
        && typeof item.source === 'string' && Array.isArray(item.components)
        && Array.isArray(item.permissions))) {
        throw new ServerApiError(response.status, 'INVALID_RESPONSE', 'Server returned an incompatible catalog response');
      }
      return data;
    },
    async getCatalogItem(id: string, request: RequestOptions = {}): Promise<CatalogItem> {
      const { data, error, response } = await client.GET('/v1/catalog/items/{id}', {
        params: { path: { id } }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Catalog item lookup failed');
      return data;
    },
    async login(email: string, password: string, request: RequestOptions = {}): Promise<LoginResponse> {
      const { data, error, response } = await client.POST('/v1/auth/login', {
        body: { email, password }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Login failed');
      return data;
    },
    async loginWithAPIKey(apiKey: string, request: RequestOptions = {}): Promise<LoginResponse> {
      const { data, error, response } = await client.POST('/v1/auth/api-key', {
        body: { apiKey }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'API key login failed');
      return data;
    },
    async me(request: RequestOptions = {}): Promise<ServerUser> {
      const { data, error, response } = await client.GET('/v1/auth/me', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Member lookup failed');
      return data;
    },
    async logout(request: RequestOptions = {}): Promise<void> {
      const { error, response } = await client.POST('/v1/auth/logout', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok) throw responseError(response, error, 'Logout failed');
    },
    async createTeam(name: string, request: RequestOptions = {}): Promise<ServerTeam> {
      const { data, error, response } = await client.POST('/v1/admin/teams', {
        body: { name }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Team creation failed');
      return data;
    },
    async listTeams(request: RequestOptions = {}): Promise<ServerTeam[]> {
      const { data, error, response } = await client.GET('/v1/admin/teams', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Team list failed');
      return data;
    },
    async listUsers(request: RequestOptions = {}): Promise<ServerUser[]> {
      const { data, error, response } = await client.GET('/v1/admin/users', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Member list failed');
      return data;
    },
    async createUser(input: components['schemas']['CreateUserRequest'], request: RequestOptions = {}): Promise<components['schemas']['CreateUserResponse']> {
      const { data, error, response } = await client.POST('/v1/admin/users', {
        body: input, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Member creation failed');
      return data;
    },
    async updateUser(id: string, input: components['schemas']['UpdateUserRequest'], request: RequestOptions = {}): Promise<ServerUser> {
      const { data, error, response } = await client.PUT('/v1/admin/users/{id}', {
        params: { path: { id } }, body: input, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Member update failed');
      return data;
    },
    async listAPIKeys(request: RequestOptions = {}): Promise<ServerAPIKey[]> {
      const { data, error, response } = await client.GET('/v1/admin/api-keys', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'API key list failed');
      return data;
    },
    async issueAPIKey(input: components['schemas']['IssueApiKeyRequest'], request: RequestOptions = {}): Promise<IssuedAPIKey> {
      const { data, error, response } = await client.POST('/v1/admin/api-keys', {
        body: input, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'API key issuance failed');
      return data;
    },
    async revokeAPIKey(id: string, request: RequestOptions = {}): Promise<void> {
      const { error, response } = await client.DELETE('/v1/admin/api-keys/{id}', {
        params: { path: { id } }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok) throw responseError(response, error, 'API key revocation failed');
    },
    async listResources(query: { kind?: ResourceInput['kind']; q?: string } = {}, request: RequestOptions = {}): Promise<ServerResource[]> {
      const { data, error, response } = await client.GET('/v1/resources', {
        params: { query }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Resource list failed');
      return data;
    },
    async getResource(id: string, request: RequestOptions = {}): Promise<ServerResource> {
      const { data, error, response } = await client.GET('/v1/resources/{id}', {
        params: { path: { id } }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Resource lookup failed');
      return data;
    },
    async createResource(input: ResourceInput, request: RequestOptions = {}): Promise<ServerResource> {
      const { data, error, response } = await client.POST('/v1/resources', {
        body: input, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Resource creation failed');
      return data;
    },
    async editResource(id: string, input: ResourceInput, request: RequestOptions = {}): Promise<ServerResource> {
      const { data, error, response } = await client.PUT('/v1/resources/{id}', {
        params: { path: { id } }, body: input, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Resource edit failed');
      return data;
    },
    async submitResource(id: string, request: RequestOptions = {}): Promise<ServerResource> {
      const { data, error, response } = await client.POST('/v1/resources/{id}/submit', {
        params: { path: { id } }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Resource submission failed');
      return data;
    },
    async approveResource(id: string, request: RequestOptions = {}): Promise<ServerResource> {
      const { data, error, response } = await client.POST('/v1/resources/{id}/approve', {
        params: { path: { id } }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Resource approval failed');
      return data;
    },
    async rejectResource(id: string, request: RequestOptions = {}): Promise<ServerResource> {
      const { data, error, response } = await client.POST('/v1/resources/{id}/reject', {
        params: { path: { id } }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Resource rejection failed');
      return data;
    },
    async withdrawResource(id: string, request: RequestOptions = {}): Promise<ServerResource> {
      const { data, error, response } = await client.POST('/v1/resources/{id}/withdraw', {
        params: { path: { id } }, signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Resource withdrawal failed');
      return data;
    },
    async reviewQueue(request: RequestOptions = {}): Promise<ServerResource[]> {
      const { data, error, response } = await client.GET('/v1/review/queue', {
        signal: requestSignal(request.signal), headers, redirect: 'error',
      });
      if (!response.ok || !data) throw responseError(response, error, 'Review queue failed');
      return data;
    },
  };
}
