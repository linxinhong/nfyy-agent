# TASK-053 验证记录：Bifrost 流取消 data race 复现与 pin 决策

Date: 2026-09-04 · Task: TASK-053 · ADR: ADR-0028 决策 3 · 结论：**keep-out**

## Harness

复现命令（ADR-0004 诊断目标，与 `make diag-upstream-race` 同源）：

```sh
go test -race -tags upstreamrace ./packages/llmkernel/ \
  -run TestStreamChatCancellation -count=3 -v
```

- 测试纯本地：provider 是 `httptest` stub（OpenAI 兼容 SSE），不需要网络。
- 取消时序：首块到达后立即 `cancel()`，命中流中途取消路径。
- `-race` 下只要出现一次 `WARNING: DATA RACE` 即判 race 复现。

对比对象：

1. 基线 pin：`github.com/maximhq/bifrost/core v1.8.4`（经 Go proxy 确认为当前最新
   tag；`go list -m -versions` 最新即 v1.8.4）。
2. 本地 dev 源码：`/Users/linxinhong/projects/bifrost/core`，
   commit `e1045c07eb81bab18df2243f3f421f48450848ff`（main+dev 合并点，无更新 tag）。
   通过 go.mod 临时 `replace`（已标注注释、验证后移除）接入；本仓库工具链
   go1.27.0 与 dev 的 `go 1.26.5` 兼容，`go mod tidy` 与编译一次通过，
   **无 API 断裂**。

## 结果

### 基线 v1.8.4（3 次）

- `WARNING: DATA RACE` 出现 3 次（3 次运行中共），其中 1 次运行判 FAIL。
- 竞态签名：`fasthttp.releaseRequestStream`（写）↔ `fasthttp.requestStream.Read`
  ← `providers/utils.SetupStreamCancellation` 的取消 goroutine 并发于
  `HandleOpenAIChatCompletionStreaming` 的 SSE 读循环。与 ADR-0004 记录一致。

### dev 源码 e1045c0（3 次）

- `WARNING: DATA RACE` 出现 3 次（3 次运行中共），其中 1 次运行判 FAIL。
- 竞态签名与基线**完全相同**：仍是 `SetupStreamCancellation`（utils.go:2653 的
  关闭回调）与 `idleTimeoutReader`/SSE 读循环对 fasthttp requestStream 的并发
  释放/读取。dev 相比 v1.8.4 新增的 idle-timeout reader、non-SSE 预检、
  cancellation 组合均未消除该竞态。

复现是概率性的（每次 3 连跑中约 1 次触发），与上游取消/读 goroutine 的调度
时序有关；两种版本下触发率相当，dev 无改善迹象。

## harness 修正（本卡提交）

`packages/llmkernel/kernel_test.go` 的 provider stub 有两处脆弱点在 dev 对比中
暴露并修复（不改变被测语义）：

1. stub 以 `strings.Contains(body, "\"stream\":true")` 判断流式请求；dev bifrost
   会 pretty-print 请求体（`"stream": true` 带空格），匹配失败导致 stub 返回
   JSON，dev 端被其新的 non-SSE 预检正确拒绝——这是 harness 假阴性来源，
   不是上游断裂。改为 JSON 解析 `stream` 字段。
2. stub 在 100ms 后才发首块；为避免慢 provider 被误判为非 SSE，打开流时先发一个
   SSE 注释行（`: stream-open`）。

## 判定：keep-out

依据（ADR-0028 决策 3 条件 (a)）：

- 最新 tag v1.8.4 race 复现（已知）。
- 当前 dev 源码（比最新 tag 更新的全部上游改动）race 同样复现，签名不变。
- 条件 (a) 的"pin 升级越过该 race"不成立；生产流式路径继续禁用 Bifrost，
  保持 schemas-as-baseline + legacy-path-only。

### 复查触发条件

满足任一即重跑本 harness（`make diag-upstream-race`，`-count=3` 下 0 次
`WARNING: DATA RACE` 才算通过）：

1. 上游发布 > v1.8.4 的新 tag；
2. 上游合并触碰
   `providers/utils.SetupStreamCancellation` / `ReleaseStreamingResponse` /
   `fasthttp requestStream` 释放语义的 commit（以 upstream CHANGELOG/PR 为准）；
3. 任何引入 routed-path Bifrost 适配的新任务卡开工前。

### 若未来 adopt

按 ADR-0028 决策 3：harness 连续通过后，仍需 (b) 插件 fail-closed 安全评审与
(c) ADR 修正案记录 swap 路径；本文件仅为条件 (a) 证据。

## 附：环境记录

- 工具链 go1.27.0 darwin/arm64；dev core go.mod `go 1.26.5`，无版本冲突。
- 验证用 replace 临时写入过 go.mod（带注释），验证完成后已移除；go.mod/go.sum
  最终与 main 一致（无 diff）。
- 诊断过程中曾将 dev core 复制到 /tmp 加打印确认 non-SSE 假阴性根因；
  bifrost 仓库本体未做任何修改。
