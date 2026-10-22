#!/usr/bin/env bash
set -euo pipefail

if [ $# -lt 4 ]; then
  echo "usage: scripts/build-socialweb-computer.sh --dev [--platform P]... [--build-root DIR] [--publish DIR [--sync-only]]" >&2
  exit 2
fi
root="$1"
build_root="$2"
publish="$3"
sync_only="$4"
shift 4
platforms=("$@")

die() { echo "build-socialweb-computer --dev: $*" >&2; exit 1; }
say() { echo "build-socialweb-computer --dev: $(date -u +%Y-%m-%dT%H:%M:%SZ) $*" >&2; }
sha_of() { sha256sum "$1" | cut -d' ' -f1; }
started_s="$(date +%s.%N)"
elapsed() { awk -v a="$started_s" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f", b - a }'; }

command -v rsync >/dev/null 2>&1 || die "rsync is required for --dev"
command -v flock >/dev/null 2>&1 || die "flock is required for --dev"

name="$(basename "$root")"
dev_root="$build_root/dev/$name"
versions="$root/internal/versions/versions.json"

case "$publish" in
  "") [ "$sync_only" = 0 ] || die "--sync-only needs --publish DIR" ;;
  /*) ;;
  *) die "--publish $publish is not an absolute path" ;;
esac

mirror() {
  local dst="$1/src" list
  mkdir -p "$dst" "$dev_root"
  list="$dev_root/mirror.include"
  git -C "$root" ls-files -z -co --exclude-standard \
    | tr '\0' '\n' \
    | awk '{ n = split($0, p, "/"); d = ""; for (i = 1; i < n; i++) { d = d "/" p[i]; if (!(d in seen)) { seen[d] = 1; print d "/" } } print "/" $0 }' \
    >"$list"
  rsync -a --delete --delete-excluded --include-from="$list" --exclude='*' "$root/" "$dst/"
  chmod a+rx "$1"
  chmod -R a+rX "$dst"
  say "mirrored $(grep -vc '/$' "$list") files of $root to $dst ($(elapsed)s)"
}

if [ "$sync_only" = 1 ]; then
  mirror "$publish"
  exit 0
fi

want_go="$(awk '$1=="toolchain"{print $2; exit}' "$root/go.mod")"
[ -n "$want_go" ] || want_go="go$(awk '$1=="go"{print $2; exit}' "$root/go.mod")"
cache="$build_root/cache"
mkdir -p "$cache/assets" "$cache/gomod" "$cache/gocache" "$build_root/tmp" "$dev_root"
export TMPDIR="$build_root/tmp"
export GOTMPDIR="$build_root/tmp"
export GOMODCACHE="$cache/gomod"
export GOCACHE="$cache/gocache"
export GOFLAGS=-mod=readonly
export CGO_ENABLED=0
unset GOOS GOARCH GOPATH GOROOT SOURCE_DATE_EPOCH
goroot="$(GOTOOLCHAIN="$want_go" go env GOROOT)" || die "cannot get the pinned toolchain $want_go"
export PATH="$goroot/bin:$PATH"
export GOTOOLCHAIN=local
have_go="$(go env GOVERSION)"
[ "$have_go" = "$want_go" ] || die "the toolchain at $goroot is $have_go; go.mod pins $want_go"

hostplat="$(go env GOHOSTOS)/$(go env GOHOSTARCH)"
if [ ${#platforms[@]} -eq 0 ]; then
  platforms=("$hostplat")
fi
for plat in "${platforms[@]}"; do
  [ "$(jq -r --arg p "$plat" '.platforms[$p].status // ""' "$versions")" = "release" ] || die "$plat is not a release platform of versions.json"
done
if [ -n "$publish" ] && [[ " ${platforms[*]} " != *" $hostplat "* ]]; then
  die "--publish copies the host binary; add --platform $hostplat"
fi
stage_plats=("${platforms[@]}")
[[ " ${stage_plats[*]} " == *" $hostplat "* ]] || stage_plats+=("$hostplat")

commit="$(git -C "$root" rev-parse HEAD)"
changed="$(git -C "$root" status --porcelain | grep -c . || true)"
dirty=false
[ "$changed" = 0 ] || dirty=true
say "worktree $root at $commit (dirty $dirty, $changed changed paths), platforms ${platforms[*]}, go $have_go"

shared_blobs="$build_root/dev/assets"
own_blobs="$dev_root/blobs"
mkdir -p "$shared_blobs" "$own_blobs"
from_tree() {
  [ "$(jq -r --arg a "$1" --arg p "$2" '.assets[$a].fetch[$p].kind // ""' "$versions")" = go ]
}
blob_of() {
  local plat="$1" asset="$2"
  if from_tree "$asset" "$plat"; then
    echo "$own_blobs/${plat/\//_}/$asset.tar.zst"
    return
  fi
  echo "$shared_blobs/${plat/\//_}/$asset-$(jq -cS --arg a "$asset" '.assets[$a]' "$versions" | sha256sum | cut -c1-16).tar.zst"
}
{
  flock 9
  for plat in "${stage_plats[@]}"; do
    key="${plat/\//_}"
    missing=()
    loose=()
    for asset in $(jq -r --arg p "$plat" '.platforms[$p].assets[]' "$versions"); do
      from_tree "$asset" "$plat" && continue
      [ -f "$(blob_of "$plat" "$asset")" ] && continue
      if [ "$asset" = socialweb-computer-k8s-downloads ] && [ "$(. /etc/os-release && echo "$ID")" != fedora ]; then
        loose+=(--asset "$asset")
      else
        missing+=(--asset "$asset")
      fi
    done
    if [ ${#missing[@]} -gt 0 ] || [ ${#loose[@]} -gt 0 ]; then
      say "stage ${missing[*]} ${loose[*]} for $plat into $shared_blobs (once per versions.json entry of the asset)"
      rm -rf "$shared_blobs/.stage"
      [ ${#missing[@]} -eq 0 ] || "$root/scripts/stage-assets.sh" --cache "$cache/assets" --blobs "$shared_blobs/.stage" --platform "$plat" "${missing[@]}"
      # ponytail: the meshd bytes follow the compiler packages, so only Fedora matches the pin; elsewhere the downloads asset is staged unpinned (the gateway's /downloads would not match the pin), add a pinned Fedora build when a non-Fedora host needs the web app downloads
      [ ${#loose[@]} -eq 0 ] || "$root/scripts/stage-assets.sh" --cache "$cache/assets" --blobs "$shared_blobs/.stage" --platform "$plat" "${loose[@]}" --print-pins >/dev/null
      mkdir -p "$shared_blobs/$key"
      for f in "$shared_blobs/.stage/$key"/*.tar.zst; do
        mv -f "$f" "$(blob_of "$plat" "$(basename "$f" .tar.zst)")"
      done
      rm -rf "$shared_blobs/.stage"
    fi
  done
} 9>"$shared_blobs/.lock"
for plat in "${platforms[@]}"; do
  tree_assets=()
  built=()
  for asset in $(jq -r --arg p "$plat" '.platforms[$p].assets[]' "$versions"); do
    from_tree "$asset" "$plat" || continue
    spec="$(jq -c --arg a "$asset" --arg p "$plat" '.assets[$a].fetch[$p]' "$versions")"
    probe="$dev_root/tree-bin/${plat/\//_}/$asset"
    mkdir -p "$(dirname "$probe")"
    (cd "$root/$(echo "$spec" | jq -r '.module // "."')" && GOOS="${plat%/*}" GOARCH="${plat#*/}" go build -trimpath \
      -buildvcs=false -ldflags='-s -w -buildid=' -o "$probe" "$(echo "$spec" | jq -r .package)")
    sum="$(sha_of "$probe")"
    if [ -f "$(blob_of "$plat" "$asset")" ] && [ "$(cat "$probe.staged" 2>/dev/null)" = "$sum" ]; then
      continue
    fi
    tree_assets+=(--asset "$asset")
    built+=("$probe:$sum")
  done
  [ ${#tree_assets[@]} -gt 0 ] || continue
  say "stage ${tree_assets[*]} for $plat (built from the working tree, changed)"
  "$root/scripts/stage-assets.sh" --cache "$cache/assets" --blobs "$own_blobs" --platform "$plat" \
    "${tree_assets[@]}" --print-pins >"$dev_root/tree-assets-pins.json" 2>"$dev_root/tree-assets-stage.log" \
    || die "staging ${tree_assets[*]} failed: $(tail -3 "$dev_root/tree-assets-stage.log")"
  for b in "${built[@]}"; do
    echo "${b##*:}" >"${b%:*}.staged"
  done
  if grep -q 'versions.json pins' "$dev_root/tree-assets-stage.log"; then
    say "warn: $(grep 'versions.json pins' "$dev_root/tree-assets-stage.log" | cut -d' ' -f2 | tr '\n' ' ')built from the working tree differ from their versions.json pins (a release build refuses them)"
  fi
done
say "assets staged ($(elapsed)s)"

hostkey="${hostplat/\//_}"
source_root="$root"
[ -z "$publish" ] || source_root="$publish/src"
stamp="github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp"
ldflags="-s -w -X $stamp.Kind=dev -X $stamp.SourceRoot=$source_root -X $stamp.Commit=$commit -X $stamp.Dirty=$dirty -X $stamp.ChangedPaths=$changed -X $stamp.BuiltUTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

for plat in "${platforms[@]}"; do
  goos="${plat%/*}"
  goarch="${plat#*/}"
  key="${goos}_${goarch}"
  out="$dev_root/$key"
  mkdir -p "$out"
  overlay="$dev_root/overlay-$key.json"
  {
    for asset in $(jq -r --arg p "$plat" '.platforms[$p].assets[]' "$versions"); do
      jq -n --arg k "$root/internal/assets/blobs/$key/$asset.tar.zst" --arg v "$(blob_of "$plat" "$asset")" '{($k): $v}'
    done
  } | jq -s '{Replace: add}' >"$overlay"
  say "go build $plat"
  (cd "$root" && GOOS="$goos" GOARCH="$goarch" go build -tags release -trimpath -buildvcs=true -overlay "$overlay" \
    -ldflags="$ldflags" -o "$out/socialweb-computer.new" ./cmd/socialweb-computer)
  mv -f "$out/socialweb-computer.new" "$out/socialweb-computer"
  if [ "$plat" = "$hostplat" ]; then
    ln -sfn "$key/socialweb-computer" "$dev_root/socialweb-computer"
  fi
  say "built $out/socialweb-computer ($(elapsed)s)"
done

if [ -n "$publish" ]; then
  mkdir -p "$publish/bin"
  cp -f "$dev_root/$hostkey/socialweb-computer" "$publish/bin/socialweb-computer.new"
  mv -f "$publish/bin/socialweb-computer.new" "$publish/bin/socialweb-computer"
  chmod a+rx "$publish/bin" "$publish/bin/socialweb-computer"
  mirror "$publish"
  say "published $publish/bin/socialweb-computer, source from $publish/src"
fi
if [[ " ${platforms[*]} " == *" $hostplat "* ]]; then
  "$dev_root/$hostkey/socialweb-computer" version | tee "$dev_root/$hostkey/version.txt"
fi
say "done in $(elapsed)s"
