import { createFileRoute } from "@tanstack/react-router";
import { AgentCenterPage } from "./page";

/** Agent Center 路由 —— 客户端目录与配置分发。 */
export const Route = createFileRoute("/portal/agent-center")({
	component: AgentCenterPage,
});
