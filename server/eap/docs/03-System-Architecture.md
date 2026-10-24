# 企业 Agent 统一接入与协作平台系统架构

> Architecture Baseline V1.0

## 1. 架构目标

建立企业 Agent 的统一外部安全边界：

```text
https://agent.company.com
```

所有 Agent/CI 客户端使用统一 Enterprise API Key 接入；官方 `eap` CLI 可通过 Device Authorization 获得设备绑定凭据。两类凭据终止于同一 Gateway，并映射到同一个 Principal、Workspace 与权限体系；内部通过短期 JWT 和 Service Identity 访问具体能力服务。

系统不绑定单一 Harness，不自研通用 Agent Runtime。

## 2. 总体逻辑架构

```text
┌─────────────────────────────────────────────────────────────────┐
│                         Employee Devices                         │
│                                                                 │
│ Harness      Pi Agent      WorkBuddy      Codex      EAP CLI    │
└───────────────────────────────┬─────────────────────────────────┘
                                │
                                │ HTTPS
                                │ Authorization: Bearer agk_xxx / device access token
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Enterprise Agent Gateway                     │
│                    https://agent.company.com                    │
│                                                                 │
│ Auth │ Principal │ Workspace │ Policy │ DLP │ Quota │ Trace     │
│ Protocol Compatibility │ Routing │ Token Exchange               │
└───────────────────────────────┬─────────────────────────────────┘
                                │ scoped short-lived JWT
              ┌─────────────────┼───────────────────┐
              │                 │                   │
              ▼                 ▼                   ▼
      ┌──────────────┐  ┌──────────────┐   ┌────────────────┐
      │ LLM Gateway  │  │ MCP Gateway  │   │ Knowledge Hub  │
      │              │  │              │   │                │
      │ Provider     │  │ Registry     │   │ Documents      │
      │ Routing      │  │ Tool ACL     │   │ Retrieval      │
      │ Cost         │  │ Credential   │   │ Citation       │
      └───────┬──────┘  └───────┬──────┘   └────────┬───────┘
              │                 │                    │
              ▼                 ▼                    ▼
       LLM Providers      Enterprise Systems     PG/Vector/MinIO
                         Git/DB/OA/Cloud...

                     ┌────────────────────────┐
                     │ Shared Control Plane   │
                     │                        │
                     │ Workspace              │
                     │ Skill Registry         │
                     │ Artifact               │
                     │ Policy / DLP            │
                     │ Audit / Trace           │
                     │ Agent Distribution      │
                     └────────────────────────┘
```

## 3. 信任边界

### Zone A：员工终端

不可信环境。

可持有：

- Enterprise API Key
- Device Credential（仅 OS Keychain）

不得持有：

- LLM Provider Key
- Git Service Token
- DB Password
- Cloud Credential
- MCP Backend Secret

### Zone B：Enterprise Agent Gateway

唯一互联网/员工网可达 Agent 业务入口。

职责：

- Authentication
- Authorization Entry
- DLP Entry
- Policy Enforcement
- Protocol Compatibility
- Rate Limit
- Audit
- Token Exchange

### Zone C：Internal Capability Services

LLM/MCP/Knowledge 等服务不直接接受 `agk_xxx`。

仅接受：

- Internal User JWT
- Service Token / Workload Identity

### Zone D：Credential / Target Systems

真实凭据通过 Credential Broker/Vault 管理。

## 4. 身份与凭据架构

## 4.1 Credential Layers

```text
Layer 1: Enterprise API Key
        employee/client → agent.company.com

Layer 1b: CLI Device Credential
        eap CLI → agent.company.com

Layer 2: Internal User JWT
        gateway → capability service

Layer 3: Service Identity
        service → service

Layer 4: Resource Credential
        MCP gateway/broker → target system
```

## 4.2 API Key

格式建议：

```text
agk_live_<random>
agk_test_<random>
```

数据库：

