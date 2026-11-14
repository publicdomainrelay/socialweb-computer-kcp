#!/usr/bin/env bash
# A static (musl) swc-test-host for a staging VM: prints its path (export SWC_TEST_HOST_STATIC=<that>).
# C deps (ring) build with zig cc: $ZIG, else the zig scripts/build-cargosrc.sh pins and caches.
set -euo pipefail
cd "$(dirname "$0")"
zc="${MESHD_ZIG_CACHE:-$HOME/.cache/socialweb-meshd-zig}"
zig="${ZIG:-$(ls "$zc"/*/zig 2>/dev/null | head -1 || true)}"
if [ ! -x "$zig" ]; then
  zv="$(sed -n 's/^zig_version=//p' ../rust/build.sh)"
  zs="$(sed -n 's/^  x86_64) zig_sha256=\([0-9a-f]*\) ;;$/\1/p' ../rust/build.sh)"
  [ -n "$zv" ] && [ -n "$zs" ] || { echo "build-static-host: no zig (set ZIG), and rust/build.sh pins none" >&2; exit 1; }
  zd="$zc/$zs"
  mkdir -p "${zd%/*}"
  zt="$(mktemp -d "$zd.tmp.XXXXXX")"
  curl -fsSL --max-time 300 -o "$zt/zig.tar.xz" "https://ziglang.org/download/$zv/zig-x86_64-linux-$zv.tar.xz"
  echo "$zs  $zt/zig.tar.xz" | sha256sum -c --quiet -
  tar -xJf "$zt/zig.tar.xz" -C "$zt" --strip-components=1 && rm "$zt/zig.tar.xz"
  mv -T "$zt" "$zd" 2>/dev/null || rm -rf "$zt"
  zig="$zd/zig"
fi
[ -x "$zig" ] || { echo "build-static-host: no zig (set ZIG)" >&2; exit 1; }
t=x86_64-unknown-linux-musl
w="${TMPDIR:-$HOME/.tmp}/swc-static-host-zig"
mkdir -p "$w"
printf '#!/bin/sh\nfor a do shift; case "$a" in --target=*) ;; *) set -- "$@" "$a" ;; esac; done\nexec %s cc -target x86_64-linux-musl -fno-sanitize=undefined "$@"\n' "$zig" > "$w/cc"
printf '#!/bin/sh\nexec %s ar "$@"\n' "$zig" > "$w/ar"
chmod +x "$w/cc" "$w/ar"
rustup target add "$t" >/dev/null
CC_x86_64_unknown_linux_musl="$w/cc" AR_x86_64_unknown_linux_musl="$w/ar" \
  cargo build -q --release --target "$t" -p swc-test-host >&2
echo "${CARGO_TARGET_DIR:-$PWD/target}/$t/release/swc-test-host"
