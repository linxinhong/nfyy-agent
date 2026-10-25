import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react';
import { useQuery } from '@tanstack/react-query';
import type { AgentRunRequest, AiTicket, DesktopBridge } from '@yuanpu-agent/protocol';
import { AppIcon } from '../shared/app-icon.js';
import { acceptComposerPlaceholder } from '../shared/composer-placeholder.js';
import { ComposerRuntimeControls, threeStepThinking } from '../shared/composer-runtime-controls.js';
import { AssistantReply } from '../shared/assistant-reply.js';
import './ai-tickets.css';

const statusText: Record<AiTicket['status'], string> = {
  queued: '等待执行', running: '正在执行', completed: '已完成', failed: '失败',
  cancelled: '已取消', interrupted: '已中断',
  needs_approval: '等待授权',
};

function shortTitle(ticket: AiTicket): string {
  return ticket.title || ticket.task.split('\n')[0]?.slice(0, 80) || '未命名工单';
}
function orderNumber(ticket: AiTicket): string {
  const date = new Date(ticket.createdAt);
  return `WO-${String(date.getMonth() + 1).padStart(2, '0')}${String(date.getDate()).padStart(2, '0')}-${ticket.id.slice(0, 4).toUpperCase()}`;
}
function elapsed(ticket: AiTicket): string {
  const end = ticket.status === 'running' ? Date.now() : Date.parse(ticket.updatedAt);
  const seconds = Math.max(0, Math.floor((end - Date.parse(ticket.createdAt)) / 1000));
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}

function TicketFace({ ticket, expanded = false }: { ticket: AiTicket; expanded?: boolean }) {
  return <>
    <span className="ai-ticket-brief-heading"><small>WORK ORDER</small><strong>{shortTitle(ticket)}</strong></span>
    <span className="ai-ticket-stamp">{statusText[ticket.status]}</span>
    <span className="ai-ticket-number"><small>NO.</small><b>{orderNumber(ticket)}</b></span>
    {!expanded && <span className="ai-ticket-elapsed"><small>耗时</small><b>{elapsed(ticket)}</b></span>}
    <span className="ai-ticket-fold" aria-hidden="true"><AppIcon name="chevron" /></span>
  </>;
}

