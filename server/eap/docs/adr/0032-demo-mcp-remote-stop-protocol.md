# ADR-0032：受控演示 MCP 的远端停止证明协议

- 状态：**Accepted — 仅受控演示协议设计（design only）；不构成实现验收或真实服务生产资格**
- 日期：2026-09-25
- 任务：冻结演示 MCP 连接器的远端停止证明协议（TASK-067）
- 设计基线：main claim `6013c05`；消费 Accepted [ADR-0030](0030-platform-revocation-and-inflight-boundary.md)。
- 实施唯一 owner：实施全平台撤销后的在途能力阻断（TASK-065）；本卡仅 ADR/任务记忆。

## 1. 范围与已核实事实

PRD §6.6/§12.6、System Architecture §5.2/§10 与开发计划 M3 要求统一 MCP 入口、canonical Principal/Workspace、凭据引用、Policy/DLP 和可靠审计。ADR-0030 已冻结 R 即拒绝新 grant 并发起停止、pending/unresolved、全部停止证明及审计齐全才 C，以及公开撤销 v2 协议；本文不重开这些决定。

| 本地事实 | 证据与边界 |
| --- | --- |
| 两个 MCP 公共入口共用执行服务 | `apps/gateway/mcp.go:handleMCP` → `Service.CallTool`；`agentmcp.go:agentExecute` → `capability/mcp_source.go:Execute` → 同一 CallTool。返回含 JSON/SSE 两种形式，均需共享 Release。 |
| 当前 HTTPExecutor 不提供执行控制 | `packages/mcpgateway/mcpgateway.go:Execute` 仅 HTTP 请求/context/Body.Close，无稳定远端 execution ID、cancel 或终态查询。 |
| 演示种子不等于协作端 | `migrations/000003_mcp.sql` 的 `gitlab:git_read_file`、`eap:internal_data_read` 是 GET/占位目标。本协议需显式测试装配，不能宣称种子已具能力。 |
| LLM no-go 不变 | `main.go` 装配 OpenAICompatClient；Chat/Responses/Messages 流与非流、`protocols.go:handleEmbeddings` 均无可验证远端停止协议；Bifrost Kernel 非当前 main 的 Server.LLM 装配。LongCat/Ark 连通成功不构成停止证据。 |
| 共享 ledger/profile 在另一任务分支 | 已独立核对 TASK-065 `68fd1d` 的 RegisterKeyDispatch/qualification_revision 接缝；后续 checkout `f603974` 可见同名接口。它们不是本分支已集成运行事实：预分配 attempt ID，Created=false 只恢复查询，不再发；profile 精确配置与期限检查。 |

设计对象是两个演示工具的**受控测试接收端**，替代隔离测试中的目标端而非转发真实 GitLab/内部数据库。只读取固定 fixture，并使用测试专用 effect counter 验证延迟副作用。禁止任意 shell、插件、外部数据库、子任务或互联网外发。演示通过只提升该精确测试 receiver/adapter 的资格；不提升同名真实工具、任意 HTTP endpoint 或 LLM Provider。

该 receiver 是测试目标进程，使用与本次 Gateway canonical authority **同一 PostgreSQL 数据库**内的独立测试 schema/最小权限角色（不是仅同一集群的另一个数据库），不增加生产基础服务、身份库或通用 Agent Runtime。后续若要求生产部署、其他副作用接收方或新协调服务，必须单独架构评审。当前设计可在此受控边界提供可验证终止；不能控制的外部执行仍 no-go。

## 2. 标识、状态与不可变绑定

`attempt_id` 为 Gateway 在落库前预分配的 canonical UUID，也是 remote execution ID；不是 MCP JSON-RPC request ID、trace ID 或任意客户端参数。作用域为稳定 `receiver_id` + attempt_id，receiver_id 为部署逻辑身份，持久且不因重启改变。Gateway execution_id 可对应多个 attempt（每次实际重试/fallback 单列）。

