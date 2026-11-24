export const THEMES = ["system", "light", "dark"] as const;
export type Theme = (typeof THEMES)[number];

export const PRESETS = ["console", "harbor"] as const;
export type Preset = (typeof PRESETS)[number];

export const ACCENTS = ["blue", "teal", "violet", "green"] as const;
export type Accent = (typeof ACCENTS)[number];

export const DENSITIES = ["comfortable", "compact"] as const;
export type Density = (typeof DENSITIES)[number];

export type Scheme = "light" | "dark";

export interface Appearance {
  readonly theme: Theme;
  readonly preset: Preset;
  readonly accent: Accent;
  readonly density: Density;
}

export const DEFAULT_APPEARANCE: Appearance = {
  theme: "system",
  preset: "console",
  accent: "blue",
  density: "comfortable",
};

export const STORAGE_KEY_PREFIX = "callharbor.appearance.v1:";

function pick<T extends string>(allowed: readonly T[], value: unknown, fallback: T): T {
  return typeof value === "string" && (allowed as readonly string[]).includes(value) ? (value as T) : fallback;
}

export function parseAppearance(value: unknown): Appearance {
  const source = typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
  return {
    theme: pick(THEMES, source.theme, DEFAULT_APPEARANCE.theme),
    preset: pick(PRESETS, source.preset, DEFAULT_APPEARANCE.preset),
    accent: pick(ACCENTS, source.accent, DEFAULT_APPEARANCE.accent),
    density: pick(DENSITIES, source.density, DEFAULT_APPEARANCE.density),
  };
}

export async function fetchAppearance(): Promise<Appearance> {
  const response = await fetch("/api/preferences/appearance", {
    method: "GET",
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  });
  if (!response.ok) {
    throw new Error("appearance unavailable");
  }
  return parseAppearance(await response.json());
}

export async function putAppearance(csrfToken: string, appearance: Appearance): Promise<Appearance> {
  const response = await fetch("/api/preferences/appearance", {
    method: "PUT",
    credentials: "same-origin",
    headers: { Accept: "application/json", "Content-Type": "application/json", "X-CSRF-Token": csrfToken },
    body: JSON.stringify(parseAppearance(appearance)),
  });
  if (!response.ok) {
    throw new Error("appearance not saved");
  }
  return parseAppearance(await response.json());
}

export function storageKey(userId: string): string {
  return `${STORAGE_KEY_PREFIX}${userId}`;
}

export function loadAppearance(userId: string | null, storage: Storage | undefined = safeStorage()): Appearance {
  if (userId === null || storage === undefined) {
    return DEFAULT_APPEARANCE;
  }
  try {
    const raw = storage.getItem(storageKey(userId));
    return raw === null ? DEFAULT_APPEARANCE : parseAppearance(JSON.parse(raw));
  } catch {
    return DEFAULT_APPEARANCE;
  }
}

export function saveAppearance(userId: string, appearance: Appearance, storage: Storage | undefined = safeStorage()): void {
  if (storage === undefined) {
    return;
  }
  try {
    storage.setItem(storageKey(userId), JSON.stringify(parseAppearance(appearance)));
  } catch {
    // Storage can be full or disabled; the choice still applies for this page view.
  }
}

export function resolveScheme(theme: Theme, systemPrefersDark: boolean): Scheme {
  if (theme === "system") {
    return systemPrefersDark ? "dark" : "light";
  }
  return theme;
}

function safeStorage(): Storage | undefined {
  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
}
