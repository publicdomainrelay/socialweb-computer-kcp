#!/bin/bash
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

SSH_TARGET="${SSH_TARGET:-root@mini-cloud-0002.chadig.com}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_no_password}"
RELAY_HOSTNAME="${RELAY_HOSTNAME:-relay.mini-cloud-0002.chadig.com}"
LISTEN="${LISTEN:-127.0.0.1:2584}"
PREFLIGHT_LISTEN="${PREFLIGHT_LISTEN:-127.0.0.1:2585}"
BASE="${BASE:-/opt/atproto-relay-go}"
STATE_DIR="${STATE_DIR:-/var/lib/atproto-relay-go}"
RUN_USER="${RUN_USER:-johnandersen777}"
DENO="${DENO:-/home/$RUN_USER/.deno/bin/deno}"
OUT_ROOT="${OUT_ROOT:-$HOME/.tmp/atproto-relay-deploy}"
UNIT=atproto-relay.service
DROPIN_DIR=/etc/systemd/system/atproto-relay.service.d
DROPIN="$DROPIN_DIR/30-go.conf"
BIN_NAME=socialweb-computer
TS_DID="did:web:127.0.0.1"
GO_DID="did:web:$RELAY_HOSTNAME"
START_SEQ_MARGIN=1000
KEEP_VERSIONS=3
SSH_OPTS=(-o IdentitiesOnly=yes -i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=15)

