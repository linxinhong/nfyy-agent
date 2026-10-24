import { BookOpen, LayoutDashboard, type LucideIcon } from "lucide-react";
import type { Permission } from "@/lib/api/mindlink";

/**
 * 导航信息架构 —— 侧栏树与 ⌘K 命令面板的**唯一事实源**。
 *
 * 独立成文件（而非埋在 appSidebar 里）的理由：命令面板要从同一份数据派生
 * 可跳转页面清单。两处各维护一份必然漂移 —— 新增一页却忘了在命令面板里加。
 *
 * 结构支持二级：一级是分组，二级是页面；分组可标 `future` 表示"结构占位、
 * 页面未实现"。占位分组现在就出现在树里（折叠态），以后加页只填 items，
 * 不重构导航。
 *
 * 替换成你的导航：改 NAV_GROUPS 即可，侧栏与命令面板自动跟随。
 */
export interface NavItem {
	key: string;
	label: string;
	to: string;
	/** 显示计数角标（如未处理数）；值由消费方在侧栏里注入。 */
	showBadge?: boolean;
	/** 仅有该全所权限的成员可见（导航层面的减噪，授权仍由服务端判定）。 */
	permission?: Permission;
}

export interface NavGroup {
	key: string;
	label: string;
	icon: LucideIcon;
	items: NavItem[];
	/** 结构占位分组 —— 页面尚未实现，树里显示为禁用的折叠项。 */
	future?: boolean;
}

export const NAV_GROUPS: NavGroup[] = [
	{
		key: "mindlink",
		label: "知识服务中心",
		icon: BookOpen,
		items: [
			{ key: "overview", label: "首页", to: "/portal/overview" },
			{ key: "wiki", label: "知识库", to: "/portal/wiki" },
			{ key: "news", label: "资讯与资源", to: "/portal/news" },
			{ key: "skills", label: "技能", to: "/portal/skills" },
			{ key: "catalog", label: "能力目录", to: "/portal/catalog" },
			{ key: "review", label: "审核中心", to: "/portal/review" },
			{ key: "members", label: "成员与团队", to: "/portal/members", permission: "members.manage" },
			{ key: "roles", label: "角色与权限", to: "/portal/roles", permission: "members.manage" },
		],
	},
	{
		key: "portal",
		label: "平台管理",
		icon: LayoutDashboard,
		items: [
			{ key: "keys", label: "我的 API Key", to: "/portal/keys" },
			{ key: "agent-center", label: "Agent Center", to: "/portal/agent-center" },
			{ key: "marketplace", label: "Skill Marketplace", to: "/portal/marketplace" },
			{ key: "admin", label: "Admin Console", to: "/portal/admin" },
		],
	},
];

/** 展平所有可跳转页面 —— 供命令面板派生跳转项，不重复维护清单。 */
export interface FlatNavPage {
	key: string;
	label: string;
	groupLabel: string;
	to: string;
}

/** 当前成员可见的导航项；has 是全所权限判定。 */
export function visibleItems(group: NavGroup, has: (permission: Permission) => boolean): NavItem[] {
	return group.items.filter((item) => !item.permission || has(item.permission));
}

export function flattenPages(has: (permission: Permission) => boolean = () => false): FlatNavPage[] {
	const out: FlatNavPage[] = [];
	for (const group of NAV_GROUPS) {
		for (const item of visibleItems(group, has)) {
			out.push({ key: item.key, label: item.label, groupLabel: group.label, to: item.to });
		}
	}
	return out;
}

/** 反查当前路径所属分组 key —— 侧栏用它决定哪个分组默认展开。 */
export function groupKeyOfPath(pathname: string): string | null {
	for (const group of NAV_GROUPS) {
		if (group.items.some((it) => pathname === it.to || pathname.startsWith(`${it.to}/`))) {
			return group.key;
		}
	}
	return null;
}
