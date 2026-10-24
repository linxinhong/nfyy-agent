/// <reference types="vitest/config" />
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

/**
 * 路由约定 —— 只有 layout.tsx / __root.tsx 是路由，其余一切都是普通模块。
 *
 * 心智模型保留 Next.js 的「page.tsx 装内容」，但路由声明集中在 layout.tsx 一处。
 * 目录结构：
 *   app/<name>/layout.tsx   ← 路由（createFileRoute）
 *   app/<name>/page.tsx     ← 纯视图，被同级 layout.tsx import
 *   app/<name>/views/       ← 路由私有组件
 *
 * 两个必须知道的坑（都实测确认过，不是推测）：
 *
 * 1. 这个 pattern 匹配的是**裸 dirent 名**（生成器逐目录 readdir，传入 `d.name`），
 *    不是相对路径。所以 `^layout\.tsx$` 这样锚定文件名是正确的；写成带斜杠的
 *    `dashboard/layout.tsx` 永远不会命中。目录名没有扩展名故不被匹配，递归照常。
 *
 * 2. `_` 前缀的目录在 routeToken:"layout" 下会被判成 **pathless layout** ——
 *    该段不进 URL。所以 `app/_preview/$id/layout.tsx` 会退化成顶层 `/$id`，
 *    捕获所有一级路径并与真实路由冲突。要字面保留下划线需写成 `[_]preview`，
 *    但那个目录名在各类 ignore glob 里 `[_]` 是字符类会静默错配 —— 建议直接
 *    改用无下划线的目录名（如 `dev-preview`）。
 */
const ROUTE_FILES = "^(?!layout\\.tsx$|__root\\.tsx$).+\\.(tsx|ts|jsx|js)$";

const apiTarget = process.env.VITE_API_TARGET ?? "http://127.0.0.1:8790";

export default defineConfig({
	plugins: [
		tanstackRouter({
			target: "react",
			routesDirectory: "./app",
			generatedRouteTree: "./app/routeTree.gen.ts",
			routeToken: "layout",
			routeFileIgnorePattern: ROUTE_FILES,
			autoCodeSplitting: true,
		}),
		react(),
		tailwindcss(),
	],
	resolve: {
		alias: {
			"@": path.resolve(__dirname),
		},
	},
	server: {
		// 固定端口 —— Playwright webServer 探测与 README 文档都依赖它；
		// 3000 等常用端口易被其他项目占用，选 3100 并 strictPort 快速失败。
		port: 3100,
		strictPort: true,
		// 显式绑 IPv4 —— Playwright 与脚本都以 127.0.0.1 探测，
		// 默认的 `localhost` 在本机会落到 ::1 而探活失败。
		host: "127.0.0.1",
		proxy: {
			// EAP 网关的企业 API 与 JWKS —— 浏览器同源 /enterprise/*，
			// 由 dev server 转发到网关（VITE_API_TARGET 可覆盖）。
			"/enterprise": { target: apiTarget, changeOrigin: true },
			"/.well-known": { target: apiTarget, changeOrigin: true },
		},
	},
	build: {
		outDir: "dist",
		emptyOutDir: true,
	},
	// vitest 单测 —— Playwright 的 e2e/*.spec.ts 由 `pnpm e2e` 独立驱动，
	// 不属于 vitest 收集范围。
	test: {
		exclude: ["**/node_modules/**", "**/dist/**", "e2e/**"],
	},
});
