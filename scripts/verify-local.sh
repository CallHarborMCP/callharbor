#!/usr/bin/env bash
# Standard developer test command; extend with real product checks as code arrives.
set -Eeuo pipefail
cd "$(dirname "$0")/.."
bash -n scripts/autonomous-loop.sh scripts/install-autonomous-service.sh scripts/verify-release-artifacts.sh
sha256sum -c .autonomous/LOCKED_SHA256SUMS
[[ ! -e .autonomous/STATE.md ]] || { echo 'No STATE.md: GitHub Issues are the backlog' >&2; exit 1; }
if [[ -f go.mod ]]; then
  gofiles="$(find . -type f -name '*.go' -not -path './vendor/*' -print0 | xargs -0 -r gofmt -l)"
  [[ -z "$gofiles" ]] || { printf 'Go formatting errors:
%s
' "$gofiles" >&2; exit 1; }
  go vet ./...
  go test -race ./...
  go build ./...
fi
if [[ -f web/package.json ]]; then
  (cd web && npm ci && npm run lint && npm run typecheck && npm run test -- --run && npm run build)
fi
# The agent MUST extend standard project tests as real modules are implemented.
# When test integration exists, run the actual production-built API/media/OAuth/SQLite/Docker/browser E2E.
if [[ -f compose.test.yaml ]]; then
  trap 'docker compose -f compose.test.yaml down --volumes --remove-orphans' EXIT
  docker compose -f compose.test.yaml up --build --abort-on-container-exit --exit-code-from sut
  docker compose -f compose.test.yaml down --volumes --remove-orphans
  trap - EXIT
fi
if [[ -f web/playwright.config.ts ]]; then
  (cd web && npx playwright test)
fi
if [[ -f .autonomous/V1_COMPLETE ]]; then
  for required in go.mod web/package.json Dockerfile compose.yaml compose.test.yaml web/playwright.config.ts docs/releases/v1.0.0.md; do
    test -f "$required" || { echo "V1 release missing required production/test file: $required" >&2; exit 1; }
  done
  bash scripts/verify-release-artifacts.sh
fi
printf '[callharbor] verify OK
'
