# OpenSSF readiness report — CallHarbor

Status: pre-code readiness assessment. This is not a certification, badge claim or compliance statement. No OpenSSF Best Practices badge, OSPS Baseline attestation or Scorecard score has been obtained or registered.

## Criteria versions and assessment date

| Item | Version / source | Recorded |
| --- | --- | --- |
| OpenSSF Best Practices criteria | https://www.bestpractices.dev/en/criteria (Passing, Silver, Gold text; the page shows no version identifier) | Read 2026-11-24 |
| OSPS Baseline | Current version v2026.08.28 (https://baseline.openssf.org/) | Read 2026-11-24 |
| Repository state | `main` at `dc5f71b`, plus the DEV-35 documentation branch | 2026-11-24 |

Scope: Best Practices Passing and Silver MUST and SHOULD/SUGGESTED items, and OSPS Baseline Level 1 (24 controls). Gold and OSPS Level 2 and 3 are out of scope for this report.

## Status legend

- **Met**: verified in the repository or in public GitHub state as of the assessment date.
- **Met (pending merge)**: satisfied by this change, not yet on `main`.
- **Partial**: some evidence exists; a gap remains.
- **Planned**: not yet satisfied; the planned work is named.
- **Owner**: requires a GitHub organisation or repository administrator action, or a human attestation. Agents cannot verify or perform it.
- **N/A**: not applicable now, with the reason given. Re-check when application code exists.

## Summary

- Met now: public HTTPS git repository, public history with author attribution, a pinned toolchain (`.mise.toml`), Renovate configuration, a CI workflow that runs on push and pull request, no secret patterns found by a manual scan of tracked files (no secret scanner runs in CI yet), and no tracked binaries.
- Partial after merge: a public vulnerability reporting process (`.github/SECURITY.md`). Its private channel is disabled on the repository (API `enabled: false`, verified 2026-11-24), so it is not Met until the owner enables it.
- Blocking gaps before any badge claim: no `LICENSE` file on `main`, no `CONTRIBUTING`, code of conduct or governance documents, no product tests, and no release.
- Owner actions: enable and verify private vulnerability reporting, the 2FA and collaborator-permission settings, branch protection, and the human attestations on secure design and access continuity.

## Passing (MUST items)

| ID | Requirement (abridged) | Status | Evidence / gap | Owner | Planned work |
| --- | --- | --- | --- | --- | --- |
| description_good | Site describes what the software does | Partial | `README.md` describes the product and states that the repository is not yet a working application | | Keep README accurate at each release |
| interact | Site explains how to obtain, report feedback and contribute | Planned | No contribution section in README | | Add public contribution guide (`CONTRIBUTING.md` or README section) |
| contribution | Contribution process explained | Planned | No contribution documentation | | As above |
| floss_license | Software released as FLOSS | Planned | No `LICENSE` file on `main`. DEV-36 records owner approval of Apache-2.0, but the file has not landed | Owner confirms licence | Add Apache-2.0 `LICENSE` once the owner confirms it |
| license_location | License in standard location in repository | Planned | As above | | As above |
| documentation_basics | Basic user/developer documentation | Partial | `.agents/*.md` are internal contracts. Public docs are required by `.agents/DOCUMENTATION.md` | | Public `docs/` for the V1 release |
| documentation_interface | Reference docs for external interface | Planned | `.agents/API_CONTRACT.md` is internal | | Public API reference at V1 |
| sites_https | Project sites support HTTPS | Met | `https://github.com/CallHarborMCP/callharbor` responds over HTTPS | | |
| discussion | Searchable discussion mechanism usable without proprietary client software | Met | GitHub Issues enabled: `has_issues: true` on the repository API (verified 2026-11-24) | | |
| english | Documentation and bug reports in English | Met | Public docs are English per `.agents/DOCUMENTATION.md` | | |
| maintained | Project is maintained | Met | Active commits through 2026-11-23 | | |
| repo_public | Public readable version-controlled repository | Met | Anonymous HTTPS access returns 200 | | |
| repo_track | Repository tracks what changed, who and when | Met | Git history with author and timestamp | | |
| repo_interim | Interim versions available for review between releases | Met | Feature branches and history on `origin` | | |
| repo_distributed | Distributed VCS | Met | git | | |
| version_unique | Unique version identifier per release | Planned | No release yet | | First release v1.0.0 (DEV-31, DEV-63) |
| version_semver | SemVer or CalVer (SHOULD) | Planned | Intended: SemVer, `v1.0.0` | | As above |
| version_tags | Release identified by git tag (SHOULD) | Planned | No tags yet | | As above |
| release_notes | Human-readable release notes | Planned | `.agents/DOCUMENTATION.md` requires `docs/releases/v1.0.0.md` | | DEV-63 |
| release_notes_vulns | Release notes list CVE-assigned run-time vulnerabilities fixed | Planned | No vulnerabilities fixed yet; N/A only if none exist at release | | DEV-63 |
| report_process | Process for submitting bug reports | Planned | No bug-report instructions outside `SECURITY.md` | | Add bug-report guidance and issue templates |
| report_tracker | Issue tracker used (SHOULD) | Met | GitHub Issues enabled (`has_issues: true`, verified 2026-11-24) | | |
| report_responses | Acknowledge majority of bug reports from the last 2–12 months | N/A (none yet) | No public bug reports known. Re-check at application time | | Track response times once reports exist |
| enhancement_responses | Respond to majority of enhancement requests (SHOULD) | N/A (none yet) | As above | | As above |
| report_archive | Public archive for reports and responses | Met | GitHub Issues enabled and publicly readable (`has_issues: true`, verified 2026-11-24) | | |
| vulnerability_report_process | Publish vulnerability reporting process on the project site | Partial (pending merge) | `.github/SECURITY.md` documents the process. The reporting channel it names is not yet enabled (see next row) | | Re-check once the channel works |
| vulnerability_report_private | Private reporting channel described and reachable | Partial, Owner | Verified 2026-11-24: `gh api repos/CallHarborMCP/callharbor/private-vulnerability-reporting` returns `{"enabled":false}`. The advisory form does not accept reports while disabled. Not Met | Owner enables private vulnerability reporting (Settings → Code security, or `PUT /repos/CallHarborMCP/callharbor/private-vulnerability-reporting`), then the PR records the `{"enabled":true}` result | Re-verify API state before any Met claim |
| vulnerability_report_response | Initial response within 14 days for reports from the last 6 months | Partial (pending merge) | `SECURITY.md` states 14 days. The commitment is documented, but no reporting channel is enabled yet. No reports exist | | Met once the channel works |
| build | Working build system that rebuilds from source | N/A (now) | No application code. Required once Go/React code lands (`go build`, `npm run build`) | | Required in DEV-2 and the first frontend PR |
| build_common_tools | Common build tools (SHOULD) | N/A (now) | As above | | As above |
| build_floss_tools | Buildable with FLOSS tools (SHOULD) | Met for the planned stack | Go, Node and npm are FLOSS | | |
| test | Automated test suite publicly released as FLOSS | Partial | `scripts/verify-local.sh` runs the checks and currently reports that application source is incomplete. No product tests exist yet | | DEV-2 and later feature PRs add tests |
| test_invocation | Standard, documented test invocation (SHOULD) | Met | `README.md`: `mise exec -- bash scripts/verify-local.sh` | | |
| test_most | Tests cover most branches and functionality (SUGGESTED) | Planned | No product code | | DEV-33 |
| test_continuous_integration | CI runs tests on change (SHOULD) | Met | `.github/workflows/required.yml` runs on push and pull request to `main` | | |
| test_policy | Written policy: tests added with major new functionality | Partial | `.agents/TEST_STRATEGY.md` and `GUARDRAILS.md` are internal | | Public policy in `CONTRIBUTING` (DEV-33) |
| tests_are_added | Evidence that the policy was followed in recent major changes | N/A (now) | No major functional changes yet | | Evidence from the first feature PR |
| warnings | Compiler warnings or linter enabled | Partial | `verify-local.sh` runs `gofmt` and `go vet` when `go.mod` exists. Lint and typecheck run for `web/` when it exists | | DEV-33 |
| warnings_fixed | Warnings addressed | N/A (now) | No code | | |
| know_secure_design | At least one primary developer knows secure design | Owner | Requires a human attestation. An agent claim is not evidence | Owner attests | |
| know_common_errors | Primary developer knows common vulnerability classes and mitigations | Owner (partial evidence) | `.agents/SECURITY.md` lists SSRF, CSRF, webhook signature/replay, stream hijack, and at-rest key protection | Owner attests | |
| crypto_published | Only publicly reviewed crypto protocols and algorithms | Planned | `.agents/SECURITY.md` mandates Argon2id, Ed25519 webhook verification and mature libraries. No code yet | | DEV-2 and feature PRs |
| crypto_call | Use specialised libraries, do not re-implement | Planned | `.agents/SECURITY.md` requires mature libraries and no custom crypto | | As above |
| crypto_floss | Crypto functionality implementable with FLOSS | Planned | Planned stack is FLOSS | | |
| crypto_keylength | Default keylengths meet NIST minimums through 2030 | Planned | No code | | |
| crypto_working | No broken algorithms by default | Planned | No code | | |
| crypto_weaknesses (SHOULD) | No known-weak algorithms by default | Planned | No code | | |
| crypto_pfs (SHOULD) | Forward secrecy for key agreement | Planned | TLS is provided by the user's reverse proxy. Confirm in documentation | | DEV-2 and deployment docs |
| crypto_password_storage | Passwords stored with iterated salted KDF | Planned | `.agents/SECURITY.md` requires Argon2id. No code | | Auth feature PRs |
| crypto_random | Keys and nonces from a CSPRNG | Planned | No code | | |
| delivery_mitm | Delivery counters MITM (HTTPS or ssh) | Met | Git over HTTPS; GitHub Actions and checkout over HTTPS | | |
| delivery_unsigned | Hashes not used from HTTP without signature | Planned | No releases yet | | Signed checksums and manifest (DEV-31, DEV-63) |
| vulnerabilities_fixed_60_days | No medium+ publicly known unpatched vulnerabilities over 60 days | Met (none known) | No product release; no known vulnerabilities in the repository | | Re-check per release |
| vulnerabilities_critical_fixed (SHOULD) | Fix critical vulnerabilities rapidly | N/A (none) | No reports | | |
| no_leaked_credentials | No valid private credentials in the public repository | Met for the tracked tree | Pattern scan of tracked files found no credentials. `.gitignore` excludes `.env*`, `*.pem`, `*.key`, `*.db` and `recordings/`. History was not scanned with a dedicated tool | | Add a secret scanner to CI (DEV-33) |
| static_analysis | Static analysis tool applied before release | Planned | `verify-local.sh` runs `go vet` only | | Add `golangci-lint`/`gosec` and ESLint security rules (DEV-33) |
| static_analysis_common_vulnerabilities (SUGGESTED) | Rules for common vulnerabilities | Planned | As above | | As above |
| static_analysis_fixed | Confirmed medium+ findings fixed | N/A (none yet) | No analysis run | | Applies once analysis runs |
| dynamic_analysis | Dynamic analysis before release (SUGGESTED) | Planned | Playwright and Docker E2E are planned | | DEV-33 |
| dynamic_analysis_unsafe | Dynamic tooling for memory-unsafe languages | N/A | Go and TypeScript are memory-safe. Go `-race` is used | | |
| dynamic_analysis_fixed | Confirmed medium+ dynamic findings fixed | N/A (none yet) | No analysis run | | |

## Silver (MUST items)

| ID | Requirement (abridged) | Status | Evidence / gap | Owner | Planned work |
| --- | --- | --- | --- | --- | --- |
| achieve_passing | Passing badge first | Blocked | Depends on the Passing items above | | |
| contribution_requirements | Contribution page includes acceptable-contribution requirements | Planned | No contribution page | | Public contribution guide |
| dco (SHOULD) | Developer Certificate of Origin or equivalent | Planned | No sign-off policy | | Adopt DCO in `CONTRIBUTING` |
| governance | Documented governance and decision model | Planned | Not public. The `.agents` and Paperclip contract is internal | Owner confirms model | `GOVERNANCE.md` |
| code_of_conduct | Code of conduct in standard location | Planned | None | Owner confirms text | `CODE_OF_CONDUCT.md` |
| roles_responsibilities | Public key roles and responsibilities | Planned | None public | Owner names roles | Public roles section in governance docs |
| access_continuity | Project continues if one person is unavailable within a week | Owner | No documented key, account or recovery process | Owner sets up recovery access | Document the continuity plan without publishing secrets |
| bus_factor (SHOULD) | Bus factor of 2 or more | Owner | One maintainer evident in commit history | Owner adds a second maintainer | |
| documentation_roadmap | Documented roadmap for the next year | Partial | `.agents/ROADMAP.md` is internal and states that future items are not authorisations | | Public roadmap |
| documentation_architecture | Architecture documentation | Partial | `.agents/ARCHITECTURE.md` exists and is internal | | Public architecture doc for V1 |
| documentation_security | Security requirements the user can expect | Partial | `.agents/SECURITY.md` is the internal contract | | Public security requirements doc (`docs/security/`) |
| documentation_quick_start | Quick start guide | Planned | No runnable application. The release requires Docker Compose instructions | | DEV-31, DEV-63 |
| documentation_current | Documentation is kept consistent with the current version | Met | README states the current state accurately | | Maintain at each change |
| documentation_achievements | Link badges and achievements within 48 hours | Planned | No badge yet | | DEV-36 |
| accessibility_best_practices (SHOULD) | Accessibility best practices | Planned | `.agents/UX.md` contains requirements | | UI feature PRs |
| internationalization (SHOULD) | i18n support | N/A (justified for the project sites) | Public docs are English. Call speech language is a product setting, not an i18n requirement of the project | | |
| sites_password_security | Project sites store external user passwords as iterated hashes | N/A | GitHub hosts the project sites; the project does not run a public site with passwords | | |
| maintenance_or_update | Maintain older versions or document upgrades | N/A (none yet) | No prior versions | | Upgrade documentation from v1.x |
| report_tracker (Silver) | Issue tracker used for individual issues | Met | GitHub Issues enabled (`has_issues: true`, verified 2026-11-24) | | |
| vulnerability_report_credit | Credit reporters of vulnerabilities resolved in the last 12 months | N/A (none yet) | No resolved reports | | `SECURITY.md` states credit policy |
| vulnerability_response_process | Documented process for responding to reports | Partial (pending merge) | `.github/SECURITY.md` documents the process. Its reporting channel is not enabled | | Met once the channel works |
| coding_standards | Identify and require a style guide for each language | Planned | `.agents/CODE_QUALITY.md` is internal | | Public `CONTRIBUTING` names the style guides (Go, TypeScript) |
| coding_standards_enforced | Automatically enforced style | Partial | `gofmt` is enforced by `verify-local.sh`. ESLint and Prettier are planned | | DEV-33 |
| build_standard_variables, build_preserve_debug, build_non_recursive | Build conventions for native binaries | N/A | No native build | | Recheck if a native build is introduced |
| build_repeatable | Reproducible builds (Silver MUST) | Planned | Requires pinned toolchains and lockfiles. `.mise.toml` pins Go and Node. Reproducible-build verification not yet designed | | DEV-31 and DEV-33 |
| installation_common | Standard install and uninstall method | Planned | Docker Compose is the release install method | | DEV-31, DEV-63 |
| installation_standard_variables | Honour standard install location variables | N/A | Container-based deployment, not a system installer | | |
| installation_development_quick | Quick developer environment setup | Met | `mise install`, then `mise exec -- bash scripts/verify-local.sh --quick` in `README.md` | | |
| external_dependencies | Dependencies listed in a machine-readable way | Partial | `.mise.toml` lists tools. `go.mod`, `package-lock.json` and Dockerfiles do not exist yet | | Lockfiles with the first code PRs |
| dependency_monitoring | Monitor and check dependencies for known vulnerabilities | Partial | `renovate.json` sets `minimumReleaseAge` to 3 days generally, and 0 days only for `vulnerabilityAlerts`. GitHub Dependabot alerts and a scanner in CI are not verified | Owner confirms Dependabot alerts | Add `govulncheck` and `npm audit` to CI (DEV-33) |
| updateable_reused_components | Easy update of reused components | Met | Renovate covers Go modules, npm, `.mise.toml`, Docker and GitHub Actions per `README.md` | Owner grants Renovate app access | |
| automated_integration_testing | Automated tests on each check-in for at least one branch | Met | `required.yml` runs on push and pull request to `main` | | |
| regression_tests_added50 | Regression tests for at least 50% of bugs fixed in the last 6 months | N/A (no bugs fixed yet) | No product code | | Applies from first bug fix |
| test_statement_coverage80 | At least 80% statement coverage | Planned | No coverage measured yet | | DEV-33: `go test -coverprofile` and Vitest coverage |
| test_policy_mandated | Formal policy requiring tests for major functionality | Partial | Internal `TEST_STRATEGY.md` only | | Public policy (DEV-33) |
| tests_documented_added | Change-proposal instructions include the test policy | Planned | No contribution instructions | | `CONTRIBUTING` |
| warnings_strict | Maximally strict warnings | Planned | Partial lint, see Passing | | DEV-33 |
| implement_secure_design | Secure design principles implemented | Planned | `.agents/GUARDRAILS.md` and `SECURITY.md` define requirements. No code | | Feature PRs |
| crypto_weaknesses (Silver, interface) | Default mechanisms avoid weak algorithms | Planned | See Passing | | |
| crypto_algorithm_agility (SHOULD) | Support multiple algorithms | Planned | No code | | |
| crypto_credential_agility | Credentials and keys stored apart from configuration and code | Planned | `.agents/SECURITY.md` requires a secret key on a persistent file or environment variable with rotation guidance | | Secret-handling docs and code |
| crypto_used_network (SHOULD) | Secure protocols for network communication | Planned | Gateway endpoints are configured as HTTPS/WSS by the spec | | |
| crypto_tls12 (SHOULD) | Support at least TLS 1.2 | Planned | Delegated to the user's reverse proxy | | Deployment docs |
| crypto_certificate_verification | Verify TLS certificates by default | Planned | No code | | Feature PRs |
| crypto_verification_private | Verify certificates before sending private headers or cookies | Planned | No code | | Feature PRs |
| signed_releases | Releases cryptographically signed, with public key verification docs | Planned | No release | | DEV-31, DEV-63 |
| version_tags_signed (SHOULD) | Important tags signed | Planned | No tags | | DEV-63 |
| input_validation | Allowlist validation of untrusted inputs | Planned | `.agents/API_CONTRACT.md` and `SECURITY.md` require it. No code | | Feature PRs with negative tests |
| hardening (SHOULD) | Hardening mechanisms | Planned | Docker nonroot and least privilege are required by `SECURITY.md` | | DEV-2 |
| assurance_case | Threat model, trust boundaries, secure-design and weakness arguments | Partial | `.agents/SECURITY.md` and `GUARDRAILS.md` are requirements. No public threat model or assurance case | | Threat model (`docs/security/`) |
| static_analysis_common_vulnerabilities | Static analysis with common-vulnerability rules | Planned | See Passing | | DEV-33 |
| static_analysis_fixed | Fix confirmed findings | N/A (none yet) | See Passing | | |
| dynamic_analysis (Silver) | Dynamic analysis on each major production release | Planned | See Passing | | DEV-33 |
| dynamic_analysis_unsafe | Not applicable to memory-safe code | N/A | | | |

## OSPS Baseline Level 1 (OSPS Baseline v2026.08.28)

| ID | Control (abridged) | Status | Evidence / gap | Owner | Planned work |
| --- | --- | --- | --- | --- | --- |
| osps_ac_01_01 | MFA for sensitive resources in the authoritative repository | Owner | Requires GitHub organisation 2FA. Not verifiable from the agent identity | Owner enforces 2FA for all maintainers | |
| osps_ac_02_01 | New collaborators need manual permission or lowest privilege by default | Owner | Requires GitHub organisation setting | Owner | |
| osps_ac_03_01 | Direct commits to the primary branch are blocked | Owner | Requires branch protection or ruleset on `main`. The required workflow exists, but enforcement is not verified | Owner configures and verifies `main` protection | |
| osps_ac_03_02 | Deleting the primary branch requires explicit confirmation | Owner | Requires branch protection or ruleset | Owner | |
| osps_br_01_01 | Untrusted CI metadata is validated before use | Met | `required.yml` uses no `github.event` inputs in `run` steps. It uses `contents: read` and the default token | | Re-check on each new workflow |
| osps_br_01_03 | Untrusted code snapshots cannot reach privileged credentials | Met (partial) | `pull_request` triggers do not expose repository secrets to fork PRs. `contents: read` is set. Owner verifies fork-PR workflow approval | Owner confirms GitHub fork-PR setting | |
| osps_br_03_01 | Official project URIs use encrypted channels | Met | Repository and documentation URLs use HTTPS | | |
| osps_br_03_02 | Official distribution channels are authenticated | Planned | No release channel yet. GHCR images need signing and digest verification | | DEV-31, DEV-63 |
| osps_br_07_01 | Prevent unencrypted secrets in version control | Met (partial) | Tracked-file scan found none. `.gitignore` excludes secret file patterns. Pre-commit or CI secret scanning not in place | | Add secret scanning to CI (DEV-33) |
| osps_do_01_01 | Documentation includes user guides for all basic functionality | Planned | Required for release | | DEV-31, DEV-63 |
| osps_do_02_01 | Documentation includes a guide for reporting defects | Planned | Only vulnerability reporting exists | | Bug-report guide |
| osps_gv_02_01 | Mechanisms for public discussion of changes and usage obstacles | Met | GitHub Issues enabled (`has_issues: true`, verified 2026-11-24) and pull requests in use | | |
| osps_gv_03_01 | Documentation explains the contribution process | Planned | No contribution documentation | | `CONTRIBUTING` |
| osps_le_02_01 | Source licence meets OSI or FSF definition | Planned | No licence file on `main` | Owner confirms Apache-2.0 | Add `LICENSE` |
| osps_le_02_02 | Released software assets licence meets OSI or FSF | Planned | No release | | DEV-63 |
| osps_le_03_01 | Source licence in `LICENSE`, `COPYING` or `LICENSES/` | Planned | Missing | | As above |
| osps_le_03_02 | Released assets include licence | Planned | No release | | DEV-63 |
| osps_qa_01_01 | Repository is publicly readable at a static URL | Met | `https://github.com/CallHarborMCP/callharbor` | | |
| osps_qa_01_02 | Publicly readable change record with author and time | Met | git history | | |
| osps_qa_02_01 | Dependency list for direct language dependencies | Planned | No `go.mod` or `package.json` yet. Renovate covers the planned files | | First code PRs |
| osps_qa_04_01 | Multi-repository projects document their codebases | N/A | Single repository | | |
| osps_qa_05_01 | No generated executable artifacts in version control | Met | No binaries tracked | | Keep build outputs in `.gitignore` |
| osps_qa_05_02 | No unreviewable binary artifacts in version control | Met | No binaries tracked. Screenshots will be reviewable images, see `.agents/DOCUMENTATION.md` | | |
| osps_vm_02_01 | Project documentation contains security contacts | Partial (pending merge) | `.github/SECURITY.md` names a reporting route that is not yet enabled (API `enabled: false`). Not Met until the route works | Owner enables private vulnerability reporting | Re-verify after enablement |

OSPS Level 2 and 3 controls were not assessed in this report.

## Items that must not be claimed yet

- No OpenSSF Best Practices badge (Passing, Silver or Gold) has been registered or awarded.
- No OSPS Baseline attestation has been registered.
- No OpenSSF Scorecard result has been produced or checked against the project.
- No release, signed artifact, SBOM or threat model exists.
- No CallHarbor application, test suite or coverage figure exists.

## Owner actions required

1. Confirm the Apache-2.0 licence and the `LICENSE` file.
2. Enable GitHub private vulnerability reporting for `CallHarborMCP/callharbor` (currently `enabled: false`). Until then the rows above stay Partial.
3. Enforce organisation 2FA, the collaborator permission default, and branch protection or a ruleset on `main`, including blocking direct commits and branch deletion.
4. Confirm Dependabot alerts are enabled (GitHub Issues is verified enabled).
5. Name the key roles, the code of conduct and governance model, and a second maintainer (bus factor).
6. Attest the secure-design knowledge of at least one primary developer.
7. Document the access continuity arrangement and keep its secrets out of the repository.
8. Confirm the 14-day initial response and 90-day disclosure defaults in `.github/SECURITY.md`.

## Planned work

- Public `CONTRIBUTING`, `CODE_OF_CONDUCT`, `GOVERNANCE` and `LICENSE` files (before the first feature PR; DEV-33 and DEV-35 follow-up).
- Go and frontend coverage artifacts, secret scanning, `govulncheck`, `npm audit`, `gosec` and ESLint security rules in CI (DEV-33).
- Badges and the Scorecard workflow (DEV-36).
- Signed releases, checksums, SBOM, release notes and the public threat model (DEV-31, DEV-63).
