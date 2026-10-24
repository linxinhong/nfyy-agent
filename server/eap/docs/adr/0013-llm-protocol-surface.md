# ADR-0013 客户端 LLM 协议面（TASK-007）

状态：已接受　日期：2026-09-01　关联任务：TASK-007（重开后真实交付）

## 背景与纠正

该卡曾被以重复 ID 误标 done（证据实为 Portal 部署卡）。注册表现实核对后重开，本 ADR 记录真实交付。

## 决策

### 1. 协议端点全集

`GET /v1/models`（控制数据别名 + Policy 过滤，被拒/需审批的模型不可见）、`POST /v1/chat/completions`（非流式 + `stream=true` 302 语义分流至 SSE 端点）、`POST /v1/chat/stream`（SSE：frozen chunk 形状、终止 [DONE]、mid-stream 错误帧）、`POST /v1/responses`（OpenAI Responses 非流式形状）、`POST /v1/embeddings`（provider /embeddings 代理）、`POST /v1/messages`（Anthropic Messages 形状翻译）。

### 2. 单一 fail-closed 管线

所有端点复用同一认证链（agk key → Workspace 成员 → 换票 aud=llm-gateway sign→verify）→ Policy（含 deny/require_approval 非执行效果）→ DLP（逐消息，deny 阻断零出网）→ 路由执行。协议只是形状翻译，执行内核唯一。

### 3. SSE 流与取消（绕开上游竞态）

流式走自研 OpenAICompatClient.ChatStream（bufio SSE 解析、ctx 贯穿）——**不经过 Bifrost 的流式路径**，因此 ADR-0004 记录的上游取消竞态不在本路径上；"生产流式接受前必须解决该竞态"的约束以绕开方式满足。客户端取消（request ctx）级联拆除上游连接；单一读取者契约（路由器不与调用方争抢 chunk 通道）。

### 4. 用量与审计

流式终止帧（usage）或取消时按已消费内容落 usage ledger 与三类治理审计记录（policy.decision/dlp.hit/llm.request），与非流式一致。

## 验证

`make verify-llm-protocols` = verify-knowledge-slice（含全链前级）+ `TestLLMProtocolsIntegration`：五协议契约形状、SSE 分段与 [DONE]、deny 类 DLP 在全部协议上零出网、客户端取消由桩观测断开、race 检测下无泄漏。
