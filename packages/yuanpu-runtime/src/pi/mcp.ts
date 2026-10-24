import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createMcpExtension, createToolSearchExtension,
  type InlineExtension, type LoadedMcpConfig, type McpServerConfig,
  type McpServerEntry, type SettingsManager } from '@earendil-works/pi-coding-agent';

export const BUILTIN_MCP_NAME = 'pi-mcp';
export const BUILTIN_MCP_VERSION = '1.4.0';

/** MCP uses Pi's deferred tool search; no extra JS VM/worker is needed in the SEA. */
export function builtinMcpExtensions(agentDir: string, cwd: string,
  settings: SettingsManager): InlineExtension[] {
  return [
    { name: 'tool-search', factory: createToolSearchExtension(), builtin: true },
    { name: 'mcp', builtin: true, factory(api) {
      const errors: string[] = [];
      const servers = new Map<string, { value: Record<string, unknown>; source: string; scope: 'global' | 'project' }>();
      for (const [scope, path] of [
        ['global', join(agentDir, 'mcp.json')],
        ...(settings.isProjectTrusted() ? [['project', join(cwd, '.pi', 'mcp.json')]] : []),
      ] as Array<['global' | 'project', string]>) {
        try {
          const document: unknown = JSON.parse(readFileSync(path, 'utf8'));
          if (!document || typeof document !== 'object' || Array.isArray(document)) throw new Error('Expected an MCP configuration object.');
          const entries = (document as { mcpServers?: unknown }).mcpServers ?? {};
          if (!entries || typeof entries !== 'object' || Array.isArray(entries)) throw new Error('Expected mcpServers to be an object.');
          for (const [name, raw] of Object.entries(entries)) {
            if (!raw || typeof raw !== 'object' || Array.isArray(raw)) { errors.push(`${path}: invalid server ${name}`); continue; }
            let value = { ...raw } as Record<string, unknown>;
            if (scope === 'project' && value.command === undefined && value.url === undefined && value.type === undefined) {
              const base = servers.get(name);
              if (!base || Object.keys(value).some((key) => !['enabled', 'exposure', 'toolExposure'].includes(key))) {
                errors.push(`${path}: invalid override for ${name}`);
                continue;
              }
              value = { ...base.value, ...value };
            } else if (scope === 'project' && value.auth !== undefined) {
              errors.push(`${path}: provider credentials are only allowed in user MCP configuration`);
              continue;
            }
            if (value.disabled === true) value.enabled = false;
            const environmentReference = (text: unknown) => typeof text === 'string'
              ? text.replace(/\$env:([A-Za-z_][A-Za-z0-9_]*)/g, (_match, name: string) => `\${${name}}`) : text;
            for (const field of ['env', 'headers']) {
              const values = value[field];
              if (values && typeof values === 'object' && !Array.isArray(values)) {
                value[field] = Object.fromEntries(Object.entries(values).map(([key, text]) => [key, environmentReference(text)]));
              }
            }
            if (value.oauth && typeof value.oauth === 'object' && !Array.isArray(value.oauth)) {
              const oauth = value.oauth as Record<string, unknown>;
              value.oauth = { ...oauth, ...(oauth.clientSecret === undefined ? {}
                : { clientSecret: environmentReference(oauth.clientSecret) }) };
            }
            // Both explicit and implicit codemode exposure retain discovery and invocation via tool_search.
            const exposure = (mode: unknown) => mode === undefined || mode === 'codemode' || mode === 'codemode-deferred'
              ? 'deferred' : mode;
            value.exposure = exposure(value.exposure);
            if (value.toolExposure && typeof value.toolExposure === 'object' && !Array.isArray(value.toolExposure)) {
              value.toolExposure = Object.fromEntries(Object.entries(value.toolExposure)
                .map(([tool, mode]) => [tool, exposure(mode)]));
            }
            servers.set(name, { value, source: path, scope });
          }
        } catch (error) {
          if ((error as NodeJS.ErrnoException).code !== 'ENOENT') errors.push(`${path}: ${String(error)}`);
        }
      }
      const entries: McpServerEntry[] = [];
      for (const [name, { value, source, scope }] of servers) {
        try {
          if (entries.some((entry) => entry.name.replaceAll('-', '_') === name.replaceAll('-', '_'))) {
            throw new Error(`Conflicting MCP namespace: ${name}`);
          }
          // Pi validates names, credentials, transports and field types through its public API.
          api.registerMcpServer(name, value as unknown as McpServerConfig);
          const registered = api.getMcpServers().find((server) => server.name === name)!;
          entries.push({ name, config: registered.config, source, scope });
        } catch (error) { errors.push(`${source}: ${String(error)}`); }
      }
      const loaded: LoadedMcpConfig = { servers: entries, errors, autoEnableCodemode: false };
      return createMcpExtension({ loadConfig: () => loaded, logPath: join(agentDir, 'mcp.log') })(api);
    } },
  ];
}
