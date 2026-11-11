import type { CallToolResult } from '@modelcontextprotocol/sdk/types.js';
import Ajv2020 from 'ajv/dist/2020.js';
import { ManagedMcpSourceError } from './mcp-source.js';

import {
  CAPABILITY_TOOL_NAMES,
  type CapabilityContext,
  type CapabilityAuthorizer,
  type CapabilityDefinition,
  type CapabilityDescriptor,
  type CapabilityFailure,
  type CapabilityRiskLevel,
  type CapabilitySourceExecuteInput,
  type CapabilityToolClient,
  type CapabilityToolDefinition,
  type ExecuteCapabilityInput,
  type ExecuteCapabilityResult,
  type SearchCapabilitiesInput,
  type SearchCapabilitiesResult,
} from './contracts.js';
import { createCapabilityId, parseCapabilityId } from './id.js';

export * from './contracts.js';
export * from './approval.js';
export * from './mcp-source.js';
export { createCapabilityId, parseCapabilityId } from './id.js';

export interface CapabilitySource {
  /** Stable for this configured source, not merely its package display name. */
  readonly sourceInstanceId: string;
  list(context: CapabilityContext): Promise<CapabilityDefinition[]>;
  resolve(
    originalName: string,
    context: CapabilityContext,
  ): Promise<CapabilityDefinition | undefined>;
  execute(
    input: CapabilitySourceExecuteInput,
    context: CapabilityContext,
  ): Promise<CallToolResult | undefined>;
}

export interface CapabilityRegistryOptions {
  discoveryTimeoutMs?: number;
  discoveryCacheTtlMs?: number;
}

export class CapabilityError extends Error {
  readonly failure: CapabilityFailure;

  constructor(failure: CapabilityFailure) {
    super(failure.message);
    this.name = 'CapabilityError';
    this.failure = failure;
  }
}

const riskOrder: Record<CapabilityRiskLevel, number> = {
  R0: 0,
  R1: 1,
  R2: 2,
  R3: 3,
  R4: 4,
  R5: 5,
};

const schemaValidator = new Ajv2020({ allErrors: true, strict: false });

function describe(source: CapabilitySource, definition: CapabilityDefinition): CapabilityDescriptor {
  return {
    ...definition,
    name: createCapabilityId(source.sourceInstanceId, definition.name),
    sourceInstanceId: source.sourceInstanceId,
    originalName: definition.name,
  };
}

function tokenize(value: string): string[] {
  return value.toLocaleLowerCase().split(/[^\p{L}\p{N}]+/u).filter(Boolean);
}

function scoreCapability(capability: CapabilityDescriptor, query: string[]): number {
  const nameTokens = tokenize(`${capability.originalName} ${capability.name}`);
  const descriptionTokens = tokenize(capability.description);
  return query.reduce((score, token) => {
    if (nameTokens.includes(token)) return score + 5;
    if (nameTokens.some((candidate) => candidate.startsWith(token))) return score + 3;
    if (descriptionTokens.includes(token)) return score + 2;
    if (descriptionTokens.some((candidate) => candidate.includes(token))) return score + 1;
    return score;
  }, 0);
}

function validateArguments(capability: CapabilityDescriptor, value: unknown): void {
  let validate;
  try {
    validate = schemaValidator.compile(capability.inputSchema);
  } catch (error) {
    throw new CapabilityError({
      error: 'execution_failed',
      message: `Capability ${capability.name} published an invalid input schema: ${error instanceof Error ? error.message : String(error)}`,
      retry: { search: false, action: 'contact_admin' },
    });
  }
  if (!validate(value ?? {})) {
    const detail = schemaValidator.errorsText(validate.errors, { separator: '; ' });
    throw new CapabilityError({
      error: 'invalid_arguments',
      message: `Arguments for ${capability.name} do not match its schema: ${detail}`,
      retry: { search: false, action: 'correct_arguments' },
    });
  }
}

