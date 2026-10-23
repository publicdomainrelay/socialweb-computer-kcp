#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
versions="$root/internal/versions/versions.json"
blobs="$root/internal/assets/blobs"
cache=""
print_pins=0
update_pins=0
blob_cache=""
jobs=6
platforms=()
only=()

usage() {
  echo "usage: $0 [--platform linux/amd64|linux/arm64]... [--asset NAME]... [--cache DIR] [--blobs DIR] [--blob-cache DIR] [--jobs N] [--print-pins|--update-pins]" >&2
  echo "  downloads every pinned asset of versions.json, verifies its sha256 (and the upstream signature" >&2
  echo "  where one is published), repacks it as internal/assets/blobs/<goos>_<goarch>/<asset>.tar.zst and" >&2
  echo "  checks the unpacked tree against its treeSha256 pin. --print-pins prints the measured pins and" >&2
  echo "  stages even when a tree pin differs. --update-pins does the same for the assets built from this" >&2
  echo "  repository (fetch kind go or script) or built here from pinned upstream source (fetch kind gosrc)" >&2
  echo "  and writes their new pins into versions.json; pins of" >&2
  echo "  downloaded assets are never changed. --cache keeps" >&2
  echo "  the downloads (keyed by sha256) in DIR instead of internal/assets/blobs/.cache. --blobs writes" >&2
  echo "  <goos>_<goarch>/<asset>.tar.zst under DIR instead of internal/assets/blobs (the --dev build's cache)." >&2
  echo "  --blob-cache keeps every zstd blob in DIR: an asset of pinned inputs (kind file, oci, debs, gosrc) by" >&2
  echo "  the sha256 of its versions.json entry, this script, assetpack and the zstd version and flags (a hit" >&2
  echo "  skips its download or build, verification and repack); an asset built from this checkout (kind go, script) is rebuilt" >&2
  echo "  and checked against its pin every time, and only its zstd step is reused, keyed by the sha256 of" >&2
  echo "  its tar stream. --jobs stages up to N assets at once (default 6)." >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --platform) platforms+=("$2"); shift 2 ;;
    --asset) only+=("$2"); shift 2 ;;
    --cache) cache="$2"; shift 2 ;;
    --blobs) blobs="$2"; shift 2 ;;
    --blob-cache) blob_cache="$2"; shift 2 ;;
    --jobs) jobs="$2"; shift 2 ;;
    --print-pins) print_pins=1; shift ;;
    --update-pins) update_pins=1; shift ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done
[ "$print_pins" = 0 ] || [ "$update_pins" = 0 ] || { echo "stage-assets: --print-pins and --update-pins are exclusive" >&2; exit 2; }

for tool in curl jq gpg ar tar zstd sha256sum unzip go xz cmp python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "stage-assets: $tool is required on the build host" >&2; exit 2; }
done

