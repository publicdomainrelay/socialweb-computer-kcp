#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build_root="$HOME/.tmp/socialweb-computer-build"
rev="HEAD"
platforms=()
dev=0
update_pins=0
publish=""
sync_only=0

usage() {
  cat >&2 <<EOF
usage: $0 [--platform linux/amd64|linux/arm64]... [--commit REV] [--build-root DIR] [--update-pins]
       $0 --dev [--platform P]... [--build-root DIR] [--publish DIR [--sync-only]]

--dev builds ONE socialweb-computer of the working tree of this checkout (uncommitted changes
included) for the host platform (or each --platform), for live runs while iterating. It reuses the
build root's go, asset and deno caches, skips the clean checkout, and is not
reproducible: 'socialweb-computer version' says "dev build, not reproducible" with the commit and the
tree's dirty state. It embeds every pinned asset (staged once per versions.json under
dev/assets/<goos>_<goarch>/<asset>-<sha256 of its versions.json entry>; the assets built from
this repository (fetch kind go, such as socialweb-vmexec) are built from the working tree each time).
Output: dev/<worktree>/<goos>_<goarch>/socialweb-computer
(and dev/<worktree>/socialweb-computer for the host platform).

  --publish DIR  also copy the host binary to DIR/bin/socialweb-computer and mirror the working tree
                 (tracked and untracked files git does not ignore) to DIR/src, readable by every
                 account (chmod a+rX), and build the binary to run from DIR/src: an
                 account such as test-0001 that cannot read this checkout runs from there
  --sync-only    with --publish: only mirror the working tree to DIR/src again (no build), before a
                 dev restart of a component

Without --dev it builds the release socialweb-computer binary (plan 0006 M4-BUILD) of one commit for
each release platform of internal/versions/versions.json (in parallel), once, from a clean checkout.
It stages every pinned asset with scripts/stage-assets.sh (in parallel, every tree checked against
its pin), stages the embedded Deno for the policy engine, then runs
go build -tags "release embed_deno" with the go.mod toolchain, CGO_ENABLED=0, -trimpath, -buildvcs=true,
GOPROXY=off. Its caches are content-addressed, so a warm build gives the same bytes as a clean one:
  cache/blobs     the zstd blob of an asset, by the sha256 of its versions.json entry, stage-assets.sh,
                  assetpack and the zstd version (downloaded kinds), or of its tar stream (built kinds)
  cache/meshd     socialweb-meshd per arch, by the sha256 of rust/ (with the zig it pins) and the rustc and cargo versions
  cache/cargo-target, cache/gocache   the cargo target dir and the go build cache
A step whose key did not change is not run again.

--update-pins stages the assets built from this repository (fetch kind go or script) and writes their
new tree and binary pins into versions.json in the clean checkout and in the working repository. Pins
of downloaded assets are never changed; an upstream change still fails the build. make uses it by
default; CI runs make check-pins and fails when versions.json differs from HEAD.

  --platform    a release platform; repeat for more (default: every release platform)
  --commit      the commit to build (default HEAD of this checkout; uncommitted changes are not built)
  --build-root  checkouts, caches and outputs, all kept between builds (default \$HOME/.tmp/socialweb-computer-build)

Layout under the build root:
  src/<n>                       the clean checkout of build n
  cache/                        pinned downloads (assets, gomod) and the build caches above
  work/<n>/                     the staging of build n
  out/<commit>/<n>/<goos>_<goarch>  socialweb-computer, its sha256, build.json, version.json
  release/<commit>              socialweb-computer-<goos>-<goarch>, SHA256SUMS, release.json,
                                version-<goos>-<goarch>.{json,txt}

The binary and its sha256 are what socialweb-computer install and upgrade --binary --sha256 take.
Every phase prints a "timing" line. CONTAINER=podman selects podman for the swc-userns-stage build
(default docker).
EOF
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --platform) platforms+=("$2"); shift 2 ;;
    --commit) rev="$2"; shift 2 ;;
    --build-root) build_root="$2"; shift 2 ;;
    --dev) dev=1; shift ;;
    --update-pins) update_pins=1; shift ;;
    --publish) publish="$2"; shift 2 ;;
    --sync-only) sync_only=1; shift ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done

