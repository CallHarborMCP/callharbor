# CallHarbor — engineering guidance

CallHarbor is a self-hosted multi-user AI telephone-calling product. Deliver the **complete** V1.0.0 contract; do not silently downgrade requirements or claim unfinished behavior as implemented. This repository contains product and engineering requirements, **not** an agent orchestrator.

## Read the project contracts

- Scope and architecture: `.agents/CHARTER.md`, `.agents/PRODUCT_SCOPE.md`, `.agents/ARCHITECTURE.md`, `.agents/PROTOCOL_FEASIBILITY.md`.
- API, security and UX: `.agents/API_CONTRACT.md`, `.agents/SECURITY.md`, `.agents/UX.md`.
- Engineering quality: `.agents/GUARDRAILS.md`, `.agents/CODE_QUALITY.md`, `.agents/TEST_STRATEGY.md`, `.agents/DOCUMENTATION.md`.
- Release acceptance: `.agents/DEFINITION_OF_DONE.md`. Future ideas: `.agents/ROADMAP.md`.
- **Paperclip is the sole authoritative backlog and acceptance tracker** for CallHarbor. Every authorized V1 work item, acceptance criterion, blocker, dependency, priority, review, decision and evidence link lives in the CallHarbor project in Paperclip. Use only native Paperclip task identifiers.

## Working in this repository

- Use `mise install` and `mise exec -- bash scripts/verify-local.sh --quick` for fast local checks; the default (no flag) runs full production Docker/browser E2E. `.mise.toml` pins Go and Node.js. Go modules and npm manage application dependencies.
- Keep `renovate.json` and the required GitHub Actions CI active. Do not weaken or bypass tests, protected branches, independent review or release gates.
- Use genuine Go/React/SQLite, protocol, audio, security and browser tests. Test actual production behavior with independent simulated Telnyx and voice-provider endpoints; automated tests must never place paid calls or incur paid AI charges.
- The required V1 product uses Telnyx, both separately selectable Realtime WebSocket and GPT-Live WebSocket adapters, one Go service with embedded React, and SQLite. Preserve isolation, auth, recording opt-in, truthful transcription and auditability.
- Protect credentials and users' data. Never invent successful calls, simulated-live compatibility, benchmarks, screenshots or completed release evidence.
- The product mission, V1 scope, security, guardrails and release acceptance are owner-controlled and protected by `.agents/LOCKED_SHA256SUMS`. Other technical documents may evolve with implementation without weakening those requirements.

## Backlog authority and implementation evidence

- Obtain authorized assignments from Paperclip tasks only. Avoid duplicate work: verify Paperclip ownership and existing PR claims before starting.
- GitHub remains the source of truth **for Git code, PRs, CI, releases and Renovate PRs only**. A PR must cite its canonical native Paperclip task identifier; review and merge evidence is recorded back on that task. Actionable Renovate PRs get one linked Paperclip task.
- Release qualification requires all V1 CallHarbor Paperclip tasks completed with real linked evidence and verified artifacts.
- Assignment, scheduling, agent identities, status transitions and orchestration remain native to Paperclip, not defined as a second workflow in this repository.

