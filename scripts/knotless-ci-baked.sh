#!/usr/bin/env bash
# Exit 0 when this VM is the baked CI toolchain image of this checkout (scripts/build-ci-image.sh): /etc/swc-image starts
# with `ci-toolchain `, names the sha256 of this checkout's scripts/knotless-ci-toolchain.sh (an image baked by another
# version of the script is installed over, not trusted), /opt/ci/env is there, and its Go is the one go.mod asks for.
# Prints why not, and exits 1, otherwise: the job then runs scripts/knotless-ci-toolchain.sh.
set -u
fail() { echo "baked toolchain: not used: $*"; exit 1; }
[ -f /etc/swc-image ] || fail "no /etc/swc-image"
marker="$(head -n1 /etc/swc-image)"
case "$marker" in "ci-toolchain "*) ;; *) fail "the image is not a ci-toolchain image: $marker" ;; esac
want="$(sha256sum scripts/knotless-ci-toolchain.sh | cut -d' ' -f1)"
case " $marker " in *" toolchain=$want "*) ;; *) fail "baked by another toolchain script (this checkout's is $want): $marker" ;; esac
[ -f /opt/ci/env ] || fail "no /opt/ci/env"
go_want="go$(awk '$1 == "go" {print $2}' go.mod)"
go_have="$(GOTOOLCHAIN=local /opt/ci/go/bin/go env GOVERSION 2>/dev/null)"
[ "$go_have" = "$go_want" ] || fail "its Go is ${go_have:-absent}, go.mod asks for $go_want"
echo "baked toolchain: used ($marker)"
