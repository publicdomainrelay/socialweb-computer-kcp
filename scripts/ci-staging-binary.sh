#!/usr/bin/env bash
# The socialweb-computer the staging run (test, compositions) puts under test, on the CI VM as /home/r2/swc-under-test, and the
# requester spec the staging VMs get it by (/home/r2/requester.spec):
#   sha:<commit>   a published binary (main/<commit>/): this commit's, or the nearest main ancestor's that is published when nothing a
#                  release binary is built from changed since it (internal/ cmd/ api/ rust/ lexicons/ deploy/ except deploy/vm-images/,
#                  go.mod go.sum, the build and asset scripts): the CI VM and every staging VM DOWNLOAD it from the public bucket over
#                  HTTPS, checked against its SHA256SUMS (fast; a 589 MB put through the requester's ssh took 12 to 38 min and broke a pipe)
#   oci:<ref>      otherwise this commit's build, pushed to ttl.sh (an anonymous, ephemeral OCI registry: no credential, so a fork's run does
#                  it too) with oras by scripts/oci-push.sh, as <host/name>@sha256:<manifest digest>: the staging VMs pull it by that
#                  digest, which oras checks. The `binary` workflow builds it once for the commit (this script with --push, its artifact
#                  oci/binary.ref holds the reference); test and compositions pull that one (after: [binary]); with none (the workflow
#                  did not run, or its ttl.sh tag is gone) they build it here, as it does
#   path:<file>    when ttl.sh is down: the test host puts the file on the staging VMs (request.put, SWC_BINARY; slow)
# A `sha:` binary reports its own commit's version: the tests run this tree's against it.
# scripts/ci-staging-binary.sh --push: only what the `binary` workflow does: nothing when a published binary serves, else build, push, and
# keep the reference as the run's artifact (oci/binary.ref).
set -euo pipefail
mode=${1:-use}
base=https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com
bin=socialweb-computer-linux-amd64
out=/home/r2/swc-under-test spec=/home/r2/requester.spec own="-o r2"
# the `binary` workflow's VM has no requester account
[ "$mode" = --push ] && { out=/root/swc-under-test spec=/root/requester.spec own=; }
sha=$(git rev-parse HEAD)
published() { curl -fsI -o /dev/null "$base/main/$1/binaries/SHA256SUMS"; }
# what a release binary is built from
relevant() { git diff --name-only "$1" HEAD | grep -vE '^deploy/vm-images/' | grep -qE '^(internal/|cmd/|api/|rust/|lexicons/|deploy/|go\.(mod|sum)$|scripts/(build-socialweb-computer|stage-assets|asset-cache|ci-build-cache|build-guest-image)[^/]*$)'; }
pick=
if published "$sha"; then
  pick=$sha
else
  # the nearest main ancestor (first parent, at most 20 back from the fork point) that is published and has no binary change since: a main
  # commit whose publish never ran (superseded) is skipped
  fork=$(git merge-base HEAD origin/main 2>/dev/null || true)
  if [ -n "$fork" ]; then
    for c in $(git rev-list --first-parent -n 20 "$fork"); do
      relevant "$c" && break
      if published "$c"; then pick=$c; break; fi
    done
  fi
fi
if [ -n "$pick" ]; then
  [ "$mode" = --push ] && { echo "binary: main/$pick is published and serves: nothing to build"; exit 0; }
  echo "binary: main/$pick (published; $([ "$pick" = "$sha" ] && echo "this commit's" || echo "the nearest published main ancestor, nothing a binary is built from changed since"))"
  tmp=$(mktemp -d)
  curl -fsSL --retry 3 -o "$tmp/$bin" "$base/main/$pick/binaries/$bin"
  curl -fsSL --retry 3 "$base/main/$pick/binaries/SHA256SUMS" | grep "  $bin\$" > "$tmp/SUMS"
  (cd "$tmp" && sha256sum -c --quiet SUMS)
  install -D -m 0755 $own "$tmp/$bin" "$out"
  rm -rf "$tmp"
  echo "sha:$pick" > "$spec"
else
  # this commit's build, as the `binary` workflow of this commit pushed it to ttl.sh, if it did
  ref=
  if [ "$mode" != --push ] && [ -n "${KNOTLESS_ARTIFACTS_URL:-}" ]; then
    ref=$(curl -sf -H "Authorization: Bearer $KNOTLESS_ARTIFACTS_TOKEN" "$KNOTLESS_ARTIFACTS_URL?workflow=binary&name=oci/binary.ref" || true)
    case "$ref" in ttl.sh/swc-*@sha256:????????????????????????????????????????????????????????????????) ;; *) ref= ;; esac
  fi
  if [ -n "$ref" ]; then
    echo "binary: this commit's build from the binary workflow, $ref"
    tmp=$(mktemp -d)
    oras=$(bash scripts/ci-oras.sh)
    if "$oras" pull --no-tty "$ref" -o "$tmp" && [ -f "$tmp/$bin" ]; then
      install -D -m 0755 $own "$tmp/$bin" "$out"
      rm -rf "$tmp"
      echo "oci:$ref" > "$spec"
    else
      echo "binary: pulling it failed (the ttl.sh tag expired?): building here"
      ref=
    fi
  fi
  if [ -z "$ref" ]; then
    echo "binary: this commit's build (not published, and the branch changed what a binary is built from)"
    if bash scripts/knotless-ci-baked.sh; then :; else bash scripts/knotless-ci-toolchain.sh --deno; fi
    . /opt/ci/env
    root=/root/branch-build; mkdir -p "$root"
    export ASSET_CACHE_URL=$base/asset-cache
    BUILD_CACHE_URL=$base/build-cache bash scripts/ci-build-cache.sh restore "$root" || echo "build-cache: restore failed: cold build"
    scripts/build-socialweb-computer.sh --platform linux/amd64 --commit "$sha" --build-root "$root" >/dev/null
    install -D -m 0755 $own "$root/release/$sha/$bin" "$out"
    # the same file under the name the VMs pull it by, to ttl.sh for the run's time (6h; the tag is the TTL)
    pushdir=$(mktemp -d); ln "$out" "$pushdir/$bin" 2>/dev/null || cp "$out" "$pushdir/$bin"
    if ref=$(bash scripts/oci-push.sh "$pushdir/$bin" 6h); then
      echo "binary: pushed to $ref"
      echo "oci:$ref" > "$spec"
      if [ "$mode" = --push ]; then mkdir -p oci && echo "$ref" > oci/binary.ref; fi
    else
      echo "binary: ttl.sh refused the push (down?): the test host puts the file instead"
      echo "path:$out" > "$spec"
    fi
    rm -rf "$pushdir"
  fi
fi
"$out" version | head -1
echo "requester $(cat "$spec")"
