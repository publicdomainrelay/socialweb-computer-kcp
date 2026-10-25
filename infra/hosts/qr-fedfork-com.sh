#!/bin/bash
set -euo pipefail
. "$(cd "$(dirname "$0")/.." && pwd)/lib/common.sh"
. "$INFRA_DIR/did-key-associator/deploy.sh"

INFRA_HOST=qr-fedfork-com
INFRA_COMPONENTS=(did-key-associator)
SSH_TARGET="${SSH_TARGET:-root@qr.fedfork.com}"
SSH_KEY="${SSH_KEY-$HOME/.ssh/id_ed25519_no_password}"
SSH_PORT="${SSH_PORT-}"
INFRA_SUDO="${INFRA_SUDO-}"

infra_host_main "$@"
