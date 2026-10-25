import type { SessionEntry } from '@earendil-works/pi-coding-agent';
import type { SessionTrajectory, SessionTrajectoryRow } from '@yuanpu-agent/protocol';
import { parseCapabilityId } from '../capabilities/id.js';

const MAX_TEXT = 240;

function short(value: string): string {
  return value.replace(/\s+/g, ' ').trim().slice(0, MAX_TEXT);
}

function visibleText(content: string | Array<{ type: string; text?: string }>): string {
  return short(typeof content === 'string' ? content
    : content.filter((part) => part.type === 'text').map((part) => part.text ?? '').join(' '));
}

function capabilityAction(name: string): string {
  const known: Record<string, string> = {
    browser_evaluate: '在网页中执行操作',
    browser_snapshot: '查看网页状态',
    browser_navigate: '打开网页',
    web_search: '搜索网页',
    fetch_content: '读取网页内容',
  };
  return known[name] ?? name.replaceAll('_', ' ').slice(0, 80);
}

/** Keep a small, predictable set of tool labels. Arbitrary arguments may contain credentials. */
export function toolSummary(name: string, args: Record<string, unknown>): string {
  if (name === 'execute_capability') {
    const capability = typeof args.name === 'string' ? parseCapabilityId(args.name) : undefined;
    if (!capability) return '调用外部能力';
    const browser = capability.originalName.startsWith('browser_');
    return `${browser ? '浏览器' : '外部能力'} · ${capabilityAction(capability.originalName)}`;
  }
  const field = ['path', 'file_path', 'query', 'command', 'url', 'name']
    .find((key) => typeof args[key] === 'string' && !/key|token|secret|password/i.test(key));
  if (!field) return name;
  const value = String(args[field]);
  // These arguments are untrusted. The trajectory is a compact label, not an argument inspector.
  if (/\b(?:sk-[A-Za-z0-9_-]{12,}|bearer|authorization|api[_-]?key|access[_-]?token|password|secret|credential|private[_-]?key)\b/i.test(value)) return name;
  if (field === 'command' && (value.includes('\n') || /[;&|`$]/.test(value))) return name;
  return `${name} · ${short(value)}`;
}

export function toolCategory(name: string, args: Record<string, unknown>): string {
  const capability = name === 'execute_capability' && typeof args.name === 'string'
    ? parseCapabilityId(args.name) : undefined;
  if (capability?.originalName.startsWith('browser_')) return '浏览器';
  if (capability && /web_search|fetch_content/i.test(capability.originalName)) return 'Web 搜索';
  if (name === 'ai_ticket') return '任务工单';
  if (name === 'subagent' && args.action === 'run') return '子 Agent';
  if (name === 'skill' || name === 'use_skill') return 'Skill';
  if ((name === 'read' || name === 'write' || name === 'edit')
    && typeof args.path === 'string' && /(?:^|\/)SKILL\.md$/i.test(args.path)) return 'Skill';
  if (/web_search|fetch_content|browser/i.test(name) || /web_search|fetch_content/i.test(String(args.name ?? ''))) return 'Web 搜索';
  if (/execute_capability|mcp/i.test(name)) return 'MCP';
  if (/run_code|python|code_execution/i.test(name)) return '代码执行';
  if (/bash|shell|terminal/i.test(name)) return 'Bash';
  if (/read|write|edit|grep|find|ls/.test(name)) return '文件';
  return '工具调用';
}

export function projectSessionTrajectory(conversationId: string, entries: readonly SessionEntry[]): SessionTrajectory {
  const rows: SessionTrajectoryRow[] = [];
  const calls = new Map<string, SessionTrajectoryRow>();
  let round = 0;
  let callCount = 0;
  for (const entry of entries) {
    if (entry.type === 'message') {
      const message = entry.message;
      if (message.role === 'user') {
        round += 1;
        rows.push({ id: entry.id, round, kind: 'user', title: '用户',
          text: visibleText(message.content), at: entry.timestamp });
      } else if (message.role === 'assistant') {
        const text = visibleText(message.content.filter((part) => part.type === 'text'));
        if (text) rows.push({ id: entry.id, round, kind: 'assistant', title: '助手', text,
          at: entry.timestamp, ...(message.stopReason === 'error' ? { status: 'failed' as const } : {}) });
        for (const block of message.content) {
          if (block.type !== 'toolCall') continue;
          callCount += 1;
          const category = toolCategory(block.name, block.arguments);
          const row: SessionTrajectoryRow = { id: `${entry.id}:${block.id}`, round,
            kind: block.name === 'ai_ticket' ? 'subagent' : 'tool',
            title: category,
            text: toolSummary(block.name, block.arguments), at: entry.timestamp,
            status: 'running', toolCallId: block.id };
          rows.push(row);
          calls.set(block.id, row);
        }
      } else if (message.role === 'toolResult') {
        const call = calls.get(message.toolCallId);
        if (call) {
          call.status = message.isError ? 'failed' : 'completed';
          const outputSize = message.content.filter((part) => part.type === 'text')
            .reduce((sum, part) => sum + part.text.length, 0);
          // Tool output may contain secrets. The row records its result and size, not its raw body.
          rows.push({ id: entry.id, round: call.round, kind: 'tool', title: '结果',
            text: `${message.toolName} → ${message.isError ? '调用失败' : '已完成'}${outputSize ? ` · ${outputSize} 字符` : ''}`,
            at: entry.timestamp, status: message.isError ? 'failed' : 'completed', toolCallId: message.toolCallId });
        }
      }
      continue;
    }
    if (entry.type === 'model_change') rows.push({ id: entry.id, round, kind: 'model',
      title: '模型', text: `${entry.provider} / ${entry.modelId}`, at: entry.timestamp });
    if (entry.type === 'thinking_level_change') rows.push({ id: entry.id, round, kind: 'model',
      title: '思考强度', text: entry.thinkingLevel, at: entry.timestamp });
    if (entry.type === 'compaction') rows.push({ id: entry.id, round, kind: 'context',
      title: '上下文', text: '已压缩上下文', at: entry.timestamp });
  }
  return { conversationId, rows, rounds: round, calls: callCount, children: [] };
}
