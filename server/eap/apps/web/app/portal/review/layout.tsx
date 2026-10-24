import { createFileRoute } from "@tanstack/react-router";
import { ReviewPage } from "./page";

/** 审核中心路由。 */
export const Route = createFileRoute("/portal/review")({
	component: ReviewPage,
});
