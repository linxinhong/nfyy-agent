import { defineTool } from '@earendil-works/pi-coding-agent';
import type { DesktopCanvasAction } from '@yuanpu-agent/protocol';
import { Type } from 'typebox';
import { jsonResult } from '../store.js';

/** Upper bounds keep one run from flooding the renderer or local storage. */
export const DESKTOP_CANVAS_LIMITS = { actionsPerRun: 16, htmlBytes: 64 * 1024, titleLength: 60 } as const;

const widgetId = Type.String({ pattern: '^[A-Za-z0-9_-]{1,64}$', description: 'Widget id from the current desktop list, or a new id for put_html.' });
const size = Type.Optional(Type.Union([Type.Literal('small'), Type.Literal('medium'), Type.Literal('large'), Type.Literal('wide')],
  { description: 'small 2x2, medium 4x2, large 4x4, wide 8x4 grid cells.' }));

export const DESKTOP_CANVAS_INSTRUCTIONS = [
  'You are the agent of the MindLink desktop canvas. The user types into a fixed composer; your text reply appears briefly above it.',
  'Use the desktop tools to decide what the canvas shows: desktop_show brings a built-in widget (clock, schedules, knowledge, skills, conversation = the history of this chat) or an existing widget id onto the canvas;',
  'desktop_put_html creates or replaces a static HTML widget (inline CSS and SVG only; scripts never run); desktop_dock tucks a widget into the Dock; desktop_remove deletes an HTML widget.',
  'Each message carries the current widget list as data; it is the only truth about what exists. desktop_show, desktop_dock and desktop_remove work only on ids in that list (or built-in widget names); if a widget you made earlier is missing from it, recreate it with desktop_put_html.',
  'Reuse an existing HTML widget id to update it instead of creating duplicates. The user owns widget positions; you only choose content and size.',
  'Your text reply is delivered to the user automatically, and the conversation widget the user placed already shows the full history.',
  'Never use desktop_put_html to show this chat, its replies or a transcript, and call desktop_show for conversation only when the user asks to see the history.',
  'Keep text replies short when the result is shown as a widget.',
].join(' ');

/**
 * Canvas tools record requested changes in `sink`; they never touch the UI themselves.
 * They are registered only on the desktop canvas session, so Work and assistant runs never see them.
 */
export function createDesktopCanvasTools(sink: DesktopCanvasAction[]) {
  const record = (action: DesktopCanvasAction) => {
    if (sink.length >= DESKTOP_CANVAS_LIMITS.actionsPerRun) {
      throw new Error(`A single run can change at most ${DESKTOP_CANVAS_LIMITS.actionsPerRun} desktop widgets.`);
    }
    sink.push(action);
    return jsonResult({ queued: action.type, ...('id' in action ? { id: action.id } : { target: action.target }) });
  };
  return [
    defineTool({
      name: 'desktop_show', label: 'Desktop show',
      description: 'Show a built-in widget (clock, schedules, knowledge, skills, conversation) or an existing widget id on the desktop canvas, optionally resized.',
      parameters: Type.Object({
        target: Type.String({ pattern: '^[A-Za-z0-9_-]{1,64}$', description: 'clock | schedules | knowledge | skills | conversation, or an existing widget id.' }),
        size,
      }, { additionalProperties: false }),
      execute: async (_id, input) => record({ type: 'show', target: input.target, ...(input.size ? { size: input.size } : {}) }),
    }),
    defineTool({
      name: 'desktop_put_html', label: 'Desktop HTML widget',
      description: 'Create or replace a static HTML widget on the desktop canvas. Scripts, forms and external resources are blocked by the sandbox.',
      parameters: Type.Object({
        id: widgetId,
        title: Type.String({ minLength: 1, maxLength: DESKTOP_CANVAS_LIMITS.titleLength }),
        html: Type.String({ minLength: 1, maxLength: DESKTOP_CANVAS_LIMITS.htmlBytes }),
        size,
      }, { additionalProperties: false }),
      execute: async (_id, input) => {
        if (Buffer.byteLength(input.html, 'utf8') > DESKTOP_CANVAS_LIMITS.htmlBytes) {
          throw new Error(`HTML widgets are limited to ${DESKTOP_CANVAS_LIMITS.htmlBytes} bytes.`);
        }
        return record({ type: 'put_html', id: input.id, title: input.title.trim(), html: input.html,
          ...(input.size ? { size: input.size } : {}) });
      },
    }),
    defineTool({
      name: 'desktop_dock', label: 'Desktop dock',
      description: 'Move a widget from the canvas into the Dock.',
      parameters: Type.Object({ id: widgetId }, { additionalProperties: false }),
      execute: async (_id, input) => record({ type: 'dock', id: input.id }),
    }),
    defineTool({
      name: 'desktop_remove', label: 'Desktop remove',
      description: 'Delete an HTML widget. Built-in widgets cannot be deleted; dock them instead.',
      parameters: Type.Object({ id: widgetId }, { additionalProperties: false }),
      execute: async (_id, input) => record({ type: 'remove', id: input.id }),
    }),
  ];
}
