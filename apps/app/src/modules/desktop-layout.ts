import type { DesktopCanvasAction, DesktopCanvasWidgetSummary, DesktopReplyRunInfo } from '@yuanpu-agent/protocol';

/** One grid cell of the desktop canvas; widgets snap to whole cells. */
export const desktopCell = 80;
export const desktopGap = 12;
export const desktopPitch = desktopCell + desktopGap;

export type DesktopWidgetType = DesktopCanvasWidgetSummary['type'];
export type DesktopWidgetSize = 'small' | 'medium' | 'large' | 'wide';

export const desktopWidgetSizes: Record<DesktopWidgetSize, { w: number; h: number; label: string }> = {
  small: { w: 2, h: 2, label: '小' },
  medium: { w: 4, h: 2, label: '中' },
  large: { w: 4, h: 4, label: '大' },
  wide: { w: 8, h: 4, label: '宽' },
};
/** Grid span of a size preset, without its display label. */
export function sizeCells(size: DesktopWidgetSize): { w: number; h: number } {
  const { w, h } = desktopWidgetSizes[size];
  return { w, h };
}
export const builtinWidgetTypes = ['clock', 'schedules', 'knowledge', 'skills', 'conversation'] as const;
export const defaultWidgetSize: Record<DesktopWidgetType, DesktopWidgetSize> = {
  clock: 'small', schedules: 'medium', knowledge: 'large', skills: 'large', conversation: 'large', html: 'medium',
};

/** The smallest tile any widget may shrink to. */
const minimumSpan = { w: 2, h: 2 };
const maxSpan = 24;

export interface DesktopWidget {
  id: string;
  type: DesktopWidgetType;
  x: number;
  y: number;
  w: number;
  h: number;
  /** Stacking order; widgets may overlap and the most recently touched one is on top. */
  z: number;
  docked: boolean;
  title?: string;
  html?: string;
}

export type DesktopRect = Pick<DesktopWidget, 'x' | 'y' | 'w' | 'h'>;

const widgetTypes = new Set<DesktopWidgetType>([...builtinWidgetTypes, 'html']);
const storageKey = 'yuanpu:desktop:v3';

export function desktopColumns(width: number): number {
  return Math.max(minimumSpan.w, Math.floor((width + desktopGap) / desktopPitch));
}

/** Converts a pixel offset inside the canvas to the nearest cell. */
export function snapToCell(pixels: number): number {
  return Math.max(0, Math.round(pixels / desktopPitch));
}

function overlaps(a: DesktopRect, b: DesktopRect): boolean {
  return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
}

function topZ(widgets: readonly DesktopWidget[]): number {
  return widgets.reduce((max, widget) => Math.max(max, widget.z), 0);
}

/**
 * Visible widgets positioned for the given width. Placement is free and overlap is allowed;
 * a widget only shifts left (and narrows) when the canvas is too narrow to hold it.
 */
export function arrangeDesktop(widgets: readonly DesktopWidget[], columns: number): DesktopWidget[] {
  return widgets.filter((widget) => !widget.docked).map((widget) => {
    const w = Math.min(widget.w, columns);
    return { ...widget, w, x: Math.min(widget.x, columns - w) };
  });
}

/** Moves or resizes one widget exactly where it was dropped and brings it to the front. */
export function placeWidget(widgets: readonly DesktopWidget[], id: string, rect: DesktopRect, columns: number): DesktopWidget[] {
  const span = (value: number, min: number, max: number) => Math.min(max, Math.max(min, Math.round(value)));
  const z = topZ(widgets) + 1;
  return widgets.map((widget) => {
    if (widget.id !== id) return widget;
    const w = span(rect.w, minimumSpan.w, Math.min(maxSpan, Math.max(columns, minimumSpan.w)));
    return { ...widget, w, h: span(rect.h, minimumSpan.h, maxSpan),
      x: Math.min(Math.max(0, Math.round(rect.x)), Math.max(0, columns - w)), y: Math.max(0, Math.round(rect.y)), z, docked: false };
  });
}

