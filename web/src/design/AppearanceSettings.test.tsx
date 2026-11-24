import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { AppearanceProvider } from "./AppearanceProvider";
import { AppearanceSettings } from "./AppearanceSettings";
import { storageKey } from "./appearance";

function renderSettings(userId: string | null = "user-1") {
  return render(
    <AppearanceProvider userId={userId}>
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

  it("applies and persists each choice for the signed-in user", () => {
    renderSettings("user-1");

    fireEvent.click(pressed("Theme", "Dark"));
    fireEvent.click(pressed("Design preset", "Harbor"));
    fireEvent.click(screen.getByRole("button", { name: "Violet" }));
    fireEvent.click(pressed("Density", "Compact"));

    expect(document.documentElement.dataset.theme).toBe("dark");
    expect(document.documentElement.dataset.preset).toBe("harbor");
    expect(document.documentElement.dataset.accent).toBe("violet");
    expect(document.documentElement.dataset.density).toBe("compact");
    expect(JSON.parse(window.localStorage.getItem(storageKey("user-1")) ?? "{}")).toEqual({
      theme: "dark",
      preset: "harbor",
      accent: "violet",
      density: "compact",
    });
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
