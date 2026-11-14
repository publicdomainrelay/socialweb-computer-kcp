#!/usr/bin/env bash
# The socialweb-computer the staging run (test, compositions) puts under test, on the CI VM as /home/r2/swc-under-test, and the
# requester spec the staging VMs get it by (/home/r2/requester.spec):
#   sha:<commit>   this commit's binary is published (main/<commit>/), or this branch changed nothing a release binary is built from
#                  since it forked from main (internal/ cmd/ api/ rust/ lexicons/ deploy/ except deploy/vm-images/, go.mod go.sum,
#                  the build and asset scripts) and the fork point's is: the CI VM and every staging VM DOWNLOAD it from the public bucket over HTTPS,
#                  checked against its SHA256SUMS (fast; a 589 MB put through the requester's ssh took 12 to 38 min and broke a pipe)
#   path:<file>    otherwise this commit's build (a branch cannot publish: no bucket key), built here with the asset and build
#                  caches restored read-only, and put on the staging VMs by the test host (request.put, SWC_BINARY)
# A `sha:` binary reports its own commit's version: the tests run this tree's against it.
set -euo pipefail
base=https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com
bin=socialweb-computer-linux-amd64
out=/home/r2/swc-under-test spec=/home/r2/requester.spec
sha=$(git rev-parse HEAD)
published() { curl -fsI -o /dev/null "$base/main/$1/binaries/SHA256SUMS"; }
pick=
if published "$sha"; then
  pick=$sha
else
  fork=$(git merge-base HEAD origin/main 2>/dev/null || true)
  if [ -n "$fork" ] && ! git diff --name-only "$fork" HEAD | grep -vE '^deploy/vm-images/' | grep -qE '^(internal/|cmd/|api/|rust/|lexicons/|deploy/|go\.(mod|sum)$|scripts/(build-socialweb-computer|stage-assets|asset-cache|ci-build-cache|build-guest-image)[^/]*$)'; then
    published "$fork" && pick=$fork
  fi
fi
if [ -n "$pick" ]; then
  echo "binary: main/$pick (published; $([ "$pick" = "$sha" ] && echo "this commit's" || echo "this branch's fork point, nothing a binary is built from changed since"))"
  tmp=$(mktemp -d)
  curl -fsSL --retry 3 -o "$tmp/$bin" "$base/main/$pick/binaries/$bin"
  curl -fsSL --retry 3 "$base/main/$pick/binaries/SHA256SUMS" | grep "  $bin\$" > "$tmp/SUMS"
  (cd "$tmp" && sha256sum -c --quiet SUMS)
  install -D -m 0755 -o r2 "$tmp/$bin" "$out"
  rm -rf "$tmp"
  echo "sha:$pick" > "$spec"
else
  echo "binary: this commit's build (not published, and the branch changed what a binary is built from)"
  if bash scripts/knotless-ci-baked.sh; then :; else bash scripts/knotless-ci-toolchain.sh --deno; fi
  . /opt/ci/env
  root=/root/branch-build; mkdir -p "$root"
  export ASSET_CACHE_URL=$base/asset-cache
  BUILD_CACHE_URL=$base/build-cache bash scripts/ci-build-cache.sh restore "$root" || echo "build-cache: restore failed: cold build"
  scripts/build-socialweb-computer.sh --platform linux/amd64 --commit "$sha" --build-root "$root" >/dev/null
  install -D -m 0755 -o r2 "$root/release/$sha/$bin" "$out"
  echo "path:$out" > "$spec"
fi
"$out" version | head -1
echo "requester $(cat "$spec")"
