# CallHarbor — engineering guidance

CallHarbor is a self-hosted multi-user AI telephone-calling product. Deliver the **complete** V1.0.0 contract; do not silently downgrade requirements or claim unfinished behavior as implemented. This repository contains product and engineering requirements, **not** an agent orchestrator.

## Read the project contracts

- Scope and architecture: `.agents/CHARTER.md`, `.agents/PRODUCT_SCOPE.md`, `.agents/ARCHITECTURE.md`, `.agents/PROTOCOL_FEASIBILITY.md`.
- API, security and UX: `.agents/API_CONTRACT.md`, `.agents/SECURITY.md`, `.agents/UX.md`.
- Engineering quality: `.agents/GUARDRAILS.md`, `.agents/CODE_QUALITY.md`, `.agents/TEST_STRATEGY.md`, `.agents/DOCUMENTATION.md`, `.agents/LEARNINGS.md`.
- Release acceptance: `.agents/DEFINITION_OF_DONE.md`. Future ideas: `.agents/ROADMAP.md`.
- GitHub Issues contain individual acceptance criteria. Historical Issue references to `.autonomous/<file>` refer to the corresponding `.agents/<file>`. Old orchestration documents are retired and remain in Git history.

## Working in this repository

- Use `mise install` and `mise exec -- bash scripts/verify-local.sh`; `.mise.toml` pins Go and Node.js. Go modules and npm manage application dependencies.
- Keep `renovate.json` and the required GitHub Actions CI active. Do not weaken or bypass tests, protected branches, independent review or release gates.
- Use genuine Go/React/SQLite, protocol, audio, security and browser tests. Test actual production behavior with independent simulated Telnyx and voice-provider endpoints; automated tests must never place paid calls or incur paid AI charges.
- The required V1 product uses Telnyx, both separately selectable Realtime WebSocket and GPT-Live WebSocket adapters, one Go service with embedded React, and SQLite. Preserve isolation, auth, recording opt-in, truthful transcription and auditability.
- Protect credentials and users' data. Never invent successful calls, simulated-live compatibility, benchmarks, screenshots or completed release evidence.
- The locked product contracts and their SHA manifest are owner-controlled. Do not change their requirements without explicit authorization.

Assignment, scheduling, issue state transitions, agent identities, handoffs and task orchestration are deliberately **not defined in this repository**.
