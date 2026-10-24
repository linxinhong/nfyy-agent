# Knowledge 检索契约 v1

Owner：冻结知识检索与复用契约（TASK-056）。随已接受 [ADR-0029](../adr/0029-knowledge-retrieval-generations.md) 冻结设计契约；生产门禁仍未通过。本文定义目标；不是现有实现的通过证明。撤销消费已接受 [ADR-0030](../adr/0030-platform-revocation-and-inflight-boundary.md) 的 R/C 与共享 execution 接缝，由 TASK-065 实现；Knowledge 不另建撤销权威。

## 当前治理实况与补齐责任

基线 `0c8abee`（运行代码与 `0658a30` 相同）：

| 入口 | 实际调用链 | 尚缺的切片能力（TASK-057 owner） |
| --- | --- | --- |
| `apps/gateway/knowledge.go:handleKnowledgeUpload` | requireAPIKey → selectWorkspace → withDLPSubject → Scan(content) → 有规则时 RequireWrite(dlp.hit) → 脱敏 content → Service.Ingest → 201 | 无独立 knowledge Policy/读写动作门禁、无所有成功/拒绝操作完整审计；Title 被写进 audit Resource；Embedder 不带 Session/generation，远程 egress 治理不能由当前调用证明 |
| `handleKnowledgeSearch` | 同一身份/Workspace → query Scan/脱敏 → Service.Search → 每 hit 内容再 Scan → withheld/redaction → 豁免用户可恢复 token → 有命中规则才可靠写 dlp.hit → 200 | Query deny 无本层可靠事件、无每次 knowledge.read、无知识/embedding Policy；标题/来源未扫；无 generation/查询与发布时版本检查；共享 Dispatch/Release/CommitMutation 由 TASK-065 先补齐，TASK-057 加知识检查 |
| `protocols.go:handleEmbeddings` | commonProtocolAuth：key+membership+aud=llm-gateway sign/verify → RoutePlanFor → 首 route → ResolveProviderTarget → credential resolver → OpenAICompatClient.Embeddings → usage=0 | Session/claims 被丢弃，未调用 Router/Policy/DLP；没有完整 egress、可靠治理审计与 Usage；未知用量被写成 0。只复用此客户端不能满足新治理接口 |
| `apps/gateway/main.go` / `packages/knowledge/knowledge.go` | HashEmbedder 注入；Ingest 在 DB transaction 中 Embed；Search 取 Workspace chunks 后 Go cosine；引用已有六字段 | 服务注释“production provider-backed”与装配不符，以代码为准。需外部调用移出事务、可靠结果/激活/读取审计、同代向量与 SQL 候选 |

员工 key 的 Principal.Scopes 目前未持久化/赋值；兼容权限由有效 key+Workspace membership、重建角色和明确 knowledge Policy action 决定，详见 ADR §1。现有 TablePolicy 忽略 PolicyInput.Action，TASK-057 必须补齐同一 policy_rules 的 action 匹配，不以新增空 scope 检查拒绝旧凭据。

现有 `audit.RequireWrite` 只保证自身 durable；不能证明与 pointer 同事务。TASK-057 必须在 TASK-065 的 CommitMutation 同一事务内写 pointer、业务 audit 与 epoch 验证；不另建 authorizing ledger。现有 `WriteGovernanceBatch` / `governance.InsertUsageTx` 可借用概念与 SQL；新切片必须传相同 pgx.Tx。`usage_records` 当前只有 cost_known，tokens_known 需迁移，不将旧账本零值推断为真实用量。

## 请求与响应逐字段映射

端点和认证不变：`POST /enterprise/v1/knowledge/documents`、`POST /enterprise/v1/knowledge/search`；员工 key 仅 Gateway 消费，服务器选择 Workspace；body 不接收可信 workspace、actor、source identity 或 model config。