usage() {
  cat <<EOF
usage: scripts/deploy-atproto-relay.sh [--binary PATH [--sha256 HEX]] [--dry-run]
       scripts/deploy-atproto-relay.sh --status | --rollback

  (no mode)   deploy the Go atproto relay (socialweb-computer internal atproto-relay) in place of the
              TypeScript relay of $UNIT on $SSH_TARGET:
              1. the binary: --binary PATH (a release binary: --sha256 HEX, or a SHA256SUMS beside it that
                 names it; a dev binary without either is hashed here), or, without --binary, a
                 linux/amd64 build (CGO off) of this checkout
              2. ship it to $BASE/<sha256 prefix>/$BIN_NAME (skipped when it is there) and check its sha256
              3. preflight: run it as $RUN_USER on $PREFLIGHT_LISTEN with a scratch state dir and wait for
                 /xrpc/_health; the TypeScript relay keeps serving
              4. while the TypeScript relay serves: save its listHosts to $STATE_DIR/seed-hosts.json (the
                 Go relay adds those hosts as idle known hosts) and read its newest firehose seq; the Go
                 relay continues its firehose at that seq plus $START_SEQ_MARGIN ($BASE/start-seq)
              5. write $DROPIN (Type=notify, the Go ExecStart on $LISTEN, state in $STATE_DIR, no
                 EnvironmentFile), restart $UNIT and check http://$LISTEN and https://$RELAY_HOSTNAME:
                 describeServer must name $GO_DID; a failed check rolls back
              caddy is not touched: it keeps proxying https://$RELAY_HOSTNAME to $LISTEN
  --dry-run   do steps 1 to 4 read-only (no build, no upload, no preflight, no file written) and print the
              plan: the binary, the remote state, the TypeScript relay's hosts and seq, the drop-in
  --rollback  remove $DROPIN and restart $UNIT: the TypeScript relay exactly as before ($STATE_DIR and
              $BASE stay for a later deploy)
  --status    print which relay serves $UNIT and its health; changes nothing

env: SSH_TARGET, SSH_KEY, RELAY_HOSTNAME, LISTEN, PREFLIGHT_LISTEN, BASE, STATE_DIR, RUN_USER, DENO,
     OUT_ROOT, START_SEQ (overrides step 4's seq), ALLOWED_HOSTS (--atproto-relay-allowed-hosts),
     NO_AUTO_ROLLBACK=1
EOF
}

say() {
  echo "deploy-atproto-relay: $*"
}

q() {
  printf "%q" "$1"
}

remote() {
  ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "$@"
}

unit_state() {
  remote "systemctl show $UNIT -p ActiveState -p SubState -p MainPID -p ExecMainStartTimestamp -p DropInPaths -p NRestarts --no-pager
pid=\$(systemctl show $UNIT -p MainPID --value)
if [ \"\$pid\" != 0 ]; then echo \"MainExe=\$(readlink /proc/\$pid/exe)\"; fi
echo \"Current=\$(readlink $(q "$BASE/current") || true)\"
if [ -f $(q "$DROPIN") ]; then echo \"DropIn=\$(grep '^ExecStart=.' $(q "$DROPIN") | cut -c1-400)\"; else echo DropIn=none; fi
if [ -f $(q "$STATE_DIR/relay-state.json") ]; then echo \"State=\$(stat -c '%s bytes %U:%G %a' $(q "$STATE_DIR/relay-state.json"))\"; else echo State=none; fi"
}

fetch() {
  local path="$1" body code
  body="$(curl -sS --max-time 15 -w '\n%{http_code}' "https://$RELAY_HOSTNAME$path" 2>/dev/null || true)"
  code="${body##*$'\n'}"
  body="${body%$'\n'*}"
  printf '%s %s\n' "$code" "$body"
}

loopback() {
  remote "curl -sS --max-time 10 -w ' %{http_code}' http://$LISTEN$1 || true"
}

hosts_summary() {
  local body
  body="$(curl -sS --max-time 20 "https://$RELAY_HOSTNAME/xrpc/com.atproto.sync.listHosts" 2>/dev/null || true)"
  printf '%s' "$body" | jq -c '{hosts: (.hosts | length), states: (.hosts | group_by(.state) | map({(.[0].state): length}) | add)}' 2>/dev/null || echo "unreadable"
}

serving_did() {
  fetch /xrpc/com.atproto.server.describeServer | sed -n 's/.*"did":"\([^"]*\)".*/\1/p'
}

report() {
  say "unit:"
  unit_state | sed 's/^/  /'
  say "https://$RELAY_HOSTNAME/xrpc/_health: $(fetch /xrpc/_health)"
  say "https://$RELAY_HOSTNAME describeServer did: $(serving_did)"
  say "https://$RELAY_HOSTNAME listHosts: $(hosts_summary)"
  say "last journal lines:"
  remote "journalctl -u $UNIT -n 8 --no-pager -o cat | cut -c1-240" | sed 's/^/  /'
}

restart() {
  remote "systemctl daemon-reload && systemctl restart $UNIT"
}

wait_listen() {
  remote "for i in \$(seq 1 60); do curl -sf --max-time 2 -o /dev/null http://$LISTEN/xrpc/_health && exit 0; sleep 0.5; done; exit 1"
}

rollback() {
  say "removing $DROPIN and restarting $UNIT on the TypeScript relay"
  remote "rm -f $(q "$DROPIN") && rmdir --ignore-fail-on-non-empty $(q "$DROPIN_DIR") 2>/dev/null || true"
  restart
  if ! wait_listen; then
    say "rollback: $UNIT does not answer on $LISTEN"
    report
    exit 1
  fi
  local did
  did="$(serving_did)"
  report
  if [ "$did" = "$TS_DID" ]; then
    say "rolled back: the TypeScript relay serves https://$RELAY_HOSTNAME"
    return 0
  fi
  say "rolled back, but https://$RELAY_HOSTNAME describeServer names '$did', not $TS_DID"
  exit 1
}

ts_serving() {
  [ "$(remote "test -f $(q "$DROPIN") && echo go || echo ts")" = ts ]
}

ts_head() {
  remote "env HOME=/nonexistent DENO_DIR=/proc/nonexistent-deno $(q "$DENO") run --quiet --no-config --allow-net=$LISTEN - <<'JS' 2>/dev/null || echo -1
let max = -1;
let last = Date.now();
const t0 = Date.now();
const ws = new WebSocket('ws://$LISTEN/xrpc/com.atproto.sync.subscribeRepos');
ws.onmessage = (e) => {
  last = Date.now();
  const m = /\"seq\":(\\d+)/.exec(typeof e.data === 'string' ? e.data : '');
  if (m && Number(m[1]) > max) max = Number(m[1]);
};
ws.onerror = () => { console.log(max); Deno.exit(0); };
setInterval(() => {
  if (Date.now() - last > 3000 || Date.now() - t0 > 90000) { console.log(max); Deno.exit(0); }
}, 250);
JS"
}

mode=deploy
dry_run=0
binary=""
want_sha=""
while [ $# -gt 0 ]; do
  case "$1" in
  --rollback) mode=rollback ;;
  --status) mode=status ;;
  --dry-run) dry_run=1 ;;
  --binary)
    binary="${2:?--binary wants a path}"
    shift
    ;;
  --sha256)
    want_sha="${2:?--sha256 wants a hex digest}"
    shift
    ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    usage >&2
    exit 2
    ;;
  esac
  shift
