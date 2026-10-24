# /v1 与 /mcp 协议覆盖度矩阵(TASK-042)

> 基线:Bifrost `core/schemas`(v1.8.4,仓库内 pinned)> openai/openai-openai 官方
> 规范 > modelcontextprotocol/spec。机械验收:`make verify-protocol-conformance`
> + `npx @modelcontextprotocol/inspector --cli <url> --transport http ...` +
> 一个真实客户端 journey(Hermes/Codex/litellm)。

## /v1/chat/completions

| 字段/行为 | 状态 | 说明 |
|---|---|---|
| model, messages(role/content/tool_calls/tool_call_id/name) | ✅ 透传 | 含 tool-role 往返 |
| tools, tool_choice | ✅ 字节级透传 | ADR-0025 |
| assistant tool_calls / 流式 tool_calls 增量 | ✅ | finish_reason 取 provider 值 |
| temperature, top_p, max_tokens | ✅ 透传 | TASK-042 |
| stop, parallel_tool_calls, stream_options | ✅ 透传 | TASK-042(opaque JSON) |
| assistant tool_calls 消息 content=null | ✅ | 严格客户端要求(TASK-042) |
| 多模态 content-part 数组请求 | ✅ 结构性透传 | TASK-052 / ADR-0028 决策 2:数组结构原样到达 provider;text part 拍平后照旧做 DLP 请求扫描并在原位改写(与现状字节等价);非文本部分(image_url 等)原样转发不扫描(V1 立场,出现真实多模态客户端时复核) |
| assistant 非文本增量(流式/非流式) | ✅ 结构化转发 | TASK-052 / ADR-0028 决策 2:provider 返回的 content-part 数组增量(含非文本 part)结构化转发,绝不进入 SSE 文本清洗器;text 部分仍走治理文本路径 |
| 错误信封 `{"error":{"message","type","code","param","trace_id"}}` | ✅ | type 为 OpenAI 分类(如 permission_error);/enterprise/* 保持 Problem 形状 |
| n, penalties(frequency/presence), response_format, seed, user, metadata, logprobs, service_tier 及其他未类型化字段 | ✅ 透明透传 | TASK-051 / ADR-0028 决策 1:opaque 捕获并字节级原样转发给 provider;provider 是唯一校验方;受治理字段同名冲突时优先;DLP/策略/审计语义从不依赖 opaque 字段;不得用 opaque 字段承载授权/身份上下文 |
| usage | ✅ 非流式+流式末帧 | stream_options.include_usage 透传给 provider |

## /mcp(Streamable HTTP)

| 行为 | 状态 | 说明 |
|---|---|---|
| POST JSON-RPC(JSON 响应) | ✅ | 默认形态 |
| Accept: text/event-stream 协商(SSE 帧响应) | ✅ | TASK-042;单消息后收流(无状态面) |
| Mcp-Session-Id(initialize 下发) | ✅ | 无状态会话句柄;不强制校验 |
| GET SSE 流 | ✅ 405 | 规范允许无状态服务端拒绝 GET 流 |
| capabilities: tools(listChanged=false) | ✅ | resources/prompts 未声明 |

## 验收流程(每次 /v1 或 /mcp 契约变更)

1. `make verify-protocol-conformance`
2. Inspector:`npx @modelcontextprotocol/inspector --cli http://127.0.0.1:18080/mcp/agent --transport http --header "Authorization: Bearer $KEY" --header "X-Agent-Workspace: <ws>" --method tools/list`
3. 真实客户端 journey:litellm 或 Hermes 完成一次 chat + tool-calling + 错误路径。
