# 内置 Web、目标与动态工作流

这三个模块与子 Agent 一起编译进 Runtime，不需要安装插件或修改 `~/.yuanpu/agent/plugins`。
来源版本、commit 和 MIT 许可证位于 `packages/yuanpu-runtime/src/builtin/NOTICE.md`。
实现是 Yuanpu 原生适配，不是上游扩展的完整副本，参数也并非完全兼容。

## 1. Web Access

保留现有两个外部能力入口：先 `search_capabilities`，再用返回的精确 ID 调用 `execute_capability`。
新增内置能力来源 `builtin.web-access`，升级 Python 能力后重建能力注册表时也会保留。

| 能力 | 参数 | 用途 |
| --- | --- | --- |
| web_search | query、provider?、numResults? | 搜索并返回来源、内容和 responseId |
| fetch_content | url | 获取 HTML 正文、文本、JSON 或 XML |
| get_web_content | responseId、startLine?、lineCount? | 分页读取当前会话缓存，nextLine 指向下一页 |

支持 Exa、Brave、博查。自动选择顺序：已配置的博查 → 已配置的 Brave → Exa。
Exa 无 Key 时使用公开 MCP 搜索服务；有 Key 时使用官方搜索 API。显式指定 provider 时不会偷偷切换服务。
服务错误、限流和格式错误会返回失败。未设置自动故障转移，不保证公共免费服务的可用性。

密钥读取环境变量 `BOCHA_API_KEY`、`BRAVE_API_KEY`、`EXA_API_KEY`，或统一的
`~/.yuanpu/app/auth.json` 中 `web:bocha` / `web:brave` / `web:exa` 项：

```json
{"web:bocha":{"type":"api_key","key":"YOUR_KEY"}}
```

不会复用模型 API Key，不读取浏览器 Cookie，也不会上传本地文件。
网页请求只允许公网 HTTP(S)，DNS 结果验证后绑定连接；每个重定向重新验证，
拒绝私网、回环、保留地址、带账号密码的 URL、带凭据的跨站重定向和 POST 重定向。
支持 HTTP(S)_PROXY / ALL_PROXY 和 NO_PROXY；代理 CONNECT 也使用已验证的目标 IP，保留原始 Host 和 TLS servername。
单次响应上限 8 MiB，缓存正文上限 50 万字符；首次返回 16000 字符，后续按行分页。
内存缓存最多 64 项，有效期 30 分钟，按用户、工作区和会话隔离。重启后失效。
搜索结果、网页正文和缓存分页均标记为不可信数据；传给 Pi 时另加引用边界，系统提示词要求忽略其中的角色声明、工具指令和授权声明。提取时会删除脚本、表单、部分控制字符和双向文本控制符。这些处理不能保证模型绝不受自然语言诱导。

同一会话首次读取外部搜索或网页结果后，后续 `web_search` 和 `fetch_content` 会提升为 R3，必须通过宿主对精确参数进行一次性审批；`get_web_content` 只读当前会话的已缓存内容，无需再次联网或审批。这样可阻止网页直接诱导模型向新地址发送搜索词或请求。若宿主没有审批能力，后续联网会被拒绝。审批状态目前在 Runtime 内存中，重启会清空；本地 `bash`、文件写入及其他来源的能力还没有统一的“外部内容污染”强制隔离，因此仍应把网页信息作为证据核对，不能把这项措施视为完全防护。

本轮未移植：其余搜索商、浏览器 Cookie 登录、视频理解、PDF 读取、GitHub 自动克隆、curator 界面。

## 2. Goal

内置工具 `goal` 提供：

- create：创建草稿；objective 必填，tasks / criteria 可选，ordered 开启严格顺序。
- activate / resume：用户确认计划或明确要求直接执行后开始；不会从普通请求自动创建目标。
- task：记录任务状态、完成证据或跳过原因；ordered 模式禁止越过未完成任务。
- complete：提交证据。默认调用独立 reviewer 检查，失败或非结构化审查结果都不会标记完成。
- pause / block / revise / focus / list / status：暂停、阻塞、修订、切换及查询。

一个会话可保存多个目标，同时只有一个处于活动状态。激活后会在当前 Runtime 运行中自动继续，
直到完成、暂停、阻塞、审批等待、模型错误或达到续跑限额。默认最多自动续跑 10 次，
`maxContinuations` 可设 0–100；达到限额后暂停，需要明确 resume。
停止按钮也会暂停目标。重启不会自动恢复执行，目标进度保留，需明确 resume。

目标审查使用当前模型的独立只读子会话；不是同一段对话的自我声明。
这不替代项目测试。任务记录和证据仍需由执行者如实提供。

本轮未移植：TUI 仪表盘、slash 指令、无限自动续跑、多层任务树、上游的独立审查模型设置页。

## 3. Dynamic Workflows

内置 `workflow` 执行 JavaScript 编排，`workflow_control` 管理运行。

