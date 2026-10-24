import type { ServerCenterResult, ServerCenterUser } from '@yuanpu-agent/protocol';
import { normalizeServerCenterEndpoint } from './server-center-endpoint.js';

const keyPattern = /^agk_(?:live|test)_[A-Za-z0-9]{32}$/;

/** Validate a member key at EAP's employee endpoint without exposing it to the webview. */
export async function checkKnowledgeCenterKey(endpoint: string, key: string): Promise<ServerCenterResult<ServerCenterUser>> {
  if (!keyPattern.test(key)) return { ok: false, code: 'UNAUTHORIZED' };
  let url: string;
  try { url = normalizeServerCenterEndpoint(endpoint); }
  catch { return { ok: false, code: 'UNAVAILABLE' }; }
  try {
    const response = await fetch(new URL('/enterprise/v1/me', url), {
      headers: { authorization: `Bearer ${key}` },
      redirect: 'error',
      signal: AbortSignal.timeout(8_000),
    });
    if (response.status === 401) return { ok: false, code: 'UNAUTHORIZED' };
    if (response.status === 403 || response.status === 404) return { ok: false, code: 'FORBIDDEN' };
    if (!response.ok) return { ok: false, code: 'UNAVAILABLE' };
    const data = await response.json() as { user?: { id?: unknown; email?: unknown; display_name?: unknown };
      workspaces?: Array<{ roles?: unknown }> };
    if (typeof data.user?.id !== 'string' || typeof data.user.email !== 'string'
      || typeof data.user.display_name !== 'string') return { ok: false, code: 'UNAVAILABLE' };
    const roles = Array.isArray(data.workspaces) ? data.workspaces.flatMap((item) => Array.isArray(item.roles) ? item.roles : []) : [];
    return { ok: true, value: { id: data.user.id, email: data.user.email, name: data.user.display_name,
      role: roles.includes('admin') ? 'admin' : 'member' } };
  } catch { return { ok: false, code: 'UNAVAILABLE' }; }
}
