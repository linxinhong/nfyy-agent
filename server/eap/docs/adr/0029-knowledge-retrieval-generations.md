# ADR-0029：受治理知识检索、generation 与来源契约

日期：2026-09-25。状态：**Accepted — 仅设计与契约；真实 Provider 与生产运行仍受 no-go 门禁约束**。任务：冻结知识检索与复用契约（TASK-056）。

本决策有意替代 ADR-0012 的同步事务内 embedding、删除旧行和 cosine score 规则；本卡仍须 root 集成并完成；TASK-057/058 只能在全部注册表依赖（包括 TASK-065）完成后解锁。TASK-060/063 已按独立实验/设计范围完成，不追溯要求消费本候选。TASK-061 仍需实验后独立的 Wiki 生产接入 ADR。本卡不改变运行代码。编号通过全仓 `rg --files --hidden` 核对，当时既有最大 ADR 编号 0028（后来新增 ADR-0030/0031；历史编号不重写）。

## 1. 决策及权限

继续使用现有 Gateway、PostgreSQL、Policy、DLP、Audit、Provider credential resolver，不新增运行服务。Knowledge 是 Gateway 进程内模块；逻辑执行接缝位于 `packages/knowledge`，Provider 执行及配置位于 `packages/llmkernel` / `packages/governance`，装配在 `apps/gateway/main.go`。禁止 Knowledge 直接以任意字符串调用 `OpenAICompatClient.Embeddings`。

Gateway 每请求由 `requireAPIKey`、`selectWorkspace` 得到 `identity.Session`，验证当前凭据有效与 membership；不得用 body 中 workspace/user/title 建立授权。兼容现行员工 key：查询、上传仍允许当前 Workspace 成员发起，再分别执行 `knowledge.read` / `knowledge.write` Policy 决定；不新增 key scope 必填条件。`Principal.Scopes` 当前没有员工 key 持久化/赋值来源（`auth.go:principalFor`、`store.APIKey`），因此不能用这个空字段拒绝全部旧 key，也不能默默写入全权 scope。重建/人工激活另要求服务器读取的 `session.Workspace.Roles` 包含 `owner` 或 `admin`，并执行 `knowledge.reindex` Policy；全局角色不替代 Workspace membership。常规上传完成后的内部激活沿用该上传的 `knowledge.write` 权限，不要求普通成员额外获得 reindex 权限。

Policy 接缝沿用 `PolicyInput.Action` 和现有 TablePolicy/效果词汇。当前 TablePolicy 不匹配 Action，因此 TASK-057 必须在已有 `policy_rules` 表新增 `action text NOT NULL DEFAULT '*'`，旧行回填 `*` 保持现行匹配，再让 TablePolicy 同时匹配 Workspace、model_alias、action（现有优先级规则不变）。无模型的 knowledge 动作 Model 为空串，只匹配空串或 `*`，不能冒用员工提供的模型；`llm.embeddings` 则使用服务器固定 generation alias。独立动作规则可收窄读/写/重建，规则载入失败和未知效果拒绝；没有新增匹配规则时沿用现有 TablePolicy 的 no-match allow，但必须已经通过对应成员/角色门槛，不能视为匿名允许。控制面输入/读回与审计同步支持 action，并回归既有 LLM/MCP 规则匹配，禁止创建第二 Policy 引擎或另立授权表。

知识权限不自动授予外部模型权限，每次 embedding 还需服务器选定 alias 的模型授权与 `llm.embeddings` 决定。background 作业每次尝试和最终激活重新校验操作者/key 仍有效、成员/角色及 Policy 仍允许、来源未撤销。服务身份只允许执行绑定 job，不替代被委托用户权限。未来细粒度 key scopes 需要单独身份迁移设计，不在此卡悄悄扩展。

同进程传递服务器创建的 Session，禁止从 JSON 反序列化成可信身份；跨进程仅在未来单独部署获批后使用 audience=`llm-gateway` 的短期内部 JWT，验证 iss/aud/exp/jti，并携带作业的受限委托信息。员工 key 不传下游。已有 `commonProtocolAuth` 的签发/验证并不等于完成 Policy 或 DLP。

## 2. 冻结执行接口（语义签名，TASK-057 实现）

