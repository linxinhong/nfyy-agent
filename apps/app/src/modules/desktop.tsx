import { useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type PointerEvent as ReactPointerEvent } from 'react';
import type { AgentRunRecord, DesktopCanvasAction, DesktopReplyRunInfo, InstalledPlugin, ScheduleRecord } from '@yuanpu-agent/protocol';

import { AppIcon } from '../shared/app-icon.js';
import { MessageContent } from '../shared/message-content.js';
import { ReplyRunDetails } from '../shared/reply-run-details.js';
import { htmlPreviewDocument } from '../shared/preview-content.js';
import { readSavedContent, savedContentEvent } from '../shared/saved-content.js';
import type { UiDestination } from '../ui-registry.js';
import {
  appendDesktopConversation, applyDesktopActions, clearDesktopConversation, desktopConversationEvent, readDesktopConversation,
  resolveDesktopActions, updateDesktopConversation, arrangeDesktop, bringToFront, builtinWidgetTypes, defaultDesktop, defaultWidgetSize, desktopColumns,
  desktopGap, desktopPitch, desktopWidgetSizes, placeWidget, readDesktop, showOrAddWidget, sizeCells, snapToCell, summarizeDesktop, undockWidget, writeDesktop,
  type DesktopRect, type DesktopWidget, type DesktopWidgetSize, type DesktopWidgetType,
} from './desktop-layout.js';
import './desktop.css';

type IconName = Parameters<typeof AppIcon>[0]['name'];

const catalog: Record<DesktopWidgetType, { title: string; icon: IconName; opens?: UiDestination }> = {
  clock: { title: '时钟', icon: 'clock' },
  schedules: { title: '定时任务', icon: 'schedules', opens: 'schedules' },
  knowledge: { title: '知识库', icon: 'knowledge', opens: 'knowledge' },
  skills: { title: '已安装技能', icon: 'skills', opens: 'skills' },
  conversation: { title: '对话内容', icon: 'work' },
  html: { title: 'HTML 挂件', icon: 'code' },
};

const previewPlugins: Array<Pick<InstalledPlugin, 'name' | 'version' | 'enabled'>> = [
  { name: 'web-research', version: '1.2.0', enabled: true },
  { name: 'python-capabilities', version: '0.4.1', enabled: true },
  { name: 'diagram-tools', version: '2.0.3', enabled: false },
];

type Drag = {
  id: string;
  mode: 'move' | 'resize';
  startX: number;
  startY: number;
  dx: number;
  dy: number;
  /** Widget rectangle in canvas pixels when the drag started. */
  left: number;
  top: number;
  width: number;
  height: number;
  target?: DesktopRect;
  overDock: boolean;
};

type HtmlDraft = { id?: string; title: string; html: string; size: DesktopWidgetSize };
type Reply = { tone: 'running' | 'done' | 'error'; text: string };

const terminalStatuses: AgentRunRecord['status'][] = ['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown'];
const pixels = (cells: number) => cells * desktopPitch - desktopGap;
const titleOf = (widget: DesktopWidget) => widget.title || catalog[widget.type].title;
const inside = (rect: DOMRect | undefined, x: number, y: number) =>
  Boolean(rect && x >= rect.left && x <= rect.right && y >= rect.top && y <= rect.bottom);
const shortId = () => crypto.randomUUID().slice(0, 8);
const dockCollapsedKey = 'yuanpu:desktop:dock-collapsed';
const readDockCollapsed = () => { try { return window.localStorage.getItem(dockCollapsedKey) === '1'; } catch { return false; } };

