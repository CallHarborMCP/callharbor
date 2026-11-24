import { afterEach, describe, expect, it, vi } from "vitest";
import { createUser, listUsers, resetPassword, updateUser, UserAdminError } from "./users";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("user administration API", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("lists users with the Accept header and same-origin credentials", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { users: [{ id: "u1" }] }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(listUsers()).resolves.toEqual([{ id: "u1" }]);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/users",
      expect.objectContaining({ method: "GET", credentials: "same-origin", headers: { Accept: "application/json" } }),
    );
  });

  it("creates a user with CSRF and a JSON body", async () => {
    const created = { id: "u2", username: "bob", role: "user" };
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(201, created));
    vi.stubGlobal("fetch", fetchMock);

    await expect(createUser("tok", { username: "bob", password: "correct horse battery", role: "user" })).resolves.toEqual(created);
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/users",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ username: "bob", password: "correct horse battery", role: "user" }),
        headers: { Accept: "application/json", "X-CSRF-Token": "tok", "Content-Type": "application/json" },
      }),
    );
  });

  it("encodes the user id in PATCH and PUT paths", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await updateUser("tok", "a/b", { active: false });
    await resetPassword("tok", "a/b", "correct horse battery");

    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/users/a%2Fb");
    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ method: "PATCH", body: JSON.stringify({ active: false }) });
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/users/a%2Fb/password");
    expect(fetchMock.mock.calls[1]?.[1]).toMatchObject({ method: "PUT", body: JSON.stringify({ password: "correct horse battery" }) });
  });

  it("surfaces the server error code, or internal when the body is not an error", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(409, { error: { code: "username_taken" } }))
        .mockResolvedValueOnce(new Response("not json", { status: 500 }))
        .mockResolvedValueOnce(jsonResponse(400, { error: { code: 7 } })),
    );

    await expect(createUser("tok", { username: "bob", password: "correct horse battery", role: "user" })).rejects.toMatchObject({ code: "username_taken" });
    await expect(updateUser("tok", "u1", { role: "admin" })).rejects.toMatchObject({ code: "internal" });
    await expect(resetPassword("tok", "u1", "correct horse battery")).rejects.toMatchObject({ code: "internal" });
  });

  it("reports a network failure as a UserAdminError", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("offline")));

    const error = await listUsers().catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(UserAdminError);
    expect((error as UserAdminError).code).toBe("network");
  });
});
