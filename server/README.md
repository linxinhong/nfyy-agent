# 知识服务中心

当前中心复用 `~/projects/mindlink-agent/server/eap` 的 EAP Go 网关和 Portal，源码独立保存在 [`eap/`](eap/)；模型整理进程依赖的 Go 模块保存在 [`agent-runtime/`](agent-runtime/)。浏览器和桌面内嵌页面使用同一个网站，开发默认地址为 `http://127.0.0.1:8790`。来源与本仓调整见 [`eap/YUANPU-IMPORT.md`](eap/YUANPU-IMPORT.md)。

```bash
pnpm build:server:web  # 安装、检查并构建 Portal
pnpm build:server      # 构建网关和整理进程
EAP_DATABASE_URL='postgres://...' EAP_REDIS_ADDR='127.0.0.1:16379' pnpm dev:server
pnpm check:server      # EAP Go、运行时与 Portal 检查
```

网关需要 PostgreSQL 和 Redis；`pnpm dev:server` 同时启动网关和自动整理进程，网关启动时执行 EAP 迁移。未同时设置 `EAP_CURATOR_MODEL` 与 `EAP_CURATOR_API_KEY` 时使用规则版整理器。投稿进入整理队列，生成的页面审核发布后才在网站显示。内嵌网站通过本身的成员登录建立会话。原 `server/backend`、`server/web` 及其未提交改动暂时保留在下方，供既有数据和客户端契约迁移使用；它们不再是默认中心服务。旧命令以 `:legacy` 后缀运行。

## 旧版中心（保留供迁移）

中心服务独立于 Electron 与本地 Agent 运行。通用建设见[建设思路与待办](../docs/todos/yuanpu-server.md)，律所行业包见[技术设计](../docs/legal-knowledge-platform-technical-design.md)和[落地与验收清单](../docs/todos/legal-knowledge-platform.md)。`server/` 下按工程划分：

| 目录 | 职责 | 开发端口 |
| --- | --- | --- |
| [`backend/`](backend/) | 独立 Go module，中心 API、身份、内容、技能目录、制品和部署 | 8788 |
| [`web/`](web/) | 独立 React/Vite 网站，通过同源 `/v1` API 访问中心 | 8789 |

桌面端仍在 `apps/app`；`packages/yuanpu-server-client` 是 Web、桌面共用的中心 API SDK。

当前已提供：Go HTTP 服务、OpenAPI 契约、Go/TS 类型生成、TS SDK、数据库就绪检查、带校验和的显式迁移、Compose 部署与联调脚本。身份模块支持单组织管理员初始化、团队和成员管理、四种基础角色、登录/登出、停用撤销会话，以及管理员向成员发放、查看和撤销有期限的 API key。内容模块已有资讯/Wiki/AI 能力的草稿、团队范围、提审、审核、发布和撤回 API 初版；网站提供成员内容工作台和管理员后台。律所行业包、提交前校验和 BookStack 可选适配层已加入，但案件级权限、Agent 流水线与完整权限审计尚未实现，因此未达到律所方案的生产验收。

