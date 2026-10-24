import { defineConfig } from "@playwright/test";

/**
 * E2E 配置 —— 直接访问统一入口（TASK-007）。
 *
 * 被测系统就是部署形态本身：网关托管 Portal SPA（EAP_WEB_DIST）并在同一
 * 端口上提供 API。不再依赖 Vite dev server；`make verify-portal-deploy`
 * 会先构建 dist 并拉起网关。
 */
export default defineConfig({
	testDir: "./e2e",
	timeout: 60_000,
	retries: 0,
	use: {
		baseURL: process.env.E2E_BASE_URL ?? "http://127.0.0.1:18081",
		locale: "zh-CN",
		trace: "retain-on-failure",
		// 可选：用已安装的 Chromium（版本与 Playwright 自带的不一致时）。
		...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ? { launchOptions: { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE } } : {}),
	},
});
