#!/usr/bin/env bash
# On the staging VM, as stg: the suite tarball's compositions against the market network instance stg.
# Usage: suite.sh <run> <runbook>... ; one "RESULT <runbook> <exit>" line each; exit 1 when one failed.
set -uo pipefail
run=$1; shift
D=${SWC_DIR:-/home/stg/stg}
export SWC_READINGS=$D/readings
cd $D/suite
compositions/staging/target.sh "$run" > $D/target-staging.json || exit 3
failed=0
for r in "$@"; do
  SWC_TARGET=$D/target-staging.json ${SWC_TEST_HOST:-$D/swc-test-host} "compositions/$r" --account alice --on-fail cleanup > "$D/suite-$r.log" 2>&1
  rc=$?
  echo "RESULT $r $rc"
  [ $rc = 0 ] || failed=1
done
exit $failed