if [ ${#platforms[@]} -eq 0 ]; then
  mapfile -t platforms < <(jq -r '.platforms | to_entries[] | select(.value.status == "release") | .key' "$versions")
fi

cache="${cache:-$blobs/.cache}"
mkdir -p "$cache" "$blobs/.work"
work="$(mktemp -d "$blobs/.work/run.XXXXXX")"
trap 'chmod -R u+w "$work" 2>/dev/null; rm -rf "$work"' EXIT
pins_dir=""
if [ "$update_pins" = 1 ]; then
  pins_dir="$work/pins"
  mkdir -p "$pins_dir"
fi

die() { echo "stage-assets: $*" >&2; exit 1; }
say() { echo "stage-assets: $*" >&2; }

sha_of() { sha256sum "$1" | cut -d' ' -f1; }
dl_args=(-fSL --retry 3 --connect-timeout 10 --speed-limit 10000 --speed-time 20 --progress-bar)
api_args=(-fsSL --retry 3 --connect-timeout 10 --speed-limit 10000 --speed-time 20)

fetch_pinned() {
  local url="$1" want="$2" out="$cache/$2"
  if [ -f "$out" ] && [ "$(sha_of "$out")" = "$want" ]; then
    echo "$out"
    return
  fi
  local part="$out.part.$BASHPID" got
  curl "${dl_args[@]}" -o "$part" "$url" || { rm -f "$part"; die "download failed: $url"; }
  got="$(sha_of "$part")"
  if [ "$got" != "$want" ]; then
    rm -f "$part"
    die "sha256 mismatch for $url: got $got, pinned $want"
  fi
  mv -f "$part" "$out"
  echo "$out"
}

fetch_fresh() {
  local url="$1" out="$2"
  curl "${dl_args[@]}" -o "$out" "$url" || die "download failed: $url"
}

key_ring() {
  local url="$1" fpr="$2" ring="$GNUPGHOME/keys/$2.gpg"
  if [ ! -f "$ring" ]; then
    mkdir -p "$GNUPGHOME/keys"
    fetch_fresh "$url" "$GNUPGHOME/keys/$fpr.asc"
    gpg --batch --quiet --no-default-keyring --keyring "$ring" --import "$GNUPGHOME/keys/$fpr.asc" 2>/dev/null
    gpg --batch --no-default-keyring --keyring "$ring" --with-colons --list-keys \
      | awk -F: '$1=="pub"{p=1;next} p&&$1=="fpr"{print $10; p=0}' | grep -qx "$fpr" \
      || die "key from $url does not carry primary fingerprint $fpr"
  fi
  echo "$ring"
}

gpg_verify() {
  local ring="$1" fpr="$2" sig="$3" data="${4:-}" out="${5:-}" status
  if [ -n "$data" ]; then
    status="$(gpg --batch --no-default-keyring --keyring "$ring" --trust-model always --status-fd 1 --verify "$sig" "$data" 2>/dev/null || true)"
  else
    status="$(gpg --batch --no-default-keyring --keyring "$ring" --trust-model always --status-fd 1 --output "$out" --decrypt "$sig" 2>/dev/null || true)"
  fi
  echo "$status" | awk '$2=="VALIDSIG"{print $NF}' | grep -qx "$fpr" || die "signature on $sig does not verify with $fpr"
}

check_checksums() {
  local f="$1" name="$2" want="$3"
  if awk -v n="$name" -v w="$want" '{s=$NF; sub(/^\*/,"",s); sub(/^.*\//,"",s)} $1==w && (s==n || NF==1){ok=1} END{exit !ok}' "$f"; then
    return
  fi
  die "checksum file for $name does not list the pinned $want"
}

copy_members() {
  local from="$1" to="$2" members="$3" src dst
  while IFS=$'\t' read -r src dst; do
    [ -n "$src" ] || continue
    if [ ! -e "$from/$src" ]; then
      (cd "$from" && find . | head -40) >&2
      die "member $src is not in the unpacked archive (listing above)"
    fi
    mkdir -p "$(dirname "$to/$dst")"
    cp -L "$from/$src" "$to/$dst"
    chmod 0755 "$to/$dst"
  done < <(echo "$members" | jq -r 'to_entries[] | "\(.key)\t\(.value)"')
}

stage_file() {
  local spec="$1" tree="$2" x="$3" url want archive file
  url="$(echo "$spec" | jq -r .url)"
  want="$(echo "$spec" | jq -r .sha256)"
  archive="$(echo "$spec" | jq -r .archive)"
  file="$(fetch_pinned "$url" "$want")"
  if [ "$(echo "$spec" | jq -r '.checksums // empty')" != "" ]; then
    local cs name sig
    cs="$x/checksums"
    fetch_fresh "$(echo "$spec" | jq -r .checksums.url)" "$cs"
    name="$(echo "$spec" | jq -r .checksums.name)"
    sig="$(echo "$spec" | jq -r '.checksums.signature // empty')"
    if [ -n "$sig" ]; then
      local fpr ring
      fpr="$(echo "$spec" | jq -r .checksums.fingerprint)"
      ring="$(key_ring "$(echo "$spec" | jq -r .checksums.key)" "$fpr")"
      fetch_fresh "$sig" "$x/checksums.sig"
      gpg_verify "$ring" "$fpr" "$x/checksums.sig" "$cs"
    fi
    check_checksums "$cs" "$name" "$want"
  fi
  mkdir -p "$x/u"
  case "$archive" in
    tar.gz) tar -xzf "$file" -C "$x/u" ;;
    zip) unzip -q "$file" -d "$x/u" ;;
    raw) cp "$file" "$x/u/$(echo "$spec" | jq -r '.members | keys[0]')" ;;
    *) die "unknown archive kind $archive" ;;
  esac
  copy_members "$x/u" "$tree" "$(echo "$spec" | jq -c .members)"
}

