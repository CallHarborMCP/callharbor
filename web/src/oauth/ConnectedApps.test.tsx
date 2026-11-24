import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ConnectedApps } from "./ConnectedApps";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const grant = {
  id: "cho_g_1",
  client_id: "https://claude.example.com/app",
  client_name: "Claude Desktop",
  scopes: ["mcp:read", "mcp:call"],
  created_at: "2026-11-20T09:30:00.000Z",
  last_used_at: "2026-11-24T18:05:00.000Z",
};

const otherGrant = {
  id: "cho_g_2",
  client_id: "cho_client_xyz",
  client_name: "",
  scopes: ["mcp:read"],
  created_at: "2026-11-21T10:00:00.000Z",
  last_used_at: null,
};

describe("ConnectedApps", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("lists grants with name, host, scopes, connected and last-used dates", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, { grants: [grant, otherGrant] })));
    render(<ConnectedApps csrfToken="csrf" />);

    const first = (await screen.findByText("Claude Desktop")).closest("li") as HTMLElement;
    expect(within(first).getByText("claude.example.com")).toBeInTheDocument();
    expect(within(first).getByText("mcp:read")).toBeInTheDocument();
    expect(within(first).getByText("mcp:call")).toBeInTheDocument();
    expect(within(first).getByText("View your calls, transcripts, recordings and profiles")).toBeInTheDocument();
    expect(within(first).getByText(/Connected/)).toBeInTheDocument();
    expect(within(first).getByText(/Last used/)).toBeInTheDocument();

    const second = screen.getByText("Unnamed app").closest("li") as HTMLElement;
    expect(within(second).getByText(/Not used yet/)).toBeInTheDocument();
    expect(within(second).getByText("cho_client_xyz")).toBeInTheDocument();
  });

  it("shows an empty state with the MCP server URL for the current origin", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, { grants: [] })));
    render(<ConnectedApps csrfToken="csrf" />);

    expect(await screen.findByText("No apps are connected yet.")).toBeInTheDocument();
    expect(screen.getByText(`${window.location.origin}/mcp`)).toBeInTheDocument();
  });

  it("reports a load failure and retries", async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new TypeError("offline"))
      .mockResolvedValueOnce(jsonResponse(200, { grants: [grant] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<ConnectedApps csrfToken="csrf" />);

    expect(await screen.findByRole("alert")).toHaveTextContent("The server could not be reached");
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));

    expect(await screen.findByText("Claude Desktop")).toBeInTheDocument();
  });

  it("asks for confirmation before revoking, and keeps access when cancelled", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { grants: [grant] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<ConnectedApps csrfToken="csrf" />);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke access for Claude Desktop" }));

    const dialog = screen.getByRole("group", { name: /Revoke access for Claude Desktop\?/ });
    await waitFor(() => expect(dialog).toHaveFocus());
    expect(fetchMock).toHaveBeenCalledTimes(1);

    fireEvent.click(within(dialog).getByRole("button", { name: "Keep access" }));
    expect(screen.queryByRole("group", { name: /Revoke access/ })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Revoke access for Claude Desktop" })).toHaveFocus();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("revokes with CSRF, removes the row and announces success", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, { grants: [grant, otherGrant] }))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    render(<ConnectedApps csrfToken="csrf-revoke" />);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke access for Claude Desktop" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke access" }));

    expect(await screen.findByRole("status")).toHaveTextContent(
      "Access for Claude Desktop was revoked. It can no longer call CallHarbor.",
    );
    expect(screen.queryByText("Claude Desktop")).not.toBeInTheDocument();
    expect(screen.getByText("Unnamed app")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenNthCalledWith(
      2,
      "/api/oauth/grants/cho_g_1",
      expect.objectContaining({ method: "DELETE", headers: expect.objectContaining({ "X-CSRF-Token": "csrf-revoke" }) }),
    );
    await waitFor(() => expect(screen.getByRole("heading", { name: "Connected apps" })).toHaveFocus());
  });

  it("shows the empty state after the last grant is revoked", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, { grants: [grant] }))
        .mockResolvedValueOnce(new Response(null, { status: 204 })),
    );
    render(<ConnectedApps csrfToken="csrf" />);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke access for Claude Desktop" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke access" }));

    expect(await screen.findByText("No apps are connected yet.")).toBeInTheDocument();
  });

  it("reloads the list when the grant was already revoked elsewhere", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, { grants: [grant] }))
      .mockResolvedValueOnce(jsonResponse(404, { error: { code: "not_found" } }))
      .mockResolvedValueOnce(jsonResponse(200, { grants: [] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<ConnectedApps csrfToken="csrf" />);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke access for Claude Desktop" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke access" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("no longer connected");
    expect(await screen.findByText("No apps are connected yet.")).toBeInTheDocument();
  });

  it("keeps the confirmation open and reports a network failure", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, { grants: [grant] }))
        .mockRejectedValueOnce(new TypeError("offline")),
    );
    render(<ConnectedApps csrfToken="csrf" />);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke access for Claude Desktop" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke access" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("The server could not be reached");
    expect(screen.getByRole("button", { name: "Revoke access" })).toBeEnabled();
  });

  it("disables other revoke controls while one revoke is in flight", async () => {
    let finish: (response: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, { grants: [grant, otherGrant] }))
        .mockReturnValueOnce(new Promise<Response>((resolve) => (finish = resolve))),
    );
    render(<ConnectedApps csrfToken="csrf" />);

    fireEvent.click(await screen.findByRole("button", { name: "Revoke access for Claude Desktop" }));
    fireEvent.click(screen.getByRole("button", { name: "Revoke access" }));

    expect(screen.getByRole("button", { name: "Revoking..." })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Keep access" })).toBeDisabled();
    finish(new Response(null, { status: 204 }));
    await waitFor(() => expect(screen.queryByText("Claude Desktop")).not.toBeInTheDocument());
  });
});
