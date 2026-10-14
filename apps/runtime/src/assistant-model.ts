import { join } from 'node:path';
import { ModelRuntime } from '@earendil-works/pi-coding-agent';
import { createModels, createProvider, type Api, type Model, type ModelAuth, type ProviderStreams } from '@earendil-works/pi-ai';
import { anthropicMessagesApi } from '@earendil-works/pi-ai/api/anthropic-messages.lazy';
import { googleGenerativeAIApi } from '@earendil-works/pi-ai/api/google-generative-ai.lazy';
import { openAICompletionsApi } from '@earendil-works/pi-ai/api/openai-completions.lazy';
import { openAIResponsesApi } from '@earendil-works/pi-ai/api/openai-responses.lazy';
import type { AssistantHost } from '@yuanpu-agent/assistant';
import { withYuanpuModelCompatibility } from '@yuanpu-agent/runtime-kit';

export interface AssistantModelConfig {
  model: Model<Api>;
  auth: ModelAuth;
}

export interface AssistantModelSelection {
  appPath: string;
  agentPath: string;
  provider: string;
  model: string;
}

/** The Runtime host reads model configuration and resolves credentials; neither path is sent to the Worker. */
export async function resolveAssistantModelConfig(selection: AssistantModelSelection): Promise<AssistantModelConfig> {
  const runtime = await ModelRuntime.create({
    authPath: join(selection.appPath, 'auth.json'),
    modelsPath: join(selection.appPath, 'models.json'),
    modelsStorePath: join(selection.agentPath, 'models-store.json'),
  });
  const model = runtime.getModel(selection.provider, selection.model);
  if (!model) throw new Error(`Unknown assistant model ${selection.provider}/${selection.model}.`);
  const credentials = await runtime.getAuth(model);
  if (!credentials) throw new Error(`No credentials for assistant model ${selection.provider}/${selection.model}.`);
  return { model: withYuanpuModelCompatibility(model), auth: credentials.auth };
}

function streams(api: Api): ProviderStreams {
  switch (api) {
    case 'openai-completions': return openAICompletionsApi();
    case 'openai-responses': return openAIResponsesApi();
    case 'anthropic-messages': return anthropicMessagesApi();
    case 'google-generative-ai': return googleGenerativeAIApi();
    default: throw new Error(`Assistant model API is not yet supported: ${api}`);
  }
}

export function assistantModelHost(resolveConfig: () => Promise<AssistantModelConfig>): AssistantHost {
  return {
    async resolveModel() {
      const { model, auth } = await resolveConfig();
      const models = createModels();
      models.setProvider(createProvider({
        id: model.provider,
        name: model.provider,
        baseUrl: model.baseUrl,
        models: [model],
        auth: { apiKey: { name: 'Runtime host', resolve: async () => ({ auth }) } },
        api: streams(model.api),
      }));
      return { models, model };
    },
  };
}
