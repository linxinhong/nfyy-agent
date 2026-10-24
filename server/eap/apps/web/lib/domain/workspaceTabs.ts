/**
 * 页内详情标签的打开/关闭规则 —— 与 WorkspaceTabBar 配套的纯函数。
 *
 * 约定：列表视图是 id 为 LIST_TAB_ID 的固定标签；详情标签按业务 id 幂等
 * （重复打开同一单据只激活不新增）；关闭当前激活标签时激活右邻，无右邻
 * 则左邻，全部关完回到列表。
 */

export interface DetailTab {
	id: string;
	label: string;
}

export const LIST_TAB_ID = ":list";

// 泛型：允许调用方携带扩展字段（如来电方向）随标签状态一起流转
export function openDetailTab<T extends DetailTab>(tabs: T[], tab: T): { tabs: T[]; activeId: string } {
	const exists = tabs.some((t) => t.id === tab.id);
	return { tabs: exists ? tabs : [...tabs, tab], activeId: tab.id };
}

export function closeDetailTab<T extends DetailTab>(
	tabs: T[],
	id: string,
	activeId: string | null,
): { tabs: T[]; activeId: string | null } {
	const index = tabs.findIndex((t) => t.id === id);
	if (index === -1) return { tabs, activeId };
	const next = tabs.filter((t) => t.id !== id);
	if (activeId !== id) return { tabs: next, activeId };
	const fallback = next[index] ?? next[index - 1];
	return { tabs: next, activeId: fallback?.id ?? null };
}