stage_oci() {
  local spec="$1" tree="$2" x="$3" plat="$4" reg repo index manifest layer tok f
  reg="$(echo "$spec" | jq -r .registry)"
  repo="$(echo "$spec" | jq -r .repository)"
  index="$(echo "$spec" | jq -r .index)"
  manifest="$(echo "$spec" | jq -r .manifest)"
  layer="$(echo "$spec" | jq -r .layer)"
  tok="$(curl "${api_args[@]}" "https://$reg/token?scope=repository:$repo:pull" | jq -r .token)"
  local accept="application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
  curl "${api_args[@]}" -H "Authorization: Bearer $tok" -H "Accept: $accept" -o "$x/index.json" "https://$reg/v2/$repo/manifests/$index"
  [ "sha256:$(sha_of "$x/index.json")" = "$index" ] || die "$repo index does not hash to $index"
  jq -e --arg d "$manifest" --arg os "${plat%/*}" --arg a "${plat#*/}" \
    '.manifests[] | select(.digest == $d and .platform.os == $os and .platform.architecture == $a)' "$x/index.json" >/dev/null \
    || die "$repo index $index does not name $manifest for $plat"
  curl "${api_args[@]}" -H "Authorization: Bearer $tok" -H "Accept: $accept" -o "$x/manifest.json" "https://$reg/v2/$repo/manifests/$manifest"
  [ "sha256:$(sha_of "$x/manifest.json")" = "$manifest" ] || die "$repo manifest does not hash to $manifest"
  jq -e --arg d "$layer" '.layers[] | select(.digest == $d)' "$x/manifest.json" >/dev/null || die "$manifest does not name layer $layer"
  f="$cache/${layer#sha256:}"
  if [ ! -f "$f" ] || [ "sha256:$(sha_of "$f")" != "$layer" ]; then
    local part="$f.part.$BASHPID"
    curl "${dl_args[@]}" -H "Authorization: Bearer $tok" -o "$part" "https://$reg/v2/$repo/blobs/$layer"
    [ "sha256:$(sha_of "$part")" = "$layer" ] || { rm -f "$part"; die "layer does not hash to $layer"; }
    mv -f "$part" "$f"
  fi
  mkdir -p "$x/u"
  tar -xzf "$f" -C "$x/u"
  copy_members "$x/u" "$tree" "$(echo "$spec" | jq -c .members)"
}

stage_script() {
  local spec="$1" tree="$2" x="$3" script want out
  script="$root/$(echo "$spec" | jq -r .script)"
  want="$(echo "$spec" | jq -r '.sha256 // ""')"
  mkdir -p "$x/u"
  "$script" --out "$x/u" >&2
  out="$x/u/$(echo "$spec" | jq -r '.members | keys[0]')"
  if [ -n "$want" ] && [ "$(sha_of "$out")" != "$want" ]; then
    [ "$update_pins" = 1 ] || die "$script built $(sha_of "$out"), pinned $want"
  fi
  copy_members "$x/u" "$tree" "$(echo "$spec" | jq -c .members)"
}

stage_go() {
  local spec="$1" tree="$2" x="$3" plat="$4" pkg module want out
  pkg="$(echo "$spec" | jq -r .package)"
  module="$(echo "$spec" | jq -r '.module // "."')"
  want="$(echo "$spec" | jq -r '.sha256 // ""')"
  out="$x/u/$(echo "$spec" | jq -r '.members | keys[0]')"
  mkdir -p "$x/u"
  (cd "$root/$module" && CGO_ENABLED=0 GOOS="${plat%/*}" GOARCH="${plat#*/}" go build -trimpath -buildvcs=false \
    -ldflags='-s -w -buildid=' -o "$out" "$pkg")
  say "$module $pkg $plat binary sha256 $(sha_of "$out")"
  if [ -n "$want" ] && [ "$(sha_of "$out")" != "$want" ]; then
    echo "stage-assets: $module $pkg $plat built $(sha_of "$out"), pinned $want" >&2
    [ "$print_pins" = 1 ] || [ "$update_pins" = 1 ] || die "$module $pkg $plat: sha256 differs from the pin"
  fi
  copy_members "$x/u" "$tree" "$(echo "$spec" | jq -c .members)"
}

gosrc_proxy="https://proxy.golang.org"

