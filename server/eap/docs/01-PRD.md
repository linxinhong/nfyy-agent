# 企业 Agent 统一接入与协作平台 PRD

> 工作名称：Enterprise Agent Platform（EAP）  
> 版本：V1.0  
> 日期：2026-08-31  
> 状态：需求基线

## 1. 产品背景

公司员工已广泛使用 Harness Agent 等 Agent 客户端开展研发、方案、文档、分析、运维等日常工作。随着 Agent 使用规模扩大，现有模式逐步暴露出以下问题：

1. 每个员工分别维护模型 API Key、MCP 凭据、知识库地址和工具配置，接入复杂且难以统一治理。
2. 模型调用分散，无法统一控制模型、成本、配额、敏感数据、日志与审计。
3. MCP 工具直接持有 Git、数据库、SSH、业务系统等真实凭据，权限和泄露风险较高。
4. 员工的 Agent 使用过程产生大量有价值的方案、决策、经验和文档，但缺乏统一沉淀机制。
5. 公司主用 Harness，但未来可能同时使用 WorkBuddy、Pi Agent、Codex、Claude Code、OpenCode 等客户端，不应把企业基础设施绑定到单一 Agent。
6. Agent 客户端的版本、安装包、配置和升级缺乏统一分发机制。

因此需要建设一套企业级 Agent 基础设施，以统一入口承载企业内部全部 Agent 能力。

## 2. 产品定位

本产品不是新的聊天机器人平台，也不是替代 Harness 的统一 Agent Runtime。

产品定位为：

> **企业所有 Agent 客户端的统一接入、身份、模型、工具、知识、安全、协作和审计基础设施。**

员工只需要：

```text
https://agent.company.com
+
Enterprise API Key
```

即可接入公司授权的全部 Agent 能力。

公司统一治理：

- 身份与组织
- API Key
- Agent 客户端分发
- 模型与模型路由
- MCP Server / Tool
- 企业知识
- Skill
- Artifact
- 敏感数据
- 权限策略
- 调用成本
- Trace / Audit

## 3. 核心设计原则

### P1. 一个域名

所有客户端仅连接：

```text
https://agent.company.com
```

### P2. 一个员工侧凭据

员工侧只持有 Enterprise API Key：

```text
agk_live_xxxxxxxxx
```

LLM、MCP、Knowledge、Skill、Artifact 不再分别申请 Key。

### P3. 外部一个 Key，内部不透传

Enterprise API Key 只用于 Agent Gateway。

内部服务使用：

- Short-lived JWT：代表用户身份，建议 5~15 分钟
- Service Token：服务到服务身份
- Vault/KMS Credential：访问第三方系统真实凭据

### P4. 客户端解耦

优先支持 Harness，但协议不与 Harness 私有绑定。

只要客户端支持以下任一标准能力即可接入：

- OpenAI Compatible API
- OpenAI Responses API
- Anthropic Messages API
- MCP Streamable HTTP
- 自定义 Base URL + API Key

### P5. 企业只暴露一个外部安全边界

`agent.company.com` 是唯一外部 Agent 能力入口。

内部仍采用最小权限、短期凭据和服务身份，不建立“内网即可信”的假设。

### P6. 知识在使用过程中沉淀

不要求员工工作完成后手工整理知识。

平台从 Agent 产出的 Artifact、决策、任务结果中提取知识候选，经过治理后进入正式知识库。

### P7. 不自动把所有对话写入知识库

Conversation 是原始工作数据，不直接等于 Canonical Knowledge。

知识需经过：

```text
提取 → 去重 → 冲突检测 → 敏感检查 → 来源追踪 → 可信度 → 审核/规则 → 发布
```

## 4. 用户角色

### 4.1 普通员工

- 企业 SSO 登录
- 下载/配置允许使用的 Agent 客户端
- 创建和吊销自己的 API Key
- 使用企业模型、MCP、Knowledge、Skill
- 查看个人调用量、最近设备和 Key 状态

