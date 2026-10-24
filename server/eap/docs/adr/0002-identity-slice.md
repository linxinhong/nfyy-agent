# ADR-0002 员工身份与 API Key 切片（TASK-003）

状态：已接受　日期：2026-09-01　关联任务：TASK-003

## 背景

TASK-001 冻结了 Principal/Workspace/JWT/Error/Audit 契约。本切片交付员工身份闭环：登录（OIDC 适配边界）、`agk_*` 密钥生命周期、`/enterprise/v1/me`、audience 限定的 Token 交换、JWKS、限流与认证审计。

## 决策

### 1. OIDC 适配边界与开发模式

身份同步以 `store.IDClaims`（IdP subject/email/display_name/org/dept）为唯一输入，`UpsertUser`/`SyncMemberships` 幂等。开发态提供 dev-login 端点作为 IdP 替身（`EAP_DEV_MODE=1` 才启用，compose 默认开启；生产关闭后端点 404）。dev 模式下请求体即"已验证 id_token"——生产适配器必须从验证过的 IdP 响应产出同样 claims，业务代码无感。生产 SSO 属后续 slice。

### 2. API Key 形态与存储

- 格式 `agk_live_<32位base62>` / `agk_test_...`（≥128 bit 熵，`packages/apikey`）。
- 仅存 SHA-256 哈希（64 hex）+ 13 字符展示前缀；完整值仅创建响应返回一次。选 SHA-256 而非 bcrypt/argon2：密钥为高熵随机值不可穷举，且支持 O(1) 哈希查表（每请求一次查库）。
- 元数据：name、expires_at、last_used_at、last_ip、client_id、revoked_at。
- 创建/吊销与 `audit_events` 写入同一事务：不存在无审计证据的密钥状态变更。

### 3. 吊销生效窗口

**0 秒（立即）**。本切片每次认证直接读取 PostgreSQL 唯一事实源，无认证缓存。后续如引入缓存，必须带回明确的跨实例窗口并保持失败闭合。

### 4. 会话语义（对 ADR-0001 的显式细化）

`agent-gateway` audience 的 token 是控制面登录会话：签发于 Workspace 选择之前，**豁免 workspace 声明**（仅此一个 audience 豁免），且交换端点永不签发该 audience。能力 audience（llm-gateway/mcp-gateway/knowledge/audit/control-plane-api）仍强制 workspace 权威，TASK-001 的既有安全测试全部保持。

### 5. Token 交换（新公共端点，契约已冻结）

`POST /enterprise/v1/token/exchange`，鉴权 = `agk_*` key；`X-Agent-Workspace` 选择 Workspace，服务端成员资格校验失败即拒（含 resolver 故障）；audience 必须为注册内网 audience 且非 `agent-gateway`；签发复用 TASK-001 的 Ed25519/kid/TTL≤15min 契约。`gateway.token.exchanged` 审计写入失败 → 500，不出票（无审计证据的 token 不离柜）。

### 6. JWKS

`GET /.well-known/jwks.json`（OKP/Ed25519）。签名私钥存 PostgreSQL `signing_keys`（Zone B 服务端秘密；Vault/KMS 迁移留待后续），kid 寻址支持多 key 轮换，JWKS 公布 active + 历史 key。

### 7. 限流

Redis 固定窗口计数（`INCR`+bucket key），按 key 维度，`EAP_RATE_LIMIT_PER_MINUTE`（默认 120）。Redis 故障 fail-open 并显式记录——限流是滥用防护而非授权；授权歧义仍一律 fail closed。

### 8. 审计事件

认证失败（malformed/unknown/revoked/expired/workspace_denied/无效会话）→ `gateway.auth.failed`（denied）；密钥创建/吊销 → `identity.key.created/revoked`；token 交换 → `gateway.token.exchanged`。同步写入 `audit_events` 表（可靠路径，交付失败可观测）。

## 明确不做（本切片外）

LLM/MCP/Knowledge 能力路由、复杂关系授权、生产 OIDC 接入、认证缓存、管理员强制吊销、Rate limit 按用户全局聚合。

## 验证

`make verify-identity-slice` = verify-foundation + `TestIdentitySliceIntegration` + `TestIdentitySliceRateLimitIntegration`（真实 PG/Redis，一次性测试库跑全迁移）。
