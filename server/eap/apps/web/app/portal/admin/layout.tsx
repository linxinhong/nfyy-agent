import { createFileRoute } from "@tanstack/react-router";
import { AdminPage } from "./page";

/** Admin Console 路由 —— 受治理的平台检查面（TASK-022）。 */
export const Route = createFileRoute("/portal/admin")({
	component: AdminPage,
});