```text
prefix
hash
user_id
metadata
```

不保存完整明文。

## 4.3 JWT

推荐：

- Ed25519 或 ES256
- 5~15 分钟
- `aud` 必填
- `jti` 必填
- JWKS 发布公钥

示例：

```text
Agent Gateway private signing key
        ↓
Internal JWT
        ↓
LLM/MCP/Knowledge locally verify via JWKS
```

避免内部每次请求同步回查 Auth 服务。

## 4.4 Service Token

每个服务独立身份和 Scope。

```text
service://mcp-gateway
  scopes:
    credential:request
    audit:write

service://knowledge
  scopes:
    object:read
    embedding:invoke
```

不允许全局超级 Service Token。

## 5. 请求链路

### 5.1 LLM

```text
Harness
  │ agk_xxx
  ▼
Agent Gateway
  │ authenticate
  │ principal
  │ workspace
  │ policy
  │ DLP
  │ trace
  ▼
short JWT aud=llm-gateway
  ▼
LLM Gateway
  │ model alias
  │ provider route
  │ provider credential
  ▼
LLM Provider
```

### 5.2 MCP

```text
Agent
  │ agk_xxx
  ▼
Agent Gateway
  │ policy
  ▼
JWT aud=mcp-gateway
  ▼
MCP Gateway
  │ server ACL
  │ tool ACL
  │ argument policy
  │ risk level
  ▼
Credential Broker
  │ resource scoped secret
  ▼
Target System
```

### 5.3 Knowledge

```text
Agent
  │ agk_xxx
  ▼
Gateway
  │ principal + workspace
  ▼
Knowledge Hub
  │ ACL
  │ retrieval
  │ citation
  ▼
Documents / Vector Index
```

## 6. 对外协议架构

Agent Gateway 应是“协议兼容网关”，不是 Harness 私有接口。

### OpenAI

```text
/v1/models
/v1/chat/completions
/v1/responses
/v1/embeddings
```

### Anthropic Compatibility

```text
/v1/messages
```

### MCP

```text
/mcp/{server}
```

支持 Streamable HTTP。

### Enterprise Extended API

```text
/enterprise/v1/me
/enterprise/v1/bootstrap
/enterprise/v1/workspaces
/enterprise/v1/workspaces/{id}/manifest
/enterprise/v1/knowledge/*
/enterprise/v1/skills/*
/enterprise/v1/agents/*
/enterprise/v1/auth/device/*
/enterprise/v1/artifacts/*
```

`/v1/*` 只承载推理兼容协议。CLI、Workspace Manifest、Skill Registry 和 Agent Distribution 不得进入该命名空间。

## 7. Agent Portal 与客户端分发

```text
Browser
  ↓
SSO
  ↓
Agent Portal
  ├── API Keys
  ├── Agent Center
  ├── Workspaces
  ├── Skill Marketplace / Review
  ├── Agent Releases
  ├── Usage
  └── Knowledge
```

Agent Center：

```text
AgentClient
AgentRelease
OS
Arch
Version
MinimumVersion
DownloadArtifact
ConfigTemplate
ProtocolCapabilities
RolloutPolicy
```

### 最低版本控制

客户端请求带：

```text
X-Agent-Client
X-Agent-Client-Version
```

策略可返回：

```text
ALLOW
WARN_UPGRADE
DENY_VERSION_TOO_OLD
```

## 8. Bootstrap 架构

支持标准客户端增强发现：

```text
GET /enterprise/v1/bootstrap
```

根据 Principal 返回：

```text
User
Available Workspaces
Model Aliases
MCP Endpoints
Skills
Client Policy
Feature Flags
Workspace Manifest Generation / ETag
Skill Package Digest / Signature Reference
Agent Release / Minimum Version Policy
```

这允许受支持客户端做到零/少配置。

## 8.1 EAP CLI、Skill Package 与本地同步

