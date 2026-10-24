# 企业 Agent 平台开源参考清单

> 核验日期：2026-08-31  
> Star 为核验时 GitHub 公开页面的近似值，会持续变化。  
> 参考原则：优先高 Star、社区成熟、与企业 Agent 基础设施直接相关；“参考”不等于建议直接 Fork。

## 1. 第一优先级参考

| 项目 | Star 约 | 许可证/模式 | 主要参考方向 | 建议 |
|---|---:|---|---|---|
| n8n | 197k | Sustainable Use / Fair-code | Credential、Integration、Tool、MCP、企业系统连接器 | **重点看架构与连接器，不作为核心二开底座** |
| Dify | 153k | Dify Open Source License（Apache 2.0 + 附加条件） | Workspace、模型、知识、Tool、团队协作产品模型 | **重点看产品与数据模型** |
| Open WebUI | 147k | 多许可证，当前包含 Open WebUI License | Workspace、Knowledge、Model、Tool、员工体验 | **重点看 UX，不建议直接作为公司平台底座** |
| RAGFlow | 86.8k | Apache-2.0 | Knowledge / RAG / Context Engine、文档处理、检索 | **知识引擎第一参考** |
| Mem0 | 64k 左右 | Apache-2.0 | Agent Memory、长期记忆、使用过程沉淀 | **V2 Memory/知识沉淀第一参考** |
| LiteLLM | 57.5k | Core MIT，存在 Enterprise 目录/能力 | LLM Gateway、Provider Adapter、Key、Cost、Routing | **LLM Gateway 第一参考** |
| Langfuse | 32.9k 左右 | Core MIT，ee 商业许可 | Trace、LLM Observability、Cost、Evaluation | **Trace 模型第一参考** |

## 2. 安全与权限参考

| 项目 | Star 约 | License | 主要参考方向 | 建议 |
|---|---:|---|---|---|
| Microsoft Presidio | 8.3k | MIT | PII 检测、Mask、Redact、匿名化 | DLP 检测增强服务 |
| OpenFGA | 5.2k | Apache-2.0 | Zanzibar 风格关系权限、Go 实现 | Workspace/Knowledge/MCP 细粒度权限参考 |

这些项目 Star 不如 Agent 平台项目高，但在各自垂直领域具有较高参考价值。

## 3. 各项目如何用于本项目

## 3.1 LiteLLM

GitHub：

https://github.com/BerriAI/litellm

当前定位：AI Gateway，支持大量 LLM Provider、OpenAI 兼容接口、Cost、Guardrail、Load Balance、Logging 等。

### 重点研究

- Provider Adapter
- Model Alias
- Provider Routing
- Retry / Fallback
- Virtual Key 产品思路
- Usage / Cost
- Guardrail Hook
- Streaming
- API Compatibility

### 不建议照搬

本项目员工身份和统一 API Key 应由 Enterprise Agent Gateway 掌握，不应让 LiteLLM 成为企业唯一身份源。

推荐关系：

```text
Enterprise Agent Gateway
          ↓
   自研 LLM Gateway
          ↓
  参考 LiteLLM 设计
```

如果早期时间紧，也可以把 LiteLLM 暂时作为内部 Provider Gateway，但不直接暴露给员工。

---

## 3.2 RAGFlow

GitHub：

https://github.com/infiniflow/ragflow

### 重点研究

- Document Parsing
- Chunking
- Retrieval
- Rerank
- Citation
- Context Engine
- Agentic Retrieval
- Knowledge 管理交互

### 本项目与 RAGFlow 的差异

RAGFlow 重点是知识/RAG 引擎。

本项目还需要：

```text
Knowledge Candidate
Provenance
Workspace ACL
Artifact → Knowledge
Knowledge Version
Conflict Detection
Enterprise Policy
```

因此更适合“参考/集成知识引擎”，而不是直接把 RAGFlow 当整个 Agent 平台。

---

## 3.3 Mem0

GitHub：

https://github.com/mem0ai/mem0

### 重点研究

- User / Session / Agent Memory
- Memory Extract
- Long-term Memory
- Memory Search
- Agent Integrations

### 建议

V1 不急于引入 Memory。

先解决：

```text
Document Knowledge
Artifact
Knowledge Candidate
Canonical Knowledge
```

V2 再研究把 Mem0 思路用于 Working Knowledge / Personal Memory。

---

## 3.4 Langfuse

