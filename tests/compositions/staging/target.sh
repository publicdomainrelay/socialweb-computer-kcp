#!/usr/bin/env bash
# On the staging VM, as stg, after dev up: two throwaway accounts on the network's bidder PDS and the suite's target
# file (the targets/prod.json shape) for the market network instance stg. Usage: target.sh <run> > target-staging.json
# restartSshMaxS 20: with --network a restarted invocation re-places itself as a network child (~7 s) and its requester
# becomes Ready again (~7 s) before ssh; prod keeps 10.
set -euo pipefail
run=$1
S=${SWC:-/home/stg/stg/socialweb-computer-linux-amd64} ST=${SWC_STATE:-/home/stg/stg/s} I=${SWC_INSTANCE:-stg} C=${SWC_CREDS:-/home/stg}
N=(--instance "$I" --state-dir "$ST")
st=$($S dev status "${N[@]}" 2>/dev/null)
eval "$($S dev env "${N[@]}" 2>/dev/null)"
pds=$(jq -r '.network.status.bidderPdsUrl' <<<"$st")
host() { jq -r --arg n "$1" '.network.status.network.hosts[] | select(.name == $n) | .address' <<<"$st"; }
plc=$(jq -r '.network.status.network.plcDirectoryUrl' <<<"$st")
accounts='{}'
for a in alice bob; do
  h="$a-${run: -4}$(date +%s | tail -c 5).bob.test" pw=$(head -c 24 /dev/urandom | base64 | tr -dc A-Za-z0-9) c=$C/creds-$a
  did=$(curl -fsS -H 'content-type: application/json' "$pds/xrpc/com.atproto.server.createAccount" \
    -d "$(jq -nc --arg h "$h" --arg p "$pw" '{handle: $h, password: $p, email: ($h + "@staging.invalid")}')" | jq -r .did)
  (umask 077 && echo "ATPROTO_PASSWORD=$pw" > "$c")
  accounts=$(jq -c --arg a "$a" --arg h "$h" --arg d "$did" --arg c "$c" '.[$a] = {handle: $h, did: $d, creds: $c, throwaway: true}' <<<"$accounts")
done
bidders=$(jq -c '[.network.status.apps[] | select(.kind == "MarketBidder") | {name, did}]' <<<"$st" | jq -c '.[]' | while read -r b; do
  n=$(jq -r .name <<<"$b")
  op=$(curl -fsS "$plc/$(jq -r .did <<<"$b")" | jq -r '[.alsoKnownAs[] | select(startswith("https://ident.link/agent-of/"))][0] // "" | ltrimstr("https://ident.link/agent-of/")')
  jq -c --arg op "$op" --arg kc "$KUBECONFIG" --arg ws "root:$n" '. + {operator: $op, kcp: {kubeconfig: $kc, workspace: $ws, via: {kind: "direct"}}}' <<<"$b"
done | jq -sc .)
# the network's one XRPC entry: its front (status.network.xrpcFront) serves the PLC, the relay and every PDS by name
# (their ingress Exposures, *.relay.localhost); the test host sends every named request there, trusting the network CA
ca=$C/network-ca.pem
jq -r '.network.status.network.caBundle' <<<"$st" > "$ca"
front=$(jq -r '.network.status.network.xrpcFront // empty' <<<"$st")
[ -n "$front" ] || front=$(host dispatcher-tls.default.relay.svc.kcp.local)
# an ingress Exposure's https name, from the network's kcp root (curl on the admin token: no kubectl on a staging VM)
ktoken=$(awk '/ token:/{print $2; exit}' "$KUBECONFIG") kserver=$(awk '/ server:/{print $2; exit}' "$KUBECONFIG")
named() { curl -fsSk -H "Authorization: Bearer $ktoken" "${kserver%%/clusters/*}/clusters/root/apis/exposure.socialweb.computer/v1alpha1/exposures/$1" | jq -er .status.address; }
# pds stays the bidder PDS's own URL: the requester under test (run request) signs in there, and it does not go through the front
jq -n --arg plc "$(named svc-plc-plc-ingress)" --arg pds "$pds" --arg relay "$(named svc-relay-relay-ingress)" \
  --arg front "$front" --arg ca "$ca" \
  --argjson accounts "$accounts" --argjson bidders "$bidders" --arg s "$S" --arg i "$I" --arg st "$ST" '{
  name: "staging", restartSshMaxS: 20, plc: $plc, pds: $pds, relay: $relay, xrpcFront: {address: $front, ca: $ca}, requester: ("path:" + $s),
  network: {instance: $i, stateDir: $st}, accounts: $accounts, bidders: $bidders,
  gateway: {url: "https://gateway.staging.invalid", aud: "did:web:gateway.staging.invalid", ssh: "gateway.staging.invalid"}
}'
