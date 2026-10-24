import { expect, test } from "@playwright/test";

test("first sign-in accepts a distinct complex password", async ({ page }) => {
	test.skip(process.env.E2E_ALLOW_PASSWORD_CHANGE !== "1", "requires an isolated fresh database");
	const initialPassword = process.env.E2E_ADMIN_PASSWORD ?? "";
	test.skip(!initialPassword, "E2E_ADMIN_PASSWORD is required");
	const newPassword = "Testing-different-passphrase-2026";

	await page.goto("/login");
	await page.getByTestId("local-username").fill("admin");
	await page.getByTestId("local-password").fill(initialPassword);
	await page.getByTestId("local-login-submit").click();
	await expect(page.getByTestId("local-new-password")).toBeVisible();
	await page.getByTestId("local-new-password").fill(newPassword);
	await page.getByTestId("local-login-submit").click();
	await expect(page.getByTestId("overview-page")).toBeVisible();
});
