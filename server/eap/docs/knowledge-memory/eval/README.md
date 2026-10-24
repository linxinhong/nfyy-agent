# 固定检索评测 v1

冻结时点：冻结知识检索与复用契约（TASK-056），2026-09-25；先定义数据/标签/门槛，再运行现有 HashEmbedder 基线。算法调优 owner 无权就地改标签或阈值。数据均为人工构造测试资料，无真实企业秘密；`ZEUS-8844` 是虚构隔离陷阱。

`dataset-v1.json` SHA-256：`39016633682391ab61d8222fd8d2a97c35e4adb34d972d127ef8a621d47c246f`。
10 文档、2 Workspace、12 查询；每文档小于单 chunk；精确术语2、中文2、语义改写3、无相关2、跨 Workspace 陷阱3。相关性标签：2=直接回答、1=部分支持、0/未列=无关；v1 只有明确的2/0，无臆测标签。每个查询只对授权 Workspace 内资料标注，x02 故意与 d01 同文；alpha 不得返回 x01/x02，beta 不得返回 d01..d08。标签列出的文档在 top3 中计一次，重复 chunk 不加分。

## 指标和固定门槛

- Relevant queries 的 macro Recall@3、MRR@3、nDCG@3 按组分别报告；nDCG gain=`2^grade-1`，discount=`log2(rank+1)`，空标签的这些指标为 N/A，不用“零召回”压低均值。
- exact/chinese 组 Recall@3=1、MRR@3=1；paraphrase Recall@3=1、MRR@3≥0.80、nDCG@3≥0.85，且三个指标均不得低于 Hash baseline（至少 Recall 提高1/3）。不能用精确术语组高分掩盖改写退化。
- 无相关组 q08/q09 与跨域空结果 q10 必须全部 `results=[]`（false positive queries=0/3）；不得为制造空结果对 eval query ID 做特殊处理。
- workspace trap q11/q12 的授权相关命中 Recall/MRR@3=1；所有12查询返回的越权 source/document/chunk 数为0，且必须用真实 SQL/API 另测，而不是仅凭离线过滤宣布隔离通过。
- 所有返回引用的 document/version/seq/source 必须匹配当前资料；成功替换后旧版本命中数0，构建失败时旧活动版本仍命中；这些业务断言由 TASK-057/058/059 的运行验证覆盖，v1 小语料只做内容基线。

这些是资格门槛，不是事先保证任何模型通过。Provider 不合格时不得激活，配置/模型改变重跑同一集；发现标签缺陷需独立评审和 v2 数据集，保留 v1 报告，不围绕 v1 调参后冒称泛化能力。

## 已运行 Hash 基线

复用实际 `packages/knowledge.HashEmbedder{Dims:256}` 和 `Chunk`；runner 复制当前 Search 的 cosine>0、Workspace 预筛选与 stable score 排序。输入同分使用 dataset 顺序（旧 SQL 未承诺同分序），基线只报告该确定顺序；新 RRF 必须按契约稳定 tie-break。

| 分组 | Recall@3 | MRR@3 | nDCG@3 | 观察 |
| --- | --- | --- | --- | --- |
| exact | 1.000 | 1.000 | 1.000 | 2 queries |
| chinese | 1.000 | 1.000 | 1.000 | 2 queries |
| paraphrase | 0.667 | 0.667 | 0.667 | 3 queries，中英改写存在遗漏 |
| no_result | N/A | N/A | N/A | 两个查询均误报非空；Hash 碰撞/词重叠不能证明相关 |
| workspace_trap（有标签的2项） | 1.000 | 1.000 | 1.000 | q10 应为空但误报；离线没有返回跨域 ID |

原始可复核输出：[hash-baseline-v1.json](hash-baseline-v1.json)。这不是 DB、DLP、可靠审计或真实模型验收；不能把向量 stub 提升为语义质量证据。

```sh
# 从 repo/worktree 根；不需要 DB 或 Provider，不读取任何凭据
# 当前知识实现未变化时的快速对照（TASK-057 后以固定修订 runner 为准）
go run ./docs/knowledge-memory/eval/hash-baseline.go
python3 docs/knowledge-memory/eval/reproduce-frozen-baseline.py
sha256sum docs/knowledge-memory/eval/dataset-v1.json
```

执行环境：Go 1.27.0，macOS arm64，工作树 `astra-knowledge`，代码源修订 `0c8abee`；runner 位于本卡提交。run 记录为 coding-owner worktree-kit `hash-baseline`，exit 0、实际命令 1.7698 秒（含 Go run，不是检索 latency）。baseline JSON 固定源实现及数据摘要。永久重现入口 `reproduce-frozen-baseline.py` 导出完整代码修订 `0c8abee7dcb2b818fcfa8afd62d3294bc6547946` 到一次性临时目录（非第二 git worktree），只从固定 artifact 修订 `f16976029f35ffd3a0b380ba37603203aea00a04` 加入 runner 和 dataset；不会编译后续变化的 packages/knowledge。输出与固定基线 JSON 逐字比较；当前仓库改动不得替换历史基线。

## 延迟协议（后续实现执行，当前 unverified）

分离：① Gateway/Policy/DLP/Audit/SQL 自身时间（Provider spans 扣除，不能扣掉 DB 或故障处理）；②端到端含 Provider 时间；③ ingest chunk/token/egress cost。固定开发资格机4 CPU/8GiB、PG16/pgvector0.8.0 + Redis、本机网络，2 Workspace 各5,000个合成固定模板 chunk（不得用于 relevance 评分），并发10、20次 warmup、200次测量、禁结果缓存，每查询 top_k=3；报告硬件/PG配置/具体模型配置摘要/语料生成 seed=56 和分位数计算方法 nearest-rank。

自身 search p95<100ms；端到端真实 Provider p95≤2000ms；200次错误率=0（故障注入单列）。时间以请求入口到最后 response 为基准；超时亦记失败，不能丢掉慢请求。若不同硬件需记录差异，不冒充固定环境通过。Hash legacy 与新 hybrid 同机同语料分别测，记录变化但不以旧 Hash 时间代替新链结果。这里没有执行该负载，不能引用 go run 总时长当 latency pass。

## Stub 与真实模型证据

Stub 覆盖返回序号/维数错误、nan/zero、超时、Provider 成功后 DB 失败、unknown Usage、取消、重试、审计失败零激活、跨域 SQL 与 DLP 处理；确定性 stub 分数不计语义 gate。真实已授权 Provider 使用服务器固定 config digest，实际生成文档/query 向量后跑12查询与上述质量门槛，报告模型/deployment revision、dimension、实测 Usage known/unknown、数据摘要与 tested SHA。无权限/凭据/服务时真实质量和端到端 latency 都标 unverified，保留门禁，不向客户端配置 secret。