### 4.2 Workspace Owner

- 管理 Workspace 成员
- 管理 Workspace Knowledge
- 发布 Workspace Skill
- 选择允许使用的 MCP / Model
- 审核知识候选

### 4.3 Agent Platform Admin

- 管理全局模型、Provider 和路由
- 管理 MCP Registry
- 管理 Agent 客户端版本及分发
- 管理 DLP / Policy
- 管理配额和成本
- 管理组织权限
- 查看平台 Trace / Audit

### 4.4 Security / Auditor

- 查询 Agent 调用审计
- 查询敏感数据命中
- 查询高风险 MCP 操作
- 查询 Key、设备、IP、客户端版本
- 执行 Key 吊销、客户端版本阻断和策略调整

## 5. 典型用户流程

### 5.1 首次接入

```text
访问 agent.company.com
        ↓
企业 SSO 登录
        ↓
Agent Center
        ↓
选择 Harness / Pi / WorkBuddy / 其他客户端
        ↓
下载或生成配置
        ↓
创建 API Key 或执行 Device Login
        ↓
客户端自动发现模型 / MCP / Skill / Workspace
        ↓
开始工作
```

### 5.2 API Key 接入

```text
AGENT_BASE_URL=https://agent.company.com
AGENT_API_KEY=agk_live_xxxxx
```

API Key 只展示一次完整值；平台仅保存不可逆 Hash 和 Key Prefix。

### 5.3 EAP CLI 与 Device Login（V1）

```text
eap login
        ↓
打开 agent.company.com
        ↓
企业 SSO
        ↓
Device Authorization
        ↓
OS Keychain 保存设备绑定、可轮换凭据
```

CLI 设备凭据与 `agk_*` 使用同一个 Principal、Workspace、Roles、Scopes、Policy 和 Audit，不形成第二套身份体系。CI、Server 和无头环境继续支持：

```bash
export EAP_BASE_URL=https://agent.company.com
export EAP_API_KEY=agk_xxxxxxxxx
```

`eap` 同时是开发者工具、Skill Package Manager 和本地 Agent Bootstrap Client，负责 Skill 生命周期、Workspace 同步、Agent 安装配置和企业环境诊断。

## 6. 功能范围

## 6.1 Agent Portal

### 必须

- SSO 登录
- 个人首页
- Agent Center
- API Key 管理
- Workspace 列表
- 客户端下载
- 客户端最低版本提示
- 配置生成
- 使用量基础统计

### Agent Center

展示公司允许使用的客户端：

```text
Harness
Pi Agent
WorkBuddy
Codex
Claude Code
OpenCode
其他管理员登记客户端
```

每个客户端维护：

- 名称
- 客户端类型
- 当前企业推荐版本
- 最低允许版本
- 下载地址
- 操作系统 / 架构
- 配置模板
- 支持协议
- 是否允许使用
- 灰度发布范围

## 6.2 Enterprise Agent Gateway

所有外部请求统一进入 Gateway。

职责：

- API Key Authentication
- Principal 构建
- Workspace Context
- Rate Limit
- Quota
- Policy Decision
- DLP
- Protocol Routing
- Internal Token Exchange
- Trace ID
- Audit

统一 Principal：

```text
UserID
OrganizationID
DepartmentID
WorkspaceID
Roles
Scopes
PolicySet
ClientID
DeviceID
```

## 6.3 API Key 管理

支持一个员工多个 Key。

建议场景：

```text
MacBook Harness
Windows WorkBuddy
Linux Pi
CI Agent
```

字段：

- key_id
- prefix
- key_hash
- user_id
- name
- created_at
- expires_at
- last_used_at
- last_ip
- client_id
- revoked_at

要求：

- 完整 Key 仅创建时展示一次
- DB 不保存明文
- 支持过期时间
- 支持单 Key 吊销
- 支持用户全量吊销
- 支持管理员强制吊销
- 支持异常使用检测

