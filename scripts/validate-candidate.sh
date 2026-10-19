#!/bin/bash
# Pre-release candidate validation. Runs the lanes in parallel on their own
# droplets and prints one verdict. Tag only when it says PASS.
#
# usage: scripts/validate-candidate.sh <sha> [lanes]      lanes default "a,b,c"
#   a  the GitHub gate, apigen diff and cross-OS builds concluded success for <sha>
#   b  the full integration suite on gl-runner-1, detached, per test results
#   c  upgrade lane: install the base release, upgrade to the candidate, assert
#      the gateway keeps its DID, its fronts keep their ports, and a DenoPod
#      whose TypeScript tree changed is replaced
#
# Every lane writes ~/.tmp/validate/<sha>/<lane>.log and expires its droplet.
set -uo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
sha="${1:?usage: validate-candidate.sh <sha> [lanes]}"
lanes="${2:-a,b,c}"
short="${sha:0:8}"
out="$HOME/.tmp/validate/$short"
runners="142.93.26.14"
key="$HOME/.ssh/id_ed25519"
bucket="https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com"
mkdir -p "$out"
declare -A verdict
declare -A pids

log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$out/validate.log"; }

lane_a() {
  local ok=1
  for i in $(seq 1 60); do
    read -r run status concl < <(cd "$root" && gh run list --limit 40 --json headSha,databaseId,status,conclusion \
      --jq ".[]|select(.headSha|startswith(\"$sha\"))|select(.status==\"completed\")|\"\(.databaseId) \(.status) \(.conclusion)\"" | head -1)
    [ -n "${run:-}" ] && break
    sleep 30
  done
  [ -z "${run:-}" ] && { echo "no completed run for $sha"; return 1; }
  echo "run $run: $concl"
  [ "$concl" = success ] || ok=0
  (cd "$root" && gh run view "$run" --json jobs --jq '.jobs[]|"\(.name) \(.conclusion)"')
  return $((1 - ok))
}

lane_b() {
  local pattern="${LANE_B_PATTERN:-.}"
  local img="${LANE_B_IMAGE:?LANE_B_IMAGE names a commit with published main/<commit> artifacts}"
  ssh -i "$key" -o BatchMode=yes -o ServerAliveInterval=15 "root@$runners" \
    "setsid nohup bash /var/tmp/m4-run.sh origin/main $pattern $img vc-$short 1 > /var/tmp/vc-$short-b.log 2>&1 < /dev/null & echo started"
  for i in $(seq 1 120); do
    sleep 30
    if ssh -i "$key" -o BatchMode=yes "root@$runners" "grep -qE '^(ok|FAIL)[[:space:]]' /var/tmp/vc-$short-b.log"; then
      ssh -i "$key" -o BatchMode=yes "root@$runners" "grep -E '^--- (PASS|FAIL)|^(ok|FAIL)[[:space:]]|panic:' /var/tmp/vc-$short-b.log" | tee -a "$out/b.log"
      if ssh -i "$key" -o BatchMode=yes "root@$runners" "grep -qE '^FAIL[[:space:]]' /var/tmp/vc-$short-b.log"; then return 1; fi
      return 0
    fi
  done
  echo "lane b timed out"
  return 1
}

