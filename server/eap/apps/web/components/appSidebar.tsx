import { Bell, ChevronRight, PanelLeftClose, Search, ShieldCheck, User, X } from "lucide-react";
import { useState } from "react";
import { Link, useLocation } from "@tanstack/react-router";
import {
	Sidebar,
	SidebarContent,
	SidebarFooter,
	SidebarGroup,
	SidebarHeader,
	SidebarMenu,
	SidebarMenuButton,
	SidebarMenuItem,
	SidebarMenuSub,
	SidebarMenuSubButton,
	SidebarMenuSubItem,
	useSidebar,
} from "@/components/ui/sidebar";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { ThemeToggle } from "@/components/themeToggle";
import { Button } from "@/components/ui/button";
import { useCommandPalette } from "@/components/commandPalette";
import { NAV_GROUPS, groupKeyOfPath, visibleItems } from "@/lib/constants/nav";
import type { Permission } from "@/lib/api/mindlink";
import { usePermissions } from "@/lib/auth/permissions";
import { useI18n } from "@/components/i18nProvider";

/**
 * 品牌 logo —— 折叠态下同时是展开入口。
 *
 * 展开态它只是标识（此时右侧有专门的收起按钮）；折叠态 48px 宽放不下两个按钮，
 * 于是 logo 自己承担展开动作。用同一元素而非条件渲染两个：身份不变，
 * 只是 disabled 与 hover 反馈随状态切换。
 */
function BrandButton() {
	const { t } = useI18n();
	const { state, toggleSidebar } = useSidebar();
	const collapsed = state === "collapsed";
	return (
		<button
			type="button"
			onClick={collapsed ? toggleSidebar : undefined}
			disabled={!collapsed}
			title={collapsed ? t("nav.expand") : undefined}
			aria-label={collapsed ? t("nav.expand") : "知识服务中心"}
			data-testid="sidebar-brand"
			className="group-data-[collapsible=icon]:hover:bg-sidebar-accent flex size-8 shrink-0 items-center justify-center rounded-sm transition-colors group-data-[collapsible=icon]:mx-auto disabled:pointer-events-none"
		>
			<ShieldCheck className="text-primary size-4" aria-hidden="true" />
		</button>
	);
}

/** 收起按钮 —— 仅展开态可见；折叠后由 logo 承担展开动作。 */
function CollapseButton() {
	const { t } = useI18n();
	const { toggleSidebar } = useSidebar();
	return (
		<Button
			variant="ghost"
			size="icon"
			onClick={toggleSidebar}
			title={t("nav.collapse")}
			aria-label={t("nav.collapse")}
			className="hover:bg-sidebar-accent hover:text-sidebar-accent-foreground text-muted-foreground ml-auto size-8 shrink-0 group-data-[collapsible=icon]:hidden"
			dataTestId="sidebar-collapse"
		>
			<PanelLeftClose className="size-4" />
		</Button>
	);
}

/**
 * 侧栏搜索框 —— 就地过滤导航树。
 *
 * 交互分工（关键，别混）：
 *   框内打字 = 筛左侧树（"我知道去哪个页面，快速定位"）
 *   ⌘K       = 全局命令面板（"我要找一条告警/实体"，搜页面外的东西）
 * 两者不重叠。右侧 kbd 是 ⌘K 的次要入口提示，点它开面板。
 *
 * 折叠态（icon）下输入框放不下，缩成放大镜按钮 → 点击唤起 ⌘K（此时无法就地输入）。
 */
