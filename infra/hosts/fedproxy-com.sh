#!/bin/bash
set -euo pipefail
. "$(cd "$(dirname "$0")/.." && pwd)/lib/common.sh"
. "$INFRA_DIR/ingress-relay/deploy.sh"
. "$INFRA_DIR/fedproxy-legacy/deploy.sh"

INFRA_HOST=fedproxy-com
INFRA_COMPONENTS=(ingress-relay fedproxy-legacy)
SSH_TARGET="${SSH_TARGET:-root@fedproxy.com}"
if [ -z "${SSH_KEY+x}" ]; then
  SSH_KEY="$HOME/.ssh/id_ed25519_no_password"
  [ -f "$SSH_KEY" ] || SSH_KEY="$HOME/.ssh/id_ed25519"
fi
SSH_PORT="${SSH_PORT-}"
INFRA_SUDO="${INFRA_SUDO-}"

infra_host_main "$@"
