# CallHarbor permanent autonomous engineering — owner operations

The implementation and governance PR **must not start or enable any service**, change live Issue labels, or modify the host's current systemd units.

## Current status

The controller becomes usable **only after the owner explicitly prepares and activates it**. Until that point it is inert repository code. Product work remains subject to the full V1 locked contract (Realtime WS **and** GPT-Live WS).

## Manual cutover (future owner action, never CI or migration generator)

1. Ensure no legacy process or independent contributor is currently working on the affected Issues. Check existing claims, unfinished PRs, CI, and host processes. Arrange a coordinated transition if another worker uses legacy ownership rules.
2. Update the canonical checkout to a clean, green `main`. Run the full local tests and verify required main CI.
3. Run `bash scripts/install-autonomy-service.sh --check`. This only inspects state and prints eligible legacy Issues; it does not change labels or unit files.
4. **Only when consciously ready:** run `bash scripts/install-autonomy-service.sh --prepare`. This creates a user unit without enabling or starting it and adds `release` to verifiably managed legacy open Issues while preserving historical metadata.
5. Inspect the migrated Issues, unit file, status labels, runner adapter and actual work-selection result. Ensure no incompatible legacy worker is active.
6. **Separate activation:** `systemctl --user enable --now callharbor-autonomous.service`. This command is intentionally not used by the generator, tests, PR or installer.
7. Check `systemctl --user status callharbor-autonomous.service` and `journalctl --user -u callharbor-autonomous.service --since today`.
8. For a prepared but inactive migration, `bash scripts/install-autonomy-service.sh --rollback` restores the backed-up unit and removes only new ownership labels recorded in the backup. Rollback refuses changed unit contents. If the service was explicitly activated later, stop/disable it by deliberate owner action before rollback.

## Runtime adapter contract

The controller launches `AUTONOMY_RUNNER ABSOLUTE_PROMPT_FILE SESSION_ID`. Replace the executable to switch an agent runtime. The **role-based governance never changes**. The default optional adapter is `scripts/agent-runner-opencode.sh`; no particular agent product is an authority or required workflow dependency. Any configured adapter must run synchronously, return a meaningful exit code and must not fork detached writers.

## Support and limits

- GitHub is authoritative for Issues, claims, PRs, CI and releases. No local feature-work queue is permitted.
- Full session output is kept separately from concise journald events. Restrict access and retention; never log secrets or user records.
- Required main CI and Dependabot security queries require working GitHub credentials and permissions. Unknown state prevents selection rather than becoming idle.
- A successful governance migration does **not** imply any provider API compatibility or V1 product feature has been implemented. The locked GitHub Issues govern those features.
