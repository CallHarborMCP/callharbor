import { useAppearance } from "./appearanceContext";
import {
  ACCENTS,
  DENSITIES,
  PRESETS,
  THEMES,
  type Accent,
  type Appearance,
  type Density,
  type Preset,
  type Scheme,
  type Theme,
} from "./appearance";

const themeLabels: Record<Theme, string> = { system: "System", light: "Light", dark: "Dark" };
const presetLabels: Record<Preset, string> = { console: "Console", harbor: "Harbor" };
const presetHints: Record<Preset, string> = {
  console: "Dense sidebar layout with system typography.",
  harbor: "Warm neutral surfaces with roomier cards.",
};
const accentLabels: Record<Accent, string> = { blue: "Blue", teal: "Teal", violet: "Violet", green: "Green" };
const densityLabels: Record<Density, string> = { comfortable: "Comfortable", compact: "Compact" };

export function AppearanceSettings() {
  const { appearance, scheme, update } = useAppearance();

  return (
    <section aria-labelledby="appearance-heading" className="ch-panel">
      <h2 id="appearance-heading">Appearance</h2>
      <p className="ch-muted">Saved to your account and applied on every device you sign in on.</p>

      <div className="ch-setting" role="group" aria-labelledby="theme-label">
        <span id="theme-label" className="ch-setting-label">Theme</span>
        <div className="ch-segmented">
          {THEMES.map((theme) => (
            <ChoiceButton key={theme} active={appearance.theme === theme} onClick={() => update({ theme })}>
              {themeLabels[theme]}
            </ChoiceButton>
          ))}
        </div>
      </div>

      <div className="ch-setting" role="group" aria-labelledby="preset-label">
        <span id="preset-label" className="ch-setting-label">Design preset</span>
        <div className="ch-segmented">
          {PRESETS.map((preset) => (
            <ChoiceButton key={preset} active={appearance.preset === preset} onClick={() => update({ preset })}>
              {presetLabels[preset]}
            </ChoiceButton>
          ))}
        </div>
        <p className="ch-muted">{presetHints[appearance.preset]}</p>
      </div>

      <div className="ch-setting" role="group" aria-labelledby="accent-label">
        <span id="accent-label" className="ch-setting-label">Accent color</span>
        <div className="ch-segmented">
          {ACCENTS.map((accent) => (
            <button
              key={accent}
              type="button"
              className="ch-swatch"
              data-accent={accent}
              aria-pressed={appearance.accent === accent}
              aria-label={accentLabels[accent]}
              title={accentLabels[accent]}
              onClick={() => update({ accent })}
            />
          ))}
        </div>
      </div>

      <div className="ch-setting" role="group" aria-labelledby="density-label">
        <span id="density-label" className="ch-setting-label">Density</span>
        <div className="ch-segmented">
          {DENSITIES.map((density) => (
            <ChoiceButton key={density} active={appearance.density === density} onClick={() => update({ density })}>
              {densityLabels[density]}
            </ChoiceButton>
          ))}
        </div>
      </div>

      <AppearancePreview appearance={appearance} scheme={scheme} />
      <p role="status" className="sr-only" aria-live="polite">
        {`${themeLabels[appearance.theme]} theme, ${presetLabels[appearance.preset]} preset, ${accentLabels[appearance.accent]} accent, ${densityLabels[appearance.density]} density.`}
      </p>
    </section>
  );
}

function ChoiceButton({ active, onClick, children }: { active: boolean; onClick: () => void; children: string }) {
  return (
    <button type="button" aria-pressed={active} onClick={onClick}>
      {children}
    </button>
  );
}

function AppearancePreview({ appearance, scheme }: { appearance: Appearance; scheme: Scheme }) {
  return (
    <div className="ch-setting">
      <span className="ch-setting-label">Preview</span>
      <div className="ch-preview" aria-hidden="true">
        <div className="ch-preview-bar" />
        <div className="ch-preview-row">
          <span>Call with Dr. Novak</span>
          <span className="ch-status">Ended</span>
        </div>
        <div className="ch-preview-row">
          <span>Showing {scheme} scheme</span>
          <span className="ch-muted">{densityLabels[appearance.density]}</span>
        </div>
      </div>
    </div>
  );
}
