# Professional engineering standards — CallHarbor V1
status=locked_human_authorized

- Favor idiomatic Go and React/TypeScript, simple cohesive packages, explicit errors, context cancellation, tested concurrency and idempotency, scoped APIs, and well-defined domain boundaries.
- Maintain one lightweight modular monolith. No speculative general frameworks, microservices, generic repositories/services for every struct, or line-count-driven splitting. Use existing stable libraries for security, protocols, components and codecs where appropriate; check license/maintenance.
- Review complexity, test quality, auth isolation, latency, backpressure, concurrency, security and maintainability during real code review, not with self-made metric frameworks.
- Use Go fmt/vet/test/race, ESLint/TypeScript/Vitest, Playwright, Docker Compose E2E and Go/HTTP contract tests as appropriate. Add checks when the real implementation requires them; do not implement a general gate registry or meta-tests.
- Every bug fix should have a meaningful regression test where practicable; avoid brittle snapshots, retries that conceal flakiness, implementation-call-count-only tests and broad test skips.
- Document nontrivial architectural changes with a concise ADR only when a real decision requires it. Keep source and docs consistent. Generated code should not be edited manually.
- No arbitrary hard-coded RAM or function line-count ceilings. Measure idle/one/multiple-call resource utilization and optimize significant waste before release.
- Professionally polished, accessible, responsive React UI with production-build screenshot evidence is mandatory, not a secondary concern.
