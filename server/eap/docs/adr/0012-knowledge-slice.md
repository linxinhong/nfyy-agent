# ADR-0012 Workspace Knowledge 垂直切片（TASK-012）

状态：已接受　日期：2026-09-01　关联任务：TASK-012

## 决策

### 1. Workspace 过滤在检索查询内部

`knowledge_chunks.workspace_id` 是查询谓词的一部分——跨 Workspace 内容在 SQL 层就不被取回（架构红线：禁止全局向量检索后置过滤）。成员资格缺失/解析失败 = 无内容返回（fail closed）。

### 2. 引用强制

每个命中携带 document_id/title/version/seq/score/content；零相似度（cosine=0）的分块不作为命中返回——没有相关性依据的结果不算成功检索。

### 3. 幂等与版本替换

上传以 (workspace, title) 定位文档、content_hash 判重：相同内容重传是幂等 no-op；新内容版本号 +1 并**原子替换**（旧文档行删除、chunks 级联删除），旧版本内容立即不可检索。处理（parse/chunk/embed/index）在单事务内完成，任何失败回滚且不留下可检索状态；embedder 失败文档标记 failed 并显式报错。

### 4. 边界与预算

文档 2MiB、512 chunk、1200 字符/chunk（150 重叠）上限防无界内存/存储。V1 嵌入器为确定性 HashEmbedder（256 维、CJK 二元组 + ASCII 词、L2 归一化），`Embedder` 接缝保留 provider 级嵌入（pgvector 迁移）为后续卡；检索为 Workspace 内暴力余弦——切片规模下正确性优先。

### 5. 治理接线

上传内容与搜索查询都过 DLP 门禁（deny 类阻断在入索引/出查询之前）；原始文档文本不进入日志或审计（审计只记事件与计数）。端点复用 agk key + Workspace 成员资格语义。

## 验证

`make verify-knowledge-slice` = verify-mcp-governance（含全链前级）+ `TestKnowledgeSliceIntegration`：上传→带引用搜索（title/version/seq）、跨 Workspace 无泄漏、孤儿成员 403、DLP 阻断（内容与查询）、幂等重传、版本替换后旧内容不可检索且新版本可检索、吊销 key 后 401——真实 PG/Redis。
