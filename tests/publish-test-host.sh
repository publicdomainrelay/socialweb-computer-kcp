#!/bin/sh
# What knotless CI's test.yml downloads instead of building on its VM, built from this checkout's committed trees and
# published to the bucket with scripts/publish-spaces.ts: by knotless CI's testhost workflow on every push to main, or by hand
# on hat-8 (the bucket credential on the build host, ~/.spaces_swc_kcp_source), SHA256SUMS last:
#   tests/<git rev-parse HEAD:tests>/  the static (musl) swc-test-host and the module wasm (test-modules.tar.gz)
#   meshd/<git rev-parse HEAD:rust>/   the static socialweb-meshd-x86_64 (rust/build.sh) the multi-VM layout puts on
#                                      each VM (SWC_MESHD)
# Run it from a checkout before pushing a tests/ or rust/ change:  tests/publish-test-host.sh   (BUILD_HOST: hat-8)
set -eu
host="${BUILD_HOST:-hat-8}"
# the build half runs in place when BUILD_HOST is local or names this host (the build host itself, no ssh to itself)
local=false
case "$host" in local) local=true ;; *) [ "$host" = "$(hostname -s)" ] && local=true ;; esac
on() { if $local; then sh -c "cd && $1"; else ssh "$host" "$1"; fi; }
base=https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com
if [ "${1:-}" != --on-build-host ]; then
	cd "$(dirname "$0")/.."
	tree="$(git rev-parse HEAD:tests)" rtree="$(git rev-parse HEAD:rust)"
	if curl -sfo /dev/null "$base/tests/$tree/SHA256SUMS" && curl -sfo /dev/null "$base/meshd/$rtree/SHA256SUMS"; then
		echo "$base/tests/$tree/ and $base/meshd/$rtree/: already published"
		exit 0
	fi
	dir=".tmp/swc-tests/$tree"
	on "rm -rf $dir && mkdir -p $dir"
	git archive HEAD tests rust scripts/publish-spaces.ts | on "tar -C $dir -xf -"
	on "sh $dir/tests/publish-test-host.sh --on-build-host $tree $rtree $dir"
	exit
fi
tree="$2" rtree="$3" dir="$HOME/$4"
$local || { echo "publish-test-host: not on $host" >&2; exit 1; }
# the build host's key file, else SPACES_* in the environment (knotless CI's testhost workflow)
[ ! -f "$HOME/.spaces_swc_kcp_source" ] || . "$HOME/.spaces_swc_kcp_source"
# no bucket key (a branch's CI run): nothing is published, what is not already published is built into DIST, an absolute path
# (knotless CI's testhost workflow keeps it as the run's artifacts; `test`, `vmm` and `compositions` read it)
if [ -z "${SPACES_ACCESS_KEY_ID:-}" ]; then
	: "${DIST:?no SPACES_ACCESS_KEY_ID and no DIST to build into}"
	mkdir -p "$DIST"
	publish() { :; }
else
	: "${SPACES_BUCKET:?}" "${SPACES_REGION:?}" "${SPACES_ACCESS_KEY_ID:?}" "${SPACES_SECRET_ACCESS_KEY:?}"
	DIST=""
	publish() { deno run -A --no-lock "$dir/scripts/publish-spaces.ts" --dir "$1" --prefix "$2" --content-addressed; }
fi
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
	[ -z "$DIST" ] || { cp "$out"/swc-test-host "$out"/test-modules.tar.gz "$DIST/" && cp "$sums/SHA256SUMS" "$DIST/tests.SHA256SUMS"; }
fi
if ! curl -sfo /dev/null "$base/meshd/$rtree/SHA256SUMS"; then
	m="$dir/meshd" ms="$dir/meshd-sums"
	rm -rf "$m" "$ms" && mkdir -p "$m" "$ms"
	# rust/rust-toolchain.toml pins rustc and its musl targets, which rustup fetches on first use: installed up front, retried: a
	# download that dies mid-cargo (curl 35, a TLS record error) must not fail the publish
	for i in 1 2 3 4; do
		(cd "$dir/rust" && rustup toolchain install >&2) && break
		echo "rustup toolchain install: attempt $i failed" >&2
		[ "$i" -lt 4 ] || exit 1
		sleep $((i * 15))
	done
	MESHD_DIST="$m" bash "$dir/rust/build.sh" x86_64 >&2
	(cd "$m" && rm -f ./*.sha256 && sha256sum socialweb-meshd-x86_64 > "$ms/SHA256SUMS" && cat "$ms/SHA256SUMS")
	publish "$m" "meshd/$rtree"
	publish "$ms" "meshd/$rtree"
	[ -z "$DIST" ] || { cp "$m/socialweb-meshd-x86_64" "$DIST/" && cp "$ms/SHA256SUMS" "$DIST/meshd.SHA256SUMS"; }
fi
rm -rf "$dir"
if [ -z "$DIST" ]; then echo "$base/tests/$tree/ $base/meshd/$rtree/"; else echo "built into $DIST (unpublished, no bucket key)"; fi
