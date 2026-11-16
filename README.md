# CallHarbor

**Self-hosted, lightweight, multi-user AI calling platform.**

CallHarbor is an autonomous outbound-calling product with a responsive React web UI, an OAuth-protected MCP server, Telnyx telephony and configurable OpenAI-compatible Realtime voice gateways. It stores call timelines, transcripts and optionally locally recorded audio with configurable retention.

> Repository is initially an **architecture and autonomous-development bootstrap**. No working application is claimed until the full v1.0.0 Definition of Done is independently verified.

## Development

Developer tool versions are pinned in `.mise.toml`. Install [mise](https://mise.jdx.dev/) once if it is missing (`curl -fsSL https://mise.run | sh`; binary defaults to `~/.local/bin/mise`), then use `mise install` and `mise exec -- bash scripts/verify-local.sh`. In GitHub Actions, the official `jdx/mise-action` supplies the same Go and Node versions. Go modules and npm manage project dependencies; do not install a custom toolchain manager.

Dependency updates use [Renovate](https://github.com/apps/renovate). `renovate.json` monitors Go modules, npm, `.mise.toml`, Docker and GitHub Actions without custom managers or scripts. The GitHub organization owner must grant the Renovate GitHub App access to this repository (or choose all repositories). Renovate proposes PRs; developers/agents review and merge them after required CI.

- Canonical product contract: `.agents/PRODUCT_SCOPE.md`
- System design: `.agents/ARCHITECTURE.md`
- Security: `.agents/SECURITY.md`
- API contract: `.agents/API_CONTRACT.md`
- UI/UX contract: `.agents/UX.md`
- Test strategy: `.agents/TEST_STRATEGY.md`
- Release gate: `.agents/DEFINITION_OF_DONE.md`
- Planned future features: `.agents/ROADMAP.md`
- Engineering instructions: `AGENTS.md` (tool-independent).
- Work items and acceptance criteria: GitHub Issues. Agent scheduling, assignment and workflow are external to the repository. Renovate continues to propose dependency PRs.

At v1.0.0, replace this bootstrap README with a complete polished product README: what it does, true captured screenshots of the running release GUI committed under `docs/assets/screenshots/` and embedded inline, feature overview, one-command Docker Compose quickstart, prerequisites, first admin, roles, Telnyx and realtime gateway setup, OAuth MCP/ChatGPT connection, call/recording workflow, security/retention, upgrades, backup/restore, troubleshooting, development/testing, architecture, known limits, licensing, verified release image and supporting documentation links. No generic placeholders, mockups, generated marketing illustrations falsely presented as screenshots, broken images or screenshots captured from a nonfunctional static design.

Target delivery: single production Docker image, one persistent volume, no Redis/PostgreSQL/LiveKit/OpenClaw, Docker Compose instructions and GHCR release. External dependency: user-provided Telnyx account and an OpenAI-compatible realtime gateway.

## Human-approved dual voice protocols (2026-11-15)
V1 will support two independent selectable voice protocol families: OpenAI-compatible Realtime WebSocket and GPT-Live primary WebSocket. These are not drop-in compatible: the latter uses `session.start` on `/v1/live/sessions`. Both will use the original Telnyx outbound media bridge; compatibility with particular gateway/model aliases requires actual validation. This is a product requirement, not a claim of completed code.

## Human-approved transport-independent voice architecture (2026-11-15)
Architecture requirement (not yet implemented): protocol-independent telephony/voice/media boundaries so future WebRTC, SIP and different carriers can be added without rewriting the call engine. V1 concretely ships Telnyx media WebSocket, Realtime primary WebSocket and GPT-Live primary WebSocket; no unimplemented alternative should be presented as usable. Live WebRTC is recommended by OpenAI for direct audio, but a tested Telnyx WebSocket bridge is the selected V1 architecture.
