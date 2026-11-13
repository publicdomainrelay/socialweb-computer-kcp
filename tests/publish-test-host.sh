#!/bin/sh
# What knotless CI's test.yml downloads instead of building on its VM: the static (musl) swc-test-host and the module
# wasm of this checkout's committed tests/ tree, built on hat-8 and published to the bucket at
# tests/<git rev-parse HEAD:tests>/ (swc-test-host, test-modules.tar.gz, then SHA256SUMS) with scripts/publish-spaces.ts
# and the bucket credential on the build host (~/.spaces_swc_kcp_source). Run it from a checkout before pushing a
# tests/ change:  tests/publish-test-host.sh        (BUILD_HOST, default hat-8)
set -eu
host="${BUILD_HOST:-hat-8}"
base=https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com/tests
if [ "${1:-}" != --on-build-host ]; then
	cd "$(dirname "$0")/.."
	tree="$(git rev-parse HEAD:tests)"
	if curl -sfo /dev/null "$base/$tree/SHA256SUMS"; then
		echo "$base/$tree/: already published"
		exit 0
	fi
	dir=".tmp/swc-tests/$tree"
	ssh "$host" "rm -rf $dir && mkdir -p $dir"
	git archive HEAD tests scripts/publish-spaces.ts | ssh "$host" "tar -C $dir -xf -"
	exec ssh "$host" "sh $dir/tests/publish-test-host.sh --on-build-host $tree $dir"
fi
tree="$2" dir="$HOME/$3"
[ "$(hostname -s)" = "$host" ] || { echo "publish-test-host: not on $host" >&2; exit 1; }
cd "$dir/tests"
export CARGO_TARGET_DIR="$HOME/.tmp/swc-tests/target"
out="$dir/out" sums="$dir/sums"
rm -rf "$out" "$sums" && mkdir -p "$out" "$sums"
cp "$(./build-static-host.sh)" "$out/swc-test-host"
(cd modules && CARGO_TARGET_DIR="$PWD/target" cargo wasm >&2)
tar -czf "$out/test-modules.tar.gz" modules/*/module.json modules/target/wasm32-unknown-unknown/release/*.wasm
(cd "$out" && sha256sum swc-test-host test-modules.tar.gz > "$sums/SHA256SUMS" && cat "$sums/SHA256SUMS")
. "$HOME/.spaces_swc_kcp_source"
publish() { deno run -A --no-lock "$dir/scripts/publish-spaces.ts" --dir "$1" --prefix "tests/$tree" --content-addressed; }
publish "$out"
publish "$sums"
rm -rf "$dir"
echo "$base/$tree/"