export function bringToFront(widgets: readonly DesktopWidget[], id: string): DesktopWidget[] {
  const target = widgets.find((widget) => widget.id === id);
  const z = topZ(widgets);
  if (!target || (target.z === z && widgets.filter((widget) => widget.z === z).length === 1)) return [...widgets];
  return widgets.map((widget) => widget.id === id ? { ...widget, z: z + 1 } : widget);
}

/** The first free top-left cell, scanning rows from the top; new widgets prefer empty space. */
export function findFreeCell(visible: readonly DesktopRect[], w: number, h: number, columns: number): { x: number; y: number } {
  const width = Math.min(w, columns);
  for (let y = 0; ; y += 1) {
    for (let x = 0; x + width <= columns; x += 1) {
      if (!visible.some((widget) => overlaps(widget, { x, y, w: width, h }))) return { x, y };
    }
  }
}

export function undockWidget(widgets: readonly DesktopWidget[], id: string, columns: number): DesktopWidget[] {
  const target = widgets.find((widget) => widget.id === id);
  if (!target) return [...widgets];
  const cell = findFreeCell(arrangeDesktop(widgets.filter((widget) => widget.id !== id), columns), target.w, target.h, columns);
  const z = topZ(widgets) + 1;
  return widgets.map((widget) => widget.id === id ? { ...widget, ...cell, z, docked: false } : widget);
}

/** Widget types that exist at most once; adding another brings the existing one back instead. */
export const singletonWidgetTypes: readonly DesktopWidgetType[] = ['conversation'];

/**
 * Adds a widget, except for singletons: an existing conversation widget (the one the user placed)
 * is undocked or brought to the front rather than duplicated.
 */
export function showOrAddWidget(widgets: readonly DesktopWidget[], widget: Omit<DesktopWidget, 'x' | 'y' | 'z' | 'docked'>,
  columns: number): DesktopWidget[] {
  const existing = singletonWidgetTypes.includes(widget.type) ? widgets.find((item) => item.type === widget.type) : undefined;
  if (!existing) return addWidget(widgets, widget, columns);
  return existing.docked ? undockWidget(widgets, existing.id, columns) : bringToFront(widgets, existing.id);
}

export function addWidget(widgets: readonly DesktopWidget[], widget: Omit<DesktopWidget, 'x' | 'y' | 'z' | 'docked'>,
  columns: number): DesktopWidget[] {
  const cell = findFreeCell(arrangeDesktop(widgets, columns), widget.w, widget.h, columns);
  return [...widgets, { ...widget, ...cell, z: topZ(widgets) + 1, docked: false }];
}

/** What the canvas agent may know about the desktop: identity, title and size, never content. */
export function summarizeDesktop(widgets: readonly DesktopWidget[], titleOf: (widget: DesktopWidget) => string): DesktopCanvasWidgetSummary[] {
  return widgets.map((widget) => ({ id: widget.id, type: widget.type, title: titleOf(widget).slice(0, 60),
    docked: widget.docked, w: widget.w, h: widget.h }));
}

/**
 * Applies the canvas agent's requests. The agent picks content and size; positions the user
 * arranged stay put, and built-in widgets can only be shown or docked, never deleted.
 */
