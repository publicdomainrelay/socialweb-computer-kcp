#!/usr/bin/env bash
# On the staging VM, as stg: the suite tarball's compositions against the market network instance stg.
# Usage: suite.sh <run> <runbook>[:<entry>]... ; one "RESULT <runbook>[:<entry>] <exit>" line each; exit 1 when one failed.
set -uo pipefail
run=$1; shift
D=${SWC_DIR:-/home/stg/stg}
export SWC_READINGS=$D/readings
export SWC_REPO=$D/suite
cd $D/suite/tests
compositions/staging/target.sh "$run" > $D/target-staging.json || exit 3
# a module's public reads resolve DIDs at the network's PLC (loopback, http)
export ATLIBS_PLC_URL=$(jq -r .plc $D/target-staging.json) ATLIBS_ALLOW_PRIVATE=1
failed=0
for id in "$@"; do
  r=${id%%:*} e=${id#*:}; [ "$e" = "$id" ] && e=run
  SWC_TARGET=$D/target-staging.json ${SWC_TEST_HOST:-$D/swc-test-host} "compositions/$r" --account alice --entry "$e" --on-fail cleanup > "$D/suite-$r-$e.log" 2>&1
  rc=$?
  echo "RESULT $id $rc"
  [ $rc = 0 ] || failed=1
done
exit $failed
