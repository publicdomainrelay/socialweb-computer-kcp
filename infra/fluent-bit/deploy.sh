#!/bin/bash
if [ -z "${INFRA_LIB:-}" ]; then
  . "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
fi

fluent_bit_status() {
  infra_stub_status "fluent-bit.service"
}

fluent_bit_deploy() {
  infra_stub_todo "fluent-bit came from atproto-reverse-proxy/scripts/deploy-fluent-bit.sh of the old org-root (doctl lookups of the OpenSearch cluster and droplets, /etc/fluent-bit/fluent-bit.conf, the OpenSearch CA). Port it here with the OpenSearch credentials from OpenBao"
}

fluent_bit_rollback() {
  infra_stub_todo "no rollback until the deploy is ported"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  infra_component_main fluent-bit root@mini-cloud-0001.chadig.com "$HOME/.ssh/id_ed25519_no_password" 1997 "" "$@"
fi
