#!/bin/bash
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

SSH_TARGET="${SSH_TARGET:-root@fedproxy.com}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_no_password}"
RELAY_HOSTNAME="${RELAY_HOSTNAME:-xrpc.fedproxy.com}"
SOCKET="${SOCKET:-/opt/caddy/xrpc-relay-server.sock}"
BASE="${BASE:-/opt/xrpc-relay-go}"
OUT_ROOT="${OUT_ROOT:-$HOME/.tmp/xrpc-relay-deploy}"
UNIT=xrpc-relay-server.service
DROPIN_DIR=/etc/systemd/system/xrpc-relay-server.service.d
DROPIN="$DROPIN_DIR/30-go.conf"
BIN_NAME=socialweb-computer
SSH_OPTS=(-o IdentitiesOnly=yes -i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=15)

usage() {
  cat <<EOF
usage: scripts/deploy-xrpc-relay.sh [--rollback | --status]

  (no flag)   build socialweb-computer for linux/amd64 (CGO off) from this checkout, ship it to
              $SSH_TARGET:$BASE/<version>/, point $BASE/current at it, write $DROPIN
              (the Go relay: internal ingress-relay --unix-socket $SOCKET --hostname $RELAY_HOSTNAME),
              restart $UNIT and check it through https://$RELAY_HOSTNAME; a failed check rolls back
  --rollback  remove $DROPIN and restart $UNIT (back to the deno relay of the lower drop-ins)
  --status    print which relay serves $UNIT and the health of https://$RELAY_HOSTNAME; changes nothing

env: SSH_TARGET (root@fedproxy.com), SSH_KEY (~/.ssh/id_ed25519_no_password), RELAY_HOSTNAME,
     SOCKET, BASE (/opt/xrpc-relay-go), OUT_ROOT (~/.tmp/xrpc-relay-deploy), NO_AUTO_ROLLBACK=1
EOF
}

say() {
  echo "deploy-xrpc-relay: $*"
}

q() {
  printf "%q" "$1"
}

remote() {
  ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "$@"
}

unit_state() {
  remote "systemctl show $UNIT -p ActiveState -p SubState -p MainPID -p ExecMainStartTimestamp -p DropInPaths --no-pager; pid=\$(systemctl show $UNIT -p MainPID --value); if [ \"\$pid\" != 0 ]; then echo \"MainExe=\$(readlink /proc/\$pid/exe)\"; fi; echo \"Current=\$(readlink $(q "$BASE/current") || true)\"; if [ -S $(q "$SOCKET") ]; then echo \"Socket=\$(stat -c '%A %U:%G' $(q "$SOCKET"))\"; else echo Socket=missing; fi"
}

wait_socket() {
  remote "for i in \$(seq 1 60); do [ -S $(q "$SOCKET") ] && exit 0; sleep 0.5; done; exit 1"
}

fetch() {
  local path="$1" body code
  body="$(curl -sS --max-time 15 -w '\n%{http_code}' "https://$RELAY_HOSTNAME$path" 2>/dev/null || true)"
  code="${body##*$'\n'}"
  body="${body%$'\n'*}"
  printf '%s %s\n' "$code" "$body"
}

socket_health() {
  remote "curl -sS --max-time 10 --unix-socket $(q "$SOCKET") -H $(q "Host: $RELAY_HOSTNAME") -w ' %{http_code}' http://relay/xrpc/_health || true"
}

restart() {
  remote "systemctl daemon-reload && systemctl restart $UNIT"
}

report() {
  say "unit:"
  unit_state | sed 's/^/  /'
  say "https://$RELAY_HOSTNAME/xrpc/_health: $(fetch /xrpc/_health)"
  say "https://$RELAY_HOSTNAME/.well-known/did.json: $(fetch /.well-known/did.json)"
  say "last journal lines:"
  remote "journalctl -u $UNIT -n 8 --no-pager -o cat | cut -c1-240" | sed 's/^/  /'
}

rollback() {
  say "removing $DROPIN and restarting $UNIT on the deno relay"
  remote "rm -f $(q "$DROPIN")"
  restart
  if ! wait_socket; then
    say "rollback: $SOCKET did not come back"
    report
    exit 1
  fi
  local did
  did="$(fetch /.well-known/did.json)"
  report
  case "$did" in
  200*) say "rolled back: the deno relay serves https://$RELAY_HOSTNAME" ;;
  *)
    say "rolled back, but https://$RELAY_HOSTNAME/.well-known/did.json answers $did"
    exit 1
    ;;
  esac
}

