# ADR-0008 Provider 拓扑、逻辑模型路由与用量成本治理（TASK-008）

状态：已接受　日期：2026-09-01　关联任务：TASK-008

## 决策

### 1. 控制数据是唯一生产事实源（禁环境变量拓扑）

Provider 端点、凭据引用、逻辑模型别名、路由（priority/weight/enabled）与定价全部落 PostgreSQL 版本化表（migration 000004：providers / model_aliases / model_routes / model_pricing，带 version 列）。环境变量仅是开发态装配回退；任务卡 forbidden「环境变量作为 Provider 拓扑生产事实源」在此闭合。

### 2. 路由与回退确定性

路由顺序由 SQL `ORDER BY priority, provider, model` 固化（governance.SortRoutes 供测试镜像同一排序）；llmkernel.Router 按序尝试：第一个成功路由胜出，**provider 侧失败与凭据解析失败确定性地推进下一路由**；EAP 自身的授权形态拒绝（policy/DLP/audience）不触发 failover——门禁只跑一次、在路由之前，语义与单 Provider 内核完全一致。

### 3. 公共名是逻辑别名

`/v1/chat/completions` 的 `model` 字段是逻辑别名；provider 原生模型名（路由表 `model` 列）与凭据引用只存在于服务端，OpenAICompatClient 仅在出站载荷中使用。响应/审计不回传任何凭据材料（集成测试断言）。

### 4. 用量与成本：持久、幂等、可审计

- 幂等键 `(trace_id, request_id, attempt)`，重试/流终止重放不重复计费；
- 成本在 big.Rat 精确空间计算（micros = price_per_mt × tokens，半值舍入），定价版本随行快照，可对历史控制数据复算；
- **未知定价显式化**：记录 `cost_known=false` 并打点告警，绝不伪造零成本成功；
- 行关联 user / workspace / alias / provider / provider_model / trace_id，支撑 Workspace 级成本核算。

### 5. 凭据解析边界

凭据引用（`env:VAR`，Vault/KMS 前缀预留）在调用时服务端解析；解析失败是可观测错误并 fail over 到下一路由，全部失败则请求失败（500 + 明确消息），绝不降级为匿名出网。MCP 与 LLM 数据面共享同一 Credential 解析接缝。

## xingding-llmg 参照

按 ADR-0004 处置表，仅比对其路由/定价算法后选择性吸收（确定性 failover 序、精确成本思路），无任何 runtime 代码迁移，未产生第二个 Provider 执行内核。

## 验证

`make verify-provider-governance` = verify-mcp-slice（含 foundation/identity）+ `TestProviderGovernanceIntegration`：双独立 stub 上「主凭据失败 → 确定性回退备份 → 900 micros 精确账务 → 幂等重放零变化 → 全凭据失败安全失败零出网 → 缺价格 cost_known=false → 未知别名 404 零出网 → 响应无凭据」全链断言（真实 PG/Redis）。单测覆盖成本数学（含小数价格/大数防负）与排序确定性。
