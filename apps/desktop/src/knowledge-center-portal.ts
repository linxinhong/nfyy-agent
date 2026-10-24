import { normalizeServerCenterEndpoint } from './server-center-endpoint.js';

export interface KnowledgeCenterProfile {
  user: { id: string; display_name: string; email: string; organization_id?: string; department_id?: string };
  workspaces: Array<{ id: string; name: string; default?: boolean; roles?: string[] }>;
  default_workspace: string;
}

export interface PortalRequest {
  path: string;
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  body?: string;
  workspace?: string;
  binary?: boolean;
}

export interface PortalResponse {
  status: number;
  body: unknown;
  contentType: string;
}

function centerURL(endpoint: string, path: string): URL {
  if (typeof path !== 'string' || !path.startsWith('/enterprise/v1/') || path.startsWith('//') || path.includes('#')) {
    throw new TypeError('Invalid center request path');
  }
  const base = normalizeServerCenterEndpoint(endpoint);
  const url = new URL(path, base);
  if (url.origin !== new URL(base).origin || !url.pathname.startsWith('/enterprise/v1/')) {
    throw new TypeError('Invalid center request path');
  }
  return url;
}

/** Reads the same profile as browser login, using the key only in the trusted host. */
export async function getKnowledgeCenterProfile(endpoint: string, key: string): Promise<KnowledgeCenterProfile | null> {
  const response = await fetch(centerURL(endpoint, '/enterprise/v1/me'), {
    headers: { authorization: `Bearer ${key}` },
    redirect: 'error',
    signal: AbortSignal.timeout(8_000),
  });
  if (response.status === 401 || response.status === 403) return null;
  if (!response.ok) throw new Error(`Center profile failed: ${response.status}`);
  const profile = await response.json() as KnowledgeCenterProfile;
  if (!profile || typeof profile.user?.id !== 'string' || typeof profile.user.email !== 'string'
    || !Array.isArray(profile.workspaces) || typeof profile.default_workspace !== 'string') {
    throw new Error('Invalid center profile');
  }
  return profile;
}

/** Forwards only same-origin enterprise API calls. The raw key never reaches the guest page. */
export async function forwardKnowledgeCenterRequest(endpoint: string, key: string, input: PortalRequest): Promise<PortalResponse> {
  const method = input.method ?? 'GET';
  if (!['GET', 'POST', 'PUT', 'PATCH', 'DELETE'].includes(method)
    || (input.body !== undefined && (typeof input.body !== 'string' || input.body.length > 2_000_000))
    || (input.workspace !== undefined && (typeof input.workspace !== 'string' || input.workspace.length > 200))) {
    throw new TypeError('Invalid center request');
  }
  const response = await fetch(centerURL(endpoint, input.path), {
    method,
    headers: {
      authorization: `Bearer ${key}`,
      ...(input.body !== undefined ? { 'content-type': 'application/json' } : {}),
      ...(input.workspace ? { 'x-agent-workspace': input.workspace } : {}),
    },
    body: input.body,
    redirect: 'error',
    signal: AbortSignal.timeout(15_000),
  });
  const contentType = response.headers.get('content-type') ?? '';
  if (input.binary && response.ok) {
    const bytes = await response.arrayBuffer();
    if (bytes.byteLength > 25_000_000) throw new Error('Center download exceeds size limit');
    return { status: response.status, body: Buffer.from(bytes).toString('base64'), contentType };
  }
  if (response.status === 204) return { status: response.status, body: null, contentType };
  const text = await response.text();
  let body: unknown = text;
  if (contentType.includes('application/json')) {
    try { body = JSON.parse(text); } catch { /* Keep the response text for error reporting. */ }
  }
  return { status: response.status, body, contentType };
}
