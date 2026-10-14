import assert from 'node:assert/strict';
import test from 'node:test';
import {
  addWidget, applyDesktopActions, arrangeDesktop, bringToFront, defaultDesktop, desktopColumns, desktopPitch, findFreeCell, placeWidget,
  appendDesktopConversation, readDesktop, resolveDesktopActions, readDesktopConversation, showOrAddWidget, updateDesktopConversation, snapToCell, summarizeDesktop, undockWidget, writeDesktop,
} from '../src/modules/desktop-layout.ts';

const widget = (id, x, y, w, h, extra = {}) => ({ id, type: 'clock', x, y, w, h, z: 1, docked: false, ...extra });
let counter = 0;
const newId = () => `n${++counter}`;

test('columns and cells follow the 80px cell with 12px gap', () => {
  assert.equal(desktopPitch, 92);
  assert.equal(desktopColumns(1276), 14);
  assert.equal(desktopColumns(10), 2);
  assert.equal(snapToCell(140), 2);
  assert.equal(snapToCell(-30), 0);
});

test('widgets go exactly where they are dropped, may overlap, and come to the front', () => {
  const list = [widget('a', 0, 0, 4, 4), widget('b', 8, 0, 4, 4, { z: 2 })];
  const next = placeWidget(list, 'b', { x: 2, y: 1, w: 4, h: 4 }, 14);
  assert.deepEqual(next.find((item) => item.id === 'a'), list[0]);
  assert.deepEqual(next.find((item) => item.id === 'b'), { ...list[1], x: 2, y: 1, z: 3 });
  assert.deepEqual(bringToFront(next, 'a').find((item) => item.id === 'a').z, 4);
});

test('the rightmost widget stays reachable and movable on a narrow canvas', () => {
  const list = [widget('skills', 10, 0, 4, 4, { type: 'skills' })];
  assert.deepEqual(arrangeDesktop(list, 8).map(({ x, w }) => [x, w]), [[4, 4]]);
  assert.deepEqual(arrangeDesktop(list, 3).map(({ x, w }) => [x, w]), [[0, 3]]);
  const moved = placeWidget(list, 'skills', { x: 1, y: 3, w: 4, h: 4 }, 8)[0];
  assert.deepEqual([moved.x, moved.y], [1, 3]);
  assert.equal(placeWidget(list, 'skills', { x: 99, y: 0, w: 4, h: 4 }, 8)[0].x, 4);
});

test('resizing keeps the minimum tile and new or undocked widgets prefer free space', () => {
  const list = [widget('a', 0, 0, 4, 4), widget('d', 0, 0, 2, 2, { docked: true })];
  const tile = placeWidget(list, 'a', { x: 0, y: 0, w: 1, h: 1 }, 14)[0];
  assert.deepEqual([tile.w, tile.h], [2, 2]);
  assert.deepEqual(findFreeCell(arrangeDesktop(list, 6), 2, 2, 6), { x: 4, y: 0 });
  const undocked = undockWidget(list, 'd', 6).find((item) => item.id === 'd');
  assert.deepEqual([undocked.x, undocked.y, undocked.docked], [4, 0, false]);
  assert.deepEqual(addWidget(list, { id: 'e', type: 'skills', w: 4, h: 4 }, 8).at(-1).x, 4);
});

test('agent actions show, create, update, dock and remove without moving user widgets', () => {
  const list = [
    widget('clock', 3, 5, 2, 2),
    widget('k', 0, 0, 4, 4, { type: 'knowledge', docked: true }),
    widget('report', 6, 0, 4, 2, { type: 'html', title: '旧', html: '<p>旧</p>' }),
  ];
  const next = applyDesktopActions(list, [
    { type: 'show', target: 'knowledge' },
    { type: 'show', target: 'skills', size: 'medium' },
    { type: 'put_html', id: 'report', title: '新', html: '<p>新</p>' },
    { type: 'put_html', id: 'chart', title: '图表', html: '<svg/>', size: 'large' },
    { type: 'put_html', id: 'clock', title: '劫持', html: '<p>x</p>' },
    { type: 'dock', id: 'clock' },
    { type: 'remove', id: 'k' },
  ], 14, newId);
  const byId = new Map(next.map((item) => [item.id, item]));
  assert.equal(byId.get('k').docked, false);
  assert.ok(next.some((item) => item.type === 'skills' && item.w === 4 && item.h === 2));
  assert.deepEqual([byId.get('report').title, byId.get('report').html, byId.get('report').x], ['新', '<p>新</p>', 6]);
  assert.deepEqual(Object.keys(byId.get('chart')).sort(), ['docked', 'h', 'html', 'id', 'title', 'type', 'w', 'x', 'y', 'z']);
  assert.deepEqual([byId.get('clock').type, byId.get('clock').html, byId.get('clock').docked, byId.get('clock').x], ['clock', undefined, true, 3]);
  assert.ok(byId.has('k'), 'built-in widgets cannot be removed by the agent');
  assert.deepEqual(applyDesktopActions(next, [{ type: 'remove', id: 'chart' }], 14, newId).some((item) => item.id === 'chart'), false);
});

