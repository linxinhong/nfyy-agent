import { createFileRoute } from "@tanstack/react-router";
import { OverviewPage } from "./page";

/** Portal 概览路由 —— 身份、Workspace 上下文与快速入口。 */
export const Route = createFileRoute("/portal/overview")({
	component: OverviewPage,
});
