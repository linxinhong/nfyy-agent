import { createRootRoute, Outlet, redirect } from "@tanstack/react-router";
import { AppProviders } from "./appShell";

/**
 * 根路由 —— provider 栈全部收在这里。
 *
 * bifrost 把 provider 散在各 nested layout 里，它自己的 __root.tsx 注释承认
 * 那是待还的债。模板一开始就收敛：新增路由不需要再拼 provider。
 *
 * 注意 routeToken:"layout" 约定下 app/ 根目录**不能**有 layout.tsx ——
 * 它会解析成空路径并与 __root 冲突。所以根重定向住在这里的 beforeLoad。
 */
export const Route = createRootRoute({
	beforeLoad: ({ location }) => {
		if (location.pathname === "/") {
			// 根路由在声明期无法引用完整路由树的类型 —— 循环依赖：路由树由根构建，
			// 而根又要引用树里的路径。TanStack Router 对此没有解法，官方示例同样
			// 在根重定向处放宽类型。目标路径的正确性由别处的类型化引用保证
			// （例如 __notFound 里的 <Link to="...">，那里会真正校验）。
			throw redirect({ to: "/portal/overview" as never });
		}
	},
	component: () => (
		<AppProviders>
			<Outlet />
		</AppProviders>
	),
});
