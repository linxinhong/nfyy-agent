# Python 能力包与统一能力入口

状态：TASK-001 至 TASK-007 已实现并完成本机阶段验证；跨平台发布验收仍由 TASK-008 跟踪。2026-09-21 根据架构评审建立；任务状态仅见 [注册表](../.tasks/tasks.yaml)。

## 目标与首版边界

用户仍只看到“技能”：搜索、安装、配置、启用、调用、更新。内部区分指令技能、可信 Pi 扩展和进程外能力包；不同执行模型不能因统一名称而混用加载器。

首版交付一个随应用分发的 Python 示例能力包，并支持从自建目录服务安装其更新。目标机不需要安装 Python，不在安装/启动时运行 pip 或编译依赖。先验证无副作用 echo，再用只写隔离测试目录的能力验证审批。

不做共享 CPython/venv 管理、任意 Python 源码安装、完整 PyPI 镜像、多层 MCP 转发、操作系统沙箱、插件商家后台、所有插件强制同版本升级。冻结后的 Python 程序不是任意插件的通用解释器；进程隔离也不是权限沙箱。

## 当前事实

以下是工作区已存在的代码，不代表所有代码已提交：

| 入口 | 现状与缺口 |
| --- | --- |
| `packages/yuanpu-runtime/src/capabilities/index.ts` | 有 CapabilitySource 与两个元工具；并行发现使用 Promise.all，单源失败会连带失败；审批只判断 token 非空 |
| `packages/yuanpu-runtime/src/capabilities/contracts.ts` | Schema 与结果表达较窄；没有完整 MCP 内容块、取消和宿主授权契约 |
| `packages/yuanpu-runtime/src/pi/index.ts` | Pi 保留 read/write/edit/bash，并加载可信扩展；工具结果转为 JSON 文本 |
| `apps/runtime/src/index.ts` | 已支持通过显式环境配置装配受管 Python MCP source；生产制品路径由分发任务接管 |
| `packages/yuanpu-runtime/src/packages/index.ts` | npm/Git 安装与 Pi settings 同步；不能把 Python 包目录直接加入 Pi extensions |
| `server/backend/internal/catalog/http.go` | Go 中心提供静态目录搜索/详情和受限能力包分发；生产发布管理仍待建设 |
| `apps/desktop/src/runtime-manager.ts` | SEA 独立更新已有校验、暂存和版本切换路径；此次保留现有存储位置 |

## 决策与所有权

```text
Electron / React ──认证的 Runtime API── Node SEA
                                         ├── Pi + Yuanpu 适配
                                         │     └── search_capabilities / execute_capability
                                         └── 能力注册表（进程内）
                                               ├── 内置 TS source
                                               └── MCP client ──stdio── Python 能力包进程
```

Yuanpu 的两个工具是代理接口，不要求再起一层网络 MCP Server。Python 标准接入采用 MCP stdio，避免为每个插件另设计 HTTP API；既有 HTTP 服务可由一个 source 适配，不能再套多个 MCP 转发层。首版不实现通用远程连接器。

| 位置 | 职责 |
| --- | --- |
| `apps/app` | 技能安装/运行状态和宿主审批交互，不持有进程控制权 |
| `apps/desktop` | 窄 IPC、可信窗口校验、装配预装制品；维持 SEA 更新机制 |
| `apps/runtime` | 组合服务、API 生命周期，不承载具体插件实现 |
| `packages/yuanpu-runtime/src/capabilities` | source、MCP client、进程管理、路由、授权与执行 |
| `packages/yuanpu-runtime/src/packages` | 制品下载、验证、安装状态、切换/回滚与包类型分流 |
| `packages/yuanpu-protocol` | 跨 renderer/desktop/runtime DTO；不向前端泄漏运行时实现 |
| `apps/python-capabilities` | 官方 Python echo MCP 示例的源码、`uv.lock` 与构建配置；它是独立能力进程，不是共享 Python 守护进程 |
| `server` | 能力包目录、不可变版本元数据；文件由受控对象存储/CDN 提供 |

不增加 yuanpu-* workspace 包，不改任何 Pi 上游包，不修改兄弟 kimi-code/openwork/pi 仓库。

## 能力契约与授权

每个 source 有持久化 sourceInstanceId；对外能力 ID 必须无歧义地映射到该实例与原始工具名，不以展示名称路由。发现返回 schema、风险、可用状态与版本；执行重新解析、验证参数及当前策略，不信任发现缓存中的许可。

采用 MCP SDK 支持的协议协商；能力包另声明业务接口版本及 Runtime 兼容范围。协议能握手不等于业务接口兼容。契约卡固定 Schema 方言/校验器与旧结果迁移规则，使用真实 SDK 类型，不手写一个不完整的“全 MCP Schema”。保留 content blocks、structuredContent、isError；Pi 不支持的块明确降级或报错，不伪造成成功文本。

