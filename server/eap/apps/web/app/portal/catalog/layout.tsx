import { createFileRoute } from "@tanstack/react-router";
import { CatalogPage } from "./page";

/** 能力目录路由。 */
export const Route = createFileRoute("/portal/catalog")({
	component: CatalogPage,
});
