import { performance } from 'node:perf_hooks';
import { randomUUID } from 'node:crypto';
import type { InlineExtension } from '@earendil-works/pi-coding-agent';

/** Persist phase timings in the session, without request bodies, credentials or raw provider events. */
export function modelRequestTiming(getRunId: () => string | undefined): InlineExtension {
  return { name: 'yuanpu-model-timing', hidden: true, factory(pi) {
    let request: { id: string; started: number; firstEvent: boolean; firstText: boolean } | undefined;
    const record = (phase: string, details: Record<string, unknown> = {}) => {
      if (!request) return;
      pi.appendEntry('yuanpu_model_timing', { requestId: request.id, runId: getRunId(), phase,
        elapsedMs: Math.round(performance.now() - request.started), ...details });
    };
    pi.on('before_provider_request', (_event, context) => {
      request = { id: randomUUID(), started: performance.now(), firstEvent: false, firstText: false };
      record('request', { provider: context.model?.provider, model: context.model?.id });
    });
    pi.on('after_provider_response', (event) => { record('headers', { status: event.status }); });
    pi.on('provider_stream_event', () => {
      if (request && !request.firstEvent) {
        request.firstEvent = true;
        record('first_event');
      }
    });
    pi.on('message_update', (event) => {
      if (request && !request.firstText && event.assistantMessageEvent.type === 'text_delta') {
        request.firstText = true;
        record('first_text');
      }
    });
    pi.on('message_end', (event) => {
      if (event.message.role !== 'assistant') return;
      record('end', { stopReason: event.message.stopReason });
      request = undefined;
    });
    pi.on('agent_end', () => { record('end', { stopReason: 'interrupted' }); request = undefined; });
  } };
}
