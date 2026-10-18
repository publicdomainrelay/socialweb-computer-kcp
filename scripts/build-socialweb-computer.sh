#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build_root="$HOME/.tmp/socialweb-computer-build"
rev="HEAD"
platforms=()
dev=0
verify=0
nocache=0
publish=""
sync_only=0

usage() {
  cat >&2 <<EOF
usage: $0 [--platform linux/amd64|linux/arm64]... [--commit REV] [--build-root DIR]
       $0 --verify-reproducible [--no-cache] [--platform P]... [--commit REV] [--build-root DIR]
       $0 --dev [--platform P]... [--build-root DIR] [--publish DIR [--sync-only]]

--dev builds ONE socialweb-computer of the working tree of this checkout (uncommitted changes
included) for the host platform (or each --platform), for live runs while iterating. It reuses the
build root's go, asset and deno caches, skips the clean checkouts and the second build, and is not
reproducible: 'socialweb-computer version' says "dev build, not reproducible" with the commit and the
tree's dirty state. It embeds every pinned asset (staged once per versions.json under
dev/assets/<goos>_<goarch>/<asset>-<sha256 of its versions.json entry>; the assets built from
this repository (fetch kind go, such as socialweb-vmexec) are built from the working tree each time) and a closure
cmd/socialweb-tsbundle builds from the working tree (its deno.lock included, unpinned to a commit;
its entries are not started here, the live run starts them).
At run time the binary runs the TypeScript from the source root it was built with, live, and seeds
each DENO_DIR from that closure: 'socialweb-computer dev restart <instance> [<app>/]<denopod>' picks
up a TypeScript change without a rebuild. Output: dev/<worktree>/<goos>_<goarch>/socialweb-computer
(and dev/<worktree>/socialweb-computer for the host platform).

  --publish DIR  also copy the host binary to DIR/bin/socialweb-computer and mirror the working tree
                 (tracked and untracked files git does not ignore) to DIR/src, readable by every
                 account (chmod a+rX), and build the binary to run the TypeScript from DIR/src: an
                 account such as test-0001 that cannot read this checkout runs from there
  --sync-only    with --publish: only mirror the working tree to DIR/src again (no build), before a
                 dev restart of a DenoPod

Without --dev it builds the release socialweb-computer binary (plan 0006 M4-BUILD) of one commit for
each release platform of internal/versions/versions.json (in parallel), once, from a clean checkout.
It stages every pinned asset with scripts/stage-assets.sh (in parallel, every tree checked against
its pin), builds the TypeScript closure once with cmd/socialweb-tsbundle -release and the pinned deno
of the build host (the closure is source plus the deno cache, the same bytes for every platform; its
entries are started on the build host), embeds it for every platform, then runs
go build -tags release with the go.mod toolchain, CGO_ENABLED=0, -trimpath, -buildvcs=true,
GOPROXY=off. Its caches are content-addressed, so a warm build gives the same bytes as a clean one:
  cache/blobs     the zstd blob of an asset, by the sha256 of its versions.json entry, stage-assets.sh,
                  assetpack and the zstd version (downloaded kinds), or of its tar stream (built kinds)
  cache/meshd     socialweb-meshd per arch, by the sha256 of rust/ (with the zig it pins) and the rustc and cargo versions
  cache/cargo-target, cache/gocache   the cargo target dir and the go build cache
  cache/closure   the TypeScript closure, by the git trees of ts/, socialweb-computer-k8s/, deno.json,
                  deno.lock, deploy/ts-entries.yaml and the Go packages of socialweb-tsbundle, the last
                  commit that touched the TypeScript (the closure's commit and SOURCE_DATE_EPOCH) and deno
  cache/tsbundle-work                 the closure's DENO_DIR fill (deno.lock-verified downloads)
A step whose key did not change is not run again.

--verify-reproducible (plan 0006 ACC6-REPRO; the release-reproducible CI job on main) builds the
commit twice, from two clean checkouts, and refuses to publish a platform whose two builds differ by
one byte. It uses the keyed caches, so it rebuilds only what changed. --no-cache builds both from
scratch, in parallel: an empty go build cache, cargo target dir and closure work dir and no blob,
meshd or closure cache; it proves the caches give the bytes a clean build gives. Every mode refuses
to publish when release/<commit> already holds a binary of a platform that differs from this build.

  --platform    a release platform; repeat for more (default: every release platform)
  --commit      the commit to build (default HEAD of this checkout; uncommitted changes are not built)
  --build-root  checkouts, caches and outputs, all kept between builds (default \$HOME/.tmp/socialweb-computer-build)

Layout under the build root:
  src/<n>                       the clean checkout of build n (1; 2 with --verify-reproducible)
  cache/                        pinned downloads (assets, gomod) and the build caches above
  work/<n>/                     the pinned deno, socialweb-tsbundle and the closure staging of build n
                                (and its empty caches with --verify-reproducible)
  out/<commit>/<n>/<goos>_<goarch>  build n: socialweb-computer, its sha256, build.json, version.json
  release/<commit>              socialweb-computer-<goos>-<goarch>, SHA256SUMS, release.json (and
                                reproducible.json with --verify-reproducible), version-<goos>-<goarch>.{json,txt}

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
    --verify-reproducible) verify=1; shift ;;
    --no-cache) nocache=1; shift ;;
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

