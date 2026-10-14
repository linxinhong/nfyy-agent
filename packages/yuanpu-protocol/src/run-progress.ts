/** Bounded, display-only tool evidence. Never contains model reasoning or binary blocks. */
export interface RunToolStep {
  id: string;
  name: string;
  status: 'running' | 'completed' | 'failed' | 'cancelled' | 'interrupted';
  startedAt: string;
  finishedAt?: string;
  arguments?: string;
  result?: string;
  truncated?: boolean;
}

export interface RunProgress {
  text: string;
  steps: RunToolStep[];
  truncated?: boolean;
}

const secretKey = /(?:^|[_-])(?:authorization|api[-_]?key|access[-_]?token|refresh[-_]?token|token|password|passwd|secret|client[-_]?secret|cookie)$/i;
export function displayToolValue(value: unknown): { text: string; truncated: boolean } {
  let truncated = false;
  const clean = (item: unknown, depth: number): unknown => {
    if (typeof item === 'string') {
      if (item.length > 8_000) truncated = true;
      return item.slice(0, 8_000).replace(/\bBearer\s+[^\s"',;]+/gi, 'Bearer [redacted]')
        .replace(/((?:authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|password|passwd|secret|cookie)["']?\s*[=:]\s*)(?:"[^"\n]*"|'[^'\n]*'|[^\s,;}]+)/gi, '$1"[redacted]"');
    }
    if (!item || typeof item !== 'object') return item;
    if (depth > 6) { truncated = true; return '[omitted]'; }
    if (Array.isArray(item)) {
      if (item.length > 100) truncated = true;
      return item.slice(0, 100).map((child) => clean(child, depth + 1));
    }
    const entries = Object.entries(item);
    if (entries.length > 100) truncated = true;
    return Object.fromEntries(entries.slice(0, 100).map(([key, child]) =>
      [key, secretKey.test(key) ? '[redacted]' : clean(child, depth + 1)]));
  };
  const cleaned = clean(value, 0);
  const text = typeof cleaned === 'string' ? cleaned : JSON.stringify(cleaned, null, 2) ?? '';
  return { text: text.slice(0, 8_000), truncated: truncated || text.length > 8_000 };
}

export function toolResultText(content: unknown): string {
  return Array.isArray(content) ? content.filter((block) => block?.type === 'text' && typeof block.text === 'string')
    .map((block) => block.text).join('\n') : typeof content === 'string' ? content : '';
}

export function settleRunProgress(progress: RunProgress,
  status: 'cancelled' | 'interrupted' = 'interrupted'): RunProgress {
  return { ...progress, steps: progress.steps.map((step) => step.status === 'running'
    ? { ...step, status } : step) };
}