| 位置 | 现有字段/行为 | 目标兼容规则 |
| --- | --- | --- |
| upload request | title:string、content:string（必填） | 保留；title 是 upload source 在 Workspace 内的定位键，原文只在请求处理期间存在；content 扫描后 hash/chunk/embed；title 任一 DLP 规则命中（deny/redact/tokenize/alert）或扫描失败即拒绝；要求安全标题，不把脱敏后的显示标题作为 source 自然键；错误只有通用 DLP code/trace，不回显原文/匹配片段/规则细节 |
| upload response 201 | document_id:string(UUID) | 保留现有版本实体 UUID；同内容 replay 保留；更新可产生新 UUID，不能假设稳定 source ID 等于 document_id |
| upload response | title:string、version:integer≥1、status:string | 保留；成功仅 ready；v+1 激活成功才 201；failed/building 不通过成功响应暴露 |
| upload response | idempotent_replay:boolean、redactions:integer≥0 | 保留；同 source、扫描后 content hash、活动 generation 复用；切换 generation 期间同内容重传不导致错代复用；redactions 为本请求实际扫描计数 |
| search request | query:string、top_k:integer optional | 保留；空白 query invalid_argument；top_k 缺失、≤0 或 >20 一律 5（保持现实现），有效 1..20 |
| search response | results:array | 保留；无相关结果 `{"results":[]}`，200，不以 error 表示空结果 |
| each result | document_id、title、version、seq | 保留；当前授权、活动来源版本的 UUID/显示标题/版本/零起 chunk 序号，缺引用不得返回命中 |
| each result | content:string | 保留；受当前 DLP 的 released chunk；deny 内容采用 `[WITHHELD:DLP] ` 前缀加安全规则原因；不附 Provider 原始错误或敏感匹配文本 |
| each result | score:number | 保留数值类型；legacy hash 为 cosine>0；semantic hybrid 为下述 RRF 值。非概率，不承诺 [0,1] 中的相关性解释，不跨请求、模型、代际比较；依赖旧 cosine 阈值的客户端需升级 |
| each result（可选新增） | score_kind:string | `cosine` 或 `rrf`；缺失代表历史响应；实现 hybrid 时必须提供；旧客户端可忽略未知字段 |
| each result（可选新增） | source:object | 下述 provenance；upload 可添加，derived 必须；客户端若忽略仍能显示六个引用字段，完整追溯需读取 source |

上传没有异步公共 job API，本卡不改变同步 201 契约；内部构建超时返回现有错误信封而非假成功。请求重试同 source/hash/config 幂等查询已完成结果。模型/审计故障返回现有 upstream_unavailable/internal；新增共享 connector admission 拒绝沿用 ADR-0030 的 503 `revocation_stop_unsupported`，在 egress 前产生（不等于模型不可用后自动降级）；权限、Policy、DLP 使用既有错误码和 trace_id，调用方不能从错误获知另一个 Workspace 是否有文档。新模型不可用时不自动返回 keyword-only 或 Hash 结果。RRF 是有意的 score 行为变更，必须在 release notes 与客户端 qualification 中明确。

## 来源字段

```json
{"source":{"type":"derived","id":"srv-source-id","version":2,"derived":true,
 "parents":[{"id":"srv-original-id","version":4,"locator":{"kind":"text_range","start":0,"end":120},"digest":"sha256:..."}]}}
```

source ID 由服务器生成，不接受第三方 tenant/team/page ID 作为权限。`source.version` 为本 source 版本；parents 定位原始授权资料，version 固定，不指向浮动 latest。locator 的 text_range 为原始规范化文本中零起 Unicode code-point 半开区间；code location 留待 CodeGraph 实验，只允许固定 commit+相对路径+行号，不提前开放远程 URL。digest 仅使用脱敏后源片段的摘要，不是原始秘密哈希。parents 非空、均同 Workspace且当前可访问才允许返回 derived 命中；不接受无来源的生成页；来源失效则整个派生文档不可检索，不能只删 parents 隐藏血缘。UI/客户端不得把 derived=true 展示成已审核 Canonical Knowledge。暂不对外返回 confidence 或 model 自评可信度。

