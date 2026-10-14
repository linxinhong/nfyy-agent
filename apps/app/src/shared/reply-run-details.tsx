import { useEffect, useState } from 'react';
import type { RunToolStep } from '@yuanpu-agent/protocol';
import { AppIcon } from './app-icon.js';
import type { ReplyRunInfo } from './reply-run-cache.js';

export function elapsedLabel(run: Pick<ReplyRunInfo, 'createdAt' | 'updatedAt'>): string | undefined {
  const elapsed = new Date(run.updatedAt).getTime() - new Date(run.createdAt).getTime();
  if (!Number.isFinite(elapsed) || elapsed < 0) return undefined;
  const seconds = Math.max(1, Math.round(elapsed / 1000));
  return seconds >= 60 ? `${Math.floor(seconds / 60)}分${seconds % 60}秒` : `${seconds}秒`;
}

const statusLabels: Record<ReplyRunInfo['status'], string> = {
  queued: '等待执行', running: '正在处理', waiting_approval: '等待授权', succeeded: '已完成',
  failed: '执行失败', cancelled: '已取消', interrupted: '执行中断', result_unknown: '结果未知',
};

const toolStatusLabels: Record<RunToolStep['status'], string> = {
  running: '进行中', completed: '已完成', failed: '失败', cancelled: '已取消', interrupted: '结果未知',
};

function toolPresentation(step: RunToolStep): { icon: 'file' | 'edit' | 'code' | 'globe' | 'assistant' | 'lightning'; label: string; target: string; result: string } {
  let args: Record<string, unknown> = {};
  try { args = JSON.parse(step.arguments ?? '{}'); } catch { /* Truncated arguments remain readable below. */ }
  const target = [args?.path, args?.command, args?.query, args?.pattern, args?.name]
    .find((value): value is string => typeof value === 'string') ?? '';
  const name = step.name;
  const reading = ['read', 'note_read'].includes(name);
  const labels: Record<string, string> = { read: '阅读', note_read: '阅读', write: '写入', edit: '编辑',
    bash: '执行命令', grep: '搜索内容', find: '查找文件', ls: '浏览目录', tool_search: '搜索工具',
    search_capabilities: '查找能力', execute_capability: '调用能力', delegate_and_verify: '委派与核验',
    assistant_work_candidates: '查看工作候选' };
  return { label: labels[name] ?? name, target: target.replace(/\s+/g, ' '),
    icon: reading || name === 'ls' ? 'file' : ['edit', 'write'].includes(name) ? 'edit'
      : name === 'bash' ? 'code' : /search|grep|find/.test(name) ? 'globe'
        : /delegat|subagent/.test(name) ? 'assistant' : 'lightning',
    result: reading && step.status === 'completed' && step.result !== undefined ? step.truncated ? '已读取内容（已截断）'
      : `已读取 ${step.result?.length ?? 0} 字符` : (step.result ?? '').replace(/\s+/g, ' ').slice(0, 160) };
}

export function ReplyRunDetails({ run, summary, onReview }: {
  run?: ReplyRunInfo; summary?: string; onReview?: () => void;
}) {
  const running = Boolean(run && ['queued', 'running', 'waiting_approval'].includes(run.status));
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!running) return;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [running, run?.runId]);
  const duration = run && elapsedLabel(running ? { ...run, updatedAt: new Date(now).toISOString() } : run);
  const header = <><span className={`reply-run-meta ${run?.status ?? 'cancelled'}`}>
    {summary ?? (run ? statusLabels[run.status] : '')}</span>{duration && <span className="reply-run-duration">{duration}</span>}</>;
  const steps: RunToolStep[] = run?.steps ?? (run?.tools ?? []).map((tool, index) => ({
    id: `legacy-${index}`, name: tool.name, status: tool.status, startedAt: run!.createdAt,
  }));
  if (!steps.length) return <div className="reply-run-plain" role="status">{header}</div>;
  return <details className="reply-run-details" aria-label="运行详情">
    <summary>{header}<AppIcon name="chevron" /></summary>
    <div className="reply-run-steps">
      {steps.map((step) => {
        const presentation = toolPresentation(step);
        return <details key={step.id} className="reply-tool-step">
          <summary aria-label={`${presentation.label} ${presentation.target || step.name}，${toolStatusLabels[step.status]}`}>
            <span className={`reply-tool-icon ${presentation.icon}`}><AppIcon name={presentation.icon} /></span>
            <span className="reply-tool-title" title={`${presentation.label} ${presentation.target}`}>
              {presentation.label}{presentation.target && <span> {presentation.target}</span>}</span>
            <span className="reply-tool-result">{presentation.result}</span>
            <span className={`reply-tool-status ${step.status}`}>{toolStatusLabels[step.status]}</span>
          </summary>
          <div className="reply-tool-content">
            <div><strong>参数</strong><pre>{step.arguments || '未记录参数'}</pre></div>
            <div><strong>结果</strong><pre>{step.result || (step.status === 'running' ? '等待工具返回…' : '未记录结果')}</pre></div>
            {step.truncated && <p>内容较长，已截断展示。</p>}
          </div>
        </details>;
      })}
      {onReview && steps.some((step) => /(?:^|[._:/])(?:write|edit|apply_patch|patch)(?:$|[._:/])/i.test(step.name))
        && <button type="button" className="reply-run-review" onClick={onReview}><AppIcon name="review" />审查修改</button>}
    </div>
  </details>;
}
