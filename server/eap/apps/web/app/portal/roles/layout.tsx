import { createFileRoute } from "@tanstack/react-router";
import { RolesPage } from "./page";

/** 角色与权限路由。 */
export const Route = createFileRoute("/portal/roles")({
	component: RolesPage,
});
