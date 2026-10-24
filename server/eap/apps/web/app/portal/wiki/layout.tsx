import { createFileRoute } from "@tanstack/react-router";
import { WikiPageView } from "./page";

/** 知识库路由；?path= 或 ?id= 直接打开页面。 */
export const Route = createFileRoute("/portal/wiki")({
	validateSearch: (search: Record<string, unknown>): { path?: string; id?: string } => ({
		path: typeof search.path === "string" && search.path ? search.path : undefined,
		id: typeof search.id === "string" && search.id ? search.id : undefined,
	}),
	component: WikiPageView,
});
