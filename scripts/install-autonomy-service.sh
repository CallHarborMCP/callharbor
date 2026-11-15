#!/usr/bin/env bash
# OWNER-ONLY future cutover. Migration launcher never invokes this file.
# --check is read-only; --prepare installs DISABLED/INACTIVE only; --rollback restores.
set -Eeuo pipefail
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="${CALLHARBOR_REPO:-CallHarborMCP/callharbor}"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT="$UNIT_DIR/callharbor-autonomous.service"
BACKUP_ROOT="${XDG_STATE_HOME:-$HOME/.local/state}/callharbor-autonomy-install"
usage() { printf 'Usage: bash scripts/install-autonomy-service.sh --check|--prepare|--rollback\n'; }
[[ $# == 1 ]] || { usage >&2; exit 2; }
case "$1" in --check|--prepare|--rollback) MODE="$1";; *) usage >&2; exit 2;; esac
[[ $(id -u) != 0 ]] || { echo 'Do not run as root' >&2; exit 2; }
for c in gh git systemctl python3 awk; do command -v "$c" >/dev/null || { echo "Missing $c" >&2; exit 2; }; done
cd "$ROOT"
[[ "$(git branch --show-current)" == main ]] || { echo 'Checkout must be on main' >&2; exit 2; }
[[ -z "$(git status --porcelain)" ]] || { echo 'Checkout must be clean' >&2; exit 2; }
REMOTE_SHA="$(git ls-remote origin refs/heads/main | awk '{print $1}')"
[[ "$(git rev-parse HEAD)" == "$REMOTE_SHA" ]] || { echo 'Checkout must be synced to origin/main' >&2; exit 2; }
[[ -x "$ROOT/scripts/autonomy-controller.sh" ]] || { echo 'Controller not executable' >&2; exit 2; }
state_active="$(systemctl --user is-active callharbor-autonomous.service 2>/dev/null || true)"
state_enabled="$(systemctl --user is-enabled callharbor-autonomous.service 2>/dev/null || true)"
printf 'Current unit active=%s enabled=%s\n' "${state_active:-unknown}" "${state_enabled:-unknown}"
[[ "$state_active" != active && "$state_active" != activating && "$state_active" != reloading ]] || { echo 'Existing service running; stop only as a separate owner action' >&2; exit 2; }
[[ "$state_enabled" != enabled && "$state_enabled" != linked && "$state_enabled" != alias ]] || { echo 'Existing unit auto-starts; refuse to overwrite' >&2; exit 2; }

# Read full eligible backlog without altering anything. No reliance on a guessed count.
TEMP="$(mktemp)"; trap 'rm -f "$TEMP"' EXIT
gh api --paginate --slurp -X GET "repos/$REPO/issues?state=all&per_page=100" > "$TEMP"
PLAN="$(python3 - "$TEMP" <<'PY'
import json,re,sys
pages=json.load(open(sys.argv[1])); issues=[x for page in pages for x in page]
if not all(isinstance(x,dict) for x in issues) or len(issues)>10000:
    sys.exit('Invalid or excessive issue snapshot')
ids=[]
for x in issues:
    if 'pull_request' in x or x.get('state')!='open': continue
    labels={l['name'] for l in x.get('labels',[])}
    body=x.get('body') or ''
    if 'agent:callharbor' not in labels: continue
    if '<!-- callharbor-managed:v1 -->' not in body or not re.search(r'<!-- callharbor-issue-id:CH-\d+ -->',body):
        sys.exit(f'Unrecognized legacy ownership marker on #{x["number"]}')
    if 'status:in-progress' in labels or 'status:blocked' in labels:
        sys.exit(f'Existing active/blocked claim #{x["number"]}; reconcile before cutover')
    ids.append(x['number'])
print(','.join(str(x) for x in sorted(ids)))
PY
)" || { echo 'Ownership inspection failed' >&2; exit 2; }
printf 'Legacy eligible Issues: %s\n' "${PLAN:-none}"

if [[ "$MODE" == --check ]]; then
  printf 'READ-ONLY check finished. --prepare requires an independent explicit owner action.\n'
  printf 'No service installation, label change or activation performed.\n'
  exit 0
