import { Sidebar, SidebarProvider, SidebarTrigger } from "@/components/ui/sidebar";
import { CommandPaletteProvider } from "@/components/commandPalette";
import { AppSidebar } from "@/components/appSidebar";
import { SiteShell } from "@/components/siteShell";
import { ThemeProvider } from "@/components/themeProvider";
import { I18nProvider } from "@/components/i18nProvider";
import { queryClient } from "@/lib/api/queryClient";
import { flattenPages } from "@/lib/constants/nav";
import { QueryClientProvider } from "@tanstack/react-query";
import { useLocation, useMatches } from "@tanstack/react-router";
import { type ReactNode } from "react";
import { Toaster } from "sonner";

/**
 * 布局壳 —— 两档，由路由 staticData 声明式选择。
 *
 *   console（默认）—— 侧栏 + 完整 chrome
 *   minimal        —— 无侧栏、无导航（登录页、OAuth 回调、公开分享页）
 *
 * 为什么是声明式而非判 pathname：路由自己最清楚它需要什么壳，判 pathname
 * 会随着路由重命名而静默失效，且新增路由时容易漏。
 *
 * 两档是诚实的数量。bifrost 有四档（多出 temp-token 的 per-visitor 判定和
 * 路由自建壳），那是它的业务需求；模板声明用不到的档位等于能力过度声明，
 * 真需要第三档时再加。
 *
 * 用法：
 * ```ts
 * export const Route = createFileRoute("/login")({
 *   staticData: { shell: "minimal" },
 *   component: LoginPage,
 * });
 * ```
 */
export type ShellKind = "console" | "minimal";

interface ShellStaticData {
	shell?: ShellKind;
}

function resolveShell(matches: ReturnType<typeof useMatches>): ShellKind {
	let kind: ShellKind = "console";
	// 从外到内遍历，最内层的声明胜 —— 子路由可以覆盖父路由的选择
	for (const match of matches) {
		const declared = (match.staticData as ShellStaticData | undefined)?.shell;
		if (declared) kind = declared;
	}
	return kind;
}

/**
 * 三层层次 —— 全程零阴影。
 *
 *   L1 画布   body 的 bg-background 透过**透明侧栏**显出来
 *   L2 白卡片 主内容区 my-2 mr-2 + 自身 border + 圆角 → 8px 沟槽完成分隔
 *   L3 卡内   border 分隔子区块
 *
 * 关键动作是侧栏 `border-none bg-transparent`（见 components/appSidebar.tsx）——
 * 它不画自己的边界也不画自己的底。若给侧栏加右边框，三层深度立刻退化成两层平面。
 *
 * 窄屏（md 以下）退化为全屏无圆角无边距，并显示顶部移动 header。
 */
function ConsoleShell({ children }: { children: ReactNode }) {
	const pathname = useLocation({ select: (location) => location.pathname });
	const mobileTitle = flattenPages().find((page) => pathname === page.to || pathname.startsWith(`${page.to}/`))?.label ?? "知识服务中心";

	return (
		<SidebarProvider>
			<AppSidebar />
			{/*
				L2 中栏白卡片。高度算法必须与外边距配平：
				  窄屏：h-dvh，无外边距 → 正好占满
				  桌面：my-2 上下各 8px，所以高度取 100dvh - 1rem，二者相加正好 100dvh
				改 my-* 时必须同步改这个 calc —— 不配平会在底部露出一条空白。
			*/}
			<div className="content-container custom-scrollbar bg-card mx-0 h-dvh w-full min-w-0 overflow-auto border md:my-2 md:h-[calc(100dvh-1rem)] md:rounded-md">
				{/* 移动端顶栏：菜单触发 + 当前页标题。 */}
				<div className="bg-card sticky top-0 z-20 flex h-12 min-w-0 items-center border-b px-3 md:hidden">
					<div className="flex min-w-0 flex-1 items-center gap-2">
						<SidebarTrigger className="shrink-0" />
						<span className="truncate text-sm font-semibold">{mobileTitle}</span>
					</div>
				</div>
				<main className="content-container-inner custom-scrollbar relative flex h-[calc(100%-3rem)] min-h-0 flex-col overflow-y-hidden p-3 md:h-full md:px-4 md:py-3">
					{children}
				</main>
			</div>
		</SidebarProvider>
	);
}

/** 无 chrome 的居中容器 —— 登录、OAuth 回调、公开页。不发任何导航相关请求。 */
function MinimalShell({ children }: { children: ReactNode }) {
	return (
		<div className="content-container custom-scrollbar bg-card h-dvh w-full overflow-auto border md:my-2 md:h-[calc(100dvh-1rem)] md:rounded-md">
			<main className="content-container-inner custom-scrollbar relative mx-auto flex h-full min-h-0 flex-col overflow-y-hidden p-3 md:px-4">
				{children}
			</main>
		</div>
	);
}

function AppContent({ children }: { children: ReactNode }) {
	const shell = resolveShell(useMatches());
	const pathname = useLocation({ select: (location) => location.pathname });
	if (shell === "minimal") return <MinimalShell>{children}</MinimalShell>;
	if (["/portal/overview", "/portal/news", "/portal/wiki", "/portal/catalog"].includes(pathname)) return <SiteShell>{children}</SiteShell>;
	return <ConsoleShell>{children}</ConsoleShell>;
}

/**
 * Provider 栈 —— 全部收在这里。
 *
 * bifrost 把 provider 散在各 nested layout 里，它的 __root.tsx 注释自己承认
 * 那是待还的债（"If/when we consolidate provider setup..."）。模板一开始就收敛。
 *
 * ThemeProvider 的 attribute="class" 必须与 index.html 内联预探测脚本一致 ——
 * 那个脚本读 localStorage["theme"]（next-themes 的默认 storageKey）并在
 * 首帧前打上类名。两边同源，所以 hydration 后不会跳变。
 */
export function AppProviders({ children }: { children: ReactNode }) {
	return (
		<ThemeProvider attribute="class" defaultTheme="system" enableSystem>
			<I18nProvider>
				<QueryClientProvider client={queryClient}>
					<Toaster closeButton />
					{/* CommandPaletteProvider 在最外层：⌘K 是全局热键，任何页面都要能唤起。 */}
					<CommandPaletteProvider>
						<AppContent>{children}</AppContent>
					</CommandPaletteProvider>
				</QueryClientProvider>
			</I18nProvider>
		</ThemeProvider>
	);
}

export { Sidebar };