export class CapabilityRegistry implements CapabilityToolClient {
  readonly #sources: Map<string, CapabilitySource>;
  readonly #authorizer?: CapabilityAuthorizer;
  readonly #options: Required<CapabilityRegistryOptions>;
  readonly #discoveryCache = new Map<string, {
    contextKey: string;
    expiresAt: number;
    definitions: CapabilityDefinition[];
  }>();
  readonly #discoveries = new Map<string, {
    promise: Promise<CapabilityDefinition[]>;
    abort: AbortController;
  }>();

  constructor(
    sources: CapabilitySource[] = [],
    authorizer?: CapabilityAuthorizer,
    options: CapabilityRegistryOptions = {},
  ) {
    this.#sources = new Map();
    this.#authorizer = authorizer;
    this.#options = {
      discoveryTimeoutMs: options.discoveryTimeoutMs ?? (process.platform === 'win32' ? 90_000 : 10_000),
      discoveryCacheTtlMs: options.discoveryCacheTtlMs ?? 5_000,
    };
    for (const source of sources) {
      if (!source.sourceInstanceId.trim()) throw new Error('Capability sourceInstanceId must be non-empty.');
      if (this.#sources.has(source.sourceInstanceId)) {
        throw new Error(`Duplicate capability source instance: ${source.sourceInstanceId}`);
      }
      this.#sources.set(source.sourceInstanceId, source);
    }
  }

  #discover(
    source: CapabilitySource,
    context: CapabilityContext,
    contextKey: string,
  ): { promise: Promise<CapabilityDefinition[]>; abort: AbortController } {
    const key = `${source.sourceInstanceId}\0${contextKey}`;
    const existing = this.#discoveries.get(key);
    if (existing) return existing;

    const abort = new AbortController();
    const entry = {
      abort,
      promise: Promise.resolve([]) as Promise<CapabilityDefinition[]>,
    };
    let timeout: NodeJS.Timeout | undefined;
    entry.promise = Promise.race([
      source.list({ ...context, signal: abort.signal }),
      new Promise<never>((_resolve, reject) => {
        timeout = setTimeout(() => {
          abort.abort(new Error('Capability discovery timed out.'));
          reject(new ManagedMcpSourceError(
            'timeout',
            `Capability discovery timed out for ${source.sourceInstanceId}.`,
          ));
        }, this.#options.discoveryTimeoutMs);
        timeout.unref?.();
      }),
    ]).finally(() => {
      if (timeout) clearTimeout(timeout);
      if (this.#discoveries.get(key) === entry) this.#discoveries.delete(key);
    });
    this.#discoveries.set(key, entry);
    return entry;
  }

  async search(
    input: SearchCapabilitiesInput,
    context: CapabilityContext = {},
  ): Promise<SearchCapabilitiesResult> {
    const limit = Math.min(Math.max(Math.trunc(input.limit ?? 5), 1), 20);
    const query = tokenize(input.query ?? '');
    const contextKey = JSON.stringify([
      context.sessionId ?? '',
      context.workspaceId ?? '',
      context.userId ?? '',
      [...(context.roles ?? [])].sort(),
    ]);
    const failures: SearchCapabilitiesResult['failures'] = [];
    const groups = await Promise.all([...this.#sources.values()].map(async (source) => {
      const cached = this.#discoveryCache.get(source.sourceInstanceId);
      if (cached && cached.contextKey === contextKey && cached.expiresAt > Date.now()) {
        return cached.definitions.map((definition) => describe(source, definition));
      }
      let abortListener: (() => void) | undefined;
      try {
        const discovery = this.#discover(source, context, contextKey);
        const cancellation = context.signal
          ? new Promise<never>((_resolve, reject) => {
              abortListener = () => reject(new ManagedMcpSourceError(
                'cancelled',
                'Capability discovery was cancelled.',
              ));
              if (context.signal?.aborted) abortListener();
              else context.signal?.addEventListener('abort', abortListener, { once: true });
            })
          : new Promise<never>(() => undefined);
        const definitions = await Promise.race([
          discovery.promise,
          cancellation,
        ]);
        this.#discoveryCache.set(source.sourceInstanceId, {
          contextKey,
          expiresAt: Date.now() + this.#options.discoveryCacheTtlMs,
          definitions,
        });
        return definitions.map((definition) => describe(source, definition));
      } catch (error) {
        if (error instanceof ManagedMcpSourceError && error.code === 'cancelled') throw error;
        failures.push({
          sourceInstanceId: source.sourceInstanceId,
          error: error instanceof ManagedMcpSourceError && error.code === 'timeout'
            ? 'timeout'
            : 'unavailable',
          message: error instanceof Error ? error.message : String(error),
        });
        return [];
      } finally {
        if (abortListener) context.signal?.removeEventListener('abort', abortListener);
      }
    }));
    const capabilities = groups.flat();

    const matches = query.length === 0
      ? capabilities.sort((left, right) => left.name.localeCompare(right.name)).slice(0, limit)
      : capabilities
          .map((capability) => ({ capability, score: scoreCapability(capability, query) }))
          .filter((item) => item.score > 0)
          .sort((left, right) => right.score - left.score || left.capability.name.localeCompare(right.capability.name))
          .slice(0, limit)
          .map((item) => item.capability);

    return {
      matches,
      ...(failures.length ? { failures } : {}),
      ...(matches.length === 0 ? { hint: 'No capability matched. Try broader keywords.' } : {}),
    };
  }

  async execute(
    input: ExecuteCapabilityInput,
    context: CapabilityContext = {},
  ): Promise<ExecuteCapabilityResult> {
    const route = parseCapabilityId(input.name);
    if (!route) {
      throw new CapabilityError({
        error: 'invalid_arguments',
        message: 'Capability name must be the exact opaque id returned by search_capabilities.',
        retry: { search: true, action: 'correct_arguments' },
      });
    }
    const source = this.#sources.get(route.sourceInstanceId);
    let definition: CapabilityDefinition | undefined;
    try {
      definition = await source?.resolve(route.originalName, context);
    } catch (error) {
      if (error instanceof ManagedMcpSourceError) {
        throw new CapabilityError({
          error: error.code === 'unavailable' ? 'execution_failed' : error.code,
          message: error.message,
          retry: { search: error.code === 'unavailable' },
        });
      }
      throw error;
    }
    if (!source || !definition) {
      throw new CapabilityError({
        error: 'unknown_capability',
        message: `No capability named ${input.name}.`,
        retry: { search: true },
      });
    }
    const capability = describe(source, definition);
    if (capability.status === 'denied_by_policy' || capability.status === 'disabled') {
      throw new CapabilityError({
        error: 'policy_blocked',
        message: `${input.name} is not available under the current policy.`,
        retry: { search: false, action: 'contact_admin' },
      });
    }
    validateArguments(capability, input.arguments ?? {});
    if (capability.status === 'needs_approval' || riskOrder[capability.riskLevel] >= riskOrder.R2) {
      if (!capability.packageVersion?.trim()) {
        throw new CapabilityError({
          error: 'policy_blocked',
          message: `${input.name} is sensitive and does not declare an immutable package version.`,
          retry: { search: false, action: 'contact_admin' },
        });
      }
      if (!this.#authorizer) {
        throw new CapabilityError({
          error: 'needs_approval',
          message: `${input.name} requires host approval, but no host authorizer is available.`,
          retry: { search: false, action: 'request_approval' },
        });
      }
      const authorization = await this.#authorizer.authorize({
        approvalRequestId: input.approvalRequestId,
        runId: context.runId,
        sessionId: context.sessionId,
        conversationId: context.conversationId,
        workspaceId: context.workspaceId,
        sourceInstanceId: capability.sourceInstanceId,
        packageVersion: capability.packageVersion,
        capabilityId: capability.name,
        arguments: input.arguments ?? {},
      });
      if (authorization.status === 'pending') {
        throw new CapabilityError({
          error: 'needs_approval',
          message: `${input.name} requires host approval before execution.`,
          retry: { search: false, action: 'request_approval' },
          approvalRequestId: authorization.requestId,
        });
      }
      if (authorization.status === 'invalid') {
        throw new CapabilityError({
          error: 'approval_invalid',
          message: authorization.message,
          retry: { search: false, action: 'request_approval' },
        });
      }
    }

    let result: CallToolResult | undefined;
    try {
      result = await source.execute({
        capabilityId: capability.name,
        originalName: capability.originalName,
        arguments: input.arguments,
      }, context);
    } catch (error) {
      if (error instanceof ManagedMcpSourceError) {
        throw new CapabilityError({
          error: error.code === 'unavailable' ? 'execution_failed' : error.code,
          message: error.message,
          retry: { search: error.code === 'unavailable' },
        });
      }
      throw error;
    }
    if (result) {
      return {
        ...result,
        capability: capability.name,
        sourceInstanceId: capability.sourceInstanceId,
        riskLevel: capability.riskLevel,
      };
    }

    throw new CapabilityError({
      error: 'unknown_capability',
      message: `Capability source ${source.sourceInstanceId} no longer accepts ${route.originalName}.`,
      retry: { search: true },
    });
  }
}

