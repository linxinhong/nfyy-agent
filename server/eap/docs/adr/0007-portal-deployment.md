# ADR-0007 Portal 统一入口部署（TASK-007）

状态：已接受　日期：2026-09-01　关联任务：TASK-007

## 背景

TASK-005 交付了 CLI 生成的 Portal 源码与测试，但验收未覆盖"可访问部署"：网关不托管 UI，用户无法从统一入口看到页面，Playwright 依赖独立 Vite server。本卡补齐部署边界。

## 决策

### 1. 网关托管 SPA

`apps/web`（xingding-ui-cli 生成，ADR-0003）经 `pnpm exec vite build` 产出 dist；网关以 `EAP_WEB_DIST`（默认 `apps/web/dist`，compose 镜像内 `/web/dist`）托管，目录缺失则静态路由关闭（与 MCP/LLM 的可选装配一致）。网关只托管构建产物，UI 永远来自 CLI 生成的工程，不手写页面。

### 2. 路由优先级与 API 边界

静态托管注册在 ServeMux 的 `/` 兜底位：已注册的 API 模式（/enterprise、/v1、/mcp、/.well-known、/healthz、/readyz、/debug 及其子路径）天然优先。兜底内再判 isAPIPath：API 命名空间下的未知路径返回**冻结 JSON Problem**（404），绝不回退 index.html——机器客户端不会把 HTML 当 API 响应解析。其余路径：文件存在则直接服务（/assets/* 为内容哈希名），否则 SPA fallback 到 index.html（/portal/* 刷新可用）。

### 3. 安全与缓存基线（自动化断言）

- 安全头：Content-Security-Policy（default-src 'self'；脚本因 index 内联的主题/语言预探测脚本暂含 'unsafe-inline'，nonce 加固列入 follow-up）、X-Content-Type-Options: nosniff、X-Frame-Options: DENY、Referrer-Policy: strict-origin-when-cross-origin。
- 缓存：/assets/*（哈希文件名）public, max-age=31536000, immutable；index.html 与 SPA 路由 no-cache（员工永不运行旧壳）。
- 断言位置：apps/gateway/static_test.go（单测）+ scripts/portal-deploy-check.sh（对运行中网关）。

### 4. E2E 直打部署形态

Playwright 移除 Vite webServer，baseURL 即网关统一入口（默认 18081）。make portal-e2e 自包含：构建 dist → 以 EAP_WEB_DIST 拉起网关 → 跑浏览器流 → 清理。make verify-portal-deploy 另含 compose 路径断言：镜像多阶段构建（node:22 pnpm --frozen-lockfile 构建 web → Go 构建 → 运行时镜像内嵌 dist），make dev-up 后对 :18080 执行 portal-deploy-check。

## 明确不做

CDN/TLS 终结、生产反代拓扑、CSP nonce 加固（follow-up）、管理端。

## 验证

make verify-portal-deploy = verify-portal-shell（含 identity/foundation 门禁 + 单测 + 产物守卫）+ portal-deploy-compose（镜像构建 + dev-ready + 部署断言）+ portal-e2e（统一入口浏览器流）。make verify-m0-m3 的 Portal 环节自本卡起使用本目标。
