#!/usr/bin/env bash
set -u

mode="${1:---fast}"
case "$mode" in
  --fast) run=fast ;;
  --local) run=local ;;
  --full) run=full ;;
  *)
    echo "usage: $0 [--fast|--local|--full]" >&2
    echo "  --fast   the build gate: gofmt, go build, apigen, go vet, cross-OS and release builds" >&2
    echo "  --local  the proofs this host can run (kcp/kine/k3s/postgres); lists what it skipped" >&2
    echo "  --full   everything, and a skip is a failure; needs the live artifacts on this host" >&2
    exit 2
    ;;
esac

fail() {
  echo "check.sh: FAILED $1" >&2
  exit 1
}

unformatted="$(gofmt -l api internal cmd test)"
if [ -n "$unformatted" ]; then
  echo "$unformatted" >&2
  fail gofmt
fi
echo "check.sh: ok gofmt"

go build ./... || fail "go build"
echo "check.sh: ok go build"

gen="$(mktemp -d)"
genrepo="$gen/repo"
generatedlist="$(go run ./cmd/socialweb-apigen -root . -list)" || { rm -rf "$gen"; fail "apigen"; }
go run ./cmd/socialweb-apigen -root . -out "$genrepo" || { rm -rf "$gen"; fail "apigen"; }
drift=0
compared=0
for rel in $generatedlist; do
  compared=$((compared + 1))
  if ! diff -u "$rel" "$genrepo/$rel" >&2; then
    drift=1
  fi
done
rm -rf "$gen"
if [ "$compared" -eq 0 ]; then
  fail "apigen generated nothing to compare"
fi
if [ "$drift" -ne 0 ]; then
  echo "check.sh: the files above are generated from api/v1alpha1 and differ from what it generates now; run: go generate ./api/..." >&2
  fail "apigen drift"
fi
echo "check.sh: ok apigen ($compared generated files, no drift from api/v1alpha1)"

go vet ./... || fail "go vet"
echo "check.sh: ok go vet"

for module in $(jq -r '[.assets[].fetch // {} | .[] | select(.kind == "go") | .module // empty] | unique[]' internal/versions/versions.json); do
  (cd "$module" && go build -o /dev/null ./... && go vet ./...) || fail "go build and go vet of the asset module $module"
  echo "check.sh: ok go build and go vet of the asset module $module"
done

crossfailed=""
for target in darwin/arm64 darwin/amd64 windows/amd64 windows/arm64 linux/arm64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  if ! GOOS="$goos" GOARCH="$goarch" go build ./...; then
    crossfailed="$crossfailed build:$target"
    continue
  fi
  if ! GOOS="$goos" GOARCH="$goarch" go vet ./...; then
    crossfailed="$crossfailed vet:$target"
    continue
  fi
  if ! GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -tags release -o /dev/null ./cmd/socialweb-computer; then
    crossfailed="$crossfailed release-build:$target"
    continue
  fi
  echo "check.sh: ok go build and go vet for $target"
done
if [ -n "$crossfailed" ]; then
  echo "check.sh: the tree must build and vet for every GOOS/GOARCH above; an OS-specific file needs its build tag and a twin for the other OSes (plans 0011, 0012)" >&2
  fail "cross-OS gate:$crossfailed"
fi

releasedir="$(mktemp -d)"
for target in $(jq -r '.platforms | to_entries[] | select(.value.status == "release") | .key' internal/versions/versions.json); do
  goos="${target%/*}"
  goarch="${target#*/}"
  if ! GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -tags release -trimpath -o "$releasedir/$goos-$goarch" ./cmd/socialweb-computer; then
    rm -rf "$releasedir"
    fail "release build $target"
  fi
  rm -f "$releasedir/$goos-$goarch"
  echo "check.sh: ok release build $target"
done
rm -rf "$releasedir"

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

rust_check() {
  if ! command -v cargo >/dev/null 2>&1; then
    if [ "$run" = full ]; then
      fail "cargo not found; --full builds and tests rust/socialweb-meshd"
    fi
    echo "check.sh: skipped cargo test (cargo not found, so rust/socialweb-meshd is not tested on this host)"
    return
  fi
  if cargo fmt --version >/dev/null 2>&1; then
    cargo fmt --check --manifest-path rust/Cargo.toml || fail "cargo fmt"
    echo "check.sh: ok cargo fmt"
  else
    echo "check.sh: skipped cargo fmt (rustfmt not installed)"
  fi
  gates=""
  if [ "$run" != fast ]; then
    if sudo -n true >/dev/null 2>&1; then
      gates="SOCIALWEB_MESHD_NETNS=1 SOCIALWEB_MESHD_K3S=1"
    elif [ "$run" = full ]; then
      fail "sudo -n unavailable; --full runs the meshd netns and k3s proofs, which need CAP_NET_ADMIN"
    fi
  fi
  env $gates cargo test --locked --manifest-path rust/Cargo.toml || fail "cargo test"
  if [ -n "$gates" ]; then
    echo "check.sh: ok cargo test (rust/, with the netns and k3s proofs)"
  else
    echo "check.sh: ok cargo test (rust/; the netns and k3s proofs need SOCIALWEB_MESHD_NETNS=1 / SOCIALWEB_MESHD_K3S=1 and sudo -n)"
  fi
}
if [ "$run" != fast ]; then
  rust_check
fi

if [ "$run" != fast ]; then
  hostplat="$(go env GOOS)_$(go env GOARCH)"
  if [ -n "$(ls internal/assets/blobs/"$hostplat"/*.tar.zst 2>/dev/null)" ]; then
    verifydir="$(mktemp -d)"
    if ! CGO_ENABLED=0 go build -tags release -trimpath -o "$verifydir/socialweb-computer" ./cmd/socialweb-computer; then
      rm -rf "$verifydir"
      fail "release build for the embedded asset check ($hostplat)"
    fi
    if ! "$verifydir/socialweb-computer" dev verify-assets >/dev/null; then
      rm -rf "$verifydir"
      fail "embedded assets extract and verify ($hostplat)"
    fi
    rm -rf "$verifydir"
    echo "check.sh: ok embedded assets extract and verify ($hostplat)"
  elif [ "$run" = full ]; then
    fail "no staged blobs for $hostplat; --full extracts every embedded asset: run scripts/stage-assets.sh"
  else
    echo "check.sh: skipped embedded asset extraction (no staged blobs for $hostplat; run scripts/stage-assets.sh)"
  fi
fi

case "$run" in
  local)
    if ! SOCIALWEB_LOCAL=1 go test ./... -count=1 -timeout 60m -v >"$log" 2>&1; then
      grep -E -- '^(--- FAIL|FAIL)' "$log" | head -30 >&2
      fail "go test local"
    fi
    skipped="$(grep -c -- '^--- SKIP' "$log" || true)"
    echo "check.sh: ok go test local ($skipped artifact-gated tests skipped and named below)"
    grep -- '^--- SKIP' "$log" | sed 's/^/  /' || true
    ;;
  full)
    if ! SOCIALWEB_REQUIRE_LIVE=1 go test ./... -count=1 -timeout 60m -v >"$log" 2>&1; then
      echo "check.sh: --full needs live inputs this host must be handed; the failing tests and the input each names:" >&2
      grep -E -- '^(--- FAIL|[[:space:]]+[A-Za-z0-9_./-]+\.go:[0-9]+:)' "$log" | head -60 >&2
      fail "go test full"
    fi
    echo "check.sh: ok go test full (nothing skipped)"
    ;;
esac
