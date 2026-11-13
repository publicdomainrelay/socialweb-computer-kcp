#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
versions="$root/internal/versions/versions.json"

usage() {
  cat >&2 <<EOF
usage: $0 --asset NAME --arch x86_64|aarch64 --out FILE

Builds one static musl binary of a Rust program from its pinned source archive (the cargosrc object of
assets.<NAME> in internal/versions/versions.json: url, sha256, commit, toolchain, manifest, package, bin):
the archive is checked against its sha256 pin and its top directory must name the pinned commit; cargo
build --release --locked runs with the pinned rustc in an empty environment (env -i)
and a private mount namespace (unshare) that shows the
source at /mnt/src, the target dir at /mnt/target, CARGO_HOME at /mnt/cargo and RUSTUP_HOME at /mnt/rustup,
with zig cc and zig ar of the zig rust/build.sh pins as the C compiler, so the bytes do not depend on where
anything lives. Crates come from their registries and git sources, verified by the archive's Cargo.lock.
env: CARGOSRC_CACHE (keep each binary in DIR keyed by the sha256 of the cargosrc object, this script, the
     zig wrappers, the toolchain and the target; a hit skips cargo), CARGOSRC_TARGET_DIR (default
     ~/.cache/socialweb-cargosrc-target), MESHD_ZIG_CACHE (default ~/.cache/socialweb-meshd-zig)
EOF
  exit 2
}

asset="" arch="" out=""
while [ $# -gt 0 ]; do
  case "$1" in
    --asset) asset="$2"; shift 2 ;;
    --arch) arch="$2"; shift 2 ;;
    --out) out="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[ -n "$asset" ] && [ -n "$out" ] || usage
case "$arch" in x86_64|aarch64) ;; *) usage ;; esac

die() { echo "build-cargosrc: $*" >&2; exit 1; }
sha_of() { sha256sum "$1" | cut -d' ' -f1; }

src="$(jq -c --arg a "$asset" '.assets[$a].cargosrc' "$versions")"
[ "$src" != null ] || die "assets.$asset has no cargosrc object"
field() { echo "$src" | jq -er ".$1" || die "assets.$asset.cargosrc.$1 is not set"; }
url="$(field url)"
want="$(field sha256)"
commit="$(field commit)"
toolchain="$(field toolchain)"
manifest="$(field manifest)"
package="$(field package)"
bin="$(field bin)"
triple="$arch-unknown-linux-musl"

cargo_home="${CARGO_HOME:-$HOME/.cargo}"
rustup_home="${RUSTUP_HOME:-$HOME/.rustup}"
target_dir="${CARGOSRC_TARGET_DIR:-$HOME/.cache/socialweb-cargosrc-target}"
cache="${CARGOSRC_CACHE:-$HOME/.cache/socialweb-cargosrc}"
mkdir -p "$target_dir" "$cache" "$cargo_home/registry" "$cargo_home/git"
target_dir="$(cd "$target_dir" && pwd -P)"
unset RUSTUP_TOOLCHAIN

zig_version=0.15.2
case "$(uname -m)" in
  x86_64) zig_sha256=02aa270f183da276e5b5920b1dac44a63f1a49e55050ebde3aecc9eb82f93239 ;;
  aarch64) zig_sha256=958ed7d1e00d0ea76590d27666efbf7a932281b3d7ba0c6b01b0ff26498f667f ;;
  *) die "no pinned zig for host $(uname -m)" ;;
esac

key="$({
  echo "cargosrc v1 $asset $triple"
  echo "$src" | jq -cS .
  sha256sum "${BASH_SOURCE[0]}" "$root/rust/zig/cc" "$root/rust/zig/ar" | cut -d' ' -f1
  echo "zig $zig_version $zig_sha256"
} | sha256sum | cut -d' ' -f1)"

publish() {
  if readelf -lW "$1" | grep -q INTERP; then
    die "$1 asks for a dynamic loader; it must be static"
  fi
  mkdir -p "$(dirname "$out")"
  install -m 0755 "$1" "$out"
  echo "build-cargosrc: $asset $triple from $commit with $toolchain, binary sha256 $(sha_of "$out")" >&2
}

if [ -f "$cache/$key/$bin" ]; then
  echo "build-cargosrc: $asset $triple cached by key $key" >&2
  publish "$cache/$key/$bin"
  exit 0
fi

