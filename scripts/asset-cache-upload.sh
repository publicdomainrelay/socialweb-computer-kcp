#!/usr/bin/env bash
# scripts/asset-cache-upload.sh <blob cache dir>: the blobs stage-assets listed in <dir>/.new under a pinned- or
# source-input key, to the bucket's asset-cache/<asset>/<goos>_<goarch>/<key>.{tar.zst,sha256} with
# scripts/publish-spaces.ts (SPACES_* env), content-addressed: a key already there is not uploaded again.
set -euo pipefail
c="$1"
[ -s "$c/.new" ] || { echo "asset-cache: nothing new in $c"; exit 0; }
d="$(mktemp -d)"
sort -u "$c/.new" | while read -r e; do
  k="${e##*/}"
  case "$k" in in-*|src-*) ;; *) continue ;; esac
  [ -f "$c/$k.tar.zst" ] && [ -f "$c/$k.sha256" ] || continue
  mkdir -p "$d/$(dirname "$e")"
  ln -f "$c/$k.tar.zst" "$d/$e.tar.zst" 2>/dev/null || cp "$c/$k.tar.zst" "$d/$e.tar.zst"
  cp "$c/$k.sha256" "$d/$e.sha256"
done
if [ -n "$(find "$d" -type f | head -1)" ]; then
  deno run -A --no-lock "$(dirname "$0")/publish-spaces.ts" --dir "$d" --prefix asset-cache --content-addressed
fi
rm -rf "$d"
