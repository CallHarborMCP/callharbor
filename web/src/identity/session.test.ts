import { afterEach, describe, expect, it, vi } from "vitest";
import { getSession, login, logout, SessionError } from "./session";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const session = { user: { id: "u1", username: "alice", role: "user" }, csrf_token: "tok" };

describe("session API", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("maps a rejected network request to a network error", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("offline")));

    const error = await login("alice", "correct horse battery").catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(SessionError);
    expect((error as SessionError).code).toBe("network");
  });

  it("reports throttled and internal login failures distinctly", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(jsonResponse(429, {})).mockResolvedValueOnce(jsonResponse(500, {})));

    await expect(login("alice", "anything here")).rejects.toMatchObject({ code: "throttled" });
    await expect(login("alice", "anything here")).rejects.toMatchObject({ code: "internal" });
  });

  it("returns the session on successful login", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, session)));

    await expect(login("alice", "correct horse battery")).resolves.toEqual(session);
  });

  it("returns null when no session exists and the session on success", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(jsonResponse(401, {})).mockResolvedValueOnce(jsonResponse(200, session)));

    await expect(getSession()).resolves.toBeNull();
    await expect(getSession()).resolves.toEqual(session);
  });

  it("fails getSession with an internal error on server failure and network failure", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(jsonResponse(500, {})).mockRejectedValueOnce(new TypeError("offline")));

    await expect(getSession()).rejects.toMatchObject({ code: "internal" });
    await expect(getSession()).rejects.toMatchObject({ code: "network" });
  });

  it("sends the CSRF token on logout and tolerates an already-expired session", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(new Response(null, { status: 204 })).mockResolvedValueOnce(jsonResponse(401, {}));
    vi.stubGlobal("fetch", fetchMock);

    await expect(logout("tok")).resolves.toBeUndefined();
    await expect(logout("tok")).resolves.toBeUndefined();
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/session",
      expect.objectContaining({ method: "DELETE", headers: expect.objectContaining({ "X-CSRF-Token": "tok" }) }),
    );
  });

  it("fails logout on server error", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(500, {})));

    await expect(logout("tok")).rejects.toMatchObject({ code: "internal" });
  });
});
