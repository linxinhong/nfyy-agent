# 律所知识沉淀平台：方案落地与验收

依据用户提供的《律所知识沉淀平台 技术设计文档》（2026-10-15），整理后的目标设计见[律所知识沉淀平台技术设计](../legal-knowledge-platform-technical-design.md)。本文件区分已运行能力、已落地的接口基础和尚未具备的业务闭环，避免把通用元朴中心的 Wiki 审核误认为律所知识流水线已经完成。

## 已定边界

- 沿用本仓 `server/backend` Go 单体、`server/web` 管理站点、桌面 Agent 和 PostgreSQL；不另建仓库。BookStack 作为可替换的展示、编辑层，经 `internal/store/bookstack` REST 适配器接入，不修改 BookStack 源码。资讯与 AI 能力目录仍可保留在现有元朴中心；律所知识条目走独立的行业流水线。
- 普通浏览器以账号密码登录个人中心；yuanpu-agent 内嵌浏览器首次以账号密码登录后，中心签发设备 API key 并加密保存到桌面端，后续凭 Key 无感进入中心。BookStack 浏览器端无感登录需要单独的身份映射和 SSO 设计与验收，不能把中心 API key 暴露给 BookStack 前端或让双账号长期并存。
- 一家客户一套私有化部署。新业务表预留 `org_id`；第一阶段单组织，客户间由独立数据库、BookStack、附件卷和模型服务隔离。默认模型只连内网，关联案件资料禁止外发。
- Agent 只生成待审核稿；人工作最终发布决定。所有读、搜索、导出和模型 Context 入口都必须先通过同一套权限与隔离墙。隔离优先级为：隔离 > 案件授权 > 领域授权 > 默认可见。
- BookStack [内容权限 API](https://demo.bookstackapp.com/api/docs) 可读写书架、书、章、页的覆盖规则，但其[默认管理员始终拥有全部权限](https://www.bookstackapp.com/docs/user/roles-and-permissions/)。因此“系统管理员不默认可见案件内容”不能靠 BookStack 默认管理员角色兑现；日常管理员必须使用受限角色，Break-glass 超级管理员凭据单独托管并审计。案件隔离墙未验收前禁止放入真实敏感案件材料。

## 本轮落地

| 内容 | 位置 | 可验证行为 |
| --- | --- | --- |
| BookStack REST 适配器 | `server/backend/internal/store/bookstack` | 页面读/写、实体权限读/写、连接探针；服务端令牌不进入响应，拒绝跨站重定向，ACL 写入默认拒绝所有未列明角色。 |
| 行业包 | `server/backend/industries/legal` | 六类知识、必填字段、公共段落、置信度、模板、分类、脱敏和引用校验配置独立于 Go 业务代码。 |
| 提交前校验 | `GET /v1/industry`、`GET /v1/industry/templates/{type}`、`POST /v1/industry/validate` | 中心认证后取得当前行业规则与模板；缺来源、脱敏确认、类型字段或适用限制时返回明确字段错误。这里只校验，不入库或发布。 |
| 连接探针 | `GET /v1/integrations/bookstack/status` | 仅配置 BookStack API Token 时挂载；使用服务端 Token 检查私有 BookStack API，响应不包含令牌或页面内容。 |
| 部署入口 | `server/backend/deploy/compose.bookstack.yaml` | 可选 BookStack + MariaDB 卷，默认只绑定本机端口；Go API、PostgreSQL 继续独立。启用前须设置独立密钥、修改 BookStack 初始管理员凭据并配置私有入口。 |

## 接下来的实施顺序

1. **身份与权限基础**：为中心用户建立 BookStack 用户映射；设计浏览器成员会话进入 BookStack 的 SSO，桌面设备 API key 留在元朴受限桥接与中心鉴权链路中；建立 `org`、业务领域和最小角色映射，保证 Agent 账号无法访问正式知识库或调用发布 API。完成领域级权限计算、写入和夜间漂移比对。验收同领域/跨领域的页面、搜索、附件拒绝行为。
2. **收件箱纵切**：BookStack 系统书架配置收件箱、待审核、已处理和导出；Webhook 只作触发，30 分钟轮询补漏；Go 保存原始页 ID、作者、来源、状态变化与幂等键。提交先按行业包校验，再做脱敏复查、归类、相似/冲突提示，把结果写到待审核。Webhook 无可靠签名依据时须用随机回调路径、内网限制和重新从 BookStack 拉取页面，不信任回调正文。
3. **人工审核与追溯**：领域负责人确认或退回；发布写 BookStack 正式章节并回链原始提交，失败可重试且不会重复发布；Go 保存审核决策、版本、来源和引用。所有状态变化写只追加审计。引用只能从原文或现有获授权条目提取，未经核验的法条/案号不能变成已验证结论。
4. **本地模型与导出**：Go `llm` 网关按任务路由到本地 OpenAI 兼容接口；配置默认禁止外部地址，记录用途、模型、延迟与输入摘要哈希，不记原始敏感内容。周报与专题手册由持久任务驱动，保留来源和权限水位。
5. **案件与隔离墙**：`clients`、`matters`、成员、双人确认的隔离名单；页面 ACL、搜索与导出三处同判。先测规模与角色数，再考虑推广到全所。同步失败时拒绝发布或读取，不按旧 ACL 放行。
6. **交付验收**：Compose 增加完整 HTTPS 入口、备份/恢复、密钥轮换、模型服务和任务队列；用真实 BookStack 实例做端到端测试，再做第二个行业包验证“只换配置不改核心”。

## 验收缺口

| 方案章节 | 当前状态 | 尚需证明 |
| --- | --- | --- |
| 1—3 目标、架构 | 基础结构就位 | BookStack 真实部署及浏览器无感登录。 |
| 4 知识流水线 | 未实现 | 10 步状态机、Webhook/轮询、原始归档、冲突分支、人工发布。 |
| 5 知识模型 | 行业包及提交前校验已落地 | BookStack WYSIWYG 表单映射、来源真实性、字段持久化。 |
| 6 权限与隔离墙 | 通用角色×范围已有；BookStack ACL 适配器已落地 | 领域角色同步、案件授权、双管理员隔离确认、夜间漂移检测、拒绝测试。 |
| 7 Go 模块与表 | 现有 Go/PG 骨架 | `org_id`、案件、提交、引用、审核与模型调用数据模型和迁移。 |
| 8 模型 | 未实现 | 内网网关、脱敏、引文核验、外部 API 默认关闭。 |
| 9 安全与运维 | 浏览器账号密码、桌面设备 API key、基础审计和可选 Compose 入口 | 全量只追加审计、HTTPS、备份恢复、令牌轮换和附件加密。 |
| 10—11 产品化与计划 | 行业包首版 | 多行业验证、安装包、许可证清单、容量测试和客户验收。 |

BookStack 源码为 [MIT 许可](https://github.com/BookStackApp/BookStack/blob/development/LICENSE)，但第三方容器及依赖要单独登记；本轮的 LinuxServer 容器是社区打包，BookStack 官方[不负责测试或支持社区 Docker 方案](https://www.bookstackapp.com/docs/admin/installation/)。正式交付要固定镜像 digest 并做版本/架构验证。
