#!/usr/bin/env bash
# Run the production Go binary with embedded web assets for browser E2E (Playwright webServer).
set -Eeuo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f web/static/index.html ]]; then
  bash scripts/build-web.sh
fi

bin_dir="$(mktemp -d)"
data_dir="$(mktemp -d)"
trap 'rm -rf "$bin_dir" "$data_dir"' EXIT

CGO_ENABLED=0 go build -o "$bin_dir/callharbor" ./cmd/callharbor
CALLHARBOR_LISTEN_ADDR=127.0.0.1:4173 CALLHARBOR_PUBLIC_URL=http://127.0.0.1:4173 CALLHARBOR_DATA_DIR="$data_dir" "$bin_dir/callharbor"
