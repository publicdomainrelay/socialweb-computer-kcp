#!/bin/sh
# scripts/swc-heavy.sh <command...>: runs the command holding one of hat-8's heavy-job slots (SWC_HEAVY_SLOTS, default 2: a
# local gate or make check-pins, release and guest image builds, a toolchain image, staging stand-up and suites). A slot is
# an flock -n on /var/tmp/swc-heavy.lock.d/slot-<n> (any account: the directory and its files are world-writable), taken
# together with a SHARED flock on the old lock directory SWC_HEAVY_LOCK (default /var/tmp/swc-heavy.lock), so a copy of this
# script from before the slots, which holds that directory exclusively, still excludes every job and is excluded by them.
# SWC_HEAVY_EXCLUSIVE=1 takes the whole box instead (VMM tests, anything that measures or needs every core and every port):
# it waits for the running jobs to finish while new ones hold off, then holds the directory exclusively.
# SWC_HEAVY_WAIT=<secs> gives up after that long. SWC_HEAVY_HELD=1 is set for the command (tests/gate.sh nests on it).
set -eu
lock="${SWC_HEAVY_LOCK:-/var/tmp/swc-heavy.lock}"
slots="${SWC_HEAVY_SLOTS:-2}"
dir="${SWC_HEAVY_SLOTS_DIR:-$lock.d}"
want="$dir/exclusive-wanted"
if [ -n "${SWC_HEAVY_WAIT:-}" ]; then deadline=$(( $(date +%s) + SWC_HEAVY_WAIT )); else deadline=""; fi
left() { if [ -z "$deadline" ]; then echo 31536000; else echo $(( deadline - $(date +%s) )); fi; }
giveup() { echo "swc-heavy: gave up after ${SWC_HEAVY_WAIT}s waiting for $lock" >&2; exit 1; }
[ -e "$lock" ] || mkdir -m 0755 "$lock" 2>/dev/null || true
if [ ! -d "$dir" ]; then
  (umask 000; mkdir -m 0777 "$dir" 2>/dev/null || true)
fi
exec 9<"$lock"
if [ "${SWC_HEAVY_EXCLUSIVE:-}" = 1 ]; then
  echo "swc-heavy: waiting for the whole box ($lock, exclusive)" >&2
  (umask 000; : >"$want") 2>/dev/null || true
  trap 'rm -f "$want"' EXIT INT TERM
  [ "$(left)" -gt 0 ] || giveup
  flock -x -w "$(left)" 9 || giveup
  rm -f "$want"
  trap - EXIT INT TERM
  SWC_HEAVY_HELD=1 exec "$@"
fi
echo "swc-heavy: waiting for one of $slots slots in $dir" >&2
n=0
while :; do
  # an exclusive job waits: let the running ones drain (a marker older than 30 minutes is a dead waiter's)
  if [ -n "$(find "$want" -mmin -30 2>/dev/null)" ]; then
    [ "$(left)" -gt 0 ] || giveup
    sleep 2
    continue
  fi
  i=1
  while [ "$i" -le "$slots" ]; do
    (umask 000; : >>"$dir/slot-$i") 2>/dev/null || true
    exec 8>>"$dir/slot-$i"
    if flock -n 8; then
      n=$i
      break
    fi
    exec 8>&-
    i=$((i + 1))
  done
  [ "$n" -eq 0 ] || break
  [ "$(left)" -gt 0 ] || giveup
  sleep 2
done
echo "swc-heavy: slot $n, waiting for $lock to be free of exclusive holders" >&2
[ "$(left)" -gt 0 ] || giveup
flock -s -w "$(left)" 9 || giveup
SWC_HEAVY_HELD=1 exec "$@"