for tool in git go jq zstd tar sha256sum unshare flock; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required on the build host"
done

mkdir -p "$build_root"
build_root="$(cd "$build_root" && pwd -P)"
case "$build_root" in
  /tmp|/tmp/*) die "--build-root $build_root is under /tmp" ;;
esac

versions="$root/internal/versions/versions.json"

if [ "$dev" = 1 ]; then
  [ "$verify" = 0 ] && [ "$nocache" = 0 ] || die "--verify-reproducible and --no-cache do not go with --dev"
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
mkdir -p "$cache/assets" "$cache/gomod" "$cache/gocache" "$cache/blobs" "$cache/meshd" "$cache/cargo-target" "$cache/closure"
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
[ "$verify" = 0 ] || mode="verify-reproducible"
[ "$nocache" = 0 ] || mode="$mode no-cache"
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

ts_paths=(ts socialweb-computer-k8s deno.json deno.lock deploy/ts-entries.yaml)

closure_key() {
  local tree="$1" deno="$2" d
  {
    echo "socialweb-tsbundle closure v1"
    sha_of "$deno"
    go env GOVERSION
    for d in "${ts_paths[@]}" go.mod go.sum; do
      echo "$d $(git -C "$tree" rev-parse "$commit:$d")"
    done
    git -C "$tree" log -1 --format=%H "$commit" -- "${ts_paths[@]}"
    (cd "$tree" && go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./cmd/socialweb-tsbundle) | sed -n "s|^$tree/||p" | sort -u |
      while read -r d; do echo "$d $(git -C "$tree" rev-parse "$commit:$d")"; done
  } | sha256sum | cut -d' ' -f1
}

build_once() {
  local n="$1" tree="$build_root/src/$1" work="$build_root/work/$1" plat goos goarch key out started deno t
  local closure_work stage_cache=() deno_blobs jobs_n
  if [ "$nocache" = 1 ]; then
    export GOCACHE="$work/gocache"
    export CARGO_TARGET_DIR="$work/cargo-target"
    closure_work="$work/closure/tsbundle"
    chmod -R u+w "$work" 2>/dev/null || true
    rm -rf "$work"
  else
    export CARGO_TARGET_DIR="$cache/cargo-target"
    export MESHD_CACHE="$cache/meshd"
    stage_cache=(--blob-cache "$cache/blobs")
    closure_work="$cache/tsbundle-work"
    for d in tree probe-bundle probe-state probe-home; do
      chmod -R u+w "$closure_work/$d" 2>/dev/null || true
      rm -rf "${closure_work:?}/$d"
    done
    chmod -R u+w "$work" 2>/dev/null || true
    rm -rf "$work"
  fi
  mkdir -p "$work/closure" "$work/deno" "$GOCACHE" "$CARGO_TARGET_DIR" "$closure_work"
  if [ "$n" != 1 ]; then
    t="$(now)"
    checkout "$tree"
    timing "build $n: checkout" "$t"
  fi

  t="$(now)"
  deno_blobs="$work/host-deno"
  (cd "$tree" && ./scripts/stage-assets.sh --cache "$cache/assets" "${stage_cache[@]}" --blobs "$deno_blobs" \
    --platform "$hostplat" --asset deno)
  zstd -dc "$deno_blobs/${hostplat/\//_}/deno.tar.zst" | tar -x -C "$work/deno"
  deno="$work/deno/deno"
  [ -x "$deno" ] || die "the staged deno blob for $hostplat holds no deno"
  timing "build $n: host deno" "$t"

  jobs_n=0
  (
    t="$(now)"
    local ckey="" cdir=""
    if [ "$nocache" = 0 ]; then
      ckey="$(closure_key "$tree" "$deno")"
      cdir="$cache/closure/$ckey"
    fi
    if [ -n "$cdir" ] && [ -f "$cdir/tsbundle.json" ]; then
      mkdir -p "$work/closure/out"
      cp "$cdir/closure.tar.zst" "$cdir/manifest.json.zst" "$work/closure/out/"
      cp "$cdir/tsbundle.json" "$work/closure/tsbundle.json"
      say "build $n: TypeScript closure tree $(jq -r .treeSha256 "$cdir/tsbundle.json") reused by its source key $ckey"
    else
      say "build $n: TypeScript closure (pinned deno $hostplat)"
      (cd "$tree" && go build -trimpath -o "$work/closure/socialweb-tsbundle" ./cmd/socialweb-tsbundle)
      (cd "$tree" && "$work/closure/socialweb-tsbundle" -source-root "$tree" -release -deno "$deno" \
        -work "$closure_work" -out "$work/closure/out" >"$work/closure/tsbundle.json" 2>"$work/closure/tsbundle.log") \
        || { tail -20 "$work/closure/tsbundle.log" >&2; die "build $n: the closure build failed"; }
      if [ -n "$cdir" ]; then
        local outside
        outside="$(jq -r --argjson keep "$(printf '%s\n' "${ts_paths[@]}" | jq -R . | jq -s .)" \
          '[.repos[].prefixes[] | select(. as $p | $keep | any(. as $k | $p == $k or ($p | startswith($k + "/"))) | not)] | join(" ")' \
          "$work/closure/tsbundle.json")"
        if [ -n "$outside" ]; then
          say "warn: the closure stages $outside, outside the closure key's paths; not cached"
        else
          mkdir -p "$cache/closure/.tmp.$BASHPID"
          cp "$work/closure/out/closure.tar.zst" "$work/closure/out/manifest.json.zst" "$work/closure/tsbundle.json" "$cache/closure/.tmp.$BASHPID/"
          mv -T "$cache/closure/.tmp.$BASHPID" "$cdir" 2>/dev/null || rm -rf "$cache/closure/.tmp.$BASHPID"
        fi
      fi
    fi
    timing "build $n: TypeScript closure" "$t"
  ) &
  jobs_n=$((jobs_n + 1))
  (
    t="$(now)"
    local stage_args=() p
    for p in "${platforms[@]}"; do stage_args+=(--platform "$p"); done
    say "build $n: stage assets"
    (cd "$tree" && ./scripts/stage-assets.sh --cache "$cache/assets" "${stage_cache[@]}" "${stage_args[@]}")
    timing "build $n: stage assets" "$t"
  ) &
  jobs_n=$((jobs_n + 1))
  wait_all "build $n: closure or asset staging" "$jobs_n"

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
      cp "$work/closure/out/"* "$tree/internal/tsbundle/closure/$key/"
      say "build $n $plat: go build"
      (cd "$tree" && GOOS="$goos" GOARCH="$goarch" go build -tags release -trimpath -buildvcs=true \
        -ldflags="-s -w -buildid= ${version:+-X github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp.Version=$version}" \
        -o "$out/socialweb-computer" ./cmd/socialweb-computer)
      (cd "$out" && sha256sum socialweb-computer >socialweb-computer.sha256)
      "$out/socialweb-computer" version --json >"$out/version.json"
      "$out/socialweb-computer" version >"$out/version.txt"
      jq -n --arg commit "$commit" --arg platform "$plat" --argjson build "$n" --arg go "$have_go" --arg mode "$mode" \
        --arg started "$started" --arg finished "$(utc)" --arg sha256 "$(sha_of "$out/socialweb-computer")" \
        --argjson closure "$(jq --arg p "$hostplat" '{treeSha256, builtAndStartedOn: $p, entries: [.entries[] | {name, ok}]}' "$work/closure/tsbundle.json")" \
        '{commit: $commit, platform: $platform, build: $build, mode: $mode, go: $go, startedUtc: $started, finishedUtc: $finished, sha256: $sha256, typescriptClosure: $closure}' \
        >"$out/build.json"
      say "build $n $plat: sha256 $(sha_of "$out/socialweb-computer")"
      timing "build $n $plat: go build" "$t"
    ) &
    jobs_n=$((jobs_n + 1))
  done
  wait_all "build $n: go build" "$jobs_n"
  [ -z "$(git -C "$tree" status --porcelain)" ] || die "build $n left $tree dirty"
}

t="$(now)"
if [ "$verify" = 1 ] && [ "$nocache" = 1 ]; then
  (build_once 1 2>&1 | sed -u 's/^/[build 1] /' >&2) &
  (build_once 2 2>&1 | sed -u 's/^/[build 2] /' >&2) &
  wait_all "a clean build" 2
elif [ "$verify" = 1 ]; then
  build_once 1
  build_once 2
else
  build_once 1
fi
timing "builds" "$t"

release="$build_root/release/$commit"
failed=0
records=()
for plat in "${platforms[@]}"; do
  key="${plat/\//_}"
  name="socialweb-computer-${plat/\//-}"
  a="$build_root/out/$commit/1/$key"
  sa="$(sha_of "$a/socialweb-computer")"
  if [ -f "$release/SHA256SUMS" ]; then
    prior="$(awk -v n="$name" '$2 == n { print $1 }' "$release/SHA256SUMS")"
    if [ -n "$prior" ] && [ "$prior" != "$sa" ]; then
      say "MISMATCH $plat: release/$commit holds $prior from an earlier build, this build made $sa"
      failed=1
    fi
  fi
  if [ "$verify" = 1 ]; then
    b="$build_root/out/$commit/2/$key"
    sb="$(sha_of "$b/socialweb-computer")"
    records+=("$(jq -n --arg p "$plat" --arg sa "$sa" --arg sb "$sb" --slurpfile ja "$a/build.json" --slurpfile jb "$b/build.json" \
      '{platform: $p, identical: ($sa == $sb), sha256: (if $sa == $sb then $sa else null end), builds: [$ja[0], $jb[0]]}')")
    if [ "$sa" != "$sb" ]; then
      say "NOT REPRODUCIBLE $plat: build 1 $sa, build 2 $sb ($a, $b)"
      failed=1
    else
      say "reproducible $plat sha256 $sa"
    fi
  fi
done
if [ "$failed" = 1 ]; then
  rm -rf "$release"
  die "refusing to publish release/$commit"
fi

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
if [ "$verify" = 1 ]; then
  printf '%s\n' "${records[@]}" | jq -s --arg commit "$commit" '{commit: $commit, platforms: .}' >"$release/reproducible.json"
  jq -c '.platforms[] | {platform, identical, sha256}' "$release/reproducible.json" >&2
fi
timing "total" "$started_all"
say "timings:"
cat "$timings" >&2
rm -f "$timings"
