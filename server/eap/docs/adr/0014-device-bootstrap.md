# ADR-0014 设备授权流与版本化 CLI Bootstrap（TASK-015）

状态：已接受　日期：2026-09-01　关联任务：TASK-015

## 决策

### 1. RFC 8628 设备流（网关管理授权）

`/enterprise/v1/auth/device/{code,approve,deny,token,logout}`：CLI 请求 device+user code → 员工在 Portal 用控制面会话批准/拒绝（批准即绑定当时成员资格的 Workspace）→ CLI 轮询换 access（aud=agent-gateway）+ refresh（30 天，仅存 SHA-256 哈希）。授权**单次消费**：token 签发即删除授权行，泄露的已批准码无法重复领取。

### 2. 轮询治理

pending 阶段超过 `devicePollSlowAt`（3 次）返回 slow_down；批准（人工动作）时 poll_count 清零，客户端按新间隔轮询必然成功；过期码返回 expired_token；拒绝返回 access_denied（带审计）。

### 3. 刷新令牌一次性轮换

refresh 换新时旧凭据标记 rotated，重放旧 token 一律 invalid_grant 并审计（refresh_replay_or_revoked）。成员被移除后刷新即失败（membership_removed 审计）；logout 显式吊销。凭据从不明文落库。

### 4. ETag 版本化 Bootstrap

`GET /enterprise/v1/cli/bootstrap`（设备 access 或 agk key 双凭据面）：manifest_version + user + workspace + endpoints；ETag 为清单内容哈希，If-None-Match 命中返回 304。**同一用户经设备凭据或 agk key 解析出等价 canonical Principal 与授权内容**（集成断言 user.id 一致）。

### 5. Principal 等价

设备 access 会话由 aud=agent-gateway 的内部 JWT 承载，subject 与 agk key 的 user_id 同源（users 表唯一事实源）——两种凭据在 bootstrap 中解析出同一 Principal。

## 验证

`make verify-device-bootstrap` = verify-m2-m5（含全链前级）+ `TestDeviceBootstrapIntegration`：pending/denied/slow_down/approve/轮换/重放/成员移除/logout 全闭合，bootstrap 200/304、ETag、Principal 等价、审计 ≥2 条——真实 PG/Redis。
