import { expect, test } from "@playwright/test";

const adminPassword = process.env.E2E_ADMIN_PASSWORD ?? "";

test("first-login password change uses the entered values", async ({ page }) => {
	test.skip(!adminPassword, "E2E_ADMIN_PASSWORD is required");

	await page.goto("/login");
	await page.getByTestId("local-username").fill("admin");
	await page.getByTestId("local-password").fill(adminPassword);
	await page.getByTestId("local-login-submit").click();
	await expect(page.getByTestId("local-new-password")).toBeVisible();
	await expect(page.getByTestId("local-login-error")).toHaveCount(0);
	await expect(page.getByTestId("local-password")).toHaveAttribute("autocomplete", "current-password");
	await expect(page.getByTestId("local-new-password")).toHaveAttribute("autocomplete", "new-password");
	await expect(page.getByTestId("local-new-password")).toHaveAttribute("name", "new_password");
	await page.getByTestId("local-new-password").locator("..").getByRole("button", { name: "显示" }).click();
	await expect(page.getByTestId("local-new-password")).toHaveAttribute("type", "text");
	await page.getByTestId("local-new-password").locator("..").getByRole("button", { name: "隐藏" }).click();
	await expect(page.getByTestId("local-new-password")).toHaveAttribute("type", "password");

	let submitted: { current_password: string; new_password: string } | undefined;
	let changeRequests = 0;
	await page.route("**/enterprise/v1/auth/local/password", async (route) => {
		changeRequests += 1;
		submitted = route.request().postDataJSON() as typeof submitted;
		// Verify the browser payload without changing the shared demo account.
		await route.fulfill({
			status: 400,
			contentType: "application/json",
			body: JSON.stringify({ code: "invalid_argument", message: "test response" }),
		});
	});

	await page.getByTestId("local-new-password").fill(adminPassword);
	await page.getByTestId("local-login-submit").click();
	await expect(page.getByTestId("local-login-error")).toContainText("新密码不能与当前密码相同");
	expect(changeRequests).toBe(0);

	const distinctPassword = "Testing-different-passphrase-2026";
	await page.getByTestId("local-new-password").fill(distinctPassword);
	await expect(page.getByTestId("local-login-error")).toHaveCount(0);
	await page.getByTestId("local-login-submit").click();
	await expect.poll(() => changeRequests).toBe(1);
	expect(submitted).toMatchObject({ current_password: adminPassword, new_password: distinctPassword });
	await expect(page.getByTestId("local-login-error")).toContainText("密码修改未通过，请检查输入后重试");
});
