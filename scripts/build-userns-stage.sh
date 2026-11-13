#!/usr/bin/env bash
# stage-assets inputs: deploy/swc-userns-stage
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
versions="$root/internal/versions/versions.json"

usage() {
  cat >&2 <<EOF
usage: $0 [--out DIR]

Builds swc-userns-stage (deploy/swc-userns-stage/stage.c, plan 0007 M0-VM-USERNS) for linux/amd64
inside the pinned guest builder image (internal/versions/versions.json .guestImages.builder), with
no network, as the calling uid. The output is byte-reproducible: a static, libc-free ELF with no
build id and no source paths. Prints the output path and its sha256.

  --out   where swc-userns-stage lands (default \$HOME/.tmp/userns-stage-build)

CONTAINER=podman selects podman (default docker). Rootless podman runs as root of its user namespace,
which is the calling uid, and builds the builder image with apt's sandbox user set to root through a
build-time mount (one uid in the namespace; the image does not keep the file).
EOF
  exit 2
}

out="$HOME/.tmp/userns-stage-build"
while [ $# -gt 0 ]; do
  case "$1" in
    --out) out="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done
case "$out" in
  /tmp|/tmp/*) echo "build-userns-stage: --out $out is under /tmp" >&2; exit 1 ;;
esac

src="$root/deploy/swc-userns-stage"
[ -f "$src/stage.c" ] || { echo "build-userns-stage: $src/stage.c is missing" >&2; exit 1; }
container="${CONTAINER:-docker}"
dockerfile="$root/$(jq -r '.guestImages.builder.dockerfile' "$versions")"
builder="localhost/socialweb-guest-builder:sha-$(sha256sum "$dockerfile" | cut -c1-12)"
rootless_podman=0
case "$(basename "$container")" in
  podman) [ "$(id -u)" = 0 ] || rootless_podman=1 ;;
esac
if ! "$container" image inspect "$builder" >/dev/null 2>&1; then
  echo "build-userns-stage: building the builder image $builder" >&2
  build_mounts=()
  if [ "$rootless_podman" = 1 ]; then
    mkdir -p "$out"
    printf 'APT::Sandbox::User "root";\n' >"$out/apt-sandbox-root.conf"
    build_mounts=(--security-opt label=disable -v "$out/apt-sandbox-root.conf:/etc/apt/apt.conf.d/00-sandbox-root:ro")
  fi
  "$container" build -q "${build_mounts[@]}" -t "$builder" -f "$dockerfile" "$(dirname "$dockerfile")" >&2
  rm -f "$out/apt-sandbox-root.conf"
fi

mkdir -p "$out"
user="$(id -u):$(id -g)"
[ "$rootless_podman" = 0 ] || user="0:0"
"$container" run --rm --network none --user "$user" --security-opt label=disable \
  -e LANG=C -e LC_ALL=C -e SOURCE_DATE_EPOCH=0 \
  -v "$src:/src:ro" -v "$out:/out" -w /src "$builder" \
  gcc -Os -static -nostdlib -ffreestanding -fno-stack-protector -fno-builtin -fno-pie -no-pie \
    -fno-asynchronous-unwind-tables -ffile-prefix-map=/src=. -Wall -Werror \
    -Wl,--build-id=none -s -o /out/swc-userns-stage stage.c

sha256sum "$out/swc-userns-stage"