Go 骨架按星鼎 One 的静态装配边界整理：`internal/app` 连接身份、内容与 HTTP Host；`internal/httpapi` 只管理公共端点和模块挂载；`internal/httpkit` 统一请求 ID、响应、严格 JSON 与 Bearer 解析；`internal/identity`、`internal/content` 各自拥有路由和存储；`internal/authorization` 按当前用户及资源事实判断内容动作。业务模块不依赖 Host，Host 不导入业务模块，边界由测试检查。迁移仍需显式执行，服务启动只做就绪探针。复用范围和未移植部分见[框架决策](../docs/todos/yuanpu-server.md#星鼎-one-框架复用评估2026-10-12)。

## 本地开发

需要 Go 1.26 或更新版本，以及仓库要求的 Node 24 / pnpm。以下命令均在仓库根目录执行：

```bash
pnpm install --frozen-lockfile --ignore-pnpmfile
pnpm dev:server:legacy
pnpm dev:server:web:legacy
```

Go 后端默认只监听 `127.0.0.1:8788`；网站监听 `127.0.0.1:8789`，开发时将 `/v1` 和 `/openapi.yaml` 代理到 Go 后端。网站与后端独立构建；正式部署需在同源入口转发 API。没有数据库配置时，`GET /v1/health` 返回 200，而 `GET /v1/ready` 返回 503 / `DATABASE_NOT_CONFIGURED`；公开 Wiki 示例仍可浏览。

| 环境变量 | 用途 | 默认值 |
| --- | --- | --- |
| `YUANPU_SERVER_LISTEN` | HTTP 监听地址，支持端口 0 | `127.0.0.1:8788` |
| `YUANPU_DATABASE_URL` | PostgreSQL 连接串 | 未配置；就绪检查失败 |
| `YUANPU_ARTIFACT_ROOT` | 已签名能力包的 manifest 和归档目录 | 未配置；目录可搜索，制品返回 404 |
| `YUANPU_TEST_DATABASE_URL` | Go 数据库集成测试使用的专用测试库 | 未配置时跳过该集成测试 |
| `YUANPU_INDUSTRY_ROOT`、`YUANPU_INDUSTRY_ID` | 可选外部行业包目录及当前行业 | 内置行业包、`legal` |
| `YUANPU_BOOKSTACK_URL`、`YUANPU_BOOKSTACK_TOKEN_ID`、`YUANPU_BOOKSTACK_TOKEN_SECRET` | 可选 BookStack REST API 及服务端 Token，三者须同时设置 | 未启用 |
| `YUANPU_BOOKSTACK_ALLOW_HTTP` | 仅隔离内网开发时允许明文 HTTP 连接 BookStack | `false` |

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

这份 Compose 是开发与部署骨架，当前数据库账号负责安装扩展和迁移。上线前需在账号权限阶段落实运行账号与迁移账号分离、网络入口、备份恢复和升级策略。首版文件上传保存在 PostgreSQL，单文件上限 2 MiB；扩大规模前应迁往对象存储并补充恶意文件扫描。

律所方案的 BookStack 展示/编辑层可选择 `server/backend/deploy/compose.bookstack.yaml` 叠加启动。先按[律所平台落地清单](../docs/todos/legal-knowledge-platform.md)生成独立的 BookStack 应用密钥和 MariaDB 密码，设置 `YUANPU_BOOKSTACK_PUBLIC_URL`，将初始管理员凭据改掉，随后在 BookStack 内创建最小权限 API Token。Go 端填入 `YUANPU_BOOKSTACK_URL=http://bookstack`、Token ID/Secret 与 `YUANPU_BOOKSTACK_ALLOW_HTTP=true` 才启用连接探针；内部 HTTP 只能存在于隔离的 Compose 网络，用户访问仍需 HTTPS 入口。BookStack 的数据库和附件卷要与 PostgreSQL 一起备份。

## 数据库迁移

设置 `YUANPU_DATABASE_URL` 后运行：

```bash
pnpm migrate:server:legacy
```

迁移 SQL 嵌入 Go 二进制。`migrate` 使用事务和 advisory lock 串行执行，保存版本与 SHA-256；重复执行不重复应用，已修改的迁移历史和比当前二进制更新的数据库版本会被拒绝。首个迁移启用 pgvector，业务表由后续迁移追加。

Schema 7 曾移除旧密码字段并清除未绑定 API key 的历史会话；Schema 8 重新加入可选的 Argon2id 密码字段和密码会话。已有成员不会自动获得密码，管理员需在后台单独设置。API key 会话继续与发放记录绑定，撤销 Key 会使相应会话失效。

`serve` 不自动执行 DDL。数据库无法连接或迁移未完成时，进程可继续提供存活检查，但就绪检查返回 503。数据库恢复且迁移完成后，无需重启服务即可恢复就绪。

迁移完成后，可通过标准输入向 `go -C server/backend run ./cmd/yuanpu-server bootstrap-admin` 提供一次性 JSON：`{"email":"admin@example.com","name":"管理员"}`。这个命令仅在用户表为空时成功，并在标准输出显示一次初始管理员 API key。请将 Key 存入凭据管理器；服务只保存其哈希。管理员在网站后台为成员设置密码。普通浏览器使用账号密码进入个人中心；yuanpu-agent 内嵌浏览器首次使用账号密码登录后，会发放设备专用 API key 并加密保存到本机，后续凭 Key 自动进入中心。管理员仍可手动发放和撤销 Key。

## 接口与客户端

[backend/api/openapi.yaml](backend/api/openapi.yaml) 是 Go 和 TS 的共同契约。公开接口：

- `GET /v1/health`：存活状态、服务名、发布版本与 API 版本。
- `GET /v1/ready`：数据库连接和迁移状态；未就绪返回结构化错误。
- `GET /v1/preview/wiki`：只读公开示例词条，包含正文、等级、经验值和关联词条；不读取或保存用户内容。
- `GET /v1/catalog/search`、`GET /v1/catalog/items/{id}`：技能目录搜索与详情，桌面和网站共用。
- `GET /v1/capability-packages/builtin.python.echo/manifest`、`GET /v1/capability-packages/builtin.python.echo/artifacts/{filename}`：只发布配置目录中的 manifest 和其中列出的合法归档。
- `POST /v1/auth/api-key`：用有效的 `ypk_` Key 换取 12 小时 bearer 会话；`GET /v1/auth/me` 与 `POST /v1/auth/logout` 验证或撤销会话。Key 仅在服务端保存哈希；过期、撤销或所属成员停用后不能再登录。
- `GET/POST /v1/admin/teams`、`GET/POST /v1/admin/users`、`PUT /v1/admin/users/{id}`：仅管理员可管理团队、成员角色和启停状态；停用成员会撤销会话，最后一名有效管理员不能被停用或降级。
- `GET/POST /v1/admin/api-keys`、`DELETE /v1/admin/api-keys/{id}`：管理员查询、发放、撤销成员 Key。发放响应只显示一次明文，查询只返回前缀和元数据；撤销时立即撤销此 Key 派生的会话。
- `GET /v1/admin/roles`、`PUT /v1/admin/roles/{role}/{action}`：管理员维护角色、动作和范围的权限矩阵；变更对现有会话的下一次请求立即生效。
- `GET/POST /v1/resources`、`GET/PUT /v1/resources/{id}`：按当前身份列出/读取已发布资源，或由创建者建立/修改草稿；`kind` 为 `news`、`wiki`、`capability`，可按标题/摘要查询。
- `GET /v1/resources/mine`、`POST /v1/resources/{id}/submit|approve|reject|withdraw`、`GET /v1/review/queue`：作者确认提审、独立审核、撤回和工作台列表；资源历史版本保留在数据库，目前未提供历史浏览接口。
- `POST /v1/logs`：上传限定字段的结构化运行记录，以成员和幂等键去重，不接收对话正文。
- `POST /v1/data-uploads`、`GET /v1/data-uploads/{id}`：上传、读取不超过 2 MiB 的私有数据文件，可关联作者的资源；当前仅上传者可下载。
- `GET /v1/industry`、`GET /v1/industry/templates/{type}`、`POST /v1/industry/validate`：认证后取得当前行业的知识类型、模板与提交前字段校验。校验不写入 BookStack，也不代表脱敏复查或人工审核完成。
- `GET /v1/integrations/bookstack/status`：仅配置 BookStack 服务端 Token 后可用，认证后测试 API 连接；不会向客户端返回 Token 或页面内容。
- `GET /openapi.yaml`：当前二进制内嵌的 OpenAPI 文档。

所有响应带 `X-Request-Id`。错误形如 `{ error: { code, message, requestId } }`。未知路由返回 404，已知路由不支持的方法返回 405。本骨架不设置通配 CORS。

`/v1` 为协议兼容边界；`info.version` 与服务 `version` 表示发布版本。新增兼容字段或接口保留 v1，破坏性变更需新协议版本及迁移策略。当前健康响应含 `service: yuanpu-server`，SDK 会拒绝将旧目录服务的响应视为中心服务。

```bash
pnpm generate:server:legacy
pnpm --filter @yuanpu-agent/server-client run build
pnpm --filter @yuanpu-agent/server-client run example
```

示例默认连接 8788，可用 `YUANPU_SERVER_URL` 指定目标。数据库未就绪时示例以非零状态退出，并打印错误码与请求 ID。SDK 使用方法见 [Client README](../packages/yuanpu-server-client/README.md)。

## 验证

```bash
pnpm check:server
pnpm build:server:web:legacy
pnpm smoke:server:compose:legacy
pnpm check
```

`check:server` 检查生成文件是否与 OpenAPI 一致，运行 Go vet/测试，构建二进制与 SDK，验证 SDK 类型和行为，再启动真实 Go 子进程进行联调并检查退出。默认不依赖 Docker；数据库集成测试需要显式的测试库配置。

`smoke:server:compose` 使用随机项目名、随机密码和动态端口，构建并启动真实服务，运行数据库迁移集成测试和 SDK 调用，验证重启后就绪，最后清理自己的容器与数据卷。Docker 构建可通过 `GOPROXY` 指定当前网络可访问的 Go 模块代理，默认保留官方代理和校验机制。

```bash
# 仅指向专用测试数据库；测试会创建并清理独立 schema。
YUANPU_TEST_DATABASE_URL='<测试库连接串>' go -C server/backend test ./internal/database -v
```

完整仓库检查已接入服务端检查。技能目录和制品分发由 Go 中心提供；桌面开发会将本地签名制品目录传给它。生产部署可将制品目录只读挂载到服务容器，并将容器内路径设为 `YUANPU_ARTIFACT_ROOT`；未配置时目录可浏览，但不能下载能力包。

桌面开发命令 `pnpm dev` 会连接 `YUANPU_SERVER_URL` 指定的 Go API（默认 `http://127.0.0.1:8788`），并在桌面内嵌 `YUANPU_CENTER_URL` 指定的中心网站（默认 `http://127.0.0.1:8789`）。中心端点可在桌面设置中修改；远程地址须使用 HTTPS。本地无兼容 Go 服务时开发命令会启动服务。Wiki 示例预览不依赖数据库；要看到“已就绪”及 Schema 版本，还需要按上文启动 PostgreSQL 并运行迁移。浏览器门户已提供投稿、审核与角色配置工作台；桌面端目前只读取已发布资源，日志和数据上传 SDK 可供后续接入。原规划逐项满足度见[需求基线](../docs/todos/yuanpu-server-requirements-coverage.md)。