stage_gosrc() {
  local asset="$1" spec="$2" tree="$3" x="$4" plat="$5" src url want commit toolchain pkg file comment goroot ldflags p k out
  src="$(jq -c --arg a "$asset" '.assets[$a].gosrc' "$versions")"
  [ "$src" != "null" ] || die "$asset: fetch kind gosrc needs an asset-level gosrc object"
  url="$(echo "$src" | jq -r .url)"
  want="$(echo "$src" | jq -r .sha256)"
  commit="$(echo "$src" | jq -r .commit)"
  toolchain="$(echo "$src" | jq -r .toolchain)"
  pkg="$(echo "$src" | jq -r .package)"
  file="$(fetch_pinned "$url" "$want")"
  comment="$(python3 -I -c 'import sys, tarfile; print(tarfile.open(sys.argv[1]).pax_headers.get("comment", ""))' "$file")"
  [ "$comment" = "$commit" ] || die "$url: the archive names commit '$comment', pinned $commit"
  mkdir -p "$x/src" "$x/u"
  tar -xzf "$file" -C "$x/src" --strip-components=1
  ldflags="-s -w -buildid="
  for p in $(echo "$src" | jq -r '.versionPackages[]'); do
    while IFS=$'\t' read -r k v; do
      ldflags="$ldflags -X $p.$k=$v"
    done < <(echo "$src" | jq -r '.versionVars | to_entries[] | "\(.key)\t\(.value)"')
  done
  out="$x/u/$(echo "$spec" | jq -r '.members | keys[0]')"
  goroot="$(GOTOOLCHAIN="$toolchain" GOPROXY="$gosrc_proxy" GOSUMDB=sum.golang.org go env GOROOT)" || die "cannot get the toolchain $toolchain"
  (cd "$x/src" && PATH="$goroot/bin:$PATH" GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly GOPROXY="$gosrc_proxy" \
    GOSUMDB=sum.golang.org GONOSUMDB= GOPRIVATE= GONOPROXY= GOINSECURE= CGO_ENABLED=0 GOOS="${plat%/*}" GOARCH="${plat#*/}" \
    go build -trimpath -buildvcs=false -ldflags="$ldflags" -o "$out" "$pkg")
  [ "$(PATH="$goroot/bin:$PATH" GOTOOLCHAIN=local go version "$out" | awk '{print $2}')" = "$toolchain" ] || die "$asset $plat was not built by $toolchain"
  say "$asset $plat built from $commit with $toolchain, binary sha256 $(sha_of "$out")"
  copy_members "$x/u" "$tree" "$(echo "$spec" | jq -c .members)"
}

deb_arch() { case "$1" in linux/amd64) echo amd64 ;; linux/arm64) echo arm64 ;; *) die "no deb arch for $1" ;; esac; }

fetch_index() {
  local dir="$1" name="$2" sum="$3" pf
  if [ -f "$cache/$sum" ] && [ "$(sha_of "$cache/$sum")" = "$sum" ]; then
    say "index $name: cached by sha256 $sum"
    echo "$cache/$sum"
    return
  fi
  say "index $name: fetching by-hash ${dir}by-hash/SHA256/$sum"
  if pf="$(fetch_pinned "${dir}by-hash/SHA256/$sum" "$sum")"; then
    echo "$pf"
    return
  fi
  say "index $name: by-hash path unavailable, fetching ${dir}$name"
  fetch_pinned "${dir}$name" "$sum"
}