export const YUANPU_MCP_TOOLS: readonly CapabilityToolDefinition[] = [
  {
    name: CAPABILITY_TOOL_NAMES.search,
    description: 'Find MindLink-managed external capabilities available in the current workspace.',
    inputSchema: {
      type: 'object',
      properties: {
        query: { type: 'string', description: 'Keywords describing the required capability.' },
        limit: { type: 'integer', minimum: 1, maximum: 20 },
      },
      additionalProperties: false,
    },
  },
  {
    name: CAPABILITY_TOOL_NAMES.execute,
    description: 'Execute a capability by the exact opaque id returned from search_capabilities.',
    inputSchema: {
      type: 'object',
      required: ['name'],
      properties: {
        name: { type: 'string', minLength: 1 },
        arguments: { type: 'object' },
        approvalRequestId: { type: 'string', description: 'Host-issued pending approval request id.' },
      },
      additionalProperties: false,
    },
  },
] as const;

export class YuanpuMcpServer implements CapabilityToolClient {
  readonly #registry: CapabilityRegistry;

  constructor(
    sources: CapabilitySource[] = [],
    authorizer?: CapabilityAuthorizer,
    options?: CapabilityRegistryOptions,
  ) {
    this.#registry = new CapabilityRegistry(sources, authorizer, options);
  }

