export const MIN_PASSWORD_LENGTH = 12;
export const MAX_PASSWORD_LENGTH = 1024;
export const USERNAME_PATTERN = /^[a-zA-Z0-9._-]{1,64}$/;

export type ApiErrorCode =
  | "setup_complete"
  | "username_taken"
  | "invalid_input"
  | "internal"
  | "network";

export class ApiError extends Error {
  readonly code: ApiErrorCode;

  constructor(code: ApiErrorCode) {
    super(code);
    this.name = "ApiError";
    this.code = code;
  }
}

export interface SetupStatus {
  readonly setup_required: boolean;
}

export interface SetupInput {
  readonly username: string;
  readonly password: string;
}

const knownCodes: readonly ApiErrorCode[] = ["setup_complete", "username_taken", "invalid_input", "internal"];

async function request<T>(path: string, init: RequestInit): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, {
      ...init,
      credentials: "same-origin",
      headers: { Accept: "application/json", ...init.headers },
    });
  } catch {
    throw new ApiError("network");
  }
  const body: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const code = extractErrorCode(body);
    throw new ApiError(code);
  }
  return body as T;
}

function extractErrorCode(body: unknown): ApiErrorCode {
  if (typeof body === "object" && body !== null && "error" in body) {
    const error = (body as { error?: { code?: unknown } }).error;
    const code = error?.code;
    if (typeof code === "string" && (knownCodes as readonly string[]).includes(code)) {
      return code as ApiErrorCode;
    }
  }
  return "internal";
}

export function getSetupStatus(): Promise<SetupStatus> {
  return request<SetupStatus>("/api/setup", { method: "GET" });
}

export function createFirstAdmin(input: SetupInput): Promise<unknown> {
  return request<unknown>("/api/setup", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}
