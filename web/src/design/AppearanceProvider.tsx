import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { AppearanceContext, type AppearanceContextValue } from "./appearanceContext";
import { loadAppearance, resolveScheme, saveAppearance, type Appearance } from "./appearance";

const darkQuery = "(prefers-color-scheme: dark)";

function readPrefersDark(): boolean {
  return typeof window.matchMedia === "function" && window.matchMedia(darkQuery).matches;
}

export interface AppearanceProviderProps {
  readonly userId: string | null;
  readonly children: ReactNode;
}

export function AppearanceProvider({ userId, children }: AppearanceProviderProps) {
  return <AppearanceSession key={userId ?? "anonymous"} userId={userId}>{children}</AppearanceSession>;
}

function AppearanceSession({ userId, children }: AppearanceProviderProps) {
  const [appearance, setAppearance] = useState<Appearance>(() => loadAppearance(userId));
  const [prefersDark, setPrefersDark] = useState<boolean>(readPrefersDark);

  useEffect(() => {
    if (typeof window.matchMedia !== "function") {
      return;
    }
    const query = window.matchMedia(darkQuery);
    const onChange = (event: MediaQueryListEvent) => setPrefersDark(event.matches);
    query.addEventListener("change", onChange);
    return () => query.removeEventListener("change", onChange);
  }, []);

  const scheme = resolveScheme(appearance.theme, prefersDark);

  useEffect(() => {
    const root = document.documentElement;
    root.dataset.theme = scheme;
    root.dataset.preset = appearance.preset;
    root.dataset.accent = appearance.accent;
    root.dataset.density = appearance.density;
    root.style.colorScheme = scheme;
  }, [scheme, appearance.preset, appearance.accent, appearance.density]);

  const update = useCallback(
    (patch: Partial<Appearance>) => {
      const next: Appearance = { ...appearance, ...patch };
      setAppearance(next);
      if (userId !== null) {
        saveAppearance(userId, next);
      }
    },
    [appearance, userId],
  );

  const value = useMemo<AppearanceContextValue>(() => ({ appearance, scheme, update }), [appearance, scheme, update]);

  return <AppearanceContext.Provider value={value}>{children}</AppearanceContext.Provider>;
}
