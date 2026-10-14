import assert from 'node:assert/strict';
import test from 'node:test';
import { restoreWorkRunOutcomes } from '../src/work-transcript.ts';

test('cancelled Runtime outcome survives Pi error records and keeps the actual tool result', () => {
  const messages = [{ id: 'reply', role: 'assistant', text: '', run: { runId: 'run', status: 'failed',
    steps: [{ id: 'tool', status: 'failed', result: 'Command aborted' }] } }];
  const run = { status: 'cancelled', createdAt: 'start', updatedAt: 'end', owner: { entryPoint: 'desktop' },
    context: { conversation: { conversationId: 'work-one' } } };
  const restored = restoreWorkRunOutcomes(messages, 'work-one', () => run);
  assert.equal(restored[0].run.status, 'cancelled');
  assert.equal(restored[0].run.updatedAt, 'end');
  assert.equal(restored[0].text, '任务已取消。');
  assert.equal(restored[0].run.steps[0].result, 'Command aborted');
  assert.equal(messages[0].run.status, 'failed');
  assert.deepEqual(restoreWorkRunOutcomes(messages, 'another-work', () => run), messages,
    'a transcript custom entry cannot borrow a different conversation outcome');
  assert.deepEqual(restoreWorkRunOutcomes(messages, 'work-one', () => ({ ...run, owner: { entryPoint: 'wecom' } })), messages);
});
