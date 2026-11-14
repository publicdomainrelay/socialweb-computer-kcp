#!/usr/bin/env bash
# usage: scripts/devsup-stop-race.sh <socialweb-computer binary> <n> <delay seconds> [state dir]
# Reproduces the restart-while-the-previous-stop-is-in-flight race: `dev up --profile quick` (the proc backend, a real devsup), then n
# times `dev down` SIGKILLed <delay> s after it started (devsup keeps stopping its units in order: bao, core, kcp, front, kine, then
# exits) and `dev up` at once. Every up must succeed and its kcp stay ready. Without the devsup `stopping` handling about half of the
# ups failed ("is Unknown (... is deactivating)", or a kcp whose kine was stopped under it: not ready after 60 s).
set -u
B=${1:?binary}; N=${2:-5}; D=${3:-1.0}; ST=${4:-/var/tmp/swc-devsup-race}
ARGS=(--instance devsup-race --state-dir "$ST")
for p in $(ps -eo pid,cmd | awk -v s="$ST" 'index($0, "internal devsup --state " s) && !/awk/ {print $1}'); do kill "$p"; done; sleep 9
rm -rf "$ST"
timeout 200 "$B" dev up --profile quick "${ARGS[@]}" --host-backend proc > "$ST.up0.log" 2>&1 || { echo "first up failed"; tail -3 "$ST.up0.log"; exit 1; }
ok=0; bad=0
for i in $(seq "$N"); do
  "$B" dev down "${ARGS[@]}" > /dev/null 2>&1 & dp=$!
  sleep "$D"; kill -9 "$dp" 2>/dev/null; wait "$dp" 2>/dev/null
  t0=$(date +%s.%N)
  timeout 200 "$B" dev up --profile quick "${ARGS[@]}" --host-backend proc > "$ST.up-$i.log" 2>&1; rc=$?
  t1=$(date +%s.%N)
  sleep 6
  url=$(grep -m1 '"kcpURL"' "$ST.up-$i.log" | sed 's/.*"\(https[^"]*\)".*/\1/')
  code=$(curl -sk -m 5 -o /dev/null -w '%{http_code}' "$url/readyz" 2>/dev/null)
  if [ "$rc" = 0 ] && [ "$code" = 200 ]; then r=OK; ok=$((ok + 1)); else r=BAD; bad=$((bad + 1)); fi
  echo "ITER $i $r rc=$rc up=$(printf '%.1f' "$(echo "$t1 - $t0" | bc)")s readyz=$code"
done
echo "RESULT delay=$D ok=$ok bad=$bad"
"$B" dev down "${ARGS[@]}" > /dev/null 2>&1
