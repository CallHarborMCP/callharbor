#!/usr/bin/env bash
# Final CI gate verifies the actual public release, Git tag and OCI manifests.
# Real clean-install/restore evidence remains a separate mandatory DoD gate.
set -Eeuo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
REPO="${CALLHARBOR_FULL_REPO:-CallHarborMCP/callharbor}"
IMAGE="ghcr.io/${REPO,,}"
for cmd in gh docker python3 git sha256sum; do
  command -v "$cmd" >/dev/null || { echo "Missing $cmd" >&2; exit 1; }
done
docker buildx version >/dev/null || {
  echo 'Docker buildx required to verify public multi-architecture image' >&2
  exit 1
}
release_json="$(gh release view v1.0.0 -R "$REPO" \
  --json tagName,isDraft,isPrerelease,assets)"
python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["tagName"]=="v1.0.0" and not r["isDraft"] and not r["isPrerelease"]
names={x["name"] for x in r["assets"]}
assert {"compose.yaml","example.env"} <= names, names
' <<<"$release_json"
git fetch --no-tags origin tag v1.0.0 || git fetch origin refs/tags/v1.0.0
release_sha="$(git rev-parse 'refs/tags/v1.0.0^{commit}')"
git merge-base --is-ancestor "$release_sha" HEAD || {
  echo 'Released source tag must be an ancestor of completion-marker main' >&2
  exit 1
}
# Every tag must be a real multi-platform index; latest and version must match.
digests=()
for tag in v1.0.0 latest; do
  raw="$(docker buildx imagetools inspect --raw "$IMAGE:$tag")"
  python3 -c '
import json,sys
j=json.load(sys.stdin)
p={"{}/{}".format(m.get("platform",{}).get("os"),m.get("platform",{}).get("architecture")) for m in j.get("manifests",[])}
assert {"linux/amd64","linux/arm64"} <= p, p
' <<<"$raw"
  digests+=("$(printf '%s' "$raw" | sha256sum | cut -d' ' -f1)")
done
[[ "${digests[0]}" == "${digests[1]}" ]] || {
  echo 'GHCR v1.0.0 and latest are different OCI manifest digests' >&2
  exit 1
}
printf '[callharbor] Verified public release, source tag and amd64/arm64 OCI digests\n'
