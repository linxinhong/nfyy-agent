/** Endpoint defaults only; explicit models.json compatibility settings take precedence. */
export function withYuanpuModelCompatibility<T extends { api: string; baseUrl: string; compat?: object }>(model: T): T {
  if (model.api !== 'openai-completions') return model;
  let hostname: string;
  try { hostname = new URL(model.baseUrl).hostname; } catch { return model; }
  if (hostname !== 'api.longcat.chat') return model;
  return { ...model, compat: {
    maxTokensField: 'max_tokens',
    thinkingFormat: 'deepseek',
    supportsReasoningEffort: false,
    ...model.compat,
  } };
}
