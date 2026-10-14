# Yuanpu Agent 桌面实际运行验收

日期：2026-10-11。平台：macOS arm64。目标：当前工作区的 Electron / React / Runtime，Pi npm 1.0.4。

使用独立 Yuanpu Home 和 Electron userData，复用已有模型连接配置进行真实模型调用。测试目录不包含原有对话和工作；模型凭据不写入验收材料。操作通过 agent-browser CDP 驱动桌面窗口，非网页预览。

## 验证进度

| 流程 | 结果 |
| --- | --- |
| Electron、Runtime、目录服务启动 | 通过，Pi 1.0.4 |
| Work 真实模型回复、历史与文件预览 | 通过；重启后恢复历史；Markdown 文件树、预览正常 |
| 内置 MCP 配置保存、真实工具调用与停用 | 通过；UI 保存、重启保留，tool_search → echo 实际返回 nonce；停用落盘 enabled:false |
| Assistant 真实对话 | 通过，约 6 秒返回“助理链路正常。” |
| 技能、设置、定时任务、知识库导航 | 通过页面打开与空态检查；未执行计划任务或企业微信发送 |
| 退出、重启与进程清理 | 通过，退出后 Electron、Runtime、Assistant Worker、MCP 子进程均消失 |

MCP 返回值为 `{"nonce":"YUANPU-MCP-20261011","pid":68855,"verifiedBy":"real-stdio-mcp"}`，来自保存的 Pi toolResult，与操作系统进程信息一致。不是只根据模型回复判定成功。截图见 [MCP 回复](screenshots/05-mcp-reply.png)、[文件预览](screenshots/06-file-preview.png)。本次未单独执行停用后的负向工具调用；停用行为另由自动测试覆盖。

Electron 不支持 agent-browser 录像所用的 `Target.createBrowserContext`，因此采用截图和实际运行数据作为证据，没有录像文件。隔离验收结束后删除复制的模型凭据，并移除临时 fetch 插桩；正常开发启动使用用户原有配置。

## 问题

### 1. 简单问候首次耗时 112 秒：事实确认，根因尚未完全确定

用户截图对应 `7d50ab6b-3691-4c6e-834f-b4937f8456b1`。运行从 22:02:15.493 到 22:04:07.176，111.683 秒。会话初始化和用户消息入库在前 34 毫秒完成；无工具调用，推理仅 16 tokens，最终正常结束。首次没有 HTTP 首包记录，无法事后严格区分服务端等待、网络等待或请求层内部耗时。

复测全部使用相同 LongCat-2.5-Preview 连接，保持推理开启。以下首包和模型耗时相对请求发起时刻；整体耗时相对 Runtime 接受任务时刻：

| 场景 | 本地准备 | 模型首包 | 模型首段文本 | 模型请求总耗时 | 运行总耗时 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 原会话问候，改动前 | 35 ms | 1.838 s | 3.167 s | 3.456 s | 3.492 s |
| 新会话问候，改动前 | 41 ms | 2.430 s | 5.253 s | 7.801 s | 7.843 s |
| 新会话再次问候，改动前 | 11 ms | 2.229 s | 4.050 s | 6.030 s | 6.041 s |
| 重启后问候，兼容适配与阶段记录已启用 | 30 ms | 3.038 s | 4.923 s | 5.700 s | 5.731 s |

另一次独立 Pi 请求首包等待约 13.4 秒；后台 Assistant 请求也出现响应头等待 26.18 秒、整体 39.96 秒的波动。复测没有发现本地初始化卡住、MCP 阻塞或重复请求。**模型服务或网络等待是当前更有证据支持的方向，但 112 秒异常未复现，不能认定已经根治，也不能把改动前后的短样本差异当作优化收益。**

对捕获的同一请求做顺序 A/B：原参数 6.479 秒、官方兼容参数且推理开启 6.994 秒、关闭推理 1.747 秒。样本不足以作统计比较；没有为了让测试变快而关闭用户推理或降低模型输出限制。

证据：[运行起止时间](run-times.json)、[临时请求计时](model-timings.jsonl)、[移除临时插桩后的正式阶段记录](work-timings-after.json)。这些文件不含凭据、提示词、回复正文或原始推理。

### 2. LongCat 兼容参数与阶段证据缺失：已处理

- 原请求发送 `max_completion_tokens`、`reasoning_effort`；LongCat 官方文档使用 `max_tokens`、`thinking.type`。增加按精确域名和 API 类型匹配的默认兼容参数，覆盖 Work 与 Assistant，保留用户显式 compat 配置优先级。依据：[LongCat Chat API](https://longcat.chat/platform/docs/api/chat.html)、[官方配置示例](https://longcat.chat/platform/docs/OpenClaw.html)。这项修正没有被证明是 112 秒等待的根因。
- Work 通过 Pi 公共扩展事件写入 `yuanpu_model_timing` 自定义会话记录，关联 host run ID / request ID，包含 request、headers、first_event、first_text、end。以后可从会话 JSONL 直接定位慢在哪一段。无 prompt、headers 或原始事件日志。
- 未调整超时或重试策略：现有证据不足以选择更短的全局超时，盲目降低会截断正常长推理。Pi SDK 默认请求超时为 300 秒；模型错误在界面仍使用既有脱敏提示。

## 自动验证

- `pnpm check`：通过，构建、TypeScript 和全套测试；runtime-kit 215、Runtime 138、Assistant 53、桌面 26、renderer 48 项测试通过。
- 新增 loopback 实际 Pi 请求测试：验证序列化后的字段、推理开关、阶段顺序、运行关联和敏感内容不进入计时记录。
- `pnpm build:native && pnpm smoke:native`：通过，macOS arm64 SEA、内置 MCP、Python 签名能力和 Runtime 暂存更新。
- `git diff --check`：通过。
- 其他操作系统、发布签名和外部企业微信链路不在本次桌面验收范围。

正常配置启动核对：`pnpm dev` 后可见 Yuanpu Agent 窗口，恢复原有 9 月 27 日工作历史；Runtime 与 Assistant Worker 正常运行。应用保留打开，供用户继续操作。
