#!/usr/bin/env bash
# OPTIONAL agent runtime adapter. Canonical governance does not depend on this CLI.
set -Eeuo pipefail
[[ $# == 2 && -s "$1" ]] || { echo 'usage: adapter PROMPT_FILE SESSION_ID' >&2; exit 2; }
OPENCODE_BIN="${OPENCODE_BIN:-${HOME}/.opencode/bin/opencode}"
if [[ ! -x "$OPENCODE_BIN" ]]; then OPENCODE_BIN="$(command -v opencode || true)"; fi
[[ -n "$OPENCODE_BIN" && -x "$OPENCODE_BIN" ]] || { echo 'OpenCode adapter is not installed; set AUTONOMY_RUNNER to another adapter' >&2; exit 2; }
exec "$OPENCODE_BIN" run --auto --agent orchestrator --title "CallHarbor session $2" "$(cat "$1")"
