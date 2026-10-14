import assert from 'node:assert/strict';
import test from 'node:test';
import { mountWordEditor } from '../dist/renderer.js';

function fixture() {
  const view = new EventTarget();
  const iframe = Object.assign(new EventTarget(), {
    style: {}, contentWindow: {}, setAttribute() {}, remove() { this.removed = true; },
  });
  const container = { ownerDocument: { defaultView: view, createElement: () => iframe }, appendChild() {} };
  const document = { id: 'owned', title: 'source.docx', mode: 'edit', load: 'frame', path: 'source.docx',
    origin: 'http://127.0.0.1:39180', url: 'http://127.0.0.1:39180/static/doc/pc.html?mode=edit' };
  const message = (data, origin = document.origin, source = iframe.contentWindow) => {
    const event = Object.assign(new Event('message'), { data, origin, source });
    view.dispatchEvent(event);
  };
  return { iframe, container, document, message };
}

test('accepts messages only from the owned editor frame and cleans up listeners', () => {
  const { container, document, iframe, message } = fixture();
  const states = [];
  let saves = 0;
  let loads = 0;
  const mounted = mountWordEditor(container, document, { theme: 'dark', onLoad: () => loads++,
    onDirtyChange: (dirty) => states.push(dirty), onSaveRequest: () => saves++ });
  const dirty = { type: 'workbuddy:document-status-changed', hasUnSavedChange: true };
  message(dirty, 'http://example.com');
  message(dirty, document.origin, {});
  assert.deepEqual(states, []);
  message(dirty);
  message({ type: 'workbuddy:tencent-docs-save-shortcut' });
  iframe.dispatchEvent(new Event('load'));
  assert.deepEqual(states, [true]);
  assert.equal(saves, 1);
  assert.equal(loads, 1);
  assert.equal(new URL(iframe.src).searchParams.get('workbuddyTheme'), 'dark');
  mounted.destroy();
  message(dirty);
  assert.deepEqual(states, [true]);
  assert.equal(iframe.removed, true);
});

test('rejects non-loopback URLs and ignores save shortcuts in read-only mode', () => {
  const { container, document, message } = fixture();
  assert.throws(() => mountWordEditor(container, { ...document, url: 'https://example.com/static/doc/pc.html' }));
  assert.throws(() => mountWordEditor(container, { ...document, load: 'model' }));
  let saves = 0;
  const mounted = mountWordEditor(container, { ...document, mode: 'readonly' }, { onSaveRequest: () => saves++ });
  message({ type: 'workbuddy:tencent-docs-save-shortcut' });
  assert.equal(saves, 0);
  mounted.destroy();
});
