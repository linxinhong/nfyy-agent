import { createRouter, RouterProvider } from "@tanstack/react-router";
import { StrictMode, useSyncExternalStore } from "react";
import { createRoot } from "react-dom/client";
import "./globals.css";
import { ErrorComponent } from "./__error";
import { NotFoundComponent } from "./__notFound";
import { UpdatingBanner, UpdatingScreen } from "./__updating";
import { routeTree } from "./routeTree.gen";
import { clearAutoReloadGuard, getSkewMode, installVersionSkewListeners, subscribeSkew } from "@/lib/utils/versionSkew";

/**
 * Router —— search 序列化用原生实现，不做任何覆写。
 *
 * bifrost 覆写了 parseSearch / stringifySearch，那是为了和 nuqs 的编解码格式对齐。
 * 本模板不用 nuqs（同一份 URL 状态两个事实源严格更差），于是：
 *   · 无需覆写序列化 —— 原生实现就是唯一实现
 *   · 各路由用 `validateSearch` 声明并校验自己的 search，带类型
 *   · ID 一律当**不透明字符串**，绝不 JSON.parse —— 大数字 ID 经 Number 强转丢精度，
 *     这是 bifrost 在注释里特意留下的教训
 *
 * validateSearch 用法：
 * ```ts
 * export const Route = createFileRoute("/items")({
 *   validateSearch: (raw: Record<string, unknown>) => ({
 *     page: typeof raw.page === "string" ? raw.page : undefined,
 *     q: typeof raw.q === "string" ? raw.q : undefined,
 *   }),
 * });
 * ```
 */
const router = createRouter({
	routeTree,
	defaultPreload: "intent",
	notFoundMode: "root",
	defaultErrorComponent: ErrorComponent,
	defaultNotFoundComponent: NotFoundComponent,
});

declare module "@tanstack/react-router" {
	interface Register {
		router: typeof router;
	}
}

declare global {
	interface Window {
		__appBooted?: boolean;
	}
}

// 版本偏移防护的第 2、3 层（第 1 层在 index.html 的 head 内联脚本里）。
// 顺序要紧：先装监听再渲染，否则首屏 chunk 失败会漏掉。
installVersionSkewListeners();

// 告知 index.html 的 boot 守卫「bundle 活着」，撤销它的 20s 超时兜底
window.__appBooted = true;

// 健康运行 30s 后清空重载预算 —— 否则长期累积会误封后续真实的偏移重载
setTimeout(clearAutoReloadGuard, 30_000);

/**
 * 偏移状态壳 —— 按 mode 决定渲染什么。
 *
 *   none → 正常渲染
 *   soft → app 继续跑 + 底部横幅（路由还活着，用户可继续工作并自行决定何时重载）
 *   hard → 整屏替换（路由已卸载，救不回来）
 *
 * 这个分档是整套机制的核心：一律整屏替换会打断正在填表的用户；
 * 一律只弹横幅则在路由已死时留下白屏。
 *
 * 用 useSyncExternalStore 而非 useState：偏移可能在 React 树之外发生
 * （捕获阶段的 error 事件、unhandledrejection），事实源必须在 React 之外。
 */
function Root() {
	const mode = useSyncExternalStore(subscribeSkew, getSkewMode, () => "none" as const);

	if (mode === "hard") return <UpdatingScreen />;
	return (
		<>
			<RouterProvider router={router} />
			{mode === "soft" && <UpdatingBanner />}
		</>
	);
}

const rootElement = document.getElementById("root");
if (!rootElement) throw new Error("#root 不存在 —— 检查 index.html");

createRoot(rootElement).render(
	<StrictMode>
		<Root />
	</StrictMode>,
);
