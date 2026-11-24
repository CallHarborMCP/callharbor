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

// Production-build screenshots of the OAuth consent screen and Connected apps.
// All data is synthetic and served from route mocks; nothing is written to the server.
const syntheticUser = { id: "user-1", username: "alex.morgan", role: "user" };
const syntheticConsent = {
  id: "cho_q_demo_consent",
  client_id: "https://assistant.example.net/mcp",
  client_name: "Harbor Assistant",
  client_uri: "https://assistant.example.net",
  client_host: "assistant.example.net",
  metadata_document: true,
  redirect_uri: "https://assistant.example.net/oauth/callback",
  scopes: ["mcp:read", "mcp:call"],
  resource: "http://127.0.0.1:4173/mcp",
  expires_at: "2026-11-25T12:10:00.000Z",
};
const syntheticGrants = {
  grants: [
    {
      id: "cho_g_demo_1",
      client_id: "https://assistant.example.net/mcp",
      client_name: "Harbor Assistant",
      scopes: ["mcp:read", "mcp:call"],
      created_at: "2026-11-18T09:30:00.000Z",
      last_used_at: "2026-11-24T18:05:00.000Z",
    },
    {
      id: "cho_g_demo_2",
      client_id: "cho_client_7f3a9c",
      client_name: "Notes Sync",
      scopes: ["mcp:read"],
      created_at: "2026-11-20T10:00:00.000Z",
      last_used_at: null,
    },
  ],
};

test.describe("OAuth screenshots", () => {
  test.skip(!process.env.CALLHARBOR_CAPTURE_SCREENSHOTS, "set CALLHARBOR_CAPTURE_SCREENSHOTS to capture");

  test.beforeEach(async ({ page }) => {
    await page.route("**/api/**", async (route) => {
      const url = new URL(route.request().url());
      if (url.pathname === "/api/setup") {
        await route.fulfill({ json: { setup_required: false } });
      } else if (url.pathname === "/api/session") {
        await route.fulfill({ json: { user: syntheticUser, csrf_token: "csrf-demo" } });
      } else if (url.pathname.startsWith("/api/oauth/requests/")) {
        await route.fulfill({ json: syntheticConsent });
      } else if (url.pathname === "/api/oauth/grants") {
        await route.fulfill({ json: syntheticGrants });
      } else if (url.pathname === "/api/preferences/appearance") {
        await route.fulfill({ json: { theme: "light", preset: "console", accent: "blue", density: "comfortable" } });
      } else {
        await route.fulfill({ status: 404, json: { error: { code: "not_found" } } });
      }
    });
  });

  for (const viewport of [
    { label: "desktop", width: 1440, height: 900 },
    { label: "phone", width: 390, height: 844 },
  ]) {
    test(`captures oauth-consent-${viewport.label}`, async ({ page }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await page.goto("/oauth/consent?request=cho_q_demo_consent");
      await expect(page.getByRole("heading", { level: 1, name: "Allow app access to CallHarbor" })).toBeVisible();
      await page.screenshot({
        path: resolve(outDir, `oauth-consent-${viewport.label}.png`),
        fullPage: true,
      });
    });

    test(`captures connected-apps-${viewport.label}`, async ({ page }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await page.goto("/");
      await expect(page.getByText("alex.morgan (user)")).toBeVisible();
      await page.getByRole("button", { name: "Connected apps" }).click();
      await expect(page.getByRole("heading", { name: "Connected apps" })).toBeVisible();
      // The phone bottom bar is position:fixed. Growing the viewport to the page
      // height keeps it at the foot of the image instead of over the last rows.
      const pageHeight = await page.evaluate(() => document.documentElement.scrollHeight);
      await page.setViewportSize({ width: viewport.width, height: pageHeight });
      await page.screenshot({
        path: resolve(outDir, `connected-apps-${viewport.label}.png`),
        fullPage: true,
      });
    });
  }
});
