import { displayToolValue, toolResultText, type RunToolStep,
  type DesktopReplyRunInfo, type DesktopTranscriptMessage } from '@yuanpu-agent/protocol';
import { browserScreenshotPath, withBrowserScreenshots } from './browser-screenshot-attachment.js';

type TranscriptEntry = {
  id: string;
  timestamp: string;
  type: string;
  customType?: string;
  data?: unknown;
  message?: {
    role: string;
    content?: string | Array<{ type: string; text?: string; id?: string; name?: string; arguments?: unknown }>;
    stopReason?: string;
    toolName?: string;
    toolCallId?: string;
    isError?: boolean;
    details?: unknown;
  };
};

/** Derive a public summary from the selected Pi branch, without leaking tool payloads. */
export function summarizeTranscript(
  entries: readonly TranscriptEntry[],
  options: { limit?: number; completedOnly?: boolean; includeToolDetails?: boolean } = {},
): DesktopTranscriptMessage[] {
  if (options.includeToolDetails) return summarizeRunTranscript(entries).slice(-(options.limit ?? 100));
  const messages: DesktopTranscriptMessage[] = [];
  let startedAt: string | undefined;
  let events: DesktopReplyRunInfo['events'] = [];
  let tools: DesktopReplyRunInfo['tools'] = [];
  let browserScreenshots: string[] = [];
  for (const entry of entries) {
    const message = entry.message;
    if (entry.type !== 'message' || !message) continue;
    if (message.role === 'user') {
      startedAt = entry.timestamp;
      events = [{ id: 0, title: '收到消息', at: entry.timestamp }];
      tools = [];
      browserScreenshots = [];
    }
    if (message.role === 'toolResult' && startedAt && message.toolName) {
      if (message.toolName === 'execute_capability') {
        const path = browserScreenshotPath(message.details);
        if (path) browserScreenshots.push(path);
      }
      tools.push({ name: message.toolName, status: message.isError ? 'failed' : 'completed' });
      events.push({ id: events.length, title: message.isError ? '工具调用失败' : '工具调用完成', detail: message.toolName, at: entry.timestamp });
    }
    if (message.role !== 'user' && message.role !== 'assistant') continue;
    if (options.completedOnly && message.role === 'assistant' && message.stopReason !== 'stop') continue;
    const text = typeof message.content === 'string' ? message.content
      : (message.content ?? []).filter((block) => block.type === 'text').map((block) => block.text ?? '').join('\n');
    if (!text.trim()) continue;
    const status = message.stopReason === 'error' ? 'failed' : message.stopReason === 'aborted' ? 'cancelled'
      : message.stopReason === 'toolUse' ? 'running' : ['stop', 'length'].includes(message.stopReason ?? '') ? 'succeeded' : undefined;
    const run: DesktopReplyRunInfo | undefined = message.role === 'assistant' && startedAt && status ? {
      source: 'transcript', status, createdAt: startedAt, updatedAt: entry.timestamp,
      events: [...events, { id: events.length, title: status === 'succeeded' ? '已生成回复' : status === 'running' ? '继续处理' : '处理结束', at: entry.timestamp }],
      tools: [...tools],
    } : undefined;
    messages.push({ id: entry.id, role: message.role,
      text: message.role === 'assistant' && status === 'succeeded'
        ? withBrowserScreenshots(text, browserScreenshots) : text,
      at: entry.timestamp, ...(run ? { run } : {}) });
  }
  return messages.slice(-(options.limit ?? 100));
}

/** Desktop-only grouping. Other consumers keep the payload-free transcript projection above. */
function summarizeRunTranscript(entries: readonly TranscriptEntry[]): DesktopTranscriptMessage[] {
  const messages: DesktopTranscriptMessage[] = [];
  let turn: { startedAt: string; at: string; id: string; texts: string[]; steps: Map<string, RunToolStep>;
    screenshots: string[]; runId?: string; status?: DesktopReplyRunInfo['status'] } | undefined;
  const finish = () => {
    if (!turn || (!turn.texts.length && !turn.steps.size)) return;
    const status = turn.status ?? (turn.steps.size ? 'interrupted' : 'result_unknown');
    const steps = [...turn.steps.values()].map((step) => step.status === 'running'
      ? { ...step, status: status === 'cancelled' ? 'cancelled' as const : 'interrupted' as const } : step);
    messages.push({ id: turn.id, role: 'assistant', at: turn.at,
      text: withBrowserScreenshots(turn.texts.join('\n\n'), turn.screenshots),
      run: { source: 'transcript', runId: turn.runId, status, createdAt: turn.startedAt, updatedAt: turn.at,
        events: [], steps, tools: steps.filter((step) => step.status === 'completed' || step.status === 'failed')
          .map((step) => ({ name: step.name, status: step.status as 'completed' | 'failed' })) } });
  };
  for (const entry of entries) {
    if (turn && entry.type === 'custom' && entry.customType === 'yuanpu_model_timing'
      && entry.data && typeof entry.data === 'object' && 'runId' in entry.data
      && typeof entry.data.runId === 'string') turn.runId = entry.data.runId;
    const message = entry.message;
    if (entry.type !== 'message' || !message) continue;
    if (message.role === 'user') {
      finish();
      turn = { startedAt: entry.timestamp, at: entry.timestamp, id: entry.id, texts: [], steps: new Map(), screenshots: [] };
      const text = toolResultText(message.content);
      if (text.trim()) messages.push({ id: entry.id, role: 'user', text, at: entry.timestamp });
      continue;
    }
    if (message.role !== 'assistant' && message.role !== 'toolResult') continue;
    turn ??= { startedAt: entry.timestamp, at: entry.timestamp, id: entry.id, texts: [], steps: new Map(), screenshots: [] };
    turn.at = entry.timestamp;
    turn.id = entry.id;
    if (message.role === 'assistant') {
      const text = toolResultText(message.content);
      if (text.trim()) turn.texts.push(text);
      turn.status = message.stopReason === 'error' ? 'failed' : message.stopReason === 'aborted' ? 'cancelled'
        : ['stop', 'length'].includes(message.stopReason ?? '') ? 'succeeded' : undefined;
      for (const block of Array.isArray(message.content) ? message.content : []) {
        if (block.type !== 'toolCall' || !block.id || !block.name || turn.steps.has(block.id) || turn.steps.size >= 200) continue;
        const value = displayToolValue(block.arguments);
        turn.steps.set(block.id, { id: block.id, name: block.name, status: 'running',
          startedAt: entry.timestamp, arguments: value.text, truncated: value.truncated });
      }
    } else {
      const step = message.toolCallId ? turn.steps.get(message.toolCallId) : undefined;
      if (step && step.name === message.toolName) {
        const value = displayToolValue(toolResultText(message.content));
        const failed = message.isError || Boolean(message.details && typeof message.details === 'object'
          && 'capabilityError' in message.details);
        Object.assign(step, { status: failed ? 'failed' : 'completed', finishedAt: entry.timestamp,
          result: value.text, truncated: step.truncated || value.truncated });
      }
      if (message.toolName === 'execute_capability') {
        const path = browserScreenshotPath(message.details);
        if (path) turn.screenshots.push(path);
      }
    }
  }
  finish();
  return messages;
}
