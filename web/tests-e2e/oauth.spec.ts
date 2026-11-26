import { createHash, randomBytes } from "node:crypto";
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type APIRequestContext, type BrowserContext, type Page } from "@playwright/test";

// The server is shared by every spec in this run. identity.spec.ts creates the
// first administrator with these credentials. This spec creates the same
// administrator when setup is still required and reuses it otherwise.
const ROOT_USERNAME = "root";
const ROOT_PASSWORD = "correct horse battery root";

const ORIGIN = "http://127.0.0.1:4173";
const RESOURCE = `${ORIGIN}/mcp`;
// Loopback callback that nothing listens on. The browser is intercepted before
// it connects, so the spec reads the final redirect. Port 9 is avoided because
// Chromium refuses it as an unsafe port.
const CALLBACK_ORIGIN = "http://127.0.0.1:48123";
const CALLBACK = `${CALLBACK_ORIGIN}/cb`;
const STATE = "e2e-state-value";

const CONSENT_HEADING = "Allow app access to CallHarbor";

function base64url(buffer: Buffer): string {
  return buffer.toString("base64url");
}

async function ensureAdministrator(api: APIRequestContext) {
  const status = await api.get("/api/setup");
  expect(status.ok()).toBeTruthy();
  const body = (await status.json()) as { setup_required: boolean };
  if (body.setup_required) {
    const created = await api.post("/api/setup", { data: { username: ROOT_USERNAME, password: ROOT_PASSWORD } });
    expect(created.status()).toBe(201);
  }
}

async function registerClient(api: APIRequestContext, name: string): Promise<string> {
  const response = await api.post(`${ORIGIN}/oauth/register`, {
    data: {
      client_name: name,
      redirect_uris: [CALLBACK],
      token_endpoint_auth_method: "none",
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
    },
  });
  expect(response.status()).toBe(201);
  return ((await response.json()) as { client_id: string }).client_id;
}

interface PKCE {
  readonly verifier: string;
  readonly challenge: string;
}

function newPKCE(): PKCE {
  const verifier = base64url(randomBytes(32));
  const challenge = base64url(createHash("sha256").update(verifier).digest());
  return { verifier, challenge };
}

function authorizeURL(clientID: string, pkce: PKCE): string {
  const params = new URLSearchParams({
    response_type: "code",
    client_id: clientID,
    redirect_uri: CALLBACK,
    code_challenge: pkce.challenge,
    code_challenge_method: "S256",
    state: STATE,
    scope: "mcp:read mcp:call",
    resource: RESOURCE,
  });
  return `${ORIGIN}/oauth/authorize?${params.toString()}`;
}

/**
 * Signs in only when the sign-in form is shown. A page can already hold a
 * session from an earlier step, in which case the signed-in view appears.
 */
async function signInIfPrompted(page: Page) {
  const signIn = page.getByRole("heading", { name: "Sign in to CallHarbor" });
  const signedIn = page.getByRole("heading", { name: "CallHarbor", level: 1 });
  const consent = page.getByRole("heading", { level: 1, name: CONSENT_HEADING });
  await expect(signIn.or(signedIn).or(consent).first()).toBeVisible();
  if (await signIn.isVisible()) {
    await page.getByLabel("Username").fill(ROOT_USERNAME);
    await page.getByLabel("Password").fill(ROOT_PASSWORD);
    await page.getByRole("button", { name: "Sign in" }).click();
  }
}

/**
 * Sends a JSON-RPC tools/list request to /mcp with a bearer token. Streamable
 * HTTP requires the client to accept both JSON and event streams.
 */
function mcpToolsList(context: BrowserContext, token: string) {
  return context.request.post(`${ORIGIN}/mcp`, {
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: "application/json, text/event-stream",
      "Content-Type": "application/json",
      "MCP-Protocol-Version": "2025-11-25",
    },
    data: { jsonrpc: "2.0", id: 1, method: "tools/list" },
    failOnStatusCode: false,
  });
}

/** Opens an authorization request, signs in if needed, and waits for the consent heading. */
async function openConsent(page: Page, url: string): Promise<string> {
  await page.goto(url);
  await signInIfPrompted(page);
  await expect(page.getByRole("heading", { level: 1, name: CONSENT_HEADING })).toBeFocused();
  const requestID = new URL(page.url()).searchParams.get("request");
  expect(requestID).toMatch(/^cho_q_/);
  return requestID ?? "";
}

