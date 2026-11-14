#!/usr/bin/env bash
# The release gate: every composition against a target, as many at once as there are driver accounts.
# Usage (from tests/): ./gate.sh <target.json> [--account <name>[,<name>...]] [<runbook>[:<entry>] ...]
# One "RESULT <runbook>:<entry> PASS|FAIL|SKIP <seconds> <note> [MEMORY <events>]" line each as it ends, then a summary
# table in gate order (with each run's VM memory events from its readings.memory: the test modules fail a run on an OOM
# kill and list pressure); exit 1 on a FAIL.
# Parallel: one run per account at a time (an account has one driver: the test host's account lock), so the accounts
# given (GATE_ACCOUNTS, else --account, default bob) are the slots; each slot has its own iroh key. A run holds the
# VMs of its entry's weight (WEIGHT below, default 1) out of GATE_VMS (4) so the target's bidders are not oversubscribed.
# An exclusive entry (EXCLUSIVE: it fills or counts bidders, or places VMs per host) runs alone: the gate drains first.
# GATE_JOBS=1 runs it serially in gate order.
# The heavy-job lock: held for the whole run on a target with local networks, for the build only on prod (no `network`).
# A failed run freezes (attach: the FROZEN line of its log) until GATE_FREEZE_TTL (900 s), then its slot goes on.
# SWC_COMPAT_REQUESTER (the previous rc's CLI, e.g. release:v0.0.0-rc165) runs 0006:compat; unset, it is skipped.
# GATE_SKIP="<runbook>:<entry>=<reason>;..." skips more (no ";" in a reason), with the reason in its RESULT line.
set -uo pipefail
# The heavy-job lock (hat-8's one heavy job at a time) covers the whole run only for a target with local networks (dev, staging:
# VMs and cores run here); a target without `network` (prod) runs its VMs on the prod bidders and holds the lock only for the build.
if [ -z "${SWC_HEAVY_HELD:-}" ] && jq -e '.network' "${1:?usage: gate.sh <target.json> [--account <name>[,<name>...]] [<runbook>[:<entry>] ...]}" > /dev/null 2>&1; then
  exec "$(dirname "$0")/../scripts/swc-heavy.sh" "$0" "$@"
fi
target=$(realpath "${1:?usage: gate.sh <target.json> [--account <name>[,<name>...]] [<runbook>[:<entry>] ...]}"); shift
accounts=${GATE_ACCOUNTS:-bob}
if [ "${1:-}" = --account ]; then accounts=$2; shift 2; fi
all="0001:run 0001:s2 0002:run 0003:run 0004:run 0005:run 0005:b 0005:r2fail 0006:run 0006:compat 0007:run 0007:restart 0008:run
  0009:run 0010:run 0011:run 0011:token 0012:run 0013:run 0013:c 0013:b
  0014:run 0014:concurrent 0014:exhausted 0014:backtoback 0015:run 0015:pressure 0016:run 0016:badsha"