mode=deploy
case "${1:-}" in
"") ;;
--rollback) mode=rollback ;;
--status) mode=status ;;
-h | --help)
  usage
  exit 0
  ;;
*)
  usage >&2
  exit 2
  ;;
esac

command -v curl >/dev/null || {
  say "curl is required for the health check"
  exit 1
}

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

full_commit="$(git -C "$REPO" rev-parse HEAD)"
commit="${full_commit:0:12}"
version="$commit"
if [ -n "$(git -C "$REPO" status --porcelain --untracked-files=no)" ]; then
  version="$commit-dirty-$(date -u +%Y%m%dT%H%M%SZ)"
fi
out="$OUT_ROOT/$version"
mkdir -p "$out"
say "building $BIN_NAME $version for linux/amd64 (CGO_ENABLED=0)"
stamp=github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp
(cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -ldflags "-s -w -X $stamp.Version=$version -X $stamp.Commit=$full_commit" -o "$out/$BIN_NAME" ./cmd/socialweb-computer)
sha="$(sha256sum "$out/$BIN_NAME" | cut -d' ' -f1)"
say "built $out/$BIN_NAME sha256 $sha ($(du -h "$out/$BIN_NAME" | cut -f1))"
if [ "$(uname -s)/$(uname -m)" = "Linux/x86_64" ]; then
  "$out/$BIN_NAME" internal ingress-relay --help >/dev/null 2>&1 || {
    say "the built binary does not run internal ingress-relay --help"
    exit 1
  }
fi

dest="$BASE/$version"
say "shipping to $SSH_TARGET:$dest"
remote "install -d -m 0755 $(q "$dest") && cat > $(q "$dest/$BIN_NAME.partial") && chmod 0755 $(q "$dest/$BIN_NAME.partial") && mv $(q "$dest/$BIN_NAME.partial") $(q "$dest/$BIN_NAME")" <"$out/$BIN_NAME"
remote_sha="$(remote "sha256sum $(q "$dest/$BIN_NAME") | cut -d' ' -f1")"
[ "$remote_sha" = "$sha" ] || {
  say "the shipped binary is $remote_sha, not $sha"
  exit 1
}
remote "$(q "$dest/$BIN_NAME") internal ingress-relay --help >/dev/null 2>&1" || {
  say "the shipped binary does not run on $SSH_TARGET"
  exit 1
}

say "pointing $BASE/current at $version and writing $DROPIN"
remote "set -e
prev=\$(readlink $(q "$BASE/current") || true)
if [ -n \"\$prev\" ] && [ \"\$prev\" != $(q "$version") ]; then printf '%s\n' \"\$prev\" > $(q "$BASE/previous"); fi
ln -sfn $(q "$version") $(q "$BASE/current.new")
mv -T $(q "$BASE/current.new") $(q "$BASE/current")
install -d -m 0755 $(q "$DROPIN_DIR")
cat > $(q "$DROPIN.partial") <<'UNIT'
[Service]
Type=notify
NotifyAccess=main
WorkingDirectory=$BASE
ExecStart=
ExecStart=$BASE/current/$BIN_NAME internal ingress-relay --unix-socket $SOCKET --hostname $RELAY_HOSTNAME
TimeoutStopSec=20
UNIT
mv $(q "$DROPIN.partial") $(q "$DROPIN")"

say "restarting $UNIT"
if ! restart || ! wait_socket; then
  say "$UNIT did not come up on the Go relay"
  report
  if [ "${NO_AUTO_ROLLBACK:-}" = 1 ]; then
    say "NO_AUTO_ROLLBACK=1: left as is; scripts/deploy-xrpc-relay.sh --rollback restores the deno relay"
    exit 1
  fi
  rollback
  exit 1
fi

health="$(fetch /xrpc/_health)"
did="$(fetch /.well-known/did.json)"
via_socket="$(socket_health)"
report
say "via $SOCKET: $via_socket"
case "$health" in
200*"$commit"*)
  case "$did" in
  200*)
    say "deployed: $version ($sha) serves https://$RELAY_HOSTNAME; rollback with scripts/deploy-xrpc-relay.sh --rollback"
    exit 0
    ;;
  esac
  ;;
esac
say "the health check failed: _health $health, did.json $did"
if [ "${NO_AUTO_ROLLBACK:-}" = 1 ]; then
  say "NO_AUTO_ROLLBACK=1: left as is; scripts/deploy-xrpc-relay.sh --rollback restores the deno relay"
  exit 1
fi
rollback
exit 1
