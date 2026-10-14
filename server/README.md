# Yuanpu Server

中心服务独立于 Electron 与本地 Agent 运行。设计见 [建设思路与待办](../docs/todos/yuanpu-server.md)。`server/` 下按工程划分：

| 目录 | 职责 | 开发端口 |
| --- | --- | --- |
| [`backend/`](backend/) | 独立 Go module，中心 API、身份、内容、技能目录、制品和部署 | 8788 |
| [`web/`](web/) | 独立 React/Vite 网站，通过同源 `/v1` API 访问中心 | 8789 |

桌面端仍在 `apps/app`；`packages/yuanpu-server-client` 是 Web、桌面共用的中心 API SDK。

当前已提供：Go HTTP 服务、OpenAPI 契约、Go/TS 类型生成、TS SDK、数据库就绪检查、带校验和的显式迁移、Compose 部署与联调脚本。身份模块支持单组织管理员初始化、团队和成员管理、四种基础角色、登录/登出、停用撤销会话，以及管理员向成员发放、查看和撤销有期限的 API key。内容模块已有资讯/Wiki/AI 能力的草稿、团队范围、提审、审核、发布和撤回 API 初版；网站目前展示公开知识与能力目录，成员登录后可按权限阅读已发布的组织资讯、知识与能力说明。投稿、审核、成员管理页面、附件、能力执行与完整权限审计仍待开发，因此未达到企业共享平台验收。

