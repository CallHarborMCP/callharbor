import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { FirstAdminWizard } from "./FirstAdminWizard";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function fill(username: string, password: string, confirm: string) {
  fireEvent.change(screen.getByLabelText("Username"), { target: { value: username } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: password } });
  fireEvent.change(screen.getByLabelText("Confirm password"), { target: { value: confirm } });
}

describe("FirstAdminWizard", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("rejects invalid input without calling the server", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    render(<FirstAdminWizard onCreated={() => {}} />);

    fill("bad name!", "short", "different");
    fireEvent.click(screen.getByRole("button", { name: "Create administrator" }));

    expect(screen.getByText(/Use 1 to 64 letters/)).toBeInTheDocument();
    expect(screen.getByText(/at least 12 characters/)).toBeInTheDocument();
    expect(screen.getByText("Passwords do not match.")).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("posts the admin credentials and reports success", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(201, { id: "u1", role: "admin" }));
    vi.stubGlobal("fetch", fetchMock);
    const onCreated = vi.fn();
    render(<FirstAdminWizard onCreated={onCreated} />);

    fill("admin.one", "correct horse battery", "correct horse battery");
    fireEvent.click(screen.getByRole("button", { name: "Create administrator" }));

    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/setup",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ username: "admin.one", password: "correct horse battery" }),
      }),
    );
  });

  it("shows a clear message when setup is already complete", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(409, { error: { code: "setup_complete" } })));
    const onCreated = vi.fn();
    render(<FirstAdminWizard onCreated={onCreated} />);

    fill("admin", "correct horse battery", "correct horse battery");
    fireEvent.click(screen.getByRole("button", { name: "Create administrator" }));

    expect(await screen.findByText(/Setup is already complete/)).toBeInTheDocument();
    expect(onCreated).not.toHaveBeenCalled();
  });

  it("maps an unknown server error to a generic message", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(500, { error: { code: "stack trace here" } })));
    render(<FirstAdminWizard onCreated={() => {}} />);

    fill("admin", "correct horse battery", "correct horse battery");
    fireEvent.click(screen.getByRole("button", { name: "Create administrator" }));

    expect(await screen.findByText(/Something went wrong/)).toBeInTheDocument();
    expect(screen.queryByText(/stack trace/)).not.toBeInTheDocument();
  });
});
