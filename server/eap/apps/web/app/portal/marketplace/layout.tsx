import { createFileRoute } from "@tanstack/react-router";
import { MarketplacePage } from "./page";

/** Skill Marketplace 路由 —— 服务端目录，替代静态 Agent Center 目录。 */
export const Route = createFileRoute("/portal/marketplace")({
	component: MarketplacePage,
});