```text
GovernedEmbedding.Execute(ctx, {
  Session: server-verified identity.Session,
  Execution: shared execution handle with credential lineage and canonical epochs,
  Operation: knowledge.document | knowledge.query,
  RequestID, TraceID, JobID?, AttemptID,
  GenerationID, ConfigDigest, ExpectedGovernanceSnapshot,
  Inputs: [{Ordinal, Text}],
  Deadline, MaxInputBytes, MaxTokens, MaxCostMicros?
}) -> {GenerationID, ConfigDigest, Dimension,
       Vectors: [{Ordinal, Values}], Usage, ReceiptID}
```

Execution 由 ADR-0030 的 BeginExecution 创建，不能从 body/source job JSON 建立；凭据种类沿用现行 key-only 路由，未来 device 支持另经协议授权。文本、Session 只在进程内/受认证内部传输，不进日志。调用方不能覆盖 Provider target、credential reference、模型版本或维数。Session.Workspace.WorkspaceID 必须等于 generation、source、job 的 Workspace。接口从 generation 解析不可变配置；独立检查当前模型授权，不依赖调用者已检查。返回必须恰好匹配输入序号，拒绝缺失/重复、非有限数、零向量、维数错误及配置不匹配；不落部分向量。2 MiB/512 chunks/1200 字符/150 重叠沿用现有限额，但超过 chunk 上限必须拒绝而非截断。Provider 每批最大 16 chunks，单次 30 秒、作业总 10 分钟，可调低但不得绕过更低 Provider/Policy token 和预算上限。

不可变 `EmbeddingConfig` 必含 alias、provider ID、server target ID、provider model、可验证 model revision 或管理员显式发布的不可变 deployment revision、dimension（1..2000）、预处理版本 `eap-text-v1`、chunker `eap-fixed-rune-1200-overlap150-v1`、normalization=`l2`、distance=`cosine`、egress classification。digest=这些字段按键排序 UTF-8 JSON 的 SHA-256（不含 secret 值；credential ref 仅作服务端引用，轮换密钥不改变向量空间）。生产不设默认真实 Provider 或模型；配置不完整、未资格验证即拒绝激活。alias 路由改变只创建新 generation，不能改旧配置；同维数不证明同空间。Provider 无 revision 时由管理员发布 deployment revision 并执行固定 canary；漂移发现停用查询/重建，不自动相信旧向量兼容。stub/hash 配置 mode 与 semantic mode 分离；Hash 固定 256 维，仅显式离线。

Policy deny/error 直接拒绝；require_approval 无获批且绑定当前输入/配置的续接机制时拒绝；local_model_only 必须选择该 generation 已固定且服务器标明 local 的目标，否则拒绝，不能临时换模型。allow_with_redaction 用现有 ScanRedact 语义；普通 allow 使用 ScanEnforce。入库文本、query、实际 provider-bound 文本各过当前 DLP；原文查看豁免不能免除 Provider egress。引用内容返回前重扫，deny 用现有 withheld 行为、redaction 返回脱敏内容；title 与来源显示字段同样扫描，禁止经引用元数据漏出敏感值。新上传 title 任一 DLP 命中或扫描失败均拒绝，不能先脱敏再作为 source 自然键（会误合并不同标题）；通用错误不回显标题/命中细节，审计 Resource 用内部 source/request ID。历史标题读出只改安全显示，不改变内部来源定位。审计只存 ID/计数/决定/安全错误码；不能记录标题、query、原文、模型输入输出、URL secret 或 Provider 错误原文。

`Usage` = `{tokens_known, prompt_tokens?, total_tokens?, pricing_known, price_version?, cost_known, cost_micros?, currency?, provider_request_id?}`。未知 token 为 null/absent 且 tokens_known=false；未知价格 pricing_known=false；任一未知则 cost_known=false，不能把 0 当作已知免费调用。TASK-057 扩展现有 `usage_records` 和 `governance.UsageRecord` 的 token-known 标记；历史记录不凭猜测回填 known。每个实际尝试有唯一 AttemptID 和 ledger key，重试不覆盖前次未知/失败成本；外部调用可能计费而结果未收到时标 unknown。不修改公共 `/v1/embeddings` 响应形状来交付此接口；现有公共路由治理缺口另由 TASK-057 在该入口复用同一治理执行接缝补齐，协议扩展与真实客户端资格验证仍按既有规则。

## 3. 消费共享撤销门禁的提交、egress 与恢复顺序

