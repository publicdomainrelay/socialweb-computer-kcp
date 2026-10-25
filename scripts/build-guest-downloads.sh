#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
versions="$root/internal/versions/versions.json"

usage() {
  cat >&2 <<EOF
usage: $0 --out DIR

Builds the pinned downloads a bidder's GuestControlPlane serves at /v1/downloads/<sha256>/<name>
(the socialweb-guest-downloads asset of internal/versions/versions.json) into DIR:
  opentunnel-x86_64, opentunnel-aarch64                       the assets.opentunnel release tarballs, sha256 verified
  wootty-x86_64, wootty-aarch64                               the guestDownloads.files.wootty image layers, sha256 verified
  socialweb-guest-agent-x86_64, socialweb-guest-agent-aarch64 cmd/socialweb-guest-agent (linux, CGO disabled)
each with a sha256sum-format <name>.sha256 sidecar. The tarballs and layers are kept in
\$GUEST_DOWNLOADS_CACHE by their sha256 when it is set.
EOF
  exit 2
}

out=""
while [ $# -gt 0 ]; do
  case "$1" in
    --out) out="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done
[ -n "$out" ] || usage
case "$out" in
  /tmp|/tmp/*) echo "build-guest-downloads: --out $out is under /tmp" >&2; exit 1 ;;
esac
for tool in curl jq tar sha256sum go; do
  command -v "$tool" >/dev/null 2>&1 || { echo "build-guest-downloads: $tool is required" >&2; exit 1; }
done
mkdir -p "$out"
out="$(cd "$out" && pwd -P)"
work="$(mktemp -d "$out/.work.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cache="${GUEST_DOWNLOADS_CACHE:-$work/cache}"
mkdir -p "$cache"

sha_of() { sha256sum "$1" | cut -d' ' -f1; }

fetch_pinned() {
  local url="$1" want="$2" auth="${3:-}" file="$cache/$2" part
  local args=()
  if [ -f "$file" ] && [ "$(sha_of "$file")" = "$want" ]; then
    echo "$file"
    return
  fi
  part="$file.part.$BASHPID"
  [ -z "$auth" ] || args=(-H "Authorization: Bearer $auth")
  curl "${args[@]}" -fSL --retry 5 --retry-all-errors --retry-delay 3 --connect-timeout 10 --speed-limit 10000 --speed-time 20 -s -o "$part" "$url" \
    || { rm -f "$part"; echo "build-guest-downloads: download failed: $url" >&2; exit 1; }
  if [ "$(sha_of "$part")" != "$want" ]; then
    echo "build-guest-downloads: $url has sha256 $(sha_of "$part"), pinned $want" >&2
    rm -f "$part"
    exit 1
  fi
  mv -f "$part" "$file"
  echo "$file"
}

registry_token() {
  local registry="$1" repository="$2" token
  token="$(curl -fsS --retry 5 --retry-all-errors --retry-delay 3 --connect-timeout 10 "https://$registry/token?scope=repository:$repository:pull" | jq -r '.token // empty')" \
    || { echo "build-guest-downloads: no pull token from $registry for $repository" >&2; exit 1; }
  [ -n "$token" ] || { echo "build-guest-downloads: $registry gave an empty pull token for $repository" >&2; exit 1; }
  echo "$token"
}

extract_member() {
  local name="$1" archive="$2" member="$3"
  mkdir -p "$work/$name"
  tar -xzf "$archive" -C "$work/$name" -- "$member"
  local src="$work/$name/$member"
  if [ -L "$src" ] || [ ! -f "$src" ]; then
    echo "build-guest-downloads: $member in $archive is not a regular file" >&2
    exit 1
  fi
  cp "$src" "$out/$name"
  chmod 0755 "$out/$name"
  (cd "$out" && sha256sum "$name" >"$name.sha256")
}

for pair in x86_64:amd64 aarch64:arm64; do
  arch="${pair%%:*}"
  goarch="${pair##*:}"
  pin="$(jq -c --arg p "linux/$goarch" '.assets.opentunnel.fetch[$p]' "$versions")"
  [ "$pin" != null ] || { echo "build-guest-downloads: assets.opentunnel pins nothing for linux/$goarch" >&2; exit 1; }
  [ "$(echo "$pin" | jq -r .archive)" = tar.gz ] || { echo "build-guest-downloads: the opentunnel linux/$goarch pin is not a tar.gz" >&2; exit 1; }
  member="$(echo "$pin" | jq -r '.members | keys[0]')"
  tarball="$(fetch_pinned "$(echo "$pin" | jq -r .url)" "$(echo "$pin" | jq -r .sha256)")"
  echo "build-guest-downloads: opentunnel-$arch from $(basename "$(echo "$pin" | jq -r .url)") member $member" >&2
  extract_member "opentunnel-$arch" "$tarball" "$member"

  pin="$(jq -c --arg p "linux/$goarch" '.guestDownloads.files.wootty.fetch[$p]' "$versions")"
  [ "$pin" != null ] || { echo "build-guest-downloads: guestDownloads.files.wootty pins nothing for linux/$goarch" >&2; exit 1; }
  [ "$(echo "$pin" | jq -r .kind)" = oci-layer ] || { echo "build-guest-downloads: the wootty linux/$goarch pin is not an oci-layer" >&2; exit 1; }
  registry="$(echo "$pin" | jq -r .registry)"
  repository="$(echo "$pin" | jq -r .repository)"
  sum="$(echo "$pin" | jq -r .sha256)"
  member="$(echo "$pin" | jq -r .member)"
  layer="$cache/$sum"
  if [ ! -f "$layer" ] || [ "$(sha_of "$layer")" != "$sum" ]; then
    layer="$(fetch_pinned "https://$registry/v2/$repository/blobs/sha256:$sum" "$sum" "$(registry_token "$registry" "$repository")")"
  fi
  echo "build-guest-downloads: wootty-$arch from $registry/$repository layer sha256:$sum member $member" >&2
  extract_member "wootty-$arch" "$layer" "$member"

  echo "build-guest-downloads: building socialweb-guest-agent-$arch" >&2
  (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -buildvcs=false \
    -ldflags='-s -w -buildid=' -o "$out/socialweb-guest-agent-$arch" ./cmd/socialweb-guest-agent)
  (cd "$out" && sha256sum "socialweb-guest-agent-$arch" >"socialweb-guest-agent-$arch.sha256")
done

(cd "$out" && sha256sum -c --quiet ./*.sha256) || { echo "build-guest-downloads: a download does not match its sidecar" >&2; exit 1; }
for f in "$out"/*.sha256; do
  echo "build-guest-downloads: $(cat "$f")" >&2
done