export function applyDesktopActions(widgets: readonly DesktopWidget[], actions: readonly DesktopCanvasAction[],
  columns: number, newId: () => string): DesktopWidget[] {
  let next = [...widgets];
  const resize = (id: string, size?: DesktopWidgetSize) => {
    const target = next.find((widget) => widget.id === id);
    if (!target || !size) return;
    next = placeWidget(next, id, { x: target.x, y: target.y, ...sizeCells(size) }, columns);
  };
  for (const action of actions) {
    if (action.type === 'show') {
      const existing = next.find((widget) => widget.id === action.target)
        ?? next.find((widget) => widget.type === action.target && !widget.docked)
        ?? next.find((widget) => widget.type === action.target);
      if (existing) {
        next = existing.docked ? undockWidget(next, existing.id, columns) : bringToFront(next, existing.id);
        resize(existing.id, action.size);
      } else if ((builtinWidgetTypes as readonly string[]).includes(action.target)) {
        const type = action.target as DesktopWidgetType;
        next = addWidget(next, { id: `${type}-${newId()}`, type, ...sizeCells(action.size ?? defaultWidgetSize[type]) }, columns);
      }
    } else if (action.type === 'put_html') {
      // The conversation widget already shows every exchange; an HTML copy of it would only duplicate it.
      if (isConversationCopy(action, next)) continue;
      const existing = next.find((widget) => widget.id === action.id);
      if (existing && existing.type !== 'html') continue;
      if (existing) {
        next = next.map((widget) => widget.id === action.id ? { ...widget, title: action.title, html: action.html } : widget);
        next = existing.docked ? undockWidget(next, existing.id, columns) : bringToFront(next, existing.id);
        resize(existing.id, action.size);
      } else {
        next = addWidget(next, { id: action.id, type: 'html', title: action.title, html: action.html,
          ...sizeCells(action.size ?? 'medium') }, columns);
      }
    } else if (action.type === 'dock') {
      next = next.map((widget) => widget.id === action.id ? { ...widget, docked: true } : widget);
    } else if (action.type === 'remove') {
      next = next.filter((widget) => widget.id !== action.id || widget.type !== 'html');
    }
  }
  return next;
}

/**
 * Which widgets a batch of agent actions pointed at (to reveal them) and which ids it named
 * that do not exist, so a show or dock of a missing widget is reported instead of silently ignored.
 */
export function resolveDesktopActions(before: readonly DesktopWidget[], after: readonly DesktopWidget[],
  actions: readonly DesktopCanvasAction[]): { touched: string[]; missing: string[] } {
  const touched = new Set<string>();
  const missing = new Set<string>();
  const known = new Set(before.map((widget) => widget.id));
  for (const action of actions) {
    if (action.type === 'put_html') {
      known.add(action.id);
      if (after.some((widget) => widget.id === action.id && widget.type === 'html')) touched.add(action.id);
    } else if (action.type === 'show') {
      const shown = after.find((widget) => widget.id === action.target)
        ?? after.find((widget) => widget.type === action.target && !widget.docked);
      if (shown) touched.add(shown.id);
      else missing.add(action.target);
    } else if (!known.has(action.id)) {
      missing.add(action.id);
    }
  }
  return { touched: [...touched].filter((id) => after.some((widget) => widget.id === id && !widget.docked)), missing: [...missing] };
}

function isConversationCopy(action: { id: string; title: string }, widgets: readonly DesktopWidget[]): boolean {
  if (!widgets.some((widget) => widget.type === 'conversation')) return false;
  const normalized = (value: string) => value.toLowerCase().replace(/[\s_-]/g, '');
  return ['conversation', 'chat', 'chathistory', '对话', '对话内容', '对话记录', '聊天记录']
    .some((name) => normalized(action.id) === name || normalized(action.title) === name);
}

export function defaultDesktop(): DesktopWidget[] {
  return [
    { id: 'clock', type: 'clock', x: 0, y: 0, w: 2, h: 2, z: 1, docked: false },
    { id: 'schedules', type: 'schedules', x: 0, y: 2, w: 4, h: 2, z: 2, docked: false },
    { id: 'knowledge', type: 'knowledge', x: 4, y: 0, w: 4, h: 4, z: 3, docked: false },
    { id: 'skills', type: 'skills', x: 8, y: 0, w: 4, h: 4, z: 4, docked: false },
    { id: 'html-sample', type: 'html', x: 12, y: 0, w: 4, h: 4, z: 5, docked: true, title: '示例 HTML 挂件',
      html: '<h2 style="margin:0 0 8px">本周进展</h2><p style="margin:0 0 12px;color:#61666b">这是一个模拟 Agent 生成的 HTML 挂件，运行在无脚本沙箱中。</p>'
        + '<svg viewBox="0 0 260 90"><g fill="#51866a"><rect x="10" y="40" width="34" height="40" rx="4"/><rect x="60" y="22" width="34" height="58" rx="4"/>'
        + '<rect x="110" y="50" width="34" height="30" rx="4"/><rect x="160" y="12" width="34" height="68" rx="4"/><rect x="210" y="30" width="34" height="50" rx="4"/></g></svg>' },
  ];
}

