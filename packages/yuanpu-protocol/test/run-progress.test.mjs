import assert from 'node:assert/strict';
import test from 'node:test';
import { displayToolValue, toolResultText, settleRunProgress } from '../dist/index.mjs';

test('tool display bounds payloads and redacts credential fields and embedded text', () => {
  const value = displayToolValue({ command: 'echo hi', env: { LONGCAT_API_KEY: 'PRIVATE_ONE',
    Authorization: 'Basic PRIVATE_TWO', accessToken: 'PRIVATE_THREE' },
  output: '{"apiKey":"PRIVATE_FOUR", "cookie":"PRIVATE_FIVE session=value"} password=PRIVATE_SIX',
  log: 'Authorization: Bearer PRIVATE_SEVEN', long: 'x'.repeat(20_000) });
  assert.doesNotMatch(value.text, /PRIVATE_/);
  assert.match(value.text, /echo hi/);
  assert.equal(value.truncated, true);
  assert.ok(value.text.length <= 8_000);
  assert.equal(toolResultText([{ type: 'thinking', thinking: 'PRIVATE_THOUGHT' },
    { type: 'image', data: 'PRIVATE_IMAGE' }, { type: 'text', text: 'visible' }]), 'visible');
});

test('unfinished tools are settled without changing completed outcomes', () => {
  const progress = { text: '', steps: [{ id: 'one', status: 'running' }, { id: 'two', status: 'failed' }] };
  assert.deepEqual(settleRunProgress(progress, 'cancelled').steps.map((step) => step.status), ['cancelled', 'failed']);
  assert.equal(progress.steps[0].status, 'running');
});
