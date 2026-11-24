import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Dashboard } from "./Dashboard";
import type { Session } from "./session";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const adminSession: Session = {
  user: { id: "admin-1", username: "root", role: "admin" },
  csrf_token: "csrf-admin",
};

const userSession: Session = {
  user: { id: "user-1", username: "alice", role: "user" },
  csrf_token: "csrf-user",
};

describe("Dashboard", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("hides user administration from non-admins", () => {
    vi.stubGlobal("fetch", vi.fn());
    render(<Dashboard session={userSession} onSignedOut={() => {}} />);

    expect(screen.getByText(/Signed in as alice \(user\)/)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Users" })).not.toBeInTheDocument();
  });

  it("lists users for administrators and marks their own row", async () => {
    const users = [
      { id: "admin-1", username: "root", role: "admin", active: true, created_at: "x", updated_at: "x" },
      { id: "user-1", username: "alice", role: "user", active: true, created_at: "x", updated_at: "x" },
    ];
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, { users })));
    render(<Dashboard session={adminSession} onSignedOut={() => {}} />);

    expect(await screen.findByText("alice")).toBeInTheDocument();
    expect(screen.getByText("root (you)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Deactivate alice" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Deactivate root" })).toBeDisabled();
  });

  it("sends CSRF on mutations and reloads the list", async () => {
    const users = [{ id: "user-1", username: "alice", role: "user", active: true, created_at: "x", updated_at: "x" }];
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, { users }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(jsonResponse(200, { users: [{ ...users[0], active: false }] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<Dashboard session={adminSession} onSignedOut={() => {}} />);

    fireEvent.click(await screen.findByRole("button", { name: "Deactivate alice" }));

    expect(await screen.findByText("alice deactivated.")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/api/users/user-1",
      expect.objectContaining({ method: "PATCH", headers: expect.objectContaining({ "X-CSRF-Token": "csrf-admin" }) }),
    );
  });

  it("surfaces last-admin rejection from the server", async () => {
    const users = [{ id: "other", username: "bob", role: "admin", active: true, created_at: "x", updated_at: "x" }];
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, { users }))
        .mockResolvedValueOnce(jsonResponse(409, { error: { code: "last_admin" } }))
        .mockResolvedValueOnce(jsonResponse(200, { users })),
    );
    render(<Dashboard session={adminSession} onSignedOut={() => {}} />);

    fireEvent.click(await screen.findByRole("button", { name: "Make bob a user" }));

    expect(await screen.findByText("At least one active administrator must remain.")).toBeInTheDocument();
  });

  it("signs out through the logout endpoint", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    const onSignedOut = vi.fn();
    render(<Dashboard session={userSession} onSignedOut={onSignedOut} />);

    fireEvent.click(screen.getByRole("button", { name: "Sign out" }));

    await waitFor(() => expect(onSignedOut).toHaveBeenCalled());
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/session",
      expect.objectContaining({ method: "DELETE", headers: expect.objectContaining({ "X-CSRF-Token": "csrf-user" }) }),
    );
  });
});
