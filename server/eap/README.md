# 知识服务中心（EAP）

企业所有 Agent 客户端的统一接入、身份、模型、工具、知识、安全、协作和审计基础设施。

员工只需要一个端点和一个 Enterprise API Key（`agk_*`）即可接入公司授权的全部 Agent 能力；API Key 在 Agent Gateway 终结，内部服务只接受 audience 限定的短时 JWT 与 Service Identity。

- 产品需求：`docs/01-PRD.md`
- 系统架构：`docs/03-System-Architecture.md`
- 开发计划：`docs/02-Development-Plan.md`
- 导入与本仓适配：`YUANPU-IMPORT.md`
- 契约冻结决策：`docs/adr/0001-foundation-contracts.md`
- 本地开发：`docs/dev/LOCAL-DEV.md`

## 快速开始

```bash
make dev-up       # PostgreSQL / Redis / MinIO / gateway 一键启动
make dev-ready    # 断言 /healthz 与 /readyz
make verify-foundation   # 仓库验证目标：fmt + vet + build + test + race
```

## 仓库结构

```text
apps/gateway/            # Agent Gateway（唯一公共入口：身份切片已上线）
apps/web/                # EAP Portal（登录 / 我的 Key / Agent Center）
packages/
  identity/              # Canonical Principal + Workspace 权威契约
  token/                 # 内部 JWT（Ed25519、aud 强校验、JWKS）
  apikey/                # agk_* 员工密钥格式与哈希
  errs/                  # 公共错误 schema 与 fail-closed 语义
  tracing/               # W3C traceparent + X-Request-ID 传播契约
  audit/                 # 可靠安全审计事件契约
  store/                 # PostgreSQL 持久层（用户/成员/密钥/签名密钥/审计）
  protocol-openai/       # OpenAI 兼容 wire 契约
  protocol-anthropic/    # Anthropic Messages 兼容契约
  protocol-mcp/          # MCP JSON-RPC 契约
  protocol-enterprise/   # /enterprise/v1/* 契约（me/keys/exchange/bootstrap）
contracts/schemas/v1/    # 协议黄金 fixtures（版本化）
internal/golden/         # fixture 绑定测试助手
migrations/              # SQL 迁移（gateway 启动时幂等应用）
deploy/                  # docker compose + gateway 镜像
scripts/                 # dev-ready 等开发脚本
```

## 验证目标

| 目标 | 说明 |
|---|---|
| `make verify-foundation` | 基础切片完整门禁（CI 同款） |
| `make verify-identity-slice` | 基础门禁 + 真实 PG/Redis 身份闭环集成测试 |
| `make verify-portal-shell` | 身份门禁 + Portal 构建/守卫/浏览器流（Playwright） |
| `make verify-llm-kernel-spike` | 基础门禁 + Bifrost 内核切片测试 |
| `make verify-mcp-slice` | 身份门禁 + MCP 隔离/凭据集成测试 |
| `make verify-provider-governance` | MCP 门禁 + 双 Provider 路由/回退/用量成本账务（TASK-008） |
| `make verify-production-oidc` | 身份门禁 + IdP stub 浏览器流/伪造重放闭合（TASK-009） |
| `make verify-policy-dlp-audit` | 治理门禁 + Policy 效果/脱敏/三类可靠审计（TASK-010） |
| `make verify-mcp-governance` | 治理门禁 + MCP 中央 Policy/审批/SSRF 旅程（TASK-011） |
| `make verify-knowledge-slice` | 治理门禁 + Knowledge 上传/引用检索/隔离旅程（TASK-012） |
| `make verify-llm-protocols` | 治理门禁 + 五协议契约面/SSE 取消/零出网拒绝（TASK-007） |
| `make verify-device-bootstrap` | M2-M5 门禁 + 设备授权流/轮换/ETag bootstrap（TASK-015） |
| `make verify-skill-registry` | 治理门禁 + Skill 供应链签名/轮换/负矩阵（TASK-016） |
| `make verify-eap-cli` | 治理门禁 + eap CLI 旅程/配置优先级/跨平台构建（TASK-017） |
| `make verify-eap-skill-sync` | CLI 门禁 + Skill 同步引擎（托管收敛/原子应用/负矩阵）（TASK-018） |
| `make verify-agent-distribution` | 同步门禁 + Agent 发布/同域票据/原子安装（TASK-019） |
| `make verify-m0-m3` | M0-M3 全链路集成与安全门禁（TASK-006） |
| `make verify-portal-deploy` | Portal 统一入口部署验收（镜像托管 UI + 部署断言 + E2E） |
| `make test` / `make race` | 单元 + 契约 + 安全回归测试 |
| `make dev-up` / `make dev-ready` | 本地环境验收（健康/就绪检查） |

安全契约回归（不可绕过）：

- 缺失 Workspace 权威 / 非服务端校验来源 → 拒绝（`packages/identity`）；
- 内部 JWT aud 错配、过期、篡改、alg 混淆、未知 kid、缺 workspace 声明 → 拒绝（`packages/token`）；
- Policy/DLP/Workspace 引擎自身失败 → fail closed 拒绝（`packages/errs`）；
- 安全审计投递失败 → 请求失败并可观测（`packages/audit`）。
