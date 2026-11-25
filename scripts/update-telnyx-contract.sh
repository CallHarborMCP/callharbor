#!/usr/bin/env bash
# Regenerates api/upstream/telnyx/callcontrol.openapi.json from a pinned official
# Telnyx OpenAPI revision. Network is needed only when regenerating; tests use the
# committed extract. Use --check to fail when the committed extract has drifted.
set -Eeuo pipefail
cd "$(dirname "$0")/.."

# Public upstream git commit and content digest pins, not credentials.
TELNYX_OPENAPI_SHA="bf2eb8ed5b816213987fecae8fe33329e98cdd84" # gitleaks:allow
TELNYX_OPENAPI_SHA256="fc59cb60a25c5091e73889842c808f1db7b4c0bb36bbb0228ee9ed2f629eca18" # gitleaks:allow
SOURCE_URL="https://raw.githubusercontent.com/team-telnyx/openapi/${TELNYX_OPENAPI_SHA}/openapi/spec3.json"
OUT="api/upstream/telnyx/callcontrol.openapi.json"

MODE="${1:-}"
if [[ -n "$MODE" && "$MODE" != --check ]]; then
  echo "Usage: bash scripts/update-telnyx-contract.sh [--check]" >&2
  exit 2
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 -o "$tmp/spec3.json" "$SOURCE_URL"
echo "${TELNYX_OPENAPI_SHA256}  $tmp/spec3.json" | sha256sum -c --quiet -

jq -S --arg sha "$TELNYX_OPENAPI_SHA" --arg sum "$TELNYX_OPENAPI_SHA256" --arg url "$SOURCE_URL" '
  def refs: [.. | objects | .["$ref"]? // empty | strings | select(startswith("#/components/"))];
  def target($doc): ltrimstr("#/") | split("/") as $p | {path: $p, value: ($doc | getpath($p))};
  . as $doc
  | {
      paths: {
        "/calls": {post: .paths["/calls"].post},
        "/calls/{call_control_id}": {get: .paths["/calls/{call_control_id}"].get},
        "/calls/{call_control_id}/actions/hangup": .paths["/calls/{call_control_id}/actions/hangup"],
        "/calls/{call_control_id}/actions/streaming_start": .paths["/calls/{call_control_id}/actions/streaming_start"],
        "/calls/{call_control_id}/actions/streaming_stop": .paths["/calls/{call_control_id}/actions/streaming_stop"],
        "/call_control_applications/{id}": {get: .paths["/call_control_applications/{id}"].get},
        "/phone_numbers": {get: .paths["/phone_numbers"].get}
      },
      webhooks: (.webhooks | with_entries(select(.key | IN(
        "callInitiated", "callAnswered", "callHangup", "callMachineDetectionEnded",
        "callStreamingStarted", "callStreamingStopped", "callStreamingFailed"))))
    } as $sel
  | {seen: [], todo: ($sel | refs | unique)}
  | until(.todo | length == 0;
      .seen += .todo
      | .todo = ((([.todo[] | target($doc).value | refs] | add // []) | unique) - .seen)
    ) as $closure
  | reduce ($closure.seen[] | target($doc)) as $t
      ({
        openapi: $doc.openapi,
        info: {
          title: "Telnyx Call Control subset used by CallHarbor",
          version: $doc.info.version,
          "x-callharbor-source": {
            repository: "https://github.com/team-telnyx/openapi",
            commit: $sha,
            file: "openapi/spec3.json",
            url: $url,
            sha256: $sum,
            note: "Mechanical subset of the official specification; regenerate with scripts/update-telnyx-contract.sh. Do not edit by hand."
          }
        },
        paths: $sel.paths,
        webhooks: $sel.webhooks
      }; setpath($t.path; $t.value))
' "$tmp/spec3.json" > "$tmp/out.json"

if [[ "$MODE" == --check ]]; then
  if ! diff -q "$OUT" "$tmp/out.json" >/dev/null; then
    echo "Telnyx contract extract differs from pinned upstream; run scripts/update-telnyx-contract.sh" >&2
    exit 1
  fi
  echo "Telnyx contract extract matches ${TELNYX_OPENAPI_SHA}"
  exit 0
fi
mkdir -p "$(dirname "$OUT")"
mv "$tmp/out.json" "$OUT"
echo "wrote $OUT from ${TELNYX_OPENAPI_SHA}"
