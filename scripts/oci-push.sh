#!/usr/bin/env bash
# scripts/oci-push.sh <file-or-dir> [ttl=1h]: pushes a file, or a whole directory tree, to https://ttl.sh (an anonymous, ephemeral OCI
# registry: the tag is the TTL, 24h at most) with oras, under an unguessable name, and prints
#   ttl.sh/<name>@sha256:<digest of the manifest>
# the one thing a consumer pins (`oras pull <that> -o dir` verifies every blob against it; a tag is never trusted). No credential, so
# a fork's run can use it. Exit 1 when the registry refuses or is down: the caller keeps its local build.
set -euo pipefail
src=$1 ttl=${2:-1h}
oras=$(bash "$(dirname "${BASH_SOURCE[0]}")/ci-oras.sh")
name="ttl.sh/swc-$(cat /proc/sys/kernel/random/uuid)"
cd "$(dirname "$src")"
"$oras" push --no-tty --artifact-type application/vnd.swc.ci-output "$name:$ttl" "$(basename "$src")" --format go-template='{{.reference}}'