test('the conversation widget the user placed is the only one', () => {
  const placed = widget('conversation-mine', 6, 3, 4, 4, { type: 'conversation', docked: true });
  const list = [widget('clock', 0, 0, 2, 2), placed];
  const reopened = showOrAddWidget(list, { id: 'conversation-new', type: 'conversation', w: 4, h: 4 }, 14);
  assert.deepEqual(reopened.filter((item) => item.type === 'conversation').map((item) => [item.id, item.docked]), [['conversation-mine', false]]);
  assert.equal(showOrAddWidget(list, { id: 'clock-2', type: 'clock', w: 2, h: 2 }, 14).length, 3);

  const next = applyDesktopActions(reopened, [
    { type: 'show', target: 'conversation' },
    { type: 'put_html', id: 'chat-history', title: '对话内容', html: '<p>copy</p>' },
    { type: 'put_html', id: 'conversation', title: 'Recap', html: '<p>copy</p>' },
    { type: 'put_html', id: 'report', title: '周报', html: '<p>ok</p>' },
  ], 14, newId);
  assert.deepEqual(next.filter((item) => item.type === 'conversation').map((item) => item.id), ['conversation-mine']);
  assert.deepEqual(next.filter((item) => item.type === 'html').map((item) => item.id), ['report']);
  assert.ok(applyDesktopActions([widget('clock', 0, 0, 2, 2)], [{ type: 'put_html', id: 'chat', title: '对话', html: '<p/>' }], 14, newId)
    .some((item) => item.id === 'chat'), 'without a conversation widget the agent may still make such a widget');
});

test('the agent snapshot carries identity and size but no widget content', () => {
  const summary = summarizeDesktop([widget('r', 0, 0, 4, 2, { type: 'html', title: '报告', html: '<p>secret</p>' })], (item) => item.title);
  assert.deepEqual(summary, [{ id: 'r', type: 'html', title: '报告', docked: false, w: 4, h: 2 }]);
});

test('stored layouts are validated and default when missing or malformed', () => {
  const values = new Map();
  globalThis.window = { localStorage: { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) } };
  assert.deepEqual(readDesktop(), defaultDesktop());
  writeDesktop([widget('a', 0, 0, 2, 2), { ...widget('bad', 0, 0, 2, 2), type: 'chat' }, { ...widget('noz', 0, 0, 2, 2), z: undefined }]);
  assert.deepEqual(readDesktop().map((item) => item.id), ['a']);
  values.set('yuanpu:desktop:v3', '{');
  assert.deepEqual(readDesktop(), defaultDesktop());
  delete globalThis.window;
});

test('conversation entries carry bounded run progress that updates in place', () => {
  const values = new Map();
  globalThis.window = { localStorage: { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) },
    dispatchEvent: () => true };
  const at = new Date().toISOString();
  appendDesktopConversation({ role: 'user', text: '做一个案件看板' });
  const id = appendDesktopConversation({ role: 'assistant', text: '', run: { status: 'running', createdAt: at, updatedAt: at, events: [], tools: [], steps: [] } });
  const steps = Array.from({ length: 50 }, (_, index) => ({ id: `s${index}`, name: 'desktop_put_html', status: 'completed', startedAt: at,
    arguments: 'x'.repeat(5_000), result: 'ok' }));
  updateDesktopConversation(id, { text: '完成。', run: { status: 'succeeded', createdAt: at, updatedAt: at, events: [{ id: 1, title: 'x', at }], tools: [], steps } });
  const [user, reply] = readDesktopConversation();
  assert.equal(user.text, '做一个案件看板');
  assert.equal(reply.id, id);
  assert.deepEqual([reply.text, reply.run.status, reply.run.steps.length, reply.run.events.length], ['完成。', 'succeeded', 40, 0]);
  assert.deepEqual([reply.run.steps[0].id, reply.run.steps[0].arguments.length, reply.run.steps[0].truncated], ['s10', 2_000, true]);
  delete globalThis.window;
});

test('agent actions report the widgets they touched and the ids that do not exist', () => {
  const before = [widget('clock', 0, 0, 2, 2), widget('k', 0, 4, 4, 4, { type: 'knowledge', docked: true }), widget('law', 6, 9, 4, 4, { type: 'html' })];
  const actions = [
    { type: 'show', target: 'lawyer-cases', size: 'large' },
    { type: 'show', target: 'law' },
    { type: 'show', target: 'knowledge' },
    { type: 'put_html', id: 'new-report', title: '报告', html: '<p/>' },
    { type: 'dock', id: 'ghost' },
  ];
  const after = applyDesktopActions(before, actions, 14, newId);
  const { touched, missing } = resolveDesktopActions(before, after, actions);
  assert.deepEqual(touched.sort(), ['k', 'law', 'new-report']);
  assert.deepEqual(missing, ['lawyer-cases', 'ghost']);
});