  listTools(): readonly CapabilityToolDefinition[] {
    return YUANPU_MCP_TOOLS;
  }

  search(input: SearchCapabilitiesInput, context: CapabilityContext = {}): Promise<SearchCapabilitiesResult> {
    return this.#registry.search(input, context);
  }

  execute(input: ExecuteCapabilityInput, context: CapabilityContext = {}): Promise<ExecuteCapabilityResult> {
    return this.#registry.execute(input, context);
  }

  callTool(
    name: string,
    input: SearchCapabilitiesInput | ExecuteCapabilityInput,
    context: CapabilityContext = {},
  ): Promise<SearchCapabilitiesResult | ExecuteCapabilityResult> {
    if (name === CAPABILITY_TOOL_NAMES.search) return this.search(input as SearchCapabilitiesInput, context);
    if (name === CAPABILITY_TOOL_NAMES.execute) return this.execute(input as ExecuteCapabilityInput, context);
    return Promise.reject(new CapabilityError({
      error: 'unknown_capability',
      message: `The MCP server does not expose a tool named ${name}.`,
      retry: { search: false },
    }));
  }
}

export function createYuanpuMcpServer(
  sources: CapabilitySource[] = [],
  authorizer?: CapabilityAuthorizer,
  options?: CapabilityRegistryOptions,
): YuanpuMcpServer {
  return new YuanpuMcpServer(sources, authorizer, options);
}

const echoCapability: CapabilityDefinition = {
  name: 'yuanpu.echo',
  description: 'Echo text back unchanged. Use this capability to verify the external execution path.',
  type: 'mcp_tool',
  riskLevel: 'R0',
  status: 'available',
  inputSchema: {
    type: 'object',
    required: ['text'],
    properties: { text: { type: 'string', description: 'Text to echo.' } },
    additionalProperties: false,
  },
};

export function createDemoCapabilitySource(): CapabilitySource {
  return {
    sourceInstanceId: 'builtin.demo',
    async list() {
      return [echoCapability];
    },
    async resolve(name) {
      return name === echoCapability.name ? echoCapability : undefined;
    },
    async execute(input) {
      if (input.originalName !== echoCapability.name) return undefined;
      const text = input.arguments?.text;
      return {
        content: [{ type: 'text', text: String(text) }],
        structuredContent: { text },
      };
    },
  };
}
