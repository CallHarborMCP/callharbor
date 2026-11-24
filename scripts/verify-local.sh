#!/usr/bin/env bash
# Canonical project verification: full current-build E2E by default, fast --quick opt-in.
set -Eeuo pipefail
cd "$(dirname "$0")/.."

MODE="${1:---full}"
if (( $# > 1 )) || [[ "$MODE" != --quick && "$MODE" != --full ]]; then
  echo "Usage: bash scripts/verify-local.sh [--quick|--full]" >&2
  exit 2
fi

bash -n scripts/verify-release-artifacts.sh
sha256sum -c .agents/LOCKED_SHA256SUMS

gitleaks git --no-banner --exit-code 1 --log-opts=HEAD .
gitleaks dir --no-banner --exit-code 1 .

if [[ -f web/package.json ]]; then
  bash scripts/build-web.sh
fi

if [[ -f go.mod ]]; then
  gofiles="$(find . -type f -name '*.go' -not -path './vendor/*' -not -path './web/node_modules/*' -print0 | xargs -0 -r gofmt -l)"
  [[ -z "$gofiles" ]] || { printf 'Go formatting errors:\n%s\n' "$gofiles" >&2; exit 1; }
  go_pkgs="$(go list ./... | grep -v '/web/node_modules/')"
  go vet $go_pkgs
  golangci-lint run ./...
  gosec -quiet ./...
  govulncheck ./...
  go build $go_pkgs
  COVERAGE_MIN=80
  go test -race -count=1 -covermode=atomic -coverprofile=coverage-go.out $go_pkgs
  go_total="$(go tool cover -func=coverage-go.out | awk '/^total:/ {sub("%", "", $NF); print $NF}')"
  printf 'Go statement coverage: %s%% (minimum %s%%)\n' "$go_total" "$COVERAGE_MIN"
  awk -v got="$go_total" -v min="$COVERAGE_MIN" 'BEGIN { exit !(got + 0 >= min + 0) }' \
    || { echo "Go coverage below ${COVERAGE_MIN}%" >&2; exit 1; }
fi

if [[ -f web/package.json ]]; then
  (cd web && npm ci && npm audit && npx playwright install --with-deps chromium && npm run lint && npm run typecheck && npm run test:coverage && npm run build && npx playwright test tests-e2e/appearance.spec.ts)
  web_total="$(node -e 'const s=require("./web/coverage/coverage-summary.json").total.statements.pct; process.stdout.write(String(s))')"
  printf 'React statement coverage: %s%% (minimum 80%%)\n' "$web_total"
fi

bash -n scripts/update-telnyx-contract.sh

if [[ "$MODE" == --full ]]; then
  # The committed Telnyx contract must be the mechanical extract of the pinned upstream spec.
  bash scripts/update-telnyx-contract.sh --check
  # Full E2E must test CURRENT production code, not a stale application image.
  if [[ -f compose.test.yaml ]]; then
    trap 'docker compose -f compose.test.yaml down --volumes --remove-orphans' EXIT
    docker compose -f compose.test.yaml up --build --abort-on-container-exit --exit-code-from sut
    docker compose -f compose.test.yaml down --volumes --remove-orphans
    trap - EXIT
  fi
  if [[ -f web/playwright.config.ts ]]; then
    (cd web && npx playwright install --with-deps chromium && npx playwright test)
  fi
fi

if [[ ! -f go.mod || ! -f web/package.json ]]; then
  echo '[callharbor] NOTE: application source is incomplete; checks do not prove product functionality'
fi
printf '[callharbor] %s verification completed\n' "$MODE"
