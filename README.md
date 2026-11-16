# CallHarbor

**Self-hosted, lightweight, multi-user AI calling platform.**

CallHarbor is an autonomous outbound-calling product with a responsive React web UI, an OAuth-protected MCP server, Telnyx telephony and configurable OpenAI-compatible Realtime voice gateways. It stores call timelines, transcripts and optionally locally recorded audio with configurable retention.

> This repository currently contains product specifications and test scaffolding, not a working application. Complete release requirements are in `.agents/DEFINITION_OF_DONE.md`.

## Development

Developer tool versions are pinned in `.mise.toml`. Install [mise](https://mise.jdx.dev/) once if it is missing (`curl -fsSL https://mise.run | sh`; binary defaults to `~/.local/bin/mise`), then use `mise install` and `mise exec -- bash scripts/verify-local.sh --quick` for quick tests. Use `mise exec -- bash scripts/verify-local.sh` for full production Docker/browser E2E (also used by GitHub CI). In GitHub Actions, the official `jdx/mise-action` supplies the same Go and Node versions. Go modules and npm manage project dependencies; do not install a custom toolchain manager.

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

The V1 release requires complete professional installation, configuration, security, backup/restore, troubleshooting and development documentation with authentic screenshots of the running application. Acceptance criteria live in `.agents/DOCUMENTATION.md` and `.agents/DEFINITION_OF_DONE.md`.

Target delivery: single production Docker image, one persistent volume, no Redis/PostgreSQL/LiveKit/OpenClaw, Docker Compose instructions and GHCR release. External dependency: user-provided Telnyx account and an OpenAI-compatible realtime gateway.

## Required voice architecture

V1 implements two independently selectable voice protocols: OpenAI-compatible Realtime WebSocket and GPT-Live primary WebSocket. Both use the Telnyx outbound media bridge behind transport-independent call/audio boundaries. WebRTC and SIP remain future options; never claim unverified provider/model compatibility.
