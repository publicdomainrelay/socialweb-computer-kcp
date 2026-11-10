#!/bin/bash
set -euo pipefail

SSH_TARGET="${SSH_TARGET:-root@fedproxy.com}"
if [ -z "${SSH_KEY:-}" ]; then
  SSH_KEY="$HOME/.ssh/id_ed25519_no_password"
  [ -f "$SSH_KEY" ] || SSH_KEY="$HOME/.ssh/id_ed25519"
fi
ROUTE_ID=route-xrpc-custom-domains
POLICY_ID=tls-policy-wildcard-fedproxy.com

remote() {
  ssh -o IdentitiesOnly=yes -i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=20 "$SSH_TARGET" "$@"
}

script='set -euo pipefail
api() { curl -sS --fail-with-body --unix-socket /opt/caddy/caddy-admin.sock "$@"; }
C=http://127.0.0.1
issuers() { api "$C/id/'$POLICY_ID'/issuers" | jq -c "[.[] | {module, dns: (.challenges.dns.provider.name // null)}]"; }
status() {
  echo "route:   $(api -o /dev/null -w "%{http_code}" "$C/id/'$ROUTE_ID'" 2>/dev/null || true)"
  echo "issuers: $(issuers)"
  echo "ask:     $(api "$C/config/apps/tls/automation/on_demand/permission/endpoint")"
}
case "$1" in
remove)
  [ "$(api "$C/id/'$POLICY_ID'/issuers" | jq length)" -lt 2 ] || api -X DELETE "$C/id/'$POLICY_ID'/issuers/1"
  api -o /dev/null "$C/id/'$ROUTE_ID'" 2>/dev/null && api -X DELETE "$C/id/'$ROUTE_ID'"
  ;;
status) ;;
*) exit 2 ;;
esac
status'

case "${1:-}" in
remove | status) remote "bash -c $(printf %q "$script") _ $1" ;;
*)
  echo "usage: $0 status|remove (docs/infra/FEDPROXY.md, Custom domains); env: SSH_TARGET (root@fedproxy.com), SSH_KEY (~/.ssh/id_ed25519_no_password, else ~/.ssh/id_ed25519)" >&2
  exit 2
  ;;
esac
