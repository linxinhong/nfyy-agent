import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

/**
 * 知识服务中心页面浏览器流：初始管理员首次登录改密 → 建团队与成员（一次性密码只显示一次）
 * → 自定义团队角色 → 建书架/书/章 → 发布、审核并启用受保护技能 → 审核人发布一页 → 旧式 /wiki/<路径> 链接打开知识库页面（服务端清洗的
 * HTML）→ 审核中心排队全局重整 → 未登录打开设备确认页，登录后回到该页且验证码保留。
 *
 * 需要以 EAP_LOCAL_AUTH=1、EAP_LOCAL_ADMIN_PASSWORD=$E2E_ADMIN_PASSWORD 启动、
 * 托管本 Portal 构建产物的网关（E2E_BASE_URL）。数据库应为全新库：初始管理员改密只能发生一次。
 */

const ADMIN_PASSWORD = process.env.E2E_ADMIN_PASSWORD ?? "";
const NEW_ADMIN_PASSWORD = `${ADMIN_PASSWORD}-new`;
const STAMP = Date.now().toString(36);

/** 应用内导航：会话只在内存，page.goto 会整页刷新并丢失登录。 */
async function nav(page: Page, label: string) {
	const group = page.getByRole("button", { name: "知识服务中心" });
	if ((await group.getAttribute("aria-expanded")) !== "true") await group.click();
	await page.getByRole("link", { name: label, exact: true }).click();
}

async function loginForm(page: Page, password: string) {
	await page.getByTestId("local-username").fill("admin");
	await page.getByTestId("local-password").fill(password);
	await page.getByTestId("local-login-submit").click();
}

async function apiLogin(request: APIRequestContext, username: string, password: string): Promise<string> {
	const res = await request.post("/enterprise/v1/auth/local/login", { data: { username, password } });
	expect(res.ok()).toBeTruthy();
	const body = (await res.json()) as { session_token?: string };
	expect(body.session_token).toBeTruthy();
	return body.session_token!;
}

