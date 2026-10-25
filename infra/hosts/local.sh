#!/bin/bash
set -euo pipefail
. "$(cd "$(dirname "$0")/.." && pwd)/lib/common.sh"
. "$INFRA_DIR/bidder/deploy.sh"

INFRA_HOST=local
INFRA_COMPONENTS=(bidder)
SSH_TARGET=local
SSH_KEY=""
SSH_PORT=""
INFRA_SUDO="${INFRA_SUDO-sudo -n}"

infra_host_main "$@"