上位安全契约是已 Accepted 的 [ADR-0030](0030-platform-revocation-and-inflight-boundary.md)，接受的是设计/协议与生产 no-go，非运行验收。用户已决定在途 LLM/MCP 必须停止并接受 pending → completed；本卡不再询问或重释该决定。R 是 canonical 撤销/epoch+requested audit 提交，R 后拒绝新授权动作并主动取消旧执行；C 只有本地 writer、内部委托及相关远端执行全部有停止/不可恢复终态证明，且 completion audit 提交后成立。R..C 可为 pending/unresolved，不能称全部已停。

**删除旧候选的 Knowledge 全局 advisory 56001/56002、独立 dispatch pool/permit、阻塞 R 的 orphan trigger 方案。** TASK-065 独占 canonical authority/epoch、共享 execution ledger、实际 Dispatch/Release、CancelAndJoin、CompleteRevocation、DB role/写权限及 revocation wire 迁移。TASK-057 只消费下面接口并增加 source/generation 业务约束；不得建自己的 revoke operation、dispatching/stopped 状态或以 Knowledge job 完成代替共享执行停止。名称为语义接缝，由 TASK-065 的实际导出 API 映射，不要求复制同名实现。

| 知识动作 | 共享接口及附加业务条件 |
| --- | --- |
| 上传、查询、重建/恢复 | `BeginExecution` 登记服务器 credential lineage、Principal/Workspace、全部 canonical epoch 与 action；job 仅引用其 execution ID。恢复需重新验证仍有效的委托，不能反序列化旧 Session 当授权 |
| 每次 document/query embedding attempt | `Dispatch` 查当前 authority、受版本约束的 connector profile、实际网络写与远端 execution 映射；重试/重定向/fallback 每次均重新登记。知识模型配置固定，不允许跨模型 fallback |
| 激活/更新/删除/回滚 | `CommitMutation` 在**同一 pgx.Tx** 中将 canonical epoch 复查与业务 source/index CAS、发布 audit 提交；不得先另连接验权后再开业务事务 |
| 引用、上传201/重复上传、缓存命中返回 | `Release` 管住实际 Write/Flush；权限失效未发 header 则失败，已写一部分则停止并 join，不为了补全 JSON 继续发内容 |
| 撤销/取消/超时/崩溃 | `CancelAndJoin` 和共享 recovery 保留本地、远端证明/unknown；Knowledge 无权通过 TTL、DB session 消失、stub success 清理共享 ledger 或宣告 C |

当前 `OpenAICompatClient.Embeddings` 缺 execution ID/cancel/终态证明，仍为 ADR-0030 §7.4 的 **production no-go**。每个目标 profile 必须绑定 adapter 版本、endpoint/config 摘要和独立故障资格；缺失/过期/不匹配或证明不完整，Dispatch 在任何业务字节前返回 503 `revocation_stop_unsupported`。配置固定/真实向量质量好/HTTP 200 均不替代停止能力资格。当前无合格远端时可以完成离线/stub 实现验证，但远程语义 generation 不能生产激活；不自动切 Hash 掩盖此拒绝。需新 connector 协作协议的工作归共享资格设计，不在本卡批准新服务。

冻结处理顺序：

