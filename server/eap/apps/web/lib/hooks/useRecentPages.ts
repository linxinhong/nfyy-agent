import { useEffect, useState } from "react";
import { useRouterState } from "@tanstack/react-router";

/**
 * 最近访问页面 —— 供命令面板显示快捷回跳。
 *
 * 存 sessionStorage 而非 localStorage：最近访问是"本次工作会话"的概念，
 * 关掉标签页应该清零，而不是下次打开还带着上周的记录。
 *
 * 只记录 workspace 下的页面路径（去掉 query），去重且 capped 到 5 条。
 * 用 pathname 而非完整 URL：同一页不同筛选参数不该算两条最近记录。
 */
const KEY = "app:recent-pages";
const MAX = 5;

function read(): string[] {
	try {
		const raw = sessionStorage.getItem(KEY);
		if (!raw) return [];
		const parsed: unknown = JSON.parse(raw);
		return Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === "string") : [];
	} catch {
		return [];
	}
}

export function useRecentPages() {
	const pathname = useRouterState({ select: (s) => s.location.pathname });
	const [recent, setRecent] = useState<string[]>(read);

	useEffect(() => {
		// 只记 workspace 页面；根路径与非工作区路径不进历史
		if (!pathname.startsWith("/workspace/")) return;
		setRecent((prev) => {
			// 去重后置顶当前页
			const next = [pathname, ...prev.filter((p) => p !== pathname)].slice(0, MAX);
			try {
				sessionStorage.setItem(KEY, JSON.stringify(next));
			} catch {
				// sessionStorage 不可用（隐私模式）—— 内存态仍可用，不报错
			}
			return next;
		});
	}, [pathname]);

	return { recent };
}
