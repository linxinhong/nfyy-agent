# ADR-0030：全平台撤销与在途执行完成边界

- 状态：**Accepted — 仅冻结设计边界/协议及生产 no-go；不构成实现验收或上线批准**
- 日期：2026-09-25
- 所属：冻结全平台即时撤销与在途执行边界（TASK-064）
- 基线：`a258a9d`（claim 提交；生产代码与 `a94aaae` 相同）
- 安全架构独立审查：Sol 接受固定提交 `2af93c9` 的设计决定；非阻断澄清已补入，历史与最终结论见 §9。

## 1. 决策范围与结论

PRD §12.6 要求离职或 Key 吊销后所有 Agent 能力立即不可访问。系统架构允许水平扩容、内部 JWT 本地验签；Architecture Boundary 要求 canonical 身份、逐请求实时权限、可靠审计。短 TTL JWT、通知、取消 context、再次查询权限各自都不足以解决“查询通过后暂停，撤销成功后恢复发送”的竞争。

**用户已确认的产品要求（本轮 root 转达）**：Key 吊销后，所有 LLM 和 MCP 在途执行必须停止，不能允许已鉴权请求继续完成。该要求覆盖流与非流、直接 MCP 与 Capability Gateway；只阻断新请求、隐藏响应或取消本地 context 均不能替代停止执行的证明。用户随后已明确接受“处理中 → 确认完成”：R 提交即阻断新授权动作并发起所有相关 LLM/MCP 在途停止，接口先明确返回 pending；只有全部相关执行有停止证明且可靠审计提交后才 completed。无法确认则 unresolved 并告警，不能虚报完成。该决定批准处理状态语义；本次架构提案在 §7 冻结具体版本化 wire 契约供独立审查，不等于已经发布 API。

本提案定义全平台统一的安全完成条件，并给出实现接缝。**现有同步 DELETE 204、任意暂停/分区下有限时间完成、撤销后不发生旧权限内容释放或新副作用，这三者不能在当前部署能力下同时保证。** 用户接受处理状态后，接口无需同步等待所有执行停止，因而不再要求同步 204 同时承担 R 与 C。接口应在有界请求预算内返回 pending 或明确的基础设施错误；该预算不是撤销停止证明的超时，不能把尚未停止改称撤销失败或完成。远端停止能力与实际发送隔离仍须证实，不能因允许 pending 而无限静默等待：超出停止观测期限即 unresolved、告警并进入恢复流程。本设计已独立审查接受；TASK-064 经 root 集成验证并完成后，可解锁 TASK-056 的契约复审与 TASK-065 实现。TASK-057 仍等待其全部依赖完成；任何设计卡完成都不代表强停止生产能力可用。

“所有能力”包括直接 MCP 和 Capability Gateway、三种 LLM 协议、embeddings、Knowledge、CLI 获取包/凭据/票据及服务器内的相关后台委托。依据已 Accepted 的 [ADR-0010](0010-eap-cli-v1.md)，CLI 管理本地安装/同步，服务器分发包、策略和短期票据，不拥有离线机器的远程擦除或通用执行控制。已下载/安装的副本及员工保存的输出不能追缴，也不承诺停止本地离线执行；服务端撤销约束后续在线票据签发/兑换、下载和同步授权。客户端同步仅操作 EAP 管理的文件并保留非托管内容，不能将在线拒绝等同已删除本地副本。该本地边界不豁免 EAP 发起且仍在远端运行的 LLM/MCP 执行。

## 2. 已核实的运行事实（不是目标能力）

