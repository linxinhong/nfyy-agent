# Contracts

本目录存放公共协议黄金 fixtures。fixture 与 `packages/protocol-*` 中的 Go 类型通过
`internal/golden` 双向绑定（fixture → 类型可解码；类型 → fixture 逐字段一致）。

## 版本规则

- 每个契约代际一个目录：`schemas/v1/...`。破坏性变更必须新开 `v2/` 并保留 `v1/`，
  属于公共契约变更，需架构评审（见 `docs/adr/0001-foundation-contracts.md`）。
- 各协议包内的 `ContractVersion` 常量必须与目录一致，由契约测试保证。

## 清单（v1）

```text
openai/chat-completions.request.json
openai/chat-completions.response.json
openai/chat-completions.stream-chunk.json
openai/models.response.json
anthropic/messages.request.json
anthropic/messages.response.json
anthropic/messages.stream-event.json
mcp/initialize.request.json
mcp/initialize.response.json
mcp/tools-list.request.json
mcp/tools-list.response.json
enterprise/bootstrap.response.json
enterprise/me.response.json
enterprise/token-exchange.request.json
enterprise/token-exchange.response.json
enterprise/api-key.create.request.json
enterprise/api-key.create.response.json
enterprise/api-key.view.json
enterprise/jwks.example.json
error/problem.example.json
```

`mcp/` 固定 MCP protocolVersion `2025-06-18`。修改 fixture 即修改公共契约。
`openapi/`（OAS 文档）与 `events/`（事件 schema）随 owning slice 补充。
