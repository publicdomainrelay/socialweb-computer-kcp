#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

gateway_defaults() {
  GATEWAY_ACCOUNT="${GATEWAY_ACCOUNT:-socialweb}"
  GATEWAY_STATE="${GATEWAY_STATE:-/var/lib/socialweb-computer/$GATEWAY_ACCOUNT}"
  GATEWAY_DOMAIN="${GATEWAY_DOMAIN:-socialweb.computer}"
  GATEWAY_DEPLOY_DIR="${GATEWAY_DEPLOY_DIR:-$HOME/.tmp/deploy}"
  GATEWAY_ENV_FILE="${GATEWAY_ENV_FILE-$GATEWAY_DEPLOY_DIR/gateway.env}"
  GATEWAY_SPEC_FILE="${GATEWAY_SPEC_FILE-$GATEWAY_DEPLOY_DIR/gateway-spec.yaml}"
  GATEWAY_PLATFORM="${GATEWAY_PLATFORM:-linux/amd64}"
  GATEWAY_UNIT=socialweb-computer-gateway.service
}

gateway_status() {
  gateway_defaults
  infra_ro_root "systemctl --user -M $(infra_q "$GATEWAY_ACCOUNT")@ show $GATEWAY_UNIT -p ActiveState -p SubState -p ExecMainStartTimestamp --no-pager | paste -sd' '
echo \"bin/current: \$(readlink $(infra_q "$GATEWAY_STATE/bin/current"))\"
echo \"bin/previous: \$(readlink $(infra_q "$GATEWAY_STATE/bin/previous") 2>/dev/null || echo none)\"
echo \"caddy sites: \$(ls -1 /etc/caddy/sites 2>/dev/null | tr '\n' ' ')\"
systemctl is-active caddy.service"
  infra_report_urls "https://k8s.$GATEWAY_DOMAIN/api/config" "https://kcp.$GATEWAY_DOMAIN/" "https://$GATEWAY_DOMAIN/" "https://ssh.$GATEWAY_DOMAIN/"
}

gateway_deploy() {
  local env_line
  gateway_defaults
  INFRA_BINARY="${GATEWAY_BINARY:-}"
  INFRA_SHA256="${GATEWAY_SHA256:-}"
  infra_release_binary "$GATEWAY_PLATFORM"
  if [ -n "$GATEWAY_ENV_FILE" ] && [ ! -f "$GATEWAY_ENV_FILE" ]; then
    infra_die "no GATEWAY_ENV_FILE at $GATEWAY_ENV_FILE (set it empty to ship none)"
  fi
  if [ -n "$GATEWAY_SPEC_FILE" ] && [ ! -f "$GATEWAY_SPEC_FILE" ]; then
    infra_die "no GATEWAY_SPEC_FILE at $GATEWAY_SPEC_FILE (set it empty to ship none)"
  fi
  env_line="BINARY=$INFRA_BINARY SHA256=$INFRA_SHA256 SSH_TARGET=$SSH_TARGET SSH_PORT=${SSH_PORT:-} SSH_KEY=${SSH_KEY:-} DOMAIN=$GATEWAY_DOMAIN ACCOUNT=$GATEWAY_ACCOUNT GATEWAY_ENV_FILE=$GATEWAY_ENV_FILE GATEWAY_SPEC_FILE=$GATEWAY_SPEC_FILE ALLOW_DEV_BINARY=${ALLOW_DEV_BINARY:-}"
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "[dry-run] local: $env_line $INFRA_REPO/scripts/deploy.sh"
    infra_log "it ships the binary, gateway.env and the spec (paths only here), installs or upgrades as $GATEWAY_ACCOUNT, writes the caddy sites, validates and reloads caddy"
    return 0
  fi
  infra_log "running scripts/deploy.sh for $INFRA_BINARY ($INFRA_SHA256)"
  BINARY="$INFRA_BINARY" SHA256="$INFRA_SHA256" SSH_TARGET="$SSH_TARGET" SSH_PORT="${SSH_PORT:-1997}" SSH_KEY="${SSH_KEY:-}" \
    DOMAIN="$GATEWAY_DOMAIN" ACCOUNT="$GATEWAY_ACCOUNT" STATE_DIR="$GATEWAY_STATE" \
    GATEWAY_ENV_FILE="$GATEWAY_ENV_FILE" GATEWAY_SPEC_FILE="$GATEWAY_SPEC_FILE" ALLOW_DEV_BINARY="${ALLOW_DEV_BINARY:-}" \
    bash "$INFRA_REPO/scripts/deploy.sh"
  infra_health_wait "https://k8s.$GATEWAY_DOMAIN/api/config" 200 30 2
  gateway_status
}

gateway_rollback() {
  gateway_defaults
  infra_rw_root "runuser -u $(infra_q "$GATEWAY_ACCOUNT") -- env XDG_RUNTIME_DIR=/run/user/\$(id -u $(infra_q "$GATEWAY_ACCOUNT")) CONFIG_PATH_SOCIALWEB_COMPUTER=$(infra_q "$GATEWAY_STATE/config.json") $(infra_q "$GATEWAY_STATE/bin/current/socialweb-computer") rollback"
  if [ "$INFRA_APPLY" = 1 ]; then
    infra_health_wait "https://k8s.$GATEWAY_DOMAIN/api/config" 200 30 2
    gateway_status
  fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main gateway root@socialweb.computer "$HOME/.ssh/id_ed25519_no_password" 1997 "" "$@"
fi