| 接缝 | 当前证据与缺口 |
| --- | --- |
| Key | `apps/gateway/auth.go:authenticateAPIKey` 读取 canonical `revoked_at`/expiry；`selectWorkspace` 逐请求读取 membership。已取得的 session 不随数据库变化自动失效。 |
| 撤销 API | `enterprise.go:handleRevokeKey` → `store.go:RevokeAPIKey`：同一事务更新未撤销且属于调用者的 Key、插入 audit；commit 后 204。未知、其他人的 Key、重复撤销均 404（`no live key with this id`），不是幂等 204。无在途排空。 |
| Chat 非流 | `llm.go:handleChatCompletions` 路由调用、治理审计、`writeChatResponse`；不存在释放前的撤销屏障。legacy 路径亦须覆盖。 |
| Chat SSE | `protocols.go:handleChatStream` 已鉴权后启动 provider，emit 直接 Write/Flush；DLP holdback 不验证撤销。 |
| Responses / Messages | `protocols.go` 的 stream/non-stream handlers、`openSSE` 同样入口鉴权后发送；不可用 Chat 测试替代。 |
| Embeddings | `handleEmbeddings` 经 commonProtocolAuth 后直接 provider client，最终 writeJSON；共享边界必须覆盖此旁路。 |
| MCP | `mcp.go:handleMCP` 的 tools/call → CallTool → DLP → writeMCPResult（JSON/SSE）；`agentmcp.go:handleAgentMCP/agentExecute` 为第二公共入口。入口验权不约束之后工具调用和响应。 |
| Knowledge | `knowledge.go` 的 upload/search 入口鉴权后 Ingest/Search、writeJSON；生产 HashEmbedder 不是远程模型证明；未来 egress 必须消费统一门禁。 |
| Device | `device.go` refresh 查 `device_credentials.status`，logout 仅撤销当前 active refresh 行。`tokenIssueFor` 的 access JWT 无设备凭据族关联；`auth.go:authenticateSession` 只验 JWT/GetUser，不查设备状态。logout 审计失败仅 log，随后仍 204。 |
| 用户离职 | `store.go:GetUser` 只选择 ID/email/display_name/org/department，不能声称已有 canonical disabled 状态和全凭据撤销链。需要同一 user 权威上的状态迁移，不新建身份服务。 |
| 协议范围 | `server.go` 注册 Key-only 的 LLM/MCP/Knowledge；`requireAPIKey` 拒绝无 Key 的 session。设备修复不意味着悄悄允许 device token 调用所有 `/v1/*`。 |

检索：继承索引 INDEX_MISSING 事实，不建索引；按上述确切文件/符号、PRD §12、架构和 ADR-0029 §7 做限定 rg 与片段读取。ADR-0029 仅在 `task/task-056-knowledge-contracts`，不是本分支已接受依赖。

## 3. 需要冻结的时序定义

以下为候选契约，不能当作当前实现证据。

- **R（拒绝提交）**：canonical 禁用/撤销/成员变更与可靠 `revocation.requested` 事件在同一 PostgreSQL 事务提交。R 后不准新登记授权动作；权限恢复/重新入组分配新 epoch，旧请求永不复活。
- **D（实际发送）**：EAP 控制的出口开始交付 provider/tool 请求字节，或向客户端/下游交付内容字节。排队、取得 permit、序列化、检查通过均不是 D。重试、fallback、重定向、下一 SSE chunk 各是新动作，不能沿用一次 permit。
- **C（撤销完成）**：R 已提交，所有受影响旧 epoch 的发送者已证明停止/完成，旧委托不能恢复，可靠 completion audit 已提交。只有 C 后才可声称“撤销完成”及满足该次 PRD 强停止完成条件。R..C 的 pending/unresolved 期间可能仍有旧执行运行，界面与 API 必须明确这一事实，不能说“全部已停”。在途计算结果即使已完成也必须丢弃；未知外部执行不冒称取消成功。C 的证明集合既包含 EAP writer/委托，也包含已 dispatch 的相关 provider/tool execution：只证明 EAP 停写而远端状态 unknown 不允许 C。
- **不可逾越条件**：C 后不得开始或继续 EAP 控制的旧权限内容释放、外发、内部持久副作用。持久变更的事务必须与 R 串行，不能出现 C 后旧 upload 激活或旧 refresh mint。C 不能仅依据取消通知、TTL、DB session 消失或负载均衡摘流。
- **交付边界待确认**：已经在 C 前交给 OS/代理/接收方的字节可能 C 后才被观察；已交远端的工具可能 C 后才执行。已交 OS 的字节无法撤回，必须记录为先前交付而不能当作撤销后的新授权；这不允许尚在执行的远端工作继续。用户要求全部在途执行停止，因此 C 必须有接收方终止/终态证据或可验证地强制终止该工作的证明；无此能力的 provider/tool 无法支持强停止保证。已经发生的远端副作用不作回滚承诺，补偿若有需要属独立授权动作；停止证明必须确认不会继续产生该执行的新效果。不得把 D 定义成“拿到许可”来掩盖 C 后实际写入。

R 后，可达的 canonical authority 不再签发旧权限的新 grant；无法访问权威的新申请 fail closed。这不保证 R 前已经获准、随后与 PostgreSQL 分区的 writer 在 R 时停止实际字节或 chunk：其可能继续已有写入，或在暂停后恢复。R..C 因此是显式未完成状态；在证明所有旧 writer 停止之前，C 不得完成。若产品要求 R 时物理发送立即停止，需要可强制的出口 fencing 或协作接收方，当前门禁记录和 cancel 通知不能提供这种保证。

## 4. canonical 状态与共享执行接缝

