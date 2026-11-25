export interface ConsentRequest {
  readonly id: string;
  readonly client_id: string;
  readonly client_name: string;
  readonly client_uri: string;
  readonly client_host: string;
  readonly metadata_document: boolean;
  readonly redirect_uri: string;
  readonly scopes: readonly string[];
  readonly resource: string;
  readonly expires_at: string;
}

export interface ConnectedApp {
  readonly id: string;
  readonly client_id: string;
  readonly client_name: string;
  readonly scopes: readonly string[];
  readonly created_at: string;
  readonly last_used_at: string | null;
}

export type OAuthErrorCode =
  | "not_found"
  | "unauthenticated"
  | "csrf_invalid"
  | "invalid_input"
  | "network"
  | "internal";

export class OAuthError extends Error {
  readonly code: OAuthErrorCode;

  constructor(code: OAuthErrorCode) {
    super(code);
    this.name = "OAuthError";
    this.code = code;
  }
}

const knownCodes: readonly OAuthErrorCode[] = [
  "not_found",
  "unauthenticated",
  "csrf_invalid",
  "invalid_input",
  "internal",
];

async function send(path: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(path, {
      ...init,
      credentials: "same-origin",
      headers: { Accept: "application/json", ...init.headers },
    });
  } catch {
    throw new OAuthError("network");
  }
}

async function failure(response: Response): Promise<OAuthError> {
  const body: unknown = await response.json().catch(() => null);
  const code = errorCode(body);
  if (code !== null) {
    return new OAuthError(code);
  }
  if (response.status === 401) return new OAuthError("unauthenticated");
  if (response.status === 404) return new OAuthError("not_found");
  return new OAuthError("internal");
}

function errorCode(body: unknown): OAuthErrorCode | null {
  if (typeof body !== "object" || body === null || !("error" in body)) {
    return null;
  }
  const error = (body as { error?: { code?: unknown } }).error;
  const code = error?.code;
  return typeof code === "string" && (knownCodes as readonly string[]).includes(code)
    ? (code as OAuthErrorCode)
    : null;
}

export async function getConsentRequest(id: string): Promise<ConsentRequest> {
  const response = await send(`/api/oauth/requests/${encodeURIComponent(id)}`, { method: "GET" });
  if (!response.ok) {
    throw await failure(response);
  }
  return (await response.json()) as ConsentRequest;
}

// decideConsent returns the URL the browser must be sent to. Only http(s)
// targets are accepted; the server builds this from a validated redirect_uri.
export async function decideConsent(csrfToken: string, id: string, approve: boolean): Promise<string> {
  const response = await send(`/api/oauth/requests/${encodeURIComponent(id)}/decision`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken },
    body: JSON.stringify({ approve }),
  });
  if (!response.ok) {
    throw await failure(response);
  }
  const body = (await response.json()) as { redirect_url?: unknown };
  if (typeof body.redirect_url !== "string" || !isWebURL(body.redirect_url)) {
    throw new OAuthError("internal");
  }
  return body.redirect_url;
}

export async function listConnectedApps(): Promise<ConnectedApp[]> {
  const response = await send("/api/oauth/grants", { method: "GET" });
  if (!response.ok) {
    throw await failure(response);
  }
  const body = (await response.json()) as { grants?: ConnectedApp[] };
  return body.grants ?? [];
}

export async function revokeConnectedApp(csrfToken: string, id: string): Promise<void> {
  const response = await send(`/api/oauth/grants/${encodeURIComponent(id)}`, {
    method: "DELETE",
    headers: { "X-CSRF-Token": csrfToken },
  });
  if (!response.ok) {
    throw await failure(response);
  }
}

function isWebURL(raw: string): boolean {
  try {
    const { protocol } = new URL(raw);
    return protocol === "https:" || protocol === "http:";
  } catch {
    return false;
  }
}
