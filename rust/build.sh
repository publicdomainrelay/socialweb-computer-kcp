#!/usr/bin/env bash
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dist="${MESHD_DIST:-$here/dist}"
export CARGO_TARGET_DIR="${CARGO_TARGET_DIR:-$HOME/.cache/socialweb-meshd-target}"
export TMPDIR="${TMPDIR:-$HOME/.cache/scratch-tmp}"
cache="${MESHD_CACHE:-}"
cargo_home="${CARGO_HOME:-$HOME/.cargo}"
rustup_home="${RUSTUP_HOME:-$HOME/.rustup}"
mkdir -p "$TMPDIR" "$dist" "$CARGO_TARGET_DIR"
CARGO_TARGET_DIR="$(cd "$CARGO_TARGET_DIR" && pwd -P)"
unset RUSTUP_TOOLCHAIN

usage() {
  echo "usage: $0 [x86_64] [aarch64]" >&2
  echo "  builds static musl socialweb-meshd binaries into $dist" >&2
  echo "  writes socialweb-meshd-<arch> and socialweb-meshd-<arch>.sha256 (sha256sum format)" >&2
  echo "  cargo runs in a private mount namespace (unshare) that shows this directory at /mnt/src, the" >&2
  echo "  target dir at /mnt/target, CARGO_HOME at /mnt/cargo and RUSTUP_HOME at /mnt/rustup, so the bytes do" >&2
  echo "  not depend on where the checkout, the target dir or the toolchain are; rustc is rust-toolchain.toml's," >&2
  echo "  the C compiler and ar for ring are zig cc and zig ar of the zig pinned here (fetched once to MESHD_ZIG_CACHE)" >&2
  echo "  env: MESHD_DIST (output dir), MESHD_ZIG_CACHE (default ~/.cache/socialweb-meshd-zig)," >&2
  echo "       CARGO_TARGET_DIR (default ~/.cache/socialweb-meshd-target)," >&2
  echo "       MESHD_CACHE (keep each binary in DIR keyed by the sha256 of the files of rust/, the rustc" >&2
  echo "       and cargo versions and the target; a hit skips cargo for that arch)" >&2
  exit 2
}

arches=("$@")
if [ "${#arches[@]}" -eq 0 ]; then
  arches=(x86_64 aarch64)
fi
for arch in "${arches[@]}"; do
  case "$arch" in
    x86_64|aarch64) ;;
    *) usage ;;
  esac
done

cd "$here"
zig_version=0.15.2
case "$(uname -m)" in
  x86_64) zig_sha256=02aa270f183da276e5b5920b1dac44a63f1a49e55050ebde3aecc9eb82f93239 ;;
  aarch64) zig_sha256=958ed7d1e00d0ea76590d27666efbf7a932281b3d7ba0c6b01b0ff26498f667f ;;
  *) echo "build.sh: no pinned zig for host $(uname -m)" >&2; exit 1 ;;
esac
zig_dir="${MESHD_ZIG_CACHE:-$HOME/.cache/socialweb-meshd-zig}/$zig_sha256"
if [ ! -x "$zig_dir/zig" ]; then
  rm -rf "$zig_dir.tmp"
  mkdir -p "$zig_dir.tmp"
  curl -fsSL --max-time 300 -o "$zig_dir.tmp/zig.tar.xz" "https://ziglang.org/download/$zig_version/zig-$(uname -m)-linux-$zig_version.tar.xz"
  echo "$zig_sha256  $zig_dir.tmp/zig.tar.xz" | sha256sum -c --quiet -
  tar -xJf "$zig_dir.tmp/zig.tar.xz" -C "$zig_dir.tmp" --strip-components=1
  rm "$zig_dir.tmp/zig.tar.xz"
  mv -T "$zig_dir.tmp" "$zig_dir"
