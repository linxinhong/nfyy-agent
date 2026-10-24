import { readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

/**
 * Portal 验收浏览器流（TASK-005 acceptance）：
 * 登录 → Workspace 选择 → 一次性 Key 展示 → 客户端配置下载 → 吊销。
 * 全程断言：会话与 Key 值绝不落入浏览器存储。
 */

const EMAIL = `e2e-${Date.now()}@example.com`;

test("configured desktop key opens the center after reload without a login form", async ({ page }) => {
	await page.addInitScript(() => {
		window.yuanpuPortal = {
			restore: async () => ({
				user: { id: "member-1", email: "member@example.test", display_name: "桌面成员" },
				workspaces: [{ id: "secops", name: "安全运营" }],
				default_workspace: "secops",
			}),
			request: async ({ path }) => {
				if (path.startsWith("/enterprise/v1/resources")) return { status: 200, body: { resources: [] }, contentType: "application/json" };
				if (path.startsWith("/enterprise/v1/catalog/search"))
					return { status: 200, body: { schema_version: 1, items: [] }, contentType: "application/json" };
				if (path.startsWith("/enterprise/v1/wiki/tree"))
					return { status: 200, body: { nodes: [], pages: [] }, contentType: "application/json" };
				return { status: 404, body: { code: "not_found", message: "unknown" }, contentType: "application/json" };
			},
			saveAPIKey: async () => undefined,
			clearAPIKey: async () => undefined,
		};
	});
	await page.goto("/");
	await expect(page.getByTestId("overview-page")).toBeVisible();
	await expect(page.getByTestId("login-local-submit")).toHaveCount(0);
	await page.reload();
	await expect(page.getByTestId("overview-page")).toBeVisible();
	await page.getByTestId("site-account-menu").click();
	await expect(page.getByTestId("profile-email")).toHaveText("member@example.test");
	await expect(page.getByTestId("quick-keys")).toHaveCount(0);
	const storage = await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }));
	expect(storage).not.toContain("agk_");
});

test("published curator page without a chapter appears in the wiki", async ({ page }) => {
	await page.addInitScript(() => {
		window.yuanpuPortal = {
			restore: async () => ({
				user: { id: "member-1", email: "member@example.test", display_name: "桌面成员" },
				workspaces: [{ id: "secops", name: "安全运营" }],
				default_workspace: "secops",
			}),
			request: async ({ path }) => {
				if (path.startsWith("/enterprise/v1/wiki/tree"))
					return {
						status: 200,
						body: { nodes: [], pages: [{ id: "curated-page", node_id: "", slug: "", path: "", title: "自动整理的知识" }] },
						contentType: "application/json",
					};
				if (path.startsWith("/enterprise/v1/wiki/pages/curated-page"))
					return {
						status: 200,
						body: {
							id: "curated-page",
							title: "自动整理的知识",
							summary: "",
							path: "",
							revision: 1,
							html: "<h1>自动整理的知识</h1><p>已审核发布</p>",
							tags: [],
							attachments: [],
							sources: [],
							updated_at: "2026-10-24T00:00:00Z",
						},
						contentType: "application/json",
					};
				if (path.startsWith("/enterprise/v1/resources")) return { status: 200, body: { resources: [] }, contentType: "application/json" };
				if (path.startsWith("/enterprise/v1/catalog/search"))
					return { status: 200, body: { schema_version: 1, items: [] }, contentType: "application/json" };
				return { status: 404, body: { code: "not_found", message: "unknown" }, contentType: "application/json" };
			},
			saveAPIKey: async () => undefined,
			clearAPIKey: async () => undefined,
		};
	});
	await page.goto("/");
	await page.getByRole("link", { name: "自动整理的知识" }).click();
	await expect(page.getByTestId("wiki-tree")).toContainText("未归档页面");
	await expect(page.getByTestId("wiki-article")).toContainText("已审核发布");
	await expect(page.getByTestId("wiki-article").getByRole("heading", { name: "自动整理的知识" })).toHaveCount(1);
});

