import { createFileRoute } from "@tanstack/react-router";
import { NewsPage } from "./page";

/** 资讯路由。 */
export const Route = createFileRoute("/portal/news")({
	component: NewsPage,
});