契约 v1 使用 MCP 默认的 JSON Schema 2020-12，并由 Ajv 2020 在每次派发前验证。能力 ID 是 `sourceInstanceId` 与原始工具名各自 UTF-8/base64url 编码后的不透明路由键；显示名不得参与派发。Pi 原生支持 text/image 内容块，其他 MCP 块保留在 `details` 并给模型明确的降级提示。`isError` 原样保留且显示为外部能力错误。

授权状态：请求 → 宿主待审批 → 用户允许/拒绝 → 原子消费授权 → 执行 → 终态。宿主根据会话和工作区解析上下文，模型不能自报 userId/roles。待审批记录绑定 requestId、会话、工作区、sourceInstanceId、包版本、能力 ID、规范化参数摘要与有效期。

模型只能收到 needs_approval/requestId，不能通过任意非空 approvalToken 获权。`approvalRequestId` 只是宿主待审批记录的引用，不携带权限。用户经可信 Electron 界面确认；Electron 用仅存在于主进程内的临时 Ed25519 私钥签署决定，Runtime 只持有公钥并校验请求、决定、时间和 nonce。普通 Runtime bearer 不具备批准权，也不通过 argv/env 传递。宿主授权记录一次消费，并在参数变化、版本切换、取消、会话结束或超时后失效。重复批准/重放不能重复执行。无界面/headless 时需审批操作默认拒绝。许可必须先持久/原子消费，再派发；崩溃后不自动重做可能产生副作用的操作。

安全边界仅覆盖 Yuanpu 管理的能力调用。Pi 的 bash/文件工具和可信进程内扩展并未被沙箱限制；它们不能被描述为已受到统一网关强制约束。审批接口不暴露为 Pi 工具；认证 IPC 和防重放不等于对任意本机恶意代码的隔离。

## 进程与故障隔离

Python 包按需启动，复用一个受管进程；stdio stdout 仅承载协议。当前首版丢弃子进程 stderr，避免未受信日志阻塞管道或泄露凭据；以后若接入宿主诊断面，必须先实现有上限且脱敏的日志汇聚。使用已验证的绝对可执行路径、固定参数数组、不经 shell；子进程仅得到必需环境变量与明确工作目录，不继承整份宿主凭据。

当前开发态接入使用 MCP Python SDK `1.26.0` 与 TypeScript SDK `1.25.2` 的兼容协议面。Runtime 通过 `ManagedMcpCapabilitySource` 延迟启动子进程，限制初始化、发现与执行时限以及重启预算；工具调用派发后断线只返回 `result_unknown`，不会自动重试可能有副作用的操作。发现按 source 隔离，单个 source 超时不会隐藏健康 source；并发调用共享底层发现，但每个等待者保留独立取消语义。取消信号下传到 MCP SDK。自有 stdio transport 在 POSIX 为每个 source 建立独立进程组；Windows 由内置 supervisor 把 MCP 进程放入带 `KILL_ON_JOB_CLOSE` 的 Job Object。正常关闭、根进程异常退出或 supervisor 退出都会触发整树回收，三平台 Runtime Bundle 矩阵运行真实生命周期测试。

MCP `ToolAnnotations` 只是未受信提示，不能降低风险等级。未知工具默认 R2；只有宿主随已验证能力包提供的风险策略才能把特定工具降为 R0/R1。执行前重新发现当前定义，避免永久使用陈旧 schema 或撤销前状态。SDK 会补入少量基础用户环境变量，因此宿主把 HOME、USERPROFILE、APPDATA 与 LOCALAPPDATA 覆盖到能力私有目录；这用于阻断对用户凭据目录的默认发现，不构成操作系统沙箱。

开发与 CI 先执行 `uv sync --frozen --project apps/python-capabilities`，再设置 `YUANPU_PYTHON_MCP_EXECUTABLE`、`YUANPU_PYTHON_MCP_ROOT` 并运行 `YuanpuAgentRuntime --capability-smoke`。该入口实际依次调用唯一公开的 `search_capabilities`、`execute_capability`，验证 SEA、Node MCP client 和 Python FastMCP 的完整链路，同时断言结构化成功结果与 MCP `isError` 错误保真；它不绕过能力注册表，也不需要模型 API。

初始化、发现、调用均有独立超时与取消；发现采用逐源容错，返回可用结果和故障摘要，缓存有界且更新/停用会失效。工具重名不覆盖。进程崩溃只影响对应 source；重启有预算和退避，停用/卸载/Runtime 退出清理进程树。取消失败的操作标记结果未知，不自动重试可能有副作用的调用。

## 制品、存储与升级

首版默认采用 Python 完整目录制品（代码、解释器、依赖一起构建；构建实现可用 PyInstaller onedir），每个受支持 OS/架构在匹配环境构建、烟测后压缩分发。具体工具版本由实现卡依据当时官方文档固定。目标平台以现有 Desktop CI 矩阵为准；缺少制品的平台明确不可安装。