/** A free-form widget canvas with a Dock on the left and a fixed agent composer at the bottom centre. */
export function DesktopPage({ active, onOpen }: { active: boolean; onOpen: (destination: UiDestination) => void }) {
  const [widgets, setWidgets] = useState(readDesktop);
  const [editing, setEditing] = useState(false);
  const [columns, setColumns] = useState(14);
  // Columns stretch so the grid spans the whole canvas width; rows keep the fixed 92px pitch.
  const [pitchX, setPitchX] = useState(desktopPitch);
  const left = (cells: number) => cells * pitchX;
  const width = (cells: number) => cells * pitchX - desktopGap;
  const [drag, setDrag] = useState<Drag>();
  const [libraryOpen, setLibraryOpen] = useState(false);
  const [htmlDraft, setHtmlDraft] = useState<HtmlDraft>();
  const [reply, setReply] = useState<Reply>();
  const [dockCollapsed, setDockCollapsed] = useState(readDockCollapsed);
  const board = useRef<HTMLDivElement>(null);
  const dock = useRef<HTMLElement | null>(null);
  const library = useRef<HTMLDivElement>(null);
  const dragRef = useRef<Drag | undefined>(undefined);
  const columnsRef = useRef(columns);
  const widgetsRef = useRef(widgets);
  const [highlighted, setHighlighted] = useState<string[]>([]);
  dragRef.current = drag;
  columnsRef.current = columns;
  widgetsRef.current = widgets;

  useEffect(() => { writeDesktop(widgets); }, [widgets]);
  useEffect(() => {
    if (!highlighted.length) return;
    board.current?.querySelector(`[data-widget-id="${CSS.escape(highlighted[0]!)}"]`)
      ?.scrollIntoView({ block: 'nearest', inline: 'nearest', behavior: 'smooth' });
    const timer = window.setTimeout(() => setHighlighted([]), 2_400);
    return () => window.clearTimeout(timer);
  }, [highlighted]);
  useEffect(() => {
    try { window.localStorage.setItem(dockCollapsedKey, dockCollapsed ? '1' : '0'); } catch { /* per-viewer convenience */ }
    if (dockCollapsed) setLibraryOpen(false);
  }, [dockCollapsed]);

  useLayoutEffect(() => {
    const element = board.current;
    if (!element) return;
    const measure = () => {
      if (element.clientWidth <= 0) return;
      const count = desktopColumns(element.clientWidth);
      setColumns(count);
      setPitchX((element.clientWidth + desktopGap) / count);
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (!editing && !libraryOpen) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || document.querySelector('dialog:modal')) return;
      if (libraryOpen) setLibraryOpen(false);
      else setEditing(false);
    };
    const onPointer = (event: PointerEvent) => {
      if (libraryOpen && !library.current?.contains(event.target as Node)) setLibraryOpen(false);
    };
    document.addEventListener('keydown', onKey);
    document.addEventListener('pointerdown', onPointer);
    return () => {
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('pointerdown', onPointer);
    };
  }, [editing, libraryOpen]);

  const layout = useMemo(() => new Map(arrangeDesktop(widgets, columns).map((widget) => [widget.id, widget])), [widgets, columns]);
  const target = drag?.target;
  const rows = Math.max(...[...layout.values()].map((widget) => widget.y + widget.h), target ? target.y + target.h : 0, 0);
  const docked = widgets.filter((widget) => widget.docked);
  const topZ = widgets.reduce((max, widget) => Math.max(max, widget.z), 0);

  function update(next: (current: DesktopWidget[]) => DesktopWidget[]) {
    setWidgets((current) => next(current));
  }

  function setDocked(id: string, value: boolean) {
    update((current) => value
      ? current.map((widget) => widget.id === id ? { ...widget, docked: true } : widget)
      : undockWidget(current, id, columns));
  }

  /** Resolves the snapped drop cell from the pointer, or the Dock. Overlap is allowed. */
  function locate(current: Drag, clientX: number, clientY: number): Drag {
    const widget = layout.get(current.id);
    const dx = clientX - current.startX;
    const dy = clientY - current.startY;
    if (!widget) return current;
    if (current.mode === 'resize') {
      const span = (size: number, pitch: number) => Math.max(2, Math.round((size + desktopGap) / pitch));
      return { ...current, dx, dy, overDock: false, target: { x: widget.x, y: widget.y,
        w: Math.min(span(current.width + dx, pitchX), columns - widget.x), h: span(current.height + dy, desktopPitch) } };
    }
    if (inside(dock.current?.getBoundingClientRect(), clientX, clientY)) return { ...current, dx, dy, overDock: true, target: undefined };
    return { ...current, dx, dy, overDock: false, target: { w: widget.w, h: widget.h,
      x: Math.min(Math.max(0, Math.round((current.left + dx) / pitchX)), columns - widget.w), y: snapToCell(current.top + dy) } };
  }

  function beginDrag(event: ReactPointerEvent<HTMLElement>, widget: DesktopWidget, mode: Drag['mode']) {
    if (event.button !== 0) return;
    if (mode === 'move' && (event.target as HTMLElement).closest('button')) return;
    const placed = layout.get(widget.id);
    if (!placed) return;
    event.preventDefault();
    event.stopPropagation();
    setDrag({ id: widget.id, mode, startX: event.clientX, startY: event.clientY, dx: 0, dy: 0,
      left: left(placed.x), top: placed.y * desktopPitch, width: width(placed.w), height: pixels(placed.h),
      target: { x: placed.x, y: placed.y, w: placed.w, h: placed.h }, overDock: false });
  }

  useEffect(() => {
    if (!drag) return;
    const move = (event: PointerEvent) => setDrag((current) => current && locate(current, event.clientX, event.clientY));
    const finish = (event: PointerEvent) => {
      const current = dragRef.current;
      setDrag(undefined);
      if (!current || event.type === 'pointercancel') return;
      const settled = locate(current, event.clientX, event.clientY);
      if (settled.overDock) setDocked(current.id, true);
      else if (settled.target) update((list) => placeWidget(list, current.id, settled.target!, columns));
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', finish);
    window.addEventListener('pointercancel', finish);
    return () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', finish);
      window.removeEventListener('pointercancel', finish);
    };
  }, [drag?.id, drag?.mode, layout, columns]);

  function resizeTo(widget: DesktopWidget, size: DesktopWidgetSize) {
    update((current) => placeWidget(current, widget.id, { x: widget.x, y: widget.y, ...sizeCells(size) }, columns));
  }

  function add(type: DesktopWidgetType, extra: Partial<DesktopWidget> = {}, size = defaultWidgetSize[type]) {
    update((current) => showOrAddWidget(current, { id: `${type}-${shortId()}`, type, ...sizeCells(size), ...extra }, columns));
    setLibraryOpen(false);
  }

  function saveHtml(draft: HtmlDraft) {
    const title = draft.title.trim() || 'HTML 挂件';
    if (draft.id) update((current) => current.map((widget) => widget.id === draft.id ? { ...widget, title, html: draft.html } : widget));
    else add('html', { title, html: draft.html }, draft.size);
    setHtmlDraft(undefined);
  }

  /** Sends one message to the canvas agent, streams its progress into the conversation log, then applies its widget changes. */
  async function ask(message: string): Promise<boolean> {
    const desktop = window.yuanpu;
    if (!desktop) {
      appendDesktopConversation({ role: 'user', text: message });
      const text = '这是浏览器预览。通过桌面应用启动后，桌面 Agent 会根据你的话摆放或更新挂件。';
      appendDesktopConversation({ role: 'assistant', text });
      setReply({ tone: 'done', text });
      return true;
    }
    // The renderer hot-reloads; the preload bridge only changes after the desktop app restarts.
    if (typeof desktop.submitDesktopCanvasMessage !== 'function') {
      setReply({ tone: 'error', text: '桌面 Agent 接口还没加载。请重启应用（重新运行 `pnpm dev`）后再发送。' });
      return false;
    }
    appendDesktopConversation({ role: 'user', text: message });
    const startedAt = new Date().toISOString();
    const entryId = appendDesktopConversation({ role: 'assistant', text: '',
      run: { status: 'queued', createdAt: startedAt, updatedAt: startedAt, events: [], tools: [], steps: [] } });
    setReply({ tone: 'running', text: '正在处理…' });
    let recorded = '';
    const record = (run: AgentRunRecord, text: string, role: 'assistant' | 'error' = 'assistant') => {
      const info: DesktopReplyRunInfo = { runId: run.runId, source: 'runtime', status: run.status, createdAt: run.createdAt,
        updatedAt: run.updatedAt, events: [], tools: run.output?.tools ?? [], steps: run.output?.steps ?? run.progress?.steps ?? [] };
      const snapshot = JSON.stringify([role, text, info.status, info.steps?.map((step) => [step.id, step.status, step.result?.length])]);
      if (snapshot === recorded) return;
      recorded = snapshot;
      updateDesktopConversation(entryId, { role, text, run: info });
    };
    const fail = (text: string) => {
      updateDesktopConversation(entryId, { role: 'error', text });
      setReply({ tone: 'error', text });
      return false;
    };
    try {
      const receipt = await desktop.submitDesktopCanvasMessage(message, summarizeDesktop(widgetsRef.current, titleOf));
      while (true) {
        const run = await desktop.getAgentRun(receipt.runId);
        if (run.status === 'waiting_approval') {
          const text = '这一步需要授权。请到“对话”页查看并处理授权请求。';
          record(run, text, 'error');
          setReply({ tone: 'error', text });
          return true;
        }
        if (terminalStatuses.includes(run.status)) {
          const actions = run.output?.desktopActions ?? [];
          const missing = actions.length ? applyAgentActions(actions) : [];
          const succeeded = run.status === 'succeeded';
          // Say what actually reached the canvas, so a lost action is visible instead of a silent no-op.
          const canvasCalls = (run.output?.steps ?? []).filter((step) => step.name.startsWith('desktop_') && step.status === 'completed').length;
          const notes = [
            actions.length ? `已在桌面执行 ${actions.length} 个动作。` : '',
            canvasCalls && !actions.length ? `Agent 调用了 ${canvasCalls} 次桌面工具，但运行结果里没有带回桌面动作，桌面没有变化。` : '',
            missing.length ? `桌面上没有找到挂件：${missing.join('、')}。可以让 Agent 重新创建。` : '',
          ].filter(Boolean);
          const text = (succeeded ? run.output?.message ?? '完成。' : run.failure?.message ?? `任务结束：${run.status}`)
            + (notes.length ? `\n\n> ${notes.join(' ')}` : '');
          record(run, text, succeeded ? 'assistant' : 'error');
          setReply({ tone: succeeded ? 'done' : 'error', text });
          return succeeded;
        }
        record(run, run.progress?.text ?? '');
        await new Promise((resolveWait) => window.setTimeout(resolveWait, 900));
      }
    } catch (error) {
      return fail(error instanceof Error ? error.message : String(error));
    }
  }

  /** Applies agent widget changes, reveals every widget the agent pointed at, and returns ids it named that do not exist. */
  function applyAgentActions(actions: DesktopCanvasAction[]): string[] {
    const before = widgetsRef.current;
    const next = applyDesktopActions(before, actions, columnsRef.current, shortId);
    widgetsRef.current = next;
    setWidgets(next);
    const { touched, missing } = resolveDesktopActions(before, next, actions);
    if (touched.length) setHighlighted(touched);
    return missing;
  }

  function frame(widget: DesktopWidget) {
    const placed = layout.get(widget.id);
    const dragging = drag?.id === widget.id;
    const opens = catalog[widget.type].opens;
    // Docked widgets stay mounted (hidden) so HTML frames keep their state.
    const style: CSSProperties = !placed ? {}
      : dragging && drag.mode === 'move' ? { left: drag.left + drag.dx, top: drag.top + drag.dy, width: drag.width, height: drag.height, zIndex: topZ + 2 }
        : dragging ? { left: drag.left, top: drag.top, width: Math.max(width(2), drag.width + drag.dx),
          height: Math.max(pixels(2), drag.height + drag.dy), zIndex: topZ + 2 }
          : { left: left(placed.x), top: placed.y * desktopPitch, width: width(placed.w), height: pixels(placed.h), zIndex: widget.z };
    return <article key={widget.id} data-widget-id={widget.id}
      className={`desktop-widget desktop-widget-${widget.type} ${dragging ? 'dragging' : ''} ${placed ? '' : 'docked'} ${highlighted.includes(widget.id) ? 'highlight' : ''}`}
      style={style} aria-label={titleOf(widget)} aria-hidden={!placed}
      onPointerDown={() => { if (widget.z !== topZ) update((current) => bringToFront(current, widget.id)); }}>
      <header className="desktop-widget-header" onPointerDown={(event) => beginDrag(event, widget, 'move')}>
        <AppIcon name={catalog[widget.type].icon} />
        <strong>{titleOf(widget)}</strong>
        <div className="desktop-widget-actions">
          {editing && widget.type === 'html' && <button type="button" title="编辑 HTML" aria-label="编辑 HTML"
            onClick={() => setHtmlDraft({ id: widget.id, title: titleOf(widget), html: widget.html ?? '', size: 'medium' })}>
            <AppIcon name="edit" /></button>}
          {!editing && opens && <button type="button" title="打开页面" aria-label={`打开${catalog[widget.type].title}`}
            onClick={() => onOpen(opens)}><AppIcon name="external" /></button>}
          <button type="button" title="收进 Dock" aria-label={`把${titleOf(widget)}收进 Dock`} onClick={() => setDocked(widget.id, true)}>
            <AppIcon name="arrow-down" /></button>
          {editing && <button type="button" title="移除" aria-label={`移除${titleOf(widget)}`}
            onClick={() => update((current) => current.filter((item) => item.id !== widget.id))}>×</button>}
        </div>
      </header>
      <div className="desktop-widget-body"><WidgetBody widget={widget} active={active} /></div>
      {editing && <span className="desktop-size-group" role="group" aria-label="挂件尺寸">
        {(Object.keys(desktopWidgetSizes) as DesktopWidgetSize[]).map((size) => {
          const preset = desktopWidgetSizes[size];
          return <button key={size} type="button" aria-pressed={widget.w === preset.w && widget.h === preset.h}
            title={`${preset.label} ${preset.w}×${preset.h}`} onClick={() => resizeTo(widget, size)}>{preset.label}</button>;
        })}
      </span>}
      <span className="desktop-resize-handle" aria-hidden="true" onPointerDown={(event) => beginDrag(event, widget, 'resize')} />
    </article>;
  }

  return <section className={`desktop-page ${dockCollapsed ? 'dock-collapsed' : ''} ${editing ? 'editing' : ''} ${drag ? 'is-dragging' : ''} ${active ? '' : 'view-hidden'}`}
    aria-hidden={!active} aria-label="桌面">
    {dockCollapsed ? <button ref={(element) => { dock.current = element; }} type="button"
      className={`desktop-dock-launcher ${drag?.mode === 'move' ? 'drop-target' : ''} ${drag?.overDock ? 'drop-hover' : ''}`}
      title="展开 Dock" aria-label={`展开 Dock${docked.length ? `，${docked.length} 个收起的挂件` : ''}`} aria-expanded={false}
      onClick={() => setDockCollapsed(false)}>
      <AppIcon name="more" />{docked.length > 0 && <span className="desktop-dock-badge">{docked.length}</span>}
    </button>
    : <nav ref={dock} className={`desktop-dock ${drag?.mode === 'move' ? 'drop-target' : ''} ${drag?.overDock ? 'drop-hover' : ''}`}
      aria-label="Dock">
      <div className="desktop-dock-items">
        {docked.map((widget) => <button key={widget.id} type="button" className="desktop-dock-item" title={`打开 ${titleOf(widget)}`}
          onClick={() => setDocked(widget.id, false)}>
          <AppIcon name={catalog[widget.type].icon} /><span>{titleOf(widget)}</span>
        </button>)}
        {!docked.length && drag?.mode === 'move' && <span className="desktop-dock-hint">放到这里收起</span>}
      </div>
      <span className="desktop-dock-divider" aria-hidden="true" />
      <div ref={library} className="desktop-library-anchor">
        <button type="button" className="desktop-dock-item" title="添加挂件" aria-haspopup="menu" aria-expanded={libraryOpen}
          onClick={() => setLibraryOpen((open) => !open)}><AppIcon name="plus" /><span>添加</span></button>
        {libraryOpen && <div className="desktop-library" role="menu" aria-label="挂件库">
          {builtinWidgetTypes.map((type) => <button key={type} type="button" role="menuitem" onClick={() => add(type)}>
            <AppIcon name={catalog[type].icon} /><span>{catalog[type].title}</span>
            <small>{desktopWidgetSizes[defaultWidgetSize[type]].w}×{desktopWidgetSizes[defaultWidgetSize[type]].h}</small>
          </button>)}
          <button type="button" role="menuitem" onClick={() => { setLibraryOpen(false); setHtmlDraft({ title: '', html: '', size: 'medium' }); }}>
            <AppIcon name="code" /><span>HTML 挂件…</span><small>手动</small>
          </button>
          <hr />
          <button type="button" role="menuitem" onClick={() => {
            if (!window.confirm('恢复默认桌面布局？当前的挂件和位置会被替换。')) return;
            setWidgets(defaultDesktop());
            setLibraryOpen(false);
          }}><AppIcon name="refresh" /><span>恢复默认布局</span></button>
        </div>}
      </div>
      <button type="button" className={`desktop-dock-item ${editing ? 'active' : ''}`} aria-pressed={editing}
        title={editing ? '完成编辑' : '编辑布局'} onClick={() => setEditing((value) => !value)}>
        <AppIcon name={editing ? 'check' : 'edit'} /><span>{editing ? '完成' : '编辑'}</span>
      </button>
      <button type="button" className="desktop-dock-item desktop-dock-collapse" title="收起 Dock" aria-label="收起 Dock" aria-expanded
        onClick={() => setDockCollapsed(true)}>
        <AppIcon name="panel-left" /><span>收起</span>
      </button>
    </nav>}

    <div className="desktop-main">
      <div className="desktop-scroll">
        <div ref={board} className="desktop-board" style={{ height: Math.max(rows + (drag ? 2 : 0), 1) * desktopPitch,
          ['--desktop-pitch-x' as string]: `${pitchX}px` }}>
          {target && <div className="desktop-ghost" style={{ left: left(target.x), top: target.y * desktopPitch,
            width: width(Math.min(target.w, columns)), height: pixels(target.h), zIndex: topZ + 1 }} />}
          {/* Stored order keeps DOM nodes stable, so moving a widget never reloads its frame. */}
          {widgets.map(frame)}
          {!layout.size && <p className="desktop-empty">桌面上没有挂件。点左侧 Dock 里的挂件放回来，或在下方告诉 Agent 想看什么。</p>}
        </div>
      </div>
      {/* With the conversation widget on the canvas, replies live there only; otherwise a small popover shows the result. */}
      <DesktopComposer reply={layout.size && [...layout.values()].some((widget) => widget.type === 'conversation') ? undefined : reply}
        busy={reply?.tone === 'running'} onDismiss={() => setReply(undefined)} onSubmit={ask} />
    </div>

    {htmlDraft && <HtmlWidgetDialog draft={htmlDraft} onCancel={() => setHtmlDraft(undefined)} onSave={saveHtml} />}
  </section>;
}

