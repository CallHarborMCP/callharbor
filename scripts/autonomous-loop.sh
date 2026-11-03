#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
OPENCODE_BIN="${OPENCODE_BIN:-$HOME/.opencode/bin/opencode}"
[[ -x "$OPENCODE_BIN" ]] || OPENCODE_BIN="$(command -v opencode || true)"
[[ -x "$OPENCODE_BIN" ]] || { echo '[callharbor] opencode not found' >&2; exit 2; }
for cmd in gh git flock timeout; do command -v "$cmd" >/dev/null || { echo "missing $cmd" >&2; exit 2; }; done
exec 9>"${XDG_RUNTIME_DIR:-/tmp}/callharbor-autonomous-${UID}.lock"
flock -n 9 || { echo '[callharbor] another runner active'; exit 0; }
repo="${CALLHARBOR_FULL_REPO:-CallHarborMCP/callharbor}"
completed() {
  [[ "$(git branch --show-current)" == main ]] || return 1
  git diff --quiet && git diff --cached --quiet || return 1
  git cat-file -e HEAD:.autonomous/V1_COMPLETE 2>/dev/null || return 1
  gh release view v1.0.0 -R "$repo" >/dev/null 2>&1 || return 1
  local open
  open="$(gh issue list -R "$repo" --milestone v1.0.0 --label agent:callharbor --state open --limit 1000 --json number --jq 'length' 2>/dev/null)" || return 1
  [[ "$open" == 0 ]]
}
cycle=0
backoff=120
while true; do
  if completed; then echo '[callharbor] V1 released and verified'; exit 0; fi
  [[ -s .autonomous/CYCLE_PROMPT.md ]] || exit 2
  cycle=$((cycle+1))
  rc=0
  timeout --signal=TERM --kill-after=60s "${CYCLE_TIMEOUT:-4h}" "$OPENCODE_BIN" run --auto --agent orchestrator --title "CallHarbor cycle $cycle" "$(cat .autonomous/CYCLE_PROMPT.md)" || rc=$?
  if completed; then echo '[callharbor] V1 released and verified'; exit 0; fi
  if (( rc == 0 )); then backoff=120; sleep 60; else
    echo "[callharbor] OpenCode exit $rc; next cycle will reconcile GitHub" >&2
    sleep "$backoff"
    (( backoff = backoff < 900 ? backoff * 2 : 900 ))
  fi
done
