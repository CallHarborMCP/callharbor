# Permanent autonomous engineering workflow

`controller=single_long_lived_service`; `session=one_fresh_orchestrator_per_item`; `ownership=GitHub_Issue_label_release`; `main=protected`; `service_activation=owner_only`.

## Roles and boundaries

The **owner** authorizes governance changes, cutover, initial service activation, sensitive credentials and publication authority. The **controller** selects work and launches an execution **adapter**; the **orchestrator** verifies live state; a **worker** owns one reviewable Issue/PR; an independent **reviewer** scrutinizes changes. Roles are execution-tool-neutral; the selected CLI/agent implementation lives only in a replaceable runner adapter. Each concurrent worker uses its own checkout and preferably its own GitHub account. Never touch another active claim or PR.

## Permanent controller

- One long-lived `callharbor-autonomous.service` invokes `scripts/autonomy-controller.sh`, using `scripts/autonomy-work-selection.py` and `.autonomous/AUTONOMY_PROMPT.md`.
- Each work item runs through `AUTONOMY_RUNNER` with `(absolute_prompt_file, session_id)`; runner implementations are replaceable without governance change. The default adapter is only an installation convenience, not an authority or policy dependency.
- Single exclusive lock for controller lifetime; never start overlapping writing cycles, never query GitHub while a session is alive. On failure or incomplete state, back off and fail closed.
- Idle polling is at least 300 seconds. Session status is revalidated against GitHub. Historical V1 and later releases/completion markers never terminate or restart the controller.
- The controller and migration installer are **protected governance**: ordinary engineering sessions cannot change them. Only explicit future owner-authorized governance work can.
- Installation and ownership-label cutover are a *separate, explicit owner-only action*. Running CI, cloning, merging a PR, or generating governance files never starts/enables the service.

## Work ownership and claims

- An Issue is owned **only** with exact label `release`. Milestones, titles, body markers and version numbers do not grant ownership.
- The **only required Issue label is `release`**. `status:*`, `priority:*` and area labels are optional metadata, not requirements for selection or claiming. Do not modify unrelated Issues.
- Ready means an open `release` Issue with all referenced `## Dependencies` / `Blocked by` Issues closed, no earlier active claim and no explicitly blocked/in-progress legacy status. Verify GitHub history and acceptance criteria.
- Before claiming, check Issue body, labels, comments/claims, dependencies, all open PRs and current main CI. Post `CLAIM: <role>, GitHub @<account>`, reread and honor the first valid claim. **Do not require or add status labels.** Do not claim if another worker owns it.
- One branch/PR for one focused unit. Use `Refs #N` in PR body. Run real local gates, independent review and green required checks. Merge only through protected main. Verify main HEAD and close owned Issue with evidence. Do not bypass guards or fake reviews.
- New engineering Issues may be created inside approved product scope with label `release`, explicit acceptance tests and dependencies; version milestones remain optional reporting metadata, not ownership.

## Deterministic scheduler

Priority: (1) red required main CI; (2) actionable security fixes; (3) own unfinished PR/claim/interrupted session; (4) untriaged Renovate PR older than 48 h; (5) actionable dependency PR after two merged owned feature PRs; (6) highest-priority ready owned Issue; (7) remaining actionable dependency PR; (8) eligible tested patch-release; (9) idle. Revalidate before acting; incomplete pagination, API errors and unknown CI states must not return idle. No local fairness counter: reconstruct from merge history. Reconsider unchanged blocked dependency PRs after approximately 7 days. Runtime-meaningful changes and green qualification are required for patch releases (default 14-day window), not merely elapsed time.

## Independent engineering and blockers

- Investigate actual failures, evaluate compatible alternatives, choose an implementation, add regression tests, verify and review without routine owner escalation.
- If Issues form a genuine dependency cycle: analyze graph and real constraints, split independent work or correct stale dependency edges within owned Issues and document evidence. Never deadlock permanently on a purely technical choice.
- Document blockers with reproducible evidence in the owned Issue; `status:blocked` may be used as optional metadata but is never required. Reevaluate boundedly, continue other independent work, never retry an unchanged failure forever.
- Escalate only changing human-locked product/security contracts, missing real external authorization/credentials, financial actions or unsafe destructive requests. State the exact needed action; never invent approval.

## Dependencies, CI, security and releases

- Renovate and human dependency PRs require upstream release-note inspection, compatibility changes when needed, behavioral tests, independent review and green CI; never blind-merge, suppress tests or repeat CI retries.
- Red main CI and actionable security take precedence; security fixes can trigger prompt qualified patch publication. Routine releases are eligible around 14 days only when runtime changes and release gates justify them.
- Major/minor releases are developed through owned Issues, including a coordinator Issue, with complete product/security/upgrade/backup/GUI/artefact verification. GitHub Releases are the primary public release-note record. No new per-release governance prompts/workflow files/markers are required; old completion markers remain historical evidence only.
- V1 scope is human-locked: Go/SQLite/React, Telnyx, both `realtime_ws` and `live_ws` protocol adapters, separate provider/media boundaries, MCP/OAuth, genuine functional tests and verified multiarch GHCR release. The concrete V1 Issues and locked product/DoD documents govern all acceptance criteria.

## Maintenance and logging

- One systemd service handles development, dependency, security, CI and release maintenance; no parallel maintenance timer.
- Journald contains concise cycle summaries, not full tool JSON; per-session diagnostics live in restricted bounded/rotated files. Never log secrets, user recordings or personal data.
- When no safe work exists, finish the current session and let the permanent controller wait for the next idle poll.