每次 accept 固定绑定：wire_version=1、receiver_id、attempt_id、Gateway execution_id、user_id、workspace_id、tool_id、profile_id、qualification_revision、config_digest、adapter_version、payload_digest。user/workspace 来自 Gateway canonical authority，内部服务签名不允许请求体自报覆盖。payload_digest 为**外发 DLP 完成后**的 payload_b64 解码原始字节 SHA-256（小写十六进制）；同一 attempt 重试必须逐字段完全匹配，不能换参数、工具或 Workspace。绑定与原始 payload 存储分开：账本与审计只保存摘要/标识，测试 payload 短期隔离保存，不能写业务内容日志。

### 2.1 发送前持久 binding 是恢复前提

现 TASK-065 的早期 attempt 仅记录 execution/profile/remote ID，**不足以满足本协议**；不能从请求内存、当前路由配置或 receiver 自报数据补猜恢复绑定。TASK-065 唯一负责扩展共享 schema/API：在 RegisterKeyDispatch 的同一受控事务中，持久全部上述 binding 字段、canonical credential kind/ID 与 user/key/member/device epoch 集、ExpectedGovernanceSnapshot、固定 cancel/status origin 与 trust/key-reference（仅引用、不含secret）、payload_digest，并同时写 attempt 意图及 started audit。profile_id/qualification_revision/config 与 endpoint/trust引用必须来自该事务锁定的服务器配置。binding 规范化 UUID/摘要后由服务器一次序列化成无重复key的 UTF-8 JSON 字节，连同其 SHA-256 `binding_digest` 持久；之后不可修改，后续 proof 的 incarnation 等观测数据单独保存，不改初始绑定。

数据库只允许受控登记函数写这些事实，禁止 runtime 直接 DML、换绑定或用缺字段的旧 attempt 获取 accept/recovery token。必须保证 attempt、完整 binding、审计全有或全无；只有确认该事务提交且 shared Dispatch 允许后，才可发送 accept。payload原文不进入 binding/日志；若无法恢复原payload也无需补发，cancel只需要持久摘要与元数据。

跨实例恢复 B 必须按稳定 attempt_id 从这条持久行读取 binding 精确字节和原profile引用，核对其完整性，再由受控 signer 派生最小 cancel/status JWT；员工body、A进程内存、最新profile或外部status均不是重建绑定来源。JWT的binding_digest及请求binding逐字匹配持久快照。Created=false、登记commit-unknown、A在发送前死亡均只走status/cancel；B禁止新accept。缺失/损坏binding或旧schema行只能unresolved/告警并拒绝C，不能伪造tombstone证明。

receiver durable 状态：`accepted → running → succeeded|failed|cancelled`；cancel 到来先持久 `cancel_requested`，尚未 accept 的 ID 创建同绑定 `cancel_requested` tombstone，再确认 `cancelled`。`succeeded/failed/cancelled` 统称 terminal，**只有工作确已静止、效果通道封闭时才能落 terminal**，错误/断连本身不是 failed 终态。崩溃未知标 `unresolved`，不能自动重入 running。任意 terminal 不可回退；同 ID 永不重跑。

receiver 的 `incarnation` 是每次启动注册的 UUID，启动记录包含单调 `boot_epoch` 与构建摘要；每个 attempt 在首次创建记录（accept 或 cancel tombstone）时固定 `execution_incarnation`，表示负责该次生命周期的进程代际，不意味着一定已启动 worker；另存 worker_started。新进程不得把自身 epoch 当成旧进程死亡证明。终态 proof 同时记录原执行 incarnation 与出具证明 incarnation；正常 join 两者相同，跨代恢复必须附 §5 的旧进程终止证据。

## 3. 内部 wire 与认证

内部命名空间为 `/internal/eap-stop/v1/executions/{attempt_id}`，仅接收 Gateway 服务连接；不是新的员工公共 MCP 协议。现有 `/mcp/{server}` 与 `/mcp/agent` 的请求/结果格式保持原契约。本协议只在服务器端 adapter 与测试 receiver 间使用。

