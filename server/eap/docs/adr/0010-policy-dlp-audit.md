# ADR-0010 版本化 Policy/DLP 效果与可靠治理审计（TASK-010）

状态：已接受　日期：2026-09-01　关联任务：TASK-010

## 决策

### 1. 五种规范效果，规则表驱动

`policy_rules`（workspace/model 通配 + 优先级 + effect）表达五种规范效果：`allow`、`allow_with_redaction`、`local_model_only`、`require_approval`、`deny`。最高优先级（数值最小）且最具体的规则胜出；决策携带 `Effect + RuleID`，可对历史版本复算。`require_approval` 返回稳定非执行决策（V1 无审批引擎，效果是"不执行"而非放行）。

### 2. DLP 规则表 + 自定义词典

`dlp_rules`（deny/redact 双动作 + 替换文本）与 `dlp_dictionary_terms` 组成生产扫描器（`dlp.TableScanner`，60s 规则缓存）。词典项是规则模式的**并集扩展**（不替换原正则）。`allow_with_redaction` 效果下 deny 类匹配降级为脱敏；纯 allow 下 deny 类匹配直接阻断。

### 3. 脱敏必须改变释放的载荷

逐消息扫描、逐消息原地替换：provider 只看到脱敏后的文本；provider 响应在**发布前**再扫描（回显的密钥同样被脱敏或阻断）。"记录了脱敏但没有应用"在本架构中不可表达——脱敏发生在载荷构造处，不发生在审计处。

### 4. LOCAL_MODEL_ONLY 用 provider 标志过滤

providers 表带 `local_only` 标志；该效果把路由计划过滤为仅 local 路由，无 local 路由则拒绝。不引入新的模型命名。

### 5. 三类可靠审计，trace 关联，无原文

每次路由决策经可靠路径（RequireWrite）写 `policy.decision`（effect/rule_id/outcome）、`dlp.hit`（请求/响应脱敏计数 + 规则名 + outcome）、`llm.request`（effect/尝试数/错误码）。Detail 只含计数、规则名、效果码——**原始匹配内容与凭据永不入库**（集成断言 audit_events 无 `sk-`/手机号）。审计写失败 → 应答被扣留（fail closed），与 best-effort 日志严格分离。

### 6. 边界与升级

MCP 风险/审批、Knowledge、策略管理 UI 均在卡外。审批引擎落地时 `require_approval` 只需替换 kernel 的非执行分支为审批票据校验，公共契约不变。

## 验证

`make verify-policy-dlp-audit` = verify-production-oidc（含 provider-governance/identity/foundation）+ `TestPolicyDLPAuditIntegration`：redact（provider 收脱敏文本、响应回显被再扫描、dlp.hit 记 request_redactions）、deny/approval 零出网且规则锚定、local_model_only 只选 local provider、审计三类 ≥ 预期且无原始密钥泄漏——全部对真实 PG/Redis 断言。
