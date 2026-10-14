import assert from 'node:assert/strict';
import test from 'node:test';
import { AssistantRunProgress } from '../src/run-progress.ts';

const snapshot = (entries = [], tools = []) => ({ type: 'snapshot', entries, tools });
const entry = (id, message) => ({ id, model: [{ timestamp: Date.parse('2026-10-11T12:00:00Z'), ...message }] });

test('durable projection isolates turns and recovers missed events from snapshots', () => {
  const old = entry(1, { role: 'assistant', content: [{ type: 'text', text: 'old reply' }] });
  const projection = new AssistantRunProgress(snapshot([old]));
  projection.apply({ type: 'message_update', changes: [
    { type: 'thinking_delta', contentIndex: 0, delta: 'PRIVATE_REASONING' },
    { type: 'text_delta', contentIndex: 1, delta: 'Working' },
  ] });
  assert.equal(projection.value().text, 'Working');
  projection.apply({ type: 'tool_execution_start', toolCallId: 'one', toolName: 'lookup', args: { apiKey: 'PRIVATE_KEY' } });
  assert.equal(projection.value().steps[0].status, 'running');
  const call = entry(2, { role: 'assistant', content: [
    { type: 'thinking', thinking: 'PRIVATE_REASONING' }, { type: 'text', text: 'Working' },
    { type: 'toolCall', id: 'one', name: 'lookup', arguments: { apiKey: 'PRIVATE_KEY' } },
  ] });
  const result = entry(3, { role: 'toolResult', toolCallId: 'one', toolName: 'lookup',
    isError: false, content: [{ type: 'text', text: 'Found it' }, { type: 'image', data: 'PRIVATE_IMAGE' }] });
  const final = entry(4, { role: 'assistant', content: [{ type: 'text', text: 'Done' }] });
  projection.apply(snapshot([old, call, result, final]));
  projection.apply({ type: 'entry_appended', entry: result });
  const value = projection.value();
  assert.equal(value.text, 'Working\n\nDone');
  assert.equal(value.steps.length, 1);
  assert.equal(value.steps[0].status, 'completed');
  assert.equal(value.steps[0].result, 'Found it');
  assert.doesNotMatch(JSON.stringify(value), /PRIVATE_|old reply/);
  assert.deepEqual(new AssistantRunProgress(snapshot([old, call, result, final])).value().steps, []);
});

test('missing tool commit remains interrupted and explicit tool errors remain failed', () => {
  const projection = new AssistantRunProgress(snapshot());
  projection.apply({ type: 'tool_execution_start', toolCallId: 'lost', toolName: 'lookup', args: {} });
  projection.apply(snapshot([], [{ callId: 'lost', status: 'done' }]));
  assert.equal(projection.value().steps[0].status, 'interrupted');
  projection.apply({ type: 'tool_execution_end', toolCallId: 'failed', entry: entry(2,
    { role: 'toolResult', toolCallId: 'failed', toolName: 'lookup', isError: true,
      content: [{ type: 'text', text: 'Tool failed' }] }) });
  assert.equal(projection.value().steps[1].status, 'failed');
});
