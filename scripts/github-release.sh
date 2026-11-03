#!/bin/sh
# Publishes a GitHub Release with assets, as the CI release job does.
#
#   GITHUB_TOKEN=... scripts/github-release.sh owner/repo v1.2.0 <commit sha> file...
#
# The mirror pushes the commit to GitHub on its own schedule, so this waits for it, then creates
# the release as a draft, uploads the files and publishes it, so "latest" never points at a
# release without its files.
set -eu

REPO="$1"; TAG="$2"; SHA="$3"; shift 3
API="https://api.github.com/repos/$REPO"
auth() { curl -fsS -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: application/vnd.github+json" "$@"; }

echo "• waiting for $SHA on github.com/$REPO"
i=0
until auth -o /dev/null "$API/commits/$SHA" 2>/dev/null; do
  i=$((i + 1)); [ $i -le 60 ] || { echo "the mirror did not push $SHA within 10 minutes" >&2; exit 1; }
  sleep 10
done

echo "• creating release $TAG"
RELEASE="$(auth -X POST "$API/releases" -d "$(jq -n --arg t "$TAG" --arg c "$SHA" \
  '{tag_name: $t, target_commitish: $c, name: $t, draft: true}')")"
ID="$(printf '%s' "$RELEASE" | jq -r .id)"
UPLOAD="$(printf '%s' "$RELEASE" | jq -r .upload_url | sed 's/{.*//')"

for f in "$@"; do
  echo "• uploading $(basename "$f")"
  auth -o /dev/null -X POST -H "Content-Type: application/octet-stream" \
    --data-binary "@$f" "$UPLOAD?name=$(basename "$f")"
done

auth -o /dev/null -X PATCH "$API/releases/$ID" -d '{"draft": false}'
echo "• published https://github.com/$REPO/releases/tag/$TAG"
