import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, createFirstAdmin, getSetupStatus, MAX_PASSWORD_LENGTH, MIN_PASSWORD_LENGTH, USERNAME_PATTERN } from "./api";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

describe("setup API", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("exposes the password and username rules used by the forms", () => {
    expect(MIN_PASSWORD_LENGTH).toBe(12);
    expect(MAX_PASSWORD_LENGTH).toBe(1024);
    expect(USERNAME_PATTERN.test("ops.admin-1")).toBe(true);
    expect(USERNAME_PATTERN.test("bad name")).toBe(false);
    expect(USERNAME_PATTERN.test("x".repeat(65))).toBe(false);
  });

  it("returns the setup status", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, { setup_required: true })));

    await expect(getSetupStatus()).resolves.toEqual({ setup_required: true });
  });

  it("maps known server error codes and falls back to internal for unknown ones", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(409, { error: { code: "setup_complete" } }))
        .mockResolvedValueOnce(jsonResponse(400, { error: { code: "invalid_input" } }))
        .mockResolvedValueOnce(jsonResponse(502, { error: { code: "surprise" } }))
        .mockResolvedValueOnce(new Response("<html>", { status: 500 })),
    );
    const input = { username: "root", password: "correct horse battery" };

    await expect(createFirstAdmin(input)).rejects.toMatchObject({ code: "setup_complete" });
    await expect(createFirstAdmin(input)).rejects.toMatchObject({ code: "invalid_input" });
    await expect(createFirstAdmin(input)).rejects.toMatchObject({ code: "internal" });
    await expect(createFirstAdmin(input)).rejects.toMatchObject({ code: "internal" });
  });

  it("reports a network failure as an ApiError", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("offline")));

    const error = await getSetupStatus().catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).code).toBe("network");
  });
});