function SearchBox({ value, onChange }: { value: string; onChange: (v: string) => void }) {
	const { t } = useI18n();
	const { setOpen } = useCommandPalette();
	const { state } = useSidebar();

	if (state === "collapsed") {
		return (
			<button
				type="button"
				onClick={() => setOpen(true)}
				aria-label={t("nav.searchFocus")}
				data-testid="sidebar-search"
				className="border-input hover:bg-sidebar-accent text-muted-foreground mx-auto flex size-8 items-center justify-center rounded-sm border transition-colors"
			>
				<Search className="size-4" aria-hidden="true" />
			</button>
		);
	}

	return (
		<div className="border-input focus-within:ring-ring flex h-8 items-center gap-2 rounded-sm border px-2 focus-within:ring-1">
			<Search className="text-muted-foreground size-4 shrink-0" aria-hidden="true" />
			<input
				type="text"
				value={value}
				onChange={(e) => onChange(e.target.value)}
				onKeyDown={(e) => {
					if (e.key === "Escape") onChange("");
				}}
				placeholder={t("nav.search")}
				aria-label={t("nav.search")}
				data-testid="sidebar-search"
				className="placeholder:text-muted-foreground min-w-0 flex-1 bg-transparent text-sm outline-none"
			/>
			{value ? (
				<button
					type="button"
					onClick={() => onChange("")}
					aria-label={t("nav.clearFilter")}
					className="text-muted-foreground hover:text-foreground shrink-0"
				>
					<X className="size-3.5" />
				</button>
			) : (
				<button
					type="button"
					onClick={() => setOpen(true)}
					aria-label={t("nav.searchFocus")}
					data-testid="sidebar-cmdk"
					className="bg-muted text-muted-foreground hover:text-foreground shrink-0 rounded px-1.5 text-xs"
				>
					⌘K
				</button>
			)}
		</div>
	);
}

/** 分组在当前 filter 下命中的子项；filter 为空时返回全部。 */
function matchedItems(group: (typeof NAV_GROUPS)[number], filter: string, has: (permission: Permission) => boolean) {
	const items = visibleItems(group, has);
	const q = filter.trim().toLowerCase();
	if (!q) return items;
	// item label 或所属 group label 命中即保留 —— 搜"监控"能带出整组
	if (group.label.toLowerCase().includes(q)) return items;
	return items.filter((it) => it.label.toLowerCase().includes(q));
}

/**
 * 一个可折叠分组。
 *
 * 位置感的核心：含当前路由的分组**默认展开**（展开状态即"你在这个分组"的信号，
 * 替代了面包屑）。其余分组可自由折叠。future 分组（页面未实现）显示为禁用占位。
 *
 * filter 非空时：只显示有命中项的分组，且强制展开（否则筛出来的项藏在折叠里看不到）；
 * future 占位分组在筛选时隐藏（无可跳转项，是噪音）。
 */
function NavGroupTree({ group, pathname, filter }: { group: (typeof NAV_GROUPS)[number]; pathname: string; filter: string }) {
	// 权限随登录与切换 Workspace 重新获取，导航项随之重新筛选。
	const { has } = usePermissions();
	const containsActive = groupKeyOfPath(pathname) === group.key;
	const GroupIcon = group.icon;
	const filtering = filter.trim().length > 0;

	if (group.future) {
		// 筛选时占位分组无意义，隐藏
		if (filtering) return null;
		return (
			<SidebarMenuItem>
				<SidebarMenuButton disabled tooltip={`${group.label}（即将上线）`} className="opacity-50">
					<GroupIcon />
					<span>{group.label}</span>
					<ChevronRight className="ml-auto size-4 group-data-[collapsible=icon]:hidden" />
				</SidebarMenuButton>
			</SidebarMenuItem>
		);
	}

	const items = matchedItems(group, filter, has);
	// 筛选时无命中项的分组整个隐藏
	if (filtering && items.length === 0) return null;

	// 筛选时强制展开；否则含激活项才默认展开
	const open = filtering || containsActive;

	return (
		<Collapsible open={filtering ? true : undefined} defaultOpen={containsActive} className="group/collapsible">
			<SidebarMenuItem>
				<CollapsibleTrigger asChild>
					<SidebarMenuButton tooltip={group.label} isActive={containsActive}>
						<GroupIcon />
						<span>{group.label}</span>
						{/* chevron 随展开旋转 90° —— 展开态朝下，折叠态朝右 */}
						<ChevronRight
							className={`ml-auto size-4 transition-transform group-data-[collapsible=icon]:hidden group-data-[state=open]/collapsible:rotate-90 ${open ? "rotate-90" : ""}`}
						/>
					</SidebarMenuButton>
				</CollapsibleTrigger>
				<CollapsibleContent>
					<SidebarMenuSub>
						{items.map((item) => {
							const isActive = pathname === item.to || pathname.startsWith(`${item.to}/`);
							return (
								<SidebarMenuSubItem key={item.key}>
									<SidebarMenuSubButton asChild isActive={isActive}>
										<Link to={item.to}>
											<span>{item.label}</span>
										</Link>
									</SidebarMenuSubButton>
								</SidebarMenuSubItem>
							);
						})}
					</SidebarMenuSub>
				</CollapsibleContent>
			</SidebarMenuItem>
		</Collapsible>
	);
}

