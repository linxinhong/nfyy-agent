# ADR-0005 MCP 治理切片（TASK-004）

状态：已接受　日期：2026-09-01　关联任务：TASK-004

## 决策

### 1. 内核复用方式：语义适配（而非模块依赖）

开发计划指定以 `xingding-mcpa` 的 Registry、执行治理与审计语义为起点。该仓库治理核心位于 Go `internal/` 包（模块外不可导入），且 `github.com/linxinhong/xingding-mcpa` 未发布到模块代理，无法钉版依赖（与 TASK-002 的 bifrost 不同）。故本切片**按语义适配**实现 EAP 自有的 `packages/mcpgateway`，设计来源锚定 xingding-mcpa 本地克隆（语义：deny-by-default、发现=执行同策略、凭据引用、可靠审计）。若上游未来发布可钉版内核包，可重新评估替换。

### 2. Workspace 策略（发现与执行同一谓词）

`mcp_tools.workspaces text[]` 声明可执行的工作空间；`GetToolForWorkspace`/`ListToolsForWorkspace` 使用同一 `workspaces @> ARRAY[ws]` 过滤：

- 发现：外部工作空间的工具**不出现在 tools/list**（满足任务卡 forbidden：不允许发现不可执行的工具）；
- 执行：显式调用外部工具返回与"未知工具"相同的错误（不存在性不泄露），并写 `mcp.tool.executed / denied / reason=workspace_not_allowed` 审计。

### 3. 凭据引用

`credential_refs`/`mcp_tools.credential_ref` 存引用（`env:VAR` 形态，Vault/KMS 前缀预留），执行时由 `CredentialResolver` 服务端解析，仅注入上游请求头。响应只透传上游载荷——单测与集成测试断言凭据永不回传 Agent。凭据缺失 → 调用失败闭合（不降级为匿名请求）。

### 4. 端点与执行

统一入口 `POST /mcp/{server}`（JSON-RPC 2.0：initialize / tools/list / tools/call，JSON 响应；Streamable HTTP 的 SSE 形态在 M3+ 按客户端需要补齐）。上游 endpoint 来自注册表（Agent 永远不能选 URL），`HTTPExecutor` 做 {arg} 模板替换（PathEscape）、响应 1MiB 上限、15s 超时；R2+ 风险的审批扩展点保留在 RiskLevel 元数据（审批引擎为 V1.5 范围）。

### 5. 身份与审计

复用 TASK-003 面：agk key 鉴权 + `X-Agent-Workspace` + 成员资格校验（`identity.SelectWorkspace`，resolver 故障 fail closed）；`token.Claims.AuthorizeCapability(aud)` 供服务拆分后的 mcp-gateway 进程强制 aud=mcp-gateway + workspace 权威。每次执行（允许/拒绝）经 `audit.RequireWrite` 落 `audit_events`。

### 6. 首批工具 fixture

`git_read_file`（gitlab server，R1，凭据引用）与 `internal_data_read`（eap server，R1，无凭据），均注册于 secops 工作空间；集成测试将 git 工具端点指向受控上游桩完成全链路验证。

## 明确不做

Bifrost MCP Agent Mode、SSH/K8s 高危写工具、通用审批/工作流引擎、SQLite 运行时（mcpa 的 simple runtime 不进入生产装配）。

## 验证

`make verify-mcp-slice` = verify-identity-slice（含 foundation）+ `TestMCPSliceIntegration`：初始化 → 授权发现含 git_read_file → 授权执行读取（响应含载荷、不含凭据）→ 跨工作空间发现不可见、执行被拒且审计留痕。