所有请求 HTTPS，并验证固定 receiver_id 对应服务器证书；profile 固定 origin、路径前缀、证书/签名信任锚、adapter/config 摘要。禁止 3xx 重定向、客户端 URL、环境变量临时改目标。服务端凭据通过现有 broker/reference；员工 agk、Provider/MCP backend secret 均不得用作取消授权或送到 receiver。

Gateway 使用现有内部 issuer 签发短期、明确 audience=`eap-demo-receiver:<receiver_id>` 的 JWT，固定 subject=Gateway 服务身份，携带 action、上述不可变绑定、jti、iat/exp。accept/result 的 JWT 另携 canonical Principal/Workspace 委托及当前授权/治理 snapshot 引用，必须由共享 Dispatch/Release 门禁准许；服务身份不是新用户授权来源。cancel/status 使用最小 recovery scope，Gateway 只可依据已有 ledger 为固定 attempt 重新签发，不依赖已被撤销员工继续授权，也不能用 recovery token accept/read result。receiver 验 issuer/audience/签名/期限、路径 ID、action、binding_digest 和所有 binding；accept/result/effect 还必须过 §4.1 的在线 canonical 门禁，JWT 或snapshot引用不是当前授权证明；错误统一不泄漏其他 Workspace 的 execution。

| 方法 | 请求与持久保证 | 响应 |
| --- | --- | --- |
| `PUT /.../{id}`（accept） | JSON `{binding:{...}, payload_b64:"..."}`；JWT 绑定必须一致；校验摘要、固定工具 allow-list/参数 schema。在 §4.1 canonical gate 与唯一键串行事务内插 execution+accepted audit，commit 后才入可恢复队列。启动 worker 前再验在线门禁与 cancel/terminal。 | 首次 202 `{state:"accepted",attempt_id,receiver_id,execution_incarnation}`；相同绑定重复 200 当前状态，不重复 enqueue；同 ID 不同绑定 409 `execution_conflict`；已有 tombstone 返回 200 当前 cancel_requested/unresolved；仅 T 后返回 cancelled + proof，永不执行。 |
| `POST /.../{id}/cancel` | JSON `{binding:{...},reason:"authority_revoked"}`，不含 payload。cancel 与 accept、worker 启动/效果提交同 execution 行锁串行；commit cancel_requested+audit 后再通知 worker 停止。未知 ID 也创建 tombstone，不能只返回 404。 | 未证实停止 202 cancel_requested/unresolved；已 terminal 返回 200 + proof。重复取消幂等；同 ID 绑定不符 409。HTTP 200 本身不是完成依据。 |
| `GET /.../{id}`（status） | recovery action=status，固定 ledger 绑定；强一致读取持久状态，Cache-Control:no-store。 | 200 仅状态/证明；无记录 404 unknown（不是 stopped）；数据库不可用 503 unknown。无 business payload。 |
| `GET /.../{id}/result` | action=result，仅允许成功终态，原绑定与receiver端 §4.1 在线 Release 门禁有效；Gateway另行执行自己的 Release/DLP。 | 200 fixture 结果；未成功 409 result_unavailable。R 后不得获取新 result grant；此前已登记writer须取消/join，未排空不可C，recovery token 永远403。 |

accept/cancel commit 响应不明返回 503 outcome_unknown 或断连：调用方保存 attempt ID，不发新 ID猜测结果。receiver 对完全重复的 wire accept 仍幂等（用于防网络重放），但 Gateway `Created=false` 路径只 status/cancel，不把查到旧 row 当第二次 send permit；首次 Created=true 在真正写之前仍检查 shared Dispatch。如果登记成功而从未发出，恢复时也发 cancel 建 tombstone，并确认 sender 已 join，不能仅凭 status 404 宣称停止。

