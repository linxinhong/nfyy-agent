# 企业 Agent 统一接入与协作平台开发计划

> 对应 PRD：V1.0  
> 推荐目标：先完成可投入公司内部使用的 V1，而不是一次性建设完整 Agent 平台。

## 1. 开发策略

### 1.1 原则

1. **Gateway First**：先打通“一个域名 + 一个 API Key”。
2. **Harness First, Protocol General**：先保证 Harness 好用，但接口必须标准化。
3. **Security by Default**：API Key、JWT、Service Identity、Policy 从第一版进入主链路。
4. **Knowledge Second Loop**：先实现可检索知识，再实现使用过程知识沉淀。
5. **Modular Monolith First**：控制面优先模块化单体，避免早期微服务膨胀。
6. **Open Standards First**：OpenAI API、Anthropic API、MCP、JWT、JWKS、OpenTelemetry。

## 2. 推荐团队

最小可行团队：

| 角色 | 人数 | 主要职责 |
|---|---:|---|
| Tech Lead / Architect | 1 | 架构、接口、权限、安全基线 |
| Go Backend | 2 | Gateway、Auth、MCP、Portal API |
| Frontend | 1 | Portal / Admin Console |
| AI / Knowledge | 1 | RAG、Embedding、Knowledge Pipeline |
| QA / Security | 0.5~1 | 自动化测试、安全测试、兼容测试 |

如果只有 2~3 人，优先延后 Artifact/Skill/Knowledge Candidate UI，不延后 Auth/Policy/Trace。

## 3. 技术基线建议

### 后端

- Go
- PostgreSQL
- Redis
- pgvector
- MinIO

### 前端

- React + TypeScript
- Vite
- Tailwind CSS
- shadcn/ui
- TanStack Query / Router

### 可观测

- OpenTelemetry
- OTLP Collector
- Langfuse（可先集成，后续替换）
- Prometheus / Grafana（基础基础设施指标）

### DLP

- Go Regex / Secret Scanner
- Presidio 作为可选增强服务

### Auth

- 企业已有 SSO 优先使用 OIDC/SAML Adapter
- API Key 自研
- JWT Ed25519/ES256
- JWKS

### CLI

- Go
- Cobra + Viper
- OS Keychain
- `net/http`
- deterministic `tar.gz` + SHA-256
- Ed25519 package signature verification

### 3.1 已有项目复用决策

项目不从零实现所有数据面能力。以下本地仓库作为代码来源或设计参考，但 EAP 的身份、Workspace、Policy、DLP 与 Audit 边界仍由本仓库掌握。

| 项目 | 定位 | 计划内复用 | 明确不复用 |
|---|---|---|---|
| `../bifrost` | 成熟 AI Gateway / Go SDK | Provider、OpenAI/Anthropic/Responses 协议、流式处理、Retry/Fallback、Key Pool、错误转换 | 企业身份源、Workspace 模型、完整 Dashboard、Agent Mode、OSS fallback RBAC |
| `../xingding-llmg` | 轻量 LLM Gateway 原型 | 路由与成本算法、SSE/契约测试、EAP 数据面装配经验 | 员工 Key 直达 LLM、SQLite 生产事实源、静态 Admin Token、可丢关键 Audit |
| `../xingding-mcpa` | 受治理 MCP Server 内核 | Tool Registry、执行治理、审计语义、MCP 协议内核 | 当前 SQLite/simple runtime 生产装配、绕开 EAP Principal/Workspace 的管理面 |
| `../xingding-ui-cli` | React 管理台脚手架 | Portal 工程底座、设计系统、路由、Query、i18n、版本偏移防护 | 私有 Agent Runtime 契约、敏感对话本地持久化、现成 EAP 业务页面 |

#### LLM 数据面决策门

M2 开始前必须完成一个有自动化契约测试的技术验证：

1. 将 Bifrost Core 作为库嵌入薄 EAP LLM Gateway。
2. 验证内部 JWT/Workspace 上下文、Responses、Anthropic、SSE、取消、Retry/Fallback、Usage/Cost Hook。
3. 验证 Policy/DLP 拒绝路径是 fail-closed；不能依赖 Bifrost 通用 Plugin error 自动阻断。
4. 评估升级方式、依赖体积、Provider 扩展成本和 Apache-2.0 NOTICE 义务。

验证通过时，以 Bifrost Core 作为 Provider/协议执行内核，`xingding-llmg` 仅保留值得迁移的算法与测试。验证不通过时，记录 ADR，并从 Bifrost 选择性移植协议与 Provider 层。禁止长期维护两个功能重叠的 LLM Gateway。