1. Gateway 验证当前 key/成员，BeginExecution 登记与 R 串行；知识 Policy、来源读取权限、文本 DLP 和 generation 检查完成后，通过共享执行事务写 attempt 意图与 `knowledge.embedding.started` 安全事件，再允许 Dispatch。未提交持久意图或审计失败则零 Provider 调用；started 不是网络已发送或一次可缓存 permit。日志/audit 只含 ID、计数、配置摘要、安全结果码。
2. 调用远端在事务外，完全通过共享 Dispatch（含实际 writer 与远端 execution ID 的无丢失绑定）。R 后主动停止；本地 context canceled/连接断开/Body.Close/写完请求体均不是远端停止证明。暂停或分区后不能续用旧 permit；未证实停止则共享 operation unresolved/告警，**不延迟或回滚已提交 R**。本卡不另设全局 gate，不让一个 Workspace 的慢模型阻塞其他不相交主体。
3. 返回向量先验证完整序号、维数、有限值、配置；短事务写知识 attempt 结果、Usage、共享 execution/receipt 关联与完成审计。结果接收 receipt 仅证明该结果/审计已持久化，不是 R/C 终态证明；终态证据由共享 connector profile 验证。审计/DB 提交不明则不发布、不返回内容；共享 unknown 保留供恢复，不能声明零成本。R 已发生即丢弃新向量用于发布的资格，即使 provider 恰好自然完成也不能发布。
4. 所有构建先写 staging。最终调用 CommitMutation：按共享 canonical target 固定顺序锁验 authority/epoch 后，在该事务中锁 Workspace index，再按 source_id 顺序锁 sources；检查 expected_source_epoch/version、generation/config、完整 chunk receipt、当前 Policy/DLP/source 权限。业务写与 security audit 同事务完成。所有写 source.active_version/deleted_at/lineage可见性/source ACL 或新增 source 的路径，都同时递增 source.epoch 与 workspace_index.epoch；取消未发布 job 至少递增对应 source.epoch，使旧 job 的 expected_epoch 失配，防陈旧发布。R 先提交则 epoch 检查失败；业务提交先于 R 的内容仍须在返回时过 Release，不能据此复活旧委托。
5. 查询快照 active generation，受治理 query embedding 后在 SQL 内以 Workspace+generation+活动 source version/ACL 过滤；Release 前经共享授权门禁，并核对捕获的 index/source epoch。变化则丢弃候选、至多重试一次（新授权动作），仍变化 conflict；先可靠提交 knowledge.read/DLP结果计数 audit 才送内容。Release 的整个 writer 生命周期继续受共享停止门禁，不把一次 SELECT 当作发送屏障。R 后授权失效不释放内容；分区旧 writer 停止未证明时共享 C 不得成立。
6. Provider 成功而结果/激活落库失败、进程重启、竞争上传、取消/删除与模型返回同时发生：均保持 staging 不可见或旧 active pointer 不变。知识 job 可在共享恢复已确认旧 attempt 终态且仍有新授权后创建新 attempt；未知外部执行不自动重放。同一 job/source/hash/generation 的已持久成功 receipt 可以复用计算产物，但必须重新 BeginExecution/Policy/DLP/CommitMutation，不能复用旧授权。成本去重用实际 attempt ID，不能把可能重复计费的远端调用当作 exactly-once。

### 3.1 治理快照与控制写的原子边界（本轮补充契约）

ADR-0030 只冻结 canonical 撤销门禁，**没有现成承诺 Policy/DLP/config 的原子快照门禁**。本节定义消费方所需的共享扩展；TASK-065 是共享 API、治理版本锚点迁移、控制 writer 强制入口与执行边界的唯一 owner，TASK-057 是 Knowledge Policy action、generation/source 及调用适配 owner。两者接口未对齐/未验证时 TASK-057 不进入生产，不能把下述方法当作已存在。仍使用同一 Policy/DLP/Provider 事实表，不新增策略引擎或身份来源。

`ExpectedGovernanceSnapshot` 是服务器读得的不可变 `{global_revision, workspace_id, workspace_revision, target_id, target_config_revision, policy_digest, dlp_digest, embedding_config_digest}`，其中 revision 为单调整数，digest绑定被计算的完整规则集/配置。全局/wildcard规则、Workspace规则、DLP规则和词典、Provider local-only/target/route及embedding配置的**新增、删除、更新**都必须使相关 revision递增；因此新规则 INSERT 和 DELETE不会被“只锁已读规则行”漏掉。global revision覆盖全局影响；Workspace revision覆盖明确Workspace规则；target revision覆盖服务器目标/路由配置。尚无法可靠缩小影响范围的写升级为global影响，不猜测归属。revision锚点只是canonical配置的版本与并发控制元数据，不能独立授予权限。

共享补充接口：

