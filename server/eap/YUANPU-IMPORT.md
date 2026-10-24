# 知识服务中心来源

本目录从同一所有者的 `~/projects/mindlink-agent/server/eap` 导入，来源提交为 `16b480c`。导入网关、Portal、契约、迁移、部署、测试与 Go 包；未导入来源仓库的 `.tasks`、实验目录及构建产物。`api/schema.gen.ts` 来自该提交的 `packages/yuanpu-server-client/src/schema.gen.ts`，供 Portal 独立类型检查；更新 OpenAPI 时须重新生成它。

`../agent-runtime` 是模型整理进程的本地 Go 依赖，保留其 MIT `LICENSE` 和 `SOURCE.md`。本仓调整了 Portal 的展示名称、契约类型路径、桌面入口及本地开发端口；EAP 的身份、权限、审计和数据迁移行为保持来源实现。现有 `server/backend` 与 `server/web` 保留作旧数据迁移依据，不与 EAP 共用数据库。
