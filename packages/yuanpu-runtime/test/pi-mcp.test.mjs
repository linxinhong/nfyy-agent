import test from 'node:test';
import { createYuanpuChatSession } from '../dist/index.mjs';
import { piMcpScenario } from './fixtures/pi-mcp-scenario.mjs';

test('built-in Pi MCP discovers and calls a deferred stdio tool and closes its process',
  { timeout: 20_000 }, (t) => piMcpScenario(t, createYuanpuChatSession));