## 6.4 JWT / Service Token

### User JWT

由 Gateway 根据 API Key 身份动态签发，仅用于内部服务。

建议 Claims：

```json
{
  "iss": "https://agent.company.com",
  "sub": "user_123",
  "aud": "mcp-gateway",
  "workspace": "secops",
  "roles": ["developer"],
  "scopes": ["mcp:gitlab:read", "knowledge:read"],
  "jti": "...",
  "exp": 0
}
```

要求：

- `aud` 强校验
- 短有效期
- 非对称签名
- JWKS
- Key Rotation
- JTI 可追踪

### Service Token

服务以独立 Service Identity 访问其他服务，不允许使用员工 API Key。

示例：

```text
service://agent-gateway
service://llm-gateway
service://mcp-gateway
service://knowledge
service://audit
```

## 6.5 LLM Gateway

对外至少兼容：

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/responses
POST /v1/embeddings
POST /v1/messages        # Anthropic compatibility
```

员工使用逻辑模型名：

```text
daily-fast
reasoning-high
coding-fast
coding-high
vision
embedding
```

后台维护真实 Provider 映射。

能力：

- Provider Adapter
- Model Alias
- Model Routing
- Retry / Fallback
- Load Balance
- Token Usage
- Cost
- Quota
- Rate Limit
- Model Policy
- DLP Hook
- Trace Hook

V1 不要求开发复杂 Workflow Runtime。

## 6.6 MCP Gateway

统一入口：

```text
https://agent.company.com/mcp/{server}
```

员工继续使用同一个：

```http
Authorization: Bearer agk_live_xxx
```

Gateway 内部完成身份转换。

能力：

- MCP Registry
- MCP Server Registration
- Tool Discovery
- Streamable HTTP Proxy
- Tool Permission
- Parameter Policy
- Credential Broker
- Risk Level
- Approval Extension Point
- Rate Limit
- Trace / Audit

工具建议风险等级：

| 等级 | 定义 | 示例 |
|---|---|---|
| R0 | 公开只读 | 公共搜索 |
| R1 | 企业内部只读 | Git/知识/数据库查询 |
| R2 | 创建草稿或低风险写入 | 创建 Issue、生成草稿 |
| R3 | 修改业务数据 | 修改记录、提交代码 |
| R4 | 高风险生产操作 | 删除、发布、执行生产命令 |
| R5 | 高危/破坏性 | 批量删除、核心生产变更 |

## 6.7 Credential Broker

员工和 Agent 不直接持有业务系统真实凭据。

```text
Agent → API Key → Agent Gateway → MCP Gateway → Credential Broker → Target
```

支持：

- Git Token
- DB Credential
- SSH Key
- Cloud Credential
- Internal API Secret

后续接入 Vault / KMS。

## 6.8 Knowledge Hub

### 三层知识模型

#### Source Knowledge

原始资料：制度、规范、项目资料、代码文档、历史方案等。

#### Working Knowledge

Agent 工作中形成的经验、决策、问题解决记录。

#### Canonical Knowledge

经过治理、可正式复用的知识。

### V1 能力

- Workspace Knowledge
- 文档上传
- 文档解析
- Chunk
- Embedding
- Hybrid Retrieval
- Citation
- Source Provenance
- Knowledge ACL
- Knowledge Version

### Knowledge Candidate

Agent 工作完成后可生成候选：

```text
Artifact / Task Result
       ↓
Knowledge Candidate
       ↓
PII / Secret Scan
       ↓
Dedup
       ↓
Conflict Detection
       ↓
Confidence
       ↓
Review
       ↓
