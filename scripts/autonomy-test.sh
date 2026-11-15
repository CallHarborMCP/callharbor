#!/usr/bin/env bash
# Offline only: never touches GitHub, systemd, or a real agent runner.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
for f in "$ROOT/scripts/autonomy-controller.sh" "$ROOT/scripts/agent-runner-opencode.sh" "$ROOT/scripts/install-autonomy-service.sh" "$ROOT/scripts/autonomous-loop.sh" "$ROOT/scripts/install-autonomous-service.sh"; do
  bash -n "$f"
done
python3 "$ROOT/scripts/test-autonomy-work-selection.py"
python3 "$ROOT/scripts/test-autonomy-cutover.py"
python3 "$ROOT/scripts/test-autonomy-controller.py"
printf '[autonomy] offline tests passed\n'
