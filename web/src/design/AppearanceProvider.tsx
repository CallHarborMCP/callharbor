import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { AppearanceContext, type AppearanceContextValue } from "./appearanceContext";
import {
  fetchAppearance,
  loadAppearance,
  putAppearance,
  resolveScheme,
  saveAppearance,
  type Appearance,
} from "./appearance";

const darkQuery = "(prefers-color-scheme: dark)";

function readPrefersDark(): boolean {
  return typeof window.matchMedia === "function" && window.matchMedia(darkQuery).matches;
}

export interface AppearanceProviderProps {
  readonly userId: string | null;
  readonly csrfToken?: string | null;
  readonly children: ReactNode;
}

export function AppearanceProvider({ userId, csrfToken = null, children }: AppearanceProviderProps) {
  return (
    <AppearanceSession key={userId ?? "anonymous"} userId={userId} csrfToken={csrfToken}>
      {children}
    </AppearanceSession>
  );
}

function AppearanceSession({ userId, csrfToken, children }: AppearanceProviderProps) {
  const [appearance, setAppearance] = useState<Appearance>(() => loadAppearance(userId));
  const [prefersDark, setPrefersDark] = useState<boolean>(readPrefersDark);

  useEffect(() => {
    if (userId === null) {
      return;
    }
    let active = true;
    fetchAppearance()
      .then((server) => {
        if (!active) return;
        setAppearance(server);
        saveAppearance(userId, server);
      })
      .catch(() => {
        // Keep the cached value for this page view when the server cannot be reached.
      });
    return () => {
      active = false;
    };
  }, [userId]);

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
      const token = csrfToken;
      const owner = userId;
      if (owner === null || token == null) {
        return;
      }
      putAppearance(token, next)
        .then((saved) => saveAppearance(owner, saved))
        .catch(() => {
          // The choice stays applied for this page view; the next load reflects the server value.
        });
    },
    [appearance, userId, csrfToken],
  );

  const value = useMemo<AppearanceContextValue>(() => ({ appearance, scheme, update }), [appearance, scheme, update]);

  return <AppearanceContext.Provider value={value}>{children}</AppearanceContext.Provider>;
}