TASK-065 独占 Gateway/identity/store 的执行门禁及迁移。依然由现有 PostgreSQL canonical user、key、membership、device credential family 决定权限；执行记录仅为其派生授权的生命周期账本，不能自行授权。

候选最小数据：revocation operation ID、目标类型/ID、单调 epoch、requested/completed 时间及状态；execution ID、instance incarnation、Principal/Workspace、credential kind/ID/family、授权 epoch 集合、action、dispatch 状态、trace/request ID。不存原文、prompt、refresh token 或密钥。用户、Key、成员、设备族均可命中同一 execution；按固定 target 顺序锁定避免死锁。

| 目标 | R 后失效集合 |
| --- | --- |
| Key | 该 key 的当前/排队/后台委托，跨所有 Workspace；其他独立 key 不受单 Key 操作影响。 |
| 用户离职/禁用 | 该用户所有 Key、Portal/device session、refresh、内部委托、后台 job、已签票据；禁止 mint 新 key/token。用户状态仍归 canonical user。 |
| Workspace 成员移除/角色收窄 | 该 user + Workspace 的执行、包下载、票据、模型/工具/知识；其他合法 Workspace 保留。移除再加入不能复活旧 execution。 |
| 设备族撤销/过期 | 该族旋转前后的 refresh/access 及其委托；独立设备/CI key 按各自权限继续。需将族 ID 绑定到服务器签发的 access claims，不能信任 Device header。 |

共享接口职责（名称为设计，不宣称已有实现）：`BeginExecution` 读取并登记 canonical authority；`Dispatch` 管住实际网络写全过程；`Release` 管住员工内容实际 Write/Flush 全过程；`CommitMutation` 将授权复查与业务变更/audit 原子提交；`CancelAndJoin` 等待本实例 writer 真正停止；`CompleteRevocation` 检查跨实例证明并落审计。入口 session 只是上下文，不是可无限复用的授权。

关键竞争规则：先在短事务读取/锁定目标状态并登记 execution，R 与登记互斥。发送之前持久登记 dispatching，并在实际 writer 全生命周期保留待排空记录；完成写且确认不会再写之后才能落 stopped/completed。R 后通知仅加速 cancel，不能成为正确性依据；漏通知时，新 grant 申请仍由实时权威拒绝，但此前已获准的 writer 不能据此视为已停止。C 检查所有登记者，不依赖当前存活实例列表。若记录已登记但尚未发送，观察到 R 的执行者必须取消并 join，不能以“许可已发”有意继续发送。若执行者分区或暂停而未观察到 R，其恢复后实际写出的风险必须保持 unresolved 并阻止 C；不能把登记时刻当实际发送时刻，也不能宣称现有机制在 R 时已阻止该风险。

DB session/advisory lock 可以串行 canonical 更新，却不能隔离已拥有 socket 的分区进程。session 消失留下 dispatching 记录时必须保留 unresolved；重新连接不能续用旧 permit。进程内 map 只用于取消句柄，重启不会消除数据库证据。DB runtime role 必须禁止绕过治理更新及执行账本，migration owner 与 runtime 分离；不得依赖现有高权限 compose 账号作强制边界。这是待实现部署条件，不是已有保证。

跨服务内部 JWT 继续有 audience/短 TTL，但携带不可自行修改的 execution/credential lineage；每个能产生副作用或释放内容的接收方必须验证执行门禁。仅本地验签可继续计算，不能继续授权效果。不能控制出口的旧服务在强保证上线前必须禁用。服务 identity 不解除原用户撤销约束。

## 5. 各协议的强制检查点

| 路径 | 发送/取消/释放规则 |
| --- | --- |
| Chat/Responses/Messages 非流 | 每次 provider attempt 在 Dispatch；返回前审计及 Release；撤销后丢弃整份结果，尚未发 header 返回既有认证/授权错误。响应写到一半时关闭连接，不把余下 JSON 写完。 |
| 三种 LLM SSE | 首次 provider 调用、每次 fallback 受 Dispatch；每次实际 Write/Flush 受 Release；cancel producer、清空 DLP 缓存、join writer。已发 200 后直接终止流，不再发旧 delta、success/usage/DONE；已发出的部分不能收回。 |
| MCP direct /mcp/agent | 审批续接/重试必须复验，在真实 connector dispatch 前检查，工具结果两种响应格式均 Release；请求已发送而无终止证明标 unknown，绝不自动重放有副作用调用。 |
| Embeddings / Knowledge | Query 与文档向量化每次 attempt 都 Dispatch；引用/内容返回 Release；upload 激活与授权 epoch 同事务；撤销后未激活版本不可见。缓存命中也必须过 Release。 |
| Device / CLI / Skill / Agent | refresh、签 ticket、发布/下载分别覆盖 mutation/Release；票据消费再验族/用户/成员及包状态。key-only 入口保持原有凭据类型规则。已下载离线内容无法追缴。 |
| discovery / reveal / exchange | 受保护目录、DLP reveal、内部 token 签发也受相同授权代际规则，避免旁路；公共安装器/JWKS/health 不属于员工旧权限内容。 |

