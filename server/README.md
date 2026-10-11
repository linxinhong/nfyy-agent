# Yuanpu Server

中心服务的 Go 骨架，独立于 Electron 与本地 Agent 运行。设计见 [建设思路与待办](../docs/todos/yuanpu-server.md)。

当前已提供：Go HTTP 服务、OpenAPI 契约、Go/TS 类型生成、TS SDK、数据库就绪检查、带校验和的显式迁移、Compose 部署与联调脚本。账号、权限、Wiki、项目及任务队列尚未实现。现有 `src/` 下的 Node 技能目录服务继续使用 8787，本骨架默认使用 8788。

## 本地开发

需要 Go 1.26 或更新版本，以及仓库要求的 Node 24 / pnpm。以下命令均在仓库根目录执行：

```bash
pnpm install --frozen-lockfile
pnpm dev:server
```

默认只监听 `127.0.0.1:8788`。没有数据库配置时，`GET /v1/health` 返回 200，而 `GET /v1/ready` 返回 503 / `DATABASE_NOT_CONFIGURED`。

| 环境变量 | 用途 | 默认值 |
| --- | --- | --- |
| `YUANPU_SERVER_LISTEN` | HTTP 监听地址，支持端口 0 | `127.0.0.1:8788` |
| `YUANPU_DATABASE_URL` | PostgreSQL 连接串 | 未配置；就绪检查失败 |
| `YUANPU_TEST_DATABASE_URL` | Go 数据库集成测试使用的专用测试库 | 未配置时跳过该集成测试 |

数据库连接串不写入应用日志或 API 错误。当前业务认证尚未实现，默认部署只开放本机访问；后续接入账号权限后再提供面向律所网络的部署配置。

## Docker Compose

先准备未跟踪的环境文件，将 `YUANPU_DB_PASSWORD` 设置为随机的 URL 安全字符串，例如 `openssl rand -hex 24` 的输出：

```bash
cp server/deploy/.env.example server/deploy/.env
# 编辑 server/deploy/.env，填写随机密码。
docker compose --env-file server/deploy/.env -f server/deploy/compose.yaml up --build -d --wait
```

Compose 按顺序启动数据库、运行迁移、启动服务。数据库端口默认 `127.0.0.1:54328`，服务端口默认 `127.0.0.1:8788`，数据保存在项目专属 Docker volume 中。镜像通过 digest 固定；调整镜像时重新验证目标架构。

```bash
docker compose --env-file server/deploy/.env -f server/deploy/compose.yaml down
```

普通 `down` 保留数据库卷。不要给需要保留数据的部署追加 `--volumes`。

这份 Compose 是开发与部署骨架，当前数据库账号负责安装扩展和迁移。上线前需在账号权限阶段落实运行账号与迁移账号分离、网络入口、备份恢复和升级策略。尚未提供文件上传，文件数据卷随该功能加入。

## 数据库迁移

设置 `YUANPU_DATABASE_URL` 后运行：

```bash
pnpm migrate:server
```

迁移 SQL 嵌入 Go 二进制。`migrate` 使用事务和 advisory lock 串行执行，保存版本与 SHA-256；重复执行不重复应用，已修改的迁移历史和比当前二进制更新的数据库版本会被拒绝。首个迁移启用 pgvector，业务表由后续迁移追加。

`serve` 不自动执行 DDL。数据库无法连接或迁移未完成时，进程可继续提供存活检查，但就绪检查返回 503。数据库恢复且迁移完成后，无需重启服务即可恢复就绪。

## 接口与客户端

[api/openapi.yaml](api/openapi.yaml) 是 Go 和 TS 的共同契约。公开接口：

- `GET /v1/health`：存活状态、服务名、发布版本与 API 版本。
- `GET /v1/ready`：数据库连接和迁移状态；未就绪返回结构化错误。
- `GET /openapi.yaml`：当前二进制内嵌的 OpenAPI 文档。

所有响应带 `X-Request-Id`。错误形如 `{ error: { code, message, requestId } }`。未知路由返回 404，已知路由不支持的方法返回 405。本骨架不设置通配 CORS。

`/v1` 为协议兼容边界；`info.version` 与服务 `version` 表示发布版本。新增兼容字段或接口保留 v1，破坏性变更需新协议版本及迁移策略。当前健康响应含 `service: yuanpu-server`，SDK 会拒绝将旧目录服务的响应视为中心服务。

```bash
pnpm generate:server
pnpm --filter @yuanpu-agent/server-client run build
pnpm --filter @yuanpu-agent/server-client run example
```

示例默认连接 8788，可用 `YUANPU_SERVER_URL` 指定目标。数据库未就绪时示例以非零状态退出，并打印错误码与请求 ID。SDK 使用方法见 [Client README](../packages/yuanpu-server-client/README.md)。

## 验证

```bash
pnpm check:server
pnpm smoke:server:compose
pnpm check
```

`check:server` 检查生成文件是否与 OpenAPI 一致，运行 Go vet/测试，构建二进制与 SDK，验证 SDK 类型和行为，再启动真实 Go 子进程进行联调并检查退出。默认不依赖 Docker；数据库集成测试需要显式的测试库配置。

`smoke:server:compose` 使用随机项目名、随机密码和动态端口，构建并启动真实服务，运行数据库迁移集成测试和 SDK 调用，验证重启后就绪，最后清理自己的容器与数据卷。Docker 构建可通过 `GOPROXY` 指定当前网络可访问的 Go 模块代理，默认保留官方代理和校验机制。

```bash
# 仅指向专用测试数据库；测试会创建并清理独立 schema。
YUANPU_TEST_DATABASE_URL='<测试库连接串>' go -C server test ./internal/database -v
```

完整仓库检查已接入服务端检查。旧目录服务仍可通过 `pnpm dev:catalog` 运行；目录业务、制品下载和桌面中心配置的迁移由后续步骤完成。
