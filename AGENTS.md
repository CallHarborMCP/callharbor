# CallHarbor — engineering guidance

CallHarbor is a self-hosted, multi-user AI telephone-calling product. These repository instructions apply to any coding agent, independently of its runtime or work-management system.

## Authoritative contracts

- Product and architecture: `.agents/CHARTER.md`, `.agents/PRODUCT_SCOPE.md`, `.agents/ARCHITECTURE.md`, `.agents/PROTOCOL_FEASIBILITY.md`.
- API, security and UX: `.agents/API_CONTRACT.md`, `.agents/SECURITY.md`, `.agents/UX.md`.
- Engineering quality: `.agents/GUARDRAILS.md`, `.agents/CODE_QUALITY.md`, `.agents/TEST_STRATEGY.md`, `.agents/DOCUMENTATION.md`, `.agents/DEFINITION_OF_DONE.md`.

These documents describe the product baseline and enduring engineering requirements, not a release backlog or proof that a capability is already implemented. The authorized GitHub issue defines the work and its acceptance criteria. Specific release scope and dependencies belong exclusively in GitHub issues. Do not create or maintain a separate release specification file. Do not implement unrelated backlog items merely because they appear in a product contract.

Preserve existing behavior and mandatory product requirements. An explicit owner-authorized issue changing a product or engineering contract is sufficient authorization to update the affected documents and their lock hashes in the same reviewed PR. Otherwise, do not modify locked requirements, silently reduce scope or represent unverified behavior as complete. The lock manifest protects requirements; it is not a separate release approval mechanism.

## Development and verification

- Use `mise install` and `mise exec -- bash scripts/verify-local.sh --quick` for fast local checks. The full command without `--quick` runs production Docker/browser end-to-end tests. `.mise.toml` pins Go and Node; Go modules and npm manage application dependencies.
- Preserve the architecture, API, UX and security contracts. Prefer reuse of existing implementation and tests over parallel implementations.
- Test actual behavior with genuine Go/React/SQLite, protocol/audio/security and browser tests. Use independent simulated telephony and voice-provider endpoints; automated tests must never make paid calls or incur hosted AI charges.
- Protect secrets, user data and recordings. Do not invent successful calls, transcripts, screenshots, benchmarks, test results or release evidence.
- Keep Renovate and required GitHub Actions CI working. Changes use reviewable pull requests and existing branch protection; do not bypass green checks or independent quality review.
- Validate applicable contracts, compatibility, documentation, artifacts and upgrade/install behavior before declaring delivery complete. Work scheduling, assignments, task-state transitions and acceptance decisions are handled outside this repository.