/** The fixed bottom-centre input. It is not bound to a transcript; only the latest reply is shown. */
function DesktopComposer({ reply, busy, onDismiss, onSubmit }: {
  reply?: Reply;
  busy: boolean;
  onDismiss: () => void;
  onSubmit: (message: string) => Promise<boolean>;
}) {
  const [input, setInput] = useState('');
  async function send() {
    const message = input.trim();
    if (!message || busy) return;
    setInput('');
    if (!await onSubmit(message)) setInput((current) => current || message);
  }
  return <div className="desktop-composer-dock">
    {/* Progress lives in the conversation widget; above the composer only the finished reply appears. */}
    {reply && !busy && <div className={`desktop-reply desktop-reply-${reply.tone}`} role={reply.tone === 'error' ? 'alert' : 'status'}>
      <div className="desktop-reply-body"><MessageContent text={reply.text} /></div>
      <button type="button" aria-label="关闭回复" title="关闭" onClick={onDismiss}>×</button>
    </div>}
    <form className="desktop-composer" onSubmit={(event) => { event.preventDefault(); void send(); }}>
      <textarea value={input} rows={1} placeholder="告诉桌面 Agent 想看什么，例如：把本周定时任务做成一张表" aria-label="桌面 Agent 输入"
        onChange={(event) => setInput(event.target.value)}
        onKeyDown={(event) => {
          if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
          event.preventDefault();
          void send();
        }} />
      <button type="submit" disabled={!input.trim() || busy} aria-label="发送"><AppIcon name="send" /></button>
    </form>
  </div>;
}

