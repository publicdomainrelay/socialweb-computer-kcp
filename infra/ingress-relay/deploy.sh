#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

ingress_relay_defaults() {
  INGRESS_RELAY_HOSTNAME="${INGRESS_RELAY_HOSTNAME:-xrpc.fedproxy.com}"
  INGRESS_RELAY_SOCKET="${INGRESS_RELAY_SOCKET:-/opt/caddy/xrpc-relay-server.sock}"
  INGRESS_RELAY_BASE="${INGRESS_RELAY_BASE:-/opt/xrpc-relay-go}"
  INGRESS_RELAY_OUT_ROOT="${INGRESS_RELAY_OUT_ROOT:-$HOME/.tmp/xrpc-relay-deploy}"
  INGRESS_RELAY_NO_AUTO_ROLLBACK="${INGRESS_RELAY_NO_AUTO_ROLLBACK:-}"
  INGRESS_RELAY_UNIT=xrpc-relay-server.service
  INGRESS_RELAY_DROPIN_DIR="/etc/systemd/system/$INGRESS_RELAY_UNIT.d"
  INGRESS_RELAY_DROPIN="$INGRESS_RELAY_DROPIN_DIR/30-go.conf"
  INGRESS_RELAY_BIN=socialweb-computer
}

ingress_relay_unit_state() {
  local base sock
  base="$(infra_q "$INGRESS_RELAY_BASE")"
  sock="$(infra_q "$INGRESS_RELAY_SOCKET")"
  infra_ro_root "systemctl show $INGRESS_RELAY_UNIT -p ActiveState -p SubState -p MainPID -p ExecMainStartTimestamp -p DropInPaths --no-pager
pid=\$(systemctl show $INGRESS_RELAY_UNIT -p MainPID --value)
if [ \"\$pid\" != 0 ]; then echo \"MainExe=\$(readlink /proc/\$pid/exe)\"; fi
echo \"Current=\$(readlink $base/current || true)\"
if [ -f $base/previous ]; then echo \"Previous=\$(cat $base/previous)\"; fi
if [ -S $sock ]; then echo \"Socket=\$(stat -c '%A %U:%G' $sock)\"; else echo Socket=missing; fi"
}

ingress_relay_wait_socket() {
  infra_ro_root "for i in \$(seq 1 60); do [ -S $(infra_q "$INGRESS_RELAY_SOCKET") ] && exit 0; sleep 0.5; done; exit 1"
}

ingress_relay_socket_health() {
  infra_ro_root "curl -sS --max-time 10 --unix-socket $(infra_q "$INGRESS_RELAY_SOCKET") -H $(infra_q "Host: $INGRESS_RELAY_HOSTNAME") -w ' %{http_code}' http://relay/xrpc/_health || true"
}

ingress_relay_report() {
  infra_log "unit:"
  ingress_relay_unit_state | infra_indent
  infra_report_urls "https://$INGRESS_RELAY_HOSTNAME/xrpc/_health" "https://$INGRESS_RELAY_HOSTNAME/.well-known/did.json"
  infra_log "last journal lines:"
  infra_ro_root "journalctl -u $INGRESS_RELAY_UNIT -n 8 --no-pager -o cat | cut -c1-240" | infra_indent
}

ingress_relay_status() {
  ingress_relay_defaults
  ingress_relay_report
}

ingress_relay_rollback() {
  local did
  ingress_relay_defaults
  infra_log "removing $INGRESS_RELAY_DROPIN and restarting $INGRESS_RELAY_UNIT on the deno relay of the lower drop-ins"
  infra_rw_root "rm -f $(infra_q "$INGRESS_RELAY_DROPIN")"
  infra_unit_restart "$INGRESS_RELAY_UNIT"
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "dry-run: rollback printed; --rollback --apply runs it"
    return 0
  fi
  if ! ingress_relay_wait_socket; then
    infra_log "rollback: $INGRESS_RELAY_SOCKET did not come back"
    ingress_relay_report
    return 1
  fi
  did="$(infra_fetch "https://$INGRESS_RELAY_HOSTNAME/.well-known/did.json")"
  ingress_relay_report
  case "$did" in
  200*) infra_log "rolled back: the deno relay serves https://$INGRESS_RELAY_HOSTNAME" ;;
  *)
    infra_log "rolled back, but https://$INGRESS_RELAY_HOSTNAME/.well-known/did.json answers $did"
    return 1
    ;;
  esac
}

ingress_relay_fail() {
  infra_log "$1"
  ingress_relay_report
  if [ "$INGRESS_RELAY_NO_AUTO_ROLLBACK" = 1 ]; then
    infra_log "INGRESS_RELAY_NO_AUTO_ROLLBACK=1: left as is; infra/ingress-relay/deploy.sh --rollback --apply restores the deno relay"
    return 1
  fi
  ingress_relay_rollback || true
  return 1
}

