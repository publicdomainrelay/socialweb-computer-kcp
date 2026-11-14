#!/usr/bin/env bash
# scripts/check-reproducible.sh: the proof that replaces committed output pins for the assets built from this repository's
# own commit (versions.json fetch kind go or script: socialweb-vmexec, the OpenBao plugins, swc-userns-stage, the two
# downloads trees). It stages them COLD (a fresh clean checkout of the commit at a new path, empty go build, module and
# cargo caches, no blob cache, no ASSET_CACHE_URL, nothing taken from a bucket) and compares every unpacked tree digest
# with the other build it is given; any byte of difference fails the check and names the asset, platform and files.
#   without --against   two independent cold builds (a, b) are made and compared with each other
#   --against DIR       DIR is the blobs directory of the build to prove (<goos>_<goarch>/<asset>.tar.zst, as embedded in
#                       the release binary, e.g. <build-root>/src/1/internal/assets/blobs): its blobs are unpacked and
#                       hashed (not its .built.json, which must also equal them), and one cold build is compared to them,
#                       so a cache that handed the release a different tree is caught too
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build_root="$HOME/.tmp/socialweb-computer-build/reproduce"
rev="HEAD"
against=""
gomod_cache=""
platforms=()

usage() {
  echo "usage: $0 [--platform P]... [--commit REV] [--build-root DIR] [--against BLOBS_DIR] [--gomod-cache DIR]" >&2
  echo "  stages the assets built from this repository cold (twice, or once against --against) and fails on any difference" >&2
  exit 2
}
while [ $# -gt 0 ]; do
  case "$1" in
    --platform) platforms+=("$2"); shift 2 ;;
    --commit) rev="$2"; shift 2 ;;
    --build-root) build_root="$2"; shift 2 ;;
    --against) against="$2"; shift 2 ;;
    --gomod-cache) gomod_cache="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done

die() { echo "check-reproducible: $*" >&2; exit 1; }
say() { echo "check-reproducible: $(date -u +%H:%M:%SZ) $*" >&2; }
for tool in git go jq zstd tar sha256sum cmp diff; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

