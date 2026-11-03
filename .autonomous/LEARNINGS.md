# Durable engineering learnings — shared across every OpenCode/Paperclip cycle

format=ID | actionable verified rule | evidence (managed issue, PR or test)
status=mutable_via_protected_PR
not_a_progress_store=true

## Automatic cross-cycle learning protocol (Tendo V3 model)
1. At the start of EVERY cycle, read this file and apply the relevant rules BEFORE planning, implementation, delegation and review. Reconcile GitHub Issues/PRs first; this file NEVER tracks task statuses or replaces GitHub Issues.
2. After a confirmed bug, CI flake, security finding, architectural mistake, tool failure or review correction, reproduce it and record the cause, exact evidence and prevention in the currently owned, managed GitHub Issue.
3. If the finding is reusable across future cycles, propose a minimal `LNNN | rule | evidence` entry in the same protected PR as the regression fix, or a separate managed issue/PR when cross-cutting. Evidence must identify a merged PR, meaningful fail-before/pass-after test, reproducible command, or vendor specification. No guesses.
4. Promote repeated mistakes to an automated regression test, CI gate, or bounded FLOW/tooling improvement WHERE POSSIBLE; a written admonition alone is insufficient when machine enforcement exists.
5. Obtain independent skeptical review of the lesson, its generality and the regression before merging. Merge only after required CI passes and review findings are addressed.
6. In subsequent cycles re-use lessons, verify that the affected automated gate still executes, consolidate superseded lessons and remove obsolete entries only through reviewed PR with evidence.
7. Do not edit locked scope/DoD/guardrails to 'learn'; human authorization is required for those. Do not install memory daemons, vector stores, duplicate state files, or external paid services. Shared learning occurs through Git history, this document, tests and GitHub evidence, NOT model retraining.
8. Cross-agent lessons propagate through merged protected main. Never write this file concurrently in the same worktree or edit another developer's active branch. If it conflicts, resolve via a separate managed PR.

## Proven baseline rules
L001 | Preserve the PRIMARY failure exit status and traceback when cleanup/reporting also fails. | Tendo FLOW failure recovery
L002 | After merge fetch and verify green clean main; a successful CLI exit does not prove deployment. | Tendo FLOW and CallHarbor bootstrap history
L003 | Reproduce bugs with a deterministic failing test first, then prove pass-after; never weaken E2E/CI to obtain green. | Tendo TESTING
L004 | Identify failing tool/dependency root cause rather than reinstalling blindly. | Tendo LEARNINGS L003
L005 | If another writer owns the primary worktree, use separate branch/worktree and pin every subagent workdir. | Tendo LEARNINGS L004,L006
L006 | Orchestrator triages multidisciplinary review findings into bounded packets; do not send the entire report to a single writer. | Tendo LEARNINGS L005
L007 | Use native gh commands and explicit milestone/label filters; do not build custom GitHub pagination wrappers. | CallHarbor V10 workflow simplification
L008 | Keep collaborator-specific prompts OUT of public Git; store only privately with 0600 permissions. | CallHarbor bootstrap correction
L009 | Never auto-adopt external/untagged GitHub Issues; milestone and ownership are immutable authority boundaries. | Tendo V3_ISSUES_WORKFLOW
L010 | Run high-risk test suites serially when fixtures/state/services are shared; isolate fixture IDs and reset per test. | Tendo LEARNINGS L007,L012
L011 | Validate authentic browser screenshots at asserted viewport sizes, not just their filenames, before making claims. | Tendo LEARNINGS L010
L012 | A regression test must fail against the ACTUAL faulty behavior and pass against the prior correct behavior; mock-only or timing-dependent assertions are insufficient. | Tendo LEARNINGS L015
