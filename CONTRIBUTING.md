# Contributing to CallHarbor

Thank you for considering a contribution to CallHarbor.

## Reporting problems

- Security vulnerabilities: do not open a public issue. Follow [`.github/SECURITY.md`](.github/SECURITY.md).
- Bugs, documentation problems and enhancement requests: open a GitHub issue at https://github.com/CallHarborMCP/callharbor/issues. Describe what you expected, what happened, and how to reproduce it. Use synthetic data only; never include real credentials, call audio, transcripts or personal data.

## Proposing changes

1. Open an issue first for anything larger than a documentation fix, so the change can be checked against the product contract.
2. Work on a branch and open a pull request against `main`.
3. Keep each pull request focused on one change and explain why it is needed.
4. Pull requests must pass the required CI check (`required`) and receive independent review before merge. Direct commits to `main` are not accepted.

The canonical product, architecture, security and API contracts are in `.agents/`. GitHub issues define planned changes and release scope. An explicit owner-authorized issue changing a locked contract authorizes the corresponding contract and lock-manifest updates in the reviewed PR; no separate release specification is needed.

## Acceptable contributions

- Changes that follow the contracts in `.agents/` and the guidance in `AGENTS.md`.
- Documentation that is accurate for the current state of the repository. Do not describe planned behaviour as implemented.
- Tests for any new behaviour. Application code must come with tests at the level set in `.agents/TEST_STRATEGY.md`.
- No custom cryptography, no committed secrets, and no tests that make paid calls or incur hosted AI charges.

## Development setup

Tool versions are pinned in `.mise.toml`.

```sh
mise install
mise exec -- bash scripts/verify-local.sh --quick
```

Run the full command without `--quick` only when you need the production Docker and browser end-to-end checks.

`web/` runs TypeScript 7 (the `typescript7` alias) for `typecheck` and `build`. The `typescript` name is aliased to `@typescript/typescript6` only because typescript-eslint needs the TypeScript compiler API, which TypeScript 7 does not ship. Remove this side-by-side setup once typescript-eslint supports the TypeScript 7 API.

## Code style

- Go code must be formatted with `gofmt` and pass `go vet`, `golangci-lint`, `gosec`, `govulncheck` and `go test -race`.
- TypeScript and React code must pass the lint and type checks configured in the repository. The style guides and enforced tooling are described in `.agents/CODE_QUALITY.md`.
- Secrets must never be committed. `gitleaks` scans the full git history and the working tree.

## Testing policy

- New functionality must include automated tests at the level described in `.agents/TEST_STRATEGY.md`.
- Bug fixes must include a regression test that fails before the fix and passes after it.
- Go statement coverage must be at least 80%, measured with `go test -coverprofile` and enforced by `scripts/verify-local.sh` and the `required` check. React statement coverage is enforced at 80% by Vitest V8 in `web/vite.config.ts` and runs in `verify-local.sh`.
- Tests must not be weakened, skipped or removed to make a change pass.
- Exclude code from coverage only when it is generated, test or vendor code. Each exclusion needs a written reason in the pull request.
- `scripts/verify-local.sh` runs the static analysis, secret scanning, vulnerability checks and tests above. The `required` workflow runs the same script on every pull request and blocks merge when it fails.

## Sign-off (Developer Certificate of Origin)

By contributing, you certify that you wrote the change or have the right to submit it under the Apache License 2.0, in line with the [Developer Certificate of Origin 1.1](https://developercertificate.org/). Sign each commit with `git commit -s`, which adds a `Signed-off-by:` line.

## Code of conduct

All participants must follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Governance

Decisions about the project are made as described in [GOVERNANCE.md](GOVERNANCE.md).

## License

CallHarbor is licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