test.describe.serial("OAuth consent and connected apps", () => {
  let context: BrowserContext;
  let page: Page;
  let clientID = "";
  let accessToken = "";
  const clientName = `E2E Notes Client ${Date.now()}`;
  const denyName = `E2E Deny Client ${Date.now()}`;
  const callbacks: URL[] = [];

  test.beforeAll(async ({ browser, playwright }) => {
    context = await browser.newContext();
    // Every redirect to the client callback is recorded and fulfilled locally.
    await context.route(`${CALLBACK_ORIGIN}/**`, async (route) => {
      callbacks.push(new URL(route.request().url()));
      await route.fulfill({ status: 200, contentType: "text/plain", body: "callback received" });
    });
    page = await context.newPage();
    const api = await playwright.request.newContext({ baseURL: ORIGIN });
    await ensureAdministrator(api);
    clientID = await registerClient(api, clientName);
    await api.dispose();
  });

  test.afterAll(async () => {
    await context.close();
  });

  test("approval sends a code, state and iss to the client, and the code exchanges for a working bearer", async () => {
    const pkce = newPKCE();
    await openConsent(page, authorizeURL(clientID, pkce));

    await expect(page.getByText(clientName, { exact: true })).toBeVisible();
    await expect(page.getByText("The app supplied this name. CallHarbor has not verified it.")).toBeVisible();
    await expect(page.getByText("Registered as")).toBeVisible();
    await expect(page.getByText("View your calls, transcripts, recordings and profiles")).toBeVisible();
    await expect(page.getByText("Place and stop phone calls on your behalf")).toBeVisible();
    await expect(page.getByText(`Signed in as ${ROOT_USERNAME}.`)).toBeVisible();
    await expect(page.getByRole("button", { name: "Allow" })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Deny" })).toBeEnabled();

    const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"]).analyze();
    expect(results.violations, JSON.stringify(results.violations, null, 2)).toEqual([]);

    await page.getByRole("button", { name: "Allow" }).click();
    await expect.poll(() => callbacks.length).toBeGreaterThan(0);
    const redirect = callbacks[0];
    expect(redirect.origin + redirect.pathname).toBe(CALLBACK);
    const code = redirect.searchParams.get("code") ?? "";
    expect(code).not.toBe("");
    expect(redirect.searchParams.get("state")).toBe(STATE);
    expect(redirect.searchParams.get("iss")).toBe(ORIGIN);

    const exchange = await context.request.post(`${ORIGIN}/oauth/token`, {
      form: {
        grant_type: "authorization_code",
        code,
        redirect_uri: CALLBACK,
        client_id: clientID,
        code_verifier: pkce.verifier,
        resource: RESOURCE,
      },
    });
    expect(exchange.status()).toBe(200);
    accessToken = ((await exchange.json()) as { access_token: string }).access_token;

    // A valid bearer passes authorization and reaches the MCP transport. The
    // Accept header is required by Streamable HTTP; tools/list is a real read.
    const mcp = await mcpToolsList(context, accessToken);
    expect(mcp.status()).toBe(200);
    const listed = (await mcp.json()) as { result?: { tools?: { name: string }[] } };
    expect(listed.result?.tools?.map((tool) => tool.name).sort()).toEqual(["get_call", "get_profiles", "list_calls", "make_call"]);
  });

  test("connected apps lists the client, and revoking it stops MCP access", async () => {
    await page.goto("/");
    await signInIfPrompted(page);
    await page.getByRole("button", { name: "Connected apps" }).click();

    const row = page.getByRole("listitem").filter({ hasText: clientName });
    await expect(row).toBeVisible();
    await expect(row.getByText(clientID)).toBeVisible();
    await expect(row.getByText(/Connected/)).toBeVisible();

    await row.getByRole("button", { name: `Revoke access for ${clientName}` }).click();
    const confirm = row.getByRole("group");
    await expect(confirm).toBeFocused();
    await confirm.getByRole("button", { name: "Revoke access" }).click();

    await expect(page.getByRole("status")).toContainText(`Access for ${clientName} was revoked.`);
    await expect(page.getByRole("listitem").filter({ hasText: clientName })).toHaveCount(0);

    const mcp = await mcpToolsList(context, accessToken);
    expect(mcp.status()).toBe(401);
    expect(mcp.headers()["www-authenticate"]).toContain('error="invalid_token"');
  });

  test("denying redirects the client with error=access_denied and no code", async ({ playwright }) => {
    const api = await playwright.request.newContext({ baseURL: ORIGIN });
    const denyClientID = await registerClient(api, denyName);
    await api.dispose();

    const before = callbacks.length;
    const pkce = newPKCE();
    await openConsent(page, authorizeURL(denyClientID, pkce));
    await page.getByRole("button", { name: "Deny" }).click();

    await expect.poll(() => callbacks.length).toBeGreaterThan(before);
    const redirect = callbacks[callbacks.length - 1];
    expect(redirect.searchParams.get("error")).toBe("access_denied");
    expect(redirect.searchParams.get("state")).toBe(STATE);
    expect(redirect.searchParams.get("code")).toBeNull();
  });

  test("a request that was already answered shows a clear message and no decision buttons", async ({ playwright }) => {
    const api = await playwright.request.newContext({ baseURL: ORIGIN });
    const reusedID = await registerClient(api, `${clientName} reuse`);
    await api.dispose();

    const requestID = await openConsent(page, authorizeURL(reusedID, newPKCE()));
    await page.getByRole("button", { name: "Deny" }).click();
    // Let the browser finish the redirect to the callback before reusing the request.
    await page.waitForURL(`${CALLBACK_ORIGIN}/**`);

    await page.goto(`/oauth/consent?request=${encodeURIComponent(requestID)}`);
    await expect(page.getByRole("alert")).toContainText("expired or was already answered");
    await expect(page.getByRole("button", { name: "Allow" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Deny" })).toHaveCount(0);
  });

  for (const width of [360, 1440]) {
    test(`consent screen has no horizontal page scroll at ${width}px`, async ({ playwright }) => {
      await page.setViewportSize({ width, height: 800 });
      const api = await playwright.request.newContext({ baseURL: ORIGIN });
      const id = await registerClient(api, `${clientName} width ${width}`);
      await api.dispose();

      await openConsent(page, authorizeURL(id, newPKCE()));
      await expect(page.getByRole("button", { name: "Allow" })).toBeVisible();
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(overflow).toBeLessThanOrEqual(0);
      await page.getByRole("button", { name: "Deny" }).click();
    });
  }
});

