#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

did_key_associator_defaults() {
  DID_KEY_ASSOCIATOR_REPO="${DID_KEY_ASSOCIATOR_REPO:-$HOME/src/kcp/did-key-associator}"
  DID_KEY_ASSOCIATOR_MODE="${DID_KEY_ASSOCIATOR_MODE:-full}"
  DID_KEY_ASSOCIATOR_DOMAIN=qr.fedfork.com
  DID_KEY_ASSOCIATOR_UNIT=qr-fedfork-backend.service
  DID_KEY_ASSOCIATOR_BACKUPS=/root/qr-backup
}

did_key_associator_args() {
  DID_KEY_ASSOCIATOR_ARGS=()
  case "$DID_KEY_ASSOCIATOR_MODE" in
  full) ;;
  static) DID_KEY_ASSOCIATOR_ARGS+=(--static-only) ;;
  *) infra_die "DID_KEY_ASSOCIATOR_MODE is full or static, not $DID_KEY_ASSOCIATOR_MODE" ;;
  esac
}

did_key_associator_status() {
  did_key_associator_defaults
  infra_unit_state "$DID_KEY_ASSOCIATOR_UNIT" caddy.service
  infra_ro_root "echo \"backend sha256: \$(sha256sum /opt/qr-fedfork/qr-fedfork-backend | cut -d' ' -f1)\"; echo \"latest backups: \$(ls -1t $DID_KEY_ASSOCIATOR_BACKUPS 2>/dev/null | head -3 | tr '\n' ' ')\""
  infra_report_urls "https://$DID_KEY_ASSOCIATOR_DOMAIN/" "https://$DID_KEY_ASSOCIATOR_DOMAIN/xrpc/com.fedfork.atprotoOauthQR"
}

did_key_associator_deploy() {
  local script
  did_key_associator_defaults
  did_key_associator_args
  script="$DID_KEY_ASSOCIATOR_REPO/scripts/deploy.sh"
  if [ "$INFRA_APPLY" != 1 ]; then
    infra_log "[dry-run] local: SSH_TARGET=$SSH_TARGET SSH_KEY=${SSH_KEY:-<ssh config>} $script ${DID_KEY_ASSOCIATOR_ARGS[*]}"
    infra_log "it builds the static site and the backend, backs up to $DID_KEY_ASSOCIATOR_BACKUPS/<ts>, uploads, validates caddy and restores on a failure"
    if [ "${DID_KEY_ASSOCIATOR_DEEP_DRY_RUN:-}" = 1 ]; then
      SSH_TARGET="$SSH_TARGET" SSH_KEY="${SSH_KEY:-}" "$script" "${DID_KEY_ASSOCIATOR_ARGS[@]}" --dry-run
    else
      infra_log "DID_KEY_ASSOCIATOR_DEEP_DRY_RUN=1 runs its own --dry-run (a local build, no upload)"
    fi
    return 0
  fi
  [ -f "$script" ] || infra_die "no $script; set DID_KEY_ASSOCIATOR_REPO"
  [ -z "${SSH_PORT:-}" ] || infra_die "$script takes no SSH_PORT"
  SSH_TARGET="$SSH_TARGET" SSH_KEY="${SSH_KEY:-}" bash "$script" "${DID_KEY_ASSOCIATOR_ARGS[@]}"
  infra_health_wait "https://$DID_KEY_ASSOCIATOR_DOMAIN/" 200 15 2
  infra_health_wait "https://$DID_KEY_ASSOCIATOR_DOMAIN/xrpc/com.fedfork.atprotoOauthQR" 401 15 2
}

did_key_associator_rollback() {
  local pick
  did_key_associator_defaults
  if [ -n "${DID_KEY_ASSOCIATOR_BACKUP_TS:-}" ]; then
    pick="$(infra_q "$DID_KEY_ASSOCIATOR_BACKUPS/$DID_KEY_ASSOCIATOR_BACKUP_TS")"
  else
    pick="\$(ls -1dt $DID_KEY_ASSOCIATOR_BACKUPS/*/ | head -1)"
  fi
  infra_rw_root "set -e
b=$pick
[ -f \"\$b/restore.sh\" ] || { echo \"no restore.sh in \$b\" >&2; exit 1; }
echo \"restoring from \$b\"
bash \"\$b/restore.sh\""
  infra_health_wait "https://$DID_KEY_ASSOCIATOR_DOMAIN/" 200 15 2
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main did-key-associator root@qr.fedfork.com "$HOME/.ssh/id_ed25519_no_password" "" "" "$@"
fi
