import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page, type Route } from "@playwright/test";

const user = { id: "user-1", username: "alice", role: "user" };
const admin = { id: "admin-1", username: "root", role: "admin" };

const widths = [320, 360, 390, 768, 1024, 1440];

async function mockApi(page: Page, who: typeof user | typeof admin = user) {
  await page.route("**/api/**", async (route: Route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/setup") {
      await route.fulfill({ json: { setup_required: false } });
    } else if (url.pathname === "/api/session") {
      await route.fulfill({ json: { user: who, csrf_token: "csrf" } });
    } else if (url.pathname === "/api/users") {
      await route.fulfill({ json: { users: [{ ...admin, active: true, created_at: "x", updated_at: "x" }] } });
    } else {
      await route.fulfill({ status: 404, json: { error: { code: "not_found" } } });
    }
  });
}

async function openSettings(page: Page) {
  await page.goto("/");
  await expect(page.getByText("Signed in as alice (user)")).toBeVisible();
  await page.getByRole("button", { name: "Settings" }).click();
  await expect(page.getByRole("heading", { name: "Appearance" })).toBeVisible();
}

test.describe("appearance settings", () => {
  test.beforeEach(async ({ page }) => {
    await mockApi(page);
  });

  test("defaults to system theme, console preset, blue accent and comfortable density", async ({ page }) => {
    await openSettings(page);
    await expect(page.locator("html")).toHaveAttribute("data-preset", "console");
    await expect(page.locator("html")).toHaveAttribute("data-accent", "blue");
    await expect(page.locator("html")).toHaveAttribute("data-density", "comfortable");
  });

  test("dark theme is applied and persists across reload for the same user", async ({ page }) => {
    await openSettings(page);
    await page.getByRole("button", { name: "Dark", exact: true }).click();
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");

    await page.reload();
    await page.getByRole("button", { name: "Settings" }).click();
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expect(page.getByRole("button", { name: "Dark", exact: true })).toHaveAttribute("aria-pressed", "true");
  });

  test("system theme follows the OS color scheme", async ({ page }) => {
    await page.emulateMedia({ colorScheme: "dark" });
    await openSettings(page);
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");

    await page.emulateMedia({ colorScheme: "light" });
    await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  });

  test("harbor preset, teal accent and compact density apply and persist", async ({ page }) => {
    await openSettings(page);
    await page.getByRole("button", { name: "Harbor" }).click();
    await page.getByRole("button", { name: "Teal" }).click();
    await page.getByRole("button", { name: "Compact" }).click();

    await page.reload();
    await page.getByRole("button", { name: "Settings" }).click();
    await expect(page.locator("html")).toHaveAttribute("data-preset", "harbor");
    await expect(page.locator("html")).toHaveAttribute("data-accent", "teal");
    await expect(page.locator("html")).toHaveAttribute("data-density", "compact");
  });

  test("appearance choices are isolated per user", async ({ page, browser }) => {
    await openSettings(page);
    await page.getByRole("button", { name: "Dark", exact: true }).click();

    const other = await browser.newContext();
    const otherPage = await other.newPage();
    await mockApi(otherPage, admin);
    await otherPage.goto("/");
    await expect(otherPage.getByText("Signed in as root (administrator)")).toBeVisible();
    await otherPage.getByRole("button", { name: "Settings" }).click();
    await expect(otherPage.locator("html")).toHaveAttribute("data-theme", "light");
    await other.close();
  });

  test("settings screen has no critical or serious accessibility violations in light and dark", async ({ page }) => {
    for (const theme of ["Light", "Dark"]) {
      await openSettings(page);
      await page.getByRole("button", { name: theme, exact: true }).click();
      const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
      const serious = results.violations.filter((v) => v.impact === "critical" || v.impact === "serious");
      expect(serious, JSON.stringify(serious, null, 2)).toEqual([]);
    }
  });

  for (const width of widths) {
    test(`layout has no horizontal page scroll at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 800 });
      await openSettings(page);
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow).toBeLessThanOrEqual(0);
    });
  }

  test("phone width uses a bottom navigation bar and desktop uses a sidebar", async ({ page }) => {
    await page.setViewportSize({ width: 360, height: 800 });
    await openSettings(page);
    const phoneBox = await page.getByRole("navigation", { name: "Sections" }).boundingBox();
    expect(phoneBox?.y ?? 0).toBeGreaterThan(700);

    await page.setViewportSize({ width: 1440, height: 900 });
    const desktopBox = await page.getByRole("navigation", { name: "Sections" }).boundingBox();
    expect(desktopBox?.x ?? 999).toBeLessThan(40);
    expect(desktopBox?.y ?? 999).toBeLessThan(100);
  });
});
