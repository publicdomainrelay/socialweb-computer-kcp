#!/bin/bash
set -euo pipefail
. "$(cd "$(dirname "$0")/.." && pwd)/lib/common.sh"
. "$INFRA_DIR/tray/deploy.sh"

INFRA_HOST=tray-fedfork-com
INFRA_COMPONENTS=(tray)
SSH_TARGET="${SSH_TARGET:-root@tray.fedfork.com}"
SSH_KEY="${SSH_KEY-$HOME/.ssh/id_ed25519_no_password}"
SSH_PORT="${SSH_PORT-}"
INFRA_SUDO="${INFRA_SUDO-}"

infra_host_main "$@"
