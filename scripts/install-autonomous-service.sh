#!/usr/bin/env bash
# MANUAL OPT-IN ONLY: never invoked by bootstrap.
set -Eeuo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
[[ $(id -u) != 0 ]] || { echo 'Run as normal user, not root' >&2; exit 1; }
for x in systemctl flock timeout gh git python3; do command -v "$x" >/dev/null || { echo "Missing $x" >&2; exit 1; }; done
OPENCODE_BIN="${CALLHARBOR_OPENCODE_BIN:-${OPENCODE_BIN:-$HOME/.opencode/bin/opencode}}"
if [[ ! -x "$OPENCODE_BIN" ]]; then OPENCODE_BIN="$(command -v opencode || true)"; fi
[[ -x "$OPENCODE_BIN" ]] || { echo 'OpenCode CLI is missing' >&2; exit 1; }
cd "$ROOT"
REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
[[ -n "$REPO" ]] || { echo 'Cannot establish GitHub repo identity' >&2; exit 1; }
unit_dir="$HOME/.config/systemd/user"
mkdir -p "$unit_dir"
unit_file="$unit_dir/callharbor-autonomous.service"
[[ ! -e "$unit_file" ]] || { echo "Service already exists: $unit_file (not overwriting)" >&2; exit 1; }
cat >"$unit_file" <<UNIT
[Unit]
Description=CallHarbor V1 autonomous OpenCode development
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$ROOT
Environment=OPENCODE_BIN=$OPENCODE_BIN
Environment=CALLHARBOR_FULL_REPO=$REPO
ExecStart=/bin/bash $ROOT/scripts/autonomous-loop.sh
Restart=on-failure
RestartSec=45
KillMode=control-group
TimeoutStopSec=75

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
printf 'Service installed, NOT ENABLED or STARTED: %s\n' "$unit_file"
printf 'When ready, run: systemctl --user enable --now callharbor-autonomous.service\n'
