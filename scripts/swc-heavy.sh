#!/bin/sh
# scripts/swc-heavy.sh <command...>: runs the command holding hat-8's heavy-job lock, one heavy job at a time (VMM tests,
# a local gate or make check-pins, release and guest image builds, staging stand-up and suites). The lock is an exclusive
# flock on the directory SWC_HEAVY_LOCK (default /var/tmp/swc-heavy.lock, any account: a directory, as a sticky /var/tmp
# refuses opening another account's file there with O_CREAT); SWC_HEAVY_WAIT=<secs> gives up after that long.
set -eu
lock="${SWC_HEAVY_LOCK:-/var/tmp/swc-heavy.lock}"
[ -e "$lock" ] || mkdir -m 0755 "$lock" 2>/dev/null || true
echo "swc-heavy: waiting for $lock" >&2
SWC_HEAVY_HELD=1 exec flock ${SWC_HEAVY_WAIT:+-w "$SWC_HEAVY_WAIT"} "$lock" "$@"
