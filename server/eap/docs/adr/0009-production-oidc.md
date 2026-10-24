# ADR-0009 生产 OIDC 登录边界（TASK-009）

状态：已接受　日期：2026-09-01　关联任务：TASK-009

## 决策

### 1. 标准 Authorization Code + PKCE

网关实现 `GET /enterprise/v1/auth/oidc/{login,callback,logout,profile}`：login 生成单次 state/nonce/PKCE(S256) 三元组并持久化到 `oidc_flow_states`（服务器端，浏览器不可见），302 到 IdP；callback 原子消费 state（重放/未知即拒）、带 verifier 换 code、校验 ID token（JWKS 签名 + iss/aud/nonce/exp）。客户端 secret 只在服务端。

### 2. 会话与浏览器存储零 token

登录成功后建立 `portal_sessions` 行，浏览器只拿到**不透明 session id**（HttpOnly + Secure + SameSite=Lax cookie，8h TTL）。OIDC 的 access/refresh/ID token 不落库、不下发浏览器；Portal 内存会话模型不变。logout 吊销会话行并清 cookie。`EAP_DEV_MODE=0` 时 dev-login 路由 404（测试锁定）。

### 3. 组名到 Workspace 的显式映射

IdP groups 是声明不是资源 ID：`idp_group_workspaces` 映射表转换；无映射组不产生任何 Workspace 权威；无映射成员的登录被拒并审计。会话内每次访问**即时重验成员资格**——登录后被移除成员的用户立即失去访问（403 + `membership_removed` 审计）。

### 4. 失败闭合分类（全部带审计）

`gateway.auth.failed` + `credential_type=oidc` + reason：`state_replay_or_unknown`（单次消费的 DELETE...RETURNING 原子保证）、`state_expired`、`token_exchange_failed`、`id_token_rejected`（签名/iss/aud/nonce/exp 分支细分，JWKS 验签只接受 EdDSA/ES256，拒绝 none/HS256/RSA）、`no_workspace_membership`、`membership_removed`。

### 5. 顺带修复的真实缺陷

TASK-003 的 `IsMember` SQL 在成员行被删除后返回空结果集被误判为存储错误（500）。正确契约：**无行 = 非成员**（干净 403）。已修复并由本卡测试锁定——这正是"移除成员 fail closed"验收要求的语义。

## 验证

`make verify-production-oidc` = verify-mcp-slice（含 foundation/identity）+ `TestProductionOIDCIntegration`：IdP stub（Ed25519 JWKS + PKCE 校验的 token 端点）驱动全浏览器流；forged signature / replayed state / logout 后重放 / removed membership 全部闭合且审计 ≥3 条；生产配置无 dev-login 路由；cookie 为 HttpOnly 不透明值。单测覆盖 ValidateIDToken 六个拒绝分支。
