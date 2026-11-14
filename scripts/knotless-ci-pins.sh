#!/usr/bin/env bash
# The digest of what the CI toolchain image installs from this tree: rust/rust-toolchain.toml (channel, profile, targets), go.mod's go
# line, the pinned deno of internal/versions/versions.json and the zig (version, sha256) rust/build.sh pins. The bake
# (scripts/build-ci-image.sh) writes it in the image's marker and scripts/knotless-ci-baked.sh compares it with this checkout's: an
# image baked before one of those pins moved is installed over, not trusted, so a job never downloads a toolchain mid-build.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
{
  cat rust/rust-toolchain.toml
  awk '$1 == "go" {print "go", $2}' go.mod
  jq -c '.assets.deno.fetch["linux/amd64"]' internal/versions/versions.json
  grep -E '^zig_version=|zig_sha256=' rust/build.sh
} | sha256sum | cut -d' ' -f1