Canonical Knowledge
```

V1 可先采用人工审核，V2 再增加自动治理。

## 6.9 Skill Registry

Skill 作为企业知识的重要组成部分。

字段：

- name
- version
- owner
- scope
- description
- compatible_clients
- required_models
- required_mcp
- permissions
- status
- usage_count

支持：

- Global Skill
- Department Skill
- Workspace Skill
- Version
- Publish / Deprecate
- Client Pull

Skill 源包保持 Agent Skills 兼容结构：

```text
skill/
├── SKILL.md
├── skill.yaml
├── scripts/
├── references/
├── assets/
└── tests/
```

`SKILL.md` 保存标准 Skill 指令，`skill.yaml` 保存 owner、scope、兼容客户端、所需 MCP/capability 和 risk 等企业元数据。完整生命周期为：

```text
init → validate → test → pack → publish → review → release
     → install → sync → update → deprecate/revoke
```

CLI 负责确定性打包、开发者侧扫描和 SHA-256；服务端独立重复扫描并在审核通过后使用专用 release key 签名。客户端安装前必须验证摘要和平台签名。同名同版本不可覆盖，下载包不得自动执行脚本。

## 6.9.1 EAP CLI

V1 命令域：

```text
eap
├── login / logout / whoami
├── auth
├── agent
├── workspace
├── skill
├── mcp
├── model
├── config
├── sync
└── doctor
```

CLI 使用服务端标准 Skill Package，通过本地 Adapter 安装到 Harness、Codex、Claude Code、Pi 或项目级目录。Server 不维护员工本地文件路径。

Workspace 使用两份声明：

- `.eap/workspace.yaml`：人维护的期望状态；
- `eap.lock`：CLI 生成的精确版本、摘要、签名 key ID 和 Agent 目标。

`eap sync` 只变更 EAP 管理的文件，保留手工安装内容，并以原子写入和失败回滚完成安装、升级和策略要求的删除。

## 6.10 Artifact

统一管理 Agent 产出：

- Markdown
- 文档
- 报告
- 方案
- 代码评审
- Patch
- 图片
- 数据文件
- 任务结果

Artifact 是知识沉淀的重要来源，但 Artifact 本身不自动成为正式知识。

## 6.11 Trace / Audit

一次 Agent 工作建议贯穿统一 Trace ID：

```text
User Request
├── Knowledge Retrieval
├── LLM Call
├── MCP Tool Call
├── LLM Call
└── Artifact
```

记录：

- user
- client
- device
- workspace
- model
- provider
- token
- cost
- knowledge source
- MCP server/tool
- risk level
- policy decision
- DLP decision
- latency
- result status

敏感 Prompt / Response 是否落盘由数据策略控制，不应默认永久保存明文。

## 6.12 DLP / Policy

统一 Policy Engine 同时服务 LLM、MCP、Knowledge。

策略输入至少包括：

```text
User
Department
Workspace
Client
Data Classification
Model Provider
MCP Tool
Operation Risk
Resource
```

策略结果：

```text
ALLOW
ALLOW_WITH_REDACTION
LOCAL_MODEL_ONLY
REQUIRE_APPROVAL
DENY
```

检测范围：

- 手机号
- 身份信息
- 姓名
- 银行卡
- 内部 IP
- API Key
- Token
- 密码
- DB Connection String
- SSH Key
- Secret
- 企业自定义敏感词/实体

## 7. Workspace 模型

```text
Organization
└── Department
    └── Workspace
        ├── Members
        ├── Knowledge
        ├── Skills
        ├── MCP
        ├── Model Policy
        ├── Artifacts
        └── Audit