die() { echo "build-socialweb-computer: $*" >&2; exit 1; }
say() { echo "build-socialweb-computer: $(date -u +%Y-%m-%dT%H:%M:%SZ) $*" >&2; }
utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }
sha_of() { sha256sum "$1" | cut -d' ' -f1; }
now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }'; }
timing() {
  local line="timing $1 $(since "$2")s"
  say "$line"
  echo "$line" >>"$timings"
}

for tool in git go jq zstd tar sha256sum unshare flock curl; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required on the build host"
done

mkdir -p "$build_root"
build_root="$(cd "$build_root" && pwd -P)"
case "$build_root" in
  /tmp|/tmp/*) die "--build-root $build_root is under /tmp" ;;
esac

versions="$root/internal/versions/versions.json"

if [ "$dev" = 1 ]; then
  [ "$update_pins" = 0 ] || die "--update-pins does not go with --dev; run make pins first"
  exec "$root/scripts/build-socialweb-computer-dev.sh" "$root" "$build_root" "$publish" "$sync_only" "${platforms[@]}"
fi
if [ -n "$publish" ] || [ "$sync_only" = 1 ]; then
  die "--publish and --sync-only go with --dev"
fi

exec 8>"$build_root/release.lock"
flock -n 8 || die "another release build holds $build_root/release.lock"

started_all="$(now)"
timings="$build_root/tmp/timings.$$"
mkdir -p "$build_root/tmp"
: >"$timings"

if [ ${#platforms[@]} -eq 0 ]; then
  mapfile -t platforms < <(jq -r '.platforms | to_entries[] | select(.value.status == "release") | .key' "$versions")
fi
for plat in "${platforms[@]}"; do
  [ "$(jq -r --arg p "$plat" '.platforms[$p].status // ""' "$versions")" = "release" ] || die "$plat is not a release platform of versions.json"
done

commit="$(git -C "$root" rev-parse --verify "$rev^{commit}")" || die "no commit $rev"
if [ "$rev" = "HEAD" ] && [ -n "$(git -C "$root" status --porcelain --untracked-files=no)" ]; then
  say "warn: $root has uncommitted changes; only commit $commit is built"
fi

version="$(git -C "$root" describe --tags --exact-match "$commit" 2>/dev/null || true)"

want_go="$(git -C "$root" show "$commit:go.mod" | awk '$1=="toolchain"{print $2; exit}')"
if [ -z "$want_go" ]; then
  want_go="go$(git -C "$root" show "$commit:go.mod" | awk '$1=="go"{print $2; exit}')"
fi
cache="$build_root/cache"
mkdir -p "$cache/assets" "$cache/gomod" "$cache/gocache" "$cache/blobs" "$cache/meshd" "$cache/cargo-target"
export TMPDIR="$build_root/tmp"
export GOTMPDIR="$build_root/tmp"
export GOMODCACHE="$cache/gomod"
export GOCACHE="$cache/gocache"
export GOFLAGS=-mod=readonly
export CGO_ENABLED=0
unset GOOS GOARCH GOPATH GOROOT SOURCE_DATE_EPOCH MESHD_CACHE CARGO_TARGET_DIR

goroot="$(GOTOOLCHAIN="$want_go" go env GOROOT)" || die "cannot get the pinned toolchain $want_go"
export PATH="$goroot/bin:$PATH"
export GOTOOLCHAIN=local
export GOENV=off
have_go="$(go env GOVERSION)"
[ "$have_go" = "$want_go" ] || die "the toolchain at $goroot is $have_go; commit $commit pins $want_go"
hostplat="$(go env GOHOSTOS)/$(go env GOHOSTARCH)"

checkout() {
  local tree="$1"
  rm -rf "$tree"
  git clone --quiet --shared --no-checkout "$root" "$tree"
  git -C "$tree" checkout --quiet --force --detach "$commit"
  git -C "$tree" clean -ffdxq
  [ "$(git -C "$tree" rev-parse HEAD)" = "$commit" ] || die "$tree is not at $commit"
}

stop_jobs() {
  local p
  for p in $(jobs -p); do
    pkill -TERM -P "$p" 2>/dev/null || true
    kill -TERM "$p" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}

wait_all() {
  local what="$1" n="$2" i
  for ((i = 0; i < n; i++)); do
    if ! wait -n; then
      stop_jobs
      die "$what failed; stopped the rest"
    fi
  done
}

mode="cached"
[ "$update_pins" = 0 ] || mode="update-pins"
say "commit $commit, platforms ${platforms[*]}, go $have_go, mode $mode, build root $build_root"

t="$(now)"
checkout "$build_root/src/1"
timing "checkout 1" "$t"
t="$(now)"
say "module download into $GOMODCACHE"
(cd "$build_root/src/1" && GOPROXY="$(GOENV= go env GOPROXY)" go mod download)
for module in $(jq -r '[.assets[].fetch // {} | .[] | select(.kind == "go") | .module // empty] | unique[]' "$build_root/src/1/internal/versions/versions.json"); do
  say "module download for asset module $module"
  (cd "$build_root/src/1/$module" && GOPROXY="$(GOENV= go env GOPROXY)" go mod download)
done
export GOPROXY=off
export GOSUMDB=off
timing "module download" "$t"

stage_deno_embedded() {
  local tree="$1" tree_versions="$1/internal/versions/versions.json" dir plat goos goarch url want cached part
  dir="$tree/internal/policyengine/deno/deno_embedded"
  mkdir -p "$cache/deno-embed" "$dir"
  for plat in linux/amd64 linux/arm64; do
    goos="${plat%/*}"
    goarch="${plat#*/}"
    url="$(jq -er --arg p "$plat" '.assets.deno.fetch[$p].url' "$tree_versions")" || die "no pinned deno url for $plat"
    want="$(jq -er --arg p "$plat" '.assets.deno.fetch[$p].sha256' "$tree_versions")" || die "no pinned deno sha256 for $plat"
    cached="$cache/deno-embed/$want.zip"
    if [ ! -f "$cached" ] || [ "$(sha_of "$cached")" != "$want" ]; then
      part="$cached.part.$BASHPID"
      curl -fSL --retry 3 --connect-timeout 10 --speed-limit 10000 --speed-time 20 -o "$part" "$url" \
        || { rm -f "$part"; die "deno download failed: $url"; }
      [ "$(sha_of "$part")" = "$want" ] || { rm -f "$part"; die "deno sha256 mismatch for $url"; }
      mv -f "$part" "$cached"
    fi
    cp -f "$cached" "$dir/deno-$goos-$goarch.zip"
    [ "$(sha_of "$dir/deno-$goos-$goarch.zip")" = "$want" ] || die "staged deno-$goos-$goarch.zip does not match its pin"
  done
}

