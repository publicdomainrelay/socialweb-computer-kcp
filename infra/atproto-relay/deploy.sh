#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

atproto_relay_defaults() {
  ATPROTO_RELAY_SCRIPT="${ATPROTO_RELAY_SCRIPT:-$INFRA_REPO/scripts/deploy-atproto-relay.sh}"
  ATPROTO_RELAY_HOSTNAME="${ATPROTO_RELAY_HOSTNAME:-relay.mini-cloud-0002.chadig.com}"
  ATPROTO_RELAY_UNIT="${ATPROTO_RELAY_UNIT:-atproto-relay.service}"
  ATPROTO_RELAY_CHECKOUT="${ATPROTO_RELAY_CHECKOUT:-/home/johnandersen777/prod-org-root/atproto-relay}"
}

atproto_relay_status() {
  atproto_relay_defaults
  infra_unit_state "$ATPROTO_RELAY_UNIT"
  infra_ro_root "systemctl show $(infra_q "$ATPROTO_RELAY_UNIT") -p ExecStart --no-pager | sed -n 's/.*argv\\[\\]=\\([^;]*\\);.*/ExecStart=\\1/p'
echo \"checkout: \$(git -c safe.directory='*' -C $(infra_q "$ATPROTO_RELAY_CHECKOUT") log -1 --format='%h %cs %s' 2>/dev/null | cut -c1-80)\""
  infra_report_urls "https://$ATPROTO_RELAY_HOSTNAME/xrpc/_health"
  if [ ! -f "$ATPROTO_RELAY_SCRIPT" ]; then
    infra_log "$ATPROTO_RELAY_SCRIPT has not landed; deploy and rollback stay stubs until it does"
  fi
}

atproto_relay_delegate() {
  atproto_relay_defaults
  if [ ! -f "$ATPROTO_RELAY_SCRIPT" ]; then
    infra_stub_todo "the Go atproto relay (internal atproto-relay) deploys with $ATPROTO_RELAY_SCRIPT, which has not landed; fold its logic into this file once it does"
    return 0
  fi
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "[dry-run] local: SSH_TARGET=$SSH_TARGET SSH_KEY=${SSH_KEY:-} bash $ATPROTO_RELAY_SCRIPT $*"
    return 0
  fi
  SSH_TARGET="$SSH_TARGET" SSH_KEY="${SSH_KEY:-}" bash "$ATPROTO_RELAY_SCRIPT" "$@"
}

atproto_relay_deploy() {
  atproto_relay_delegate
}

atproto_relay_rollback() {
  atproto_relay_delegate --rollback
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main atproto-relay root@mini-cloud-0002.chadig.com "$HOME/.ssh/id_ed25519_no_password" "" "" "$@"
fi