function isWidget(value: unknown): value is DesktopWidget {
  if (!value || typeof value !== 'object') return false;
  const item = value as Record<string, unknown>;
  const cell = (key: string, min: number) => Number.isInteger(item[key]) && (item[key] as number) >= min && (item[key] as number) <= 100_000;
  return typeof item.id === 'string' && widgetTypes.has(item.type as DesktopWidgetType)
    && cell('x', 0) && cell('y', 0) && cell('w', 1) && cell('h', 1) && cell('z', 0) && typeof item.docked === 'boolean'
    && (item.title === undefined || typeof item.title === 'string')
    && (item.html === undefined || typeof item.html === 'string');
}

/** One exchange with the desktop agent; the composer itself never shows history. */
export interface DesktopConversationEntry {
  id: string;
  role: 'user' | 'assistant' | 'error';
  text: string;
  at: string;
  /** Status and tool steps of the agent run behind an assistant entry, shown like the main chat. */
  run?: DesktopReplyRunInfo;
}

const conversationKey = 'yuanpu:desktop:conversation:v1';
export const desktopConversationEvent = 'yuanpu:desktop-conversation-changed';
const maximumConversationEntries = 100;

export function readDesktopConversation(): DesktopConversationEntry[] {
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(conversationKey) ?? '[]');
    return Array.isArray(value) ? value.filter((item): item is DesktopConversationEntry => Boolean(item)
      && typeof item.id === 'string' && ['user', 'assistant', 'error'].includes(item.role)
      && typeof item.text === 'string' && typeof item.at === 'string'
      && (item.run === undefined || (typeof item.run === 'object' && typeof item.run.status === 'string'))) : [];
  } catch { return []; }
}

/** Step arguments and results are bounded before they reach local storage. */
function storedRun(run: DesktopReplyRunInfo): DesktopReplyRunInfo {
  const clip = (value?: string) => value === undefined ? undefined : value.slice(0, 2_000);
  return { ...run, events: [], steps: run.steps?.slice(-40).map((step) => ({ ...step, arguments: clip(step.arguments),
    result: clip(step.result), truncated: step.truncated || (step.arguments?.length ?? 0) > 2_000 || (step.result?.length ?? 0) > 2_000 })) };
}

function writeDesktopConversation(entries: DesktopConversationEntry[]): void {
  const next = entries.slice(-maximumConversationEntries).map((entry) => entry.run ? { ...entry, run: storedRun(entry.run) } : entry);
  try { window.localStorage.setItem(conversationKey, JSON.stringify(next)); } catch { /* history is a convenience */ }
  window.dispatchEvent(new Event(desktopConversationEvent));
}

export function appendDesktopConversation(entry: Omit<DesktopConversationEntry, 'id' | 'at'>): string {
  const id = crypto.randomUUID();
  writeDesktopConversation([...readDesktopConversation(), { ...entry, id, at: new Date().toISOString() }]);
  return id;
}

export function updateDesktopConversation(id: string, patch: Partial<Omit<DesktopConversationEntry, 'id' | 'at'>>): void {
  const entries = readDesktopConversation();
  if (!entries.some((entry) => entry.id === id)) return;
  writeDesktopConversation(entries.map((entry) => entry.id === id ? { ...entry, ...patch } : entry));
}

export function clearDesktopConversation(): void {
  try { window.localStorage.removeItem(conversationKey); } catch { /* history is a convenience */ }
  window.dispatchEvent(new Event(desktopConversationEvent));
}

export function readDesktop(): DesktopWidget[] {
  try {
    const raw = window.localStorage.getItem(storageKey);
    if (raw === null) return defaultDesktop();
    const value: unknown = JSON.parse(raw);
    return Array.isArray(value) ? value.filter(isWidget) : defaultDesktop();
  } catch { return defaultDesktop(); }
}

export function writeDesktop(widgets: readonly DesktopWidget[]): void {
  try { window.localStorage.setItem(storageKey, JSON.stringify(widgets)); } catch { /* the layout still applies for this session */ }
}
