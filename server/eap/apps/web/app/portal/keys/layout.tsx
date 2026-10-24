import { createFileRoute } from "@tanstack/react-router";
import { KeysPage } from "./page";

/** 我的 API Key 路由。 */
export const Route = createFileRoute("/portal/keys")({
	component: KeysPage,
});
