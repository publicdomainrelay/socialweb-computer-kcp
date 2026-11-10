#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

ingress_relay_script() {
  local env_line="SSH_TARGET=$SSH_TARGET SSH_KEY=${SSH_KEY:-} CUSTOM_DOMAINS=${CUSTOM_DOMAINS:-1} NO_AUTO_ROLLBACK=${NO_AUTO_ROLLBACK:-}"
  if [ "$1" != --status ] && [ "$INFRA_APPLY" != 1 ]; then
    infra_log "[dry-run] local: $env_line $INFRA_REPO/scripts/deploy-xrpc-relay.sh $1"
    return 0
  fi
  if [ "$1" = --status ] && [ "$INFRA_PRINT_RO" = 1 ]; then
    infra_log "[dry-run] local (read-only): $env_line $INFRA_REPO/scripts/deploy-xrpc-relay.sh --status"
    return 0
  fi
  SSH_TARGET="$SSH_TARGET" SSH_KEY="${SSH_KEY:-}" bash "$INFRA_REPO/scripts/deploy-xrpc-relay.sh" ${1:+"$1"}
}

ingress_relay_deploy() {
  ingress_relay_script ""
}

ingress_relay_rollback() {
  ingress_relay_script --rollback
}

ingress_relay_status() {
  ingress_relay_script --status
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  key="$HOME/.ssh/id_ed25519_no_password"
  [ -f "$key" ] || key="$HOME/.ssh/id_ed25519"
  infra_component_main ingress-relay root@fedproxy.com "$key" "" "" "$@"
fi
