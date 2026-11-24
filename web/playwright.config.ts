import { defineConfig, devices } from "@playwright/test";

const port = 4173;

export default defineConfig({
  testDir: "./tests-e2e",
  testIgnore: process.env.CALLHARBOR_CAPTURE_SCREENSHOTS ? [] : ["**/screenshots.spec.ts"],
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: [["list"], ["html", { open: "never", outputFolder: "playwright-report" }]],
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: "retain-on-failure",
    reducedMotion: "reduce",
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
  ],
  webServer: {
    command: "bash ../scripts/serve-e2e.sh",
    url: `http://127.0.0.1:${port}/healthz`,
    reuseExistingServer: false,
    timeout: 240_000,
  },
});