done

for tool in curl jq sha256sum; do
  command -v "$tool" >/dev/null || {
    say "$tool is required"
    exit 1
  }
done

case "$mode" in
status)
  report
  exit 0
  ;;
rollback)
  rollback
  exit 0
  ;;
esac

if [ -n "$binary" ]; then
  [ -f "$binary" ] || {
    say "no binary at $binary"
    exit 1
  }
  sha="$(sha256sum "$binary" | cut -d' ' -f1)"
  sums="$(dirname "$binary")/SHA256SUMS"
  if [ -z "$want_sha" ] && [ -f "$sums" ]; then
    want_sha="$(awk -v n="$(basename "$binary")" '$2 == n || $2 == "*" n {print $1}' "$sums" | head -1)"
  fi
  if [ -n "$want_sha" ]; then
    [ "$sha" = "$want_sha" ] || {
      say "$binary is $sha, not the expected $want_sha"
      exit 1
    }
    say "binary $binary sha256 $sha (verified)"
  else
    say "binary $binary sha256 $sha (a dev binary: no --sha256 and no SHA256SUMS entry)"
  fi
elif [ "$dry_run" = 1 ]; then
  sha=""
  say "would build $BIN_NAME of $(git -C "$REPO" rev-parse --short=12 HEAD) for linux/amd64 (CGO_ENABLED=0)"
else
  full_commit="$(git -C "$REPO" rev-parse HEAD)"
  version="${full_commit:0:12}"
  if [ -n "$(git -C "$REPO" status --porcelain --untracked-files=no)" ]; then
    version="$version-dirty-$(date -u +%Y%m%dT%H%M%SZ)"
  fi
  out="$OUT_ROOT/$version"
  mkdir -p "$out"
  say "building $BIN_NAME $version for linux/amd64 (CGO_ENABLED=0)"
  stamp=github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp
  (cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X $stamp.Version=$version -X $stamp.Commit=$full_commit" -o "$out/$BIN_NAME" ./cmd/socialweb-computer)
  binary="$out/$BIN_NAME"
  sha="$(sha256sum "$binary" | cut -d' ' -f1)"
  say "built $binary sha256 $sha ($(du -h "$binary" | cut -f1))"
fi
if [ -n "$sha" ] && [ "$(uname -s)/$(uname -m)" = "Linux/x86_64" ]; then
  "$binary" internal atproto-relay --help >/dev/null 2>&1 || {
    say "$binary does not run internal atproto-relay --help"
    exit 1
  }
fi

vdir="$BASE/${sha:0:16}"
bin="$vdir/$BIN_NAME"
say "remote state:"
unit_state | sed 's/^/  /'

serving=go
if ts_serving; then serving=ts; fi
start_seq="${START_SEQ:-}"
seed_note="kept (the Go relay already serves; its own state is the source)"
if [ "$serving" = ts ]; then
  hosts_json="$(remote "curl -sS --max-time 20 http://$LISTEN/xrpc/com.atproto.sync.listHosts" || true)"
  seed_note="$(printf '%s' "$hosts_json" | jq -c '{hosts: (.hosts | length), states: (.hosts | group_by(.state) | map({(.[0].state): length}) | add)}' 2>/dev/null || echo unreadable)"
  if [ -z "$start_seq" ]; then
    stored="$(remote "cat $(q "$BASE/start-seq") 2>/dev/null || true")"
    head="$(ts_head | tail -1)"
    case "$head" in '' | *[!0-9-]*) head=-1 ;; esac
    if [ "$head" -ge 0 ]; then
      start_seq=$((head + START_SEQ_MARGIN))
    else
      start_seq="${stored:-0}"
    fi
    say "the TypeScript relay's newest firehose seq: $head; the Go relay continues at $start_seq"
  fi
else
  start_seq="${start_seq:-$(remote "cat $(q "$BASE/start-seq") 2>/dev/null || echo 0")}"
fi
case "$start_seq" in '' | *[!0-9]*) start_seq=0 ;; esac

