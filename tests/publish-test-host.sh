#!/bin/sh
# What knotless CI's test.yml downloads instead of building on its VM, built on hat-8 from this checkout's committed
# trees and published to the bucket with scripts/publish-spaces.ts and the bucket credential on the build host
# (~/.spaces_swc_kcp_source), SHA256SUMS last:
#   tests/<git rev-parse HEAD:tests>/  the static (musl) swc-test-host and the module wasm (test-modules.tar.gz)
#   meshd/<git rev-parse HEAD:rust>/   the static socialweb-meshd-x86_64 (rust/build.sh) the multi-VM layout puts on
#                                      each VM (SWC_MESHD)
# Run it from a checkout before pushing a tests/ or rust/ change:  tests/publish-test-host.sh   (BUILD_HOST: hat-8)
set -eu
host="${BUILD_HOST:-hat-8}"
base=https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com
if [ "${1:-}" != --on-build-host ]; then
	cd "$(dirname "$0")/.."
	tree="$(git rev-parse HEAD:tests)" rtree="$(git rev-parse HEAD:rust)"
	if curl -sfo /dev/null "$base/tests/$tree/SHA256SUMS" && curl -sfo /dev/null "$base/meshd/$rtree/SHA256SUMS"; then
		echo "$base/tests/$tree/ and $base/meshd/$rtree/: already published"
		exit 0
	fi
	dir=".tmp/swc-tests/$tree"
	ssh "$host" "rm -rf $dir && mkdir -p $dir"
	git archive HEAD tests rust scripts/publish-spaces.ts | ssh "$host" "tar -C $dir -xf -"
	exec ssh "$host" "sh $dir/tests/publish-test-host.sh --on-build-host $tree $rtree $dir"
fi
tree="$2" rtree="$3" dir="$HOME/$4"
[ "$(hostname -s)" = "$host" ] || { echo "publish-test-host: not on $host" >&2; exit 1; }
. "$HOME/.spaces_swc_kcp_source"
publish() { deno run -A --no-lock "$dir/scripts/publish-spaces.ts" --dir "$1" --prefix "$2" --content-addressed; }
if ! curl -sfo /dev/null "$base/tests/$tree/SHA256SUMS"; then
	cd "$dir/tests"
	out="$dir/out" sums="$dir/sums"
	rm -rf "$out" "$sums" && mkdir -p "$out" "$sums"
	cp "$(CARGO_TARGET_DIR="$HOME/.tmp/swc-tests/target" ./build-static-host.sh)" "$out/swc-test-host"
	(cd modules && CARGO_TARGET_DIR="$PWD/target" cargo wasm >&2)
	tar -czf "$out/test-modules.tar.gz" modules/*/module.json modules/target/wasm32-unknown-unknown/release/*.wasm
	(cd "$out" && sha256sum swc-test-host test-modules.tar.gz > "$sums/SHA256SUMS" && cat "$sums/SHA256SUMS")
	publish "$out" "tests/$tree"
	publish "$sums" "tests/$tree"
fi
if ! curl -sfo /dev/null "$base/meshd/$rtree/SHA256SUMS"; then
	m="$dir/meshd" ms="$dir/meshd-sums"
	rm -rf "$m" "$ms" && mkdir -p "$m" "$ms"
	MESHD_DIST="$m" bash "$dir/rust/build.sh" x86_64 >&2
	(cd "$m" && rm -f ./*.sha256 && sha256sum socialweb-meshd-x86_64 > "$ms/SHA256SUMS" && cat "$ms/SHA256SUMS")
	publish "$m" "meshd/$rtree"
	publish "$ms" "meshd/$rtree"
fi
rm -rf "$dir"
echo "$base/tests/$tree/ $base/meshd/$rtree/"
