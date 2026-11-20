# CallHarbor — engineering guidance

CallHarbor is a self-hosted, multi-user AI telephone-calling product. This repository provides product and engineering requirements for any coding agent, regardless of its runtime or work-management system. It does not define agent assignments, scheduling, task-state transitions, review routing or approval workflows.

## Authoritative contracts

- Product and architectural scope: `.agents/CHARTER.md`, `.agents/PRODUCT_SCOPE.md`, `.agents/ARCHITECTURE.md`, `.agents/PROTOCOL_FEASIBILITY.md`.
- API, security and UX: `.agents/API_CONTRACT.md`, `.agents/SECURITY.md`, `.agents/UX.md`.
- Engineering quality: `.agents/GUARDRAILS.md`, `.agents/CODE_QUALITY.md`, `.agents/TEST_STRATEGY.md`, `.agents/DOCUMENTATION.md`.
- Release acceptance: `.agents/DEFINITION_OF_DONE.md`. Future possibilities: `.agents/ROADMAP.md`; a roadmap item is not authorization to implement it.

Deliver the complete V1.0.0 contract. Do not silently remove mandatory functionality, downgrade requirements or represent unverified behavior as complete. The product, scope, security, guardrails and acceptance contracts protected by `.agents/LOCKED_SHA256SUMS` require explicit owner authorization for changes.

## Development and verification

- Use `mise install` and `mise exec -- bash scripts/verify-local.sh --quick` for fast local checks. The full command without `--quick` runs production Docker/browser end-to-end tests. `.mise.toml` pins Go and Node; Go modules and npm manage application dependencies.
- Preserve the V1 product design: a Go modular monolith with embedded React, SQLite and one production container; Telnyx outbound calling, independently selectable Realtime and GPT-Live WebSocket adapters, per-user isolation, authorization, recording opt-in and auditable outcomes.
- Test actual behavior with genuine Go/React/SQLite, protocol/audio/security and browser tests. Use independent simulated Telnyx and voice-provider endpoints; automated tests must never make paid calls or incur hosted AI charges. Document simulation limits honestly.
- Protect secrets, user data and recordings. Do not invent successful calls, transcripts, screenshots, benchmarks, test results or release evidence.
- Keep Renovate and required GitHub Actions CI working. Changes to source and tests use reviewable pull requests and existing branch protection; do not bypass green checks or independent quality review.
- Validate applicable contracts, compatibility, documentation, artifacts and upgrade/install behavior before declaring technical delivery complete. Work authorization, operational task tracking and acceptance decisions are handled outside this repository.
