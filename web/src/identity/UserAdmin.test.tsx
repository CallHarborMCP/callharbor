import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { UserAdmin } from "./UserAdmin";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const alice = { id: "user-1", username: "alice", role: "user", active: true, created_at: "x", updated_at: "x" };
const bob = { id: "user-2", username: "bob", role: "admin", active: false, created_at: "x", updated_at: "x" };

describe("UserAdmin", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("shows a load error when the user list cannot be fetched", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(403, { error: { code: "forbidden" } })));

    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    expect(await screen.findByRole("alert")).toHaveTextContent("You are not allowed to make that change.");
  });

  it("shows a network message when the server is unreachable", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("offline")));

    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    expect(await screen.findByRole("alert")).toHaveTextContent("The server could not be reached");
  });

  it("reports a rejected mutation with its specific message and keeps the list", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, { users: [alice] }))
      .mockResolvedValueOnce(jsonResponse(409, { error: { code: "last_admin" } }))
      .mockResolvedValueOnce(jsonResponse(200, { users: [alice] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    fireEvent.click(await screen.findByRole("button", { name: "Deactivate alice" }));

    expect(await screen.findByText("At least one active administrator must remain.")).toBeInTheDocument();
    expect(screen.getByText("alice")).toBeInTheDocument();
  });

  it("shows a not-found message and refreshes after a mutation fails on a missing user", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, { users: [alice] }))
      .mockResolvedValueOnce(jsonResponse(404, { error: { code: "not_found" } }));
    vi.stubGlobal("fetch", fetchMock);
    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    fireEvent.click(await screen.findByRole("button", { name: "Make alice an admin" }));

    expect(await screen.findByText("That user no longer exists. Refresh the list.")).toBeInTheDocument();
  });

  it("shows the generic message for an unrecognised failure", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, { users: [alice] }))
      .mockResolvedValueOnce(jsonResponse(500, { error: { code: "weird" } }));
    vi.stubGlobal("fetch", fetchMock);
    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    fireEvent.click(await screen.findByRole("button", { name: "Deactivate alice" }));

    expect(await screen.findByText("Something went wrong. Try again.")).toBeInTheDocument();
  });

  it("changes a role and activates a deactivated user with status notices", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, { users: [alice, bob] }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(jsonResponse(200, { users: [{ ...alice, role: "admin" }, bob] }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }))
      .mockResolvedValueOnce(jsonResponse(200, { users: [{ ...alice, role: "admin" }, { ...bob, active: true }] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    fireEvent.click(await screen.findByRole("button", { name: "Make alice an admin" }));
    expect(await screen.findByText("Role updated for alice.")).toBeInTheDocument();

    fireEvent.click(await screen.findByRole("button", { name: "Activate bob" }));
    expect(await screen.findByText("bob activated.")).toBeInTheDocument();
    expect(fetchMock.mock.calls[1]?.[1]).toMatchObject({ method: "PATCH", body: JSON.stringify({ role: "admin" }) });
    expect(fetchMock.mock.calls[3]?.[1]).toMatchObject({ method: "PATCH", body: JSON.stringify({ active: true }) });
  });

  it("validates a reset password before calling the server", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(jsonResponse(200, { users: [alice] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    const input = await screen.findByLabelText("New password for alice");
    fireEvent.change(input, { target: { value: "short" } });
    fireEvent.click(screen.getByRole("button", { name: "Reset password" }));

    expect(await screen.findByText("Password must be 12 to 1024 characters.")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("validates the new user form before calling the server", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(jsonResponse(200, { users: [] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    fireEvent.change(await screen.findByLabelText("Username"), { target: { value: "bad name!" } });
    fireEvent.click(screen.getByRole("button", { name: "Create user" }));
    expect(await screen.findByText("Use 1 to 64 letters, numbers, dots, underscores or hyphens.")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Username"), { target: { value: "carol" } });
    fireEvent.change(screen.getByLabelText("Initial password"), { target: { value: "short" } });
    fireEvent.click(screen.getByRole("button", { name: "Create user" }));
    expect(await screen.findByText("Password must be 12 to 1024 characters.")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("creates a user and reports the new username", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, { users: [] }))
      .mockResolvedValueOnce(jsonResponse(201, { ...alice, username: "carol" }))
      .mockResolvedValueOnce(jsonResponse(200, { users: [{ ...alice, username: "carol" }] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<UserAdmin csrfToken="tok" currentUserId="admin-1" />);

    fireEvent.change(await screen.findByLabelText("Username"), { target: { value: "carol" } });
    fireEvent.change(screen.getByLabelText("Initial password"), { target: { value: "correct horse battery" } });
    fireEvent.click(screen.getByRole("button", { name: "Create user" }));

    expect(await screen.findByText("Created user carol.")).toBeInTheDocument();
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  });
});
