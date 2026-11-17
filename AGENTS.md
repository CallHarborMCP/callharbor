# CallHarbor — engineering guidance

CallHarbor is a self-hosted multi-user AI telephone-calling product. Deliver the **complete** V1.0.0 contract; do not silently downgrade requirements or claim unfinished behavior as implemented. This repository contains product and engineering requirements, **not** an agent orchestrator.

## Read the project contracts

- Scope and architecture: `.agents/CHARTER.md`, `.agents/PRODUCT_SCOPE.md`, `.agents/ARCHITECTURE.md`, `.agents/PROTOCOL_FEASIBILITY.md`.
- API, security and UX: `.agents/API_CONTRACT.md`, `.agents/SECURITY.md`, `.agents/UX.md`.
- Engineering quality: `.agents/GUARDRAILS.md`, `.agents/CODE_QUALITY.md`, `.agents/TEST_STRATEGY.md`, `.agents/DOCUMENTATION.md`.
- Release acceptance: `.agents/DEFINITION_OF_DONE.md`. Future ideas: `.agents/ROADMAP.md`.
- GitHub Issues contain individual acceptance criteria.

## Working in this repository

- Use `mise install` and `mise exec -- bash scripts/verify-local.sh --quick` for fast local checks; the default (no flag) runs full production Docker/browser E2E. `.mise.toml` pins Go and Node.js. Go modules and npm manage application dependencies.
- Keep `renovate.json` and the required GitHub Actions CI active. Do not weaken or bypass tests, protected branches, independent review or release gates.
- Use genuine Go/React/SQLite, protocol, audio, security and browser tests. Test actual production behavior with independent simulated Telnyx and voice-provider endpoints; automated tests must never place paid calls or incur paid AI charges.
- The required V1 product uses Telnyx, both separately selectable Realtime WebSocket and GPT-Live WebSocket adapters, one Go service with embedded React, and SQLite. Preserve isolation, auth, recording opt-in, truthful transcription and auditability.
- Protect credentials and users' data. Never invent successful calls, simulated-live compatibility, benchmarks, screenshots or completed release evidence.
- The product mission, V1 scope, security, guardrails and release acceptance are owner-controlled and protected by `.agents/LOCKED_SHA256SUMS`. Other technical documents may evolve with implementation without weakening those requirements.

## Maintenance releases (after the initial V1.0.0 release)

- Once the first complete V1.0.0 release is published, apply this policy to **every subsequent version**. Normally publish a maintenance patch **no earlier than 14 days after the previous published release**, and only when meaningful merged application/dependency changes are ready. No empty or documentation-only maintenance releases.
- **Early security exception only:** a confirmed critical vulnerability affecting CallHarbor, or a demonstrably severe actively exploited vulnerability with material product impact, may require an expedited security hotfix before the 14-day window. An ordinary Renovate alert or dependency update is not by itself an exception.
- Do not run competing maintenance and feature releases. Coordinate urgent security hotfixes safely with any ongoing feature release. Preserve mandatory independent review, required green CI, professional release notes, protected PR/merge and verification of published artifacts.

Assignment, scheduling, issue state transitions, agent identities, handoffs and task orchestration are deliberately **not defined in this repository**.