上传 source.type=`upload`，derived=false，parents=[]；现有 version/document_id/seq 不改变，source 是稳定来源与版本实体之间的补充。引用显示标题与 locator 元数据也过 DLP；DLP 不允许安全展示时用占位标题/隐藏定位，源 ACL 失败则整个命中消失。历史已存敏感 title 引用与管理输出仍扫描脱敏/占位，定位始终用其内部 source/document ID，不以显示名重新定位。审计 Resource 使用 source ID（创建前用 request ID），不得继续拼接原 title。来源 ID/version 不因标题相同合并。真实字段示例位于 `contracts/schemas/v1/enterprise/knowledge.*.json`（仓库此目录既有文件是 JSON 示例，不冒称完整 JSON Schema）。

## 候选、score 和确定性

TASK-058：每一路在 SQL 中先限制 Workspace、source.active_version、workspace.active_generation 与未撤销状态，再取最多 100 个候选；向量精确 cosine（零向量拒绝）、词法 `ts_rank_cd`。ASCII 小写词、CJK U+4E00..U+9FFF 单字与双字 token（与 Hash tokenizer 同一规则）；其他文本遵循 `eap-text-v1`，大小写 Unicode lowercase，不做会改变引用偏移的 NFKC。ASCII 常见功能词固定排除 `a an the is are was were be been being of to in on at for from by with and or but if it its this that these those which what how do does did can could would should my your our their`（文档与 query 同表）；版本变化须重建 generation。词法用 `to_tsvector('simple', token stream)` 与 token 的 OR tsquery，全部参数绑定并转义 tsquery 操作符。不是 BM25；不增加 jieba/新搜索服务。

向量最低 cosine=0.65，词法至少一个非空规范 token 匹配；先拒绝未达条件候选再排名，不把所有向量近邻视为相关。此阈值固定为 v1 初始资格门槛，未通过不能偷偷在评测集上调阈值。冻结集是小型 qualification 集，不代表所有领域；改变阈值/分词/模型需新增独立评测版本并评审，旧结果保持可比。

每路同 chunk 只出现一次；RRF=`sum(1/(60+rank))`，rank 从1起；缺席一路贡献0，双路最高2/61。按 `(workspace_id,generation_id,document_id,seq)` 合并，score 相等按 document_id 字典序、seq 升序打破平局；每路同分亦用此键。只在融合后取 top_k；不以标题去重。RRF 只排序已经有相关性依据的候选，没有额外概率含义；generation/config/score_kind 进入内部 trace 元数据，不包含 query。

## 撤销与调用资格

上传/查询/后台重建经 BeginExecution；每次 embedding 用 Dispatch；发布、删除和回滚用同事务 CommitMutation；包括缓存命中的最终内容用 Release。R 后无新授权并主动停止在途；C 必须包括相关远端停止证明，unknown 保持 pending/unresolved且告警。取消 HTTP、收到向量或本地 writer join 均不能替代远端终态证明。完整 revocation mutation 的202/204/GET/version2契约由 ADR-0030 §7唯一拥有，本文件不修改其 wire。现有OpenAICompat embeddings缺停止证明，真实模型质量即使通过也仍production no-go，TASK-057同时依赖TASK-056与TASK-065。

Policy/DLP/config治理快照使用ADR-0029 §3.1的ExpectedGovernanceSnapshot与共享writer guard；不是先重读版本后无锁发送。规则新增/删除/修改与revision同事务，控制写未证明旧writer停止不得声称新版本生效。拒绝/扫描故障在返回准确业务错误前可靠写审计；audit不可用只返回通用基础设施fail-closed错误、零业务内容/egress/激活并告警，不能声称已审计（§3.2）。
