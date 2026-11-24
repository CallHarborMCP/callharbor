export type AIProtocol = "realtime_ws" | "live_ws";

export const PROTOCOL_LABELS: Record<AIProtocol, string> = {
  realtime_ws: "OpenAI Realtime (WebSocket)",
  live_ws: "GPT-Live (WebSocket)",
};

export const MAX_INSTRUCTIONS = 8192;

export interface AIProfileInput {
  readonly name: string;
  readonly protocol: AIProtocol;
  readonly endpoint_url: string;
  readonly model: string;
  readonly voice: string;
  readonly instructions: string;
  readonly api_secret_id?: string;
}

export function validateEndpoint(raw: string): string | null {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return "Enter a valid WebSocket URL.";
  }
  if (url.protocol === "wss:") {
    return null;
  }
  if (url.protocol === "ws:") {
    const loopback = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
    return loopback ? null : "Plain ws endpoints must be loopback. Use wss.";
  }
  return "Endpoint must use wss.";
}

export class AIProfileError extends Error {
  readonly code: string;

  constructor(code: string) {
    super(code);
    this.name = "AIProfileError";
    this.code = code;
  }
}

export async function createAIProfile(csrfToken: string, input: AIProfileInput): Promise<void> {
  let response: Response;
  try {
    response = await fetch("/api/ai-profiles", {
      method: "POST",
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-CSRF-Token": csrfToken,
      },
      body: JSON.stringify(input),
    });
  } catch {
    throw new AIProfileError("network");
  }
  if (!response.ok) {
    const parsed: unknown = await response.json().catch(() => null);
    throw new AIProfileError(extractCode(parsed));
  }
}

// createAIAPISecret stores an AI provider key with the write-only secrets API.
// The server never returns the value, so the caller keeps nothing after this.
export async function createAIAPISecret(csrfToken: string, name: string, value: string): Promise<string> {
  let response: Response;
  try {
    response = await fetch("/api/secrets", {
      method: "POST",
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-CSRF-Token": csrfToken,
      },
      body: JSON.stringify({ name, purpose: "ai_api_key", value }),
    });
  } catch {
    throw new AIProfileError("network");
  }
  if (!response.ok) {
    const parsed: unknown = await response.json().catch(() => null);
    throw new AIProfileError(extractCode(parsed));
  }
  const body = (await response.json()) as { id?: unknown };
  if (typeof body.id !== "string") {
    throw new AIProfileError("internal");
  }
  return body.id;
}

function extractCode(body: unknown): string {
  if (typeof body === "object" && body !== null && "error" in body) {
    const error = (body as { error?: { code?: unknown } }).error;
    if (typeof error?.code === "string") {
      return error.code;
    }
  }
  return "internal";
}