#### MCP 与 UI 决策

- MCP 主实现以 `xingding-mcpa` 为内核，Bifrost MCP 只作为协议、OAuth 和兼容性参考。
- Portal 使用 `xingding-ui-cli` 一次性生成并纳入本仓库维护，不把脚手架作为运行时依赖。
- V1 默认不启用通用 Agent Chat/Agent Mode，保持“平台提供基础设施，不建设通用 Agent Runtime”的产品边界。

## 4. 推荐仓库结构

```text
enterprise-agent-platform/
├── apps/
│   ├── gateway/                 # 对外唯一 API 入口
│   ├── portal-api/              # 控制面 API，可早期与 gateway 合并
│   └── web/                     # Portal + Admin Console
│
├── cmd/
│   └── eap/                     # 官方单文件 CLI
│
├── services/
│   ├── llm-gateway/
│   ├── mcp-gateway/
│   ├── knowledge/
│   └── worker/
│
├── packages/
│   ├── identity/
│   ├── apikey/
│   ├── token/
│   ├── policy/
│   ├── dlp/
│   ├── tracing/
│   ├── audit/
│   ├── eapcli/                  # CLI command/client/config/sync
│   ├── skillpkg/                # deterministic package + verify
│   ├── agentadapter/            # local Agent adapters
│   ├── protocol-openai/
│   ├── protocol-anthropic/
│   └── protocol-mcp/
│
├── contracts/
│   ├── openapi/
│   ├── events/
│   └── schemas/
│
├── migrations/
├── deploy/
└── docs/
```

V1 不强求每个 `services` 都独立部署，可按负载和安全边界逐步拆分。

## 5. 里程碑计划

推荐 12 周 V1，按实际团队规模可压缩/延长。EAP CLI、Device Authorization、Skill Registry 和 Agent Distribution 已提升为 V1 主链能力；Artifact/Knowledge Candidate 不阻塞 V1。

## M0：架构基线与项目骨架（第 1 周）

### 输出

- Repo
- CI
- Docker Compose
- PostgreSQL / Redis / MinIO
- API Contract 目录
- OpenTelemetry 基础
- Error Model
- Request ID / Trace ID
- 安全编码规范

### 核心决策

- API Prefix
- Principal Model
- Workspace Model
- API Key 格式
- JWT Claims
- Service Identity
- Audit Event Schema
- 本地四个可复用项目的版本/许可证清单
- Bifrost Core 集成验证 ADR 与退出条件

### 验收

- 所有服务可本地一键启动
- `/healthz`、`/readyz`
- Trace ID 可贯穿 Gateway → Demo Service

---

## M1：Identity + API Key + Unified Gateway（第 2~3 周）

### 任务

#### SSO / User

- OIDC Adapter
- User Sync
- Department / Organization 基础模型

#### API Key

- Create
- List
- Revoke
- Expire
- Prefix + Hash
- Last Used

#### Gateway

- Bearer Auth
- Principal
- Workspace Header
- Rate Limit
- Request Audit

#### JWT

- Ed25519 / ES256
- JWKS
- Audience
- Expiry
- JTI

### 验收场景

```text
SSO 登录
→ 创建 agk_xxx
→ curl agent.company.com/enterprise/v1/me
→ 返回用户和 Workspace
```

吊销后立即拒绝。

---

## M2：LLM Gateway（第 3~4 周）

