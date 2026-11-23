# Contributing to CallHarbor

Thank you for considering a contribution. CallHarbor is pre-release: the repository currently holds product specifications and verification scaffolding, not a working application.

## Reporting problems

- Security vulnerabilities: do not open a public issue. Follow [`.github/SECURITY.md`](.github/SECURITY.md).
- Bugs, documentation problems and enhancement requests: open a GitHub issue at https://github.com/CallHarborMCP/callharbor/issues. Describe what you expected, what happened, and how to reproduce it. Use synthetic data only; never include real credentials, call audio, transcripts or personal data.

## Proposing changes

1. Open an issue first for anything larger than a documentation fix, so the change can be checked against the product contract.
2. Work on a branch and open a pull request against `main`.
3. Keep each pull request focused on one change and explain why it is needed.
4. Pull requests must pass the required CI check (`required`) and receive independent review before merge. Direct commits to `main` are not accepted.

The canonical product, architecture, security and API contracts are in `.agents/`. Changes that alter product scope, security requirements or other locked contracts need explicit owner authorization and cannot be merged on the strength of a pull request alone.

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

## Code style

- Go code must be formatted with `gofmt` and pass `go vet` and `go test -race`.
- TypeScript and React code must pass the lint and type checks configured in the repository once they exist. The style guides and enforced tooling are described in `.agents/CODE_QUALITY.md`.

## Sign-off (Developer Certificate of Origin)

By contributing, you certify that you wrote the change or have the right to submit it under the Apache License 2.0, in line with the [Developer Certificate of Origin 1.1](https://developercertificate.org/). Sign each commit with `git commit -s`, which adds a `Signed-off-by:` line.

## Code of conduct

All participants must follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Governance

Decisions about the project are made as described in [GOVERNANCE.md](GOVERNANCE.md).

## License

CallHarbor is licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
