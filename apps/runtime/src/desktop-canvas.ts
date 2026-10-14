import type { DesktopCanvasWidgetSummary } from '@yuanpu-agent/protocol';

const widgetTypes = new Set<DesktopCanvasWidgetSummary['type']>(['clock', 'schedules', 'knowledge', 'skills', 'conversation', 'html']);
const maximumWidgets = 64;

/**
 * Accepts the renderer's widget snapshot only in its exact bounded shape. The snapshot is
 * forwarded to the canvas agent as data, so titles are trimmed and nothing else is passed through.
 */
export function parseDesktopCanvasWidgets(value: unknown): DesktopCanvasWidgetSummary[] | undefined {
  if (!Array.isArray(value) || value.length > maximumWidgets) return undefined;
  const widgets: DesktopCanvasWidgetSummary[] = [];
  for (const item of value) {
    if (!item || typeof item !== 'object') return undefined;
    const { id, type, title, docked, w, h } = item as Record<string, unknown>;
    const span = (cells: unknown) => Number.isInteger(cells) && (cells as number) >= 1 && (cells as number) <= 24;
    if (typeof id !== 'string' || !/^[A-Za-z0-9_-]{1,64}$/.test(id)
      || !widgetTypes.has(type as DesktopCanvasWidgetSummary['type'])
      || typeof title !== 'string' || typeof docked !== 'boolean' || !span(w) || !span(h)) return undefined;
    widgets.push({ id, type: type as DesktopCanvasWidgetSummary['type'], title: title.trim().slice(0, 60), docked,
      w: w as number, h: h as number });
  }
  return widgets;
}
