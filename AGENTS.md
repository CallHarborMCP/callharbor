# CallHarbor — autonomous engineering

## Scope

Build the complete production V1.0.0, **not an MVP**. GitHub Issues and pull requests are the authoritative work source; permanent autonomy governance is release-neutral. The current V1 contract explicitly requires independently selectable **OpenAI-compatible Realtime WebSocket** (`realtime_ws`) **and GPT-Live WebSocket** (`live_ws`) voice protocols, behind transport-independent call/media boundaries. WebSocket is the V1 transport; WebRTC and SIP are not implemented V1 features. Never drop either voice protocol or weaken the locked product/security/release contract.

## Roles, not vendor-specific agents

`owner` authorizes governance and releases; `controller` schedules; `orchestrator` selects and coordinates; `worker` implements a single claimed item; `reviewer` independently verifies. Multiple authorized workers can use different execution tools on independent checkouts and GitHub identities. Tool-specific CLI integration belongs **only** to a replaceable runtime adapter, not policy or workflow documents.

## Required reading

Always: this file, `.autonomous/AUTONOMY_PROMPT.md`, `.autonomous/FLOW.md`, `.autonomous/GUARDRAILS.md`, `.autonomous/CODE_QUALITY.md`, `.autonomous/DEFINITION_OF_DONE.md` and `.autonomous/LEARNINGS.md`.

For product changes also read: `.autonomous/PRODUCT_SCOPE.md`, `.autonomous/ARCHITECTURE.md`, `.autonomous/PROTOCOL_FEASIBILITY.md`, `.autonomous/SECURITY.md`, `.autonomous/API_CONTRACT.md`, `.autonomous/TEST_STRATEGY.md`, `.autonomous/UX.md`; consult documentation and roadmap as relevant. Review live GitHub Issues/PRs, CI, security and dependency state every cycle.

## Engineering workflow

Only Issues carrying exact `release` label are agent-owned **after the owner-controlled label cutover**. Status labels and first-claim-wins protect concurrently working contributors. No Issue title, milestone, version or historical body marker grants ownership by itself. Protected `main` is PR-only. Fix defects with behavioral regression tests, run all required verification, obtain independent review, merge only green required CI, then verify main and close the Issue with evidence. Renovate/security/CI/patch maintenance is permanent and does not require a separate work scheduler. Do not touch an unrelated or another worker's claimed Issue/PR.

## Runtime and governance safety

Only the owner may modify the autonomy controller, selector, runtime adapter, installer, systemd unit, governance authority, locked specs or credentials. Autonomous product-feature workers **must not modify their own authority**. The owner controls installation and service activation separately. No routine action may activate a disabled service. No version completion marker stops the evergreen controller.

## Product/test invariants

Go + SQLite, embedded responsive React/TypeScript WebGUI, local recordings OFF by default, multiuser RBAC and data isolation, OAuth2.1/MCP and optional OIDC, Telnyx outbound telephony, both selectable realtime voice protocol adapters, full-duplex natural speech/barge-in, actual played-audio semantics, auditable outcomes, one production Docker container. Standard `mise`, Go/npm, Playwright and Docker Compose tests; no paid API calls or real Telnyx dialing in automated tests. Do not represent independent simulator tests as proven live provider compatibility. Preserve all locked V1 Definition of Done, genuine screenshots, backups, source tag, and verified GHCR multiarch release evidence.
