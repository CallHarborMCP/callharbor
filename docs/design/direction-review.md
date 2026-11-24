# DEV-19 direction review

Two interactive prototypes were built and compared. The chosen direction is **Prototype A (Console)**, per the CEO decision recorded on DEV-19.

Screenshots (Playwright, 390x844 phone and 1440x900 desktop, full page):

- `screenshots/prototype-a-phone.png`, `screenshots/prototype-a-desktop.png`
- `screenshots/prototype-b-phone.png`, `screenshots/prototype-b-desktop.png`

Regenerate with `cd web && npx playwright test tests-e2e/screenshots.spec.ts`.

## Comparison

| Aspect | A: Console | B: Harbor cards |
| --- | --- | --- |
| Desktop navigation | Left sidebar | Top navigation |
| Phone navigation | Bottom bar | Bottom tabs |
| Call history | Dense table; cards on phones | Card list on all sizes |
| Typography | System font, 15px | System display font, 16px |
| Surfaces | Cool neutral | Warm neutral |
| Information density | High (operations tool) | Low to medium |

## Decision

Prototype A is the production direction. CallHarbor is an operations tool for call history, administration and diagnostics. The dense table and sidebar suit repeated scanning of many calls better than B's card-led layout. B's warmer surfaces are kept as the `harbor` preset, so users can still pick that look.

## Open item

The visual judgement of these screenshots is not recorded here. The implementer could not visually review pixel output, so the direction review needs a human or vision-capable reviewer to confirm the screenshots before release. This document does not claim a manual visual review was done.
