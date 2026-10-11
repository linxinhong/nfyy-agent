# TASK-073：Server / Client 骨架验证

日期：2026-10-11。设计基线提交：`8952125`。用户授权先提交设计，再实现第一步 Go Server 与 TS Client 骨架。

## 交付范围

- 根目录 `server/` 下的独立 Go module、HTTP 服务、环境配置、JSON 启动日志与有界退出。
- OpenAPI 单一契约，生成 Go DTO 与 TS 类型；生成一致性检查不修改源文件。
- `@yuanpu-agent/server-client`：健康与就绪接口、结构化错误、请求 ID、超时、取消与兼容性检查。
- PostgreSQL/pgvector 显式迁移：事务锁、版本与校验和、重复执行、历史篡改和未来 schema 拒绝。
- Docker Compose、独立构建/开发/迁移命令、真实联调与容器验证脚本。
- 根 `pnpm check` 接入中心检查，CI 增加 Go 工具链配置。

未交付账号权限、Wiki、文件同步、任务队列、管理 UI 或桌面中心账号接线。旧 Node 目录仍在 8787，Go 中心默认 8788。

## 验证结果

环境：macOS arm64，Node 24.15.0，pnpm 11.22.0，Go 1.26.0，Docker 29.2.1。Compose 实际验证 Linux arm64 镜像与 PostgreSQL 16/pgvector；镜像 digest 记录在部署文件中。

| 检查 | 结果 | 证据与边界 |
| --- | --- | --- |
| `pnpm check:server` | 通过 | OpenAPI 生成一致性、Go vet/测试、二进制构建、SDK 构建和类型检查、SDK 6 项测试、真实子进程联调 |
| SDK 行为 | 6/6 通过 | URL 前缀、请求与响应、结构化错误、旧目录协议拒绝、超时/取消、重定向拒绝、配置校验 |
| `pnpm smoke:server` | 通过 | 直接启动 Go 二进制与通过开发启动器启动均可调用；无数据库返回明确 503；SIGTERM 后正常退出 |
| `GOPROXY=https://goproxy.cn,direct pnpm smoke:server:compose` | 通过 | 真实容器构建、数据库就绪、迁移、SDK 调用、数据库停机/恢复、服务重启 |
| 数据库集成测试 | 2 项顶层测试通过 | 未配置/非法 DSN；独立 schema 中首次迁移、并发重复迁移、就绪、校验和篡改和未来 schema 拒绝 |
| 容器清理 | 通过 | 临时项目使用随机名称、密码与端口；清理自建容器、网络、卷与镜像；结束时无该测试前缀的容器 |
| `pnpm check` | 失败，既有问题 | 构建、Yuanpu 类型检查及中心检查通过；Runtime 测试 211/212，失败为原有内置技能顺序断言 |
| YAML、文档与 diff 检查 | 通过 | 任务注册表、CI、Compose、OpenAPI 可解析；链接与空白检查通过 |

完整检查失败位置为 `packages/yuanpu-runtime/test/builtin-agent-context.test.mjs:58`，断言 `markdown-visuals` 应排在 `low-ai-ui-design` 之前。该断言在本轮前的 Word 编辑器验证日志中已失败；本次未修改该 Runtime 源码或测试。完整检查不能标记为全绿，后续需单独处理该问题。

本机详细日志：`/tmp/yuanpu-server-check.log`、`/tmp/yuanpu-server-compose.log`、`/tmp/yuanpu-server-full-check.log`。此前失败对照：`/tmp/yuanpu-word-editor-check-final.log`。这些临时日志不作为部署依赖。

## 验证中修正的问题

- 容器访问官方 Go 模块代理超时：部署支持显式 `GOPROXY` 构建参数，验证使用本机已有代理；模块校验未关闭。
- 数据库集成测试的 schema 曾回退到 `public`，读到了服务已执行的迁移：测试改为仅使用自己的 schema。
- Docker 随机发布的端口在重启后可能变化：重启验证重新发现端口，再连接 SDK；正式默认端口为固定 8788。
- 开发启动器直接启动已构建二进制并转发信号，避免 `go run` 多层子进程的退出边界不明确。

## 保留事项

- Windows、Linux amd64、具体 NAS 与托管 CI 未验证；不能将本机和 Linux arm64 容器结果外推为这些平台已通过。
- 当前 Compose 只向本机发布端口；业务认证、运行数据库账号与迁移账号分离、远程访问及上线备份策略由后续阶段落实。
- 当前工作树原有桌面开发脚本、Python 制品文档和 Word 编辑器增量保留；依赖锁文件原有 importer 与 snapshot 解析结果保持不变，仅增加 Client 所需依赖。
- 下一步进入第一阶段账号、团队和资源范围权限设计与实现，不将骨架验收视为完整知识中心业务通过。
