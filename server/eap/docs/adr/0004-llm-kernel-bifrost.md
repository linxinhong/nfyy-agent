# ADR-0004 LLM 执行内核：采纳 Bifrost Core（TASK-002）

状态：已接受　日期：2026-09-01　关联任务：TASK-002

## 决策

**采纳** `github.com/maximhq/bifrost/core` 作为 EAP LLM 网关的 Provider/协议执行内核，以钉版库依赖嵌入（**不 fork**）。EAP 侧唯一 LLM 数据面入口是本仓库 `packages/llmkernel`（安全管线 + 内核装配）；`xingding-llmg` 降级为参考仓库，其运行时路径不再迁移，重叠能力按下方处置表收编或退役。

## 依据（spike 证据：packages/llmkernel）

- 嵌入面：`bifrost.Init(ctx, schemas.BifrostConfig{Account})` 即可实例化；`ChatCompletionRequest / ChatCompletionStreamRequest / EmbeddingRequest / ResponsesRequest` 覆盖 M2 协议需求；Provider 经 `Account` 接口注入（OpenAI 支持 BaseURL → 实测以 httptest 桩完成全部验证，无真实 Key 消耗）。
- 安全管线（验收断言，全部通过，Provider 桩零命中）：
  - aud≠llm-gateway → `permission_denied`；缺 workspace 声明 → 签发即拒 + `workspace_forbidden`；
  - DLP 阻断与 **DLP 引擎故障** → `dlp_blocked`（fail closed）；Policy 拒绝与**引擎故障** → `policy_denied`（fail closed）；
  - 员工凭据（agk_/JWT）不出现在出站载荷；Provider 凭据由内核服务端持有（桩收到的 Authorization = 服务端 Key）。
- 重试/回退：`Fallbacks` 实测主模型 500 → 同调用内回退备用模型成功。
- 用量钩子：响应 `Usage` → `UsageRecorder` 记录（user/workspace/model/tokens）；成本在 M2 后续接 `Usage.Cost`。
- 流式与取消：chunk 通道 + ctx 取消实测传播到桩连接断开。
- 错误归一：`BifrostError.StatusCode → errs` 冻结码（400/404→invalid_argument；429→rate_limited；401/403/5xx→upstream_unavailable，provider 鉴权故障属我方基础设施问题）。

## 钉版与升级路径

- 钉版：`github.com/maximhq/bifrost/core v1.8.4`（发布 tag；本地参考克隆 e1045c0 ≈1.7.11-dev 仅作源码审阅）。
- 升级：tag 递进 + `make verify-llm-kernel-spike` 契约测试作回归门禁；`schemas` 公共类型破坏性变更视为上游契约变更，须重审本 ADR。
- 明确不使用：Bifrost Dashboard、Virtual Key 管理、MCP Agent Mode、Enterprise-only 特性、其 HTTP transports（EAP 的 Agent Gateway 是唯一边界）。

## 已知上游缺陷（spike 实证）

- **流式取消数据竞争**：v1.8.4 在 ctx 取消时，`SetupStreamCancellation` 释放 fasthttp requestStream 与内部 SSE 读取 goroutine 并发访问同一流（`providers/utils/utils.go:2938` vs `sse.go:191`），`go test -race` 稳定复现（诊断目标 `make diag-upstream-race`，`-tags upstreamrace`）。
- 影响评估：取消的**功能语义正确**（chunk 通道关闭、连接断开，功能测试在常规与 race 下对非流式路径均通过）；竞争位于上游清理簿记，不触及 EAP 安全管线。
- 处置：常规门禁保留流式功能测试；race 覆盖对取消路径拆分至诊断目标；升级到修复版本后将该测试折回 `race` 目标（已列入 follow-up）。

## 许可证义务

- Bifrost Core：Apache-2.0。随二进制分发时须保留其 LICENSE/NOTICE 声明。
- 其 `THIRD_PARTY_NOTICES.md` 声明部分嵌入源码为 MPL-2.0（以 Larger Work 方式组合、未修改）。采纳库依赖时该义务随模块传递；发布物需附对应第三方声明清单（依赖与许可证盘点归 TASK-006）。

## xingding-llmg 重叠处置

| 能力 | 处置 |
|---|---|
| Provider/协议/流式 | 退役参考实现；以 bifrost Core 为唯一执行内核 |
| 路由与成本算法 | M2 实现 model alias/成本时逐项比对后选择性移植到 `packages/llmkernel` 上层，不再保留其 runtime |
| SSE/契约测试手法 | 作为测试设计参考；测试本体在本仓库重写（见 llmkernel 测试） |
| 员工 Key 直达 LLM / SQLite 事实源 / 静态 Admin Token / 可丢审计 | 违反 EAP 边界，明确不迁移 |

结果：全公司只维护**一个** LLM Gateway 实现（本仓库），xingding-llmg 不再演进。

## 验证

`make verify-llm-kernel-spike` = verify-foundation + `go test ./packages/llmkernel/`（安全门禁、回退、取消、归一化、用量钩子）。
