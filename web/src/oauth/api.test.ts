import { afterEach, describe, expect, it, vi } from "vitest";
import {
  decideConsent,
  getConsentRequest,
  listConnectedApps,
  OAuthError,
  revokeConnectedApp,
} from "./api";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("OAuth API", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("loads a consent request from the same origin with credentials", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { id: "cho_q_1" }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(getConsentRequest("cho_q_1")).resolves.toEqual({ id: "cho_q_1" });
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/oauth/requests/cho_q_1",
      expect.objectContaining({ method: "GET", credentials: "same-origin" }),
    );
  });

  it("encodes request IDs in the path", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, {}));
    vi.stubGlobal("fetch", fetchMock);

    await getConsentRequest("a/b?c");
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/oauth/requests/a%2Fb%3Fc");
  });

  it("maps expired and unknown requests to not_found", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(404, { error: { code: "not_found" } })));

    await expect(getConsentRequest("cho_q_x")).rejects.toMatchObject({ code: "not_found" });
  });

  it("maps a missing session to unauthenticated even without a coded body", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("nope", { status: 401 })));

    const error = await getConsentRequest("cho_q_x").catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(OAuthError);
    expect((error as OAuthError).code).toBe("unauthenticated");
  });

  it("maps an unknown error code and a non-JSON failure to internal", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(500, { error: { code: "surprise" } }))
        .mockResolvedValueOnce(new Response("oops", { status: 502 })),
    );

    await expect(getConsentRequest("a")).rejects.toMatchObject({ code: "internal" });
    await expect(getConsentRequest("a")).rejects.toMatchObject({ code: "internal" });
  });

  it("maps a rejected fetch to network", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("offline")));

    await expect(listConnectedApps()).rejects.toMatchObject({ code: "network" });
  });

  it("sends the decision with CSRF and returns the redirect URL", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { redirect_url: "https://app.example/cb?code=1" }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(decideConsent("csrf-1", "cho_q_1", true)).resolves.toBe("https://app.example/cb?code=1");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/oauth/requests/cho_q_1/decision",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ approve: true }),
        headers: expect.objectContaining({ "X-CSRF-Token": "csrf-1", "Content-Type": "application/json" }),
      }),
    );
  });

  it("rejects a decision response whose redirect is not a web URL", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, { redirect_url: "javascript:alert(1)" }))
        .mockResolvedValueOnce(jsonResponse(200, { redirect_url: "not a url" }))
        .mockResolvedValueOnce(jsonResponse(200, {})),
    );

    await expect(decideConsent("c", "id", false)).rejects.toMatchObject({ code: "internal" });
    await expect(decideConsent("c", "id", false)).rejects.toMatchObject({ code: "internal" });
    await expect(decideConsent("c", "id", false)).rejects.toMatchObject({ code: "internal" });
  });

  it("surfaces an invalid decision from the server", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(400, { error: { code: "invalid_input" } })));

    await expect(decideConsent("c", "id", true)).rejects.toMatchObject({ code: "invalid_input" });
  });

  it("lists grants and tolerates a missing list", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValueOnce(jsonResponse(200, { grants: [{ id: "g" }] })).mockResolvedValueOnce(jsonResponse(200, {})),
    );

    await expect(listConnectedApps()).resolves.toEqual([{ id: "g" }]);
    await expect(listConnectedApps()).resolves.toEqual([]);
  });

  it("revokes a grant with CSRF and accepts 204", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(revokeConnectedApp("csrf-2", "cho_g_9")).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/oauth/grants/cho_g_9",
      expect.objectContaining({ method: "DELETE", headers: expect.objectContaining({ "X-CSRF-Token": "csrf-2" }) }),
    );
  });

  it("surfaces a rejected revoke", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(403, { error: { code: "csrf_invalid" } })));

    await expect(revokeConnectedApp("bad", "cho_g_9")).rejects.toMatchObject({ code: "csrf_invalid" });
  });
});
