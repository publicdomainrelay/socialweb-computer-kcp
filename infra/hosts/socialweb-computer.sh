#!/bin/bash
set -euo pipefail
. "$(cd "$(dirname "$0")/.." && pwd)/lib/common.sh"
. "$INFRA_DIR/gateway/deploy.sh"

INFRA_HOST=socialweb-computer
INFRA_COMPONENTS=(gateway)
SSH_TARGET="${SSH_TARGET:-root@socialweb.computer}"
SSH_KEY="${SSH_KEY-$HOME/.ssh/id_ed25519_no_password}"
SSH_PORT="${SSH_PORT-1997}"
INFRA_SUDO="${INFRA_SUDO-}"

infra_host_main "$@"
