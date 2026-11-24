# CallHarbor design system

Source: `web/src/design/`. Components consume tokens; they do not hard-code colors.

## Tokens

`tokens.css` defines CSS custom properties on `:root`, selected by data attributes set on `<html>`:

- `data-theme`: `light` or `dark`. Resolved from the user's choice; `system` follows `prefers-color-scheme`.
- `data-preset`: `console` (default) or `harbor`.
- `data-accent`: `blue` (default), `teal`, `violet`, `green`.
- `data-density`: `comfortable` (default) or `compact`.

Spacing, radius, control size (44px minimum tap target) and motion durations are tokens. Reduced motion sets durations to 0.

## Appearance preferences

- Model and validation: `appearance.ts`. Unknown values fall back to defaults per field.
- Provider: `AppearanceProvider.tsx` applies attributes and follows OS scheme changes.
- Settings UI: `AppearanceSettings.tsx`, English-only copy.
- Storage: per user on the server (`GET`/`PUT /api/preferences/appearance`, table `user_preferences`). The choice follows the account across devices. `localStorage` holds only a per-user cache for the first paint and is overwritten by the server value on sign-in.

Limitation: preferences are not yet synced across devices. Server persistence needs a per-user preferences API, which is not implemented.

## Layout and responsiveness

- Desktop (above 768px): sidebar navigation.
- Phone and tablet (768px and below): bottom navigation bar.
- No horizontal scrolling of the page; tested at 320, 360, 390, 768, 1024 and 1440px.

## Accessibility

- Focus ring uses a token, 3px outline, visible on all interactive controls.
- Toggle groups use `aria-pressed`; the swatch buttons are labelled.
- Settings changes are announced through a polite live region.
- axe checks (WCAG 2.0/2.1/2.2 A and AA) report no critical or serious violations on the settings screen in light and dark.

## Not yet covered

- Screenshot regression baselines are not stored; screenshots are generated for review only.
- Automated checks do not replace a visual review.
