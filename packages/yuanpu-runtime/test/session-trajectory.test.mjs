import assert from 'node:assert/strict';
import { test } from 'node:test';
import { register } from 'tsx/esm/api';

register();
const { projectSessionTrajectory, toolSummary, toolCategory } = await import('../src/pi/session-trajectory.ts');
const { createCapabilityId } = await import('../src/capabilities/id.ts');

const at = (second) => `2026-09-27T03:00:${String(second).padStart(2, '0')}.000Z`;
const message = (id, role, second, content, extra = {}) => ({
  id, parentId: null, type: 'message', timestamp: at(second), message: { role, content, ...extra },
});

test('tool labels omit credential-bearing and compound shell arguments', () => {
  assert.equal(toolSummary('bash', { command: 'curl -H "Authorization: Bearer private" example.com' }), 'bash');
  assert.equal(toolSummary('bash', { command: 'pnpm check && echo hidden' }), 'bash');
  assert.equal(toolSummary('bash', { command: 'pnpm check' }), 'bash · pnpm check');
  assert.equal(toolCategory('use_skill', { name: 'frontend-design' }), 'Skill');
});

test('opaque browser capability IDs become readable actions in live and saved trajectories', () => {
  const name = createCapabilityId('builtin.host.browser', 'browser_evaluate');
  assert.equal(toolCategory('execute_capability', { name }), '浏览器');
  assert.equal(toolSummary('execute_capability', { name }), '浏览器 · 在网页中执行操作');
  assert.equal(toolSummary('execute_capability', { name: 'ypcap:invalid' }), '调用外部能力');
});

test('independent ticket calls activate the task ticket category', () => {
  assert.equal(toolCategory('subagent', { action: 'list' }), '工具调用');
  assert.equal(toolCategory('subagent', { action: 'run', agent: 'scout' }), '子 Agent');
  assert.equal(toolCategory('ai_ticket', { action: 'create' }), '任务工单');
  const trajectory = projectSessionTrajectory('work:one', [
    message('u1', 'user', 0, 'Create a work order'),
    message('a1', 'assistant', 1, [{ type: 'toolCall', id: 'ticket-1', name: 'ai_ticket',
      arguments: { action: 'create', task: 'Research the topic' } }], { stopReason: 'toolUse' }),
  ]);
  assert.equal(trajectory.rows.at(-1)?.kind, 'subagent');
  assert.equal(trajectory.rows.at(-1)?.title, '任务工单');
});


test('trajectory follows the active Pi branch across turns and reconciles tool results', () => {
  const trajectory = projectSessionTrajectory('work:one', [
    message('u1', 'user', 0, 'Inspect the code'),
    message('a1', 'assistant', 1, [
      { type: 'thinking', thinking: 'PRIVATE_REASONING' },
      { type: 'text', text: 'Checking files.' },
      { type: 'toolCall', id: 'call-1', name: 'read', arguments: { path: 'src/app.ts', apiKey: 'PRIVATE_KEY' } },
    ], { stopReason: 'toolUse' }),
    message('t1', 'toolResult', 2, [{ type: 'text', text: 'PRIVATE_RESULT' }],
      { toolCallId: 'call-1', toolName: 'read', isError: false }),
    message('u2', 'user', 3, 'Now check the build'),
    message('a2', 'assistant', 4, [{ type: 'toolCall', id: 'call-2', name: 'bash', arguments: { command: 'pnpm check' } }],
      { stopReason: 'toolUse' }),
    message('t2', 'toolResult', 5, [{ type: 'text', text: 'PRIVATE_FAILED_OUTPUT' }],
      { toolCallId: 'call-2', toolName: 'bash', isError: true }),
    { id: 'm2', parentId: null, type: 'model_change', timestamp: at(6), provider: 'test', modelId: 'next' },
    { id: 'c2', parentId: null, type: 'compaction', timestamp: at(7), summary: 'PRIVATE_SUMMARY' },
  ]);

  assert.equal(trajectory.rounds, 2);
  assert.equal(trajectory.calls, 2);
  assert.deepEqual(trajectory.rows.map((row) => [row.round, row.kind]), [
    [1, 'user'], [1, 'assistant'], [1, 'tool'], [1, 'tool'],
    [2, 'user'], [2, 'tool'], [2, 'tool'], [2, 'model'], [2, 'context'],
  ]);
  assert.match(trajectory.rows[2].text, /read · src\/app\.ts/);
  assert.equal(trajectory.rows[6].status, 'failed');
  assert.doesNotMatch(JSON.stringify(trajectory), /PRIVATE_/);
});
