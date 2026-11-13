#!/usr/bin/env bash
# The release gate: every composition, in order, one at a time, against a target, as one account.
# Usage (from tests/): ./gate.sh <target.json> [--account <name>] [<runbook>[:<entry>] ...]
# One "RESULT <runbook>:<entry> PASS|FAIL|SKIP <seconds> <note>" line each, then a summary table; exit 1 on a FAIL.
# A failed run freezes (attach: the FROZEN line of its log) until GATE_FREEZE_TTL (900 s), then the gate goes on.
# SWC_COMPAT_REQUESTER (the previous rc's CLI, e.g. release:v0.0.0-rc165) runs 0006:compat; unset, it is skipped.
# GATE_SKIP="<runbook>:<entry>=<reason>;..." skips more (no ";" in a reason), with the reason in its RESULT line.
set -uo pipefail
target=$(realpath "${1:?usage: gate.sh <target.json> [--account <name>] [<runbook>[:<entry>] ...]}"); shift
account=bob
if [ "${1:-}" = --account ]; then account=$2; shift 2; fi
all="0001:run 0001:s2 0002:run 0003:run 0004:run 0005:run 0005:b 0005:r2fail 0006:run 0006:compat 0007:run 0008:run
  0009:run 0010:run 0011:run 0011:token 0012:run 0013:run 0013:c 0013:b"
ids=${*:-$all}
cd "$(dirname "$0")"
unset ATPROTO_HANDLE ATPROTO_PASSWORD ATP_USERNAME ATP_PASSWORD ATPROTO_PDS
out=${GATE_OUT:-$HOME/.tmp/swc-system-tests/gate/$(date -u +%Y%m%dT%H%M%SZ)}
mkdir -p "$out" && chmod 700 "$out"
(cd modules && cargo wasm -q) && cargo build -q -p swc-test-host || exit 2
host=$PWD/target/debug/swc-test-host
export SWC_TARGET=$target SWC_READINGS=$out
skip_of() {
  case $1 in
    0010:*) echo "iOS paused (owner)"; return ;;
    0006:compat) [ -n "${SWC_COMPAT_REQUESTER:-}" ] || { echo "SWC_COMPAT_REQUESTER unset (the previous rc's CLI)"; return; } ;;
  esac
  tr ';' '\n' <<<"${GATE_SKIP:-}" | sed -n "s/^$1=//p" | head -1
}
failed=0
: > "$out/results.tsv"
for id in $ids; do
  r=${id%%:*}; e=${id#*:}; [ "$e" = "$id" ] && e=run
  why=$(skip_of "$r:$e")
  t0=$(date +%s)
  if [ -n "$why" ]; then
    res=SKIP note=$why
  elif [ ! -f "compositions/$r/composition.json" ]; then
    res=SKIP note="no composition"
  else
    "$host" "compositions/$r" --account "$account" --entry "$e" --on-fail freeze --freeze-ttl "${GATE_FREEZE_TTL:-900}" \
      > "$out/$r-$e.json" 2> "$out/$r-$e.log" < /dev/null
    rc=$?
    doc=$(cat "$out/$r-$e.json")
    if [ $rc = 0 ] && jq -e '.readings.skipped' <<<"$doc" > /dev/null 2>&1; then
      res=SKIP note=$(jq -r '.readings.skipped' <<<"$doc")
    elif [ $rc = 0 ]; then
      res=PASS note=$(jq -r '.dump // ""' <<<"$doc" 2>/dev/null)
    else
      res=FAIL note=$(jq -r '[.error, (.cleanupErrors // [] | join("; "))] | map(select(. != null and . != "")) | join(" | ") | .[0:200]' <<<"$doc" 2>/dev/null)
      [ -n "$note" ] || note="exit $rc: $(tail -1 "$out/$r-$e.log" | cut -c1-200)"
      failed=1
    fi
  fi
  s=$(( $(date +%s) - t0 ))
  echo "RESULT $r:$e $res $s $note"
  printf '%s\t%s\t%s\t%s\n' "$r:$e" "$res" "$s" "$note" >> "$out/results.tsv"
done
printf '\n| composition | result | seconds | note |\n| --- | --- | --- | --- |\n'
awk -F'\t' '{printf "| %s | %s | %s | %s |\n", $1, $2, $3, $4}' "$out/results.tsv"
echo "logs and readings: $out"
exit $failed
