import { beforeEach, describe, expect, it } from "vitest";
import {
  DEFAULT_APPEARANCE,
  loadAppearance,
  parseAppearance,
  resolveScheme,
  saveAppearance,
  storageKey,
} from "./appearance";

class MemoryStorage implements Storage {
  private readonly items = new Map<string, string>();
  get length() {
    return this.items.size;
  }
  clear() {
    this.items.clear();
  }
  getItem(key: string) {
    return this.items.get(key) ?? null;
  }
  key(index: number) {
    return [...this.items.keys()][index] ?? null;
  }
  removeItem(key: string) {
    this.items.delete(key);
  }
  setItem(key: string, value: string) {
    this.items.set(key, value);
  }
}

describe("appearance", () => {
  let storage: MemoryStorage;

  beforeEach(() => {
    storage = new MemoryStorage();
  });

  it("falls back to defaults for unknown or missing values", () => {
    expect(parseAppearance(null)).toEqual(DEFAULT_APPEARANCE);
    expect(parseAppearance({ theme: "neon", density: 3, accent: "red", preset: "x" })).toEqual(DEFAULT_APPEARANCE);
  });

  it("keeps valid values and replaces invalid ones individually", () => {
    expect(parseAppearance({ theme: "dark", accent: "teal", density: "compact", preset: "bogus" })).toEqual({
      theme: "dark",
      preset: DEFAULT_APPEARANCE.preset,
      accent: "teal",
      density: "compact",
    });
  });

  it("stores appearance per user and does not leak it to another user", () => {
    saveAppearance("alice", { ...DEFAULT_APPEARANCE, theme: "dark", density: "compact" }, storage);

    expect(loadAppearance("alice", storage)).toMatchObject({ theme: "dark", density: "compact" });
    expect(loadAppearance("bob", storage)).toEqual(DEFAULT_APPEARANCE);
    expect(storage.getItem(storageKey("alice"))).not.toBeNull();
  });

  it("ignores corrupt stored JSON", () => {
    storage.setItem(storageKey("alice"), "{not json");
    expect(loadAppearance("alice", storage)).toEqual(DEFAULT_APPEARANCE);
  });

  it("uses defaults without a signed-in user", () => {
    saveAppearance("alice", { ...DEFAULT_APPEARANCE, theme: "dark" }, storage);
    expect(loadAppearance(null, storage)).toEqual(DEFAULT_APPEARANCE);
  });

  it("resolves the system theme from the OS preference", () => {
    expect(resolveScheme("system", true)).toBe("dark");
    expect(resolveScheme("system", false)).toBe("light");
    expect(resolveScheme("light", true)).toBe("light");
    expect(resolveScheme("dark", false)).toBe("dark");
  });
});
