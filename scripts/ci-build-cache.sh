#!/usr/bin/env bash
# scripts/ci-build-cache.sh restore|save <build root>: the release build's caches (<build root>/cache/{gomod,gocache,
# cargo-target,meshd}, scripts/build-socialweb-computer.sh) in the bucket's build-cache/<kind>/<key>.tar.zst with a
# <key>.sha256 beside it. Content-addressed: every cache holds only what its inputs name, so a build with one restored
# gives the bytes of a cold build (-trimpath; verified by comparing SHA256SUMS).
#   cargo-target  rust/Cargo.lock, rust/rust-toolchain.toml (1.2 GB, 353 MB packed)
#   meshd         the git tree of rust/ (socialweb-meshd per arch, as build.sh keys it; 8 MB packed)
# Opt-in with BUILD_CACHE_KINDS="cargo-target meshd gomod gocache" (the default is the first two): measured on hat-8 for
# d5f6601e, a fully warm release build is 80 s on the asset cache alone, 12 s with all four kinds, but restoring gomod
# (510 MB packed) and gocache (3.1 GB packed, 11 GB unpacked) costs 150 s over the public URL, more than the 68 s they save.
#   gomod         go.sum of the module and of each asset module, and the Go version
#   gocache       the same, and linux_amd64 (the go build cache entries are keyed by compiler, flags and sources)
# restore: BUILD_CACHE_URL, the bucket's public HTTPS build-cache/ URL (any run can read it, like ASSET_CACHE_URL); a kind
# that is absent, unreadable, does not hash to its record or does not unpack is skipped: a cold build is the fallback and
# restore never fails. save: only with the bucket key (SPACES_ACCESS_KEY_ID, SPACES_SECRET_ACCESS_KEY: sealed secrets of
# main and v* pushes; a fork, a pull or a branch has none and writes nothing), uploads a kind only where the key is absent,
# under BUILD_CACHE_PREFIX (default build-cache). Run from a kcp checkout.
set -euo pipefail
mode="${1:?usage: ci-build-cache.sh restore|save <build root>}"
cache="$(cd "${2:?usage: ci-build-cache.sh restore|save <build root>}" && pwd -P)/cache"
read -ra kinds <<<"${BUILD_CACHE_KINDS:-cargo-target meshd}"

sha_of() {
  local h
  h="$(sha256sum "$1" 2>/dev/null | cut -d' ' -f1)"
  if [ -z "$h" ]; then
    echo "sha_of: cannot hash $1" >&2
    printf 'unhashable:%s\n' "$1"
    return 1
  fi
  printf '%s\n' "$h"
}
want_go="$(awk '$1 == "toolchain" {print $2; exit}' go.mod)"
[ -n "$want_go" ] || want_go="go$(awk '$1 == "go" {print $2; exit}' go.mod)"
sums="$(sha_of go.sum)"
for m in $(jq -r '[.assets[].fetch // {} | .[] | select(.kind == "go") | .module // empty] | unique[]' internal/versions/versions.json); do
  sums="$sums $(sha_of "$m/go.sum")"
done

key_of() {
  case "$1" in
    gomod) printf '%s\n' "$want_go $sums" ;;
    gocache) printf '%s\n' "$want_go linux_amd64 $sums" ;;
    cargo-target) printf '%s\n' "$(sha_of rust/Cargo.lock) $(sha_of rust/rust-toolchain.toml)" ;;
    meshd) git rev-parse HEAD:rust ;;
  esac | sha256sum | cut -d' ' -f1
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

case "$mode" in
  restore)
    : "${BUILD_CACHE_URL:?restore needs BUILD_CACHE_URL}"
    mkdir -p "$cache"
    for k in "${kinds[@]}"; do
      key="$(key_of "$k")" || { echo "build-cache: $k key: cannot be computed: cold"; continue; }
      u="$BUILD_CACHE_URL/$k/$key"
      t0=$SECONDS
      rm -rf "$work/x" && mkdir -p "$work/x"
      if curl -sfL --retry 2 --max-time 900 -o "$work/$k.tar.zst" "$u.tar.zst" && curl -sfL --max-time 60 -o "$work/$k.sha256" "$u.sha256" &&
        [ -s "$work/$k.sha256" ] && [ "$(sha_of "$work/$k.tar.zst")" = "$(cat "$work/$k.sha256")" ] &&
        zstd -dcq "$work/$k.tar.zst" | tar -x -C "$work/x" && [ -d "$work/x/$k" ]; then
        rm -rf "${cache:?}/$k" && mv "$work/x/$k" "$cache/$k"
        echo "build-cache: $k restored from $u ($(du -sh "$cache/$k" | cut -f1), $((SECONDS - t0)) s)"
      else
        echo "build-cache: $k key $key is not in the bucket, unreadable or does not unpack: cold"
      fi
      rm -rf "$work/$k.tar.zst" "$work/$k.sha256" "$work/x"
    done
    ;;
  save)
    if [ -z "${SPACES_ACCESS_KEY_ID:-}" ] || [ -z "${SPACES_SECRET_ACCESS_KEY:-}" ] || [ -z "${SPACES_BUCKET:-}" ] || [ -z "${SPACES_REGION:-}" ]; then
      echo "build-cache: no bucket key here: nothing is saved"
      exit 0
    fi
    prefix="${BUILD_CACHE_PREFIX:-build-cache}"
    base="https://$SPACES_BUCKET.$SPACES_REGION.digitaloceanspaces.com/$prefix"
    n=0
    for k in "${kinds[@]}"; do
      [ -n "$(ls -A "$cache/$k" 2>/dev/null)" ] || { echo "build-cache: $k is empty here: not saved"; continue; }
      key="$(key_of "$k")"
      if curl -sfI "$base/$k/$key.sha256" >/dev/null; then
        echo "build-cache: $k key $key is already in the bucket"
        continue
      fi
      t0=$SECONDS
      mkdir -p "$work/up/$k"
      tar -C "$cache" -cf - "$k" | zstd -q -3 -T0 -o "$work/up/$k/$key.tar.zst"
      sha_of "$work/up/$k/$key.tar.zst" >"$work/up/$k/$key.sha256"
      echo "build-cache: $k key $key packed ($(du -sh "$work/up/$k/$key.tar.zst" | cut -f1), $((SECONDS - t0)) s)"
      n=$((n + 1))
    done
    if [ "$n" -gt 0 ]; then
      deno run -A --no-lock "$(dirname "$0")/publish-spaces.ts" --dir "$work/up" --prefix "$prefix" --content-addressed
    fi
    ;;
  *) echo "usage: ci-build-cache.sh restore|save <build root>" >&2; exit 2 ;;
esac
