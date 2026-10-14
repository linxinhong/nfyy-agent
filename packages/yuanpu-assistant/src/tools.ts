import type { Static, TSchema } from 'typebox';
import type { ToolResultMessage } from '@earendil-works/pi-ai';

/** Yuanpu's tool contract stays independent of the experimental Pi harness API. */
export interface AssistantTool<T extends TSchema = TSchema> {
  name: string;
  label: string;
  description: string;
  parameters: T;
  replay: 'safe' | 'never';
  execute(callId: string, input: Static<T>): Promise<{
    content: ToolResultMessage['content'];
    details?: unknown;
  }>;
}
