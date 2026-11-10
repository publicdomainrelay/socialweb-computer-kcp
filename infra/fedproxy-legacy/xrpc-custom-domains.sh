#!/bin/bash
# Custom domains on the xrpc ingress relay, through Caddy's admin API (never a Caddyfile reload).
#
# atprp-ssh-relay POSTs every SSH tunnel's route to the END of srv0's routes and owns the
# subject-less DNS-01 automation policy, so a hostless Caddyfile site block (`https:// { ... }`) would
# shadow every tunnel route appended after it and take over the *.fedproxy.com wildcard's issuance.
# Instead, three idempotent writes:
#   1. route  route-xrpc-custom-domains: every host NOT under fedproxy.com -> the relay socket. Disjoint
#      from atprp's routes, so its place in the array does not matter.
#   2. issuer a plain ACME issuer (HTTP-01 / TLS-ALPN-01) appended to atprp's policy
#      tls-policy-wildcard-fedproxy.com, after its Cloudflare DNS-01 issuer: names outside the
#      Cloudflare zone (mush.at) fail fast on the first issuer and are issued by the second.
#   3. ask    on_demand permission -> the relay's ask listener (5556; /etc/caddy/Caddyfile says the same).
# A Caddy restart drops 1 and 2 (atprp re-creates its policy without the fallback): run apply again.
#
# usage: infra/fedproxy-legacy/xrpc-custom-domains.sh apply|remove|status
# env: SSH_TARGET (root@fedproxy.com), SSH_KEY (~/.ssh/id_ed25519), ASK (http://127.0.0.1:5556/check),
#      LEGACY_ASK (http://127.0.0.1:5555/check, what remove restores)
set -euo pipefail

SSH_TARGET="${SSH_TARGET:-root@fedproxy.com}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
ASK="${ASK:-http://127.0.0.1:5556/check}"
LEGACY_ASK="${LEGACY_ASK:-http://127.0.0.1:5555/check}"
ROUTE_ID=route-xrpc-custom-domains
POLICY_ID=tls-policy-wildcard-fedproxy.com

route='{"@id":"'$ROUTE_ID'","match":[{"not":[{"host":["fedproxy.com","*.fedproxy.com","*.*.fedproxy.com","*.*.*.fedproxy.com"]}]}],"handle":[{"handler":"subroute","routes":[{"handle":[{"flush_interval":-1,"handler":"reverse_proxy","stream_close_delay":300000000000,"transport":{"keep_alive":{"probe_interval":30000000000},"protocol":"http"},"upstreams":[{"dial":"unix//opt/caddy/xrpc-relay-server.sock"}]}]}]}],"terminal":true}'

remote() {
  ssh -o IdentitiesOnly=yes -i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=20 "$SSH_TARGET" "$@"
}

# The remote side: $1 = action. Prints no secrets (the policy's api_token never leaves jq).
script='set -euo pipefail
api() { curl -sS --fail-with-body --unix-socket /opt/caddy/caddy-admin.sock "$@"; }
C=http://127.0.0.1
issuers() { api "$C/id/'$POLICY_ID'/issuers" | jq -c "[.[] | {module, dns: (.challenges.dns.provider.name // null)}]"; }
status() {
  echo "route:   $(api -o /dev/null -w "%{http_code}" "$C/id/'$ROUTE_ID'" || true)"
  echo "issuers: $(issuers)"
  echo "ask:     $(api "$C/config/apps/tls/automation/on_demand/permission/endpoint")"
}
case "$1" in
apply)
  api -o /dev/null "$C/id/'$ROUTE_ID'" 2>/dev/null || api -X POST -H "Content-Type: application/json" -d '"'"$route"'"' "$C/config/apps/http/servers/srv0/routes"
  [ "$(api "$C/id/'$POLICY_ID'/issuers" | jq length)" -ge 2 ] || api -X POST -H "Content-Type: application/json" -d "{\"module\":\"acme\"}" "$C/id/'$POLICY_ID'/issuers"
  [ "$(api "$C/config/apps/tls/automation/on_demand/permission/endpoint")" = "\"'"$ASK"'\"" ] || api -X PATCH -H "Content-Type: application/json" -d "\"'"$ASK"'\"" "$C/config/apps/tls/automation/on_demand/permission/endpoint"
  ;;
remove)
  api -X PATCH -H "Content-Type: application/json" -d "\"'"$LEGACY_ASK"'\"" "$C/config/apps/tls/automation/on_demand/permission/endpoint"
  [ "$(api "$C/id/'$POLICY_ID'/issuers" | jq length)" -lt 2 ] || api -X DELETE "$C/id/'$POLICY_ID'/issuers/1"
  api -o /dev/null "$C/id/'$ROUTE_ID'" 2>/dev/null && api -X DELETE "$C/id/'$ROUTE_ID'"
  ;;
status) ;;
*) exit 2 ;;
esac
status'

case "${1:-}" in
apply | remove | status) remote "bash -c $(printf %q "$script") _ $1" ;;
*)
  echo "usage: $0 apply|remove|status" >&2
  exit 2
  ;;
esac
