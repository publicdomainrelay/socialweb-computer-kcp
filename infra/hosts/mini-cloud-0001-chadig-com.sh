#!/bin/bash
set -euo pipefail
. "$(cd "$(dirname "$0")/.." && pwd)/lib/common.sh"
. "$INFRA_DIR/fluent-bit/deploy.sh"
. "$INFRA_DIR/legacy-poc/deploy.sh"
. "$INFRA_DIR/oidc-issuer/deploy.sh"

INFRA_HOST=mini-cloud-0001-chadig-com
INFRA_COMPONENTS=(legacy-poc fluent-bit oidc-issuer)
SSH_TARGET="${SSH_TARGET:-root@mini-cloud-0001.chadig.com}"
SSH_KEY="${SSH_KEY-$HOME/.ssh/id_ed25519_no_password}"
SSH_PORT="${SSH_PORT-1997}"
INFRA_SUDO="${INFRA_SUDO-}"
OIDC_ISSUER_HOSTNAME="${OIDC_ISSUER_HOSTNAME:-mini-cloud-0001.fedfork.com}"

infra_host_main "$@"
