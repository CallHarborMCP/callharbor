import { createContext, useContext } from "react";
import { DEFAULT_APPEARANCE, type Appearance, type Scheme } from "./appearance";

export interface AppearanceContextValue {
  readonly appearance: Appearance;
  readonly scheme: Scheme;
  readonly update: (patch: Partial<Appearance>) => void;
}

export const AppearanceContext = createContext<AppearanceContextValue | null>(null);

export function useAppearance(): AppearanceContextValue {
  const value = useContext(AppearanceContext);
  if (value === null) {
    return { appearance: DEFAULT_APPEARANCE, scheme: "light", update: () => {} };
  }
  return value;
}