```

API Key 绑定 User，不绑定死单一 Workspace。

请求可通过：

```http
X-Agent-Workspace: secops
```

或者客户端配置选择 Workspace。

## 8. Bootstrap / Discovery API

建议提供：

```http
GET /enterprise/v1/bootstrap
Authorization: Bearer agk_xxx
```

返回当前员工可用的：

- Workspace
- Model Alias
- MCP Server
- Skill
- Client Policy
- Feature Flags
- Workspace Manifest Generation / ETag
- Skill package digest / release signature reference
- Agent release and minimum-version policy

目的是让受支持客户端做到“一次认证，自动完成企业配置”。

## 9. 非功能要求

### 安全

- API Key 不落明文
- TLS Only
- JWT 非对称签名
- JWKS Rotation
- 内部服务拒绝员工 API Key
- Service Identity 最小权限
- MCP 凭据隔离
- DLP
- 审计
- Key Revocation

### 可用性

V1 目标：

- Gateway 无单点
- LLM Provider 可配置 Retry / Fallback
- 关键服务支持健康检查
- 配置变更可审计

### 性能

V1 建议目标：

- Gateway 自身额外 P95 延迟 < 100ms（不含模型推理和外部工具）
- Auth/Policy Cache
- JWT 本地验签，避免每个内部请求回查 Auth DB

### 兼容性

优先支持：

- Harness Agent
- 任何 OpenAI Compatible 客户端
- 任何支持 MCP HTTP + Bearer Header 的客户端

逐步验证：

- Pi Agent
- WorkBuddy
- Codex
- Claude Code
- OpenCode

## 10. V1 范围

### P0

1. SSO + Portal
2. API Key Lifecycle
3. Agent Gateway
4. OpenAI Compatible LLM Gateway
5. MCP Gateway 基础 Proxy + Registry
6. Workspace
7. Knowledge RAG 基础能力
8. Policy / DLP 基础能力
9. JWT / Service Identity
10. Trace / Audit
11. Harness 下载与配置页
12. EAP CLI + Device Authorization
13. Skill Registry + 签名发布/安装/同步
14. Agent Release + 本地 Adapter

### P1

1. Artifact
2. Knowledge Candidate
3. Model Cost / Quota Dashboard
4. Credential Broker

### P2 / V1.5+

1. WorkBuddy/OpenCode 等新增 Adapter
2. MCP 高风险审批
3. Knowledge Conflict Detection
4. Client Grey Release
5. 企业 Memory

## 11. 明确不做

V1 不做：

- 自研通用 Agent Runtime
- 替代 Harness
- Dify 式复杂工作流编排
- 通用低代码 Agent Builder
- 全量聊天 UI
- 自动把所有聊天写入知识库
- 自研大型向量数据库
- 自研企业 IAM
- 一开始拆成大量微服务

## 12. V1 验收标准

1. 员工 SSO 登录后可以下载 Harness 并创建 API Key。
2. 仅配置 `agent.company.com + agk_xxx`，Harness 可以调用至少两个真实 LLM Provider。
3. 同一个 API Key 可以访问授权的 MCP Server。
4. 同一个 API Key 可以访问 Workspace Knowledge。
5. API Key 不被内部服务接受；内部调用使用 JWT/Service Identity。
6. 员工离职或 Key 吊销后，所有 Agent 能力立即不可访问。
7. 不同 Workspace 的知识和 MCP 权限可以正确隔离。
8. 敏感字段可以命中策略并执行脱敏/拒绝。
9. 一次请求可以串联 LLM、Knowledge、MCP 的统一 Trace。
10. 管理员可以查看用户、客户端、模型、MCP、Token、成本、DLP 和审计信息。
11. 至少验证 Harness + 1 个非 Harness Agent 客户端的兼容接入。
12. `eap login`、`eap doctor`、Skill 发布/审核/安装/同步和 Agent 安装/升级形成可审计闭环。
13. Skill 或 Agent 包的摘要、签名、Workspace 权限、版本吊销任一校验失败时，本地环境保持原有可用状态。

## 13. 产品成功指标

V1 上线后关注：

- Agent 平台月活员工数
- 企业统一 Gateway 覆盖的 Agent 调用比例
- 员工平均配置项数量下降幅度
- 独立第三方 API Key 数量下降幅度
- MCP 真实凭据暴露数量下降幅度
- Knowledge Retrieval 使用次数
- Artifact → Knowledge Candidate 转化率
- Canonical Knowledge 复用次数
- LLM 成本/人/部门趋势
- DLP 命中与拦截数量
- API Key 泄露/异常事件数量