每个内部 HTTP 请求预算 5 秒；cancel 已 commit 而 join 尚未完成就返回 202，不占长连接等终态。Gateway 用独立有界 recovery context 重试 status/cancel，不绑定已断开的员工 HTTP context。30 秒仍无法停止沿用 ADR-0030 unresolved/可靠故障事件/告警；退避仅调整轮询，不延迟首次 cancel。DB 不可用/证明写失败不给成功 ACK；取消计算可以先执行，但不能冒称审计或终态已持久。

## 4. 工作停止与副作用封闭的可执行定义

受控 receiver 的一个 attempt 只有一名活 worker；durable claim 记录 instance/worker 标识。并发 accept 只创建一次，worker 启动与取消在本地 lifecycle mutex + 持久 execution 行锁中决定，不能 accept 提交后启动一个未被登记的 goroutine。队列/timer/输出 publisher 都属于该 attempt 的 join 集合。

演示业务为 fixture 读取与可暂停分段计算。测试副作用仅为 PostgreSQL 测试 counter/event 表（effect_seq使用事务内行计数，不使用不可回滚sequence作为效果）：每个 effect 必须通过 §4.1 受控事务，先验证并锁定canonical授权/治理事实，再锁receiver execution检查 state=running、incarnation/worker token相符且无cancel，追加 `(attempt_id,effect_seq)` 唯一事件、更新counter与可靠effect audit，同事务提交。禁止事务外定时器、外部网络或数据库写；除本协议、审计及受控 supervisor 通道外的业务副作用 I/O 不属于合格实现。R 与 effect 持同一 canonical authority 锁串行，因此即使 K 报文仍在网络中，R commit 之后也不允许新的 F commit；Cancel 另持receiver execution锁将cancel_requested落库，K之后的效果亦被拒绝；此前已提交的 counter 不回滚，不伪造撤销历史效果。

### 4.1 接收方在线 canonical gate 与 R 串行

受控演示receiver没有独立身份authority，必须使用同一 PostgreSQL 数据库里的 TASK-065 受控函数。其 runtime role 只获固定 receiver_id 的 gate/effect/recovery 函数 EXECUTE 权限，不获 users/api_keys/membership、profile、ledger、effect/audit表直接写权限；SECURITY DEFINER函数由独立不可登录migration owner持有，固定search_path与schema限定，拒绝任意SQL/表名/Workspace参数覆盖。函数从完整attempt binding解析canonical身份与scope，核对DB调用role绑定的receiver_id；业务HTTP JWT仅证明请求者，不能替代此在线读取。

`AcceptReceiverExecution`、`BeginReceiverWork`、`CommitReceiverEffect` 和 `AcquireReceiverResultRelease` 均在同一受控事务读取canonical user/key/member/device当前epoch、禁用/撤销/expiry、原profile/config资格及ExpectedGovernanceSnapshot。比较失败、治理切换中或权威不可达时fail closed，不使用JWT TTL、内存缓存或receiver本地state代替。`CommitReceiverEffect`将此授权核验与实际counter/event/audit写合在一次数据库事务，禁止“先HTTP查权限、再另一个事务effect”的TOCTOU。

统一锁顺序由TASK-065落实到Gateway与receiver受控writer：先从不可变binding读取ID，再按 user → key/device-family → membership → scoped governance gate → Gateway execution/attempt → receiver execution → effect/audit 锁定并复核，多个scope/ID按稳定顺序。禁止已有方法持execution锁后再反向获取authority锁与新路径混用；owner需迁移现有早期seam并增加真实双事务回归。R只需canonical目标/operation/audit的短事务，不等待receiver join或网络；effect事务持authority读锁到F提交，R持写锁到撤销提交，所以F先提交或R先提交二选一。若F先获锁，R可等其短事务，但不得被网络/长期计算拖住：effect函数禁止外部调用、sleep/后台工作，运行角色锁/语句超时与ADR-0030请求预算一致；事务失败不产效果。join在事务外执行。