test("wiki samples stay in the wiki instead of the news list", async ({ page }) => {
	await page.addInitScript(() => {
		window.yuanpuPortal = {
			restore: async () => ({
				user: { id: "member-1", email: "member@example.test", display_name: "桌面成员" },
				workspaces: [{ id: "secops", name: "安全运营" }],
				default_workspace: "secops",
			}),
			request: async ({ path }) => {
				if (path.startsWith("/enterprise/v1/resources")) {
					if (!path.includes("kind=news")) return { status: 400, body: { code: "invalid_argument" }, contentType: "application/json" };
					return {
						status: 200,
						body: {
							resources: [
								{ id: "news-one", kind: "news", title: "中心试运行", summary: "资讯", body: "资讯内容", visibility: "organization" },
								{ id: "wiki-one", kind: "wiki", title: "合同审查清单", summary: "知识", body: "知识内容", visibility: "organization" },
							],
						},
						contentType: "application/json",
					};
				}
				if (path.startsWith("/enterprise/v1/wiki/tree"))
					return {
						status: 200,
						body: { nodes: [], pages: [{ id: "wiki-one", node_id: "", slug: "", path: "", title: "合同审查清单" }] },
						contentType: "application/json",
					};
				return { status: 404, body: { code: "not_found" }, contentType: "application/json" };
			},
			saveAPIKey: async () => undefined,
			clearAPIKey: async () => undefined,
		};
	});
	await page.goto("/portal/news");
	await expect(page.getByTestId("news-list")).toContainText("中心试运行");
	await expect(page.getByTestId("news-list")).not.toContainText("合同审查清单");
	await page.goto("/portal/wiki");
	await expect(page.getByTestId("wiki-tree")).toContainText("合同审查清单");
});

test("member homepage features published content and searches across the center", async ({ page }) => {
	const news = {
		id: "news-featured",
		kind: "news",
		owner_id: "member",
		visibility: "organization",
		revision: 1,
		published_revision: 1,
		status: "approved",
		title: "合规周报",
		summary: "本周发布的合规知识",
		body: "已发布的完整周报内容",
		details: {},
		updated_at: "2026-10-23T08:00:00Z",
	};
	await page.route("**/enterprise/v1/resources*", async (route) => {
		const q = new URL(route.request().url()).searchParams.get("q");
		await route.fulfill({ json: { resources: q && !news.title.includes(q) ? [] : [news] } });
	});
	await page.route("**/enterprise/v1/wiki/tree", async (route) => {
		await route.fulfill({
			json: { nodes: [], pages: [{ id: "wiki-one", node_id: "chapter", slug: "guide", path: "guide", title: "办案指南" }] },
		});
	});
	await page.goto("/login");
	await page.getByTestId("login-email").fill(`e2e-home-${Date.now()}@example.com`);
	await page.getByTestId("login-workspaces").fill("secops");
	await page.getByTestId("login-submit").click();
	await expect(page.getByRole("navigation", { name: "站点导航" })).toBeVisible();
	await expect(page.getByTestId("home-featured-news")).toContainText("合规周报");
	await expect(page.getByRole("link", { name: "办案指南" })).toBeVisible();
	await page.getByTestId("home-featured-news").click();
	await expect(page.getByRole("dialog")).toContainText("已发布的完整周报内容");
	await page.keyboard.press("Escape");
	await page.getByRole("search").getByRole("textbox").fill("合规");
	await page.getByRole("search").getByRole("button").click();
	await expect(page.getByTestId("home-search-results")).toContainText("合规周报");
});

