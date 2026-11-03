# CallHarbor — autonomous engineering

Full production V1.0.0, NOT MVP. OpenCode and Paperclip collaborate on separate hosts using **GitHub Issues directly**. The dedicated milestone is `v1.0.0`, label `agent:callharbor`, issue marker `<!-- callharbor-managed:v1 -->`. GitHub is the only task/status source; never create an extra coordinator or STATE.md.

## Read each cycle
` .autonomous/CYCLE_PROMPT.md`, `.autonomous/V1_ISSUES_WORKFLOW.md`, `.autonomous/PRODUCT_SCOPE.md`, `.autonomous/ARCHITECTURE.md`, `.autonomous/DEFINITION_OF_DONE.md`, `.autonomous/TEST_STRATEGY.md`, `.autonomous/SECURITY.md`, `.autonomous/UX.md`, `.autonomous/GUARDRAILS.md`, `.autonomous/CODE_QUALITY.md`; other documents on demand. Also inspect git, PRs, relevant issues and CI. Respect locked specifications.

## Process
Pick a dependency-ready open managed issue, claim with plain `gh issue comment` and `gh issue edit`, recheck first claim, then implement one focused branch and PR. No Python issue helpers, leases, heartbeats or own task manager. Finish your own existing PR before another issue. Treat Renovate PRs as normal reviewed dependency updates, not as new managed issues. Don't touch other agent's work. Tests and docs accompany code; independent review, green CI, protected main merge and issue closure are mandatory. Investigate blockers independently, report evidence and continue other tasks.

## Product invariants
Go + SQLite, embedded React/TypeScript UI (responsive and professionally designed, user-configurable appearance), admin/user RBAC, built-in OAuth2.1 for MCP clients plus optional external OIDC, Telnyx outbound, configurable OpenAI-compatible realtime model/gateway, responsive duplex speech with barge-in, multiuser isolation, call audit, recordings with opt-in and in-app playback. One production Docker container. No paid AI fallback, no production Telnyx calls in CI, no made-up live compatibility claim.

## Development environment
`.mise.toml` pins Go and Node for local and CI use. If `mise` is missing, install the official CLI from https://mise.run into `~/.local/bin/mise` (do not alter global tools or shell profiles), then verify it. Run `mise install` and `mise exec -- bash scripts/verify-local.sh`. The official `jdx/mise-action` supplies the same tools to GitHub Actions. Use Go modules and npm lockfiles for source dependencies. Never write a custom toolchain/installation framework.

## Test and release
Use standard `go test`, `go vet`, Go race, `npm`/Vitest, Playwright, Compose-based isolated integration tests and GitHub Actions; no self-made CI frameworks or tests-of-tests. Test product behavior, not merely mocks. Release only when all locked DoD items are proven, required CI green, GHCR images/docs/screenshots/backup restore validated and owner release merged via protected PR. Paperclip cannot independently publish release. Runner stops after verified release and closed managed issues.
