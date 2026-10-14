import type { AgentRunRecord, DesktopTranscriptMessage } from '@yuanpu-agent/protocol';

/** Pi may record an aborted generation as an error; the owning Runtime knows the actual outcome. */
export function restoreWorkRunOutcomes(messages: DesktopTranscriptMessage[], conversationId: string,
  lookup: (runId: string) => AgentRunRecord | undefined): DesktopTranscriptMessage[] {
  return messages.map((message) => {
    const run = message.run?.runId ? lookup(message.run.runId) : undefined;
    if (!message.run || !run || run.owner.entryPoint !== 'desktop'
      || run.context.conversation.conversationId !== conversationId
      || !['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown'].includes(run.status)) return message;
    const fallback = run.status === 'cancelled' ? '任务已取消。' : run.status === 'failed' ? '任务执行失败。'
      : run.status === 'interrupted' ? '任务已中断。' : '';
    return { ...message, text: message.text || fallback,
      run: { ...message.run, source: 'runtime', status: run.status,
        createdAt: run.createdAt, updatedAt: run.updatedAt } };
  });
}