Result的每次实际写也必须登记为共享release writer：`AcquireReceiverResultRelease`与R串行核验、持久登记sender incarnation/绑定后才准许受控writer；R后新grant拒绝。已登记而未结束的Write/Flush属于R..C在途集合，不能以一次在线检查自称物理字节瞬间停止；receiver必须取消并join所有result writer，未能证明的writer使T/C保持unresolved。禁止一个长效result许可覆盖后续多个未登记写入，业务worker自动启动的publisher属于原执行，必须join后才T。`GET /result`是T后对已持久结果的显式只读操作，不重启业务worker；它需要独立持久release_id、原attempt/Principal/epoch绑定和sender incarnation。Receiver与Gateway各自的这些读取writer均独立纳入C聚合，旧T proof不能证明后来结果读取已停止。R/K关闭该attempt的result admission，已登记result writer必须cancel/join或取得§5同级fence证据，并以同一receiver签名方式保存绑定release_id的停止收据后C才可完成；不得通过“原业务已terminal”漏掉活跃读取。R先于result grant时返回零业务内容字节；慢读取每段尾部重新申请Release，R后拒绝后续段并取消已有writer，已进入Write的那段必须join后才能C，不能以cancel调用已返回充当停止。

接收方失去canonical DB连接时不得accept新工作、提交effect或获取result grant，并主动cancel本地worker；已存在writer/计算是否真正终止仍以join/fence证明，不能因fail-closed检查失败就标terminal。R..K间worker可能还在计算，**无新effect是数据库门禁保证，全部远端停止是K/cancel+join/T证明保证**，两者不能混称。cancel/status/终态恢复是受限安全操作：读取绑定/写取消审计但不要求原用户仍有效，也不允许提交业务effect或内容释放，因此离职/Key撤销不会阻止停止工作。

取消处理必须关闭调度、取消 worker、等待实际 worker/所有派生 timer/结果 publisher join。只有 join 成功且 effect barrier 已关闭，才能同事务将 receiver execution 置 cancelled、写 terminal audit 和 proof payload。正常 succeeded/failed 也必须 join 且关 effect barrier；不能先标成功再由 defer 启动异步工作。持锁不等待 worker join，避免 worker 等 DB、cancel 等 worker 的死锁：先持久 cancel_requested，释放事务，再 join，再短事务核对 epoch/无活 owner 并终态。

terminal proof 至少含：protocol、receiver_id、attempt_id、execution_id、user_id/workspace_id/tool_id、payload_digest、profile/config/adapter/qualification_revision、execution_incarnation、proof_incarnation、boot_epoch、terminal_state、monotonic_state_revision、last_effect_seq、effect_barrier=closed、workers_joined=true、terminal_audit_id、可选 fence_receipt_digest。它证明“该固定工作不能再运行或产生新效果”，不是只有数据库中某个字符串等于 cancelled。

receiver 将 proof payload 与 terminal audit/state 同事务持久，commit 后用该 receiver 专用 Ed25519 签名 key 对存储的**精确 UTF-8 proof 字节**签名，返回 `{payload_b64, signature_b64, key_id}`，不由客户端解析后重新序列化验签。签名可重试生成，事务未提交前禁止签发。签名 key 与 Gateway/internal JWT/package key 分离，证书/profile 限定信任锚；原始签名和 proof 摘要由 Gateway 可靠保存，再把 attempt 置 terminal。拥有 key 的 receiver 属于受资格约束的信任边界，签名只证明来源；worker/effect 的故障验证和最低权限部署才支撑其真实性，不能仅验证签名就签发资格 profile。

C 的 Gateway 端仍需完整binding证据及所有受影响 attempt 的有效 proof、所有本地 sender/release writer join、权威撤销持久和 completion audit；单 receiver cancelled 不等于 Key C。Terminal 前的已交 OS 字节可能迟到，不能追回；Gateway 不再读取/释放其 business result，不把迟到字节当新授权。R..C 仍可能有旧执行，必须保持 pending/unresolved。

## 5. 重启、incarnation 与证据寿命

