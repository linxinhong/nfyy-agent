import assert from 'node:assert/strict';
import test from 'node:test';
import { DESKTOP_CANVAS_LIMITS, createDesktopCanvasTools } from '../dist/index.mjs';

const byName = (tools) => new Map(tools.map((tool) => [tool.name, tool]));

test('desktop canvas tools only record actions for the renderer', async () => {
  const sink = [];
  const tools = byName(createDesktopCanvasTools(sink));
  assert.deepEqual([...tools.keys()], ['desktop_show', 'desktop_put_html', 'desktop_dock', 'desktop_remove']);
  await tools.get('desktop_show').execute('1', { target: 'skills', size: 'medium' });
  await tools.get('desktop_put_html').execute('2', { id: 'report', title: ' 周报 ', html: '<p>ok</p>' });
  await tools.get('desktop_dock').execute('3', { id: 'clock' });
  await tools.get('desktop_remove').execute('4', { id: 'report' });
  assert.deepEqual(sink, [
    { type: 'show', target: 'skills', size: 'medium' },
    { type: 'put_html', id: 'report', title: '周报', html: '<p>ok</p>' },
    { type: 'dock', id: 'clock' },
    { type: 'remove', id: 'report' },
  ]);
});

test('desktop canvas tools bound HTML size and actions per run', async () => {
  const sink = [];
  const tools = byName(createDesktopCanvasTools(sink));
  await assert.rejects(tools.get('desktop_put_html').execute('1', { id: 'big', title: 'x', html: '汉'.repeat(30_000) }), /limited/);
  for (let index = 0; index < DESKTOP_CANVAS_LIMITS.actionsPerRun; index += 1) await tools.get('desktop_dock').execute(String(index), { id: 'a' });
  await assert.rejects(tools.get('desktop_dock').execute('x', { id: 'a' }), /at most/);
});