包元数据必须包含 kind、包 ID/版本、接口版本、Runtime 兼容范围、OS/架构/必要系统基线、归档格式、URL、字节数、SHA-256、入口相对路径、配置 schema、权限声明。制品自带所有首调必需依赖；模型/OCR 数据等若非随包携带，必须作为同版本锁定的受控资源显式展示和校验。

在 `~/.yuanpu/packages/` 内为进程外制品增设 `artifacts/<safe-id>/<version>/<target>/`，复用 `.staging/`、`config/` 和 `state.json` 的边界；包 ID 转为安全目录标识并检查最终路径。旧 npm 安装布局保持。凭据不放入制品/市场元数据或日志，复用宿主受控配置传递。SEA 更新继续位于 Electron `userData/runtime`，不迁移到此目录。

生命周期：下载 → 限额解包 → 完整性/真实性/兼容校验 → 隔离健康检查 → 安装为不可变目录 → 原子切换活动版本 → 下一次调用使用新版本。并发安装需跨进程协调；旧版本在调用排空或超时终止前不能删除，Windows 不原地覆盖运行中的文件。切换失败保留旧版本和配置，支持崩溃恢复。卸载默认保留用户配置，清理配置是独立显式动作。

SHA-256 只保证与元数据一致，不证明来源。生产安装使用可信签名的版本元数据，客户端固定信任根并支持轮换；拒绝签名失败、路径穿越、越界链接、解压炸弹、兼容性不符与陈旧元数据回放。降级只能由用户显式选择经过验证的历史版本。目录签名不能替代 macOS/Windows 的应用代码签名；发行卡必须验证未签名或现有签名模式下的系统提示，不承诺无警告运行。

自建 catalog 与制品域名可独立配置；官方制品与资源应在目标区域可达。失败保留旧版并允许重试，不静默回落到公网 PyPI/npm/GitHub。开发本地源必须显式标记为开发模式，不能绕过生产信任校验。制品构建可能联网，客户端运行不必联网，两者分开验收。

## 兼容与界面流程

既有 Pi 包继续原加载路径；python-mcp 包只注册为 capability source，不写入 Pi extensions。Pi 1.0 迁移会退役受管的 `pi-mcp-adapter`，由 Pi 内置 MCP 读取保留的配置；同名连接若要改由签名能力包托管，需先在“MCP 服务”中停用，安装端也检查重复托管。

界面沿用“技能市场/本地技能”，详情展示来源、权限、平台和运行状态。新安装需要用户信任确认；更新可在详情中触发，失败显示仍在用的版本及重试动作。敏感调用在对话中进入“待确认”，显示能力、目标、参数摘要和允许一次/拒绝；拒绝回到对话，不陷入自动反复申请。

TASK-007 已在现有技能页和对话页实现该流程：Runtime 验签 manifest 后，安装确认展示其中的真实权限；更新失败保留并显示活动版本，支持重试和历史版本回滚；签名 schema 驱动的配置独立于版本目录保存；敏感调用以内联卡片提供“允许一次/拒绝”，刷新后由 Runtime 的待审批记录恢复，允许后由宿主直接执行原始绑定调用。当前版本的 MCP 插件退役与配置保留规则见 [Pi 集成说明](pi-integration.md)。交互方案和浏览器证据保存在 `.tasks/ui/task-007-skill-ui/`。

## 实施与验收

顺序：基线 → 契约 →（授权、Python 接入可独立实现）→ 后端阶段验证 → 制品与分发 → 桌面接入 → 完整业务验收。公共协议由契约卡统一修改，后续变更需同步所有消费者；不存在默认的多 Agent 并行授权。

| 场景 | 必须成立的事实 |
| --- | --- |
| S1 正常调用 | 真实 SEA 经两个工具调用真实 Python 子进程；Pi 不直接加载 Python |
| S2 授权 | 伪造、过期、跨会话/工作区、改参数、改版本、重放均拒绝；允许一次只执行一次 |
| S3 故障 | 另一 source 正常时单源挂起/崩溃不拖垮发现；取消及退出无遗留进程 |
| S4 安装 | 清洁目标机没有 Python/pip，安装预装包并执行；平台不匹配/坏签名/恶意归档不可激活 |
| S5 更新 | 自建源下载新包，失败或中断保留旧版；并发调用/更新、Windows 文件占用可恢复 |
| S6 离线与迁移 | 预装及已装能力离线可用，不暗中外网拉依赖；旧 npm 包/配置不变，连接无重复注册 |
| S7 用户链路 | 界面搜索→信任确认→安装→配置→对话→审批→更新/回滚，刷新后状态一致 |

阶段验证记录：场景 ID、期望、操作、实际结果、pass/fail/unverified、commit（含未提交 diff 标识）、OS/架构、真实或 fixture、复现命令及脱敏证据路径。后端门验证 S1–S3；发布门覆盖 S1–S7 和实际支持的三系统制品。独立复核审批权是否只能由宿主产生、恶意包能否逃逸安装目录，以及升级崩溃是否造成重复副作用。缺环境必须标 unverified，不能宣称发布就绪。
