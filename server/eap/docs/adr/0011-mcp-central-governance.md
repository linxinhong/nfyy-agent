# ADR-0011 MCP 中央 Policy、风险与审批语义（TASK-011）

状态：已接受　日期：2026-09-01　关联任务：TASK-011

## 决策

### 1. 一套词汇：MCP 消费中央 Policy

`mcpgov.PolicyEngine.DecideMCP(workspace, server, tool, action)` 与 LLM 平面共用 llmkernel 规范效果词汇（allow/deny/...），不存在第二套 MCP 授权词汇。引擎失败即拒绝（policy_engine_failure 审计）。发现与执行同谓词：被中央 policy 拒绝的工具在 tools/list 中**不可见**——发现永远不会比执行权限更宽。

### 2. 风险分级审批（R2+ 零上游接触）

`EvaluateApproval`：R1 直接执行；R2+ 必须持有一次性授权延续（approval token，绑定 principal+tool，Consume 原子销毁，重放即拒）；**未知风险等级 fail closed**（绝不当作 R1）。审批前对上游**零接触**——审批必需的调用在 token 消费成功前不产生任何出站请求。审批存储是有界记录（不是工作流引擎）；生产可替换为 PG 存储。

### 3. SSRF 策略：初始请求 + 每一跳重定向

`SSRFPolicy.Check` 拒绝解析到 loopback/私网/link-local 的目标（DNS 解析后校验，防 rebinding）；`SafeHTTPClient` 的 CheckRedirect 对**每个重定向跳**重新校验，超过 3 跳拒绝，跨源重定向剥离 Authorization 头。上游端点永远来自注册表（调用方传入 URL/凭据引用即违反 forbidden）。

### 4. 审计分类

`mcp.tool.executed` denied 带原因分类：`policy_denied:<rule>`、`approval_required`、`approval_impossible`、`ssrf_blocked`、`unknown_risk`、`policy_engine_failure`、`workspace_not_allowed`；allowed 记录 R1 直接执行与已批准的 R2 执行。全部走可靠路径（RequireWrite）。

## 验证

`make verify-mcp-governance` = verify-policy-dlp-audit（含全链前级）+ `TestMCPGovernanceIntegration`：R1 发现/执行、deny 工具发现不可见且执行被拒、R2 零接触 + 授权延续恰好一次执行 + token 重放零接触、SSRF link-local 与重定向跳阻断、分类审计 ≥4 类 denied + allowed 记录——真实 PG/Redis。
