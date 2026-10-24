# 钉版复用清单与实现接缝

所属：冻结知识检索与复用契约（TASK-056），生产采用须 [ADR-0029](../adr/0029-knowledge-retrieval-generations.md) 接受。
参考仓库只读：`/Users/linxinhong/projects/TencentDB-Agent-Memory`；实测 HEAD=`828ac8907ccde2cfed14ca42acd0420bea19e409`。所有候选以此 commit 的 `git show`/archive 取材，不以工作区未提交文件取材。本卡未复制源代码或新增运行依赖。

## 文件与许可边界

| 拟采用/评估对象（均相对参考仓库） | 直接依赖及处理 | 许可/补丁边界与 owner |
| --- | --- | --- |
| `MemoryCore/src/core/store/search-utils.ts` 中 `RRF_K/rrfMerge` | 无 import，独立算法；不引入 SQLite store、身份、memory router | 根 MIT（Tencent 2026）；TASK-058 可移植到 Go，保留完整 MIT/copyright 到 `third_party/tencentdb-agent-memory/LICENSE` 与源码出处注释。补丁：SQL 预过滤、路内去重、稳定 tie-break；不复制 TypeScript 包运行时 |
| `MemoryKnowledge/src/engines/wiki/ingest-v2/chunker.ts`、`safe-path.ts` | chunker 无外部 import；safe-path 依赖 node:path；仅 TASK-060 隔离实验 | 根 MIT；保留许可证；safe path 不能当作远程抓取 SSRF 边界。生产 chunker 暂仍 EAP 固定 rune 版本，变更会创建新 generation |
| `MemoryKnowledge/src/engines/wiki/ingest-v2/index.ts` 及其相对依赖 `template.ts/prompts.ts/file-protocol.ts/frontmatter.ts/merge.ts/slug.ts/index-builder.ts/log-writer.ts/llm.ts` | node:fs/path、p-limit 类型、logger；llm→ai、@ai-sdk/openai、@ai-sdk/anthropic；继续追踪上述文件的所有 imports 并生成实验 manifest | 根 MIT 仅覆盖本仓源码；TASK-060 是候选闭包审计而非本卡生产 import 批准。必须将 llm.ts 换为 EAP 受治理接口 stub；去 Panel callback、secret 配置、原始日志；禁止安装整包把 SDK provider 直连带入生产 |
| `MemoryKnowledge/src/engines/code/index.ts`、`bridge.ts`、`normalize.ts` | bridge 运行时加载 `@colbymchenry/codegraph`、node:module/url 与 logger | 本仓 MIT；外部 codegraph 许可证不能继承根 MIT。TASK-060 从钉版 lockfile 解析精确版本、integrity 与对应 tarball LICENSE 后才装实验依赖；审查未通过则 no-go；不新增生产图存储 |
| `MemoryKnowledge/src/module.ts`、`middleware/auth.ts`、`routes/tools.ts`、`store/*`、`callback.ts` | Hono、SQLite、Panel callback、默认配置/鉴权及调度依赖 | 只读负面参考，不拟复制。不能将匿名只读路由、调用方 tenant ID、SQLite 权威目录或 Panel 绑定移入 EAP |

凡候选相对依赖不在上述范围，TASK-060 先在实验报告扩展精确文件/hash/依赖/许可清单，不可静默变成生产依赖。package.json 的 `^` 范围不是钉版证据；必须随参考 commit 的锁文件与包内容核对。没有审核的外部依赖保持 `unverified / prohibited for production`。本卡当前唯一获准进入后续生产评审的移植对象为独立 RRF；Wiki/CodeGraph 源码只获准隔离实验。复制 substantial portion 时根 MIT 完整文本必须随副本和分发保留；现未发现需要随独立 search-utils 复制的其他 NOTICE，扩展闭包须重新核查。

