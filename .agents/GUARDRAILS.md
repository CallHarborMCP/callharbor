# CallHarbor immutable guardrails
status=locked_human_authorized

- Full V1.0.0, not MVP. Preserve the locked scope, security, UX, architecture and acceptance criteria. One lightweight Go service with embedded React assets + SQLite, deployed as one Docker container.
- NEVER push directly to protected main, force push, skip checks, weaken CI or knowingly merge broken code. Every production change gets tests and an independently reviewed PR.
- Use standard GitHub CLI, `mise` for pinned Go/Node versions, Go modules, npm, Playwright, Docker Compose and GitHub Actions. Do not alter global host toolchain versions, system configuration or unrelated projects; do not create redundant toolchain installers or meta-test frameworks.
- Source code must be idiomatic, cohesive, appropriately reviewed, secure and maintainable. Tests must verify actual behavior, including tenant isolation, OAuth, Telnyx/webhook/audio contracts, barge-in, recording, recovery, Docker deploy and real responsive UI. No fake CI success or missing end-to-end coverage.
- Never run live Telnyx phone calls or pay for AI during automated tests. Use independent Telnyx protocol fixtures and deterministic audio/Realtime emulator; disclose limits of simulation.
- Never expose or log secrets, other users' data, recordings, production credentials or personal data. Do not change global host configuration or unrelated projects.
- Recordings OFF for newly created users; configurable per user; respect all locked privacy controls. No country-specific announcements or legal decision automation.
- Keep full V1 release requirement: published image, source tag, release notes, README with authentic UI screenshots, backup/restore proof and clean-install documentation, green main. Release qualification requires genuine artifact verification, protected PRs and green CI.
- Locked files and SHA manifest are human-owned and may be changed only by explicit human authorization, never autonomously to weaken release policy.
