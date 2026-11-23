# CallHarbor Governance

Status: the project currently has one maintainer. This document describes how decisions are made now and what changes once a second maintainer is named. It does not claim a multi-maintainer structure that does not yet exist.

## Roles

- **Maintainer**: merges pull requests, manages releases and the repository settings, and handles private security reports. The current maintainer is listed in the repository's `CODEOWNERS` file once one exists. A second maintainer has not yet been named.
- **Contributor**: anyone who opens an issue or pull request. Contributors do not need a special role.

Roles and responsibilities will be recorded here when they change.

## Decision making

- Day-to-day decisions (implementation details, documentation wording, dependency updates) are made in the pull request, by the maintainer after independent review.
- Changes to product scope, security requirements, API or other locked contracts in `.agents/` require explicit owner authorization. The owner is the project maintainer until a different arrangement is recorded here.
- Changes to the release process, licence or governance documents are decided by the maintainer and recorded in a pull request.
- Disagreements are discussed in the issue or pull request. If they cannot be resolved there, the maintainer decides and records the reasoning.

## Review and merge

- Every change reaches `main` through a pull request that passes the required CI check.
- A pull request is reviewed independently of its author before merge where the project has more than one reviewer. While there is one maintainer, the review is recorded in the pull request or the issue, and the maintainer does not merge their own unreviewed change to a locked contract.
- Direct commits to `main` are not used.

## Continuity

The project keeps its repository access and recovery arrangements outside the repository. Documenting them here must not expose credentials. A second maintainer is required before the bus-factor requirement is met.

## Changes to this document

Changes to this file are made by pull request. They take effect when merged to `main`.