build_once() {
  local n="$1" tree="$build_root/src/$1" work="$build_root/work/$1" plat goos goarch key out started t
  local stage_cache=() jobs_n
  export CARGO_TARGET_DIR="$cache/cargo-target"
  export MESHD_CACHE="$cache/meshd"
  stage_cache=(--blob-cache "$cache/blobs")
  chmod -R u+w "$work" 2>/dev/null || true
  rm -rf "$work"
  mkdir -p "$GOCACHE" "$CARGO_TARGET_DIR"
  if [ "$n" != 1 ]; then
    t="$(now)"
    checkout "$tree"
    timing "build $n: checkout" "$t"
  fi

  t="$(now)"
  local stage_args=() p
  for p in "${platforms[@]}"; do stage_args+=(--platform "$p"); done
  [ "$update_pins" = 0 ] || stage_args+=(--update-pins)
  say "build $n: stage assets"
  (cd "$tree" && ./scripts/stage-assets.sh --cache "$cache/assets" "${stage_cache[@]}" "${stage_args[@]}")
  if [ "$update_pins" = 1 ]; then
    cp -f "$tree/internal/versions/versions.json" "$root/internal/versions/versions.json"
    say "pins written to $root/internal/versions/versions.json"
  fi
  timing "build $n: stage assets" "$t"

  t="$(now)"
  say "build $n: stage embedded deno"
  stage_deno_embedded "$tree"
  timing "build $n: embedded deno" "$t"

  jobs_n=0
  for plat in "${platforms[@]}"; do
    (
      goos="${plat%/*}"
      goarch="${plat#*/}"
      key="${goos}_${goarch}"
      out="$build_root/out/$commit/$n/$key"
      started="$(utc)"
      t="$(now)"
      rm -rf "$out"
      mkdir -p "$out"
      say "build $n $plat: go build"
      (cd "$tree" && GOOS="$goos" GOARCH="$goarch" go build -tags "release embed_deno" -trimpath -buildvcs=true \
        -ldflags="-s -w -buildid= ${version:+-X github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp.Version=$version}" \
        -o "$out/socialweb-computer" ./cmd/socialweb-computer)
      (cd "$out" && sha256sum socialweb-computer >socialweb-computer.sha256)
      "$out/socialweb-computer" version --json >"$out/version.json"
      "$out/socialweb-computer" version >"$out/version.txt"
      jq -n --arg commit "$commit" --arg platform "$plat" --argjson build "$n" --arg go "$have_go" --arg mode "$mode" \
        --arg started "$started" --arg finished "$(utc)" --arg sha256 "$(sha_of "$out/socialweb-computer")" \
        --argjson denoEmbedded "$(jq -c '{version: .assets.deno.version, zips: (.assets.deno.fetch | with_entries(.value = .value.sha256))}' "$tree/internal/versions/versions.json")" \
        '{commit: $commit, platform: $platform, build: $build, mode: $mode, go: $go, startedUtc: $started, finishedUtc: $finished, sha256: $sha256, denoEmbedded: $denoEmbedded}' \
        >"$out/build.json"
      say "build $n $plat: sha256 $(sha_of "$out/socialweb-computer")"
      timing "build $n $plat: go build" "$t"
    ) &
    jobs_n=$((jobs_n + 1))
  done
  wait_all "build $n: go build" "$jobs_n"
  local dirty
  dirty="$(git -C "$tree" status --porcelain)"
  if [ "$update_pins" = 1 ]; then
    dirty="$(printf '%s\n' "$dirty" | grep -v ' internal/versions/versions.json$' || true)"
  fi
  [ -z "$dirty" ] || die "build $n left $tree dirty: $dirty"
}

t="$(now)"
build_once 1
timing "builds" "$t"

release="$build_root/release/$commit"
rm -rf "$release"
mkdir -p "$release"
for plat in "${platforms[@]}"; do
  key="${plat/\//_}"
  name="socialweb-computer-${plat/\//-}"
  a="$build_root/out/$commit/1/$key"
  cp "$a/socialweb-computer" "$release/$name"
  cp "$a/version.json" "$release/version-${plat/\//-}.json"
  cp "$a/version.txt" "$release/version-${plat/\//-}.txt"
  (cd "$release" && sha256sum "$name" >>SHA256SUMS)
  say "published $plat sha256 $(sha_of "$a/socialweb-computer") -> $release/$name"
done
jq -n --arg commit "$commit" --arg mode "$mode" --slurpfile builds <(for plat in "${platforms[@]}"; do cat "$build_root/out/$commit/1/${plat/\//_}/build.json"; done) \
  '{commit: $commit, mode: $mode, builds: $builds}' >"$release/release.json"
timing "total" "$started_all"
say "timings:"
cat "$timings" >&2
rm -f "$timings"