/**
 * 侧栏 —— `border-none bg-transparent` 不可改动。
 *
 * 侧栏不画自己的边框、不画自己的底，让画布色透出来；分隔交给主卡片的 border
 * 加 8px 沟槽。加 border-r 会让三层深度塌成两层平面。
 *
 * 导航是可折叠分组树（Cloudflare 模式）：位置感靠分组展开 + 二级项高亮，
 * 不做面包屑。future 分组占位，加页只填 NAV_GROUPS 不重构。
 */
export function AppSidebar() {
	const { t } = useI18n();
	const pathname = useLocation({ select: (location) => location.pathname });
	const [filter, setFilter] = useState("");
	const { has } = usePermissions();

	const filtering = filter.trim().length > 0;
	// 筛选时算一下有没有任何命中，用于"无结果"提示
	const anyMatch = NAV_GROUPS.some((g) => !g.future && matchedItems(g, filter, has).length > 0);

	return (
		<Sidebar collapsible="icon" className="overflow-y-clip border-none bg-transparent">
			<SidebarHeader className="gap-2">
				<div className="flex h-8 items-center gap-2 px-2 group-data-[collapsible=icon]:px-0">
					<BrandButton />
					<span className="truncate text-sm font-semibold group-data-[collapsible=icon]:hidden">知识服务中心</span>
					<CollapseButton />
				</div>
				<div className="px-1 group-data-[collapsible=icon]:px-0">
					<SearchBox value={filter} onChange={setFilter} />
				</div>
			</SidebarHeader>

			<SidebarContent className="overflow-hidden pb-4">
				<SidebarGroup>
					<SidebarMenu>
						{NAV_GROUPS.map((group) => (
							<NavGroupTree key={group.key} group={group} pathname={pathname} filter={filter} />
						))}
						{filtering && !anyMatch && (
							<li className="text-muted-foreground px-2 py-6 text-center text-xs" data-testid="sidebar-no-match">
								{t("nav.noMatch")}
							</li>
						)}
					</SidebarMenu>
				</SidebarGroup>
			</SidebarContent>

			{/*
				底部区 —— 用户 + 通知 + 外观，一行放下。
				放底部而非顶部：这些是低频账户级操作，顶部黄金位置留给品牌与导航。
				折叠态下改为纵向排列，图标全部保留可点。
				尺寸与导航项一致：h-8 / text-sm / size-4。
			*/}
			<SidebarFooter className="border-t p-2">
				<div className="flex items-center gap-1 group-data-[collapsible=icon]:flex-col group-data-[collapsible=icon]:gap-2">
					<button
						type="button"
						className="hover:bg-sidebar-accent hover:text-sidebar-accent-foreground flex h-8 min-w-0 flex-1 items-center gap-2 rounded-sm px-2 text-left text-sm transition-colors group-data-[collapsible=icon]:size-8 group-data-[collapsible=icon]:flex-none group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:px-0"
						title={t("nav.user")}
						data-testid="sidebar-user"
					>
						<User className="size-4 shrink-0" aria-hidden="true" />
						<span className="truncate group-data-[collapsible=icon]:hidden">{t("nav.user")}</span>
					</button>

					<button
						type="button"
						className="hover:bg-sidebar-accent hover:text-sidebar-accent-foreground relative flex size-8 shrink-0 items-center justify-center rounded-sm transition-colors"
						title={t("nav.notifications")}
						aria-label={t("nav.notifications")}
						data-testid="sidebar-notifications"
					>
						<Bell className="size-4" aria-hidden="true" />
					</button>

					<ThemeToggle />
				</div>
			</SidebarFooter>
		</Sidebar>
	);
}
