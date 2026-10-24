import { createFileRoute } from "@tanstack/react-router";
import { DevicePage } from "./page";

/** 设备登录确认路由（桌面设备登录打开的 verification_uri）。 */
export const Route = createFileRoute("/portal/device")({
	validateSearch: (search: Record<string, unknown>): { user_code?: string } => ({
		user_code: typeof search.user_code === "string" && search.user_code ? search.user_code : undefined,
	}),
	component: DevicePage,
});
