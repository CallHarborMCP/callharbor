# CallHarbor — Definition of Done

This is an enduring quality and acceptance standard, not a feature list for a specific release. Authorized GitHub issues define the deliverables and acceptance criteria for each change. Product and engineering contracts define the baseline that must be preserved.

## Every change

- Meet the issue's functional acceptance criteria with actual implementation and evidence. Reuse existing code and tests, preserve unrelated behavior, and avoid unrequested scope.
- Review applicable product, architecture, API, security and UX contracts. Verify tenant isolation, authorization, input validation, persistence, failure behavior and compatibility where relevant. No silently weakened requirement or unsupported success claim.
- Use pinned project-local tooling and standard verification commands. Add meaningful behavior and regression tests in the introducing PR; exercise invalid, unauthorized, concurrent and fault cases where appropriate. No hidden skips, fake green checks, or mocks of the behavior under test.
- Independently review code for correctness, cohesion, maintainability, architecture, security and high-risk hotspots. Resolve blocking findings. Protected PR and required CI must pass for code changes; non-code changes require suitable verification.
- Update affected product/API/operator/user documentation and authentic screenshots where behavior changes. Keep contracts and lock manifest consistent when an owner-authorized issue explicitly changes them.
- Record evidence of completion and disclose known limitations. Unmet mandatory requirements are blockers, not discretionary scope reductions.

## Integrated product quality

- Maintain working account and OAuth/MCP flows, responsive and accessible UI, secure profiles, correct autonomous call lifecycle, real outcomes, transcripts and local opt-in recording according to the product, API and security contracts.
- Verify supported telephony/voice adapter combinations through independent protocol and full production-container E2E tests, including audio, barge-in, long context, recording, transcript provenance, failure, concurrency and recovery. Keep adapter boundaries independent and prevent hidden paid fallback. Test safe profile migration, measured latency/usage, and negative capability cases.
- Maintain unit, race, integration, contract, browser/visual, security, performance and recovery coverage in required CI. Test actual Go/React/SQLite and production artifacts; no paid calls or hosted AI charges in automated tests. Distinguish simulator evidence from live provider or ChatGPT UI verification.
- Maintain the threat model, license/dependency provenance, accessibility and design evidence, documentation, backup/restore and upgrade/rollback tests. Fix release-blocking defects instead of merely documenting them.

## Release qualification

- Verify the release issue and its authorized scope against the exact candidate, all required dependencies, product contracts and independently reviewed evidence. Do not claim incomplete scope as delivered.
- Green required CI on the exact candidate; verify the production build, full supported-protocol E2E matrix, security and applicable performance evidence, genuine responsive screenshots, README and user/admin/operator/developer documentation, clean install, upgrade, rollback and recovery. Independently review release notes and documentation.
- Prepare and verify the immutable semantic-version source tag, public GitHub Release, signed/checksummed GHCR multi-platform Linux/amd64 and Linux/arm64 image, matching version/latest manifest at publication, downloadable Compose/setup assets and actual source/digest correspondence. Verify published artifacts in a clean environment; do not mark a release complete before verification.
- The release issue is the sole release specification. Generated release notes and evidence document what shipped; they are not additional manually maintained planning documents. Scheduling, approval and publication authorization are external to these instructions.
