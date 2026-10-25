#!/bin/bash
set -euo pipefail
. "$(cd "$(dirname "$0")/.." && pwd)/lib/common.sh"
. "$INFRA_DIR/bidder/deploy.sh"

INFRA_HOST=hat-7
INFRA_COMPONENTS=(bidder)
SSH_TARGET="${SSH_TARGET:-hat-7}"
SSH_KEY="${SSH_KEY-}"
SSH_PORT="${SSH_PORT-}"
INFRA_SUDO="${INFRA_SUDO-sudo -n}"

infra_host_main "$@"