repo_index() {
  local asset="$1" repo="$2" plat="$3" x="$4" arch base comp fpr ring suite idx
  arch="$(deb_arch "$plat")"
  base="$(jq -r --arg a "$asset" --arg r "$repo" --arg p "$plat" '.assets[$a].repos[$r].base[$p]' "$versions")"
  comp="$(jq -r --arg a "$asset" --arg r "$repo" '.assets[$a].repos[$r].component' "$versions")"
  fpr="$(jq -r --arg a "$asset" --arg r "$repo" '.assets[$a].repos[$r].fingerprint' "$versions")"
  ring="$(key_ring "$(jq -r --arg a "$asset" --arg r "$repo" '.assets[$a].repos[$r].key' "$versions")" "$fpr")"
  : >"$x/$repo.packages"
  for suite in $(jq -r --arg a "$asset" --arg r "$repo" '.assets[$a].repos[$r].suites[]' "$versions"); do
    local found="" attempt pf="" sum
    for attempt in 1 2 3 4 5 6; do
      fetch_fresh "${base}dists/$suite/InRelease" "$x/$suite.InRelease"
      gpg_verify "$ring" "$fpr" "$x/$suite.InRelease" "" "$x/$suite.Release"
      for ext in xz gz; do
        idx="$comp/binary-$arch/Packages.$ext"
        sum="$(awk -v f="$idx" '/^SHA256:/{s=1;next} /^[^ ]/{s=0} s && $3==f {print $1}' "$x/$suite.Release" | head -1)"
        [ -n "$sum" ] || continue
        if pf="$(fetch_index "${base}dists/$suite/$comp/binary-$arch/" "${idx##*/}" "$sum")"; then
          case "$ext" in xz) xz -dc "$pf" >>"$x/$repo.packages" ;; gz) gzip -dc "$pf" >>"$x/$repo.packages" ;; esac
          echo >>"$x/$repo.packages"
          found=1
        fi
        break
      done
      [ -z "$found" ] || break
      say "$suite index does not match its signed InRelease (the mirror is syncing); retry $attempt of 6 in 20 s"
      sleep 20
    done
    [ -n "$found" ] || die "$suite: no Packages index matching its signed InRelease after 6 tries, or InRelease names no $comp/binary-$arch/Packages index"
  done
  echo "$base"
}