活 receiver 正常 cancel 必须以自身 join 证明终止。receiver 崩溃/进程暂停/与 DB 分区时，新启动实例只能读取状态并报告 unresolved，不能用 PG connection 消失、heartbeat/TTL 超时或 boot_epoch 增大宣布旧 worker 已死。旧实例重新连上也不能复用失效 claim；effect 事务须核对数据库 incarnation。

受控测试恢复的唯一跨代终止途径：测试 supervisor 拥有 receiver 完整进程组/容器及其 worker，先强制终止并 `wait` 确认全部进程退出，确认不允许 daemonize/逃逸进程、没有外部子任务，关闭其网络/输出句柄，再生成持久 fence receipt（old incarnation、进程/容器 identity、退出证据、构建及隔离配置摘要）。supervisor receipt 由独立测试 supervisor key 对精确持久字节签名，该 key 的验证锚在资格 profile 中固定；收据绑定完整进程组/容器创建身份（不能只用可复用 PID）、旧 incarnation、隔离范围和退出结果，不接受调用者自由填写的“已停”标记。只有 receiver 受控恢复器验证可信 supervisor receipt、持久冻结旧 incarnation、确认 effect barrier 与旧 claim 关闭后，才可对旧 execution 生成跨代 cancelled proof，引用 receipt digest。测试 runner 不是身份权威，仅证明自己控制的测试进程终止；不把此能力泛化到真实远端服务。无可验证进程终止（包括无法访问旧宿主机）时保持 unresolved，绝不按时间过期清 orphan。

receiver 必须禁止两个同一 incarnation 的进程启动，重启新 incarnation 先完成注册；旧进程尚未证实死亡时新实例可处理 status/cancel、不得接收新的 accept（503 recovery_incomplete）。已持久且有效的 terminal proof 可在重启后原样返回；不能改变原执行 incarnation。签名 key 轮换保留旧 verification key 和已封存 proof；key 泄漏/资格撤销的证据按配置治理触发 unresolved/安全事件，不能接受其新签 proof。

cancel tombstone、terminal proof、execution 绑定、audit 不按请求 TTL 删除，演示首版不实现此类 GC。否则网络延迟的旧 accept 会复活任务。测试库清理只发生在 Gateway/receiver 全部停止且该 receiver_id/签名 key/profile 已永久退役后；新测试命名空间用新的 receiver_id，不能恢复备份到旧 ID 后接受旧令牌。丢失 receiver DB/证明且不能核对完整 ledger 时更换 namespace并维持旧操作 unresolved，绝不从新空表推出 stopped。

## 6. R/accept/cancel 的线性化与失败矩阵

A=receiver accept durable commit；F=每次 effect 事务提交；K=cancel_requested/tombstone durable commit；T=join/fence 证明后 terminal durable commit。A 与 K 由同一 execution ID 唯一键及行锁串行，未存在记录的竞态也必须使用 canonical ID advisory/唯一插入重试，不能先查不存在再无保护执行。K 不等于 T；C 必须在 T/proof 验证及 Gateway 自身排空之后。

