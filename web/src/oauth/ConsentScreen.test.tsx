import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ConsentScreen } from "./ConsentScreen";
import type { Session } from "../identity/session";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const session: Session = {
  user: { id: "user-1", username: "alice", role: "user" },
  csrf_token: "csrf-alice",
};

const registeredRequest = {
  id: "cho_q_abc",
  client_id: "cho_client_123",
  client_name: "Claude Desktop",
  client_uri: "https://claude.example.com/app",
  client_host: "claude.example.com",
  metadata_document: false,
  redirect_uri: "https://claude.example.com/callback",
  scopes: ["mcp:read", "mcp:call"],
  resource: "https://harbor.example.com/mcp",
  expires_at: "2026-11-25T12:10:00.000Z",
};

function renderConsent(requestId: string | null = "cho_q_abc", navigate = vi.fn(), onSignedOut = vi.fn()) {
  render(
    <ConsentScreen
      session={session}
      requestId={requestId}
      onSignedOut={onSignedOut}
      navigate={navigate}
    />,
  );
  return { navigate, onSignedOut };
}

describe("ConsentScreen", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("explains an incomplete link without calling the server", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    renderConsent(null);

    expect(screen.getByRole("heading", { level: 1, name: "Connection request unavailable" })).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("This link is incomplete");
    expect(screen.queryByRole("button", { name: "Allow" })).not.toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("shows the request for a registered client with its identity, redirect host and permissions", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, registeredRequest)));
    renderConsent("cho_q_abc");

    expect(await screen.findByText("Claude Desktop")).toBeInTheDocument();
    expect(screen.getByText("The app supplied this name. CallHarbor has not verified it.")).toBeInTheDocument();
    const identity = screen.getByText("Registered as").parentElement as HTMLElement;
    expect(within(identity).getByText("cho_client_123")).toBeInTheDocument();
    expect(within(identity).getByText("(unverified)")).toBeInTheDocument();
    expect(screen.getByText(/anyone can/)).toBeInTheDocument();
    expect(screen.getByText(/send you back to/).textContent).toContain("claude.example.com");
    expect(screen.getByText("View your calls, transcripts, recordings and profiles")).toBeInTheDocument();
    expect(screen.getByText("Place and stop phone calls on your behalf")).toBeInTheDocument();
    expect(screen.getByText("Signed in as alice.")).toBeInTheDocument();
  });

  it("names the client_id host as the domain for metadata-document clients", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          ...registeredRequest,
          client_id: "https://ids.example.org/app",
          metadata_document: true,
          scopes: ["mcp:read"],
        }),
      ),
    );
    renderConsent();

    const identity = (await screen.findByText("Domain")).parentElement as HTMLElement;
    expect(within(identity).getByText("ids.example.org")).toBeInTheDocument();
    expect(screen.getByText(/identified by the domain shown above/)).toBeInTheDocument();
    expect(screen.queryByText("Registered as")).not.toBeInTheDocument();
  });

  it("never shows a client_uri host as the domain of a metadata-document client", async () => {
    // A metadata document fetched from evil.example may claim any client_uri.
    // The server also sends a spoofed client_host. Neither may become the domain.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          ...registeredRequest,
          client_id: "https://evil.example/c.json",
          client_uri: "https://chatgpt.com",
          client_host: "chatgpt.com",
          metadata_document: true,
          scopes: ["mcp:read"],
        }),
      ),
    );
    renderConsent();

    const identity = (await screen.findByText("Domain")).parentElement as HTMLElement;
    expect(within(identity).getByText("evil.example")).toBeInTheDocument();
    expect(identity).not.toHaveTextContent("chatgpt.com");
  });

  it("never shows a registered client's client_uri or client_host as a verified domain", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          ...registeredRequest,
          client_id: "cho_client_999",
          client_uri: "https://chatgpt.com",
          client_host: "chatgpt.com",
          metadata_document: false,
        }),
      ),
    );
    renderConsent();

    const identity = (await screen.findByText("Registered as")).parentElement as HTMLElement;
    expect(within(identity).getByText("cho_client_999")).toBeInTheDocument();
    expect(within(identity).getByText("(unverified)")).toBeInTheDocument();
    expect(screen.queryByText("Domain")).not.toBeInTheDocument();
    expect(identity).not.toHaveTextContent("chatgpt.com");
  });

  it("renders client-supplied text as text, never as markup", async () => {
    const hostile = {
      ...registeredRequest,
      client_name: "<img src=x onerror=alert(1)>Evil",
      client_uri: "javascript:alert(1)",
      client_host: "evil.example.com",
    };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, hostile)));
    const { container } = render(
      <ConsentScreen session={session} requestId="cho_q_abc" onSignedOut={() => {}} navigate={() => {}} />,
    );

    expect(await screen.findByText("<img src=x onerror=alert(1)>Evil")).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("a")).toBeNull();
  });

  it("links the client website only when it is https, with safe rel", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, registeredRequest)));
    renderConsent();

    const link = await screen.findByRole("link", { name: /claude\.example\.com/ });
    expect(link).toHaveAttribute("href", "https://claude.example.com/app");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });

  it("does not link a non-https client website", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(jsonResponse(200, { ...registeredRequest, client_uri: "http://claude.example.com" })),
    );
    const { container } = render(
      <ConsentScreen session={session} requestId="cho_q_abc" onSignedOut={() => {}} navigate={() => {}} />,
    );

    await screen.findByText("Claude Desktop");
    expect(container.querySelector("a")).toBeNull();
  });

  it("names the client as unnamed when it sends no name", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, { ...registeredRequest, client_name: "" })));
    renderConsent();

    expect(await screen.findByText("Unnamed app")).toBeInTheDocument();
  });

  it("describes an unrecognised scope cautiously", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, { ...registeredRequest, scopes: ["mcp:admin"] })));
    renderConsent();

    expect(await screen.findByText(/An unrecognised permission/)).toBeInTheDocument();
    expect(screen.getByText("mcp:admin")).toBeInTheDocument();
  });

  it("approves, sends the browser to the redirect URL, and disables both buttons while busy", async () => {
    let resolveDecision: (response: Response) => void = () => {};
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, registeredRequest))
      .mockReturnValueOnce(new Promise<Response>((resolve) => (resolveDecision = resolve)));
    vi.stubGlobal("fetch", fetchMock);
    const { navigate } = renderConsent();

    fireEvent.click(await screen.findByRole("button", { name: "Allow" }));

    expect(screen.getByRole("button", { name: "Allowing..." })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Deny" })).toBeDisabled();
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/api/oauth/requests/cho_q_abc/decision",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ approve: true }),
        headers: expect.objectContaining({ "X-CSRF-Token": "csrf-alice" }),
      }),
    );

    resolveDecision(jsonResponse(200, { redirect_url: "https://claude.example.com/callback?code=x&state=y" }));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("https://claude.example.com/callback?code=x&state=y"));
  });

  it("denies and redirects with the server-provided URL", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, registeredRequest))
      .mockResolvedValueOnce(jsonResponse(200, { redirect_url: "https://claude.example.com/callback?error=access_denied" }));
    vi.stubGlobal("fetch", fetchMock);
    const { navigate } = renderConsent();

    fireEvent.click(await screen.findByRole("button", { name: "Deny" }));

    await waitFor(() =>
      expect(navigate).toHaveBeenCalledWith("https://claude.example.com/callback?error=access_denied"),
    );
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/api/oauth/requests/cho_q_abc/decision",
      expect.objectContaining({ body: JSON.stringify({ approve: false }) }),
    );
  });

  it("refuses a redirect URL that is not http or https", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, registeredRequest))
        .mockResolvedValueOnce(jsonResponse(200, { redirect_url: "javascript:alert(1)" })),
    );
    const { navigate } = renderConsent();

    fireEvent.click(await screen.findByRole("button", { name: "Allow" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("Something went wrong");
    expect(navigate).not.toHaveBeenCalled();
  });

  it("shows a clear message and no decision buttons when the request expired or was used", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(404, { error: { code: "not_found" } })));
    renderConsent();

    expect(await screen.findByRole("heading", { name: "Connection request unavailable" })).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("expired or was already answered");
    expect(screen.queryByRole("button", { name: "Allow" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Deny" })).not.toBeInTheDocument();
  });

  it("moves to the unavailable state when the request expires during a decision", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, registeredRequest))
        .mockResolvedValueOnce(jsonResponse(404, { error: { code: "not_found" } })),
    );
    renderConsent();

    fireEvent.click(await screen.findByRole("button", { name: "Allow" }));

    expect(await screen.findByRole("heading", { name: "Connection request unavailable" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Allow" })).not.toBeInTheDocument();
  });

  it("keeps the decision open for retry after a network failure", async () => {
    const navigate = vi.fn();
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, registeredRequest))
        .mockRejectedValueOnce(new TypeError("offline"))
        .mockResolvedValueOnce(jsonResponse(200, { redirect_url: "https://claude.example.com/callback" })),
    );
    renderConsent("cho_q_abc", navigate);

    fireEvent.click(await screen.findByRole("button", { name: "Allow" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The server could not be reached");
    expect(screen.getByRole("button", { name: "Allow" })).toBeEnabled();

    fireEvent.click(screen.getByRole("button", { name: "Allow" }));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("https://claude.example.com/callback"));
  });

  it("shows a retry control when the request cannot be loaded", async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new TypeError("offline"))
      .mockResolvedValueOnce(jsonResponse(200, registeredRequest));
    vi.stubGlobal("fetch", fetchMock);
    renderConsent();

    expect(await screen.findByRole("alert")).toHaveTextContent("The server could not be reached");
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));

    expect(await screen.findByText("Claude Desktop")).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("returns to sign-in when the session has ended", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(401, { error: { code: "unauthenticated" } })));
    const { onSignedOut } = renderConsent();

    await waitFor(() => expect(onSignedOut).toHaveBeenCalledTimes(1));
  });

  it("returns to sign-in when the session ends during a decision", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, registeredRequest))
        .mockResolvedValueOnce(jsonResponse(401, { error: { code: "unauthenticated" } })),
    );
    const { onSignedOut, navigate } = renderConsent();

    fireEvent.click(await screen.findByRole("button", { name: "Allow" }));

    await waitFor(() => expect(onSignedOut).toHaveBeenCalledTimes(1));
    expect(navigate).not.toHaveBeenCalled();
  });

  it("signs out to choose another account", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse(200, registeredRequest))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    const { onSignedOut } = renderConsent();

    fireEvent.click(await screen.findByRole("button", { name: "Not you? Sign out" }));

    await waitFor(() => expect(onSignedOut).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/api/session",
      expect.objectContaining({ method: "DELETE", headers: expect.objectContaining({ "X-CSRF-Token": "csrf-alice" }) }),
    );
  });

  it("reports a failed sign-out without leaving the consent screen", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(jsonResponse(200, registeredRequest))
        .mockResolvedValueOnce(jsonResponse(500, {})),
    );
    const { onSignedOut } = renderConsent();

    fireEvent.click(await screen.findByRole("button", { name: "Not you? Sign out" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("Sign-out failed");
    expect(onSignedOut).not.toHaveBeenCalled();
  });

  it("moves focus to the heading once the request is loaded", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, registeredRequest)));
    renderConsent();
    await screen.findByText("Claude Desktop");

    await waitFor(() =>
      expect(screen.getByRole("heading", { level: 1, name: "Allow app access to CallHarbor" })).toHaveFocus(),
    );
  });
});
