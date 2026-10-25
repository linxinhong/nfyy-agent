import { useEffect, useRef, type Dispatch, type SetStateAction } from 'react';
import type { AgentRunRequest, SessionTrajectory } from '@yuanpu-agent/protocol';

const thinkingLevels = [
  { value: 'low', label: '低' },
  { value: 'medium', label: '中' },
  { value: 'high', label: '高' },
] as const;

export function threeStepThinking(level?: string): 'low' | 'medium' | 'high' {
  if (level === 'minimal' || level === 'low') return 'low';
  if (level === 'high' || level === 'xhigh' || level === 'max') return 'high';
  return 'medium';
}

function tokenLabel(value: number | undefined): string {
  if (value === undefined) return '—';
  return value >= 1_000 ? `${(value / 1_000).toFixed(value >= 10_000 ? 0 : 1)}K` : String(value);
}

export function ComposerRuntimeControls({ currentModel, choices, selectedThinkingLevel, context,
  modelOpen, setModelOpen, contextOpen, setContextOpen, onChooseModel, onContextAction, contextActionLabel }: {
  currentModel?: NonNullable<AgentRunRequest['modelSelection']>;
  choices: Array<{ provider: string; model: string; name: string }>;
  selectedThinkingLevel?: NonNullable<AgentRunRequest['modelSelection']>['thinkingLevel'];
  context?: SessionTrajectory['context'];
  modelOpen: boolean;
  setModelOpen: Dispatch<SetStateAction<boolean>>;
  contextOpen: boolean;
  setContextOpen: Dispatch<SetStateAction<boolean>>;
  onChooseModel(next: NonNullable<AgentRunRequest['modelSelection']>): void;
  onContextAction(): void;
  contextActionLabel: string;
}) {
  const controlsRef = useRef<HTMLDivElement>(null);
  const thinkingLevelIndex = thinkingLevels.findIndex((level) => level.value === threeStepThinking(selectedThinkingLevel));
  const contextRingPercent = typeof context?.percent === 'number' && Number.isFinite(context.percent)
    ? Math.min(100, Math.max(0, context.percent)) : undefined;

  useEffect(() => {
    if (!modelOpen && !contextOpen) return;
    const dismiss = (event: PointerEvent) => {
      if (!controlsRef.current?.contains(event.target as Node)) {
        setModelOpen(false);
        setContextOpen(false);
      }
    };
    const escape = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape') { setModelOpen(false); setContextOpen(false); }
    };
    document.addEventListener('pointerdown', dismiss);
    document.addEventListener('keydown', escape);
    return () => { document.removeEventListener('pointerdown', dismiss); document.removeEventListener('keydown', escape); };
  }, [modelOpen, contextOpen, setModelOpen, setContextOpen]);

  return <div ref={controlsRef} className="composer-runtime-controls">
    <button type="button" className="composer-model-button" aria-label="选择模型" aria-expanded={modelOpen}
      onClick={() => { setContextOpen(false); setModelOpen((value) => !value); }}>
      <span className="composer-model-label">{currentModel?.model ?? '选择模型'}</span>
      <svg aria-hidden="true" viewBox="0 0 16 16" fill="none"><path d="m4 6 4 4 4-4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" /></svg>
    </button>
    {modelOpen && <div className="composer-model-popover" role="dialog" aria-label="选择模型">
      <strong>模型</strong>
      {choices.length ? <div className="composer-model-list">{choices.map((item) =>
        <button type="button" key={`${item.provider}/${item.model}`}
          className={currentModel?.provider === item.provider && currentModel.model === item.model ? 'selected' : ''}
          onClick={() => {
            onChooseModel({ provider: item.provider, model: item.model,
              thinkingLevel: threeStepThinking(selectedThinkingLevel) });
            setModelOpen(false);
          }}>
          <span>{item.name}</span><small>{item.provider}</small>
        </button>)}</div> : <p>请先在模型设置中配置模型。</p>}
      {currentModel && <div className="composer-thinking">
        <div className="composer-thinking-heading"><span>思考强度</span><output>{thinkingLevels[thinkingLevelIndex]?.label}</output></div>
        <div className="composer-thinking-slider">
          <div className="composer-thinking-track" aria-hidden="true">
            <i style={{ width: `${thinkingLevelIndex / (thinkingLevels.length - 1) * 100}%` }} />
            {thinkingLevels.map((level, index) => <span key={level.value} data-active={index <= thinkingLevelIndex} />)}
          </div>
          <input type="range" min={0} max={thinkingLevels.length - 1} step={1} value={thinkingLevelIndex}
            aria-label="思考强度" aria-valuetext={thinkingLevels[thinkingLevelIndex]?.label}
            onChange={(event) => {
              const level = thinkingLevels[Number(event.target.value)];
              if (level) onChooseModel({ ...currentModel, thinkingLevel: level.value });
            }} />
        </div>
        <div className="composer-thinking-ends"><span>低</span><span>高</span></div>
      </div>}
    </div>}
    <button type="button" className="composer-context-ring"
      data-tooltip={contextRingPercent === undefined ? '上下文用量暂不可用' : `上下文已用 ${Math.round(contextRingPercent)}%`}
      aria-label={contextRingPercent === undefined ? '查看上下文用量，当前用量暂不可用' : `查看上下文用量，已用 ${Math.round(contextRingPercent)}%`}
      aria-expanded={contextOpen}
      onClick={() => { setModelOpen(false); setContextOpen((value) => !value); }}>
      <svg aria-hidden="true" viewBox="0 0 36 36" fill="none">
        <circle className="composer-context-track" cx="18" cy="18" r="13" pathLength="100" />
        {contextRingPercent !== undefined && contextRingPercent > 0 &&
          <circle className="composer-context-value" cx="18" cy="18" r="13" pathLength="100"
            strokeDasharray={`${contextRingPercent} ${100 - contextRingPercent}`} />}
      </svg>
    </button>
    {contextOpen && <div className="composer-context-popover" role="dialog" aria-label="上下文用量">
      <strong>上下文已用 {context?.percent == null ? '—' : `${Math.round(context.percent)}%`}</strong>
      <span>{context?.tokens == null ? '—' : context.tokens.toLocaleString('en-US')}
        {' / '}{context?.contextWindow?.toLocaleString('en-US') ?? '—'}</span>
      <div className="composer-context-progress"><i style={{ width: `${Math.min(100, Math.max(0, context?.percent ?? 0))}%` }} /></div>
      <dl><div><dt>系统提示词</dt><dd>{tokenLabel(context?.breakdown?.systemPrompt)}</dd></div>
        <div><dt>工具定义</dt><dd>{tokenLabel(context?.breakdown?.tools)}</dd></div>
        <div><dt>对话消息</dt><dd>{tokenLabel(context?.breakdown?.messages)}</dd></div>
        <div><dt>其他</dt><dd>{tokenLabel(context?.breakdown?.other)}</dd></div></dl>
      <button type="button" className="composer-open-trajectory" onClick={() => {
        setContextOpen(false); onContextAction();
      }}>{contextActionLabel}</button>
    </div>}
  </div>;
}