stage_debs() {
  local asset="$1" spec="$2" tree="$3" x="$4" plat="$5" arch repo base n i pkg ver pool want fname deb
  arch="$(deb_arch "$plat")"
  mkdir -p "$x/root"
  declare -A bases=()
  for repo in $(jq -r --arg a "$asset" '.assets[$a].repos | keys[]' "$versions"); do
    bases[$repo]="$(repo_index "$asset" "$repo" "$plat" "$x")"
  done
  n="$(jq -r --arg a "$asset" '.assets[$a].debs | length' "$versions")"
  for ((i = 0; i < n; i++)); do
    read -r pkg ver repo pool want < <(jq -r --arg a "$asset" --arg p "$plat" --argjson i "$i" \
      '.assets[$a].debs[$i] | "\(.package) \(.version) \(.repo) \(.pool) \(.sha256[$p])"' "$versions")
    fname="$(awk -v RS= -v p="$pkg" -v v="$ver" '
      { pk=""; vv=""; fn=""; sh="";
        n=split($0, L, "\n");
        for (j=1;j<=n;j++) { split(L[j], kv, ": ");
          if (kv[1]=="Package") pk=kv[2]; else if (kv[1]=="Version") vv=kv[2];
          else if (kv[1]=="Filename") fn=kv[2]; else if (kv[1]=="SHA256") sh=kv[2] }
        if (pk==p && vv==v) { print fn " " sh; exit } }' "$x/$repo.packages")"
    if [ -n "$fname" ]; then
      [ "${fname#* }" = "$want" ] || die "$pkg $ver: the signed $repo index names sha256 ${fname#* }, pinned $want"
      deb="$(fetch_pinned "${bases[$repo]}${fname% *}" "$want")"
    else
      say "warn: $pkg $ver is no longer in the signed $repo index for $arch (superseded); verified by its pinned sha256 alone"
      local nover="${ver#*:}"
      deb="$(fetch_pinned "${bases[$repo]}$pool${pkg}_${nover}_$arch.deb" "$want" 2>/dev/null)" \
        || deb="$(fetch_pinned "${bases[$repo]}$pool${pkg}_${nover}_all.deb" "$want")"
    fi
    rm -rf "$x/deb" && mkdir -p "$x/deb"
    (cd "$x/deb" && ar x "$deb")
    tar -xf "$x"/deb/data.tar.* -C "$x/root"
  done
  case "$(echo "$spec" | jq -r .layout)" in
    members) copy_members "$x/root" "$tree" "$(echo "$spec" | jq -c .members)" ;;
    pgdg-tree)
      local triplet
      case "$arch" in amd64) triplet=x86_64-linux-gnu ;; arm64) triplet=aarch64-linux-gnu ;; esac
      mkdir -p "$tree/usr/lib/postgresql/18" "$tree/usr/share/postgresql" "$tree/usr/lib/sys"
      cp -a "$x/root/usr/lib/postgresql/18/bin" "$x/root/usr/lib/postgresql/18/lib" "$tree/usr/lib/postgresql/18/"
      cp -a "$x/root/usr/share/postgresql/18" "$tree/usr/share/postgresql/"
      rm -rf "$tree/usr/share/postgresql/18/man" "$tree/usr/lib/postgresql/18/lib/bitcode"
      for d in "$x/root/lib/$triplet" "$x/root/usr/lib/$triplet"; do
        [ -d "$d" ] || continue
        find "$d" -maxdepth 1 -name '*.so*' \( -type f -o -type l \) -exec cp -a {} "$tree/usr/lib/sys/" \;
      done
      find "$tree" -type l | while read -r l; do
        case "$(readlink "$l")" in /*) die "absolute symlink $l -> $(readlink "$l") in the postgres tree" ;; esac
      done
      ;;
    *) die "unknown deb layout" ;;
  esac
}

(cd "$root" && go build -trimpath -buildvcs=false -o "$work/assetpack" ./internal/assets/assetpack)

zstd_args=(-q -19 --long=27 -T0)
pack_id=""
script_sha=""
if [ -n "$blob_cache" ]; then
  mkdir -p "$blob_cache"
  blob_cache="$(cd "$blob_cache" && pwd -P)"
  pack_id="$(printf '%s\n' "$(sha_of "$work/assetpack")" "$(zstd -V)" "${zstd_args[*]}" | sha256sum | cut -d' ' -f1)"
  script_sha="$(sha_of "${BASH_SOURCE[0]}")"
fi

place() {
  ln -f "$1" "$2" 2>/dev/null || cp -f "$1" "$2"
}

cache_get() {
  local f="$blob_cache/$1.tar.zst" rec="$blob_cache/$1.sha256"
  [ -f "$f" ] && [ -f "$rec" ] || return 1
  if [ "$(sha_of "$f")" != "$(cat "$rec")" ]; then
    say "warn: cached blob $f does not hash to its record; staging again"
    rm -f "$f" "$rec"
    return 1
  fi
  place "$f" "$2"
}

cache_put() {
  local tmp="$blob_cache/.$1.$BASHPID"
  place "$2" "$tmp.tar.zst"
  sha_of "$tmp.tar.zst" >"$tmp.sha256"
  mv -f "$tmp.tar.zst" "$blob_cache/$1.tar.zst"
  mv -f "$tmp.sha256" "$blob_cache/$1.sha256"
}

stage_one() {
  local plat="$1" asset="$2" started="$SECONDS" pdir spec kind x tree digest raw zbytes pin in_key="" out_key how owned=0 member="" binary=""
  pdir="$blobs/${plat/\//_}"
  spec="$(jq -c --arg a "$asset" --arg p "$plat" '.assets[$a].fetch[$p]' "$versions")"
  [ "$spec" != "null" ] || die "$asset has no fetch spec for $plat"
  kind="$(echo "$spec" | jq -r .kind)"
  case "$kind" in go|script|gosrc) owned=1 ;; esac
  pin="$(jq -r --arg a "$asset" --arg p "$plat" '.assets[$a].platforms[$p].treeSha256 // ""' "$versions")"
  x="$work/$asset-${plat/\//_}"
  tree="$x/tree"
  mkdir -p "$tree"
  export GNUPGHOME="$x/gnupg"
  mkdir -m 0700 "$GNUPGHOME"
  if [ -n "$blob_cache" ] && [ -n "$pin" ] && [ "$print_pins" != 1 ]; then
    case "$kind" in
      file|oci|debs|gosrc)
        in_key="in-$(printf '%s\n' "$plat" "$asset" "$(jq -cS --arg a "$asset" '.assets[$a]' "$versions")" "$script_sha" "$pack_id" | sha256sum | cut -d' ' -f1)"
        if cache_get "$in_key" "$pdir/$asset.tar.zst"; then
          chmod -R u+w "$x" && rm -rf "$x"
          say "ok $plat $asset tree $pin cached by its pinned inputs ($((SECONDS - started))s)"
          return 0
        fi
        ;;
    esac
  fi
  say "$plat $asset"
  case "$kind" in
    file) stage_file "$spec" "$tree" "$x" ;;
    oci) stage_oci "$spec" "$tree" "$x" "$plat" ;;
    debs) stage_debs "$asset" "$spec" "$tree" "$x" "$plat" ;;
    script) stage_script "$spec" "$tree" "$x" ;;
    go) stage_go "$spec" "$tree" "$x" "$plat" ;;
    gosrc) stage_gosrc "$asset" "$spec" "$tree" "$x" "$plat" ;;
    *) die "$asset: unknown fetch kind" ;;
  esac
  digest="$("$work/assetpack" digest "$tree")"
  raw="$(find "$tree" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}')"
  if [ "$pin" != "$digest" ]; then
    echo "stage-assets: $asset $plat unpacks to tree $digest, versions.json pins '${pin}'" >&2
    if [ "$print_pins" != 1 ] && ! { [ "$update_pins" = 1 ] && [ "$owned" = 1 ]; }; then
      return 1
    fi
  fi
  how="packed"
  if [ -n "$blob_cache" ]; then
    "$work/assetpack" tar "$tree" >"$x/tar"
    out_key="out-$(printf '%s\n' "$(sha_of "$x/tar")" "$pack_id" | sha256sum | cut -d' ' -f1)"
    if cache_get "$out_key" "$x/blob"; then
      how="zstd reused by tar sha256"
    else
      cat "$x/tar" | zstd "${zstd_args[@]}" -o "$x/blob"
      cache_put "$out_key" "$x/blob"
    fi
    [ -z "$in_key" ] || [ "$pin" != "$digest" ] || cache_put "$in_key" "$x/blob"
  else
    "$work/assetpack" tar "$tree" | zstd "${zstd_args[@]}" -o "$x/blob"
  fi
  zbytes="$(stat -c %s "$x/blob")"
  if [ "$print_pins" = 1 ]; then
    jq -cn --arg a "$asset" --arg p "$plat" --arg d "$digest" --argjson r "$raw" --argjson z "$zbytes" \
      '{asset:$a, platform:$p, treeSha256:$d, rawBytes:$r, zstdBytes:$z}'
  fi
  if [ "$update_pins" = 1 ] && [ "$owned" = 1 ]; then
    if jq -e --arg a "$asset" --arg p "$plat" '.assets[$a].fetch[$p].sha256 != null' "$versions" >/dev/null; then
      member="$(echo "$spec" | jq -r '.members | keys[0]')"
      binary="$(sha_of "$x/u/$member")"
    fi
    jq -cn --arg a "$asset" --arg p "$plat" --arg d "$digest" --argjson r "$raw" --argjson z "$zbytes" --arg b "$binary" \
      '{asset:$a, platform:$p, treeSha256:$d, rawBytes:$r, zstdBytes:$z, binarySha256:$b} | if $b == "" then del(.binarySha256) else . end' \
      >"$pins_dir/${asset}-${plat/\//_}.json"
  fi
  rm -f "$pdir/$asset.tar.zst"
  mv -f "$x/blob" "$pdir/$asset.tar.zst"
  chmod -R u+w "$x" && rm -rf "$x"
  say "ok $plat $asset tree $digest raw $raw zstd $zbytes, $how ($((SECONDS - started))s)"
}

failed=0
running=0
stop_all() {
  local p
  for p in $(jobs -p); do
    pkill -TERM -P "$p" 2>/dev/null || true
    kill -TERM "$p" 2>/dev/null || true
  done
}
reap() {
  local pid=""
  if ! wait -n -p pid; then
    failed=1
  fi
  running=$((running - 1))
  if [ "$failed" = 1 ] && [ "$print_pins" != 1 ]; then
    stop_all
    while [ "$running" -gt 0 ]; do
      wait -n || true
      running=$((running - 1))
    done
    die "an asset failed to stage; stopped the others"
  fi
}

for plat in "${platforms[@]}"; do
  mkdir -p "$blobs/${plat/\//_}"
  for asset in $(jq -r --arg p "$plat" '.platforms[$p].assets[]' "$versions"); do
    if [ ${#only[@]} -gt 0 ] && [[ ! " ${only[*]} " == *" $asset "* ]]; then
      continue
    fi
    [ "$running" -lt "$jobs" ] || reap
    stage_one "$plat" "$asset" &
    running=$((running + 1))
  done
done
while [ "$running" -gt 0 ]; do
  reap
done
[ "$failed" = 0 ] || exit "$failed"
[ "$update_pins" = 0 ] || python3 "$root/scripts/update-pins.py" "$versions" "$pins_dir"
exit 0
