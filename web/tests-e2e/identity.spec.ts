import { expect, test, type Page } from "@playwright/test";

const ROOT_PASSWORD = "correct horse battery root";
const ALICE_PASSWORD = "alice passphrase long";

async function signIn(page: Page, username: string, password: string) {
  await page.getByLabel("Username").fill(username);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
}

test.describe.serial("identity bootstrap and RBAC", () => {
  test("first administrator is created through the setup wizard", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Create the first administrator" })).toBeVisible();

    await page.getByLabel("Username").fill("root");
    await page.getByLabel("Password", { exact: true }).fill(ROOT_PASSWORD);
    await page.getByLabel("Confirm password").fill(ROOT_PASSWORD);
    await page.getByRole("button", { name: "Create administrator" }).click();

    await expect(page.getByRole("heading", { name: "Sign in to CallHarbor" })).toBeVisible();
  });

  test("administrator creates a user, grants admin, and the user is denied admin screens", async ({ browser, page }) => {
    await page.goto("/");
    await signIn(page, "root", ROOT_PASSWORD);
    await expect(page.getByText("Signed in as root (administrator)")).toBeVisible();

    const createForm = page.getByRole("region", { name: "Create user" });
    await createForm.getByLabel("Username").fill("alice");
    await createForm.getByLabel("Initial password").fill(ALICE_PASSWORD);
    await createForm.getByRole("button", { name: "Create user" }).click();
    await expect(page.getByRole("status")).toHaveText("Created user alice.");

    const userContext = await browser.newContext();
    const user = await userContext.newPage();
    await user.goto("/");
    await signIn(user, "alice", ALICE_PASSWORD);
    await expect(user.getByText("Signed in as alice (user)")).toBeVisible();
    await expect(user.getByRole("heading", { name: "Users" })).toHaveCount(0);
    await userContext.close();
  });

  test("sign-out returns to the login form", async ({ page }) => {
    await page.goto("/");
    await signIn(page, "root", ROOT_PASSWORD);
    await expect(page.getByText("Signed in as root (administrator)")).toBeVisible();

    await page.getByRole("button", { name: "Sign out" }).click();

    await expect(page.getByRole("heading", { name: "Sign in to CallHarbor" })).toBeVisible();
  });
});
