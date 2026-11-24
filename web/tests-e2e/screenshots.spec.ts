import { mkdirSync } from "node:fs";
import { resolve } from "node:path";
import { expect, test } from "@playwright/test";

const outDir = resolve(process.cwd(), "../docs/design/screenshots");

const surfaces = [
  { name: "prototype-a-phone", path: "/prototypes/a/", width: 390, height: 844 },
  { name: "prototype-a-desktop", path: "/prototypes/a/", width: 1440, height: 900 },
  { name: "prototype-b-phone", path: "/prototypes/b/", width: 390, height: 844 },
  { name: "prototype-b-desktop", path: "/prototypes/b/", width: 1440, height: 900 },
];

test.describe("design direction screenshots", () => {
  test.beforeAll(() => {
    mkdirSync(outDir, { recursive: true });
  });

  for (const surface of surfaces) {
    test(`captures ${surface.name}`, async ({ page }) => {
      await page.setViewportSize({ width: surface.width, height: surface.height });
      await page.goto(surface.path);
      await expect(page.getByRole("heading", { level: 1 }).first()).toBeVisible();
      await page.screenshot({ path: resolve(outDir, `${surface.name}.png`), fullPage: true });
    });
  }
});
