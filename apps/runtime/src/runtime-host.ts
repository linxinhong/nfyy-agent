import type {
  AgentService,
  AuthenticatedAgentCaller,
  ChannelInboundRoute,
  ChannelOutboundRecord,
} from '@yuanpu-agent/runtime-kit';
import type { AgentRunRecord, PrivateImRunSummary } from '@yuanpu-agent/protocol';

import type { PersistedWecomDocument } from './wecom-channel.js';

export async function getDesktopNavigableRun(
  agent: Pick<AgentService, 'get'>,
  runId: string,
  desktopCaller: AuthenticatedAgentCaller,
  schedulerCaller: AuthenticatedAgentCaller,
): Promise<AgentRunRecord | undefined> {
  return await agent.get(desktopCaller, runId)
    ?? await agent.get(schedulerCaller, runId);
}

export function getDesktopPrivateImRunSummary(
  runId: string,
  stores: {
    agentRuns: Pick<{ get(runId: string): AgentRunRecord | undefined }, 'get'>;
    channels: {
      getInboundForRun(runId: string): ChannelInboundRoute | undefined;
      getOutboundForRun(runId: string): ChannelOutboundRecord | undefined;
    };
  },
  document: PersistedWecomDocument,
  workspaceId: string,
): PrivateImRunSummary | undefined {
  const run = stores.agentRuns.get(runId);
  const inbound = stores.channels.getInboundForRun(runId);
  if (!run || !inbound
    || run.owner.entryPoint !== 'im'
    || run.owner.identity.kind !== 'channel_user'
    || run.owner.identity.authenticatedBy !== 'channel_adapter'
    || run.owner.identity.authorityId !== inbound.connectionId
    || run.owner.identity.subjectId !== inbound.conversationDigest
    || run.context.workspaceId !== workspaceId
    || !run.context.conversation.namespace.startsWith('im:wecom:')
    || run.context.conversation.conversationId !== `single:${inbound.conversationDigest}`
    || run.context.delivery.kind !== 'channel'
    || run.context.delivery.routeId !== inbound.inboundId
    || inbound.provider !== 'wecom'
    || inbound.conversationType !== 'single'
    || inbound.action !== 'run'
    || !document.connections.some((connection) => connection.connectionId === inbound.connectionId
      && connection.pairedSenderDigests?.includes(inbound.senderDigest))) return undefined;
  const outbound = stores.channels.getOutboundForRun(runId);
  if (outbound && (outbound.inboundId !== inbound.inboundId || outbound.runId !== runId)) return undefined;
  return {
    runId: run.runId,
    runStatus: run.status,
    replyDeliveryStatus: outbound?.status ?? 'not_created',
  };
}

export interface RuntimeCleanupResources {
  closeAssistantWorker?(): Promise<void>;
  closeChannels?(): Promise<void>;
  closeScheduler(): Promise<void>;
  closeNotificationRouter(): void;
  closeAgentService(): Promise<void>;
  closePythonSource?(): Promise<void>;
  closeTailscale?(): Promise<void>;
  closeMetadata(): void;
}

export async function cleanupRuntimeResources(resources: RuntimeCleanupResources): Promise<void> {
  const failures: unknown[] = [];
  const [schedulerResult] = await Promise.allSettled([
    Promise.resolve().then(() => resources.closeScheduler()),
  ]);
  if (schedulerResult?.status === 'rejected') failures.push(schedulerResult.reason);

  // Scheduled delivery consumes an active channel, so stop new deliveries first.
  const channelResults = await Promise.allSettled([
    ...(resources.closeChannels
      ? [Promise.resolve().then(() => resources.closeChannels!())]
      : []),
  ]);
  failures.push(...channelResults
    .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    .map((result) => result.reason));

  const closeResults = await Promise.allSettled([
    ...(resources.closeAssistantWorker
      ? [Promise.resolve().then(() => resources.closeAssistantWorker!())]
      : []),
    Promise.resolve().then(() => resources.closeNotificationRouter()),
    Promise.resolve().then(() => resources.closeAgentService()),
    ...(resources.closePythonSource
      ? [Promise.resolve().then(() => resources.closePythonSource!())]
      : []),
    ...(resources.closeTailscale
      ? [Promise.resolve().then(() => resources.closeTailscale!())]
      : []),
  ]);
  failures.push(...closeResults
    .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    .map((result) => result.reason));

  try {
    resources.closeMetadata();
  } catch (error) {
    failures.push(error);
  }

  if (failures.length > 0) {
    throw new AggregateError(failures, 'Runtime cleanup did not complete cleanly.');
  }
}