fi

if [[ "$MODE" == --rollback ]]; then
  [[ -f "$BACKUP_ROOT/last/manifest" ]] || { echo 'No prepared migration backup' >&2; exit 2; }
  # Expected migration file written by this installer; data is trusted local owner state.
  [[ "$(cat "$BACKUP_ROOT/last/manifest")" == "$REPO" ]] || { echo 'Backup repository mismatch' >&2; exit 2; }
  [[ -f "$BACKUP_ROOT/last/new-unit.sha256" ]] || exit 2
  [[ -f "$UNIT" ]] || { echo 'Missing unit: refusing unsafe rollback' >&2; exit 2; }
  sha256sum -c "$BACKUP_ROOT/last/new-unit.sha256" --status || { echo 'Unit was modified; refusing rollback' >&2; exit 2; }
  if [[ -f "$BACKUP_ROOT/last/old-unit" ]]; then
    cp "$BACKUP_ROOT/last/old-unit" "$UNIT"
  else
    rm "$UNIT"
  fi
  systemctl --user daemon-reload
  if [[ -s "$BACKUP_ROOT/last/newly-labelled" ]]; then
    while IFS= read -r n; do gh issue edit "$n" -R "$REPO" --remove-label release; done < "$BACKUP_ROOT/last/newly-labelled"
  fi
  printf 'Rolled back owner-prepared files and added release labels. Nothing was activated.\n'
  exit 0
fi

# --prepare: explicit owner action in the future. Never start/enable.
[[ "$MODE" == --prepare ]]
[[ ! -e "$BACKUP_ROOT/last" ]] || { echo 'Prior preparation found; inspect/rollback first' >&2; exit 2; }
[[ "$ROOT" != *' '* && "$ROOT" != *$'\n'* ]] || { echo 'Unsupported path whitespace in unit' >&2; exit 2; }
mkdir -p "$UNIT_DIR" "$BACKUP_ROOT"
chmod 700 "$BACKUP_ROOT"
backup="$BACKUP_ROOT/$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -m 700 "$backup"
if [[ -f "$UNIT" ]]; then cp "$UNIT" "$backup/old-unit"; fi
printf '%s\n' "$REPO" > "$backup/manifest"
: > "$backup/newly-labelled"
cat > "$UNIT" <<EOF
[Unit]
Description=CallHarbor evergreen autonomous engineering (owner activated)
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
WorkingDirectory=$ROOT
Environment=CALLHARBOR_REPO=$REPO
Environment=IDLE_POLL_SECONDS=300
ExecStart=/bin/bash $ROOT/scripts/autonomy-controller.sh
Restart=on-failure
RestartSec=60
KillMode=control-group
TimeoutStopSec=90
[Install]
WantedBy=default.target
EOF
sha256sum "$UNIT" | sed "s|$UNIT|$UNIT|" > "$backup/new-unit.sha256"
# Prepare only: don't run a service, don't change enablement.
systemctl --user daemon-reload
# Save the rollback pointer before external mutations; an interrupted run can still be rolled back.
ln -s "$backup" "$BACKUP_ROOT/last"
# Label changes are intentionally the final step and preserve old labels for legacy readers.
if ! gh label list -R "$REPO" --json name --jq '.[].name' | grep -qx 'release'; then
  gh label create release -R "$REPO" --color '0969da' --description 'Evergreen agent-owned engineering work'
fi
IFS=',' read -r -a issue_ids <<< "$PLAN"
for n in "${issue_ids[@]}"; do
  [[ -n "$n" ]] || continue
  present="$(gh issue view "$n" -R "$REPO" --json labels --jq '[.labels[].name] | index("release") != null')"
  if [[ "$present" != true ]]; then
    gh issue edit "$n" -R "$REPO" --add-label release
    printf '%s\n' "$n" >> "$backup/newly-labelled"
  fi
done
printf 'Prepared (NOT started/enabled): %s\n' "$UNIT"
printf 'Future explicit owner activation ONLY after validation:\n'
printf '  systemctl --user enable --now callharbor-autonomous.service\n'
