#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <from> <to>   (each a kcp release tag, e.g. v0.33.0, or <release>=<commit> for a source-built kcp," >&2
  echo "       e.g. v0.33.0-162-g74e9fb9c03f778=74e9fb9c03f778c57d1a6a782d5adc9476ec1af5, <release> being what kcpserver.Release reads from its --version)" >&2
  exit 2
fi
from="${1%%=*}"
from_rev="${1#*=}"
to="${2%%=*}"
to_rev="${2#*=}"
repo="${KCP_REPO:-https://github.com/kcp-dev/kcp}"
work="$(mktemp -d "${TMPDIR:-$HOME/.tmp}/kcp-compat.XXXXXX")"
trap 'rm -rf "$work"' EXIT

listing() {
  local rev="$1" dir="$work/$2"
  git init -q "$dir"
  git -C "$dir" remote add origin "$repo"
  git -C "$dir" fetch -q --depth 1 --filter=blob:none origin "$rev" 2>/dev/null
  git -C "$dir" sparse-checkout set config >/dev/null 2>&1
  git -C "$dir" checkout -q FETCH_HEAD 2>/dev/null
  local d
  for d in crds system-crds root-phase0 root shard; do
    [ -d "$dir/config/$d" ] || continue
    find "$dir/config/$d" -maxdepth 1 -name '*.yaml' -print0 | sort -z | while IFS= read -r -d '' f; do
      yq -N 'select(.kind == "CustomResourceDefinition") | .metadata.name as $n | .spec.versions[] | "crd " + $n + " " + .name + " served=" + (.served | tostring) + " storage=" + (.storage | tostring)' "$f" 2>/dev/null || true
      yq -N -o=json -I=0 'select(.kind == "APIResourceSchema")' "$f" 2>/dev/null | while IFS= read -r doc; do
        [ -n "$doc" ] || continue
        name="$(printf '%s' "$doc" | yq -p=json -N '.metadata.name')"
        sum="$(printf '%s' "$doc" | yq -p=json -o=json -I=0 '.spec' | sha256sum | cut -d' ' -f1)"
        echo "schema $d $name $sum"
      done || true
      yq -N 'select(.kind == "APIExport") | "export " + .metadata.name + " " + ((.spec.resources // []) | map(.schema) | sort | join(","))' "$f" 2>/dev/null || true
    done
  done | grep -v '^$' | sort -u
}

listing "$from_rev" from >"$work/from.txt"
listing "$to_rev" to >"$work/to.txt"
if [ ! -s "$work/from.txt" ] || [ ! -s "$work/to.txt" ]; then
  echo "kcp-compat: an empty listing for $from or $to; the kcp source layout changed and this script must follow it" >&2
  exit 1
fi
diff -u --label "$from" --label "$to" "$work/from.txt" "$work/to.txt" >"$work/diff.txt" || true
compatible=true
if grep -q '^[-+]crd \|^[-+]schema \|^[-+]export ' "$work/diff.txt"; then
  compatible=false
fi
sum="$(sha256sum "$work/diff.txt" | cut -d' ' -f1)"
if [ -n "${KCP_COMPAT_DIFF:-}" ]; then
  cp "$work/diff.txt" "$KCP_COMPAT_DIFF"
fi
printf '{"from": "%s", "to": "%s", "storageCompatible": %s, "diffSHA256": "%s"}\n' "$from" "$to" "$compatible" "$sum"
