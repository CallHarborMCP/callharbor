# Security Policy

CallHarbor is pre-release. The repository currently contains specifications and verification scaffolding, not a working application, so there are no supported released versions yet.

## Reporting a vulnerability

Do not report security vulnerabilities in public issues, pull requests or discussions.

Use GitHub private vulnerability reporting for this repository:

https://github.com/CallHarborMCP/callharbor/security/advisories/new

Include the affected commit or version, a description of the impact, reproduction steps or a proof of concept, and any suggested mitigation. Do not include real credentials, call audio, transcripts or personal data; describe synthetic reproduction data instead.

## Supported versions

| Version | Supported |
| --- | --- |
| `main` (pre-release, unreleased) | Vulnerability reports accepted; no fixed-version guarantee |
| First released version (v1.0.0 and later) | Will be listed here at publication |

Once a release is published, its security support scope and end date will be documented here and in the release notes.

## Response process

- Initial response: acknowledgement and an initial triage assessment within 14 days of receipt, and normally sooner.
- Triage: confirm or reject the report, assign severity, and tell the reporter whether it is in scope.
- Remediation: fix confirmed vulnerabilities in a private branch or advisory, with regression tests where practicable. Critical issues are prioritised for the fastest safe release.
- Disclosure: coordinated disclosure. Publish a GitHub security advisory and the fixed release together, with credit to the reporter unless they ask to remain anonymous. The default target is 90 days from triage to public disclosure, shortened or extended by agreement with the reporter when the fix or exploitation status requires it.

Vulnerabilities that are publicly known and fixed will be recorded in release notes, including a CVE identifier when one is assigned.

## Scope

In scope: CallHarbor source code, released container images, and the documented deployment configuration. Issues in third-party dependencies should be reported to their maintainers; tell us if a dependency affects CallHarbor's security and we will track its update.
