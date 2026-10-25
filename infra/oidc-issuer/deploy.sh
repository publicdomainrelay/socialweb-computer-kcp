#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

oidc_issuer_defaults() {
  OIDC_ISSUER_HOSTNAME="${OIDC_ISSUER_HOSTNAME:-mini-cloud-0002.fedfork.com}"
  OIDC_ISSUER_PORT="${OIDC_ISSUER_PORT:-9000}"
}

oidc_issuer_status() {
  oidc_issuer_defaults
  infra_ro_root "if ss -ltnH 'sport = :$OIDC_ISSUER_PORT' | grep -q .; then echo 'listener on :$OIDC_ISSUER_PORT'; else echo 'nothing listens on :$OIDC_ISSUER_PORT'; fi"
  infra_report_urls "https://$OIDC_ISSUER_HOSTNAME/.well-known/openid-configuration"
}

oidc_issuer_deploy() {
  oidc_issuer_defaults
  infra_stub_todo "the caddy site $OIDC_ISSUER_HOSTNAME -> 127.0.0.1:$OIDC_ISSUER_PORT is left from the deno oidc-issuer (deno-macos-runner-desktop, digitalocean-bidder); nothing listens there. Remove the caddy block with a backup, or port the issuer"
}

oidc_issuer_rollback() {
  infra_stub_todo "nothing to roll back"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main oidc-issuer root@mini-cloud-0002.chadig.com "$HOME/.ssh/id_ed25519_no_password" "" "" "$@"
fi
