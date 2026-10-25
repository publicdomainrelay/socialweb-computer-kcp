#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

tray_status() {
  infra_stub_status "caddy.service" https://tray.fedfork.com/
}

tray_deploy() {
  infra_stub_todo "tray.fedfork.com (deno-macos-runner-desktop static site, /var/www/tray.fedfork.com behind caddy) came from deno-macos-runner-desktop/scripts/deploy.sh of the old org-root; the host was not enumerated and answers 404. Port it with infra_upload and infra_caddy_site_install, or retire it"
}

tray_rollback() {
  infra_stub_todo "no rollback until the deploy is ported"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main tray root@tray.fedfork.com "$HOME/.ssh/id_ed25519_no_password" "" "" "$@"
fi