`eap` 是 V1 官方控制桥梁：Portal 面向人，Gateway 面向 Agent/CI，CLI 面向本地环境。CLI 使用 Go 构建为 Windows、macOS、Linux 单文件，不引入新的 Principal 或权限模型。

```text
.eap/workspace.yaml  人维护期望状态
        ↓
GET /enterprise/v1/bootstrap + Workspace manifest
        ↓
解析精确版本、摘要、签名和 Agent 目标
        ↓
eap.lock
        ↓
Agent Adapter 原子安装/升级/删除 EAP-managed 文件
```

Skill 包由 CLI 确定性打包，服务端独立扫描并在 review/release 后使用专用 package release key 签名。JWT 与 package signing 使用不同密钥用途。安装端验证摘要与 `/.well-known/eap-package-keys.json` 发布的公钥；校验失败或应用中断时保留原环境。

Agent Release 以 client/OS/Arch/version 建模，通过同域名短期 download ticket 获取。CLI 负责本地目录和配置适配，Server 仅管理标准包、release metadata 与 Policy。

## 9. LLM Gateway 内部架构

```text
Protocol Adapter
      ↓
Canonical LLM Request
      ↓
Model Alias Resolver
      ↓
Policy
      ↓
Router
      ↓
Provider Adapter
      ↓
Provider
```

建议内部定义稳定 Canonical Model，而不是业务代码直接绑定 OpenAI JSON。

示意：

```go
type Provider interface {
    Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    Responses(ctx context.Context, req *ResponseRequest) (*Response, error)
    Embeddings(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error)
}
```

Model Alias：

```text
coding-high
   ↓
policy/router
   ↓
provider/model-x
```

## 10. MCP Gateway 内部架构

```text
MCP Transport
    ↓
Server Resolver
    ↓
Tool Discovery / Cache
    ↓
Principal Authorization
    ↓
Parameter Policy
    ↓
DLP
    ↓
Risk Policy
    ↓
Credential Broker
    ↓
Backend MCP / Target
```

### Registry 核心对象

```text
MCPServer
MCPServerVersion
MCPTool
MCPPermission
MCPHealth
MCPCredentialRef
```

### 高风险扩展点

V1 预留：

```text
REQUIRE_APPROVAL
```

但审批引擎可 V1.5 实现。

## 11. Knowledge 架构

```text
Source
  ↓
Document Parser
  ↓
Normalizer
  ↓
Chunker
  ↓
Embedding
  ↓
Index
  ↓
Hybrid Retrieval
  ↓
Rerank
  ↓
Citation
```

存储建议：

```text
PostgreSQL  → metadata / ACL / versions
pgvector    → embeddings
MinIO       → original files / artifacts
Redis       → cache / jobs
```

第一阶段无需强制 Elasticsearch。

## 12. Knowledge Governance

```text
Conversation / Artifact / Task Result
              ↓
      Candidate Extractor
              ↓
      Sensitive Scan
              ↓
         Deduplicate
              ↓
      Conflict Detection
              ↓
      Provenance Binding
              ↓
      Confidence / Review
              ↓
      Canonical Knowledge
```

核心要求：每条正式知识具备 Provenance。

建议字段：

```text
source_type
source_id
workspace_id
creator_id
trace_id
version
confidence
classification
valid_from
valid_until
```

## 13. Policy / DLP 架构

统一 Policy Decision 接口：

```go
type DecisionInput struct {
    Principal   Principal
    Workspace   string
    Action      string
    Resource    string
    DataClass   string
    Model       string
    Provider    string
    MCPTool     string
    Client      string
}
```

返回：

```go
type Decision struct {
    Effect       string
    Redactions   []Redaction
    Route        string
    Reason       string
    PolicyID     string
}
```

建议不要在每个 Gateway 分别硬编码敏感规则。

## 14. Trace / Audit 架构

OpenTelemetry 作为统一追踪协议。

