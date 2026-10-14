# 元朴中心 Web

独立的 React/Vite 网站，位于 `server/web`，与 `server/backend` 分开构建。开发时先在仓库根目录运行 `pnpm dev:server`，再运行 `pnpm dev:server:web`，打开 `http://127.0.0.1:8789`。Web 使用 `@yuanpu-agent/server-client` 调用同源 `/v1` API，Vite 开发代理默认指向 `http://127.0.0.1:8788`，可通过 `YUANPU_SERVER_API_URL` 改写。

执行 `pnpm build:server:web` 生成静态文件。正式部署时由反向代理将 `/v1/*` 和 `/openapi.yaml` 转发至 Go 后端，其余请求交给 Web 静态站点。首页提供资讯、知识、能力目录与服务状态；访客可阅读公开示例和目录，成员登录后可阅读 Go API 按账号权限返回的已发布组织资源。管理员登录后可进入“后台管理”，向有效成员发放、查看和撤销有期限的桌面 API key；明文 Key 只在发放时显示一次。令牌只存当前页面内存，刷新页面需重新登录。投稿、审核、成员管理及能力在线执行仍需后续业务页面，当前不以公开示例充当真实企业内容。
