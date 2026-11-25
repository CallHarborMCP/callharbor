import { describe, expect, it } from "vitest";
import { describeScope, formatDate, hostOf, isHttpsURL } from "./format";
import { isConsentPath, requestIdFromSearch } from "./route";

describe("OAuth display helpers", () => {
  it("describes the two supported scopes and treats others cautiously", () => {
    expect(describeScope("mcp:read")).toBe("View your calls, transcripts, recordings and profiles");
    expect(describeScope("mcp:call")).toBe("Place and stop phone calls on your behalf");
    expect(describeScope("mcp:other")).toMatch(/unrecognised/);
  });

  it("returns the host of a URL and null for garbage", () => {
    expect(hostOf("https://claude.example.com:8443/app")).toBe("claude.example.com:8443");
    expect(hostOf("not a url")).toBeNull();
    expect(hostOf("mailto:someone")).toBeNull();
  });

  it("accepts only https for client-supplied links", () => {
    expect(isHttpsURL("https://example.com")).toBe(true);
    expect(isHttpsURL("http://example.com")).toBe(false);
    expect(isHttpsURL("javascript:alert(1)")).toBe(false);
    expect(isHttpsURL("%%%")).toBe(false);
  });

  it("formats valid timestamps and passes invalid ones through", () => {
    expect(formatDate("2026-11-20T09:30:00.000Z")).toMatch(/Nov 20, 2026/);
    expect(formatDate("yesterday-ish")).toBe("yesterday-ish");
  });
});

describe("consent route", () => {
  it("matches only the exact consent path", () => {
    expect(isConsentPath("/oauth/consent")).toBe(true);
    expect(isConsentPath("/oauth/consent/")).toBe(false);
    expect(isConsentPath("/")).toBe(false);
  });

  it("reads the request ID and treats empty values as absent", () => {
    expect(requestIdFromSearch("?request=cho_q_1")).toBe("cho_q_1");
    expect(requestIdFromSearch("?request=")).toBeNull();
    expect(requestIdFromSearch("?request=%20%20")).toBeNull();
    expect(requestIdFromSearch("")).toBeNull();
  });
});
