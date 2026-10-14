import assert from 'node:assert/strict';
import test from 'node:test';
import { parseDesktopCanvasWidgets } from '../src/desktop-canvas.ts';

const widget = (extra = {}) => ({ id: 'report', type: 'html', title: '  周报  ', docked: false, w: 4, h: 2, ...extra });

test('desktop canvas snapshots keep only bounded summary fields', () => {
  assert.deepEqual(parseDesktopCanvasWidgets([{ ...widget(), html: '<p>never forwarded</p>', x: 3 }]),
    [{ id: 'report', type: 'html', title: '周报', docked: false, w: 4, h: 2 }]);
  assert.deepEqual(parseDesktopCanvasWidgets([]), []);
});

test('desktop canvas snapshots reject unknown shapes instead of guessing', () => {
  for (const value of [undefined, {}, [widget({ id: '../x' })], [widget({ type: 'chat' })], [widget({ w: 0 })],
    [widget({ h: 25 })], [widget({ docked: 'no' })], [widget({ title: 1 })], Array.from({ length: 65 }, () => widget())]) {
    assert.equal(parseDesktopCanvasWidgets(value), undefined);
  }
});