function WidgetBody({ widget, active }: { widget: DesktopWidget; active: boolean }) {
  switch (widget.type) {
    case 'clock': return <ClockWidget />;
    case 'knowledge': return <KnowledgeWidget active={active} />;
    case 'skills': return <SkillsWidget active={active} />;
    case 'schedules': return <SchedulesWidget active={active} />;
    case 'conversation': return <ConversationWidget />;
    case 'html': return <iframe className="desktop-html-frame" title={titleOf(widget)} srcDoc={htmlPreviewDocument(widget.html ?? '')}
      sandbox="" referrerPolicy="no-referrer" />;
  }
}

function ConversationWidget() {
  const [entries, setEntries] = useState(readDesktopConversation);
  const end = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const refresh = () => setEntries(readDesktopConversation());
    window.addEventListener(desktopConversationEvent, refresh);
    return () => window.removeEventListener(desktopConversationEvent, refresh);
  }, []);
  const last = entries.at(-1);
  // Scroll only the widget body; scrollIntoView would also pan the canvas and the body sideways.
  useEffect(() => {
    const body = end.current?.closest('.desktop-widget-body');
    if (body) body.scrollTop = body.scrollHeight;
  }, [entries.length, last?.text.length, last?.run?.steps?.length]);
  if (!entries.length) return <p className="desktop-widget-empty">还没有对话。在下方输入框告诉桌面 Agent 想看什么。</p>;
  return <div className="desktop-conversation">
    {entries.map((entry) => <div key={entry.id} className={`desktop-conversation-${entry.role}`}>
      {entry.role === 'user' ? <p>{entry.text}</p> : <>
        {entry.run && <ReplyRunDetails run={entry.run} />}
        {entry.text ? <MessageContent text={entry.text} />
          : entry.run && ['queued', 'running'].includes(entry.run.status) && <span className="desktop-reply-pending"><i aria-hidden="true" />正在处理…</span>}
      </>}
    </div>)}
    <button type="button" className="desktop-conversation-clear" onClick={() => { if (window.confirm('清空桌面对话记录？')) clearDesktopConversation(); }}>清空记录</button>
    <div ref={end} />
  </div>;
}

