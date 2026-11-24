export const CONSENT_PATH = "/oauth/consent";

export function isConsentPath(pathname: string): boolean {
  return pathname === CONSENT_PATH;
}

/** The consent request ID from ?request=, or null when it is absent or empty. */
export function requestIdFromSearch(search: string): string | null {
  const id = new URLSearchParams(search).get("request");
  return id === null || id.trim() === "" ? null : id;
}
