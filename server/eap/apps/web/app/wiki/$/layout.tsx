import { createFileRoute, redirect } from "@tanstack/react-router";

/**
 * 站内链接入口：服务端把 [[书架/书/章/页]] 渲染为 /wiki/<路径>，这里转到知识库页面。
 * 旧路径由服务端按别名解析到页面的当前路径。
 */
export const Route = createFileRoute("/wiki/$")({
	beforeLoad: ({ params }) => {
		throw redirect({ to: "/portal/wiki", search: { path: params._splat || undefined } });
	},
});