ids=${*:-$all}
# VMs an entry holds at its peak (the rest hold one; host checks none)
declare -A WEIGHT=([0003:run]=3 [0012:run]=3 [0013:run]=2 [0013:c]=2 [0013:b]=2 [0006:run]=0 [0008:run]=0 [0011:token]=0 [0016:badsha]=0)
# runs alone: k8s nodes one per host and its tenant scale-downs (0001), a bidder core restarted (0007:restart), bidders
# filled to capacity or booked (0014)
EXCLUSIVE=" ${GATE_EXCLUSIVE:-0001:run 0001:s2 0007:restart 0014:run 0014:concurrent 0014:exhausted 0014:backtoback} "
cd "$(dirname "$0")"
unset ATPROTO_HANDLE ATPROTO_PASSWORD ATP_USERNAME ATP_PASSWORD ATPROTO_PDS
out=${GATE_OUT:-$HOME/.tmp/swc-system-tests/gate/$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "$out" && chmod 700 "$out"
if [ -n "${SWC_HEAVY_HELD:-}" ]; then
  (cd modules && cargo wasm -q) && cargo build -q -p swc-test-host || exit 2
else
  ../scripts/swc-heavy.sh sh -c '(cd modules && cargo wasm -q) && cargo build -q -p swc-test-host' || exit 2
fi
host=$PWD/target/debug/swc-test-host
export SWC_TARGET=$target SWC_READINGS=$out
skip_of() {
  case $1 in
    0010:*) echo "iOS paused (owner)"; return ;;
    0006:compat) [ -n "${SWC_COMPAT_REQUESTER:-}" ] || { echo "SWC_COMPAT_REQUESTER unset (the previous rc's CLI)"; return; } ;;
  esac
  tr ';' '\n' <<<"${GATE_SKIP:-}" | sed -n "s/^$1=//p" | head -1
}
# one entry as <account> on slot <n>: its RESULT line, and <out>/<runbook>-<entry>.tsv for the summary
run_one() {
  local id=$1 account=$2 slot=$3 r e why t0 res note rc doc s mem input
  r=${id%%:*}; e=${id#*:}; [ "$e" = "$id" ] && e=run
  why=$(skip_of "$r:$e")
  t0=$(date +%s)
  if [ -n "$why" ]; then
    res=SKIP note=$why
  elif [ ! -f "compositions/$r/composition.json" ]; then
    res=SKIP note="no composition"
  else
    input=()
    case $r in 0016) input=(--input "$(jq -c '{image: {url, sha256}, marker}' ../deploy/vm-images/ci-toolchain.json)") ;; esac
    SWC_IROH_KEY=$out/iroh-$slot "$host" "compositions/$r" --account "$account" --entry "$e" --on-fail freeze \
      --freeze-ttl "${GATE_FREEZE_TTL:-900}" "${input[@]}" > "$out/$r-$e.json" 2> "$out/$r-$e.log" < /dev/null
    rc=$?
    doc=$(cat "$out/$r-$e.json")
    if [ $rc = 0 ] && jq -e '.readings.skipped' <<<"$doc" > /dev/null 2>&1; then
      res=SKIP note=$(jq -r '.readings.skipped' <<<"$doc")
    elif [ $rc = 0 ]; then
      res=PASS note=$(jq -r '.dump // ""' <<<"$doc" 2>/dev/null)
    else
      res=FAIL note=$(jq -r '[.error, (.cleanupErrors // [] | join("; "))] | map(select(. != null and . != "")) | join(" | ") | .[0:200]' <<<"$doc" 2>/dev/null)
      [ -n "$note" ] || note="exit $rc: $(tail -1 "$out/$r-$e.log" | cut -c1-200)"
    fi
  fi
  s=$(( $(date +%s) - t0 ))
  mem=$(jq -r '[.readings.memory // [] | .[] | "\(.event) \(.vm)"] | unique | join(", ")' "$out/$r-$e.json" 2>/dev/null)
  echo "RESULT $r:$e $res $s $note${mem:+ MEMORY $mem} (${account})"
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$r:$e" "$res" "$s" "$note" "$mem" "$account" > "$out/$r-$e.tsv"
}
IFS=, read -ra slots <<<"$accounts"
jobs=${GATE_JOBS:-${#slots[@]}}
[ "$jobs" -le "${#slots[@]}" ] || jobs=${#slots[@]}
cap=${GATE_VMS:-4}
declare -A pid_slot=() pid_weight=()
free=()
for ((n = 0; n < jobs; n++)); do free+=("$n"); done
used=0 excl=0 queue=($ids)
g0=$(date +%s)
while [ ${#queue[@]} -gt 0 ] || [ ${#pid_slot[@]} -gt 0 ]; do
  left=()
  barrier=0
  for id in "${queue[@]}"; do
    w=${WEIGHT[$id]:-1}
    [[ $id != *:* ]] && w=${WEIGHT[$id:run]:-1}
    x=0; [[ $EXCLUSIVE == *" $id "* || $EXCLUSIVE == *" $id:run "* ]] && x=1
    if [ $barrier = 1 ] || [ $excl = 1 ] || [ ${#free[@]} = 0 ] || { [ $x = 1 ] && [ ${#pid_slot[@]} -gt 0 ]; } || [ $((used + w)) -gt "$cap" -a ${#pid_slot[@]} -gt 0 ]; then
      left+=("$id")
      # an exclusive entry is a barrier: nothing after it starts before it
      [ $x = 1 ] && barrier=1
      continue
    fi
    slot=${free[0]}; free=("${free[@]:1}")
    run_one "$id" "${slots[$slot]}" "$slot" &
    pid_slot[$!]=$slot pid_weight[$!]=$w
    used=$((used + w)); excl=$x
  done
  queue=("${left[@]}")
  [ ${#pid_slot[@]} -gt 0 ] || continue
  wait -n -p done_pid
  free+=("${pid_slot[$done_pid]}")
  used=$((used - pid_weight[$done_pid])); excl=0
  unset "pid_slot[$done_pid]" "pid_weight[$done_pid]"
done
failed=0
printf '\n| composition | result | seconds | note | memory | account |\n| --- | --- | --- | --- | --- | --- |\n'
for id in $ids; do
  r=${id%%:*}; e=${id#*:}; [ "$e" = "$id" ] && e=run
  f=$out/$r-$e.tsv
  [ -f "$f" ] || { printf '| %s | FAIL | 0 | no result |  |  |\n' "$r:$e"; failed=1; continue; }
  awk -F'\t' '{printf "| %s | %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $5, $6}' "$f"
  [ "$(cut -f2 "$f")" = FAIL ] && failed=1
  cat "$f" >> "$out/results.tsv"
done
echo "wall: $(( $(date +%s) - g0 )) s ($jobs at once, $cap VMs); logs and readings: $out"
exit $failed
