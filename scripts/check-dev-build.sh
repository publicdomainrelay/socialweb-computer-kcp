#!/usr/bin/env bash
# scripts/check-dev-build.sh [--build-root DIR]: a `build-socialweb-computer.sh --dev` binary of this checkout for the host
# platform must start: `version` answers and `dev verify-assets` extracts and verifies every embedded asset (a dev build is a
# -tags release binary whose blobs and build records are overlaid by the dev script: a built asset without its record
# refuses to start, the regression of 2026-11-14). Run by the release workflow and by deploy/check.sh --local.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build_root="$HOME/.tmp/socialweb-computer-build"
while [ $# -gt 0 ]; do
  case "$1" in
    --build-root) build_root="$2"; shift 2 ;;
    *) echo "usage: $0 [--build-root DIR]" >&2; exit 2 ;;
  esac
done
"$root/scripts/build-socialweb-computer.sh" --dev --build-root "$build_root" >&2
bin="$(ls -d "$build_root"/dev/"$(basename "$root")"/socialweb-computer)"
"$bin" version >/dev/null || { echo "check-dev-build: $bin version fails" >&2; exit 1; }
"$bin" dev verify-assets >/dev/null || { echo "check-dev-build: $bin dev verify-assets fails" >&2; exit 1; }
echo "check-dev-build: ok: the --dev build of $(git -C "$root" rev-parse --short HEAD) answers version and verifies its embedded assets"
