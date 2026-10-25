#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

legacy_poc_status() {
  infra_stub_status "spindle.service market-registry.service market-registry-tunnel.service bidder-tunnel.service bidder.service claude-rc.service" \
    https://ui.fedfork.com/ https://tangled.fedcicd.com/ https://jsr.publicdomainrelay.com/ https://mini-cloud-0001.fedfork.com/
  infra_caddy_sites
}

legacy_poc_deploy() {
  infra_stub_todo "the compute-contract-reference-implementation-poc units (spindle, market-registry, the two ssh -R tunnels, bidder, claude-rc) run from ~/prod-compute-contract-reference-implementation-poc and were placed by hand; they predate the org-root. Decide per unit: retire, or port to a component here"
}

legacy_poc_rollback() {
  infra_stub_todo "nothing to roll back"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main legacy-poc root@mini-cloud-0001.chadig.com "$HOME/.ssh/id_ed25519_no_password" 1997 "" "$@"
fi