GitHub：

https://github.com/langfuse/langfuse

### 重点研究

- Trace
- Span / Generation
- Token / Cost
- Session
- Evaluation
- OpenTelemetry Integration
- Prompt / Model observability

### 使用建议

可以直接集成作为 LLM/Agent Observability，但企业关键 Audit 必须保留自有存储。

```text
OpenTelemetry
     ↓
┌────┴────────┐
▼             ▼
Langfuse    Audit DB
观察分析      安全审计
```

---

## 3.5 Dify

GitHub：

https://github.com/langgenius/dify

### 重点研究

- Workspace
- Model Provider
- Knowledge
- Tool
- Plugin
- Team collaboration
- Admin UX

### 不建议

不建议 Fork Dify 改造成 Enterprise Agent Gateway。

原因：

1. 产品目标是 Agent 应用/Workflow 平台，不是外部 Agent 的统一接入基础设施。
2. License 有附加条件。
3. 容易把项目带向 Low-code Builder，而偏离“统一企业 Agent 基础设施”。

---

## 3.6 Open WebUI

GitHub：

https://github.com/open-webui/open-webui

### 重点研究

- Workspace UI
- Knowledge UX
- Model UX
- Tool UX
- 普通员工 AI 使用体验

### 建议

主要作为产品/交互参考。

不要把聊天 UI 作为本项目核心，因为员工主要客户端仍然是 Harness / Pi / WorkBuddy 等本地 Agent。

---

## 3.7 n8n

GitHub：

https://github.com/n8n-io/n8n

### 重点研究

- Credential Model
- Integration Registry
- Node/Tool Metadata
- OAuth Credential
- Secret 隔离
- Connector 生命周期
- MCP Client / Server 方向

### 对 MCP Gateway 特别有价值

本项目最值得借鉴 n8n 的不是 Workflow，而是：

```text
Tool Integration
Credential
Connection
OAuth
Configuration Schema
```

License 不适合作为随意复制/商业化二开的假设，正式复用前需单独做许可证评估。

---

## 3.8 Presidio

GitHub：

https://github.com/microsoft/presidio

### 重点研究

- Recognizer
- Analyzer
- Entity + Confidence
- Anonymizer
- Operator

### 建议架构

```text
Go DLP Core
   ├── Regex
   ├── Secret Scanner
   ├── Dictionary
   └── Presidio Service
```

不要完全依赖 Presidio 处理企业 Secret、代码 Token、DB URL 等场景。

---

## 3.9 OpenFGA

GitHub：

https://github.com/openfga/openfga

### 重点研究

例如：

```text
user:alice
member
workspace:secops

workspace:secops
can_use
mcp:gitlab

workspace:secops
can_read
knowledge:architecture
```

### 建议

V1 权限关系不复杂时可以先自研 RBAC + Scope。

当出现：

- 跨 Workspace
- Resource Share
- Group
- Nested Team
- 复杂 MCP Resource

再引入/迁移 OpenFGA。

## 4. 按系统模块的参考优先级

| 自研模块 | 第一参考 | 第二参考 | 备注 |
|---|---|---|---|
| Enterprise Agent Gateway | LiteLLM / Envoy Gateway 思想 | Dify API 组织 | 入口必须自研掌控 |
| LLM Gateway | LiteLLM | 其他 AI Gateway | 重点看 Adapter/Router |
| MCP Gateway | n8n Credential/Integration | Dify Tool/Plugin | 当前没有一个 50k+ Star 的完整企业 MCP Gateway 可直接照搬 |
| Knowledge Hub | RAGFlow | LlamaIndex | RAGFlow 优先 |
| Memory | Mem0 | 其他 Memory 项目 | 放到 V2 |
| Workspace / Portal | Dify | Open WebUI | 学产品模型，不 Fork |
| Trace | Langfuse | OpenTelemetry 原生 | Audit 自己做 |
| DLP | Presidio | 自研 Secret Scanner | 企业规则必须自研 |
| Authorization | OpenFGA | 自研 RBAC/Scope | V1 可先轻量 |
| Connector Credential | n8n | Vault/KMS 产品 | MCP 核心参考 |

## 5. 参考优先级（建议代码阅读顺序）

### Tier A：必须认真研究

1. LiteLLM
2. RAGFlow
3. Langfuse
4. n8n

### Tier B：重点看产品/模型

