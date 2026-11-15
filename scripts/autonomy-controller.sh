#!/usr/bin/env bash
# CallHarbor permanent controller. This file is NEVER run by the migration script.
set -Eeuo pipefail
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
: "${IDLE_POLL_SECONDS:=300}"
: "${CYCLE_TIMEOUT_SECONDS:=14400}"
: "${CALLHARBOR_REPO:=CallHarborMCP/callharbor}"
: "${AUTONOMY_RUNNER:=$ROOT/scripts/agent-runner-opencode.sh}"
[[ "$IDLE_POLL_SECONDS" =~ ^[0-9]+$ ]] && (( IDLE_POLL_SECONDS >= 300 )) || { echo '[autonomy] idle polling must be >=300s' >&2; exit 2; }
[[ "$CYCLE_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] && (( CYCLE_TIMEOUT_SECONDS >= 60 )) || exit 2
[[ -x "$AUTONOMY_RUNNER" ]] || { echo '[autonomy] runner is not executable' >&2; exit 2; }
for cmd in flock setsid timeout git python3 gh ps find; do command -v "$cmd" >/dev/null || { echo "[autonomy] missing $cmd" >&2; exit 2; }; done
[[ -n "${XDG_RUNTIME_DIR:-}" && -d "$XDG_RUNTIME_DIR" ]] || { echo '[autonomy] XDG_RUNTIME_DIR is required for reliable exclusive locking' >&2; exit 2; }
exec 9>"$XDG_RUNTIME_DIR/callharbor-autonomous-$UID.lock"
flock -n 9 || exit 75
# Compatible with the old controller's lock name, if different on a future host.
# The legacy controller uses the same lock. No second controller can coexist.
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/callharbor-autonomy"
mkdir -p "$STATE_DIR/sessions"
chmod 700 "$STATE_DIR" "$STATE_DIR/sessions"
PENDING="$STATE_DIR/pending.json"
child=''
stop=0
log() { printf '[autonomy] %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
finish_child() {
  [[ -n "$child" ]] || return 0
  local pgid="$child" attempt
  if kill -0 -- "-$pgid" 2>/dev/null; then
    kill -TERM -- "-$pgid" 2>/dev/null || :
    for attempt in 1 2 3 4 5 6 7 8 9 10; do
      sleep 1
      if ! kill -0 -- "-$pgid" 2>/dev/null; then break; fi
    done
    if kill -0 -- "-$pgid" 2>/dev/null; then
      kill -KILL -- "-$pgid" 2>/dev/null || :
    fi
  fi
  wait "$child" 2>/dev/null || :
  # Fail closed if non-zombie descendants survived. Do not permit next cycle.
  if ps -eo pgid=,stat= | awk -v pgid="$pgid" '$1==pgid && $2 !~ /^Z/ { found=1 } END { exit !found }'; then
    log 'ERROR: living child processes survived termination; refusing another cycle'
    exit 77
  fi
  child=''
}
terminate() { stop=1; log 'stop requested'; finish_child; exit 0; }
trap terminate INT TERM HUP
trap 'finish_child' EXIT

ready_checkout() {
  [[ "$(git branch --show-current)" == main ]] || { log 'checkout is not on main; refusing new cycle'; return 1; }
  [[ -z "$(git status --porcelain)" ]] || { log 'checkout is dirty; refusing new cycle'; return 1; }
  git fetch --quiet origin main || return 1
  [[ "$(git rev-parse HEAD)" == "$(git rev-parse origin/main)" ]] || {
    git pull --ff-only --quiet origin main || return 1
    [[ -z "$(git status --porcelain)" ]] || return 1
  }
}

# Standard retention: remove only regular detailed logs older than thirty days.
find "$STATE_DIR/sessions" -maxdepth 1 -type f -name '*.log' -mtime +30 -delete
log 'controller started (manual service activation only)'
while (( ! stop )); do
  if [[ -f "$PENDING" ]]; then
    # A crashed controller cannot safely claim new work without inspecting its checkout.
    # A clean main after crash is safe to reselect; GitHub is the authoritative state.
    if ready_checkout; then
      log 'recovered old pending marker: checkout clean, reselecting from GitHub'
      rm -f "$PENDING"
    else
      log 'ERROR: pending cycle and unready checkout: manual reconciliation required'
      sleep "$IDLE_POLL_SECONDS"
      continue
    fi
  fi
  if ! ready_checkout; then
    sleep "$IDLE_POLL_SECONDS"
    continue
  fi
  selection=""
  if ! selection="$(python3 "$ROOT/scripts/autonomy-work-selection.py" --repo "$CALLHARBOR_REPO")"; then
    log 'selection failed; backing off (NOT idle)'
    sleep "$IDLE_POLL_SECONDS"
    continue
  fi
  kind="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["kind"])' <<<"$selection")" || {
    log 'invalid selector output'; sleep "$IDLE_POLL_SECONDS"; continue;
  }
  if [[ "$kind" == idle ]]; then
    sleep "$IDLE_POLL_SECONDS"
    continue
  fi
  session_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
  prompt="$(mktemp "$STATE_DIR/sessions/session.XXXXXX.prompt")"
  out="$STATE_DIR/sessions/$session_id.log"
  {
    cat "$ROOT/.autonomous/AUTONOMY_PROMPT.md"
    printf '\n\n## Advisory selected work (revalidate before writing)\n%s\n' "$selection"
  } >"$prompt"
  printf '%s\n' "$selection" > "$PENDING"
  log "starting session $session_id kind=$kind"
  rc=0
  # Runner contract: argv[1] absolute prompt path, argv[2] session ID.
  # Close lock FD in children so only controller retains exclusive ownership.
  setsid timeout --signal=TERM --kill-after=30s "$CYCLE_TIMEOUT_SECONDS" "$AUTONOMY_RUNNER" "$prompt" "$session_id" 9>&- >"$out" 2>&1 &
  child=$!
  wait "$child" || rc=$?
  # Group must have no living descendants before moving on.
  if ps -eo pgid=,stat= | awk -v pgid="$child" '$1==pgid && $2 !~ /^Z/ { found=1 } END { exit !found }'; then
    log 'ERROR: session descendants still alive; refusing re-entry'
    finish_child
    exit 77
  fi
  child=''
  log "session $session_id exit=$rc (detailed log: $out)"
  rm -f "$prompt"
  # If the session leaves a dirty checkout, preserve marker and block next claim.
  if ready_checkout; then rm -f "$PENDING"; fi
  # Selection or agent failures must never cause rapid retry loops.
  sleep "$IDLE_POLL_SECONDS"
done