```json
{
  "background": false,
  "args": ["登录", "权限"],
  "script": "await phase('审查'); const results = await parallel(args.map(area => () => agent('审查 ' + area, {agentType:'reviewer'}))); return results;"
}
```

支持的脚本全局：

| API | 用途 |
| --- | --- |
| agent(prompt, options?) | 独立子 Agent，返回文本 |
| parallel(thunks) | 并行分派，按输入顺序返回 |
| pipeline(items, ...stages) | 分阶段处理一组输入 |
| await phase(title) / await log(text) | 记录阶段与进度 |
| await checkpoint(prompt) | 暂停，等待宿主批准后恢复 |
| verify(item, {reviewers?, threshold?}) | 多个独立 reviewer 检查声明，默认一致通过 |
| args | 启动参数 |

`agent` 选项：`agentType`（默认 worker）、`provider` 与 `model`（须同时给出，模型须已配置）、
`isolation: "worktree"`。默认使用当前模型；工具权限不超出父会话启动工作流时的工具集合，
能力调用绑定启动时的上下文。没有递归工作流或递归子 Agent。

worktree 从当前 HEAD 创建，不包含未提交修改。工作目录和修改会保留；不会自动合并或删除。
不启用 worktree 时共享项目文件，并行写入必须划分文件范围。

内置模式：`deep-research`、`code-review`、`adversarial-review`、`multi-perspective`。
例如 `workflow({name:"deep-research",args:"研究问题"})`；研究模式通过内置网页能力查找资料，
然后由 evidence-auditor 汇总核验。

默认后台运行，返回 runId；通过 `workflow_control` 的 list/status/pause/resume/stop 查询或控制。
后台完成不会自动发消息，需查询状态。

检查点必须经过已有的宿主审批系统：`confirm` 请求审批；用户批准后带 approvalRequestId 再次 confirm，
最后 resume。`builtin.workflow-checkpoints` 使用 R3 风险等级、固定版本和一次性授权，模型自行填写 ID 无效。
没有审批宿主时不能跳过检查点。

完成步骤即时保存到 journal。恢复同一脚本时，根据调用序号、提示词、选项、阶段匹配已完成结果，
不重复调用这些步骤。重启后运行显示 interrupted，可显式恢复。尚未完成的调用可能已产生文件或外部副作用，
恢复前应检查；这不是外部操作的 exactly-once 保证。等待外部能力审批的运行需处理审批后重新规划启动。

脚本在独立 Worker 中运行，提供有限的编排全局，不开放 imports 或 Node API。
Worker 有内存和时间限制，但这不是运行敌对代码的安全沙箱；只运行已获用户授权的编排。
脚本必须 await 所有启动的操作；提前返回会取消未等待的子任务。

限额：默认 24 次 Agent 调用，最多 64；并行实际最多 3 个，与父会话子 Agent 共用累计 64 个配额。
默认最长 10 分钟，最多 30 分钟。状态显示 SDK 报告的 tokens/cost，供应商未报告时为 0，非估算成本。
缓存恢复的统计是逻辑工作流已记录用量，不代表恢复这一轮的新开销。

本轮未移植：上游 TUI、small/medium/big 路由配置、可重用工作流命令库、编辑脚本恢复、
无限任务量、token 预算、自动重试、judgePanel/loopUntilDry、完成主动推送。

## 4. Desktop Canvas

桌面视图底部的输入框只对应一个固定会话：命名空间 `desktop`、会话 `canvas`，工作目录为默认工作目录。
`desktop_show`、`desktop_put_html`、`desktop_dock`、`desktop_remove` 只注册在这个会话的 Pi session 上，
工作会话、助理和定时任务都看不到。判断依据是运行的 entryPoint、namespace 与 conversationId，不接受客户端声明。

工具不直接操作界面，只把动作记入本次运行的 `output.desktopActions`，由渲染层在运行结束后执行。
每次运行最多 16 个动作，单个 HTML 挂件不超过 64 KiB。HTML 在 `sandbox=""` 的 iframe 中渲染，
不执行脚本、不能访问本机数据。挂件位置归用户：Agent 只能决定显示哪些挂件、内容和尺寸档位，
不能删除内置挂件，也不能把内置挂件改成 HTML。

每条桌面消息附带当前挂件清单（id、类型、标题、尺寸、是否收进 Dock），作为数据而非指令传给 Agent，
不包含挂件内容。清单由 Runtime 按固定字段重新构造，最多 64 项。

## 保存与验证

目标与工作流位于 `~/.yuanpu/workflows/agent-tools/<workspace+session hash>/`：
`goals.json` 和 `runs/<runId>/run.json`。原子替换写入，目录 0700，状态文件 0600。
子 Agent 原始会话仍位于 Agent 的 subagent-runs。应用设置和认证文件不被这些运行状态污染。

测试覆盖网页私网拦截、正文提取、缓存隔离、搜索协议与错误；目标证据、顺序、审查、续跑限额与
真实 SDK 自动续跑；工作流并行、流水线、超时、取消、检查点、跨实例 journal 恢复、审批和配额。
