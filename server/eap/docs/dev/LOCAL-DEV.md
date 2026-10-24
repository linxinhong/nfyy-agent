# 本地开发环境

TASK-001 之后的每个 slice 都基于这套本地环境开发与验收。

## 前置条件

- Go >= 1.27（bifrost v1.8.4 内核要求，ADR-0004）
- Docker（含 compose v2）
- make、curl

## 一键启动全部本地服务

```bash
make dev-up      # 启动 PostgreSQL 16 / Redis 7 / MinIO / gateway（compose 构建）
make dev-ready   # 轮询断言 /healthz 与 /readyz 通过
```

gateway 启动时自动应用 `migrations/`（`schema_migrations` 记录版本，幂等）。

## 只启动基础设施、从源码运行 gateway

```bash
make dev-infra       # 仅 PostgreSQL + Redis + MinIO
make run-gateway     # go run ./apps/gateway
make dev-ready       # 健康检查
```

## 服务与端口

本地端口统一避开常见占用（5432/6379/8080 常被其他项目使用）：

| 服务 | 本地地址 | 说明 |
|---|---|---|
| gateway | http://127.0.0.1:18080 | 唯一公共入口 |
| PostgreSQL | 127.0.0.1:15432 (eap/eap, db: eap) | 元数据 / ACL / 审计 |
| Redis | 127.0.0.1:16379 | 缓存 / 任务 / 限流 |
| MinIO | 127.0.0.1:19000（console :19001） | 对象存储 |

## 身份切片快速体验（dev 模式）

compose 内 gateway 以 `EAP_DEV_MODE=1` 运行，dev-login 充当 IdP 替身（仅开发）：

```bash
# 1. 登录（同步用户 + Workspace 成员资格），获得控制面会话
curl -sS -X POST http://127.0.0.1:18080/enterprise/v1/auth/dev-login \
  -d '{"email":"li.lei@example.com","display_name":"Li Lei","workspaces":["secops"]}'

# 2. 用会话创建 agk_* key（完整值仅此一次展示）
SESSION=<上一步返回的 session_token>
curl -sS -X POST http://127.0.0.1:18080/enterprise/v1/keys \
  -H "Authorization: Bearer $SESSION" -d '{"name":"MacBook Harness"}'

# 2b. 可选：创建绑定 Workspace 的 key（ADR-0024，只能绑定自己所属的 Workspace）。
# 绑定后客户端无需再发送 X-Agent-Workspace：header 缺省时自动落在绑定 Workspace
# 上，成员资格仍然每次请求校验；显式 header 依旧优先并受成员资格约束。
curl -sS -X POST http://127.0.0.1:18080/enterprise/v1/keys \
  -H "Authorization: Bearer $SESSION" \
  -d '{"name":"CI bot","workspace":"secops"}'

# 3. 用 agk key 查询身份与 Workspace
KEY=<上一步返回的 key>
curl -sS http://127.0.0.1:18080/enterprise/v1/me -H "Authorization: Bearer $KEY"

# 4. 换取下游 audience 的短时内部 JWT（Workspace 服务端校验）
curl -sS -X POST http://127.0.0.1:18080/enterprise/v1/token/exchange \
  -H "Authorization: Bearer $KEY" -H "X-Agent-Workspace: secops" \
  -d '{"audience":"mcp-gateway"}'

# 5. JWKS（下游本地验签用）
curl -sS http://127.0.0.1:18080/.well-known/jwks.json
```

吊销后立即失效（`DELETE /enterprise/v1/keys/{key_id}`，会话鉴权）；被吊销的 key 再访问任何企业端点返回 401 并写入 `gateway.auth.failed` 审计事件。

## 健康与就绪检查

```bash
curl -sS http://127.0.0.1:18080/healthz   # 存活：进程正常即 200
curl -sS http://127.0.0.1:18080/readyz    # 就绪：PostgreSQL + Redis 探活全部成功
```

`/readyz` 任一依赖失败返回 503，并给出失败项名称（不泄露凭据）。

## Trace 传播验证

```bash
curl -sS -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' \
     http://127.0.0.1:18080/debug/request
```

入站合法 `traceparent` 被保留并在响应头回传；缺失时网关生成新的 trace id 与 `X-Request-ID`。

## Portal 前端（apps/web）

```bash
make dev-up                       # 网关托管 UI：http://127.0.0.1:18080/ 即 Portal
```

开发模式（热更新，可选）：

```bash
make dev-infra && cd apps/web && pnpm install
pnpm dev                          # http://127.0.0.1:3100（/enterprise 代理到网关 18081/18080）
pnpm check && pnpm exec vite build
```

浏览器 E2E 直打统一入口（无 Vite server）：`pnpm e2e`（需 18081 有托管 dist 的网关，`make verify-portal-deploy` 自动完成）。

会话只存内存：刷新页面需重新 dev-login（有意为之，见 ADR-0003）。

## 停止

```bash
make dev-down   # 停止，保留数据卷
make dev-nuke   # 停止并删除数据卷（本地开发数据会丢失）
```

## 环境变量（gateway）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `EAP_GATEWAY_ADDR` | `:8080` | 监听地址 |
| `EAP_DATABASE_URL` | `postgres://eap:eap@127.0.0.1:15432/eap?sslmode=disable` | PostgreSQL |
| `EAP_REDIS_ADDR` | `127.0.0.1:16379` | Redis |
| `EAP_MIGRATIONS_DIR` | `migrations` | 迁移 SQL 目录 |
| `EAP_TOKEN_ISSUER` | `https://agent.company.com/` | 内部 JWT iss |
| `EAP_RATE_LIMIT_PER_MINUTE` | `120` | 每 key 每分钟请求上限（Redis 不可用时 fail-open 并记录） |
| `EAP_DEV_MODE` | 未设置 | `1` 启用 dev-login（生产禁用） |

## 仓库验证目标

```bash
make verify-foundation        # gofmt + vet + build + test + race（基础切片门禁）
make verify-identity-slice    # 上者 + 真实 PG/Redis 上的身份闭环集成测试（TASK-003 验收）
```
