#!/usr/bin/env bash
# The toolchain of a knotless CI job on a fresh rented VM (root, Debian). Go of go.mod and rust of
# rust/rust-toolchain.toml (each download checked against its published sha256), docker for the swc-userns-stage build,
# jq, zstd. Writes /opt/ci/env to source in later steps.
#   scripts/knotless-ci-toolchain.sh [--wasm] [--deno]    --wasm: also the wasm32 target (tests/modules); --deno: the
#   pinned deno of internal/versions/versions.json (the policy engine tests, scripts/*.ts)
set -euo pipefail
ci=/opt/ci
mkdir -p "$ci"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl git jq zstd unzip xz-utils build-essential docker.io util-linux >/dev/null
systemctl start docker 2>/dev/null || service docker start
docker version --format 'docker {{.Server.Version}}'

# the zstd the pins were made with (stage-assets packs with the zstd CLI; another version packs other bytes, so
# make check-pins differs in zstdBytes and the release's embedded blobs differ): 1.5.7 from its release tarball
zstd_v=1.5.7 zstd_sha=eb33e51f49a15e023950cd7825ca74a4a2b43db8354825ac24fc1b7ee09e6fa3
curl -fsSL --max-time 120 -o "$ci/zstd.tar.gz" "https://github.com/facebook/zstd/releases/download/v$zstd_v/zstd-$zstd_v.tar.gz"
echo "$zstd_sha  $ci/zstd.tar.gz" | sha256sum -c --quiet -
rm -rf "$ci/zstd-src" && mkdir -p "$ci/zstd-src" && tar -C "$ci/zstd-src" --strip-components 1 -xzf "$ci/zstd.tar.gz"
make -s -C "$ci/zstd-src/programs" -j"$(nproc)" zstd >/dev/null
install -D -m 0755 "$ci/zstd-src/programs/zstd" "$ci/bin/zstd" && rm -rf "$ci/zstd-src" "$ci/zstd.tar.gz"

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
case " $* " in *" --wasm "*) rustup target add wasm32-unknown-unknown ;; esac
deno_path=
case " $* " in *" --deno "*)
	deno_url="$(jq -er '.assets.deno.fetch["linux/amd64"].url' internal/versions/versions.json)"
	deno_sha="$(jq -er '.assets.deno.fetch["linux/amd64"].sha256' internal/versions/versions.json)"
	curl -fsSL --max-time 120 -o "$ci/deno.zip" "$deno_url"
	echo "$deno_sha  $ci/deno.zip" | sha256sum -c --quiet -
	rm -rf "$ci/deno" && mkdir -p "$ci/deno" && unzip -q -o "$ci/deno.zip" deno -d "$ci/deno" && rm -f "$ci/deno.zip"
	deno_path="$ci/deno:"
	;;
esac

cat >"$ci/env" <<EOF
export RUSTUP_HOME=$RUSTUP_HOME CARGO_HOME=$CARGO_HOME
export PATH=$ci/bin:$deno_path$ci/go/bin:$CARGO_HOME/bin:\$PATH
export GOTOOLCHAIN=local CONTAINER=docker
EOF
go version && rustc --version && (cd rust && rustc --version) && "$ci/bin/zstd" -V
