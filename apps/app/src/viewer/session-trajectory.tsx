import { useEffect, useMemo, useState } from 'react';
import type { SessionTrajectory, SessionTrajectoryRow } from '@yuanpu-agent/protocol';

const kindLabels: Record<SessionTrajectoryRow['kind'], string> = {
  user: '用户', assistant: '助手', tool: '工具', model: '模型', context: '上下文', subagent: '任务工单',
};
const lanes = ['user', 'model', 'tool', 'context'] as const;

function elapsed(first: string | undefined, last: string | undefined): string {
  const difference = Date.parse(last ?? '') - Date.parse(first ?? '');
  if (!Number.isFinite(difference) || difference < 0) return '—';
  return difference >= 60_000 ? `${Math.floor(difference / 60_000)}m ${Math.round((difference % 60_000) / 1000)}s`
    : `${(difference / 1000).toFixed(1)}s`;
}

type TimelineMode = 'time' | 'round' | 'calls';

function SessionTimeline({ trajectory, mode, onRound }: {
  trajectory: SessionTrajectory; mode: TimelineMode; onRound(round: number): void;
}) {
  const first = Date.parse(trajectory.rows[0]?.at ?? '');
  const last = Date.parse(trajectory.rows.at(-1)?.at ?? '');
  const span = Math.max(1, last - first);
  const toolRows = trajectory.rows.filter((row) => row.kind === 'tool' || row.kind === 'subagent');
  const toolPosition = new Map(toolRows.map((row, index) => [row.id, index]));
  const position = (row: SessionTrajectoryRow) => {
    if (mode === 'round') return (row.round - 1) / Math.max(1, trajectory.rounds - 1);
    if (mode === 'calls') {
      const index = toolPosition.get(row.id) ?? -1;
      return index < 0 ? (row.round - 1) / Math.max(1, trajectory.rounds - 1)
        : index / Math.max(1, toolRows.length - 1);
    }
    return (Date.parse(row.at) - first) / span;
  };
  return <div className="session-timeline" aria-label="会话时间轴">
    {lanes.map((lane) => <div className={`session-timeline-lane ${lane}`} key={lane}>
      <span>{kindLabels[lane]}</span><div>
        {trajectory.rows.filter((row) => row.kind === lane || lane === 'tool' && row.kind === 'subagent')
          .map((row) => <button key={row.id} type="button" title={`${kindLabels[row.kind]} · ${row.text}`}
            style={{ left: `${Math.max(0, Math.min(96, position(row) * 100))}%` }}
            onClick={() => onRound(row.round)} />)}
      </div>
    </div>)}
  </div>;
}
function Row({ row }: { row: SessionTrajectoryRow }) {
  return <li className={`session-row ${row.kind} ${row.status ?? ''}`} id={`trajectory-${row.id}`}>
    <span className="session-row-kind">{row.title}</span>
    <span className="session-row-text" title={row.text}>{row.text}</span>
    <time dateTime={row.at}>{new Date(row.at).toLocaleTimeString('zh-CN', { hour12: false })}</time>
  </li>;
}

