# CallHarbor immutable guardrails
status=locked_human_authorized

- Deliver the authorized issue completely, not a prototype. Preserve product scope, security, UX, architecture and acceptance criteria; no unrelated release work. One lightweight Go service with embedded React assets + SQLite, deployed as one Docker container.
- NEVER push directly to protected main, force push, skip checks, weaken CI or knowingly merge broken code. Every production change gets tests and an independently reviewed PR.
- Use standard GitHub CLI, `mise` for pinned Go/Node versions, Go modules, npm, Playwright, Docker Compose and GitHub Actions. Do not alter global host toolchain versions, system configuration or unrelated projects; do not create redundant toolchain installers or meta-test frameworks.
- Source code must be idiomatic, cohesive, appropriately reviewed, secure and maintainable. Tests must verify actual behavior, including tenant isolation, OAuth, Telnyx/webhook/audio contracts, barge-in, recording, recovery, Docker deploy and real responsive UI. No fake CI success or missing end-to-end coverage.
- Never run live Telnyx phone calls or pay for AI during automated tests. Use independent Telnyx protocol fixtures and deterministic audio and independent Realtime/GPT-Live emulators; disclose limits of simulation.
- Never expose or log secrets, other users' data, recordings, production credentials or personal data. Do not change global host configuration or unrelated projects.
- Recordings OFF for newly created users; configurable per user; respect all locked privacy controls. No country-specific announcements or legal decision automation.
- Maintain release quality: published image, source tag, release notes, README with authentic UI screenshots, backup/restore proof and clean-install documentation, green main. Release qualification requires genuine artifact verification, protected PRs and green CI.
- Locked contracts and their SHA manifest may be changed when an explicit owner-authorized issue changes the corresponding requirement. Update them together in the reviewed PR; never autonomously weaken requirements or release policy. No separate release specification or additional approval is required for such an authorized change.
