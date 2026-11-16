# Verified engineering lessons

Evidence-backed technical lessons only; not a task queue or a workflow. The exact claim, scheduling and review process is external to this repository.

L001 | Preserve the PRIMARY failure exit status and traceback when cleanup/reporting also fails. | Tendo FLOW failure recovery
L002 | After merge fetch and verify green clean main; a successful CLI exit does not prove deployment. | Tendo FLOW and CallHarbor bootstrap history
L003 | Reproduce bugs with a deterministic failing test first, then prove pass-after; never weaken E2E/CI to obtain green. | Tendo TESTING
L004 | Identify failing tool/dependency root cause rather than reinstalling blindly. | Tendo LEARNINGS L003
L008 | Keep collaborator-specific prompts OUT of public Git; store only privately with 0600 permissions. | CallHarbor bootstrap correction
L010 | Run high-risk test suites serially when fixtures/state/services are shared; isolate fixture IDs and reset per test. | Tendo LEARNINGS L007,L012
L011 | Validate authentic browser screenshots at asserted viewport sizes, not just their filenames, before making claims. | Tendo LEARNINGS L010
L012 | A regression test must fail against the ACTUAL faulty behavior and pass against the prior correct behavior; mock-only or timing-dependent assertions are insufficient. | Tendo LEARNINGS L015
