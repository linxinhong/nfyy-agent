import { createFileRoute, Outlet, redirect } from "@tanstack/react-router";
import { bootstrapDesktopSession, bootstrapOIDCSession, getSession } from "@/lib/auth/session";

/**
 * Portal 受保护布局 —— 所有 /portal/* 路由的会话守卫。
 *
 * 守卫放在路由 beforeLoad 而非组件 effect：未登录时根本不渲染内容，
 * 也不发起任何业务请求（与 minimal 壳「不发导航相关请求」的原则一致）。
 *
 * 内存会话为空时先尝试 OIDC cookie 引导（浏览器带着网关 HttpOnly 会话
 * 回跳的场景）；引导失败才回登录页。
 */
export const Route = createFileRoute("/portal")({
	beforeLoad: async ({ location }) => {
		if (getSession() !== null) return;
		if (await bootstrapDesktopSession()) return;
		const bootstrapped = await bootstrapOIDCSession();
		// 记下原地址（例如桌面设备登录打开的 /portal/device?user_code=…），登录后回到这里。
		if (bootstrapped === null) throw redirect({ to: "/login", search: { goto: location.href } });
	},
	component: () => <Outlet />,
});