Go 骨架按星鼎 One 的静态装配边界整理：`internal/app` 连接身份、内容与 HTTP Host；`internal/httpapi` 只管理公共端点和模块挂载；`internal/httpkit` 统一请求 ID、响应、严格 JSON 与 Bearer 解析；`internal/identity`、`internal/content` 各自拥有路由和存储；`internal/authorization` 按当前用户及资源事实判断内容动作。业务模块不依赖 Host，Host 不导入业务模块，边界由测试检查。迁移仍需显式执行，服务启动只做就绪探针。复用范围和未移植部分见[框架决策](../docs/todos/yuanpu-server.md#星鼎-one-框架复用评估2026-10-12)。

## 本地开发

需要 Go 1.26 或更新版本，以及仓库要求的 Node 24 / pnpm。以下命令均在仓库根目录执行：

```bash
pnpm install --frozen-lockfile --ignore-pnpmfile
pnpm dev:server
pnpm dev:server:web
```

Go 后端默认只监听 `127.0.0.1:8788`；网站监听 `127.0.0.1:8789`，开发时将 `/v1` 和 `/openapi.yaml` 代理到 Go 后端。网站与后端独立构建；正式部署需在同源入口转发 API。没有数据库配置时，`GET /v1/health` 返回 200，而 `GET /v1/ready` 返回 503 / `DATABASE_NOT_CONFIGURED`；公开 Wiki 示例仍可浏览。

| 环境变量 | 用途 | 默认值 |
| --- | --- | --- |
| `YUANPU_SERVER_LISTEN` | HTTP 监听地址，支持端口 0 | `127.0.0.1:8788` |
| `YUANPU_DATABASE_URL` | PostgreSQL 连接串 | 未配置；就绪检查失败 |
| `YUANPU_ARTIFACT_ROOT` | 已签名能力包的 manifest 和归档目录 | 未配置；目录可搜索，制品返回 404 |
| `YUANPU_TEST_DATABASE_URL` | Go 数据库集成测试使用的专用测试库 | 未配置时跳过该集成测试 |

数据库连接串不写入应用日志或 API 错误。身份和内容接口要求认证，当前内容授权是按所有者、角色和团队的初版规则；默认部署只开放本机访问，内网开放需在统一授权、HTTPS 入口及备份恢复完成后验证。

## Docker Compose

先准备未跟踪的环境文件，将 `YUANPU_DB_PASSWORD` 设置为随机的 URL 安全字符串，例如 `openssl rand -hex 24` 的输出：

```bash
cp server/backend/deploy/.env.example server/backend/deploy/.env
# 编辑 server/backend/deploy/.env，填写随机密码。
docker compose --env-file server/backend/deploy/.env -f server/backend/deploy/compose.yaml up --build -d --wait
```

Compose 按顺序启动数据库、运行迁移、启动服务。数据库端口默认 `127.0.0.1:54328`，服务端口默认 `127.0.0.1:8788`，数据保存在项目专属 Docker volume 中。镜像通过 digest 固定；调整镜像时重新验证目标架构。

```bash
docker compose --env-file server/backend/deploy/.env -f server/backend/deploy/compose.yaml down
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

迁移完成后，可通过标准输入向 `go -C server/backend run ./cmd/yuanpu-server bootstrap-admin` 提供一次性 JSON：`{"email":"admin@example.com","name":"管理员","password":"<至少 12 字节的私密密码>"}`。这个命令仅在用户表为空时成功，密码只保存为 Argon2id 哈希；不要把真实密码写进仓库或命令参数。管理员调用 `POST /v1/admin/users` 时，服务生成随机初始密码并只在创建响应中返回一次。当前尚未提供自助改密和重置界面。

## 接口与客户端

[backend/api/openapi.yaml](backend/api/openapi.yaml) 是 Go 和 TS 的共同契约。公开接口：

- `GET /v1/health`：存活状态、服务名、发布版本与 API 版本。
- `GET /v1/ready`：数据库连接和迁移状态；未就绪返回结构化错误。
- `GET /v1/preview/wiki`：只读公开示例词条，包含正文、等级、经验值和关联词条；不读取或保存用户内容。
- `GET /v1/catalog/search`、`GET /v1/catalog/items/{id}`：技能目录搜索与详情，桌面和网站共用。
- `GET /v1/capability-packages/builtin.python.echo/manifest`、`GET /v1/capability-packages/builtin.python.echo/artifacts/{filename}`：只发布配置目录中的 manifest 和其中列出的合法归档。
- `POST /v1/auth/login`：账号密码换取 12 小时 bearer 会话；`GET /v1/auth/me` 与 `POST /v1/auth/logout` 验证或撤销会话。
- `POST /v1/auth/api-key`：用有效的 `ypk_` Key 换取同样的短期 bearer 会话。Key 仅在服务端保存哈希；过期、撤销或所属成员停用后不能再登录。
- `GET/POST /v1/admin/teams`、`GET/POST /v1/admin/users`、`PUT /v1/admin/users/{id}`：仅管理员可管理团队、成员角色和启停状态；停用成员会撤销会话，最后一名有效管理员不能被停用或降级。
- `GET/POST /v1/admin/api-keys`、`DELETE /v1/admin/api-keys/{id}`：管理员查询、发放、撤销成员 Key。发放响应只显示一次明文，查询只返回前缀和元数据；撤销时立即撤销此 Key 派生的会话。
- `GET/POST /v1/resources`、`GET/PUT /v1/resources/{id}`：按当前身份列出/读取已发布资源，或由创建者建立/修改草稿；`kind` 为 `news`、`wiki`、`capability`，可按标题/摘要查询。
- `POST /v1/resources/{id}/submit|approve|reject|withdraw`、`GET /v1/review/queue`：提审、审核、撤回和待审列表；资源历史版本保留在数据库，目前未提供历史浏览接口。
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
pnpm build:server:web
pnpm smoke:server:compose
pnpm check
```

`check:server` 检查生成文件是否与 OpenAPI 一致，运行 Go vet/测试，构建二进制与 SDK，验证 SDK 类型和行为，再启动真实 Go 子进程进行联调并检查退出。默认不依赖 Docker；数据库集成测试需要显式的测试库配置。

`smoke:server:compose` 使用随机项目名、随机密码和动态端口，构建并启动真实服务，运行数据库迁移集成测试和 SDK 调用，验证重启后就绪，最后清理自己的容器与数据卷。Docker 构建可通过 `GOPROXY` 指定当前网络可访问的 Go 模块代理，默认保留官方代理和校验机制。

```bash
# 仅指向专用测试数据库；测试会创建并清理独立 schema。
YUANPU_TEST_DATABASE_URL='<测试库连接串>' go -C server/backend test ./internal/database -v
```

完整仓库检查已接入服务端检查。技能目录和制品分发由 Go 中心提供；桌面开发会将本地签名制品目录传给它。生产部署可将制品目录只读挂载到服务容器，并将容器内路径设为 `YUANPU_ARTIFACT_ROOT`；未配置时目录可浏览，但不能下载能力包。

桌面开发命令 `pnpm dev` 会连接 `YUANPU_SERVER_URL` 指定的服务；未指定时使用 `http://127.0.0.1:8788`，若该地址尚无兼容服务则启动本机 Go 服务。Wiki 示例预览不依赖数据库；要看到“已就绪”及 Schema 版本，还需要按上文启动 PostgreSQL 并运行迁移。内容 API 尚未接入浏览器门户与桌面端，身份和后端审核接口可用不等于企业共享闭环已完成。原规划逐项满足度见[需求基线](../docs/todos/yuanpu-server-requirements-coverage.md)。