| 情形/注入点 | 确切处理与 C 条件 | 必须记录的观察 |
| --- | --- | --- |
| R 在新 Dispatch 前 | 不再 accept，若此前已登记则以相同 ID cancel tombstone；join sender。 | 实际 accept count=0；不能只看入口返回码。 |
| R已提交但K网络延迟，旧JWT仍有效 | receiver在线gate拒绝新accept/result grant，effect事务与R串行而拒绝新F；旧计算尚未join仍pending，不发T/C。 | 隔离cancel通道后用未过期旧JWT实际请求accept/result并触发effect，断言新增accepted行/result grant/F均为0、C为0；原有已登记writer单独排空，不以旧JWT或T掩盖。 |
| Gateway A登记完整binding后、发送前崩溃；B为独立进程 | B仅从持久binding派生cancel/status令牌，未知远端用匹配tombstone收敛；缺binding拒绝恢复/C。 | 杀A并丢弃全部内存及原payload，B仅读共享行；改当前profile配置后仍使用原绑定，token/body/digest逐字段相同；缺任一绑定拒绝签发，伪绑定拒绝，无第二accept，有效proof验证后才可C。 |
| A 前的报文延迟，K 先到 | durable tombstone 胜出；迟到相同 accept 只读 cancelled，不启动 worker。 | 延迟释放报文后 worker/effect count 仍零；有效 T 可参与 C。 |
| A 先到，R/K 与 worker 启动竞争 | 启动前检查同一行，未启动则直接取消；已启动就 cancel+join。 | enqueue/start 一次；只有 T 后可 C。 |
| accept 响应丢失/commit unknown | Gateway same ID status；404 非停止，发 cancel；receiver 无论曾否 accept 都收敛到同 ID T。 | 不创建第二业务 attempt，不重复效果；未知期间 C 禁止。 |
| 并发重复 accept/cancel、不同大小写 UUID | 先 normalize；同 binding 幂等；异 binding 409，原状态不变。 | 一个 execution、每个 effect_seq 一次、无跨 Workspace 结果。 |
| F 与 K 竞争、timer 延迟效果 | F先提交可保留历史计数；K先提交则 F拒绝；随后 join所有 timer/publisher。 | 记录实际 commit序号，T之后计数无增加，无活worker。 |
| cancel 202/200 ACK但worker仍阻塞 | 202仅确认请求；没有完整proof就 pending，30秒转unresolved告警。 | cancel调用返回不是停止断言；恢复join后才T。 |
| worker paused、网络或DB分区 | 不推断停止；能发cancel就立即发，无法核验持续unresolved。 | 无效/空profile不得C；不可用后新grant拒绝。 |
| receiver在A/K/T事务各点崩溃 | commit未知依同ID持久查验；新incarnation不得自证旧进程停止；有supervisor receipt才恢复终态。 | 旧worker确实退出、effect不再增长、proof原代际正确。 |
| Gateway A实例崩溃，B恢复 | B从共享ledger取得同attempt/profile，最小service recovery token仅cancel/status；禁止复发accept。 | 无第二accept/效果、可跨实例完成C且审计链连续。 |
| requested/started/terminal/C audit失败 | 对应状态事务回滚或维持未完成；已停止但proof/audit未落库也不可C。 | 数据库真实拒绝审计写、操作状态与审计一致，不用日志代替。 |
| 伪造/错ID/错Workspace/旧profile/篡改proof | 验签、binding/代际与资格校验失败，attempt保持unknown/unresolved，记录安全事件。 | 原文不泄漏，恢复token不能accept/result。 |
| profile禁用/过期/新revision | 新accept实时拒绝；旧attempt绑定原profile继续stop/status恢复，不以新profile改写旧proof。 | 旧FK/证据保留；恢复凭据保留受控最小权限。 |
| status 404、200 running、socket断开、SSE结束 | 均不是 terminal proof；不得C。 | 特别断言 C事件数量为0。 |
| 真实Git/LLM或receiver外部子任务 | 本协议无终止证据，admission deny/no-go，不走demo伪proof。 | LongCat/Ark/普通HTTPExecutor从未被自动资格提升。 |

## 7. 交接、部署资格与验证门禁

TASK-065 唯一拥有 Gateway/Store/adapter/writer 接线；本 ADR 已经独立审查接受，后续由该 owner 实现 `eap-cooperative-v1`。receiver/测试 harness 作为该任务受控 fixture（建议独立 testcmd 与 integration fixture 包），不改两个公共 MCP tool name/schema，不让员工 body 选择新协议。Registry 明确映射某测试 target 到 profile/adapter；生产默认无合格 profile，未知配置 503 admission deny。

最小实施顺序：共享 authorizer/Dispatch/Release/CommitMutation 与最小权限运行角色 → receiver durable状态/幂等 wire/审计/签名 → adapter profile匹配及ledger proof验证 → 两个MCP入口接线 → 故障矩阵。资格 profile 只由受控审核 writer 写入，绑定 receiver_id、origin/trust anchor、build/adapter/config摘要、qualification_revision及故障验证证据；runtime不能自造/启用profile、禁触发器或改proof/coverage。现有 role/coverage/governance gap 不因演示服务完成而解除。

