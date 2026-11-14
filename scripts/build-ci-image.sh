#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
versions="$root/internal/versions/versions.json"

usage() {
  cat >&2 <<EOF
usage: $0 --base DIR [--target firecracker-x86_64] [--out DIR] [--work DIR] [--publish]

Builds the knotless CI toolchain VM image: the pinned base guest image's vmlinux and its rootfs with
scripts/knotless-ci-toolchain.sh --bake --wasm --deno --arm64 run into it (in a docker container of that
rootfs, cwd this checkout), and an image.json of the base's shape that pins both files and names the
marker written to /etc/swc-image. A requester rents it with compute.vm image {url, sha256 of image.json}.

  --base     a dir holding the pinned base image (vmlinux, rootfs.ext4, image.json); every file must
             match its versions.json pin
  --out      where <name>/{vmlinux,rootfs.ext4,image.json,SHA256SUMS} land (default WORK/out)
  --work     scratch (default \$GUEST_IMAGE_WORK or \$HOME/.tmp/guest-image-build; never /tmp)
  --publish  upload <out>/<name> content-addressed to vm-images/<name>/<image.json sha256> in the
             bucket (SPACES_* in the environment; refuses when any key already exists) and write
             deploy/vm-images/<name>.json {url, sha256}

The bake is not bit-for-bit reproducible (apt and the toolchains download the current packages); the
image is pinned by the sha256 of its outputs, and a change to the toolchain script is a new image.
EOF
  exit 2
}

target=firecracker-x86_64
base=""
out=""
work="${GUEST_IMAGE_WORK:-$HOME/.tmp/guest-image-build}"
publish=0
name=ci-toolchain
while [ $# -gt 0 ]; do
  case "$1" in
    --base) base="$2"; shift 2 ;;
    --target) target="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    --work) work="$2"; shift 2 ;;
    --publish) publish=1; shift ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done