lane_c() {
  local name="vc-c-$short" ip base=v0.0.0-rc15 acct="vc-$short"
  scripts/dev-droplet.sh create "$name" > "$out/c-create.log" 2>&1 || { echo "create failed"; return 1; }
  ip="$(cat "$HOME/.tmp/dev-droplet/$name.ip")"
  echo "droplet $name $ip"
  scripts/dev-droplet.sh setup "$name" > "$out/c-setup.log" 2>&1 || { echo "setup failed"; return 1; }
  {
    echo "== build the candidate as dev publishes"
    ssh -i "$key" -o BatchMode=yes "root@$ip" "set -e
      id dev >/dev/null 2>&1 || useradd -m -s /bin/bash dev
      runuser -l dev -c 'cd ~ && ([ -d w ] || git clone -q https://github.com/publicdomainrelay/socialweb-computer-kcp w) && cd w && git fetch -q origin && git checkout -q -f $sha'
      runuser -l dev -c 'cd ~/w && scripts/build-socialweb-computer.sh --dev --publish /var/tmp/vc' | tail -3"
    echo "== install the base release $base as a bidder"
    ssh -i "$key" -o BatchMode=yes "root@$ip" "set -e
      mkdir -p /var/tmp/base && cd /var/tmp/base
      curl -fsSLO $bucket/releases/$base/binaries/socialweb-computer-linux-amd64
      curl -fsSLO $bucket/releases/$base/binaries/SHA256SUMS
      grep ' socialweb-computer-linux-amd64\$' SHA256SUMS | sha256sum -c -
      install -m 0755 socialweb-computer-linux-amd64 /usr/local/bin/socialweb-base
      /usr/local/bin/socialweb-base install bidder --account=$acct --handle=vc-$short.test --allow-dev-binary 2>&1 | tail -6
      systemctl --user -M $acct@ list-units 'socialweb-computer-bidder@*' --no-legend | awk '{print \$1,\$3,\$4}'"
    echo "== record the base slot and wait for its kcp"
    ssh -i "$key" -o BatchMode=yes "root@$ip" "S=/var/lib/socialweb-computer/$acct
      for i in \$(seq 1 36); do [ -f \$S/kcp/kcp-slots.json ] && break; sleep 5; done
      jq -r '.active' \$S/kcp/kcp-slots.json 2>/dev/null"
    echo "== upgrade to the candidate"
    ssh -i "$key" -o BatchMode=yes "root@$ip" "CB=\$(sha256sum /var/tmp/vc/bin/socialweb-computer | cut -d' ' -f1)
      install -m 0755 /var/tmp/vc/bin/socialweb-computer /usr/local/bin/socialweb-cand
      U=\$(id -u $acct)
      runuser -u $acct -- env XDG_RUNTIME_DIR=/run/user/\$U CONFIG_PATH_SOCIALWEB_COMPUTER=/var/lib/socialweb-computer/$acct/config.json /usr/local/bin/socialweb-cand upgrade --binary /usr/local/bin/socialweb-cand --sha256 \$CB --allow-dev-binary 2>&1 | tail -8"
    sleep 30
    echo "== after: unit, slot, replacement line"
    ssh -i "$key" -o BatchMode=yes "root@$ip" "S=/var/lib/socialweb-computer/$acct
      systemctl --user -M $acct@ is-active socialweb-computer-bidder@$short.test.service 2>/dev/null || systemctl --user -M $acct@ list-units 'socialweb-computer-bidder@*' --no-legend | awk '{print \$1,\$3,\$4}'
      jq -r '.active' \$S/kcp/kcp-slots.json 2>/dev/null
      journalctl --user -M $acct@ --since '-15 min' --no-pager 2>/dev/null | grep -c 'runs other assets'"
  } >> "$out/c.log" 2>&1
  local rc=1
  if grep -qE 'socialweb-computer-bidder@.*(active|running)' "$out/c.log" && grep -qE '^[1-9][0-9]*$' <(tail -1 <(sed -n '/== after: unit, slot, replacement line/,$p' "$out/c.log")); then rc=0; fi
  ssh -i "$key" -o BatchMode=yes "root@$ip" "/usr/local/bin/socialweb-cand uninstall bidder --account $acct --purge" >/dev/null 2>&1
  doctl compute droplet delete "$name" --force >/dev/null 2>&1
  return $rc
}

for lane in ${lanes//,/ }; do
  case "$lane" in
    a) ( lane_a > "$out/a.log" 2>&1; echo $? > "$out/a.rc" ) & ;;
    b) ( lane_b > "$out/b.lane.log" 2>&1; echo $? > "$out/b.rc" ) & ;;
    c) ( lane_c > "$out/c.lane.log" 2>&1; echo $? > "$out/c.rc" ) & ;;
    *) echo "unknown lane $lane"; exit 2 ;;
  esac
  pids[$lane]=$!
done
for lane in "${!pids[@]}"; do wait "${pids[$lane]}"; done

status=0
for lane in ${lanes//,/ }; do
  rc="$(cat "$out/$lane.rc" 2>/dev/null || echo 1)"
  if [ "$rc" = 0 ]; then verdict[$lane]=PASS; else verdict[$lane]=FAIL; status=1; fi
done
{
  echo "candidate $sha"
  echo "artifacts $out"
  for lane in "${!verdict[@]}"; do echo "lane $lane ${verdict[$lane]}"; done
  if [ $status = 0 ]; then echo "VERDICT PASS: safe to tag"; else echo "VERDICT FAIL: do not tag"; fi
} | tee "$out/verdict.txt"
exit $status