export function AiTicketsPanel({ conversationId, desktop }: {
  conversationId: string;
  desktop: DesktopBridge;
}) {
  const [tickets, setTickets] = useState<AiTicket[]>([]);
  const [selectedId, setSelectedId] = useState<string>();
  const [selected, setSelected] = useState<AiTicket>();
  const [listOpen, setListOpen] = useState(false);
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const [task, setTask] = useState('');
  const [title, setTitle] = useState('');
  const [deliverables, setDeliverables] = useState('');
  const [acceptanceCriteria, setAcceptanceCriteria] = useState('');
  const [draft, setDraft] = useState('');
  const [attachments, setAttachments] = useState<Array<{ name: string; contents: string }>>([]);
  const [attachmentError, setAttachmentError] = useState('');
  const [modelSelection, setModelSelection] = useState<AgentRunRequest['modelSelection']>();
  const [modelOpen, setModelOpen] = useState(false);
  const [contextOpen, setContextOpen] = useState(false);
  const [fullAccessEnabled, setFullAccessEnabled] = useState(false);
  const [redactionPreviewEnabled, setRedactionPreviewEnabled] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const attachmentInput = useRef<HTMLInputElement>(null);
  const messagesEnd = useRef<HTMLDivElement>(null);
  const chatScroll = useRef<HTMLDivElement>(null);
  const chatMessages = useRef<HTMLDivElement>(null);
  const followTicketBottom = useRef(true);
  const navigationRevision = useRef(-1);
  const selectedRef = useRef(selectedId);
  selectedRef.current = selectedId;
  const modelSettings = useQuery({ queryKey: ['settings', 'model'], queryFn: () => desktop.getModelSettings() });
  const configuredModel = modelSettings.data;
  const savedModel = selected?.modelSelection ?? (selected?.model?.includes('/')
    ? { provider: selected.model.slice(0, selected.model.indexOf('/')),
      model: selected.model.slice(selected.model.indexOf('/') + 1) } : undefined);
  const currentModel = modelSelection ?? savedModel ?? (configuredModel?.provider && configuredModel.model
    ? { provider: configuredModel.provider, model: configuredModel.model } : undefined);
  const modelChoices = configuredModel ? [
    { provider: configuredModel.provider, model: configuredModel.model, name: configuredModel.model },
    ...configuredModel.customModels.map((item) => ({ provider: item.provider, model: item.model, name: item.name })),
  ].filter((item, index, all) => all.findIndex((other) => other.provider === item.provider && other.model === item.model) === index) : [];

  useEffect(() => {
    setDraft(''); setAttachments([]); setAttachmentError('');
    setModelOpen(false); setContextOpen(false); setRedactionPreviewEnabled(false);
    if (!selectedId) { setModelSelection(undefined); setFullAccessEnabled(false); return; }
    try {
      const saved = JSON.parse(window.localStorage.getItem(`yuanpu:ticket:model:${selectedId}`) ?? 'null') as
        AgentRunRequest['modelSelection'] | null;
      setModelSelection(saved?.provider && saved.model
        ? { ...saved, thinkingLevel: threeStepThinking(saved.thinkingLevel) } : undefined);
      setFullAccessEnabled(window.localStorage.getItem(`yuanpu:ticket:full-access:${selectedId}`) === 'true');
    } catch { setModelSelection(undefined); setFullAccessEnabled(false); }
  }, [selectedId]);

  const chooseModel = (next: NonNullable<AgentRunRequest['modelSelection']>) => {
    setModelSelection(next);
    if (selectedId) try { window.localStorage.setItem(`yuanpu:ticket:model:${selectedId}`, JSON.stringify(next)); }
      catch { /* Selection still applies to this ticket in memory. */ }
  };
  const toggleFullAccess = () => {
    if (!selectedId || busy) return;
    const next = !fullAccessEnabled;
    setFullAccessEnabled(next);
    try {
      if (next) window.localStorage.setItem(`yuanpu:ticket:full-access:${selectedId}`, 'true');
      else window.localStorage.removeItem(`yuanpu:ticket:full-access:${selectedId}`);
    } catch { /* Current ticket retains the chosen mode. */ }
  };

  useEffect(() => {
    let stopped = false;
    let timer: number | undefined;
    const refresh = async () => {
      try {
        const list = await desktop.listAiTickets(conversationId);
        if (stopped) return;
        setTickets(list.tickets);
        let id = selectedRef.current;
        if (list.navigation.revision > navigationRevision.current) {
          navigationRevision.current = list.navigation.revision;
          id = list.navigation.ticketId;
          setSelectedId(id);
        }
        if (id) {
          const detail = await desktop.getAiTicket(conversationId, id);
          if (!stopped && id === selectedRef.current) setSelected(detail);
        }
      } catch (cause) {
        if (!stopped) setError(cause instanceof Error ? cause.message : String(cause));
      }
      if (!stopped) timer = window.setTimeout(() => void refresh(), selectedRef.current ? 1_500 : 4_000);
    };
    void refresh();
    return () => { stopped = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, [conversationId, desktop]);

  useEffect(() => {
    followTicketBottom.current = true;
    if (!selectedId) { setSelected(undefined); return; }
    setSelected(undefined);
    setDetailsOpen(false);
    setError('');
    void desktop.getAiTicket(conversationId, selectedId).then((ticket) => {
      if (ticket.id === selectedRef.current) setSelected(ticket);
    }).catch((cause) => setError(cause instanceof Error ? cause.message : String(cause)));
  }, [conversationId, desktop, selectedId]);

  useLayoutEffect(() => {
    const pane = chatScroll.current;
    if (pane && followTicketBottom.current) pane.scrollTop = pane.scrollHeight;
  }, [selected?.messages?.length, selected?.progress]);

  useLayoutEffect(() => {
    const pane = chatScroll.current;
    const content = chatMessages.current;
    if (!pane || !content || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (followTicketBottom.current) pane.scrollTop = pane.scrollHeight;
    });
    observer.observe(pane);
    observer.observe(content);
    return () => observer.disconnect();
  }, [selected?.id]);

  const create = async () => {
    if (!task.trim() || busy) return;
    setBusy(true); setError('');
    try {
      const ticket = await desktop.createAiTicket(conversationId, task.trim(), {
        ...(title.trim() ? { title: title.trim() } : {}),
        deliverables: deliverables.split('\n').map((item) => item.trim()).filter(Boolean),
        acceptanceCriteria: acceptanceCriteria.split('\n').map((item) => item.trim()).filter(Boolean),
      });
      setTickets((current) => [ticket, ...current]);
      setTask(''); setTitle(''); setDeliverables(''); setAcceptanceCriteria('');
      setCreating(false); setSelectedId(ticket.id);
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { setBusy(false); }
  };
  const send = async () => {
    if (!selectedId || (!draft.trim() && !attachments.length) || busy
      || selected?.status === 'running' || selected?.status === 'queued') return;
    const message = attachments.length
      ? `${draft.trim() || '请阅读附件。'}\n\n${attachments.map((file) => `附件 ${file.name}：\n${file.contents}`).join('\n\n')}`
      : draft.trim();
    setBusy(true); setError('');
    try {
      const ticket = await desktop.sendAiTicketMessage(conversationId, selectedId, message, {
        modelSelection: modelSelection ?? savedModel,
        approvalMode: fullAccessEnabled ? 'unrestricted' : 'required',
      });
      setSelected(ticket); setDraft(''); setAttachments([]); setAttachmentError('');
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { setBusy(false); }
  };
  const addAttachments = async (files: FileList | null) => {
    const chosen = Array.from(files ?? []);
    if (attachmentInput.current) attachmentInput.current.value = '';
    const remaining = Math.max(0, 3 - attachments.length);
    if (chosen.length > remaining) setAttachmentError('每条消息最多添加 3 个文本附件。');
    else setAttachmentError('');
    const added: Array<{ name: string; contents: string }> = [];
    for (const file of chosen.slice(0, remaining)) {
      if (file.size > 100_000 || !/\.(txt|md|json|csv|ts|tsx|js|jsx|py|yaml|yml|xml|html|css)$/i.test(file.name)) {
        setAttachmentError('仅支持 100 KB 以内的文本附件。');
        continue;
      }
      added.push({ name: file.name, contents: await file.text() });
    }
    if (added.length) setAttachments((current) => [...current, ...added]);
  };
  const cancel = async () => {
    if (!selectedId || busy) return;
    setBusy(true); setError('');
    try { await desktop.cancelAiTicket(conversationId, selectedId);
      setSelected(await desktop.getAiTicket(conversationId, selectedId));
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); }
    finally { setBusy(false); }
  };
  const onEnter = (event: KeyboardEvent<HTMLTextAreaElement>, action: () => void) => {
    if (acceptComposerPlaceholder(event, setDraft)) return;
    if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault(); action();
    }
  };
  const openTicket = (id: string) => { setSelectedId(id); setListOpen(false); setCreating(false); };

  return <div className="ai-tickets-panel">
    <nav className="ai-tickets-nav" aria-label="工单导航">
      <button type="button" className={!selectedId ? 'active' : ''}
        onClick={() => { setSelectedId(undefined); setCreating(false); }}>总览</button>
      <button type="button" className={selectedId ? 'active' : ''} aria-expanded={listOpen} onClick={() => setListOpen((open) => !open)}>
        工单列表 <span>{tickets.length}</span> <AppIcon name="chevron" />
      </button>
      <button type="button" className="ai-ticket-create-trigger"
        onClick={() => { setSelectedId(undefined); setCreating(true); }}>新建工单</button>
    </nav>
    {listOpen && <div className="ai-ticket-list" role="list">
      {tickets.map((ticket) => <button type="button" role="listitem" key={ticket.id}
        className={ticket.id === selectedId ? 'active' : ''} onClick={() => openTicket(ticket.id)}>
        <span className={`ai-ticket-dot ${ticket.status}`} aria-hidden="true" />
        <span>{shortTitle(ticket)}</span><small>{statusText[ticket.status]}</small>
      </button>)}
      {!tickets.length && <p>暂无工单</p>}
    </div>}
    {error && <p className="ai-ticket-error" role="alert">{error}</p>}
    {selectedId && selected?.id === selectedId ? <div className="ai-ticket-workspace">
      <section className={`ai-ticket-brief ${selected.status}`} aria-label="工单内容">
        <button type="button" className="ai-ticket-brief-toggle" aria-expanded={detailsOpen}
          onClick={() => setDetailsOpen((open) => !open)}>
          <TicketFace ticket={selected} expanded={detailsOpen} />
        </button>
        {detailsOpen && <div className="ai-ticket-brief-body">
          <div className="ai-ticket-facts">
            <div><small>执行者</small><strong>工单 Agent</strong></div>
            <div><small>模型</small><strong title={selected.model}>{selected.model?.split('/').at(-1) ?? '当前配置'}</strong></div>
            <div><small>派单时间</small><strong>{new Date(selected.createdAt).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })}</strong></div>
            <div><small>耗时</small><strong>{elapsed(selected)}</strong></div>
          </div>
          <div className="ai-ticket-fields">
            <span>任务描述</span><p>{selected.task}</p>
            {Boolean(selected.deliverables?.length) && <><span>交付物</span><div className="ai-ticket-deliverables">
              {selected.deliverables?.map((item, index) => <span key={`${item}-${index}`}>{item}</span>)}
            </div></>}
            {Boolean(selected.acceptanceCriteria?.length) && <><span>验收项</span><ol className="ai-ticket-criteria">
              {selected.acceptanceCriteria?.map((item, index) => <li key={`${item}-${index}`}>
                <small>{String(index + 1).padStart(2, '0')}</small><span>{item}</span>
              </li>)}
            </ol></>}
          </div>
          <details className="ai-ticket-dispatch"><summary>下发信息 <small>主会话 → 独立工单会话</small></summary>
            <div><span>来源</span><span>当前工作会话</span><span>模型</span><span>{selected.model ?? '当前配置'}</span>
              <span>工单编号</span><span>{orderNumber(selected)}</span></div>
          </details>
          {selected.error && <p className="ai-ticket-error">{selected.error}</p>}
          {selected.status === 'running' && <button type="button" className="ai-ticket-cancel" disabled={busy}
            onClick={() => void cancel()}>停止当前任务</button>}
        </div>}
      </section>
      <div className="ai-ticket-chat" ref={chatScroll} role="log" aria-label="工单对话" onScroll={() => {
        const pane = chatScroll.current;
        if (pane) followTicketBottom.current = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 80;
      }}>
        <div className="ai-ticket-messages" ref={chatMessages}>
          {!selected.messages?.length && <p className="ai-ticket-muted">正在创建工单会话…</p>}
          {selected.messages?.map((message) => <article className={`message ${message.role}`} key={message.id}>
            <div className="message-label">{message.role === 'user' ? '你' : 'YuanpuAgent'}</div>
            <div className="message-body">{message.role === 'assistant'
              ? <AssistantReply text={message.text} surface="work" run={message.run} /> : <p>{message.text}</p>}</div>
          </article>)}
          {selected.status === 'running' && <article className="message assistant pending">
            <div className="message-label">YuanpuAgent</div>
            {selected.progress || selected.liveRun ? <div className="message-body"><AssistantReply text={selected.progress ?? ''}
              surface="work" run={selected.liveRun} pending /></div>
              : <div className="thinking"><span /><span /><span /> 正在处理</div>}
          </article>}
          <div ref={messagesEnd} />
        </div>
      </div>
      <div className="ai-ticket-composer-wrap composer-wrap">
        <div className="composer">
          <input ref={attachmentInput} className="composer-attachment-input" type="file" multiple
            accept=".txt,.md,.json,.csv,.ts,.tsx,.js,.jsx,.py,.yaml,.yml,.xml,.html,.css"
            onChange={(event) => void addAttachments(event.currentTarget.files)} />
          <textarea aria-label="工单消息" value={draft} rows={2} placeholder="继续处理这张工单…"
            onChange={(event) => setDraft(event.target.value)} onKeyDown={(event) => onEnter(event, () => void send())} />
          {attachments.length > 0 && <div className="composer-attachments" aria-label="待发送附件">
            {attachments.map((file, index) => <span key={`${file.name}-${index}`}>{file.name}
              <button type="button" aria-label={`移除附件 ${file.name}`}
                onClick={() => setAttachments((current) => current.filter((_, itemIndex) => itemIndex !== index))}>×</button>
            </span>)}
          </div>}
          <div className="composer-toolbar">
            <div className="composer-actions">
              <button type="button" className="composer-utility" title="添加文本附件" aria-label="添加附件"
                disabled={busy} onClick={() => attachmentInput.current?.click()}><AppIcon name="plus" /></button>
              <button type="button" className="composer-permission" title={fullAccessEnabled
                ? '完全权限已开启 · 后续任务无需逐次审批' : '逐次审批 · 点击开启完全权限'}
                aria-label={fullAccessEnabled ? '关闭完全权限，恢复逐次审批' : '开启完全权限，后续任务无需逐次审批'}
                aria-pressed={fullAccessEnabled} disabled={busy} onClick={toggleFullAccess}>
                <AppIcon name={fullAccessEnabled ? 'unlock' : 'lock'} /></button>
              <button type="button" className="composer-redaction-preview"
                title={`脱敏${redactionPreviewEnabled ? '已选中' : '未选中'} · 界面预览，尚未生效`}
                aria-label={`${redactionPreviewEnabled ? '关闭' : '启用'}脱敏（界面预览，尚未生效）`}
                aria-pressed={redactionPreviewEnabled}
                onClick={() => setRedactionPreviewEnabled((value) => !value)}>
                <AppIcon name={redactionPreviewEnabled ? 'shield-filled' : 'shield'} /></button>
            </div>
            <ComposerRuntimeControls currentModel={currentModel} choices={modelChoices}
              selectedThinkingLevel={modelSelection?.thinkingLevel ?? selected.modelSelection?.thinkingLevel}
              context={selected.context} modelOpen={modelOpen} setModelOpen={setModelOpen}
              contextOpen={contextOpen} setContextOpen={setContextOpen} onChooseModel={chooseModel}
              contextActionLabel="查看工单对话 →" onContextAction={() =>
                messagesEnd.current?.scrollIntoView({ behavior: 'smooth', block: 'end' })} />
            {(selected.status === 'running' || selected.status === 'queued')
              ? <button type="button" className="composer-stop" aria-label="停止任务" title="停止当前任务"
                disabled={busy} onClick={() => void cancel()}><AppIcon name="pause" /></button>
              : <button type="button" aria-label="发送工单消息" disabled={(!draft.trim() && !attachments.length) || busy}
                onClick={() => void send()}><AppIcon name="send" /></button>}
          </div>
        </div>
        {attachmentError && <p className="composer-attachment-error" role="alert">{attachmentError}</p>}
      </div>
    </div> : selectedId ? <p className="ai-ticket-muted ai-ticket-loading">正在加载工单…</p> : <div className="ai-ticket-overview">
      {creating && <div className="ai-ticket-create">
        <input aria-label="工单标题" value={title} placeholder="工单标题（可选）"
          onChange={(event) => setTitle(event.target.value)} />
        <textarea aria-label="工单内容" value={task} placeholder="描述这张工单要完成的工作"
          rows={4} onChange={(event) => setTask(event.target.value)} />
        <textarea aria-label="交付物" value={deliverables} placeholder="交付物，每行一项（可选）"
          rows={2} onChange={(event) => setDeliverables(event.target.value)} />
        <textarea aria-label="验收项" value={acceptanceCriteria} placeholder="验收项，每行一项（可选）"
          rows={2} onChange={(event) => setAcceptanceCriteria(event.target.value)} />
        <div><button type="button" onClick={() => setCreating(false)}>取消</button>
          <button type="button" disabled={!task.trim() || busy} onClick={() => void create()}>创建并开始</button></div>
      </div>}
      {!tickets.length && !creating && <p className="ai-ticket-muted">当前工作还没有任务工单。</p>}
      {tickets.map((ticket) => <button type="button" className={`ai-ticket-card ${ticket.status}`} key={ticket.id}
        onClick={() => openTicket(ticket.id)}>
        <TicketFace ticket={ticket} />
      </button>)}
    </div>}
  </div>;
}