export function SessionTrajectoryViewer({ trajectory, title, onRefresh }: {
  trajectory?: SessionTrajectory;
  title: string;
  onRefresh?(): void;
}) {
  const [mode, setMode] = useState<TimelineMode>('time');
  const [query, setQuery] = useState('');
  const [visibleCount, setVisibleCount] = useState(300);
  const [jumpRound, setJumpRound] = useState<number>();
  const rows = trajectory?.rows ?? [];
  const filtered = useMemo(() => rows.filter((row) => !query
    || `${row.title} ${row.text}`.toLowerCase().includes(query.toLowerCase())), [rows, query]);
  const groups = useMemo(() => {
    const grouped = new Map<number, SessionTrajectoryRow[]>();
    for (const row of (query ? filtered : filtered.slice(-visibleCount)).slice().reverse()) {
      const group = grouped.get(row.round) ?? [];
      group.push(row);
      grouped.set(row.round, group);
    }
    return [...grouped].map(([round, groupRows]) => ({ round, rows: groupRows }));
  }, [filtered, query, visibleCount]);
  const scrollToRound = (round: number) => {
    setQuery('');
    const index = rows.findIndex((row) => row.round === round);
    if (index >= 0) setVisibleCount((count) => Math.max(count, rows.length - index));
    setJumpRound(round);
  };
  useEffect(() => {
    if (jumpRound === undefined) return;
    document.getElementById(`trajectory-round-${jumpRound}`)?.scrollIntoView({ block: 'start', behavior: 'smooth' });
    setJumpRound(undefined);
  }, [groups, jumpRound]);
  return <div className="session-trajectory-viewer">
    <header className="session-trajectory-header">
      <strong>当前会话 · {title}</strong>
      {onRefresh && <button type="button" onClick={onRefresh} aria-label="刷新运行轨迹">↻</button>}
    </header>
    <div className="session-trajectory-controls">
      <div className="session-trajectory-modes" role="tablist" aria-label="轨迹概览方式">
        {([['time', '时长', elapsed(rows[0]?.at, rows.at(-1)?.at)],
          ['round', '轮次', String(trajectory?.rounds ?? 0)],
          ['calls', '调用', String(trajectory?.calls ?? 0)]] as const).map(([key, label, value]) =>
          <button type="button" role="tab" key={key} aria-selected={mode === key}
            className={mode === key ? 'active' : ''} onClick={() => setMode(key)}>{label} <b>{value}</b></button>)}
      </div>
      <input type="search" aria-label="搜索运行轨迹" placeholder="搜索" value={query}
        onChange={(event) => setQuery(event.target.value)} />
    </div>
    {trajectory && <SessionTimeline trajectory={trajectory} mode={mode} onRound={scrollToRound} />}
    <div className="session-trajectory-rows">
      {!trajectory && <p className="session-trajectory-empty">正在读取当前会话…</p>}
      {trajectory && !rows.length && (!trajectory.live || trajectory.live.finishedAt)
        && <p className="session-trajectory-empty">当前会话还没有运行记录。</p>}
      {trajectory && rows.length > 0 && !groups.length && <p className="session-trajectory-empty">没有匹配的记录。</p>}
      {trajectory?.live && !trajectory.live.finishedAt && <section className="session-live-round">
        <h3>正在运行</h3>
        <ol><li className="session-row assistant">
          <span className="session-row-kind">{trajectory.live.text ? '助手' : '思考'}</span>
          <span className="session-row-text" title={trajectory.live.text.slice(-240)}>
            {trajectory.live.text ? trajectory.live.text.slice(-240) : '正在处理本轮输入'}</span>
          <time>实时</time>
        </li>{trajectory.live.tools.slice().reverse().map((tool) => <li className={`session-row tool ${tool.status}`} key={tool.id}>
          <span className="session-row-kind">{tool.category}</span><span className="session-row-text">{tool.summary}</span>
          <time>{tool.status === 'running' ? '运行中' : tool.status === 'failed' ? '失败' : '完成'}</time>
        </li>)}</ol>
      </section>}
      {groups.map(({ round, rows: groupRows }) => <section key={round} id={`trajectory-round-${round}`}>
        <h3>{round ? `第 ${round} 轮` : '会话设置'} <span>· {groupRows[0]?.at && new Date(groupRows[0].at).toLocaleTimeString('zh-CN', { hour12: false })}</span></h3>
        <ol>{groupRows.map((row) => <Row key={row.id} row={row} />)}</ol>
      </section>)}
      {!query && rows.length > visibleCount && <button className="session-show-earlier" type="button"
        onClick={() => setVisibleCount((count) => count + 300)}>显示更早记录 · {rows.length - visibleCount} 条</button>}
    </div>
  </div>;
}
