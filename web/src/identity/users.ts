export type Role = "admin" | "user";

export interface ManagedUser {
  readonly id: string;
  readonly username: string;
  readonly role: Role;
  readonly active: boolean;
  readonly created_at: string;
  readonly updated_at: string;
}

export interface CreateUserInput {
  readonly username: string;
  readonly password: string;
  readonly role: Role;
}

export interface UpdateUserInput {
  readonly role?: Role;
  readonly active?: boolean;
}

export class UserAdminError extends Error {
  readonly code: string;

  constructor(code: string) {
    super(code);
    this.name = "UserAdminError";
    this.code = code;
  }
}

async function call(method: string, path: string, csrfToken?: string, body?: unknown): Promise<Response> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (csrfToken !== undefined) {
    headers["X-CSRF-Token"] = csrfToken;
  }
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  let response: Response;
  try {
    response = await fetch(path, {
      method,
      credentials: "same-origin",
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new UserAdminError("network");
  }
  if (!response.ok) {
    const parsed: unknown = await response.json().catch(() => null);
    throw new UserAdminError(extractCode(parsed));
  }
  return response;
}

function extractCode(body: unknown): string {
  if (typeof body === "object" && body !== null && "error" in body) {
    const code = (body as { error?: { code?: unknown } }).error?.code;
    if (typeof code === "string") {
      return code;
    }
  }
  return "internal";
}

export async function listUsers(): Promise<ManagedUser[]> {
  const response = await call("GET", "/api/users");
  const body = (await response.json()) as { users: ManagedUser[] };
  return body.users;
}

export async function createUser(csrfToken: string, input: CreateUserInput): Promise<ManagedUser> {
  const response = await call("POST", "/api/users", csrfToken, input);
  return (await response.json()) as ManagedUser;
}

export async function updateUser(csrfToken: string, id: string, input: UpdateUserInput): Promise<void> {
  await call("PATCH", `/api/users/${encodeURIComponent(id)}`, csrfToken, input);
}

export async function resetPassword(csrfToken: string, id: string, password: string): Promise<void> {
  await call("PUT", `/api/users/${encodeURIComponent(id)}/password`, csrfToken, { password });
}
