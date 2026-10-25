#!/bin/bash
set -euo pipefail
. "$(cd "$(dirname "$0")/.." && pwd)/lib/common.sh"
. "$INFRA_DIR/atproto-relay/deploy.sh"
. "$INFRA_DIR/hono-jsr/deploy.sh"
. "$INFRA_DIR/org-root-git-pull/deploy.sh"
. "$INFRA_DIR/oidc-issuer/deploy.sh"

INFRA_HOST=mini-cloud-0002-chadig-com
INFRA_COMPONENTS=(atproto-relay hono-jsr org-root-git-pull oidc-issuer)
SSH_TARGET="${SSH_TARGET:-root@mini-cloud-0002.chadig.com}"
SSH_KEY="${SSH_KEY-$HOME/.ssh/id_ed25519_no_password}"
SSH_PORT="${SSH_PORT-}"
INFRA_SUDO="${INFRA_SUDO-}"
OIDC_ISSUER_HOSTNAME="${OIDC_ISSUER_HOSTNAME:-mini-cloud-0002.fedfork.com}"

infra_host_main "$@"
