#!/usr/bin/env bash
# Exit 0 when this VM is the baked CI toolchain image of this checkout (scripts/build-ci-image.sh): /etc/swc-image starts
# with `ci-toolchain `, is the sha256 of /etc/swc-image.detail, which names the sha256 of this checkout's scripts/knotless-ci-toolchain.sh (an image baked by another
# version of the script is installed over, not trusted), it was baked from this checkout's toolchain pins (scripts/knotless-ci-pins.sh: rust,
# go, deno, zig), /opt/ci/env is there, and its Go is the one go.mod asks for.
# Prints why not, and exits 1, otherwise: the job then runs scripts/knotless-ci-toolchain.sh.
set -u
fail() { echo "baked toolchain: not used: $*"; exit 1; }
[ -f /etc/swc-image ] || fail "no /etc/swc-image"
marker="$(head -n1 /etc/swc-image)"
case "$marker" in "ci-toolchain "*) ;; *) fail "the image is not a ci-toolchain image: $marker" ;; esac
# the marker is `ci-toolchain <sha256 of /etc/swc-image.detail>` (a bidder refuses one over 256 bytes); the detail names what was baked
[ -f /etc/swc-image.detail ] || fail "no /etc/swc-image.detail"
[ "ci-toolchain $(sha256sum /etc/swc-image.detail | cut -d' ' -f1)" = "$marker" ] || fail "the marker is not the digest of /etc/swc-image.detail: $marker"
want="$(sha256sum scripts/knotless-ci-toolchain.sh | cut -d' ' -f1)"
grep -qx "toolchain=$want" /etc/swc-image.detail || fail "baked by another toolchain script (this checkout's is $want): $(tr '\n' ' ' </etc/swc-image.detail)"
pins="$(scripts/knotless-ci-pins.sh)"
grep -qx "pins=$pins" /etc/swc-image.detail || fail "baked before the toolchain pins of this checkout (rust/rust-toolchain.toml, go.mod, deno, zig, ci-tools: $pins): $(tr '\n' ' ' </etc/swc-image.detail)"
[ -f /opt/ci/env ] || fail "no /opt/ci/env"
go_want="go$(awk '$1 == "go" {print $2}' go.mod)"
go_have="$(GOTOOLCHAIN=local /opt/ci/go/bin/go env GOVERSION 2>/dev/null)"
[ "$go_have" = "$go_want" ] || fail "its Go is ${go_have:-absent}, go.mod asks for $go_want"
echo "baked toolchain: used ($marker)"