SSE 不要求完整缓冲响应；门禁按 chunk 工作。慢 writer 期间 C 等待该 writer 的可验证终止，不能仅调用 cancel 即返回成功。Write deadline 是可用性手段，不是跨进程终止证明。

## 6. 故障、审计、重启与可观测结果

- R 事务/audit 写失败：不宣称已撤销；数据库不可用期间新动作一律拒绝，记录可靠审计故障健康状态。数据库恢复前不能发内容。不得将外部日志视为已提交安全审计。
- R 已提交、HTTP 断连/实例崩溃：撤销继续存在，不回滚身份；重试按同一 operation 查询/推进，不能恢复授权。C audit 失败保持未完成，旧权限仍拒绝。
- R 后 DB/network 分区：所有新 grant 申请 fail closed；已 dispatching 的执行者应取消，但数据库分区本身不能强制其 socket 停写。若无法确认停止，operation 保留 unresolved，不能宣称 C。其他不相交 Principal/Workspace 不被全局锁连带阻塞。
- 孤儿/重启：恢复器扫描未完成操作并核对 instance incarnation。只有原进程 join 所有 writer，或有可验证的进程终止和出口隔离证明，才能标停止；kill DB backend、重启另一实例、TTL 到期均不充分。运维仅人工改表不能构成证据。
- 外部已执行/未知：可靠记录 attempted、unknown、completed-before-cancel 或 confirmed-cancelled；保留真实 Usage/成本未知状态。取消 HTTP 请求不表示远端事务回滚。无幂等能力禁止自动重试未知副作用。
- 审计至少含 operation ID、actor、target、epoch、trace/request、R/C、拒绝/取消/unknown 原因与证明引用，严禁保存业务内容或凭据。R/C 与相应状态事务一致；发送前有持久 attempt 意图，发送后无法落终态时保留 unknown。告警暴露 unresolved 数量/最长年龄、audit 故障、不可证明终止的实例，不伪造成功计数。

## 7. 供独立评审的冻结公共协议与迁移

用户已决定强停止与“处理中→确认完成”；本节由架构 owner 冻结且经独立审查接受 wire 设计，无需再次询问同一产品选择。协议尚未实现；通过 TASK-065 实现/故障/客户端迁移验收之前不得宣称已上线。

### 7.1 版本、入口与统一授权

控制面统一使用 `EAP-Revocation-Version: 2`。以下新增入口只支持版本 2：`POST /enterprise/v1/revocations`、`GET /enterprise/v1/revocations/{operation_id}`。POST body 为 `{"target":{"kind":"key","id":"..."}}`；kind 固定为 `key|user|membership|device_family`，membership 必须另含 `workspace_id`，id 为 user ID。所有 ID 由 canonical store 查验，不从 body 建立权限。禁止一请求混合多个目标；user 撤销按服务器侧目标集合覆盖全部凭据/委托。

既有 `DELETE /enterprise/v1/keys/{keyID}` 在 version 2 下为 target=key 的等价适配器；device/logout 在 version 2 下由 refresh hash 查族后适配同一 operation，其旧 body 不变。user/member 新撤销统一用 POST；未来任何用户禁用/成员移除 writer 必须调用同一 R/C 服务，不允许旁路单独返回成功。首次 device logout 可凭仍可识别的 refresh hash 证明族所有权发起撤销，但响应仅 operation ID/状态、不返回用户/凭据详情；后续 GET/重试需仍有效的独立控制面 session 或管理员，不让已撤销 credential 继续授权查询。

除上述 device/logout 仅凭 refresh hash 的窄化族撤销适配器外，POST/DELETE 必须有经过实时 canonical 验证的控制面 session；key/device 目标允许本人或平台管理员，user 目标仅平台管理员，membership 目标允许该 Workspace 管理员或平台管理员。权限不能由请求声明，现有 roles/Policy 映射缺失时 fail closed。GET 每次要求有效控制面 session，且为该 operation 的原发起人或当前有目标管理权限者；用户已禁用时由管理员接管。非授权与不存在统一 404；管理员查询仍按其管理范围限制。员工 `agk_*` 不能调用控制面通用撤销接口。所有响应 `Cache-Control: no-store`，不暴露 refresh hash/密钥、prompt 或 connector endpoint。