test("login → workspace → one-time key → config download → revoke", async ({ page, request }) => {
	// 1. 登录（dev IdP 替身）
	await page.goto("/login");
	await page.getByTestId("login-email").fill(EMAIL);
	await page.getByTestId("login-name").fill("E2E 用户");
	await page.getByTestId("login-workspaces").fill("secops");
	await page.getByTestId("login-submit").click();
	await expect(page.getByTestId("overview-page")).toBeVisible();
	await page.getByTestId("site-account-menu").click();
	await expect(page.getByTestId("profile-email")).toHaveText(EMAIL);
	await page.keyboard.press("Escape");

	// 2. Workspace 选择
	await page.getByTestId("workspace-select").click();
	await page.getByRole("option", { name: /安全运营|Security Operations/ }).click();
	await expect(page.getByTestId("workspace-select")).toContainText(/安全运营|Security Operations/);

	// 3. 创建 Key：完整值只出现一次
	await page.getByTestId("site-account-menu").click();
	await page.getByTestId("quick-keys").click();
	await page.getByTestId("key-create-button").click();
	await page.getByTestId("key-name-input").fill("E2E Harness");
	await page.getByTestId("key-submit").click();
	const oneTimeDialog = page.getByTestId("one-time-key-dialog");
	await expect(oneTimeDialog).toBeVisible();
	const keyValue = (await page.getByTestId("one-time-key-value").innerText()).trim();
	expect(keyValue).toMatch(/^agk_live_[0-9A-Za-z]{32}$/);

	// 4. 下载客户端配置：必须包含端点、绝不包含 Key 值
	const [download] = await Promise.all([page.waitForEvent("download"), page.getByTestId("download-config-from-key").click()]);
	const configPath = await download.path();
	const configBody = readFileSync(configPath!, "utf8");
	expect(configBody).toContain(`"workspace": "secops"`);
	expect(configBody).toContain("/enterprise/v1");
	expect(configBody).not.toContain(keyValue);
	expect(configBody).not.toContain("agk_");

	// 关闭一次性对话框后，页面任何位置都不再出现完整 Key
	await page.getByTestId("one-time-key-close").click();
	await expect(page.getByTestId("keys-table")).toBeVisible();
	const tableText = (await page.getByTestId("keys-table").innerText()) ?? "";
	expect(tableText).not.toContain(keyValue);
	expect(tableText).toContain(keyValue.slice(0, 13));

	// 5. 浏览器存储里没有会话 token、没有 Key 值
	const storage = await page.evaluate(() =>
		JSON.stringify({
			local: { ...window.localStorage },
			session: { ...window.sessionStorage },
			cookies: document.cookie,
		}),
	);
	expect(storage).not.toContain(keyValue);
	expect(storage).not.toMatch(/eyJhbGci/);

	// 6. 吊销：确认对话框 → 状态徽章变为已吊销
	const prefix = keyValue.slice(0, 13);
	await page.getByTestId(`key-revoke-${prefix}`).click();
	await page.getByTestId("revoke-confirm").click();
	await expect(page.getByTestId(`key-status-${prefix}`)).toHaveText("已吊销");

	// 7. 服务端立即生效：同一个 Key 再访问 /me 必须被拒（审计在网关侧落库）。
	// 相对路径基于 baseURL —— 套件对任意部署入口（compose/source）可移植。
	const meResponse = await request.get("/enterprise/v1/me", {
		headers: { Authorization: `Bearer ${keyValue}` },
	});
	const meBody = await meResponse.text();
	expect(meResponse.status(), `me responded: ${meResponse.status()} url=${meResponse.url()} ${meBody}`).toBe(401);
});

test("appearance menu switches UI language zh <-> en", async ({ page }) => {
	await page.goto("/login");
	await page.getByTestId("login-email").fill(`e2e-i18n-${Date.now()}@example.com`);
	await page.getByTestId("login-workspaces").fill("secops");
	await page.getByTestId("login-submit").click();
	await expect(page.getByTestId("overview-page")).toBeVisible();

	// 默认（zh-CN locale）显示中文标题
	await expect(page.getByRole("heading", { name: "知识服务中心" })).toBeVisible();

	// 外观菜单里切到 English
	await page.getByTestId("sidebar-theme").click();
	await page.getByTestId("sidebar-language-en").click();
	await expect(page.getByRole("heading", { name: "Knowledge Service Center" })).toBeVisible();

	// 切回简体中文
	await page.getByTestId("sidebar-theme").click();
	await page.getByTestId("sidebar-language-zh").click();
	await expect(page.getByRole("heading", { name: "知识服务中心" })).toBeVisible();
});

test("agent center lists approved clients and downloads safe config", async ({ page }) => {
	await page.goto("/login");
	await page.getByTestId("login-email").fill(`e2e-ac-${Date.now()}@example.com`);
	await page.getByTestId("login-workspaces").fill("secops");
	await page.getByTestId("login-submit").click();
	await expect(page.getByTestId("overview-page")).toBeVisible();

	// 整页刷新会清掉内存会话（安全设计），所以从概览用客户端路由跳转。
	await page.getByTestId("site-account-menu").click();
	await page.getByTestId("quick-agent-center").click();
	await expect(page.getByTestId("agent-center-page")).toBeVisible();
	await expect(page.getByTestId("agent-client-harness")).toBeVisible();

	const [download] = await Promise.all([page.waitForEvent("download"), page.getByTestId("download-config-harness").click()]);
	const configBody = readFileSync(await download.path(), "utf8");
	expect(configBody).toContain('"base_url"');
	expect(configBody).not.toContain("agk_");
});
