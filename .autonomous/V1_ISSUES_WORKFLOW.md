# CallHarbor v1 GitHub Issues — straightforward collaboration
status=locked_human_authorized

GitHub Issues are the ONE shared backlog for OpenCode and Paperclip on different hosts. There is no lease system, custom coordinator, stale-claim clock, heartbeat, secondary database, or automatic takeover tool.

**Managed work only:** issue belongs to milestone `v1.0.0`, has exact label `agent:callharbor`, and contains `<!-- callharbor-managed:v1 -->` in its body. Unmanaged/community issues and PRs are outside agent authority. A new task gets all three attributes on creation, not by adopting someone else's issue.

**Pick and claim:** use `gh issue list -R OWNER/callharbor --milestone v1.0.0 --label agent:callharbor --state open`, then inspect issue body, dependencies and status. Choose a free `status:idle` issue, post a brief `CLAIM` comment with your account/agent identity, change to `status:in-progress` using `gh issue edit` and re-read comments. First active CLAIM gets it; if someone else claimed before you, take another issue. No script or arbitrator. One writing worker per GitHub identity/worktree.

**Work:** one issue -> branch -> focused PR -> independent review -> green required checks -> merge -> verify main -> comment evidence -> close issue. Use GitHub CLI directly. Don't merge another person's PR or alter their claim. If interrupted, reconcile your own claim/open PR on resumption. If blocked, record evidence and mark `status:blocked`; work on unrelated issues. Never spin indefinitely on an external blocker.

**Release:** owner OpenCode alone publishes v1.0.0 and opens the completion marker PR after every required issue and DoD test is independently verified. No real phone calls in CI; absence of live Telnyx test must be disclosed. No marker before full release artifact verification.