### 7.2 幂等、状态与完成点

所有 mutation 必须提供 UUID `Idempotency-Key`，该 UUID 就是 `operation_id`，不依赖响应才能恢复。服务器将 ID、actor、canonical target、目标 epoch 与语义版本绑定；同 ID/同语义重试返回同结果，不重复 R、审计或远端副作用；同 ID 改 target/actor/版本返回 409 `idempotency_conflict`（不披露原 operation）。目标已有 pending/unresolved 撤销时，新 ID 被可靠关联到同一 canonical operation，Location/body 返回 canonical ID；权限检查先于复用，不能用它探测别人的目标。

R 事务原子写目标禁用、epoch、operation pending 与 requested audit；同一目标的新 grant 登记与 R 串行。只有确认 R 已提交后可返回 202。响应头 `Location: /enterprise/v1/revocations/{canonical_operation_id}`、`Retry-After: 1`、`EAP-Revocation-Version: 2`；body 固定字段如下（时戳为 RFC3339，completed_at 未完成为 null）：

```json
{"operation_id":"<uuid>","state":"pending","revocation_effect":"applied","requested_at":"<timestamp>","completed_at":null,"reason":null}
```

`state` 固定 `pending|unresolved|completed`，`revocation_effect` 对已持久化 operation 固定 `applied`。pending 文案是“新授权已阻断，正在停止在途执行”，unresolved 是“新授权仍被阻断，尚不能确认全部停止，正在处理异常”；都不声称全部已停。GET 返回 200 同 schema，unresolved 的 reason 仅允许 `stop_unconfirmed|audit_unavailable|instance_unreachable|remote_unknown`，不泄漏细节；完成必须有非 null completed_at 和可靠 C audit。前台不自行按时间跳到 completed。

C 必须同时满足本地 writer/业务委托终止、全部相关远端 execution 的停止或已结束证明、无恢复执行/新副作用能力、completion audit 与 operation 状态原子提交。只有确认 C 提交后，mutation 首次或重试才可返回 204（无 body），并保留 Location、版本头与 `EAP-Revocation-State: completed`。GET 的 completed 与此 204 完全同义。重复撤销已 completed 的同一 target epoch 返回其 canonical operation 204；未知目标为 404。用户重新启用/重新入组是新 epoch，新的撤销不能复用旧 completed。Key/设备族不得撤销后复活。

数据库提交结果不明或超出请求预算：返回 503 `revocation_outcome_unknown`，body 含客户端 operation_id、Location，**不是 202，也不声称仍 active 或已完成**。客户端使用相同 ID 重试或 GET。GET 在可访问权威库且存在记录时返回真实状态；不存在为 404 `operation_not_recorded`，只证明当前未查到，不证明未提交/权限有效；客户端重试相同 mutation，通过唯一约束与同目标串行化等待原事务结算再创建或复用，不能换 ID 重放。数据库不可用 GET 返回 503，绝不返回缓存 completed。明确已回滚的输入错误按 400、无效控制面认证按 401、目标不存在/无目标权限按 404；存储错误仍 503 且不可推断权限状态。

接口处理预算冻结为 5 秒（包含写库/读取状态；无法完成则以上述 503 返回），不等待远端停止到满预算；R 确认即可返回 202。停止观测期限冻结为 R 后 30 秒，期间有明确失败可提前 unresolved；到期无全部证明则原子记录 unresolved/原因及可靠故障事件并告警，继续恢复，绝不超时清证据或恢复权限。数据库故障使这次状态写不能落库时，以健康故障告警、GET 503 表达，不造已落库记录。30 秒是运维升级阈值而非允许旧执行继续的宽限期：R 后立即发起 cancel，收到终止证明前始终未完成。恢复完成需同一 C 条件；unknown 外部副作用不自动重试。

### 7.3 切流、旧客户端拒绝与回滚

阶段 0：目前旧 DELETE 204 仍仅代表 revoked_at + audit，重复 404；明确不具备强停止保证。阶段 1：部署兼容版本，新增 version 2 operation 协议并升级 Portal/CLI/SDK，旧接口仍保持历史语义但整个环境不得宣称强保证；用实际客户端确认 pending/unresolved/completed 及 503 重试。**阶段 1 的 v2 completed/204 同样只能在该目标全部相关执行纳入 v2 登记、所有本地与远端停止证明以及 C audit 齐全后发出；若旧实例/旧执行未登记或无法证明覆盖，保持 unresolved。旧版 204 永远不能借作 C，也不能用“v2 表内计数为零”推断未登记执行已停。** 阶段 2：经全部 connector/多实例门禁通过并确认客户端迁移后，平台统一切到强保证模式。所有撤销 mutation（Key、device、user、member）缺版本头返回 409 `revocation_contract_required`，支持版本头列为 `EAP-Revocation-Supported: 2`；未知版本返回 400 `unsupported_revocation_version`，均在目标 mutation 之前拒绝。新 API 从阶段 1 起即严格要求版本头。不得根据 User-Agent 猜测兼容，也不得以任意 2xx 视作完成。阶段 2 没有绕过版本的旧 DELETE/logout 204 路径。

