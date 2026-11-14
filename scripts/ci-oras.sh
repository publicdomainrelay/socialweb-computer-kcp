#!/usr/bin/env bash
# Prints the path of the pinned oras (scripts/ci-tools.json), installing it first when this machine has none of that version: the CI toolchain
# image bakes it into /opt/ci/bin (scripts/knotless-ci-toolchain.sh); otherwise it is downloaded and its sha256 checked, to $ORAS_DIR
# (default /usr/local/bin when root, else ~/.local/bin).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
want=$(jq -r '.oras.version' scripts/ci-tools.json)
for c in /opt/ci/bin/oras "$(command -v oras 2>/dev/null || true)"; do
  if [ -n "$c" ] && [ -x "$c" ] && "$c" version 2>/dev/null | grep -q "^Version: *$want\$"; then echo "$c"; exit 0; fi
done
dir=${ORAS_DIR:-$([ "$(id -u)" = 0 ] && echo /usr/local/bin || echo "$HOME/.local/bin")}
arch=$(case "$(uname -m)" in x86_64) echo linux/amd64 ;; aarch64) echo linux/arm64 ;; *) echo "ci-oras: no pinned oras for $(uname -m)" >&2; exit 1 ;; esac)
url=$(jq -r --arg a "$arch" '.oras[$a].url' scripts/ci-tools.json) sum=$(jq -r --arg a "$arch" '.oras[$a].sha256' scripts/ci-tools.json)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 -o "$tmp/oras.tgz" "$url"
echo "$sum  $tmp/oras.tgz" | sha256sum -c --quiet -
tar -xzf "$tmp/oras.tgz" -C "$tmp" oras
install -D -m 0755 "$tmp/oras" "$dir/oras"
echo "$dir/oras"