ingress_relay_deploy() {
  local full_commit commit version out dest sha health did via_socket stamp
  ingress_relay_defaults
  full_commit="$(git -C "$INFRA_REPO" rev-parse HEAD)"
  commit="${full_commit:0:12}"
  version="$commit"
  if [ -n "$(git -C "$INFRA_REPO" status --porcelain --untracked-files=no)" ]; then
    version="$commit-dirty-$INFRA_TS"
  fi
  out="$INGRESS_RELAY_OUT_ROOT/$version"
  stamp=github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp
  infra_local_rw "build $INGRESS_RELAY_BIN $version for linux/amd64 (CGO_ENABLED=0)" \
    env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$INFRA_REPO" build -trimpath -buildvcs=false \
    -ldflags "-s -w -X $stamp.Version=$version -X $stamp.Commit=$full_commit" -o "$out/$INGRESS_RELAY_BIN" ./cmd/socialweb-computer
  sha="<sha256 of $out/$INGRESS_RELAY_BIN>"
  if [ "$INFRA_APPLY" = 1 ]; then
    sha="$(infra_sha256 "$out/$INGRESS_RELAY_BIN")"
    infra_log "built $out/$INGRESS_RELAY_BIN sha256 $sha ($(du -h "$out/$INGRESS_RELAY_BIN" | cut -f1))"
    if [ "$(uname -s)/$(uname -m)" = "Linux/x86_64" ]; then
      "$out/$INGRESS_RELAY_BIN" internal ingress-relay --help >/dev/null 2>&1 || infra_die "the built binary does not run internal ingress-relay --help"
    fi
  fi

  dest="$INGRESS_RELAY_BASE/$version"
  infra_upload "$out/$INGRESS_RELAY_BIN" "$dest/$INGRESS_RELAY_BIN" 0755 root
  infra_ro_root "$(infra_q "$dest/$INGRESS_RELAY_BIN") internal ingress-relay --help >/dev/null 2>&1" || infra_die "the shipped binary does not run on $(infra_target)"

  infra_log "pointing $INGRESS_RELAY_BASE/current at $version"
  infra_rw_root "set -e
prev=\$(readlink $(infra_q "$INGRESS_RELAY_BASE/current") || true)
if [ -n \"\$prev\" ] && [ \"\$prev\" != $(infra_q "$version") ]; then printf '%s\n' \"\$prev\" > $(infra_q "$INGRESS_RELAY_BASE/previous"); fi
ln -sfn $(infra_q "$version") $(infra_q "$INGRESS_RELAY_BASE/current.new")
mv -T $(infra_q "$INGRESS_RELAY_BASE/current.new") $(infra_q "$INGRESS_RELAY_BASE/current")"
  infra_file_install "$INGRESS_RELAY_DROPIN" 0644 "[Service]
Type=notify
NotifyAccess=main
WorkingDirectory=$INGRESS_RELAY_BASE
ExecStart=
ExecStart=$INGRESS_RELAY_BASE/current/$INGRESS_RELAY_BIN internal ingress-relay --unix-socket $INGRESS_RELAY_SOCKET --hostname $INGRESS_RELAY_HOSTNAME
TimeoutStopSec=20"

  infra_log "restarting $INGRESS_RELAY_UNIT"
  if ! infra_unit_restart "$INGRESS_RELAY_UNIT" || ! ingress_relay_wait_socket; then
    ingress_relay_fail "$INGRESS_RELAY_UNIT did not come up on the Go relay"
    return 1
  fi
  health="$(infra_fetch "https://$INGRESS_RELAY_HOSTNAME/xrpc/_health")"
  did="$(infra_fetch "https://$INGRESS_RELAY_HOSTNAME/.well-known/did.json")"
  via_socket="$(ingress_relay_socket_health)"
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "dry-run: deploy printed; the check wants _health 200 with $commit and did.json 200, else it rolls back"
    return 0
  fi
  ingress_relay_report
  infra_log "via $INGRESS_RELAY_SOCKET: $via_socket"
  case "$health" in
  200*"$commit"*)
    case "$did" in
    200*)
      infra_log "deployed: $version ($sha) serves https://$INGRESS_RELAY_HOSTNAME; rollback with infra/ingress-relay/deploy.sh --rollback --apply"
      return 0
      ;;
    esac
    ;;
  esac
  ingress_relay_fail "the health check failed: _health $health, did.json $did"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main ingress-relay root@fedproxy.com "$HOME/.ssh/id_ed25519_no_password" "" "" "$@"
fi
