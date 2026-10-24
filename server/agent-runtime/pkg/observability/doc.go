// Package observability 规范（项目统一结构化日志约定）
//
// 本包是全项目唯一允许的日志出口。禁止直接调用 slog.Info / slog.Error
// 等裸 API——裸调用绕过保留字段黑名单，会把内容/密钥写进日志。
// 一律通过 observability.Logger() 或 observability.Event 记录。
//
// # event 命名（点分）
//
// Event 的第一个参数是点分命名的事件名，例如：
//
//	run.finalize.retry        run 终态 CAS 重试
//	requirement.resolved      审批决裁
//	workflow.node.completed   workflow 节点完成
//	compaction.triggered      context 压缩触发
//
// 命名规则：<域>.<动作>.<细分>，一律小写，禁止空格。event 既是日志
// message 也是 "event" 字段，可按事件过滤。
//
// # 分级语义
//
//	error  → 需要处理：run 失败、审批决裁失败、CAS 冲突耗尽
//	warn   → 尽力继续：CAS 重试、预算超限降级、未知工具
//	info   → 生命周期：run 开始/结束、审批创建/决裁、节点完成
//	debug  → 面包屑：详细 tracing，默认不开启
//
// # 封闭字段词表
//
// 所有 agent-run 遥测必须使用本包导出的字段常量
// （FieldRunID/FieldSessionID/FieldAgentID/FieldToolName/
// FieldModelRound/FieldRequirementID/FieldDurationMs/FieldOutcome/
// FieldError），保证下游消费者拿到稳定 key。自由 key 仅允许描述性
// 细分（如请求计数），禁止携带内容本身。
//
// # 保留字段黑名单
//
// prompt / secret / token / headers / body / authorization / api_key
// （大小写不敏感）在写入时由 redacting handler 替换为 [redacted]。
// 黑名单是机制级保障：即使调用点误传，也到不了输出。嵌套 group 中的
// 黑名单 key 同样生效。
//
// # error 归一化
//
// 错误一律经 observability.Err(err) 转为 "error" group（message + 可选
// stack），nil 时不产生字段。禁止直接 slog.String("error", err.Error())。
package observability