切流由部署配置控制且写可靠 change audit；启动/readiness 检查同一集群全部可写实例具备 v2 ledger 与门禁，旧实例必须摘流并证明无在途 writer，不能新旧协议混跑后宣布强保证。已签票据/旧 access session 须完成 lineage 迁移或失效并重新认证，不能靠 TTL 等待假称门禁完备。

回滚只允许到理解 v2 operation/epoch 的兼容构建；不得把 pending/unresolved 改回 active，不得恢复旧客户端接受。若没有可用兼容构建，停止受保护流量并继续由兼容恢复器处理 ledger，不能部署旧 binary 忽略记录。历史 v1 撤销记录迁移标记为历史状态，绝不合成 C/停止证明。本文冻结的是已接受的协议设计，不在本卡修改运行端点/数据库。

### 7.4 当前 connector 能力清单与 no-go 门禁

本地证据范围为本分支生产装配、执行代码与种子配置；未读取生产数据库、未调用真实厂商服务，不把未探测的部署配置当事实。`ProviderTarget` 只有 Name/BaseURL/CredentialRef，`RegisteredTool` 只有 endpoint/method 等字段，均没有 execution/cancel/terminal-proof 契约。所有未知新增配置默认不合格，不能靠名称推断能力。

| 实际路径/可追溯证据 | Execution ID / cancel /终态证据现状 | 分区行为与判定 |
| --- | --- | --- |
| 主生产 Chat 非流，Responses/Messages 非流复用：`main.go` 装配 OpenAICompatClient；`openai_client.go:Chat` | EAP RouteAttempt 不是远端 execution ID；返回 ChatResult 无用于终止的远端 job handle。只有 NewRequestWithContext/Body.Close，无远端 cancel/query。普通 HTTP 200 不证明无异步后续工作。 | 本地 ctx 结束仅证明本地调用返回；远端未知。**no-go**。 |
| 三协议流：`stream_client.go:ChatStream/GenerateChatStreamRouted`，`protocols.go` 转换 | buffered chunks、context 和 Close；`[DONE]` 是流结束信号，未验证关联 execution 已终止及无后续效果；无 cancel ACK/query。 | 流断开不能证明推理停止；队列与 writer 尚须 join。**no-go**。 |
| embeddings：`stream_client.go:Embeddings`，`protocols.go:handleEmbeddings` 直接客户端 | 返回 []float64，未保留远端 execution ID，无取消/终态查询；受影响 Knowledge 远程路径不得复用旁路。 | 请求断开/返回向量均非经验证停止协议。**no-go**。 |
| 种子 LLM `demo-primary/demo-backup`：`migrations/000004_provider_governance.sql`，映射 daily-fast/coding-high | 使用上述相同 HTTP adapter；种子名称/URL 只证明配置，未提供远端停止证明能力。 | primary/fallback 分别必须登记，fallback 也 no-go；不能因 stub 就算生产合格。 |
| MCP 两入口：`main.go` 的 HTTPExecutor，`mcpgateway.go:HTTPExecutor.Execute`、`capability/mcp_source.go:Execute` | 只将参数替换到 HTTP endpoint，返回 RawMessage；无远端执行 ID/cancel/terminal query。JSON-RPC ID 不是远端作业控制标识。 | GET/read-only 仍可能在远端运行；网络错误不证明停机。所有 endpoint methods **no-go**。 |
| MCP 种子 `gitlab:git_read_file` / `eap:internal_data_read`：`migrations/000003_mcp.sql` | 都是 HTTPExecutor GET，endpoint 为开发占位；无协作终止协议。 | 不因风险 R1/只读而豁免强停止；**no-go**。 |
| legacy Kernel：`kernel.go:GenerateChat/StreamChat`，`llm.go` 保留 fallback 分支 | Bifrost context 取消不提供 EAP 可验证远端停止证据；`main.go` 当前只记录 env fallback，并未将 Kernel 赋值到 srv.LLM。 | 非当前生产装配但防旁路保留审查；启用即 **no-go**，不能补配绕过。 |
| Knowledge 当前 HashEmbedder / Skill Capability | `main.go` 的 HashEmbedder 是本地计算；`capability/skill_source.go:Execute` 返回安装信息/票据，明确不在平台执行 Skill。 | 不虚构远端任务；仍必须证明本地 writer/job 停止。离线已下载内容不属于可撤销远端执行，不能声称已追缴。 |