### P0 API

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/responses
POST /v1/embeddings
```

Anthropic Messages 可在 M2 后半段增加。

### Provider Adapter

优先通过 Bifrost Core 支持 2~3 个当前公司真实使用 Provider。先完成集成验证，再决定“库依赖”或“选择性移植”；不得直接 Fork 完整 Gateway 后暴露其管理面。

`xingding-llmg` 中已验证的路由、成本、SSE 与契约测试应逐项比较后迁移，迁移完成的能力必须删除重复实现或明确唯一维护方。

### Model Alias

```text
daily-fast
coding-fast
coding-high
reasoning-high
```

### 策略

- User / Department Model Allowlist
- Provider Routing
- Timeout
- Retry
- Fallback
- Usage
- Cost

### 验收

Harness 只配置：

```text
base_url=https://agent.company.com
api_key=agk_xxx
model=coding-high
```

可以完成真实任务。

---

## M3：MCP Gateway + Credential 基线（第 4~6 周）

### MCP Registry

- Server
- Version
- Endpoint
- Owner
- Workspace Scope
- Tools Cache
- Health

### MCP Gateway

- Streamable HTTP Proxy
- API Key Auth
- Internal JWT
- Tool Discovery
- Tool Allow/Deny
- Tool Risk Level
- Trace

以 `xingding-mcpa` 的 Registry、执行治理和审计语义为起点，生产装配统一改为 PostgreSQL/Redis、EAP 内部 JWT 与 Workspace 强制校验。Bifrost MCP Agent Mode 不进入 V1 主链。

### Credential

V1 先实现 Credential Reference，不把 Secret 写入 Agent 配置。

可先使用：

- K8s Secret / 环境 Secret
- 后续迁移 Vault / KMS

### 首批 MCP

推荐只选 3 类：

1. Git / GitLab
2. Knowledge
3. 一类只读内部数据源

不要第一版就接 SSH / Kubernetes 高危写操作。

### 验收

同一个 `agk_xxx`：

```text
LLM ✓
MCP Git Read ✓
MCP DB Read ✓/按权限
```

真实 Git Token 不出现在员工机器。

---

## M4：Knowledge Hub（第 5~7 周）

### 数据模型

- KnowledgeSpace
- Document
- Chunk
- Source
- Citation
- KnowledgeVersion
- ACL

### Pipeline

- Upload
- Parse
- Chunk
- Embed
- Index
- Search
- Citation

### Retrieval

V1 推荐：

```text
Vector Search
+
Keyword/BM25（有条件则加入）
+
简单 Rerank
```

不要第一版追求过度复杂 Agentic RAG。

### API

```text
POST /enterprise/v1/knowledge/search
GET  /enterprise/v1/knowledge/spaces
POST /enterprise/v1/knowledge/documents
```

### 验收

- Workspace A 无法查询 Workspace B 私有知识
- 每条回答/检索结果可追溯 Source
- Harness 可通过统一 Key 查询知识

---

## M5：Policy + DLP + Audit（第 6~8 周）

Policy 从 M1 开始预留，这一阶段完成治理能力。

### Policy

输入：

```text
Principal
Workspace
Model
Provider
MCP Tool
Data Class
Client
```

输出：

```text
ALLOW
ALLOW_WITH_REDACTION
LOCAL_MODEL_ONLY
REQUIRE_APPROVAL
DENY
```

### DLP V1

- Secret
- API Key
- Token
- Password Pattern
- DB URL
- Phone / ID 等基础 PII
- 自定义词典

### Audit

- API Key Event
- LLM Event
- MCP Event
- Knowledge Event
- Policy Event
- DLP Event

### 验收

模拟：

```text
内部敏感字符串 → 外部模型
```

策略可以正确：

- 脱敏
- 路由到本地模型
- 拒绝

并留下审计记录。

---

## M6：Agent Portal + Harness 分发（第 7~9 周）

### Portal

- SSO
- 我的 API Key
- Agent Center
- Workspace
- 基础 Usage

Portal 工程由 `xingding-ui-cli` 生成后纳入本仓库；只复用工程和设计系统。移除或默认关闭私有 `/runtime/agents/*` 面板，并在接入任何对话能力前解决敏感数据本地持久化、上传 DLP 与 Workspace 上下文问题。

### Agent Distribution

- Harness Download
- OS / Arch
- Version
- Minimum Version
- Release Notes
- Config Template

### Admin Console

- User
- Model
- MCP
- Knowledge
- API Key Audit
- Client Version
- Policy

### 验收

员工从零开始：

```text
登录 Portal
→ 下载 Harness
→ 创建 Key
→ 复制配置
→ 成功使用 LLM + MCP + Knowledge
```

---

## M7：EAP CLI + Skill Registry（第 9~11 周）

### Device Authorization

- `eap login/logout/whoami`
- Browser + enterprise SSO device approval
- rotating device credential in OS Keychain
- CI/headless `EAP_API_KEY=agk_*`

### Skill

- `init/validate/test/pack/publish/review/release`
- `search/info/install/update/remove/sync`
- immutable versions and Workspace scope
- deterministic package, independent server scan, SHA-256 and server release signature
- `.eap/workspace.yaml` desired state plus generated `eap.lock`

### Bootstrap / Doctor

- Models / MCP / Skills / Policies / Agent Profiles
- stable human and `--json` output
- connectivity, credential, Workspace, adapter, MCP and model checks

### Local adapters

- Harness
- Codex
- Claude Code
- Pi
- project-local `.agents/skills`

### 验收

```text
Device Login
→ Skill init/publish/review/release
→ eap sync
→ Harness + Codex receive the signed Skill
→ revoke release
→ sync removes only the managed version
```

Artifact 与 Knowledge Candidate 进入后续里程碑，不阻塞 V1。

---

## M8：Agent Distribution、兼容测试与生产基线（第 11~12 周）

### Agent Distribution

- Agent Client / Release / OS / Arch
- minimum version and release notes
- same-domain short-lived download ticket
- SHA-256 and platform signature verification
- `eap agent list/install/update` atomic install and rollback

### 客户端

至少验证：

- Harness
- 一个非 Harness Agent

推荐优先 Pi 或其他明确支持自定义 Base URL/API Key/MCP 的客户端。

### 安全测试

- Key Replay
- Revoked Key
- Workspace Bypass
- JWT Audience Bypass
- Expired JWT
- MCP Tool ACL
- SSRF
- Prompt Secret Leakage
- Credential Leakage
- Rate Limit Bypass

### 性能

- Auth QPS
- Gateway Overhead
- Streaming
- MCP Long Connection
- LLM Concurrent Streaming

## 6. 数据库核心表建议

```text
users
organizations
departments
workspaces
workspace_members

api_keys
clients
client_releases
devices

model_aliases
model_providers
model_routes
usage_records

mcp_servers
mcp_tools
mcp_permissions
credential_refs

knowledge_spaces
documents
chunks
knowledge_versions
knowledge_candidates

skills
skill_versions
artifacts

policy_sets
policy_rules
dlp_rules

audit_events
```

## 7. API Contract 优先级

### 第一批必须冻结

```text
Authentication Header
Error Schema
Trace Header
Workspace Header
/v1/models
/v1/chat/completions
/v1/responses
/enterprise/v1/bootstrap
/enterprise/v1/me
/mcp/{server}
```

### Header 建议

```http
Authorization: Bearer agk_xxx
X-Agent-Workspace: secops
X-Agent-Client: harness
X-Agent-Client-Version: 1.6.2
X-Agent-Device: device_xxx
```

服务端生成/传播：

```http
traceparent
X-Request-ID
```

## 8. 关键技术任务卡

### T01 API Key

- 安全随机生成
- Prefix
- Hash
- Revoke
- Expiry
- Cache

### T02 Principal

统一业务代码中不直接依赖 API Key 字符串。

```go
type Principal struct {
    UserID       string
    Organization string
    Department   string
    WorkspaceID  string
    Roles        []string
    Scopes       []string
    ClientID     string
    DeviceID     string
}
```

### T03 Token Exchange

```text
API Key
→ Principal
→ scoped internal JWT
```

### T04 Policy Hook

所有：

```text
LLM
MCP
Knowledge
```

必须走相同 Policy Interface。

### T05 Trace Context

所有内部调用强制传播 OpenTelemetry Context。

## 9. 测试策略

### Contract Test

重点测试 OpenAI / MCP 兼容性。

### Golden Test

LLM Gateway 输入输出转换采用固定样例。

### Security Regression

安全策略必须自动化回归。

### Client Compatibility Matrix

维护：

| Client | OpenAI Chat | Responses | Anthropic | MCP | Bootstrap | Status |
|---|---|---|---|---|---|---|
| Harness |  |  |  |  |  |  |
| Pi |  |  |  |  |  |  |
| WorkBuddy |  |  |  |  |  |  |
| Codex |  |  |  |  |  |  |
| Claude Code |  |  |  |  |  |  |

V1 强制资格组合为 Harness + Codex；Pi/Claude Code Adapter 必须通过本地路径和配置契约测试。WorkBuddy/OpenCode 在其稳定安装契约明确后接入。

## 10. 上线策略

### Alpha

5~10 名研发/技术人员。

只启用：

- LLM
- Git Read MCP
- Knowledge

### Beta

一个部门。

增加：

- Git Write
- Artifact
- Skill
- DLP

### GA

全公司。

要求：

- SSO
- Key Lifecycle
- Audit
- DLP
- Client Minimum Version
- HA
- Backup
- Incident Runbook

## 11. V1 完成定义

不是“所有功能开发完成”，而是以下闭环稳定：

```text
员工登录
→ 获取/自动获得企业凭据
→ 安装一个 Agent
→ 仅配置统一 Endpoint
→ 调模型
→ 查知识
→ 调 MCP
→ 全程 DLP / Policy / Audit
→ 通过 eap 安装/同步签名 Skill 与 Agent 配置
```

只要这条链稳定，平台就已经具备实际组织价值。