fi
command -v unshare >/dev/null || { echo "build.sh: unshare not found" >&2; exit 1; }
sysroot="$(rustc --print sysroot)"
host="$(rustc -vV | sed -n 's/^host: //p')"
case "$sysroot" in
  "$rustup_home"/toolchains/*) ;;
  *) echo "build.sh: rustc sysroot $sysroot is not under RUSTUP_HOME $rustup_home; install rustc with rustup" >&2; exit 1 ;;
esac
[ -d "$cargo_home/registry" ] || mkdir -p "$cargo_home/registry"
ns_sysroot="/mnt/rustup/${sysroot#"$rustup_home"/}"

echo "build.sh: $(rustc --version) $(cargo --version)"

key_of() {
  {
    echo "socialweb-meshd v2 $1"
    find . -path ./dist -prune -o -path ./target -prune -o -type f -printf '%P\0' | sort -z | xargs -0 sha256sum
    rustc -vV
    cargo -V
    echo "zig $zig_version $zig_sha256"
  } | sha256sum | cut -d' ' -f1
}

publish() {
  local arch="$1" built="$2" out="socialweb-meshd-$1"
  if readelf -lW "$built" | grep -q 'INTERP'; then
    echo "build.sh: $built asks for a dynamic loader; it must be static" >&2
    exit 1
  fi
  install -m 0755 "$built" "$dist/$out"
  (cd "$dist" && sha256sum "$out" > "$out.sha256")
  echo "build.sh: $dist/$out $(stat -c %s "$dist/$out") bytes sha256 $(cut -d' ' -f1 "$dist/$out.sha256")"
}

declare -A keys=()
build=()
cargo_env=()
for arch in "${arches[@]}"; do
  triple="$arch-unknown-linux-musl"
  if [ ! -d "$sysroot/lib/rustlib/$triple" ]; then
    echo "build.sh: target $triple is not installed; run: rustup target add $triple" >&2
    exit 1
  fi
  if [ -n "$cache" ]; then
    keys[$arch]="$(key_of "$triple")"
    if [ -f "$cache/${keys[$arch]}/socialweb-meshd" ]; then
      echo "build.sh: $arch cached by source and toolchain key ${keys[$arch]}"
      publish "$arch" "$cache/${keys[$arch]}/socialweb-meshd"
      continue
    fi
  fi
  lower="${triple//-/_}"
  upper="$(echo "$lower" | tr '[:lower:]' '[:upper:]')"
  cargo_env+=("CC_$lower=/mnt/src/zig/cc" "CFLAGS_$lower=--target=$triple" "AR_$lower=/mnt/src/zig/ar" "CARGO_TARGET_${upper}_LINKER=$ns_sysroot/lib/rustlib/$host/bin/rust-lld")
  build+=("$arch")
done

if [ "${#build[@]}" -gt 0 ]; then
  targets=()
  for arch in "${build[@]}"; do
    targets+=(--target "$arch-unknown-linux-musl")
  done
  unshare --user --map-root-user --mount bash -c '
    set -euo pipefail
    mount -t tmpfs -o mode=0755 socialweb-meshd /mnt
    mkdir /mnt/src /mnt/target /mnt/cargo /mnt/rustup /mnt/zig
    mount --bind "$1" /mnt/src
    mount --bind "$2" /mnt/target
    mount --bind "$3" /mnt/cargo
    mount --bind "$4" /mnt/rustup
    mount --bind "$5" /mnt/zig
    shift 5
    cd /mnt/src
    exec "$@"
  ' _ "$here" "$CARGO_TARGET_DIR" "$cargo_home" "$rustup_home" "$zig_dir" \
    env -u CARGO_ENCODED_RUSTFLAGS -u RUSTC_WRAPPER \
    CARGO_HOME=/mnt/cargo RUSTUP_HOME=/mnt/rustup CARGO_TARGET_DIR=/mnt/target ZIG_GLOBAL_CACHE_DIR=/mnt/target/zig-cache ZIG_LOCAL_CACHE_DIR=/mnt/target/zig-cache PATH="/mnt/cargo/bin:$PATH" \
    RUSTFLAGS="--remap-path-prefix=/mnt/src=/socialweb-meshd --remap-path-prefix=/mnt/cargo/registry/src=/cargo-registry --remap-path-prefix=$ns_sysroot=/rust-sysroot" \
    "${cargo_env[@]}" \
    cargo build --release --locked "${targets[@]}" -p socialweb-meshd --manifest-path /mnt/src/Cargo.toml
  for arch in "${build[@]}"; do
    built="$CARGO_TARGET_DIR/$arch-unknown-linux-musl/release/socialweb-meshd"
    publish "$arch" "$built"
    if [ -n "$cache" ]; then
      mkdir -p "$cache/.tmp.$$"
      install -m 0755 "$built" "$cache/.tmp.$$/socialweb-meshd"
      mv -T "$cache/.tmp.$$" "$cache/${keys[$arch]}" 2>/dev/null || rm -rf "${cache:?}/.tmp.$$"
    fi
  done
fi
