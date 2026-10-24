# ADR-0003 Portal 壳与客户端分发（TASK-005）

状态：已接受　日期：2026-09-01　关联任务：TASK-005

## 背景

Portal 由 `xingding-ui-cli` 一次性生成并纳入本仓库（`apps/web`），承载员工身份上下文、API Key 生命周期与 Agent Center 分发。脚手架自带私有 Agent Runtime 面板与本地对话持久化，按任务边界必须移除。

## 决策

### 1. 脚手架处置

生成后的代码即 EAP 源码，脚手架不是运行时依赖。移除项（禁止项清理）：

- `components/agent/*`（对话面板，含 localStorage 会话持久化）；
- `lib/api/agentRuntime.ts`（私有 `/runtime/agents/*` 契约）；
- `lib/domain/agentChat.*`；
- appShell 中的面板集成与 `app/workspace/*` 示例页。

保留项：设计系统、TanStack Router/Query、i18n、主题、版本偏移防护（versionSkew）、命令面板。

### 2. 会话安全

控制面会话（dev-login 签发的内部 JWT，aud=agent-gateway）只存于 JS 内存（`lib/auth/session.ts`，useSyncExternalStore 单例）。**不写 localStorage/sessionStorage/cookie**；刷新即失效重新登录。路由守卫（/portal/* beforeLoad）在无会话时不渲染、不请求。浏览器测试断言存储中无 token、无 key 值。

### 3. 新增网关端点（增量，不破坏既有契约）

`GET /enterprise/v1/profile` —— 会话态档案，载荷与 `/enterprise/v1/me` 相同（MeResponse）。`/me` 保持 agk key 专属（客户端面）；profile 服务 Portal 会话面。两个面的鉴权互斥有测试锁定（key 打 profile 401、session 打 me 401）。

### 4. 客户端配置分发

下载的客户端配置只含端点、Workspace 与说明（`lib/utils/clientConfig.ts`），**不含任何密钥**（单元测试与浏览器测试双重断言 `agk_` 不出现）。员工自行把一次性展示的 key 填入客户端安全存储。Agent Center V1 为静态目录（`lib/constants/agentClients.ts`），后续接控制面 API。

### 5. 一次性 Key 展示

完整 key 值只存在于创建响应 → 对话框组件状态；关闭即消失，列表只有前缀。无任何持久化路径。

## 验证

`make verify-portal-shell` = verify-identity-slice（含 foundation 全部门禁）+ `pnpm check`（typecheck/lint/format/vitest）+ 生产构建 + **产物守卫**（`grep runtime/agents dist/` 必须为空）+ Playwright 浏览器流（登录 → Workspace 选择 → 一次性 key → 配置下载 → 吊销 → 服务端 401 断言；Agent Center 目录与安全配置下载）。

## 明确不做

完整聊天 UI、客户端上传 DLP、管理员控制台、自动灰度（V1 边界，PRD §11）。
