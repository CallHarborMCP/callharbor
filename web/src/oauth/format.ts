// Helpers for showing client-supplied values safely. Nothing here renders HTML;
// callers pass the results to React text nodes.

const scopeDescriptions = new Map<string, string>([
  ["mcp:read", "View your calls, transcripts, recordings and profiles"],
  ["mcp:call", "Place and stop phone calls on your behalf"],
]);

export function describeScope(scope: string): string {
  return scopeDescriptions.get(scope) ?? "An unrecognised permission. Allow only if you expected it.";
}

/** Host (and port) of a URL, or null when it cannot be parsed. */
export function hostOf(raw: string): string | null {
  try {
    return new URL(raw).host || null;
  } catch {
    return null;
  }
}

/** True only for https URLs, so client-supplied links never run script. */
export function isHttpsURL(raw: string): boolean {
  try {
    return new URL(raw).protocol === "https:";
  } catch {
    return false;
  }
}

const dateFormat = new Intl.DateTimeFormat("en-US", { dateStyle: "medium", timeStyle: "short" });

/** Formats an RFC 3339 timestamp for display, or returns the input if it is not a date. */
export function formatDate(raw: string): string {
  const date = new Date(raw);
  return Number.isNaN(date.getTime()) ? raw : dateFormat.format(date);
}
