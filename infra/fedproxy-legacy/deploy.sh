#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

fedproxy_legacy_status() {
  infra_stub_status "caddy.service atprp-ssh-relay.service oauth-client-webapp.service caddy-check-dns-from-config.service" \
    https://fedproxy.com/ https://rp.fedproxy.com/ https://xrpc-test.fedproxy.com/
  infra_caddy_sites
}

fedproxy_legacy_deploy() {
  infra_stub_todo "caddy (xcaddy with the cloudflare dns module), atprp-ssh-relay, oauth-client-webapp and caddy-check-dns-from-config came from atproto-reverse-proxy/scripts/deploy.sh of the old org-root, which echoes CF_API_TOKEN; never run it. Port each unit here with infra_unit_install and the Caddyfile with infra_caddy_site_install, with CF_API_TOKEN from OpenBao, never from a file in the repo"
}

fedproxy_legacy_rollback() {
  infra_stub_todo "no rollback until the deploy is ported"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main fedproxy-legacy root@fedproxy.com "$HOME/.ssh/id_ed25519_no_password" "" "" "$@"
fi