command -v unshare >/dev/null || die "unshare not found"
command -v rustup >/dev/null || die "rustup not found"
command -v flock >/dev/null || die "flock not found"
mkdir -p "$rustup_home"
(
  flock 9
  if ! rustup run "$toolchain" rustc -V >/dev/null 2>&1; then
    rustup toolchain uninstall "$toolchain" >/dev/null 2>&1 || true
    rustup toolchain install --profile minimal "$toolchain" >&2
  fi
  sysroot="$(rustup run "$toolchain" rustc --print sysroot)"
  [ -d "$sysroot/lib/rustlib/$triple" ] || rustup target add --toolchain "$toolchain" "$triple" >&2
) 9>"$rustup_home/.socialweb-cargosrc.lock" || die "cannot install rust $toolchain with target $triple"
sysroot="$(rustup run "$toolchain" rustc --print sysroot)"
host="$(rustup run "$toolchain" rustc -vV | sed -n 's/^host: //p')"
case "$sysroot" in
  "$rustup_home"/toolchains/*) ;;
  *) die "rustc sysroot $sysroot is not under RUSTUP_HOME $rustup_home" ;;
esac
ns_sysroot="/mnt/rustup/${sysroot#"$rustup_home"/}"

zig_dir="${MESHD_ZIG_CACHE:-$HOME/.cache/socialweb-meshd-zig}/$zig_sha256"
if [ ! -x "$zig_dir/zig" ]; then
  mkdir -p "${zig_dir%/*}"
  zig_tmp="$(mktemp -d "$zig_dir.tmp.XXXXXX")"
  curl -fsSL --max-time 300 -o "$zig_tmp/zig.tar.xz" "https://ziglang.org/download/$zig_version/zig-$(uname -m)-linux-$zig_version.tar.xz"
  echo "$zig_sha256  $zig_tmp/zig.tar.xz" | sha256sum -c --quiet -
  tar -xJf "$zig_tmp/zig.tar.xz" -C "$zig_tmp" --strip-components=1
  rm "$zig_tmp/zig.tar.xz"
  mv -T "$zig_tmp" "$zig_dir" 2>/dev/null || rm -rf "$zig_tmp"
  [ -x "$zig_dir/zig" ]
fi

work="$(mktemp -d "$cache/.work.XXXXXX")"
trap 'chmod -R u+w "$work" 2>/dev/null; rm -rf "$work"' EXIT
archive="$cache/$want.tar.gz"
if [ ! -f "$archive" ] || [ "$(sha_of "$archive")" != "$want" ]; then
  curl -fSL --retry 5 --retry-all-errors --retry-delay 3 --connect-timeout 10 -s -o "$work/archive" "$url" || die "download failed: $url"
  [ "$(sha_of "$work/archive")" = "$want" ] || die "$url has sha256 $(sha_of "$work/archive"), pinned $want"
  mv -f "$work/archive" "$archive"
fi
top="$(tar -tzf "$archive" | awk -F/ 'NR==1{print $1}')"
[ "${top##*-}" = "$commit" ] || die "$url: the archive's top directory $top does not name the pinned commit $commit"
mkdir -p "$work/src"
tar -xzf "$archive" -C "$work/src" --strip-components=1
find "$work/src" -exec touch -h {} +
[ -f "$work/src/$manifest" ] || die "$manifest is not in the archive"
[ -f "$work/src/$(dirname "$manifest")/Cargo.lock" ] || die "the archive has no Cargo.lock beside $manifest"

lower="${triple//-/_}"
upper="$(echo "$lower" | tr '[:lower:]' '[:upper:]')"
echo "build-cargosrc: $asset $triple: cargo build of $commit with $toolchain" >&2
unshare --user --map-root-user --mount bash -c '
  set -euo pipefail
  mount -t tmpfs -o mode=0755 socialweb-cargosrc /mnt
  mkdir /mnt/src /mnt/target /mnt/cargo /mnt/rustup /mnt/zig /mnt/wrap
  mount --bind "$1" /mnt/src
  mount --bind "$2" /mnt/target
  mount --bind "$3" /mnt/cargo
  mount --bind "$4" /mnt/rustup
  mount --bind "$5" /mnt/zig
  mount --bind "$6" /mnt/wrap
  mkdir -p /mnt/target/home /mnt/target/tmp
  shift 6
  cd /mnt/src
  exec "$@"
' _ "$work/src" "$target_dir" "$cargo_home" "$rustup_home" "$zig_dir" "$root/rust/zig" \
  env -i HOME=/mnt/target/home TMPDIR=/mnt/target/tmp LANG=C.UTF-8 LC_ALL=C.UTF-8 TZ=UTC CARGO_INCREMENTAL=0 \
  CARGO_HOME=/mnt/cargo RUSTUP_HOME=/mnt/rustup RUSTUP_TOOLCHAIN="$toolchain" CARGO_TARGET_DIR=/mnt/target \
  ZIG_GLOBAL_CACHE_DIR=/mnt/target/zig-cache ZIG_LOCAL_CACHE_DIR=/mnt/target/zig-cache PATH="$ns_sysroot/bin:/usr/local/bin:/usr/bin:/bin" RUSTC="$ns_sysroot/bin/rustc" \
  RUSTFLAGS="--remap-path-prefix=/mnt/src=/$asset --remap-path-prefix=/mnt/cargo/registry/src=/cargo-registry --remap-path-prefix=/mnt/cargo/git/checkouts=/cargo-git --remap-path-prefix=$ns_sysroot=/rust-sysroot" \
  "CC_$lower=/mnt/wrap/cc" "CFLAGS_$lower=--target=$triple -fno-sanitize=undefined" "AR_$lower=/mnt/wrap/ar" \
  "CARGO_TARGET_${upper}_LINKER=$ns_sysroot/lib/rustlib/$host/bin/rust-lld" \
  "$ns_sysroot/bin/cargo" build --release --locked --target "$triple" -p "$package" --bin "$bin" --manifest-path "/mnt/src/$manifest"
built="$target_dir/$triple/release/$bin"
publish "$built"
mkdir -p "$cache/.tmp.$$"
install -m 0755 "$built" "$cache/.tmp.$$/$bin"
mv -T "$cache/.tmp.$$" "$cache/$key" 2>/dev/null || rm -rf "${cache:?}/.tmp.$$"