- `ReadGovernanceSnapshot` 在一致性数据库快照读取相关revision与规则/配置，返回绑定digest的只读快照；Policy/DLP据此计算，不能混用缓存中不同版本。过期或读失败不给默认allow。
- `CommitMutation(execution, expectedSnapshot, callback(tx))` 在同一短事务内按固定顺序锁canonical targets → governance revision keys（global/Workspace/target排序）→ Workspace index → source IDs；比较expected revisions/digests，执行Policy/DLP当前版本确认、业务CAS及audit。任何控制writer也必须先锁相同治理revision键，再变更事实表和递增revision并audit，确保重读到提交之间不能插入新规则/配置。callback只使用这个tx；版本不符不执行mutation。
- `Dispatch/Release(execution, expectedSnapshot, guardedWriter)` 必须将治理snapshot校验与**实际writer生命周期**置于同一共享串行域；控制snapshot成功切换不得穿过尚可能写出旧决策内容的guard。不能“事务读版本→解锁→普通http.Write”，也不能将取得grant视作实际发送。规则变更先于取得guard则版本不符、零新写，释放guard后重新Policy/DLP并至多重试一次；已发送部分响应遇到撤销/取消则停止，不重放。每个重试/redirect/下一次实际写都消费有效guard，不缓存许可。
- `ReplaceGovernanceSnapshot` 是所有受保护配置写的共享入口：先阻断受影响scope的新旧版本guard申请并cancel/join旧guard writers，证明不会再写后，才在上述短事务锁revision键、变更事实表、revision和change audit；提交即新配置生效点，解开scope后新请求只能取得新快照。不在等待writer期间持有数据库事务。其scope控制状态只是执行账本派生状态，由TASK-065已有共享框架承载；暂停/分区/连接丢失时不能仅凭TTL/PG session消失宣称排空。若不能证明旧writer停止或变更事务/audit失败，**配置仍为旧已提交版本**，控制写返回明确503并保持受影响scope拒绝新guard，恢复证据齐全才重新开放或完成变更；不返回“新规则已生效”。已先完成dispatch的远端计算不算仍在发送的guard，但结果Release必须重新取得当前snapshot。Key/user/member/device的R不等待这个配置排空步骤，继续ADR-0030的先拒绝后停止；不得恢复旧版阻塞R的问题。

DB必须强制使用共享入口：runtime role不是owner/superuser，禁止直接DML/DDL到治理事实表和revision锚点，只有最小权限受控写函数/执行入口可同事务更新二者；migration owner仅在受控维护/停流量窗口使用。现有 `Store.UpdateDLPRuleAction` 的pool单语句、词典新增/删除、route/Provider等writer全部迁移，不留“直接SQL自觉递增版本”的旁路。readiness检查有效grants、必需函数/约束/schema版本；原高权限compose身份不满足此条件。实现可选择等效的数据库强制方案，但必须证明所有普通runtime直写被拒绝、statement新增/删除不漏版本、旧binary不能绕过。

共享owner验收必须包含：A完成Policy/DLP后暂停、B新增deny/删除allow/改词典或Provider配置、A恢复Dispatch/Release/CommitMutation；B先成功则A旧snapshot零新写/零激活，A guard先占用则B不能虚报新配置已生效；断DB但保留socket不解除旧guard证据；失败/audit不可用不切revision；两个Workspace局部变化不阻塞无关Workspace。此处是验收义务，不是运行通过。若TASK-065不能提供该扩展，则明确保持知识远程生产no-go并提交共享接口修订，禁止TASK-057另造全局锁或Policy权威来绕开。

### 3.2 拒绝、故障与可靠审计

完成EAP入口身份验证后，每个知识Policy deny、require_approval无法续接、source ACL拒绝、DLP命中/扫描失败、快照/存储/Provider/profile失败，都必须在释放任何HTTP拒绝/失败响应前尝试同步可靠提交对应安全事件；普通业务拒绝只有audit提交成功后才能返回其准确拒绝码。事件记录完整服务器Principal/Workspace、execution/attempt/request/trace ID、action、来源内部ID（创建前request ID）、版本、阶段与安全reason code；不放标题、query、文档、命中片段、secret或原始Provider错误。来源不存在/无权限对外同一错误，不因审计详情泄露存在性。

身份尚未成立的请求沿用共享入口可靠auth-failure审计，不伪造Principal。Policy/DLP/source拒绝后零embedding egress、零激活、零内容；source读取ACL必须在取内容前完成。扫描失败先写 `knowledge.scan_failed` 安全码，再返回fail-closed错误；DLP受阻写deny事件后返回通用DLP码，不只在“有RuleNames且允许”时记录。

若audit本身不可用，**不可能承诺先持久化失败审计再返回**：明确走基础设施fail-closed错误，停止所有业务发送/发布/释放，不吞错误、不回成功或正常业务拒绝码；将audit失败计入可观测健康/告警并保留已存在的durable execution/started记录待恢复，不声称该次失败事件已落库。该基础设施错误响应只包含通用码/trace且不含受保护内容；它不是业务Release授权绕过。已开始响应则取消/join/断开，不补发success尾部。恢复时审计事件幂等关联原attempt，不能重放未知远端副作用。审计失败不得延迟已提交R或伪造C，与ADR-0030一致。

