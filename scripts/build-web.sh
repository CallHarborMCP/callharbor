#!/usr/bin/env bash
# Build the React app and sync its output into the Go embed directory (web/static).
set -Eeuo pipefail
cd "$(dirname "$0")/../web"

npm ci
npm run build

find static -mindepth 1 ! -name .gitkeep -delete
cp -R dist/. static/
test -f static/index.html || { echo 'web/static/index.html missing after sync' >&2; exit 1; }
printf '[callharbor] web assets synced to web/static\n'
