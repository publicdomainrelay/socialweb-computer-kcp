#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

hono_jsr_status() {
  infra_stub_status "hono-jsr.service"
  infra_ro_root "git -c safe.directory='*' -C /home/johnandersen777/prod-org-root/hono-jsr log -1 --format='checkout: %h %cs %s' | cut -c1-90; ss -ltnH 'sport = :8888' | awk '{print \"listen:\", \$4}'"
}

hono_jsr_deploy() {
  infra_stub_todo "hono-jsr.service (deno, hono-package-registry main.ts --port=8888 --store=git) runs from the prod-org-root checkout and was placed by hand; it listens on 0.0.0.0:8888 with no caddy site. Decide: retire it (the TypeScript is frozen) or port it here"
}

hono_jsr_rollback() {
  infra_stub_todo "no rollback until the deploy is ported"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main hono-jsr root@mini-cloud-0002.chadig.com "$HOME/.ssh/id_ed25519_no_password" "" "" "$@"
fi