**当前结论：没有任何本次核对到的远端 adapter 能证明强停止；当前生产装配是强保证部署 no-go。** 这不是关闭现有生产流量的执行指令；TASK-065 必须实现下述 admission，运营切流必须在通过后进行，不能以文档代替实现。

合格 connector 契约冻结为：dispatch 前持久 EAP attempt ID，远端接受同一幂等 execution ID 或在不可丢失的受控接收协议中建立映射；即使响应丢失，也能以该 ID 查询/取消。Cancel 必须幂等且有已认证确认；terminal proof 绑定 execution ID、远端 incarnation/代际、不可恢复的终态及停止后的副作用封闭性。禁止仅相信取消请求 200、“请求已接收”、TTL、TCP 断开或操作者勾选。远端已在撤销竞态前自然终态的证明可计入集合，但 R 后必须主动取消，不能故意等工作自然完成来规避强停止。

TASK-065 对每个 provider target / tool connector 保存受版本控制的能力 profile（adapter 版本、endpoint/config 摘要、执行标识/取消/查询能力、独立故障测试证据）。profile 是执行资格，不是新身份权威；仍实时验 canonical auth。admission 在每次实际 dispatch（包括 fallback/retry/redirect）查 profile，缺失、过期、配置摘要不符或 proof 模型不全一律 503 `revocation_stop_unsupported`，在发送任何业务字节前拒绝；不能仅启动时检查。停止/查询 endpoint 只由服务器 profile 指定、最小权限服务凭据访问，不接收客户端任意 URL。profile/config 变化使旧资格失效，旧 attempt 仍绑定原 profile 并保持恢复能力。非协作远端既有 unknown 保留 unresolved/告警，不能因禁用新的 admission 而清零旧执行。

多实例门禁不能靠连接丢失证明 socket 关闭；connector 强制隔离若仅阻止新 egress，还不能证明远端已有工作结束。新增远端协作协议或可证明终止其工作的部署方案须经独立架构审查和运行验证。本卡冻结拒绝条件与证明义务，不批准新 foundational service，不预选或宣称厂商已有取消 API。若所有候选均无合格能力，强保证环境须保持该能力不可调用；若这与必需业务能力冲突，再以具体证据升级产品矛盾，不能作者自行豁免。

## 8. 验证矩阵与实施顺序

以下是 TASK-065 必须新增的并发测试要求，**本 ADR 没有运行这些未来测试**。使用两个真实 Gateway 实例、共享 PostgreSQL、可阻塞并记录实际写入的 transport/receiver，事件序号与屏障建立 happens-before；不能只比较机器时钟。每个断言都记录 R/C/实际发送/接收/远端副作用，明确各自边界。

| 场景 | 必须观察到的结果 |
| --- | --- |
| pending/停止证明/审计三阶段 | HTTP 不等待全部远端而有界返回处理中；所有停止证明齐全但 C audit 失败仍不可 completed；恢复成功才完成。 |
| 非协作远端、停止期限届满 | 未证实停止就 unresolved + 可观察告警，权限持续拒绝，不清孤儿、不假报 completed；强保证部署拒绝未具备停止能力的新调用。 |
| A 入口验权后暂停，B 撤销，再恢复 A | 不发生新 dispatch/release；若 A 持有发送中的记录，B 不虚构 C。 |
| 三种 SSE 逐 chunk 撤销 | DLP 尚未释放尾部被丢弃，无 C 后 Write/Flush；慢 writer 真正停止后才 C，不能仅 test context cancelled。 |
| 非流 Chat/Responses/Messages/embeddings provider 返回时撤销 | JSON 未开始则拒绝，已开始则断开；包括 fallback、legacy、缓存命中。 |
| MCP 两入口 + JSON/SSE | 排队/审批/重试不得在 R 后取得新发送；远端已接收后延迟副作用必须显示不可撤回/unknown，不能假称 canceled。 |
| Knowledge egress/激活/引用 | 模型调用、发布事务、最终返回分别与撤销竞争；无 C 后激活/释放；他人 Workspace 不受影响。 |
| Key/user/member/device 组合 | 单 Key 只影响自身；用户禁用覆盖所有族/票据；成员删后重加旧 epoch 仍失效；refresh 与 logout 并发不能 mint 活跃后继族。 |
| 断客户端、慢写、暂停进程、断 DB 连接但保留 socket | C 前有停止证明；恢复暂停进程不能旧 permit 续写；移除 DB lock/TTL 不会解除 unresolved。 |
| R/C/audit 各事务故障、HTTP 成功响应丢失、重复 DELETE | commit unknown 不冒称回滚；重试不复活身份，不重复副作用；返回遵循批准的版本契约。 |
| 崩溃/重启/滚动升级 | 未完成 ledger 保留、旧 binary 禁止旁路、新实例不能清孤儿；恢复证明可靠可审计。 |

