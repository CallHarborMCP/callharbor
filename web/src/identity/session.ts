export interface SessionUser {
  readonly id: string;
  readonly username: string;
  readonly role: "admin" | "user";
}

export interface Session {
  readonly user: SessionUser;
  readonly csrf_token: string;
}

export type SessionErrorCode = "invalid_credentials" | "throttled" | "network" | "internal";

export class SessionError extends Error {
  readonly code: SessionErrorCode;

  constructor(code: SessionErrorCode) {
    super(code);
    this.name = "SessionError";
    this.code = code;
  }
}

async function send(path: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(path, {
      ...init,
      credentials: "same-origin",
      headers: { Accept: "application/json", ...init.headers },
    });
  } catch {
    throw new SessionError("network");
  }
}

export async function login(username: string, password: string): Promise<Session> {
  const response = await send("/api/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
  });
  if (response.status === 429) {
    throw new SessionError("throttled");
  }
  if (response.status === 401) {
    throw new SessionError("invalid_credentials");
  }
  if (!response.ok) {
    throw new SessionError("internal");
  }
  return (await response.json()) as Session;
}

export async function logout(csrfToken: string): Promise<void> {
  const response = await send("/api/session", {
    method: "DELETE",
    headers: { "X-CSRF-Token": csrfToken },
  });
  if (!response.ok && response.status !== 401) {
    throw new SessionError("internal");
  }
}
