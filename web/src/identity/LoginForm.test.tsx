import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { LoginForm } from "./LoginForm";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function submit(username: string, password: string) {
  fireEvent.change(screen.getByLabelText("Username"), { target: { value: username } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: password } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
}

describe("LoginForm", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("signs in and reports the session", async () => {
    const session = { user: { id: "u1", username: "alice", role: "user" }, csrf_token: "tok" };
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, session));
    vi.stubGlobal("fetch", fetchMock);
    const onSignedIn = vi.fn();
    render(<LoginForm onSignedIn={onSignedIn} />);

    submit("alice", "correct horse battery");

    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(session));
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/session",
      expect.objectContaining({ method: "POST", body: JSON.stringify({ username: "alice", password: "correct horse battery" }) }),
    );
  });

  it("shows one generic message for invalid credentials", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(401, { error: { code: "invalid_credentials" } })));
    const onSignedIn = vi.fn();
    render(<LoginForm onSignedIn={onSignedIn} />);

    submit("alice", "wrong password value");

    expect(await screen.findByText("Invalid username or password.")).toBeInTheDocument();
    expect(onSignedIn).not.toHaveBeenCalled();
  });

  it("explains throttling without revealing account state", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(429, { error: { code: "throttled" } })));
    render(<LoginForm onSignedIn={() => {}} />);

    submit("alice", "anything here");

    expect(await screen.findByText(/Too many failed sign-in attempts/)).toBeInTheDocument();
  });

  it("does not call the server when fields are empty", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    render(<LoginForm onSignedIn={() => {}} />);

    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    expect(screen.getByText("Enter your username and password.")).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
