# ADR-0001 基础契约冻结（TASK-001）

状态：已接受（V1 基线）　日期：2026-09-01　关联任务：TASK-001

## 背景

EAP 需要一个可本地运行、模块化单体优先的基础骨架，并冻结下游 slice 共享的身份、Workspace、错误、Trace、Audit 与公共协议契约（PRD V1.0、系统架构 V1.0、开发计划 M0）。

## 决策

### 1. 仓库与模块布局

单一 Go module `enterprise-agent-platform`（Go >= 1.26），目录对齐开发计划 §4：

- `apps/gateway`：唯一公共入口（现阶段仅健康检查骨架）；
- `packages/{errs,identity,tracing,audit,token}`：基础契约；
- `packages/protocol-{openai,anthropic,mcp,enterprise}`：公共协议 wire 契约；
- `contracts/schemas/v1/`：协议黄金 fixture（版本化目录）；
- `internal/golden`：fixture 绑定测试助手；
- `migrations/`、`deploy/`：本地基线与部署。

未来拆分服务时按包边界切分，不需要修改契约。

### 2. Canonical Principal 与 Workspace 权威

`identity.Principal`（user/organization/department/roles/scopes/client/device）是唯一身份词汇。禁止把 bifrost 的 Virtual Key/Team/Customer、xingding-llmg 的 Tenant/App 或 xingding-mcpa 的模型作为 Principal。

Workspace 权威只允许两个 server-validated 来源，编码在 `WorkspaceContext.Source`：

- `server_membership`：网关通过 MembershipResolver 校验成员资格（resolver 出错 = fail closed）；
- `internal_jwt`：下游校验签名 JWT 中的 workspace 声明。

`Session.Validate()` 对缺失 workspace、非服务端来源一律拒绝。客户端头 `X-Agent-Workspace` 只做选择，永不授权（架构红线 4）。

### 3. 内部 JWT

- Ed25519（EdDSA），kid 头 + 验签端多 key（支持轮换）；JWKS 发布属 TASK-003。
- aud 单值、注册表封闭：`agent-gateway/llm-gateway/mcp-gateway/knowledge/control-plane-api/audit`；跨 aud 使用被契约测试拒绝。
- workspace 声明必填，缺失即拒签/拒验；TTL 默认 10 分钟、上限 15 分钟。
- 依赖：`github.com/golang-jwt/jwt/v5 v5.2.1`（社区标准实现，避免手写 JOSE；仅用于签发/验签，公共契约类型不暴露第三方类型）。

### 4. 公共错误契约

`errs.Problem { code, message, trace_id?, retriable }`，黄金 fixture `contracts/schemas/v1/error/problem.example.json`。代码→HTTP 映射冻结（未知代码 → 500 fail closed）。Policy/DLP/Workspace 引擎自身失败必须用 `PolicyFailClosed/DLPFailClosed/WorkspaceFailClosed` 拒绝请求。

### 5. Trace 传播

W3C `traceparent` + `X-Request-ID`：网关保留合法入站 trace，否则生成；响应始终回传规范头。OpenTelemetry SDK 接线属后续 slice，本契约即其 wire 基线（头与 id 格式不变）。

### 6. Audit 可靠性

`audit.Writer.Write` 必须阻塞至持久化成功或返回错误；`audit.RequireWrite` 使安全审计投递失败转为请求失败（fail closed）。best-effort telemetry 属另一条有损管道，不得替代。生产 PostgreSQL writer 属 TASK-003。事件 Detail 禁止出现凭据形态键（`ValidateNoSecrets` 契约护栏）。

### 7. 依赖钉版

`golang-jwt/jwt/v5 v5.2.1`、`jackc/pgx/v5 v5.10.0`、`redis/go-redis/v9 v9.22.0`，其余仅 stdlib。CI（`.github/workflows/ci.yml`）在 push/PR 上运行 `make verify-foundation`。

## 明确否决

- 复用兄弟项目的 Tenant/Team/Customer/App 类型作为 Principal（任务卡 forbidden）。
- 用恒真占位命令充当验证目标（任务卡 forbidden）；`verify-foundation` 全部为真实门禁。
- 本任务引入 OpenAI/Anthropic/MCP 的真实 Provider 实现（属 TASK-002/004）。

## 后果

- 下游 slice（TASK-002/003/004/005）共享同一套契约类型与 fixture，修改即公共契约变更，需架构评审。
- OpenTelemetry SDK 与 JWKS 发布沿用本契约的 wire 约定接入。
