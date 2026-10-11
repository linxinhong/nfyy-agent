import createClient from 'openapi-fetch';
import type { components, paths } from './schema.gen.js';

export type { components, paths } from './schema.gen.js';
export type HealthResponse = components['schemas']['HealthResponse'];
export type ReadyResponse = components['schemas']['ReadyResponse'];
export type ErrorResponse = components['schemas']['ErrorResponse'];

export interface ServerClientOptions {
  baseUrl: string;
  timeoutMs?: number;
  fetch?: typeof globalThis.fetch;
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

/** Public operational client. Authentication is added with the account API. */
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
  const headers = { accept: 'application/json' };

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
  };
}
