import { createFileRoute, redirect } from "@tanstack/react-router";
import { bootstrapDesktopSession, getSession } from "@/lib/auth/session";
import { LoginPage } from "./page";

/**
 * 登录路由 —— minimal 壳（无侧栏）。
 *
 * 已持有会话的访问直接进 Portal；会话只存在内存里，刷新后落到这里重新登录。
 */
export const Route = createFileRoute("/login")({
	staticData: { shell: "minimal" },
	validateSearch: (search: Record<string, unknown>): { goto?: string } => ({
		goto: typeof search.goto === "string" && search.goto ? search.goto : undefined,
	}),
	beforeLoad: async () => {
		if (getSession() !== null) throw redirect({ to: "/portal/overview" });
		if (await bootstrapDesktopSession()) throw redirect({ to: "/portal/overview" });
	},
	component: LoginPage,
});