function ClockWidget() {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), 15_000);
    return () => window.clearInterval(timer);
  }, []);
  return <div className="desktop-clock">
    <time dateTime={now.toISOString()}>{now.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}</time>
    <span>{now.toLocaleDateString('zh-CN', { month: 'long', day: 'numeric', weekday: 'short' })}</span>
  </div>;
}

function KnowledgeWidget({ active }: { active: boolean }) {
  const [items, setItems] = useState(readSavedContent);
  useEffect(() => {
    const refresh = () => setItems(readSavedContent());
    window.addEventListener(savedContentEvent, refresh);
    if (active) refresh();
    return () => window.removeEventListener(savedContentEvent, refresh);
  }, [active]);
  const knowledge = items.filter((item) => item.kind === 'knowledge');
  if (!knowledge.length) return <p className="desktop-widget-empty">还没有知识库资料。在助手回复下方点“保存到知识库”即可添加。</p>;
  return <ul className="desktop-list">{knowledge.slice(0, 20).map((item) => <li key={item.id}>
    <span>{item.text.replace(/[#>*`_-]/g, '').trim().split('\n')[0]}</span>
    <small>{new Date(item.savedAt).toLocaleDateString('zh-CN')}</small>
  </li>)}</ul>;
}

function SkillsWidget({ active }: { active: boolean }) {
  const desktop = window.yuanpu;
  const [plugins, setPlugins] = useState<Array<Pick<InstalledPlugin, 'name' | 'version' | 'enabled'>>>(desktop ? [] : previewPlugins);
  const [error, setError] = useState(false);
  useEffect(() => {
    if (!desktop || !active) return;
    void desktop.listPlugins().then((list) => { setPlugins(list); setError(false); }).catch(() => setError(true));
  }, [desktop, active]);
  if (error) return <p className="desktop-widget-empty">技能列表暂时无法读取。</p>;
  if (!plugins.length) return <p className="desktop-widget-empty">还没有安装技能。</p>;
  return <ul className="desktop-list">{plugins.map((plugin) => <li key={plugin.name}>
    <span>{plugin.name}</span><small className={plugin.enabled ? 'on' : ''}>{plugin.enabled ? '已启用' : '已停用'} · {plugin.version}</small>
  </li>)}</ul>;
}

function SchedulesWidget({ active }: { active: boolean }) {
  const desktop = window.yuanpu;
  const [schedules, setSchedules] = useState<Array<Pick<ScheduleRecord, 'scheduleId' | 'name' | 'enabled' | 'nextTriggerAt'>>>(
    desktop ? [] : [{ scheduleId: 'preview', name: '每日工作简报', enabled: true, nextTriggerAt: new Date(Date.now() + 3_600_000).toISOString() }]);
  const [error, setError] = useState(false);
  useEffect(() => {
    if (!desktop || !active) return;
    void desktop.listSchedules().then((list) => { setSchedules(list); setError(false); }).catch(() => setError(true));
  }, [desktop, active]);
  if (error) return <p className="desktop-widget-empty">定时任务暂时无法读取。</p>;
  const upcoming = schedules.filter((item) => item.enabled && item.nextTriggerAt)
    .sort((a, b) => (a.nextTriggerAt ?? '').localeCompare(b.nextTriggerAt ?? ''));
  if (!upcoming.length) return <p className="desktop-widget-empty">没有待执行的定时任务。</p>;
  return <ul className="desktop-list">{upcoming.slice(0, 6).map((item) => <li key={item.scheduleId}>
    <span>{item.name}</span>
    <small>{new Date(item.nextTriggerAt!).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</small>
  </li>)}</ul>;
}

function HtmlWidgetDialog({ draft, onCancel, onSave }: { draft: HtmlDraft; onCancel: () => void; onSave: (draft: HtmlDraft) => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [value, setValue] = useState(draft);
  useEffect(() => {
    const element = dialog.current;
    element?.showModal();
    return () => { if (element?.open) element.close(); };
  }, []);
  return <dialog ref={dialog} className="desktop-html-dialog" onCancel={(event) => { event.preventDefault(); onCancel(); }}>
    <form method="dialog" onSubmit={(event) => { event.preventDefault(); onSave(value); }}>
      <h2>{draft.id ? '编辑 HTML 挂件' : '新建 HTML 挂件'}</h2>
      <p>模拟 Agent 生成的挂件。内容在无脚本沙箱中渲染，不能访问本机数据。</p>
      <label>标题<input value={value.title} placeholder="HTML 挂件" onChange={(event) => setValue({ ...value, title: event.target.value })} /></label>
      {!draft.id && <label>尺寸<select value={value.size} onChange={(event) => setValue({ ...value, size: event.target.value as DesktopWidgetSize })}>
        {(Object.keys(desktopWidgetSizes) as DesktopWidgetSize[]).map((size) => <option key={size} value={size}>
          {desktopWidgetSizes[size].label} · {desktopWidgetSizes[size].w}×{desktopWidgetSizes[size].h}</option>)}
      </select></label>}
      <label>HTML<textarea value={value.html} rows={10} spellCheck={false} placeholder="<h2>标题</h2><p>内容</p>"
        onChange={(event) => setValue({ ...value, html: event.target.value })} /></label>
      <div className="desktop-html-dialog-actions">
        <button type="button" onClick={onCancel}>取消</button>
        <button type="submit" className="primary" disabled={!value.html.trim()}>保存</button>
      </div>
    </form>
  </dialog>;
}