exec_line="$bin internal atproto-relay --state-dir $STATE_DIR --atproto-relay-listen $LISTEN --atproto-relay-public-url https://$RELAY_HOSTNAME --atproto-relay-start-seq $start_seq"
if [ -n "${ALLOWED_HOSTS:-}" ]; then
  exec_line="$exec_line --atproto-relay-allowed-hosts $ALLOWED_HOSTS"
fi
dropin_body="[Service]
Type=notify
NotifyAccess=main
EnvironmentFile=
WorkingDirectory=$STATE_DIR
ExecStart=
ExecStart=$exec_line
TimeoutStartSec=30
TimeoutStopSec=20"

if [ "$dry_run" = 1 ]; then
  have="$(remote "if [ -n $(q "$sha") ] && [ -f $(q "$bin") ]; then sha256sum $(q "$bin") | cut -d' ' -f1; fi")"
  port_busy="$(remote "ss -ltnH 'sport = :${PREFLIGHT_LISTEN##*:}' | wc -l")"
  say "plan (dry run, nothing changed):"
  say "  serving now: $serving"
  if [ -n "$sha" ]; then
    if [ "$have" = "$sha" ]; then say "  upload: skip, $bin is there"; else say "  upload: $binary -> $SSH_TARGET:$bin (sha256 $sha)"; fi
  else
    say "  upload: the binary built from this checkout -> $SSH_TARGET:$BASE/<sha256 prefix>/$BIN_NAME"
  fi
  say "  preflight: as $RUN_USER on $PREFLIGHT_LISTEN ($([ "$port_busy" = 0 ] && echo free || echo "BUSY, the deploy would stop here")), a scratch state dir, GET /xrpc/_health"
  say "  seed: TypeScript listHosts -> $STATE_DIR/seed-hosts.json: $seed_note"
  say "  firehose start seq: $start_seq"
  say "  $DROPIN would be:"
  printf '%s\n' "$dropin_body" | sed 's/^/    /'
  say "  then: systemctl daemon-reload; systemctl restart $UNIT; check http://$LISTEN/xrpc/_health and https://$RELAY_HOSTNAME describeServer did $GO_DID"
  say "  rollback: scripts/deploy-atproto-relay.sh --rollback (rm $DROPIN; restart $UNIT; describeServer did $TS_DID)"
  exit 0
fi

have="$(remote "if [ -f $(q "$bin") ]; then sha256sum $(q "$bin") | cut -d' ' -f1; fi")"
if [ "$have" = "$sha" ]; then
  say "$SSH_TARGET:$bin is there with sha256 $sha"
else
  say "shipping to $SSH_TARGET:$bin"
  remote "install -d -m 0755 $(q "$vdir") && cat > $(q "$bin.partial") && chmod 0755 $(q "$bin.partial") && mv $(q "$bin.partial") $(q "$bin")" <"$binary"
  have="$(remote "sha256sum $(q "$bin") | cut -d' ' -f1")"
  [ "$have" = "$sha" ] || {
    say "the shipped binary is $have, not $sha"
    exit 1
  }
fi

say "preflight: $bin as $RUN_USER on $PREFLIGHT_LISTEN"
preflight="$(remote "set -u
if [ \"\$(ss -ltnH 'sport = :${PREFLIGHT_LISTEN##*:}' | wc -l)\" != 0 ]; then echo 'busy'; exit 0; fi
dir=\$(mktemp -d /var/tmp/atproto-relay-preflight.XXXXXX)
chown $(q "$RUN_USER") \"\$dir\"
timeout 30 runuser -u $(q "$RUN_USER") -- $(q "$bin") internal atproto-relay --state-dir \"\$dir/state\" --atproto-relay-listen $PREFLIGHT_LISTEN --atproto-relay-public-url https://$RELAY_HOSTNAME --atproto-relay-recrawl-window 0 >\"\$dir/log\" 2>&1 &
pid=\$!
ok=no
for i in \$(seq 1 60); do
  if body=\$(curl -sf --max-time 2 http://$PREFLIGHT_LISTEN/xrpc/_health); then ok=\"\$body\"; break; fi
  sleep 0.5
done
did=\$(curl -sf --max-time 2 http://$PREFLIGHT_LISTEN/xrpc/com.atproto.server.describeServer || true)
kill -TERM \$pid 2>/dev/null || true
wait \$pid 2>/dev/null || true
if [ \"\$ok\" = no ]; then tail -n 5 \"\$dir/log\" | cut -c1-240; fi
rm -rf \"\$dir\"
echo \"health \$ok\"
echo \"describe \$did\"")"
printf '%s\n' "$preflight" | sed 's/^/  /'
case "$preflight" in
busy*)
  say "preflight: $PREFLIGHT_LISTEN is in use; set PREFLIGHT_LISTEN to a free loopback port"
  exit 1
  ;;
*"health no"*)
  say "preflight failed; the TypeScript relay still serves"
  exit 1
  ;;
*"$GO_DID"*) ;;
*)
  say "preflight: describeServer does not name $GO_DID; the TypeScript relay still serves"
  exit 1
  ;;
