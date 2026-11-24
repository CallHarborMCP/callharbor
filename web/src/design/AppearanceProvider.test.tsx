import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { useContext, useEffect } from "react";
import { AppearanceProvider } from "./AppearanceProvider";
import { AppearanceContext, type AppearanceContextValue } from "./appearanceContext";
import { DEFAULT_APPEARANCE, storageKey } from "./appearance";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

interface Probe {
  readonly onValue: (value: AppearanceContextValue) => void;
}

function ValueProbe({ onValue }: Probe) {
  const value = useContext(AppearanceContext);
  useEffect(() => {
    if (value !== null) {
      onValue(value);
    }
  }, [value, onValue]);
  return null;
}

function mountProvider(userId: string | null, csrfToken: string | null = null) {
  const values: AppearanceContextValue[] = [];
  const onValue = (value: AppearanceContextValue) => {
    values.push(value);
  };
  render(
    <AppearanceProvider userId={userId} csrfToken={csrfToken}>
      <ValueProbe onValue={onValue} />
    </AppearanceProvider>,
  );
  return { latest: () => values[values.length - 1] };
}

function mockMatchMedia(prefersDark: boolean) {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: prefersDark && query.includes("dark"),
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  })) as unknown as typeof window.matchMedia;
}

describe("AppearanceProvider", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    window.localStorage.clear();
    delete document.documentElement.dataset.theme;
    delete document.documentElement.dataset.preset;
    delete document.documentElement.dataset.accent;
    delete document.documentElement.dataset.density;
  });

  it("applies the server appearance and caches it for the signed-in user", async () => {
    mockMatchMedia(false);
    const server = { theme: "dark", preset: "harbor", accent: "teal", density: "compact" };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, server)));

    const probe = mountProvider("u1", "tok");

    await waitFor(() => expect(probe.latest()?.appearance).toEqual(server));
    expect(probe.latest()?.scheme).toBe("dark");
    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(document.documentElement.dataset.preset).toBe("harbor");
    expect(JSON.parse(window.localStorage.getItem(storageKey("u1")) ?? "{}")).toEqual(server);
  });

  it("keeps the cached value when the server cannot be reached", async () => {
    mockMatchMedia(false);
    window.localStorage.setItem(storageKey("u1"), JSON.stringify({ ...DEFAULT_APPEARANCE, accent: "violet" }));
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("offline")));

    const probe = mountProvider("u1", "tok");

    await waitFor(() => expect(probe.latest()?.appearance.accent).toBe("violet"));
    expect(document.documentElement.dataset.accent).toBe("violet");
  });

  it("saves a change on the server and caches the saved value", async () => {
    mockMatchMedia(false);
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, DEFAULT_APPEARANCE));
    vi.stubGlobal("fetch", fetchMock);

    const probe = mountProvider("u1", "tok");
    await waitFor(() => expect(probe.latest()).toBeDefined());
    fetchMock.mockClear();
    fetchMock.mockResolvedValue(jsonResponse(200, { ...DEFAULT_APPEARANCE, density: "compact" }));

    act(() => probe.latest()?.update({ density: "compact" }));

    expect(probe.latest()?.appearance.density).toBe("compact");
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/preferences/appearance",
        expect.objectContaining({ method: "PUT", headers: expect.objectContaining({ "X-CSRF-Token": "tok" }) }),
      ),
    );
    await waitFor(() =>
      expect(JSON.parse(window.localStorage.getItem(storageKey("u1")) ?? "{}")).toMatchObject({ density: "compact" }),
    );
  });

  it("keeps the local choice for this view and does not call the server when signed out", async () => {
    mockMatchMedia(false);
    const fetchMock = vi.fn().mockRejectedValue(new TypeError("offline"));
    vi.stubGlobal("fetch", fetchMock);

    const probe = mountProvider(null);
    act(() => probe.latest()?.update({ theme: "light" }));

    expect(probe.latest()?.appearance.theme).toBe("light");
    expect(probe.latest()?.scheme).toBe("light");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("does not save to the server when the signed-in user has no CSRF token", async () => {
    mockMatchMedia(false);
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, DEFAULT_APPEARANCE));
    vi.stubGlobal("fetch", fetchMock);

    const probe = mountProvider("u1", null);
    await waitFor(() => expect(probe.latest()).toBeDefined());
    fetchMock.mockClear();
    act(() => probe.latest()?.update({ accent: "green" }));

    expect(probe.latest()?.appearance.accent).toBe("green");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("follows live system colour-scheme changes when the theme is system", async () => {
    let listener: ((event: MediaQueryListEvent) => void) | undefined;
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: (_type: string, handler: (event: MediaQueryListEvent) => void) => {
        listener = handler;
      },
      removeEventListener: vi.fn(),
    })) as unknown as typeof window.matchMedia;

    const probe = mountProvider(null);
    expect(probe.latest()?.scheme).toBe("light");

    act(() => listener?.({ matches: true } as MediaQueryListEvent));

    expect(probe.latest()?.scheme).toBe("dark");
    expect(document.documentElement.dataset.theme).toBe("dark");
  });
});