数据库上线前提归 TASK-065：bootstrap/migration owner 与 runtime role 分离，运行 Gateway 不持 owner/superuser 凭据，不以现有 main.go 启动自动 Migrate 作生产权限模型；readiness 校验共享 schema/version、角色不能绕过 authority/ledger、必需强制约束有效。TASK-057 的 pgvector 安装和表迁移用 migration role，runtime 只获必要数据/共享函数权限；pgvector换镜像不能回退为高权限 eap runtime 或移除共享 ledger。具体权限/强制机制以已验证 TASK-065 实现为准，本设计不冒称现状已满足。

## 4. 存储与 generation

TASK-057 新增下一可用编号 migration（不得编辑历史 000007），以下为冻结逻辑 DDL；具体 SQL 及索引由实现卡提交验收，不能更改关系/约束：

| 表/关系 | 必要列与约束 |
| --- | --- |
| knowledge_sources | `(workspace_id, source_id)` 主键；type=upload/derived；server-owned locator；active_version；epoch；deleted_at；upload title 在同 Workspace 唯一，派生来源不以 title 去重 |
| knowledge_documents（保留） | 现有 UUID/title/version/content_hash/created_by 保留；增 source_id；同 source/version 唯一；复合 FK `(workspace_id,source_id)`；ready 仅处理状态，不再单独决定可见性 |
| knowledge_generations | `(workspace_id,id)` 唯一；mode/config_json/config_digest/dimension；building/ready/active/retired/failed；配置不可变 |
| knowledge_workspace_index | Workspace 主键；active_generation、epoch；复合 FK 保证 generation 同 Workspace；至多一个 active pointer |
| knowledge_chunk_vectors | `(workspace_id,generation_id,document_id,seq)` 主键；`embedding vector`，维数由写事务检查 generation.dimension（1..2000）；FK 同 Workspace generation/document；内容沿用 chunks；lexical tsvector；不设全局 ANN |
| knowledge_jobs / attempts | source/version/expected_epoch/generation/actor/limited delegation/lease/state/attempt_id/receipt_id；唯一 job idempotency key；attempt 与 Usage 唯一关联；引用 TASK-065 shared execution ID/credential lineage（不复制授权/dispatch生命周期）；成功 receipt 不可伪造 |
| shared governance revisions（TASK-065） | global/Workspace/target的单调版本锚点与canonical治理写同事务；仅共享门禁拥有，Knowledge只引用ExpectedGovernanceSnapshot，不复制权威 |
| policy_rules / usage_records | 既有表分别增 action（旧行为默认 `*`）与 tokens_known（旧记录默认 false）；沿用同一 Policy 与 Usage 权威 |
| knowledge_source_lineage | derived source/version → original Workspace/source/version + locator/内容摘要；复合 FK、不得跨 Workspace；删除/撤销使派生记录不可见 |

旧 `knowledge_chunks.embedding double precision[]` 在迁移窗口保留，但新查询只读目标 generation vectors。source/version 与 Workspace 复合约束避免仅靠 UUID FK 将其他 Workspace 文档关联进来。向量维数、generation、ReceiptID 一起验证；不同 dimension 用无 typmod vector 列支持，不创建跨维度 ANN 索引。首版候选精确 cosine 与 PostgreSQL 原生 lexical 排名，SQL 内同 Workspace/active generation/active version/未撤销来源约束，禁止取全局 top-k 后过滤。索引/缓存 key 必含这些维度；权限不缓存。

文档更新在当前 active generation 内构建该 source 新版本；成功后原子变更 active_version，旧版本立即不匹配。generation 切换是 Workspace 级别：构建该 Workspace 所有仍活动 source versions 的快照，记录 index epoch；并发上传/删除先照常处理，最终激活发现 epoch 变化时增量追平或失败重建，不能发布缺文档快照。短激活事务锁 Workspace index 行，核对 snapshot_epoch=当前 epoch 和完整 source/version coverage、当前 ACL/来源及配置资格，再 CAS generation pointer 并递增 index epoch；所有 source 可见写均先锁该行，所以不能在 coverage 检查与 pointer 提交间漏进并发新文档。没有完成全 Workspace 重建不能混用新 query 向量与旧 vectors。

