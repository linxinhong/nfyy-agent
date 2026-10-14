# Pi 1.0 npm 迁移

日期：2026-10-11。范围：在继续 Go Server 业务开发前，将本地 Pi 0.86.1 源码镜像切换到官方 npm 1.0.4，并使用内置 MCP。

## 已实现

- [x] Yuanpu 包改为精确 npm 依赖，锁定 Pi 家族的传递版本；移除源码镜像和专用同步、构建脚本。
- [x] Work 保留公开的 `pi-coding-agent` SDK；Assistant 通过窄适配接入 `pi-durable`，保留身份快照、动态记忆隔离、委派及取消行为。
- [x] 首次打开旧 Assistant 会话时导入已提交主分支，保留原文件与来源摘要，不重放未完成操作。
- [x] Work 启用 Pi 内置 MCP 和工具搜索，补齐启动、关闭钩子；兼容原配置路径、`disabled` 与环境变量引用。
- [x] 退役受管 `pi-mcp-adapter`，保留连接配置和停用状态；技能市场移除旧插件，提供独立的“MCP 服务”入口。
- [x] 保留 Yuanpu 签名 Python 能力包的安装、权限及进程治理；安装前检查同名 MCP 连接。
- [x] 移除未接入生产的旧 Pi Unix client/server 实验测试，历史结论保留在助理设计文档及 Git 中。

## 验证

- [x] `pnpm install --frozen-lockfile --ignore-pnpmfile --ignore-scripts`。
- [x] 旧会话导入、损坏文件拒绝、身份/记忆隔离测试；Assistant 共 53 项通过。
- [x] MCP 配置迁移、停用状态备份、工具搜索、stdio 调用与子进程退出专项测试。
- [x] 独立 CJS Runtime 经认证 Work API 调用 MCP 的集成测试。
- [x] agent-browser 浏览器预览检查：入口、连接开关、stdio/HTTP 切换、保存反馈；实际配置落盘由 Runtime API 测试覆盖。
- [x] 整仓 `pnpm check`，含 Go 检查与 smoke、Assistant 53 项、Runtime Kit 213 项、Runtime 138 项及其余包测试。
- [x] macOS arm64 `pnpm build:native` 与 `pnpm smoke:native`，覆盖 SEA 内置 MCP 调用及进程退出、冻结 Python 制品、SQLite/调度、Assistant Worker 和 Runtime 更新。

本轮还将原生 smoke 的数据库断言由旧 schema 15 对齐当前 schema 16；没有改变数据库迁移实现。

## 后续边界

- [ ] 在发布流水线验证 Windows/Linux 原生构建及签名；本机证据仅覆盖 macOS arm64。
- [ ] 如需 MCP Codemode，另行处理 Worker/QuickJS WASM 的 SEA 打包；当前配置映射到 deferred 工具搜索。
- [ ] 有 Azure 旧配置时，按上游 1.0.3 的 `azure` provider 更名执行迁移，不自动改写用户凭据。

`pi-durable` 仍是上游实验 API，升级补丁也需要查看变更并跑回归。集成细节、配置路径和回滚边界见 [Pi integration](../pi-integration.md)。Go Server 不依赖 Pi 内部会话/传输协议，下一阶段继续 [Server 建设待办](yuanpu-server.md)。