```text
trace_id
├── gateway.request
├── policy.evaluate
├── dlp.scan
├── knowledge.search
├── llm.generate
├── mcp.tool.call
└── artifact.create
```

Trace 与 Audit 区分：

### Trace

用于性能、调用链和 Agent 效果分析。

### Audit

用于不可抵赖的安全/治理事件记录。

关键 Audit 不依赖 Langfuse 作为唯一存储，应进入自有审计表/日志系统。

## 15. 控制面与数据面

### Control Plane

- Portal
- User / Workspace
- API Key
- Model Config
- MCP Registry
- Policy
- Skill
- Client Distribution

### Data Plane

- LLM traffic
- MCP traffic
- Knowledge retrieval
- DLP
- Token Exchange
- Trace

这样未来可以独立扩展 Gateway 实例而不复制控制面逻辑。

## 16. 推荐部署拓扑 V1

```text
             Reverse Proxy / LB
                    │
          ┌─────────┴─────────┐
          ▼                   ▼
     Agent Gateway       Agent Gateway
          │                   │
          └───────┬───────────┘
                  │
        ┌─────────┼─────────┐
        ▼         ▼         ▼
      LLM GW    MCP GW   Knowledge
        │         │         │
        └─────────┼─────────┘
                  ▼
       PostgreSQL / Redis / MinIO
                  │
                  ▼
             OTEL Collector
```

Portal Web 可静态部署或随 API 服务部署。

## 17. 服务拆分建议

V1 推荐：

```text
1. gateway
2. control-plane-api
3. llm-gateway
4. mcp-gateway
5. knowledge
6. worker
7. web
```

如果团队较小：

```text
gateway + control-plane-api
```

可以先合并。

不要按每个业务对象拆微服务。

## 18. 安全基线

### 外部

- TLS
- SSO
- API Key Hash
- Rate Limit
- Brute Force Detection
- Revocation

### Gateway

- Request Size Limit
- Streaming Limits
- Header Sanitization
- SSRF Guard
- DLP
- Policy

### JWT

- Short TTL
- Audience
- Issuer
- JWKS
- Rotation

### MCP

- Credential Isolation
- Tool ACL
- Argument Validation
- Network Egress Policy
- High-risk Operation Control

### Knowledge

- Workspace ACL
- Classification
- Source Provenance
- Delete / Retention

### Audit

- Append-oriented
- Operator Identity
- Policy Change History
- Key Event History

## 19. Availability / Performance

### Gateway

无状态优先，横向扩容。

### Streaming

LLM Streaming 不能经过会完整 Buffer 响应的中间层。

### MCP

考虑长连接和超时独立配置。

### Auth

API Key 可使用安全 Cache；权限变更和吊销需要快速失效机制。

### JWT

内部服务本地验签。

## 20. 演进路线

### V1

```text
Unified Endpoint
API Key
JWT
LLM
MCP
Knowledge
Policy/DLP
Trace/Audit
Harness Distribution
EAP CLI / Device Login
Skill Registry / Signed Packages / Sync
Agent Release / Local Adapters
```

### V1.5

```text
Credential Vault
Artifact
Knowledge Candidate
MCP Approval
Additional Client Adapters / Grey Release
```

### V2

```text
Enterprise Memory
Knowledge Auto Governance
Agent Evaluation
Skill Recommendation
Cross-project Knowledge Discovery
Advanced Client Governance
```

## 21. 架构红线

1. 不允许 Agent 客户端直连 Provider Key。
2. 不允许员工 API Key 直接用于内部服务身份。
3. 不允许 MCP Tool 把真实 Secret 返回给 Agent。
4. 不允许 Workspace ACL 只依赖客户端传参。
5. 不允许所有 Conversation 自动进入正式知识库。
6. 不允许审计完全依赖第三方 Observability 产品。
7. 不允许为了支持 Harness 发明不可复用的私有 LLM 协议。
8. 不允许第一版演进成复杂通用 Agent Builder。