资格必须证明完整封闭工作集：receiver只运行这两个fixture、无外部副作用/后台逃逸，cancel/status权限不可升级，所有效果经受控事务且所有计算实际join或可信进程组终止。仅 `workers_joined:true` mock/200响应不能通过。不能证明则此 profile 维持 disabled，设计实现可继续但不得接入强保证流量。

可运行验收环境：两个真实 Gateway 进程、共享 PostgreSQL 独立schema/最小权限角色、一个受supervisor控制的receiver进程与隔离fixture、独立端口和临时证书/签名材料、可暂停实际socket/worker/DB事务的测试屏障。Harness记录实际accept/start/effect/terminal/proof/C序列以及进程join结果，不依赖跨机壁钟判断先后。每次用fresh namespace，失败保留脱敏ledger/proof与trace证据。不能修改共享开发数据库或真实服务。

未来测试必须从 receiver单元状态机 → 真实PG双事务 → adapter真实HTTP双Gateway → 断连/进程停止/签名伪造 → 两入口业务路径逐级运行，核对真实结果内容/DLP/audit与故障。完整停止测试不仅断言counter不增，还断言worker/子进程确实不存活。至少覆盖两个入口各自JSON/SSE返回、R前成功路径、R后取消路径、用户/成员/设备/Key祖先撤销、非协作LLM仍拒绝。本设计卡不创建尚不存在的测试命令或伪造通过记录，实施卡须登记新增可运行命令并执行受影响protocol/MCP/security/foundation门禁。

## 8. 当前验证与独立审查

本卡只做设计一致性、文档路径/registry validate 和固定提交独立审查；没有 receiver、签名、网络取消或真实远端测试通过声明。检索沿用索引缺失事实、不建索引；按 PRD/架构/M3、两个MCP入口、HTTPExecutor与已核对的TASK-065精确符号限定rg和读取。

独立 Sol 审查范围：cancel-before-accept墓碑是否堵住迟到发送；worker/effect/join是否足以证明终态；崩溃/跨代是否错误推断旧进程死亡；recovery身份能否越权或读取结果；proof对原配置/代际绑定是否完整；两个MCP入口有无旁路；LLM no-go是否保留。本次状态变更依据下述固定提交独立结论，作者不自行宣称任务已集成完成。设计接受只解锁受控演示协议实施，不构成生产资格或TASK-065全卡完成。

首次独立 Sol 对固定 `50b462b` 的初步阻断处置：本修订新增 §2.1 完整binding与started audit发送前同事务持久、跨实例签发只消费该行；§4.1 将receiver accept/result/effect接到同一canonical数据库受控门禁，明确R与F串行、断连fail-closed及K/T的物理停止职责。root 已转达该次完整审查仅以上两项blocker，其余范围/墓碑/join/跨代/no-go成立；该轮保持Proposed，修订形成 `8726e87` 送独立复审；最终结论如下。

**最终独立审查（2026-09-25）**：审查人为独立 GPT-6 Sol（`/root/wiki_boundary_review`），由 root 协调并转达。固定审查提交 `8726e8755b97d84b08d467a5257281785675ad1a`。结论：**Accepted，仅受控演示协议设计**；初审的完整binding跨实例恢复、receiver canonical R/effect fencing两项阻断已闭合，无新设计阻断。此次evidence-only状态/记录更新不改变已审协议。

实施门禁仍由TASK-065唯一owner负责：完整无原文binding的schema/API与审计原子性、同canonical数据库受控函数和非owner runtime role、authority→execution统一锁序、receiver/adapter及双Gateway真实故障矩阵、所有worker/release join与可验证proof。上述运行验证通过前不能宣称演示strong-stop已可用；真实MCP、普通HTTPExecutor、LongCat/Ark及其他LLM路径继续no-go。root负责设计卡集成验证及注册表complete，本文接受不替代该流程。
