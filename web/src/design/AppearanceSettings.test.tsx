import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { AppearanceProvider } from "./AppearanceProvider";
import { AppearanceSettings } from "./AppearanceSettings";
import { storageKey } from "./appearance";

function renderSettings(userId: string | null = "user-1", csrfToken: string | null = "csrf-token") {
  return render(
    <AppearanceProvider userId={userId} csrfToken={csrfToken}>
      <AppearanceSettings />
    </AppearanceProvider>,
  );
}

function pressed(group: string, name: string) {
  return within(screen.getByRole("group", { name: group })).getByRole("button", { name });
}

describe("AppearanceSettings", () => {
  beforeEach(() => {
    window.localStorage.clear();
    delete document.documentElement.dataset.theme;
  });

  afterEach(() => {
    cleanup();
  });

  it("shows the system theme as the default choice", () => {
    renderSettings();
    expect(pressed("Theme", "System")).toHaveAttribute("aria-pressed", "true");
    expect(pressed("Density", "Comfortable")).toHaveAttribute("aria-pressed", "true");
  });

  it("applies each choice immediately and saves it to the server for the signed-in user", async () => {
    const saved: unknown[] = [];
    const fetchMock = vi.fn(async (_url: string, init?: RequestInit) => {
      const body = JSON.parse(String(init?.body ?? "{}")) as unknown;
      saved.push(body);
      return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchMock);
    renderSettings("user-1");

    fireEvent.click(pressed("Theme", "Dark"));
    fireEvent.click(pressed("Design preset", "Harbor"));
    fireEvent.click(screen.getByRole("button", { name: "Violet" }));
    fireEvent.click(pressed("Density", "Compact"));

    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(document.documentElement.dataset.preset).toBe("harbor");
    expect(document.documentElement.dataset.accent).toBe("violet");
    expect(document.documentElement.dataset.density).toBe("compact");

    const final = { theme: "dark", preset: "harbor", accent: "violet", density: "compact" };
    await vi.waitFor(() => expect(saved.at(-1)).toEqual(final));
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/api/preferences/appearance",
      expect.objectContaining({ method: "PUT" }),
    );
    await vi.waitFor(() =>
      expect(JSON.parse(window.localStorage.getItem(storageKey("user-1")) ?? "{}")).toEqual({
        theme: "dark",
        preset: "harbor",
        accent: "violet",
        density: "compact",
      }),
    );
    vi.unstubAllGlobals();
  });

  it("restores the saved choices on the next render", () => {
    window.localStorage.setItem(
      storageKey("user-1"),
      JSON.stringify({ theme: "light", preset: "harbor", accent: "green", density: "compact" }),
    );
    renderSettings("user-1");

    expect(pressed("Theme", "Light")).toHaveAttribute("aria-pressed", "true");
    expect(pressed("Design preset", "Harbor")).toHaveAttribute("aria-pressed", "true");
    expect(pressed("Density", "Compact")).toHaveAttribute("aria-pressed", "true");
  });

  it("does not persist choices when there is no signed-in user", () => {
    renderSettings(null);
    fireEvent.click(pressed("Theme", "Dark"));
    expect(window.localStorage.length).toBe(0);
  });
});