5. Dify
6. Open WebUI
7. Mem0

### Tier C：按模块使用

8. Microsoft Presidio
9. OpenFGA

## 6. 不建议的路线

### 不建议 1：Fork Dify 然后改成统一 Gateway

会被 Workflow / App Builder 数据模型绑架。

### 不建议 2：Fork Open WebUI 做员工门户

本项目 Portal 的核心是 Agent Distribution / API Key / Workspace / Policy，不是聊天 UI。

### 不建议 3：直接把 LiteLLM 暴露给全员

这样统一企业身份、MCP、Knowledge、Client Governance 仍然缺失。

### 不建议 4：为了高 Star 找一个项目“全包”

当前高 Star 项目几乎都只覆盖一部分：

```text
LLM
RAG
Workflow
UI
Memory
Observability
```

本项目的核心差异在于把这些能力通过统一身份和统一安全边界提供给外部 Agent。

## 7. 最终组合建议

```text
                 自研 Enterprise Agent Gateway
                            │
           ┌────────────────┼────────────────┐
           │                │                │
       LLM Gateway      MCP Gateway      Knowledge Hub
           │                │                │
      LiteLLM思想       n8n思想         RAGFlow思想
                                            │
                                          Mem0
                                         (V2)

                     OpenTelemetry
                          │
                       Langfuse

           Policy/DLP             Authorization
        自研 + Presidio        自研 → OpenFGA
```

## 8. 许可证结论

### 相对友好、适合借鉴/集成

- RAGFlow：Apache-2.0
- Mem0：Apache-2.0
- Presidio：MIT
- OpenFGA：Apache-2.0
- Langfuse Core：MIT（注意 ee 目录）
- LiteLLM Core：MIT（注意 enterprise 目录/商业能力边界）

### 需要特别谨慎

- Dify：基于 Apache 2.0 的自定义 License，存在附加条件
- Open WebUI：当前代码包含 Open WebUI License，多许可证历史
- n8n：Sustainable Use / Fair-code，不能简单等同 MIT/Apache

正式复制代码、发布衍生产品、对外商业化前，应对具体版本和目录做一次独立许可证审查。

## 9. 本次核验来源

- https://github.com/n8n-io/n8n
- https://github.com/langgenius/dify
- https://github.com/open-webui/open-webui
- https://github.com/infiniflow/ragflow
- https://github.com/mem0ai/mem0
- https://github.com/BerriAI/litellm
- https://github.com/langfuse/langfuse
- https://github.com/microsoft/presidio
- https://github.com/openfga/openfga


## 5. 协议一致性参考(TASK-037 新增)

> 背景:/v1 与 /mcp 协议面为手写最小子集,多个缺口(工具透传、参数 schema、
> SSE 字节切割、错误信封)均由真实客户端撞出后修复。协议演进需要对照基线。

| 参照 | 用途 | 用法 |
|---|---|---|
| **maximhq/bifrost(core/schemas,v1.8.4,已 pinned)** | **第一参考:仓库内依赖,OpenAI 兼容面字段几乎建模齐全**(ChatTool/ChatToolChoice/tool_calls 流式结构/usage/finish_reason);其 providers/ 目录是各 provider 字段差异的活文档 | protocol-openai 的字段覆盖度对齐 `schemas/chatcompletions.go`;新增字段先查它怎么建模 |
| openai/openai-openai | OpenAI 官方 OpenAPI 规范 | /v1/* 契约覆盖度对照的事实来源 |
| modelcontextprotocol/spec | MCP 官方规范 | Streamable HTTP 传输、会话(Mcp-Session-Id)、SSE 语义基线 |
| modelcontextprotocol/inspector | 官方检查器 | 协议变更后的机械化验收:`npx @modelcontextprotocol/inspector --cli <url> --transport http ...` |
| BerriAI/litellm | 最严苛 OpenAI 兼容客户端之一 | 以 litellm 指向网关冒烟:参数透传、流式、错误信封、usage |
| goodrain/new-api / songquanpeng/one-api | 国内 OpenAI 兼容聚合器 | 客户端生态真实字段使用面参考 |

流程约束:任何 /v1/* 或 /mcp/* 契约变更,完成前必须通过 Inspector 检查 +
一个真实客户端(Hermes/Codex/litellm 之一)journey;字段取舍先对照 Bifrost
schemas,再对照官方 spec,避免再靠真实客户端撞出缺口。
