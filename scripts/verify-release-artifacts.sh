#!/usr/bin/env bash
# Verify published release artifacts separately from ordinary product CI.
# Check 'latest' equality only when explicitly verifying a newly published release.
set -Eeuo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

TAG="${1:-v1.0.0}"
CHECK_LATEST="${2:-}"
if (( $# > 2 )) || [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || [[ -n "$CHECK_LATEST" && "$CHECK_LATEST" != --check-latest ]]; then
  echo "Usage: bash scripts/verify-release-artifacts.sh [vX.Y.Z] [--check-latest]" >&2
  exit 2
fi

REPO="${CALLHARBOR_FULL_REPO:-CallHarborMCP/callharbor}"
IMAGE="ghcr.io/${REPO,,}"
for cmd in gh docker python3 git sha256sum; do
  command -v "$cmd" >/dev/null || { echo "Missing $cmd" >&2; exit 1; }
done
docker buildx version >/dev/null || { echo 'Docker buildx required' >&2; exit 1; }

release_json="$(gh release view "$TAG" -R "$REPO" --json tagName,isDraft,isPrerelease,assets)"
python3 -c '
import json,sys
r=json.load(sys.stdin)
assert r["tagName"] == sys.argv[1] and not r["isDraft"] and not r["isPrerelease"]
names={x["name"] for x in r["assets"]}
assert {"compose.yaml","example.env"} <= names, names
' "$TAG" <<<"$release_json"

git fetch --no-tags origin tag "$TAG" || git fetch origin "refs/tags/$TAG"
release_sha="$(git rev-parse "refs/tags/$TAG^{commit}")"
git merge-base --is-ancestor "$release_sha" HEAD || {
  echo "Released source $TAG must be an ancestor of checked out main" >&2
  exit 1
}

raw="$(docker buildx imagetools inspect --raw "$IMAGE:$TAG")"
python3 -c '
import json,sys
j=json.load(sys.stdin)
p={"{}/{}".format(m.get("platform",{}).get("os"),m.get("platform",{}).get("architecture")) for m in j.get("manifests",[])}
assert {"linux/amd64","linux/arm64"} <= p, p
' <<<"$raw"

if [[ "$CHECK_LATEST" == --check-latest ]]; then
  latest="$(docker buildx imagetools inspect --raw "$IMAGE:latest")"
  [[ "$(printf '%s' "$raw" | sha256sum | cut -d' ' -f1)" == "$(printf '%s' "$latest" | sha256sum | cut -d' ' -f1)" ]] || {
    echo "$TAG and latest have different OCI manifest digests" >&2
    exit 1
  }
fi

printf '[callharbor] Verified %s release assets, source tag and amd64/arm64 OCI index\n' "$TAG"