pgvector 使用 [官方 v0.8.0 LICENSE](https://raw.githubusercontent.com/pgvector/pgvector/v0.8.0/LICENSE)（PostgreSQL License），镜像保留自带 `/usr/share/doc/pgvector` LICENSE/README；TASK-057 在发布 SBOM/third-party notice 中记录精确镜像 digest、扩展版本与 PostgreSQL 依赖，不复制 Tencent 许可证替代它。镜像验证记录在 ADR-0029；本卡不是镜像运行验收。

## 后续卡接缝与门禁

| 完整任务名称 | owning seam / migration / fixture | 验证与交接 |
| --- | --- | --- |
| 实施全平台撤销后的在途能力阻断（TASK-065） | 独占 canonical epoch/execution ledger/Dispatch/Release/CommitMutation/CancelAndJoin、revocation wire、runtime/migration role与强制写约束；本卡不改其代码 | ADR-0030 §8全入口/多实例/远端停止证明矩阵；交付同事务业务callback及版本检查接缝，不能用advisory lock消失作为停止证明 |
| 受治理的语义向量入库与版本切换（TASK-057） | `packages/knowledge` governed execution + generation；`packages/llmkernel` Provider 返回 usage；`apps/gateway/{main,knowledge,protocols}.go`；`packages/governance` usage known flags；`packages/store` 消费已验证 TASK-065 CommitMutation 的同一 audit tx，业务 job/attempt 只引用 shared execution；`packages/policy/table.go` action 匹配和既有规则回归；下一 migration 包含 ADR §4 表/FK/known flags/policy_rules.action | 消费 `knowledge.upload.*`、`knowledge.search.*`；真实 PG16/pgvector dump/restore；Provider 成功→DB fail→重启→重试、审批/DLP/权限拒绝无 egress、两个 writer/删除 CAS、审计不可用无激活/返回、同维错模型拒绝；接入 `make verify-knowledge-slice` 与 `make verify-foundation` |
| Workspace 混合检索与 RRF 排序（TASK-058） | `packages/knowledge` SQL 预过滤/fts/CJK/RRF；其测试；如需新增索引另取下一 migration；禁止重写 TASK-057 已接受 DDL | `knowledge.search.response.json`、`knowledge.derived-search.response.json`，eval v1；精确 top_k、去重/tie、空结果/非概率 score、源撤销与 ACL、中文和真实语义 gate；既有客户端 search journey + Make gates |
| 知识检索增强阶段验证（TASK-059） | 只读业务验证脚本/证据，消费 TASK-057/058 集成 SHA、重置 fixture 与 mock/真实 provider 配置引用 | PG/Redis 故障、成员移除/key 撤销、更新失败保旧、同代检索与质量分组；unverified 不计 pass；缺陷交回有 owner 实现卡 |
| Wiki/CodeGraph 内核拆分验证（TASK-060） | `experiments/knowledge-memory/`（候选实验根，owner 建立），钉版文件清单与许可；不写主生产装配/DDL | 受控文本→页面→原始片段 lineage；CodeGraph 固定小仓 commit 的符号/行号/update/clean；恢复和取消；输出 go/no-go 及待架构问题，不导入 Panel |
| Wiki 派生文档接入 EAP（TASK-061） | 仅在独立接受 Wiki ADR 后实现来源 adapter；消费 source/lineage/generation；复用现有 upload/search 六字段 | `knowledge.derived-search.response.json`；原始来源授权/版本/删除→派生停止检索，来源同名不覆盖；make 两门禁 + 可重置资料 |
| Wiki 来源到检索业务验收（TASK-062） | 独立真实 API/PG/Provider 验证，消费 TASK-061 运行说明 | 同 source 重试幂等、派生引用原资料、撤销/重启/预算与审计故障；不新增 UI |
| 企业工作记忆边界设计（TASK-063） | 新 proposed ADR，不能修改本文 shared contract；记忆不是当前 Knowledge 数据自动晋升 | PRD P2/V1.5+；显式提交来源/TTL/冲突/删除/权限；不默认捕获全对话，不要求或暗示已经上线 |

受限真实模型资格验证只接收服务器端已授权 credential reference。没有凭据时提供可执行命令/报告模板、标 unverified，并保留上线 gate；不要求员工配置 Provider secret。独立 reviewer 负责 ADR 审查，integration writer 在 main 记录任务完成；本卡分支通过文档/基线校验不能自行宣布后续生产可用。

## 2026-09-25 接缝更新

TASK-064/ADR-0030设计已接受，TASK-065实施中；TASK-056只修订知识契约，TASK-057在两者完成后消费共享运行门禁。远端profile缺失/失效时503 revocation_stop_unsupported，不能因本卡通过或stub返回而解除生产no-go。TASK-060/066已完成的隔离实验见 engine-spike.md：Wiki候选内存边界可继续研究，生产仍须独立ADR；CodeGraph/native分发许可闭包未因此自动通过。TASK-063的ADR-0031仍为独立proposed产品设计。

976dae2复审后的接口补充：TASK-065拥有ReadGovernanceSnapshot/ReplaceGovernanceSnapshot、治理revision迁移与Dispatch/Release实际writer guard、CommitMutation同事务版本比较；TASK-057只提供Knowledge Policy action/业务CAS与expected snapshot，不另建Policy权威。共享扩展未实现或未通过并发/直接SQL绕过验证，知识远程生产保持no-go。拒绝/扫描/audit故障的顺序按ADR-0029 §3.2验证。
