import { createFileRoute } from "@tanstack/react-router";
import { SkillsPage } from "./page";

/** 技能路由：技能架、启用与必装、发布。 */
export const Route = createFileRoute("/portal/skills")({
	component: SkillsPage,
});