[ -n "$base" ] || usage
die() { echo "build-ci-image: $*" >&2; exit 1; }
say() { echo "build-ci-image: $*" >&2; }
case "$work" in /tmp|/tmp/*) die "--work $work is under /tmp (tmpfs on build hosts)" ;; esac
for tool in docker jq sha256sum tar; do command -v "$tool" >/dev/null 2>&1 || die "$tool is required"; done

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
pin() { jq -r "$1" "$versions"; }
t=".guestImages.targets[\"$target\"]"
for f in vmlinux rootfs.ext4 image.json; do
  want="$(pin "$t.outputs[\"$f\"].sha256")"
  [ "$(sha_of "$base/$f")" = "$want" ] || die "$base/$f is not the pinned $target $f ($want)"
done
base_pin="$(pin "$t.outputs[\"image.json\"].sha256")"
sde="$(pin "$t.sourceDateEpoch")"
[ "$(pin "$t.arch")" = x86_64 ] || die "$target: the toolchain script is amd64 only"

[ -n "$out" ] || out="$work/out"
tw="$work/$name-$target"
rm -rf "$tw" && mkdir -p "$tw" "$out/$name"
dockerfile="$root/$(pin '.guestImages.builder.dockerfile')"
builder="localhost/socialweb-guest-builder:sha-$(sha_of "$dockerfile" | cut -c1-12)"
docker image inspect "$builder" >/dev/null 2>&1 || docker build -q -t "$builder" -f "$dockerfile" "$(dirname "$dockerfile")" >&2

commit="$(git -C "$root" rev-parse HEAD)"
script_sha="$(sha_of "$root/scripts/knotless-ci-toolchain.sh")"
marker="$name base=$base_pin kcp=$commit toolchain=$script_sha"
img="localhost/swc-ci-image-base:${base_pin:0:12}"
ctr="swc-ci-image-$$"
cleanup() { docker rm -f "$ctr" >/dev/null 2>&1 || true; }
trap cleanup EXIT

say "the base rootfs as a container image"
docker run --rm --network none -e LANG=C.UTF-8 -e LC_ALL=C.UTF-8 -v "$base:/base:ro" -v "$tw:/w" "$builder" sh -euc '
  mkdir /w/root && debugfs -R "rdump / /w/root" /base/rootfs.ext4 2>/dev/null
  tar -C /w/root --numeric-owner -cf /w/base.tar . && mkdir /w/etc && tar -C /w/root --numeric-owner -cf /w/etc.tar ./etc/resolv.conf ./etc/hosts ./etc/hostname 2>/dev/null || true
  rm -rf /w/root'
docker import "$tw/base.tar" "$img" >/dev/null
rm -f "$tw/base.tar"

say "baking the toolchain (scripts/knotless-ci-toolchain.sh --bake --wasm --deno --arm64)"
docker run --name "$ctr" -v "$root:/src:ro" -w /src -e HOME=/root "$img" bash -euc '
  bash scripts/knotless-ci-toolchain.sh --bake --wasm --deno --arm64
  printf "%s\n" "$0" > /etc/swc-image
  rm -rf /etc/ssh/ssh_host_* /var/lib/cloud /var/log/cloud-init* /var/lib/apt/lists/* /var/cache/apt/*.bin /root/.cache /tmp/* /var/tmp/*
  : > /etc/machine-id' "$marker" >&2

say "the baked rootfs"
docker export "$ctr" >"$tw/rootfs.tar"
# docker writes its own /etc/resolv.conf, hosts and hostname into a container: the base's go back
tar --delete -f "$tw/rootfs.tar" ./etc/resolv.conf ./etc/hosts ./etc/hostname 2>/dev/null || tar --delete -f "$tw/rootfs.tar" etc/resolv.conf etc/hosts etc/hostname 2>/dev/null || true
[ -s "$tw/etc.tar" ] && tar -Af "$tw/rootfs.tar" "$tw/etc.tar"
rm -f "$tw/etc.tar"
tar_sha="$(sha_of "$tw/rootfs.tar")"
data_bytes="$(tar -tvf "$tw/rootfs.tar" | awk '{s += $3} END {print s}')"
size_mib=$(( (data_bytes * 11 / 10 / 1048576 + 512 + 63) / 64 * 64 ))
uuid="${tar_sha:0:8}-${tar_sha:8:4}-${tar_sha:12:4}-${tar_sha:16:4}-${tar_sha:20:12}"
docker run --rm --network none -e LANG=C.UTF-8 -e LC_ALL=C.UTF-8 -e E2FSPROGS_FAKE_TIME="$sde" -v "$tw:/w" "$builder" sh -euc "
  mke2fs -q -F -t ext4 -b 4096 -I 256 -L cloudimg-rootfs -U $uuid -E hash_seed=$uuid,root_owner=0:0,lazy_itable_init=0,lazy_journal_init=0 \
    -d /w/rootfs.tar /w/rootfs.ext4 ${size_mib}M
  e2fsck -fn /w/rootfs.ext4 >/dev/null"
rm -f "$tw/rootfs.tar"

dir="$out/$name"
rm -f "$dir"/*
cp "$base/vmlinux" "$dir/vmlinux"
mv "$tw/rootfs.ext4" "$dir/rootfs.ext4"
jq --arg sha "$(sha_of "$dir/rootfs.ext4")" --argjson bytes "$(stat -c %s "$dir/rootfs.ext4")" \
  --arg tar "$tar_sha" --arg marker "$marker" --arg base "$base_pin" \
  '.rootfs.sha256 = $sha | .rootfs.bytes = $bytes | .rootfsTarSha256 = $tar | .marker = $marker
   | .inputs.ciImageBase = {sha256: $base}' "$base/image.json" >"$dir/image.json"
(cd "$dir" && sha256sum vmlinux rootfs.ext4 image.json >SHA256SUMS)
image_sha="$(sha_of "$dir/image.json")"
cat "$dir/SHA256SUMS" >&2
say "marker: $marker"
echo "$name image.json $image_sha"

if [ "$publish" = 1 ]; then
  for v in SPACES_BUCKET SPACES_REGION SPACES_ACCESS_KEY_ID SPACES_SECRET_ACCESS_KEY; do [ -n "${!v:-}" ] || die "$v is unset"; done
  prefix="vm-images/$name/$image_sha"
  url="https://$SPACES_BUCKET.$SPACES_REGION.digitaloceanspaces.com/$prefix"
  for f in vmlinux rootfs.ext4 image.json SHA256SUMS; do
    code="$(curl -s -o /dev/null -w '%{http_code}' -I "$url/$f")"
    case "$code" in 403|404) ;; *) die "$url/$f answers $code: not overwriting a published object" ;; esac
  done
  deno run -A --no-lock "$root/scripts/publish-spaces.ts" --dir "$dir" --prefix "$prefix" --content-addressed
  curl -fsSL "$url/SHA256SUMS" | cmp -s - "$dir/SHA256SUMS" || die "$url/SHA256SUMS is not the built one"
  mkdir -p "$root/deploy/vm-images"
  jq -n --arg url "$url" --arg sha "$image_sha" --arg marker "$marker" '{url: $url, sha256: $sha, marker: $marker}' \
    >"$root/deploy/vm-images/$name.json"
  say "published $url (pin in deploy/vm-images/$name.json)"
fi
