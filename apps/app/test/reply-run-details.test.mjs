import assert from 'node:assert/strict';
import test from 'node:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { register } from 'tsx/esm/api';

register();
const { ReplyRunDetails } = await import('../src/shared/reply-run-details.tsx');
const base = { status: 'succeeded', createdAt: '2026-10-11T12:00:00Z', updatedAt: '2026-10-11T12:01:10Z', events: [], tools: [] };
const render = (run) => renderToStaticMarkup(createElement(ReplyRunDetails, { run }));

test('text-only replies have status and elapsed time without an empty disclosure', () => {
  const html = render(base);
  assert.match(html, /已完成/);
  assert.match(html, /1分10秒/);
  assert.doesNotMatch(html, /<details|<summary/);
});

test('tool rows preserve individual outcomes and escape untrusted arguments and results', () => {
  const html = render({ ...base, status: 'cancelled', steps: [
    { id: 'one', name: 'read', status: 'completed', arguments: '{"path":"note.md"}', result: 'abc' },
    { id: 'two', name: 'bash', status: 'cancelled', arguments: '{"command":"<script>bad()</script>"}' },
    { id: 'three', name: 'edit', status: 'failed', result: '<img src=x onerror=bad()>' },
    { id: 'four', name: 'read', status: 'interrupted' },
  ] });
  assert.equal((html.match(/<details/g) ?? []).length, 5);
  for (const label of ['阅读', '执行命令', '编辑', '已读取 3 字符', '已取消', '失败', '结果未知']) assert.ok(html.includes(label));
  assert.doesNotMatch(html, /<script>|<img|已读取 0 字符/);
  assert.match(html, /&lt;script&gt;/);
});
