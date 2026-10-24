/** A center connection lives only in the trusted desktop and Runtime processes. */
export interface CenterConnection {
  endpoint: string;
  apiKey?: string;
}

export function parseCenterConnection(value: unknown): CenterConnection {
  if (!value || typeof value !== 'object') throw new Error('中心连接配置无效。');
  const input = value as { endpoint?: unknown; apiKey?: unknown };
  if (typeof input.endpoint !== 'string' || input.endpoint.length > 2048 || input.endpoint.trim() !== input.endpoint) {
    throw new Error('中心端点无效。');
  }
  let url: URL;
  try { url = new URL(input.endpoint); }
  catch { throw new Error('中心端点无效。'); }
  if (!url.hostname || url.username || url.password || url.search || url.hash ||
    (url.protocol !== 'https:' && !(url.protocol === 'http:' &&
      ['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname)))) {
    throw new Error('中心端点无效。');
  }
  if (input.apiKey !== undefined && (typeof input.apiKey !== 'string' ||
    !/^agk_(?:live|test)_[A-Za-z0-9]{32}$/.test(input.apiKey))) {
    throw new Error('中心 API key 无效。');
  }
  return { endpoint: url.origin, ...(input.apiKey ? { apiKey: input.apiKey } : {}) };
}

async function centerGet(connection: CenterConnection, path: string, workspace?: string): Promise<unknown> {
  const response = await fetch(new URL(path, connection.endpoint), {
    headers: { authorization: `Bearer ${connection.apiKey}`,
      ...(workspace ? { 'X-Agent-Workspace': workspace } : {}) },
    redirect: 'error',
    signal: AbortSignal.timeout(8_000),
  });
  if (!response.ok) throw new Error(`知识服务中心请求失败（HTTP ${response.status}）。`);
  return response.json();
}

export async function centerWorkspace(connection: CenterConnection): Promise<string> {
  const result = await centerGet(connection, '/enterprise/v1/me') as {
    default_workspace?: unknown; workspaces?: unknown;
  };
  if (typeof result.default_workspace !== 'string' ||
    !Array.isArray(result.workspaces) ||
    !result.workspaces.some((item) => item && typeof item === 'object' &&
      (item as { id?: unknown }).id === result.default_workspace)) {
    throw new Error('知识服务中心未返回有效的工作空间。');
  }
  return result.default_workspace;
}

/** Only names cross the client boundary. Protected SKILL.md remains on the gateway. */
export async function activeProtectedSkills(connection: CenterConnection, workspace: string): Promise<string[]> {
  if (!connection.apiKey) return [];
  const result = await centerGet(connection, '/enterprise/v1/me/skills', workspace) as { skills?: unknown };
  if (!Array.isArray(result.skills)) throw new Error('知识服务中心技能目录响应无效。');
  const names: string[] = [];
  for (const skill of result.skills) {
    if (!skill || typeof skill !== 'object') throw new Error('知识服务中心技能目录响应无效。');
    const item = skill as Record<string, unknown>;
    if (item.active !== true || item.protected !== true) continue;
    if (typeof item.name !== 'string' || !/^[a-z0-9-]{1,64}$/.test(item.name)) {
      throw new Error('知识服务中心返回了无效的技能名称。');
    }
    names.push(item.name);
  }
  const unique = [...new Set(names)];
  if (unique.length > 16) throw new Error('当前启用的受保护技能超过中心允许的 16 项。');
  return unique;
}

export async function requireCenterModel(connection: CenterConnection, workspace: string, modelId: string): Promise<void> {
  const result = await centerGet(connection, '/v1/models', workspace) as { data?: unknown };
  if (!Array.isArray(result.data)) throw new Error('知识服务中心模型目录响应无效。');
  if (!result.data.some((item) => item && typeof item === 'object' &&
    (item as { id?: unknown }).id === modelId)) {
    throw new Error(`中心尚未配置模型 ${modelId} 的路由，无法安全注入已启用的受保护技能。`);
  }
}