esac

say "state dir $STATE_DIR (owner $RUN_USER, 0700)"
remote "install -d -m 0700 -o $(q "$RUN_USER") -g $(q "$RUN_USER") $(q "$STATE_DIR") && printf '%s\n' $(q "$start_seq") > $(q "$BASE/start-seq")"
if [ "$serving" = ts ]; then
  say "saving the TypeScript relay's listHosts to $STATE_DIR/seed-hosts.json: $seed_note"
  remote "set -e
tmp=\$(mktemp $(q "$STATE_DIR/seed-hosts.json.XXXXXX"))
curl -sSf --max-time 20 http://$LISTEN/xrpc/com.atproto.sync.listHosts > \"\$tmp\"
jq -e '.hosts | type == \"array\"' \"\$tmp\" >/dev/null
chown $(q "$RUN_USER"): \"\$tmp\"
chmod 0600 \"\$tmp\"
mv \"\$tmp\" $(q "$STATE_DIR/seed-hosts.json")"
fi

current_dropin="$(remote "cat $(q "$DROPIN") 2>/dev/null || true")"
active_exe="$(remote "pid=\$(systemctl show $UNIT -p MainPID --value); [ \"\$pid\" != 0 ] && readlink /proc/\$pid/exe || true")"
remote "ln -sfn $(q "${sha:0:16}") $(q "$BASE/current.new") && mv -T $(q "$BASE/current.new") $(q "$BASE/current")"
if [ "$current_dropin" = "$dropin_body" ] && [ "$active_exe" = "$bin" ]; then
  say "$UNIT already runs $bin with this drop-in; no restart"
else
  say "writing $DROPIN and restarting $UNIT"
  remote "set -e
install -d -m 0755 $(q "$DROPIN_DIR")
cat > $(q "$DROPIN.partial") <<'UNIT'
$dropin_body
UNIT
mv $(q "$DROPIN.partial") $(q "$DROPIN")"
  if ! restart || ! wait_listen; then
    say "$UNIT did not come up on the Go relay"
    report
    if [ "${NO_AUTO_ROLLBACK:-}" = 1 ]; then
      say "NO_AUTO_ROLLBACK=1: left as is; scripts/deploy-atproto-relay.sh --rollback restores the TypeScript relay"
      exit 1
    fi
    rollback
    exit 1
  fi
fi

health="$(fetch /xrpc/_health)"
did="$(serving_did)"
local_health="$(loopback /xrpc/_health)"
exe="$(remote "readlink /proc/\$(systemctl show $UNIT -p MainPID --value)/exe || true")"
report
say "via http://$LISTEN: $local_health"
if [ "$exe" = "$bin" ] && [ "$did" = "$GO_DID" ]; then
  case "$health" in
  200*)
    remote "cd $(q "$BASE") && ls -1t | grep -E '^[0-9a-f]{16}\$' | grep -vx $(q "${sha:0:16}") | tail -n +$KEEP_VERSIONS | xargs -r rm -rf --" || true
    say "deployed: $bin ($sha) serves https://$RELAY_HOSTNAME; rollback with scripts/deploy-atproto-relay.sh --rollback"
    exit 0
    ;;
  esac
fi
say "the health check failed: exe $exe, _health $health, describeServer did '$did'"
if [ "${NO_AUTO_ROLLBACK:-}" = 1 ]; then
  say "NO_AUTO_ROLLBACK=1: left as is; scripts/deploy-atproto-relay.sh --rollback restores the TypeScript relay"
  exit 1
fi
rollback
exit 1