实施唯一 owner 与顺序：TASK-064 接受全平台决策 → TASK-056 修订 Knowledge 契约并独立审查 → TASK-065 实现共享 authority/执行/出口/设备/协议迁移及各现行路径回归 → TASK-057 消费共享门禁实现 Knowledge-specific generation/source 检查；TASK-057 不可另造 revocation authority。TASK-065 与 TASK-056 可在共享决策接受后分别推进，TASK-057 同时等待二者。TASK-059 验证知识业务，不能替代 TASK-065 的全平台多实例安全验证。

最窄本卡验证为 registry validate、文档链接/差异检查和固定 SHA 的独立架构审查。未改生产代码，不把整仓 Go suite 当成新撤销语义证明；实现时从上述矩阵对应集成测试开始，再执行受影响 identity/protocol/failure/security gate 和仓库 verify-foundation。

## 9. 审查记录

2026-09-25：root 转达独立 GPT-6 Sol 对固定提交 `ab8edf2a290c379bdd6e31ff12d3fa8ff51339b2` 的审查：**保持 Proposed，TASK-064 未完成，等待产品/公共协议选择**。审查要求修正 R 的表述：R 后无新 grant 不等于此前获准且与 PG 分区的 writer 已停止物理发送；未有停止证明时 C 不得完成，R 时强制停止需要出口 fencing/协作接收方。本次文档按该意见修订 §3/4/6。该次审查早于后续用户决定；OS handoff/远端效果的证据限制现由 §3/7 明确，不将先前审查冒称新方案通过，亦无继续运行的例外。

Sol 补充核对了 `protocols.go:openSSE` 的 event/data 分次 Write 与 Flush、Responses/Messages 各自流与非流路径、embeddings 直接 provider 路径，以及 `mcp.go` tools/call 的 JSON/SSE 返回；§5/8 的门禁必须覆盖每次实际写，不能只包围一次 emit 调用。

上述早期审查时尚无 accepted 决定；后续以本节最终结论为准。当前没有运行实现，未批准生产上线；root 负责集成验证与注册表完成记录。

本轮用户澄清记录：root 先转达“Key 吊销后所有 LLM 和 MCP 在途执行必须停止”，随后转达已接受“处理中 → 确认完成”。仅拒绝新请求的方案 C 已排除；处理状态语义已决，不再标为等待用户回答。HTTP 202/operation 与旧版 204 的具体迁移由架构 owner 在 §7 冻结，随后经下述独立审查接受；用户决定本身并不是停止机制已实现的证据。

二次 Sol 复审 `6737ee0` 的阻断处置：本次 §7 补齐 wire/授权/幂等/commit-unknown/切流/回滚、当前 connector 逐类证据与 admission no-go；§3 明确 R..C 仍可能运行，仅 C 是强停止完成。该次补充形成固定 `2af93c9` 送审，最终结论如下。

Luna 独立只读能力盘点与本次本地检查一致：`main.go:205-213,230,238-242` 的 OpenAICompatClient/HTTPExecutor 均无 outbound execution ID、cancel endpoint 或远端终态查询；`openai_client.go:159-191`、`stream_client.go:32-35,61-93,152-169`、`mcpgateway.go:121-161,314-316` 仅有 request context/Body close。Bifrost Kernel 未在当前 main 赋值到 Server.LLM。该盘点是代码事实，不是新增停止机制的验收。

**最终设计审查结论（2026-09-25）**：root 转达独立 GPT-6 Sol 对 `2af93c9fdbe2814284c31150fa72553545f80740` 的复审，接受 ADR-0030 作为“冻结边界/协议及明确生产 no-go”的设计决定。非阻断建议为：明确阶段 1 completed 必须覆盖全部相关 v2 登记与停止证明、不得借用旧 204；依据 ADR-0010 明确本地副本与后续在线授权的边界。本次已作上述澄清，未变更安全保证。TASK-064 可在 root 集成验证后完成并解锁 TASK-056/TASK-065；当前远端 adapter 仍全部 no-go，TASK-065 和生产门禁必须用运行证据兑现，不能以 Accepted 字样绕过。
