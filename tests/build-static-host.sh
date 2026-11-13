#!/usr/bin/env bash
# A static (musl) swc-test-host for a staging VM: prints its path (export SWC_TEST_HOST_STATIC=<that>).
# C deps (ring) build with zig cc: $ZIG, else the zig scripts/build-cargosrc.sh pins and caches.
set -euo pipefail
cd "$(dirname "$0")"
zig="${ZIG:-$(ls ~/.cache/socialweb-meshd-zig/*/zig 2>/dev/null | head -1)}"
[ -x "$zig" ] || { echo "build-static-host: no zig (set ZIG, or run scripts/build-cargosrc.sh once)" >&2; exit 1; }
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
