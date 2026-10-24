import type { ServerCenterStatus } from '@yuanpu-agent/protocol';
import { normalizeServerCenterEndpoint } from './server-center-endpoint.js';

/** EAP readiness is independent of the retired Go center's /v1/health API. */
export async function getKnowledgeCenterStatus(endpoint: string): Promise<ServerCenterStatus> {
  const checkedAt = new Date().toISOString();
  let url: string;
  try { url = normalizeServerCenterEndpoint(endpoint); }
  catch { return { url: '', checkedAt, state: 'incompatible', issueCode: 'INVALID_SERVER_URL' }; }

  let health: Response;
  try { health = await fetch(new URL('/healthz', url), { signal: AbortSignal.timeout(4_000) }); }
  catch { return { url, checkedAt, state: 'offline', issueCode: 'NETWORK_UNAVAILABLE' }; }
  if (!health.ok) return { url, checkedAt, state: 'incompatible', issueCode: `HTTP_${health.status}` };
  let body: unknown;
  try { body = await health.json(); }
  catch { return { url, checkedAt, state: 'incompatible', issueCode: 'INVALID_HEALTH_RESPONSE' }; }
  if (!body || typeof body !== 'object' || !('status' in body) || body.status !== 'ok'
    || !('version' in body) || typeof body.version !== 'string') {
    return { url, checkedAt, state: 'incompatible', issueCode: 'INVALID_HEALTH_RESPONSE' };
  }
  const running: ServerCenterStatus = { url, checkedAt, state: 'running', version: body.version };
  try {
    const ready = await fetch(new URL('/readyz', url), { signal: AbortSignal.timeout(4_000) });
    const readiness = await ready.json() as { status?: unknown };
    return ready.ok && readiness.status === 'ready'
      ? { ...running, state: 'ready' }
      : { ...running, issueCode: 'DEPENDENCY_UNAVAILABLE' };
  } catch { return { ...running, issueCode: 'READINESS_UNAVAILABLE' }; }
}
