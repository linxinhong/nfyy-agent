import { QueryClient } from "@tanstack/react-query";
import { ApiError } from "./client";

/**
 * QueryClient —— 单实例，模块级创建。
 *
 * 为什么不在组件里 useState 创建：这个应用是纯客户端 SPA，没有 SSR 的
 * per-request 隔离需求。模块级单例让非 React 代码（如路由 loader）也能访问它。
 *
 * retry 策略的要点：**4xx 不重试**。客户端错误重试三次只是把同一个错误
 * 重复三遍，既拖慢失败反馈又浪费请求。只有 5xx 和网络错误值得重试。
 */
export const queryClient = new QueryClient({
	defaultOptions: {
		queries: {
			staleTime: 30_000,
			gcTime: 5 * 60_000,
			refetchOnWindowFocus: false,
			retry: (failureCount, error) => {
				if (error instanceof ApiError && error.status && error.status >= 400 && error.status < 500) {
					return false;
				}
				return failureCount < 2;
			},
		},
		mutations: {
			// mutation 一律不自动重试 —— 非幂等操作重试可能造成重复副作用
			retry: false,
		},
	},
});

/**
 * Query key 工厂 —— 层级化，方便按前缀批量失效。
 *
 * 约定：key 从粗到细排列，这样 `invalidateQueries({ queryKey: keys.users.all })`
 * 能一次失效该域下所有查询（列表、详情、各种筛选变体）。
 * 这是 TanStack Query 里对应 RTK Query "tag" 的手法，但更精确 —— 前缀匹配
 * 而非字符串标签，不会误伤同名的其他域。
 *
 * 新增业务域时在这里加一节，不要在组件里手写字符串数组。
 */
export const keys = {
	health: ["health"] as const,

	// 示例域 —— 复制这个形状来加新域
	items: {
		all: ["items"] as const,
		list: (filters?: Record<string, unknown>) => ["items", "list", filters ?? {}] as const,
		detail: (id: string) => ["items", "detail", id] as const,
	},
} as const;