test("知识服务中心：改密登录、成员、知识库、审核与设备确认", async ({ page, request }) => {
	test.skip(!ADMIN_PASSWORD, "E2E_ADMIN_PASSWORD is required");

	// 1. 初始管理员首次登录必须改密。
	await page.goto("/login");
	await page.getByTestId("local-username").fill("admin");
	await page.getByTestId("local-password").fill(ADMIN_PASSWORD);
	await page.getByTestId("local-login-submit").click();
	await expect(page.getByTestId("local-new-password")).toBeVisible();
	await page.getByTestId("local-new-password").fill(NEW_ADMIN_PASSWORD);
	await page.getByTestId("local-login-submit").click();
	await expect(page.getByTestId("overview-page")).toBeVisible();

	// 2. 成员与团队：一次性密码只在创建结果里出现。
	await nav(page, "成员与团队");
	await expect(page.getByTestId("members-page")).toBeVisible();
	await page.getByPlaceholder("团队名称").fill(`诉讼组-${STAMP}`);
	await page.getByRole("button", { name: "新建团队" }).click();
	await expect(page.getByText(`诉讼组-${STAMP}`)).toBeVisible();
	await page.getByTestId("member-create-button").click();
	await page.getByLabel("用户名").fill(`reviewer-${STAMP}`);
	await page.getByLabel("姓名").fill("审核人");
	await page.getByTestId("member-create-roles").click();
	await page.getByRole("checkbox", { name: "审核人" }).click();
	await page.keyboard.press("Escape");
	await page.getByRole("dialog").getByRole("button", { name: "新建成员" }).click();
	const oneTime = (await page.getByTestId("one-time-password").innerText()).trim();
	expect(oneTime.length).toBeGreaterThan(8);
	await page.getByRole("button", { name: "关闭" }).click();
	await expect(page.getByTestId("members-table")).toContainText(`reviewer-${STAMP}`);

	// 2b. 角色与权限：自定义团队角色，团队角色只能勾选可在团队内授予的权限。
	await nav(page, "角色与权限");
	await expect(page.getByTestId("roles-page")).toBeVisible();
	await page.getByTestId("role-create-button").click();
	const roleDialog = page.getByRole("dialog");
	await roleDialog.getByLabel("标识").fill(`partner-${STAMP}`);
	await roleDialog.getByLabel("名称").fill(`团队审核人-${STAMP}`);
	await roleDialog.getByRole("combobox").click();
	await page.getByRole("option", { name: "团队内" }).click();
	await expect(roleDialog.getByText("管理成员与团队")).toHaveCount(0);
	await roleDialog.getByRole("checkbox", { name: /审核技能/ }).click();
	await roleDialog.getByRole("button", { name: "保存" }).click();
	await expect(page.getByTestId("roles-table")).toContainText(`团队审核人-${STAMP}`);

	// 3. 知识库目录：书架 → 书 → 章。
	await nav(page, "知识库");
	await expect(page.getByTestId("wiki-page")).toBeVisible();
	const tree = page.getByTestId("wiki-tree");
	for (const [slug, parent] of [
		[`litigation-${STAMP}`, null],
		[`equity-${STAMP}`, `litigation-${STAMP}`],
		[`rulings-${STAMP}`, `equity-${STAMP}`],
	] as const) {
		if (parent) await tree.getByRole("button", { name: parent }).click();
		await page.getByPlaceholder("标识（用于路径）").fill(slug);
		await page.getByPlaceholder("名称").fill(slug);
		await page.getByRole("button", { name: "新建", exact: true }).click();
		await expect(tree.getByRole("button", { name: slug })).toBeVisible();
	}

	// 4. 经 API 建页、审核人改密后发布（发布者不能是作者）。
	const adminToken = await apiLogin(request, "admin", NEW_ADMIN_PASSWORD);
	const auth = (token: string) => ({ Authorization: `Bearer ${token}`, "X-Agent-Workspace": "secops" });
	const treeRes = await request.get("/enterprise/v1/wiki/tree", { headers: auth(adminToken) });
	const chapter = ((await treeRes.json()) as { nodes: { id: string; slug: string }[] }).nodes.find((n) => n.slug === `rulings-${STAMP}`)!;
	const created = await request.post("/enterprise/v1/resources", {
		headers: auth(adminToken),
		data: {
			kind: "wiki",
			visibility: "organization",
			node_id: chapter.id,
			slug: "buyback",
			title: "回购条款效力",
			summary: "对赌失败时回购条款有效",
			body: "## 要点\n法院支持回购。<script>window.__xss = 1</script>",
		},
	});
	expect(created.status()).toBe(201);
	const pageId = ((await created.json()) as { id: string }).id;
	expect((await request.post(`/enterprise/v1/resources/${pageId}/submit`, { headers: auth(adminToken) })).ok()).toBeTruthy();
	expect(
		(
			await request.post("/enterprise/v1/auth/local/password", {
				data: { username: `reviewer-${STAMP}`, current_password: oneTime, new_password: `reviewer-pass-${STAMP}` },
			})
		).ok(),
	).toBeTruthy();
	const reviewerToken = await apiLogin(request, `reviewer-${STAMP}`, `reviewer-pass-${STAMP}`);
	expect((await request.post(`/enterprise/v1/resources/${pageId}/approve`, { headers: auth(reviewerToken) })).ok()).toBeTruthy();

	// 4b. 技能：管理员在网站发布受保护的全所技能，审核人（非作者）通过并发布，
	//     成员在技能架上启用。
	await nav(page, "技能");
	await expect(page.getByTestId("skills-page")).toBeVisible();
	await page.getByTestId("skill-publish-button").click();
	const skillDialog = page.getByRole("dialog");
	await skillDialog.getByLabel(/^标识/).fill(`equity-${STAMP}`);
	await skillDialog.getByRole("combobox").click();
	await page.getByRole("option", { name: "全所" }).click();
	await skillDialog.getByLabel(/^SKILL\.md/).fill("---\ndescription: 对赌回购审查要点\n---\n先看回购触发条件。");
	await skillDialog.getByRole("button", { name: "发布技能" }).click();
	await expect(page.getByTestId(`skill-row-equity-${STAMP}`)).toContainText("待审核");
	const reviews = await request.get("/enterprise/v1/skill-reviews", { headers: auth(reviewerToken) });
	expect(JSON.stringify(await reviews.json())).toContain(`equity-${STAMP}`);
	for (const step of ["review", "release"]) {
		const res = await request.post(`/enterprise/v1/skills/equity-${STAMP}/versions/1.0.0/${step}`, {
			headers: auth(reviewerToken),
			data: {},
		});
		expect(res.ok()).toBeTruthy();
	}
	await page.getByTestId("skills-refresh").click();
	await page.getByTestId(`skill-toggle-equity-${STAMP}`).click();
	await expect(page.getByTestId(`skill-toggle-equity-${STAMP}`)).toBeChecked();

	// 5. 站内链接形式的地址（整页打开，需重新登录并回到原地址）进入知识库页面；正文是服务端清洗后的 HTML。
	const path = `litigation-${STAMP}/equity-${STAMP}/rulings-${STAMP}/buyback`;
	await page.goto(`/wiki/${path}`);
	await loginForm(page, NEW_ADMIN_PASSWORD);
	const article = page.getByTestId("wiki-article");
	await expect(article).toContainText("回购条款效力");
	await expect(article.locator(".wiki-body h2")).toHaveText("要点");
	expect(await page.evaluate(() => (window as unknown as { __xss?: number }).__xss)).toBeUndefined();

	// 6. 审核中心：管理员可立即排队全局重整。
	await nav(page, "审核中心");
	await expect(page.getByTestId("review-page")).toBeVisible();
	await page.getByTestId("sweep-button").click();
	await expect(page.getByText(/全局重整/).last()).toBeVisible();

	// 7. 设备确认页：未登录时先登录，再回到原页面且验证码保留（会话只在内存，新页面即未登录）。
	const fresh = await page.context().newPage();
	await fresh.goto("/portal/device?user_code=ABCD-EFGH");
	await expect(fresh.getByTestId("local-username")).toBeVisible();
	await loginForm(fresh, NEW_ADMIN_PASSWORD);
	await expect(fresh.getByTestId("device-page")).toBeVisible();
	await expect(fresh.getByTestId("device-code")).toHaveValue("ABCD-EFGH");
});