## 5. PG16、迁移与回滚

选择 `pgvector/pgvector:0.8.0-pg16@sha256:a132765ec351c65111b5b675928a3a0515a466a40f97277329db8b8209ad8bc9`，扩展 `vector` 版本 `0.8.0`。2026-09-25 以 `docker buildx imagetools inspect pgvector/pgvector:0.8.0-pg16` 实测 registry 返回该 index，含 linux/amd64 与 linux/arm64；不是凭记忆填 tag。上游 [v0.8.0 Dockerfile](https://raw.githubusercontent.com/pgvector/pgvector/v0.8.0/Dockerfile) 使用 PG_MAJOR 参数构建，实际 tag 为 PG16 产物。此处只确认镜像存在和目标 PG major；运行扩展安装、平台启动和恢复仍由 TASK-057 资格验证，不能从 manifest 推断运行通过。

现有 `postgres:16-alpine` 使用 musl；新镜像不得直接挂旧 volume 启动并假定排序/locale 兼容。预演和正式迁移走逻辑 dump/restore 到新 PG16 volume：备份权限/schema/data，记录原 PG minor、locale、collation，停止写入窗口，恢复到新镜像、`CREATE EXTENSION vector VERSION '0.8.0'`，校验 FK、行数、序列、索引、查询与 collation，保留旧 volume 和备份。TASK-057 记录 `SELECT version()`、`pg_extension.extversion` 和真实恢复时长。不使用 apt latest 或浮动 pg16 tag。

旧 ready 文档按 workspace/title 创建 source，保持现有 document UUID/version/seq，不把已删除历史版本虚构恢复。建立显式 `legacy-hash-256-v1` generation，将有效 256 维数组转 vector 并核对 Hash canary；坏维数/空向量隔离为不可检索且出错误清单，不能随意补零。每 Workspace 先用 legacy generation 提供原结果；真实模型 generation 在独立 staging 从现存经 DLP 再处理的文本重建。失败保留 legacy pointer。首次语义激活必须通过质量、治理、隔离验收；不自动以 Hash 降级掩盖 Provider 故障。

旧 generation/vectors 最少保留 7 天且清理前完成恢复演练；删除/权限撤销立即影响所有代际，保留仅用于故障恢复，不恢复授权。7 天后可审计清理非活动数据，不删除审计与未知 attempt 事实。**回滚不允许恢复陈旧 source version**：旧 generation 如果覆盖当前所有 active versions，经共享 CommitMutation 校验当前 authority epoch、来源及审计 CAS 回旧 pointer；否则在旧模型配置下补齐当前版本后再切换，不能把已替换旧内容复活。应用回退用支持新 pointer 的上一兼容构建，禁止旧 binary 直接忽略 pointer 扫历史 ready 行。数据库灾备回退需停写且声明恢复点/RPO，重放仍有效的删除/撤销，完成审计核对后开放；不通过 DROP EXTENSION/丢表快速回退。

## 6. 公共响应、评测与后续拥有者

逐字段兼容、score、来源、错误冻结在 [contracts.md](../knowledge-memory/contracts.md)；fixtures 位于 `contracts/schemas/v1/enterprise/knowledge.*.json`。`score` 不再支持旧 cosine 数值阈值；返回可选 score_kind 明示算法，只保证同一响应内排序，不跨模型/请求比较。现有 clients 未改字段即可解码，需在 TASK-058 的现有客户端 journey 中验证不以旧阈值丢结果。

评测集、不可调标签、Hash 基线、分组门槛和延迟协议在 [eval/README.md](../knowledge-memory/eval/README.md)。真实模型无法调用时记录 unverified，不以 stub 质量代替；实现可以通过故障 stub，但语义上线 gate 保持关闭。

后续 owning seam、拟复用文件与许可证见 [reuse-and-handoff.md](../knowledge-memory/reuse-and-handoff.md)。本卡没有复制第三方运行代码。

## 7. 独立审查记录

审查人：独立只读 reviewer `/root/contract_review`（GPT-6 Sol），由 integration writer `/root` 安排，2026-09-25。

| 送审提交 | 结论与处置 |
| --- | --- |
| `f169760` | 阻断：新增 scope 会拒绝所有旧 key；最终验权/egress 与撤销缺少串行机制；Workspace index epoch 未由 source 写递增；Hash 基线依赖未来可变实现。随后 `f5cbf86`/`36c2434`/`f4c2814` 修正并复审 |
| `f4c2814` | scope/epoch/固定基线修正成立；仍阻断：5秒 permit 允许撤销后新开始发送、治理 writer 仅靠约定可绕过、敏感 title 脱敏自然键碰撞。`5a01610` 拒绝敏感 title；`97129dc` 提交 session gate/强制 writer 方案供审查 |
| `97129dc` | **未通过**：慢 sender（10秒/5秒）与撤销2秒等待导致正常 revoke 失败，orphan 亦可阻断 revoke，而当前 LLM SSE/MCP 不消费 Knowledge gate，不能保证 PRD 全平台即时不可访问。当前 compose/runtime 以 eap 高权限角色且 Gateway 自动迁移，候选要求非 owner role 但没有完整启动/迁移权限切分契约，trigger 可被绕过。不能以文档要求替代实现边界 |

2026-09-25 恢复设计：main 已集成并完成 TASK-064，ADR-0030 已独立 Accepted（仅设计/wire/no-go）；TASK-065 正在实施，尚非运行通过。本次先 merge main `10b6c38` 到持久 TASK-056 分支，保留上述历史审查结论，重新提交本候选复审。

本轮处置：§3 完整删除旧 Knowledge 全局锁/孤儿阻塞 R/可延迟 permit 协议；改为消费 ADR-0030 的 BeginExecution/Dispatch/Release/CommitMutation/CancelAndJoin，R 持久拒绝与 C 停止证明分离；remote embeddings 缺停止 profile 时 production no-go；数据库角色/启动强制约束归 TASK-065，pgvector 迁移保持该约束。§4 删除 Knowledge 自建 dispatch ledger，仅以业务 attempt 引用共享 execution。source/index epoch、旧 key 兼容、敏感 title 拒绝、固定评测和旧数据迁移保持原已修正边界。

此前送审时状态为 proposed，registry needs_arch_review；最终设计结论以下述记录为准。TASK-056 接受与完成不等于 TASK-065 通过，也不解锁有 TASK-065 依赖的 TASK-057。生产远端 no-go 只能由共享 connector 资格与实现验收解除，不靠本 ADR Accepted 字样解除。

复审976dae2处置：独立Sol要求补齐治理版本检查到提交/实际发送之间的串行缺口，以及拒绝/扫描失败审计顺序。本轮§3.1定义ExpectedGovernanceSnapshot、共享ReplaceGovernanceSnapshot/guard及不可绕过writer，明确是TASK-065待实现扩展而非ADR-0030既有事实；§3.2补可靠拒绝审计与audit不可用的明确fail-closed例外。该修订形成固定提交 a10de92，审查结论见下。


**最终独立设计审查（2026-09-25）**：integration writer `/root` 转达独立 GPT-6 Sol 对固定提交 `a10de920dff849cba40406bc4de4d9a9d2d427bb` 的二次复审：**设计可 Accepted**，治理版本串行与拒绝/扫描失败可靠审计的上一轮缺口已解决。接受范围为本文及关联 upload/search/score/source/Usage、generation/迁移、固定评测与共享门禁消费契约；未执行真实 Provider/pgvector迁移/跨实例运行验收，不构成上线批准。

审查所要求的共享 owner 交接已在 main `cc3580da6da0f20975502fb1c91d5dd0846d1477` 完成并同步本分支：TASK-065 卡明确拥有 Read/ReplaceGovernanceSnapshot、global/Workspace/target revision/digest、全部 Policy/DLP/Provider/Embedding 控制 writer、Dispatch/Release/CommitMutation 原子消费与双实例/直接DML绕过验收；root 确认 Sol 实现 owner 已接受。TASK-057仅拥有 Knowledge Policy action 与 source/generation 业务CAS，不能另建版本/授权真相。

因此 ADR-0029 标记 Accepted（设计/契约）；注册表完成仍由 root 集成验证后处理。TASK-065 实现与共享snapshot资格、connector停止证明、真实检索质量和迁移/恢复门禁未通过前，远程语义生产保持 no-go。此最终状态修订只记录真实审查与交接，不扩大 a10de92 的设计保证。
