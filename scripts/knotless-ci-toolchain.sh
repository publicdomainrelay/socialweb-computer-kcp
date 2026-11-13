#!/usr/bin/env bash
# The toolchain of a knotless CI job on a fresh rented VM (root, Debian): what .github/actions/toolchain and
# .github/actions/containers set up there. Go of go.mod and rust of rust/rust-toolchain.toml (each download checked
# against its published sha256), docker for the swc-userns-stage build, jq/zstd. Writes /opt/ci/env to source in
# later steps.
#   scripts/knotless-ci-toolchain.sh [--wasm]    --wasm: also the wasm32 target (tests/modules)
set -euo pipefail
ci=/opt/ci
mkdir -p "$ci"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl git jq zstd unzip xz-utils build-essential docker.io util-linux >/dev/null
systemctl start docker 2>/dev/null || service docker start
docker version --format 'docker {{.Server.Version}}'

go_version="$(awk '$1 == "go" {print $2}' go.mod)"
go_file="go$go_version.linux-amd64.tar.gz"
go_sha="$(curl -fsSL --max-time 60 'https://go.dev/dl/?mode=json&include=all' | jq -er --arg f "$go_file" '.[].files[] | select(.filename == $f) | .sha256')"
curl -fsSL --max-time 300 -o "$ci/$go_file" "https://go.dev/dl/$go_file"
echo "$go_sha  $ci/$go_file" | sha256sum -c --quiet -
rm -rf "$ci/go" && tar -C "$ci" -xzf "$ci/$go_file" && rm -f "$ci/$go_file"

export RUSTUP_HOME="$ci/rustup" CARGO_HOME="$ci/cargo"
init="$ci/rustup-init"
curl -fsSL --max-time 120 -o "$init" https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init
echo "$(curl -fsSL --max-time 30 https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init.sha256 | cut -d' ' -f1)  $init" | sha256sum -c --quiet -
chmod 0755 "$init" && "$init" -y --quiet --no-modify-path --profile minimal --default-toolchain stable && rm -f "$init"
export PATH="$ci/go/bin:$CARGO_HOME/bin:$PATH"
(cd rust && rustup toolchain install)
if [ "${1:-}" = --wasm ]; then rustup target add wasm32-unknown-unknown; fi

cat >"$ci/env" <<EOF
export RUSTUP_HOME=$RUSTUP_HOME CARGO_HOME=$CARGO_HOME
export PATH=$ci/go/bin:$CARGO_HOME/bin:\$PATH
export GOTOOLCHAIN=local CONTAINER=docker
EOF
go version && rustc --version && (cd rust && rustc --version)
