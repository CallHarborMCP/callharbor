# CallHarbor immutable guardrails
status=locked_human_authorized

- Full V1.0.0, not MVP. Preserve the locked scope, security, UX, architecture and acceptance criteria. One lightweight Go service with embedded React assets + SQLite, deployed as one Docker container.
- NEVER push directly to protected main, force push, skip checks, weaken CI or knowingly merge broken code. Every production change gets tests and an independently reviewed PR.
- Only work on GitHub issues with ALL of milestone `v1.0.0`, label `agent:callharbor`, and body marker `<!-- callharbor-managed:v1 -->`. Ignore unsolicited community issues. Do not take over another contributor's issue or PR. No shadow backlog, coordinator or lock service.
- Use standard GitHub CLI, mise (.mise.toml for pinned Go/Node), Go, Node/npm, Playwright, Docker Compose and GitHub Actions. If mise is absent on the agent's machine, install it once from the official https://mise.run installer to ~/.local/bin/mise, verify the binary, then use `mise install` and `mise exec -- ...` in this checkout. Do not invent installers, manipulate global toolchain versions or silently modify shell profiles. Do not create custom GitHub wrappers, issue leases, token-burning heartbeat processes, toolchain installers, code metric ratchets, gate-coverage scripts or tests that exist only to test our test scripts.
- Source code must be idiomatic, cohesive, appropriately reviewed, secure and maintainable. Tests must verify actual behavior, including tenant isolation, OAuth, Telnyx/webhook/audio contracts, barge-in, recording, recovery, Docker deploy and real responsive UI. No fake CI success or missing end-to-end coverage.
- Never run live Telnyx phone calls or pay for AI during automated tests. Use independent Telnyx protocol fixtures and deterministic audio/Realtime emulator; disclose limits of simulation.
- Never expose or log secrets, other users' data, recordings, production credentials or personal data. Do not change global OpenCode, Paperclip, systemd, host config, Tendo or unrelated projects.
- Recordings OFF for newly created users; configurable per user; respect all locked privacy controls. No country-specific announcements or legal decision automation.
- Keep full V1 release requirement: published image, source tag, release notes, README with authentic UI screenshots, backup/restore proof and clean-install documentation, green main. Completion marker via PR only.
- Locked files and SHA manifest are human-owned and may be changed only by explicit human authorization, never autonomously to weaken release policy.