versions="$root/internal/versions/versions.json"
commit="$(git -C "$root" rev-parse --verify "$rev^{commit}")" || die "no commit $rev"
if [ ${#platforms[@]} -eq 0 ]; then
  mapfile -t platforms < <(jq -r '.platforms | to_entries[] | select(.value.status == "release") | .key' "$versions")
fi
mkdir -p "$build_root"
build_root="$(cd "$build_root" && pwd -P)"
case "$build_root" in /tmp|/tmp/*) die "--build-root $build_root is under /tmp" ;; esac
[ -z "$against" ] || against="$(cd "$against" && pwd -P)"

assets_of() {
  jq -r --arg p "$1" '
    . as $v | $v.platforms[$p].assets[] | select(. as $a | ($v.assets[$a].fetch // {}) | length > 0
      and ([.[].kind] | all(. == "go" or . == "script")))' "$versions"
}

want_go="$(git -C "$root" show "$commit:go.mod" | awk '$1=="toolchain"{print $2; exit}')"
[ -n "$want_go" ] || want_go="go$(git -C "$root" show "$commit:go.mod" | awk '$1=="go"{print $2; exit}')"

# cold NAME: a fresh checkout at $build_root/NAME/src and empty caches; stages every built asset of every platform into
# $build_root/NAME/blobs
cold() {
  local n="$1" d="$build_root/$1" tree gm module
  chmod -R u+w "$d" 2>/dev/null || true
  rm -rf "$d"
  mkdir -p "$d/tmp" "$d/gocache" "$d/dl" "$d/meshd" "$d/cargo-target"
  tree="$d/src"
  git clone --quiet --shared --no-checkout "$root" "$tree"
  git -C "$tree" checkout --quiet --force --detach "$commit"
  git -C "$tree" clean -ffdxq
  [ "$(git -C "$tree" rev-parse HEAD)" = "$commit" ] || die "$tree is not at $commit"
  gm="${gomod_cache:-$d/gomod}"
  mkdir -p "$gm"
  (
    unset GOOS GOARCH GOPATH GOROOT SOURCE_DATE_EPOCH ASSET_CACHE_URL
    export TMPDIR="$d/tmp" GOTMPDIR="$d/tmp" GOMODCACHE="$gm" GOCACHE="$d/gocache" GOFLAGS=-mod=readonly CGO_ENABLED=0 GOENV=off
    export MESHD_CACHE="$d/meshd" GUEST_DOWNLOADS_CACHE="$d/dl" CARGO_TARGET_DIR="$d/cargo-target"
    goroot="$(GOTOOLCHAIN="$want_go" go env GOROOT)" || die "cannot get the pinned toolchain $want_go"
    export PATH="$goroot/bin:$PATH" GOTOOLCHAIN=local
    (cd "$tree" && GOPROXY="$(GOENV= go env GOPROXY)" go mod download)
    for module in $(jq -r '[.assets[].fetch // {} | .[] | select(.kind == "go") | .module // empty] | unique[]' "$tree/internal/versions/versions.json"); do
      (cd "$tree/$module" && GOPROXY="$(GOENV= go env GOPROXY)" go mod download)
    done
    export GOPROXY=off GOSUMDB=off
    args=()
    names=()
    for p in "${platforms[@]}"; do
      args+=(--platform "$p")
      mapfile -t -O "${#names[@]}" names < <(assets_of "$p")
    done
    for a in $(printf '%s\n' "${names[@]}" | sort -u); do args+=(--asset "$a"); done
    say "cold build $n: staging ${args[*]} into $d/blobs"
    (cd "$tree" && ./scripts/stage-assets.sh --cache "$d/dl" --blobs "$d/blobs" "${args[@]}")
    (cd "$tree" && go build -trimpath -buildvcs=false -o "$d/assetpack" ./internal/assets/assetpack)
  ) >&2 || die "cold build $n failed"
}

unpack() {
  local blob="$1" dir="$2"
  mkdir -p "$dir"
  zstd -dcq "$blob" | tar -x -C "$dir"
}

digest_of() {
  "$build_root/b/assetpack" digest "$1"
}

fail=0
check_record() {
  local f="$1" want="$2" plat="$3" asset="$4"
  [ -f "$f" ] || { echo "check-reproducible: $plat $asset: no build record at $f" >&2; fail=1; return; }
  if [ "$(jq -r .treeSha256 "$f")" != "$want" ]; then
    echo "check-reproducible: $plat $asset: the build record $f says tree $(jq -r .treeSha256 "$f") but its blob unpacks to $want" >&2
    fail=1
  fi
}

compare() {
  local left="$1" right="$2" lname="$3" rname="$4" plat asset key lb rb ld rd tmp f
  for plat in "${platforms[@]}"; do
    key="${plat/\//_}"
    for asset in $(assets_of "$plat"); do
      lb="$left/$key/$asset.tar.zst"
      rb="$right/$key/$asset.tar.zst"
      [ -s "$lb" ] || { echo "check-reproducible: $plat $asset: no blob at $lb" >&2; fail=1; continue; }
      [ -s "$rb" ] || { echo "check-reproducible: $plat $asset: no blob at $rb" >&2; fail=1; continue; }
      tmp="$(mktemp -d "$build_root/cmp.XXXXXX")"
      unpack "$lb" "$tmp/l"
      unpack "$rb" "$tmp/r"
      ld="$(digest_of "$tmp/l")"
      rd="$(digest_of "$tmp/r")"
      check_record "$left/$key/$asset.built.json" "$ld" "$plat" "$asset"
      check_record "$right/$key/$asset.built.json" "$rd" "$plat" "$asset"
      if [ "$ld" = "$rd" ]; then
        echo "reproducible $plat $asset tree $ld ($lname == $rname)"
      else
        fail=1
        echo "DIFFERENT $plat $asset: $lname tree $ld, $rname tree $rd"
        diff -rq "$tmp/l" "$tmp/r" | sed 's/^/    /' || true
        (cd "$tmp/l" && find . -type f | sort) | while read -r f; do
          if [ -f "$tmp/r/$f" ] && ! cmp -s "$tmp/l/$f" "$tmp/r/$f"; then
            echo "    $f: $(sha256sum <"$tmp/l/$f" | cut -c1-16) vs $(sha256sum <"$tmp/r/$f" | cut -c1-16); first differing bytes (offset, octal $lname, $rname): $(cmp -l "$tmp/l/$f" "$tmp/r/$f" 2>/dev/null | head -3 | tr '\n' ';')"
          fi
        done
      fi
      chmod -R u+w "$tmp"; rm -rf "$tmp"
    done
  done
}

if [ -n "$against" ]; then
  cold b
  compare "$against" "$build_root/b/blobs" "the build under proof" "a cold rebuild"
else
  cold a
  cold b
  compare "$build_root/a/blobs" "$build_root/b/blobs" "cold build a" "cold build b"
fi
[ "$fail" = 0 ] || die "NOT reproducible: a cold build of commit $commit differs; see the lines above"
say "reproducible: commit $commit, platforms ${platforms[*]}"
