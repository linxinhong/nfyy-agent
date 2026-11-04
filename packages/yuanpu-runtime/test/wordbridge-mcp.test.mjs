import assert from 'node:assert/strict';
import test from 'node:test';
import { builtinWordBridgeServer } from '../dist/index.mjs';

const token = 'a'.repeat(64);

test('WordBridge registers as a loopback MCP server whose token stays an environment reference', () => {
  const server = builtinWordBridgeServer({
    YUANPU_WORDBRIDGE_MCP_URL: 'http://127.0.0.1:9877/mcp',
    YUANPU_WORDBRIDGE_MCP_TOKEN: token,
  });
  assert.deepEqual(server, {
    value: {
      url: 'http://127.0.0.1:9877/mcp',
      headers: { Authorization: 'Bearer ${YUANPU_WORDBRIDGE_MCP_TOKEN}' },
      exposure: 'deferred',
    },
    source: 'builtin:wordbridge',
    scope: 'global',
  });
  assert.equal(JSON.stringify(server).includes(token), false);
});

test('WordBridge stays unregistered without a token or with a non-loopback endpoint', () => {
  assert.equal(builtinWordBridgeServer({}), undefined);
  assert.equal(builtinWordBridgeServer({ YUANPU_WORDBRIDGE_MCP_URL: 'http://127.0.0.1:9877/mcp' }), undefined);
  for (const url of ['https://127.0.0.1:9877/mcp', 'http://192.168.1.2:9877/mcp', 'http://evil.example/mcp', 'not a url']) {
    assert.equal(builtinWordBridgeServer({ YUANPU_WORDBRIDGE_MCP_URL: url, YUANPU_WORDBRIDGE_MCP_TOKEN: token }), undefined, url);
  }
});
