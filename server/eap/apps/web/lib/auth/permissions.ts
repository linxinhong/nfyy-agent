import { useQuery } from "@tanstack/react-query";
import { myPermissions, type Permission } from "@/lib/api/mindlink";
import { useSession } from "@/lib/auth/session";
import { desktopBridgeToken } from "@/lib/auth/desktopPortal";

/**
 * 当前成员在选中 Workspace 上的权限 —— 只用于决定显示哪些页面和按钮；
 * 每个操作仍由网关按权限重新判定，前端隐藏不是授权。
 *
 * 角色由管理员自定义，所以页面只问"有没有某项权限"，不认角色名。
 * 未加载完成时一律视为没有权限（宁可晚一点出现，不先露出再收回）。
 */
export interface Permissions {
	/** 全所范围的权限。 */
	has: (permission: Permission) => boolean;
	/** 全所或本团队范围内任一处有该权限（如是否显示审核入口）。 */
	hasAny: (permission: Permission) => boolean;
	/** 在自己所在团队内有该权限（全所权限也算）。 */
	hasInTeam: (permission: Permission) => boolean;
	/** 自己所在团队；不属于团队时为空。 */
	groupId: string;
}

export function usePermissions(): Permissions {
	const session = useSession();
	const workspace = session?.selectedWorkspace ?? "";
	const query = useQuery({
		queryKey: ["mindlink", "permissions", workspace],
		queryFn: myPermissions,
		enabled: Boolean(session && session.token !== desktopBridgeToken),
		staleTime: 60_000,
	});
	const data = query.data;
	const has = (permission: Permission) => data?.permissions.includes(permission) ?? false;
	const inTeam = (permission: Permission) => Boolean(data?.group_id) && (data?.team_permissions.includes(permission) ?? false);
	return {
		has,
		hasAny: (permission) => has(permission) || inTeam(permission),
		hasInTeam: (permission) => Boolean(data?.group_id) && (has(permission) || inTeam(permission)),
		groupId: data?.group_id ?? "",
	};
}
