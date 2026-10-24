import { createFileRoute } from "@tanstack/react-router";
import { MembersPage } from "./page";

/** 成员与团队路由（管理员）。 */
export const Route = createFileRoute("/portal/members")({
	component: MembersPage,
});
