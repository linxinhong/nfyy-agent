import type { AgentRunRecord, DesktopReplyRunInfo } from '@yuanpu-agent/protocol';

export type ReplyRunInfo = DesktopReplyRunInfo;

type CachedReplyRun = ReplyRunInfo & {
  runId: string;
  surface: 'work' | 'assistant';
  text: string;
  transcriptId?: string;
};

const storageKey = 'yuanpu:reply-runs:v1';

function readCache(): CachedReplyRun[] {
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(storageKey) ?? '[]');
    return Array.isArray(value) ? value.filter((item): item is CachedReplyRun => Boolean(item)
      && typeof item.runId === 'string' && typeof item.text === 'string'
      && (item.surface === 'work' || item.surface === 'assistant')
      && typeof item.createdAt === 'string' && typeof item.updatedAt === 'string'
      && Array.isArray(item.events) && Array.isArray(item.tools)) : [];
  } catch { return []; }
}

function writeCache(entries: CachedReplyRun[]): void {
  // Restore detailed evidence from the host, without retaining tool payloads in browser storage.
  try { window.localStorage.setItem(storageKey, JSON.stringify(entries.slice(0, 100).map(({ steps: _steps, ...entry }) => entry))); }
  catch { /* Run details remain available for this session if storage is unavailable. */ }
}

export function cacheReplyRun(
  surface: CachedReplyRun['surface'],
  text: string,
  run: AgentRunRecord,
  events: ReplyRunInfo['events'],
): ReplyRunInfo {
  const info: ReplyRunInfo & { runId: string } = {
    runId: run.runId,
    status: run.status,
    createdAt: run.createdAt,
    updatedAt: run.updatedAt,
    events,
    tools: run.output?.tools ?? [],
    steps: run.output?.steps ?? run.progress?.steps,
  };
  writeCache([{ ...info, surface, text }, ...readCache().filter((item) => item.runId !== run.runId)]);
  return info;
}

export function findReplyRun(
  surface: CachedReplyRun['surface'],
  transcriptId: string,
  text: string,
  at: string,
): ReplyRunInfo | undefined {
  const entries = readCache();
  const linked = entries.find((item) => item.surface === surface && item.transcriptId === transcriptId);
  if (linked) return linked;
  const atMs = new Date(at).getTime();
  if (!Number.isFinite(atMs)) return undefined;
  const candidates = entries.filter((item) => item.surface === surface && !item.transcriptId
    && item.text.trim() === text.trim()
    && Math.abs(new Date(item.updatedAt).getTime() - atMs) <= 120_000);
  candidates.sort((left, right) => Math.abs(new Date(left.updatedAt).getTime() - atMs) - Math.abs(new Date(right.updatedAt).getTime() - atMs));
  const match = candidates[0];
  if (!match) return undefined;
  match.transcriptId = transcriptId;
  writeCache(entries);
  return match;
}
