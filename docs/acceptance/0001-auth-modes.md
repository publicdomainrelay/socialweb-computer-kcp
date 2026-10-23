# Acceptance 0001: account modes, per-component pause, and one VM per host

This is the runnable acceptance used for plans 0023 and 0024. It exercises:

- a localRepo bidder;
- an oauthQr bidder that receives the market over the prod PDS firehose;
- a localRepo gateway requester;
- per-component suspend without stopping the GuestControlPlane or the shared IngressProxy;
- typed Bidder settings and fail-closed overrides;
- a tenant k3s cluster that scales from zero to three market VMs and back to zero,
  placing one VM on each bidder host.

Run it from a host with `goat`, `kubectl`, `ssh` to `socialweb.computer`, `hat-7`
and `hat-9`, and the credential files named below. It changes the prod gateway
and the bidders. Do not run it without that authority.

This runbook was validated end to end on `v0.0.0-rc81`: three pods on three
distinct tenant nodes, one market VM on each bidder host, paused winners forcing
the next placement, and every VM released when the cluster returned to zero.

The authoritative OAuth role sets are in
`internal/atproto/oauth/scope.go`, ported from
`typescript-helpers/lib/oauth-scope/mod.ts` (the generator behind
`did-key-associator/generated/oauth-scope.js`). Do not substitute
`transition:generic`.

## Variables

```bash
GW=https://k8s.socialweb.computer
AUD=did:web:k8s.socialweb.computer
N=com.publicdomainrelay.temp.cluster
CL=scale-auth-$(date +%s)
W=~/.tmp/auth-modes-0001
mkdir -p "$W"
chmod 700 "$W"

PROD_K=/var/lib/socialweb-computer/socialweb/kcp/admin.kubeconfig
PROD_ACCOUNT=2wjeqtgx
H7_K=$(ssh hat-7 'sudo -n -u swc-bidder sh -c "ls /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/admin.kubeconfig | head -1"')
H9_K=$(ssh hat-9 'sudo -n -u swc-bidder sh -c "ls /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/admin.kubeconfig | head -1"')
LOCAL_K=$(sudo -n -u swc-bidder sh -c "ls /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/admin.kubeconfig | head -1")
```

The gateway account workspace name (`PROD_ACCOUNT`) appears in the cluster
provider log line `home":"root:<account>:<cluster>"`. It is stable for a state
dir.

## Requester association

The gateway requester must be associated before it accepts any bid. Check it
before creating a cluster; a gateway restart can leave the operator claim
missing.

```bash
ID_ARGS="--kubeconfig $PROD_K --server https://127.0.0.1:6443/clusters/root:app"
ASSOC=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl $ID_ARGS get marketidentity requester -o jsonpath='{.status.associated}'")
RI_DID=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl $ID_ARGS get marketidentity requester -o jsonpath='{.status.did}'")
printf 'requester=%s associated=%s\n' "$RI_DID" "$ASSOC"
if [ "$ASSOC" != True ]; then
  OP_DID=$(deno run -A --no-lock scripts/associate-bidder.ts --creds ~/.aliceoa_bsky_social_source \
    --account ALICE --requester-did "$RI_DID" | jq -r .operatorDid)
  ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl $ID_ARGS patch marketidentity requester --type merge -p '{\"spec\":{\"approvedAssociations\":[\"$OP_DID\"]}}'"
fi
```

Wait until `associated=True` and the `Requester` child reports
`spec.operatorDID=$OP_DID`. Without that, every bid is denied by
`requester-tangled-vouch` even though bidder policy runs succeed.

## Service auth for the gateway

The gateway XRPC calls are authenticated as the Alice operator account.

```bash
if ! goat account check-auth >/dev/null 2>&1; then
  set -a
  . ~/.aliceoa_bsky_social_source
  set +a
  GOAT_USERNAME=$ALICE_ATPROTO_HANDLE GOAT_PASSWORD=$ALICE_ATPROTO_PASSWORD goat account login
fi
tok() { goat account service-auth --aud "$AUD" --lxm "$N.$1"; }
xrpc() {
  verb=$1
  method=$2
  shift 2
  goat xrpc "$verb" "$GW" "$N.$method" "$@" "Authorization:Bearer $(tok "$method")"
}
```

## 1. Pause hat-9 and local, leave hat-7 receiving

Pause only the `Bidder` child. The GuestControlPlane, Pds, IngressProxy, Services
and Exposures stay Ready.

```bash
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":{\"suspend\":true}}}}'"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl patch marketbidder app --type merge -p '{"spec":{"components":{"Bidder":{"suspend":true}}}}'
```

Check both roots:

```bash
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl get marketbidder app -o jsonpath='{.status.phase}{\"\n\"}{.status.componentSummary}{\"\n\"}'"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl get marketbidder app -o jsonpath='{.status.phase}{"\n"}{.status.componentSummary}{"\n"}'
```

Both must be `Ready` with `{"desired":5,"notReady":0,"ready":4,"suspended":1}`.

## 2. Resume hat-7 and point it at the gateway PDS firehose

Read the gateway PDS ingress URL from its Exposure. Then set it as the hat-7
receive transport. The oauthQr Bidder advertises no inbound market endpoint; it
receives `market.rfp` and `market.accept` from this stream.

```bash
PDS_URL=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K get exposure svc-pds-pds-ingress -o jsonpath='{.status.url}'")
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"suspend\":false,\"network\":{\"firehoseUrls\":[\"$PDS_URL\"]}}}'"
```

Re-apply the patch after any bidder restart: the app root does not keep
`network.firehoseUrls` across a restart, and without it the bidder fails closed
with "comms mode Firehose has no relay or firehose URL".

The root must become Ready and one EventStream must be running:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketbidder app -o jsonpath='{.status.phase}{\"\n\"}{.status.message}{\"\n\"}'"
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get eventstream -A"
```

If the MarketIdentity is `AwaitingScan`, run the headless sign-in with Bob's
credential file. Read the current QR url immediately before the command; it
expires after 5 minutes.

```bash
U=$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketidentity bidder -o jsonpath='{.status.pendingAction.url}'")
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.bobvmbuilder_bsky_social_source \
  --account BOB --url "$U" --done "You can close this window"
```

A timeout waiting for the final page text is harmless when the identity is
`Ready` afterwards. The stored scope must be the authoritative bidder set:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketidentity bidder -o jsonpath='{.status.phase}{\"\n\"}'"
```

## 3. Create the tenant cluster

```bash
xrpc procedure createCluster name="$CL"
for i in $(seq 1 60); do
  phase=$(xrpc query listClusters | jq -r --arg n "$CL" '.clusters[] | select(.name==$n) | .phase')
  [ "$phase" = Ready ] && break
  sleep 5
done
[ "$phase" = Ready ] || { echo "cluster $CL is $phase"; exit 1; }
xrpc procedure getKubeconfig name="$CL" ttlSeconds:=28800 | jq -r .kubeconfig > "$W/kubeconfig"
chmod 600 "$W/kubeconfig"
KC=$W/kubeconfig
```

## 4. First scale: one oauthQr VM on hat-7, then zero

With hat-9 and local paused, only hat-7 can bid. The deployment uses required
pod anti-affinity on `kubernetes.io/hostname`, so each replica needs a different
node, and each node is one market VM.

```bash
kubectl --kubeconfig "$KC" apply -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: scale-check
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: scale-check
  template:
    metadata:
      labels:
        app: scale-check
    spec:
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - labelSelector:
                matchLabels:
                  app: scale-check
              topologyKey: kubernetes.io/hostname
      containers:
        - name: run
          image: docker.io/library/busybox:1.36.1
          command: ["sh", "-c", "sleep 3600"]
YAML
kubectl --kubeconfig "$KC" wait --for=condition=Ready pod -l app=scale-check --timeout=15m
kubectl --kubeconfig "$KC" get pods -o wide
```

Read the winner and the VM:

```bash
NC="https://127.0.0.1:6443/clusters/root:$PROD_ACCOUNT:$CL"
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims -o wide"
```

The `WINNER` column is one of the bidder DIDs. Read the three DIDs once:

```bash
D7=$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketbidder app -o jsonpath='{.status.did}'")
D9=$(ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl get marketbidder app -o jsonpath='{.status.did}'")
DL=$(sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K kubectl get marketbidder app -o jsonpath='{.status.did}')
printf 'hat-7=%s\nhat-9=%s\nlocal=%s\n' "$D7" "$D9" "$DL"
```

Only hat-7 is unpaused, so the first winner must be `$D7`:

```bash
WINNER=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims -o jsonpath='{.items[0].status.contract.winner.did}'")
[ "$WINNER" = "$D7" ] || { echo "first VM landed on $WINNER, not hat-7 ($D7)"; exit 1; }
kubectl --kubeconfig "$KC" scale deployment scale-check --replicas=0
for i in $(seq 1 120); do
  nodes=$(kubectl --kubeconfig "$KC" get nodes --no-headers 2>/dev/null | wc -l)
  [ "$nodes" = 0 ] && break
  sleep 5
done
[ "$nodes" = 0 ] || { echo "cluster still has $nodes nodes"; exit 1; }
```

The provider holds an idle node for two minutes before it requests release, so
this loop can take several minutes after the last pod goes away. That hold is
normal, not a hung scale-down.

That proves the oauthQr bidder on hat-7 alone takes the cluster from zero to one
node and back to zero. The NodeClaim list is empty or every claim is `Released`.
The release is not complete until the bidder has destroyed the VM. Check that
hat-7 holds no Running ComputeVM for this cluster:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get computevms -o wide"
```

## 5. One VM per host: resume hat-9 and local

Resume the two paused bidders, then run the same deployment one VM at a time.
After each VM is Ready, pause the bidder that won it; the next scale-up must use
the next host.

```bash
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":null}}'"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl patch marketbidder app --type merge -p '{"spec":{"components":null}}'
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl wait --for=condition=Ready bidder/bidder --timeout=120s"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl wait --for=condition=Ready bidder/bidder --timeout=120s

kubectl --kubeconfig "$KC" scale deployment scale-check --replicas=1
kubectl --kubeconfig "$KC" wait --for=condition=Ready pod -l app=scale-check --timeout=15m
WINNER=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims -o jsonpath='{.items[0].status.contract.winner.did}'")
case "$WINNER" in
  "$D7") HOST=hat-7 ;;
  "$D9") HOST=hat-9 ;;
  "$DL") HOST=local ;;
  *) echo "winner $WINNER is not one of the three market bidders"; exit 1 ;;
esac
printf 'first winner=%s host=%s\n' "$WINNER" "$HOST"
```

Pause that winner:

```bash
case "$HOST" in
  hat-7)
    ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":{\"suspend\":true}}}}'"
    ;;
  hat-9)
    ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":{\"suspend\":true}}}}'"
    ;;
  local)
    sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
      kubectl patch marketbidder app --type merge -p '{"spec":{"components":{"Bidder":{"suspend":true}}}}'
    ;;
esac
```

Scale to two; wait, read the second winner, and pause it too:

```bash
kubectl --kubeconfig "$KC" scale deployment scale-check --replicas=2
kubectl --kubeconfig "$KC" wait --for=condition=Ready pod -l app=scale-check --timeout=15m
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims -o wide"
```

Repeat the host map/pause block for the new winner. Scale to three; the final
winner must be the one host still unpaused:

```bash
kubectl --kubeconfig "$KC" scale deployment scale-check --replicas=3
kubectl --kubeconfig "$KC" wait --for=condition=Ready pod -l app=scale-check --timeout=15m
kubectl --kubeconfig "$KC" get pods -o wide
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims -o wide"
```

Pass condition: three pods are Ready, the NodeClaims name three distinct
winners, and the three `NODE` names in `get pods -o wide` are distinct.

## 6. Resume paused winners, scale to zero, delete the cluster

A suspended Bidder stops bidding, but it also stops reading the market inbox.
Resume every paused winner before scaling to zero, or that host cannot see the
requester's `vm.delete` and its VM cannot be released.

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":null}}'"
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":null}}'"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl patch marketbidder app --type merge -p '{"spec":{"components":null}}'
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl wait --for=condition=Ready bidder/bidder --timeout=120s"
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl wait --for=condition=Ready bidder/bidder --timeout=120s"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl wait --for=condition=Ready bidder/bidder --timeout=120s

kubectl --kubeconfig "$KC" delete deployment scale-check
for i in $(seq 1 120); do
  nodes=$(kubectl --kubeconfig "$KC" get nodes --no-headers 2>/dev/null | wc -l)
  [ "$nodes" = 0 ] && break
  sleep 5
done
[ "$nodes" = 0 ] || { echo "cluster still has $nodes nodes"; exit 1; }
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims -o wide"
```

The claim list is empty (or every claim is `Released`). Check every bidder host:
no Running ComputeVM may remain, and no `socialweb-vm-*` unit may remain for
this cluster.

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get computevms -o wide"
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl get computevms -o wide"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl get computevms -o wide
```

Delete the cluster and the scratch directory:

```bash
xrpc procedure deleteCluster name="$CL"
rm -rf "$W"
```

## 7. Per-component pause checks (A1-A4)

On the oauthQr bidder, record the heartbeat `createdAt` before pausing:

```bash
BOB=$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketidentity bidder -o jsonpath='{.status.did}'")
PDS=$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketidentity bidder -o jsonpath='{.status.pdsUrl}'")
hb() { curl -sS "$PDS/xrpc/com.atproto.repo.getRecord?repo=$BOB&collection=com.publicdomainrelay.temp.market.heartbeat&rkey=self" | jq -r .value.createdAt; }
hb
```

Pause only Bidder, wait 180 s, and compare:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":{\"suspend\":true}}}}'"
sleep 180
hb
```

`createdAt` must be identical. On hat-7 the root must be Ready with
`{"desired":3,"notReady":0,"ready":2,"suspended":1}`; GuestControlPlane stays
Ready; the IngressProxy stays Ready; the GuestControlPlane ingress URL answers:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketbidder app -o jsonpath='{.status.phase}{\"\n\"}{.status.componentSummary}{\"\n\"}'"
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get guestcontrolplane,ingressproxy -A"
curl -sS "https://$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get exposure svc-guestcontrolplane-guest-kcp-ingress -o jsonpath='{.status.url}'" | sed 's#https://##')/healthz"
```

Resume, wait for Ready, and prove the heartbeat advances:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":{\"suspend\":false}}}}'"
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl wait --for=condition=Ready bidder/bidder --timeout=120s"
hb
```

## 8. Typed settings, drift and fail-closed overrides

On any Ready bidder, set a 5 s heartbeat and disable offering refresh:

```bash
K=$H7_K
HOST=hat-7
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":{\"suspend\":false,\"settings\":{\"bidder\":{\"heartbeatIntervalSeconds\":5,\"offeringRefreshSeconds\":0}}}}}}'"
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl wait --for=condition=Ready bidder/bidder --timeout=120s"
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl get marketbidder app -o jsonpath='{.status.componentOverrides}'"
```

The override entry must show `"applied":true` and the child generation. Sample
the heartbeat twice, 10 s apart; `createdAt` advances about every 5 s. The
offering record `cid` must not change for 10 minutes while
`offeringRefreshSeconds=0`.

A direct child edit is reverted and reported:

```bash
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl patch bidder bidder --type merge -p '{\"spec\":{\"suspend\":true}}'"
sleep 5
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl get bidder bidder -o jsonpath='{.spec.suspend}{\"\n\"}'"
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl get marketbidder app -o jsonpath='{.status.conditions}' | jq -c '.[]|select(.type==\"ComponentDrift\")'"
```

The child `suspend` is empty/false and `ComponentDrift=True` names
`Bidder/bidder`. A typo key fails closed:

```bash
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":{\"Bldier\":{\"suspend\":true}}}}'"
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl get marketbidder app -o jsonpath='{.status.phase}{\"\n\"}{.status.conditions}' | jq -c '.[]|select(.type==\"ComponentOverridesValid\")'"
```

The phase is `Failed` and the condition is `False/InvalidKey`; no child
generation changed. Remove the typo and the test settings when done:

```bash
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl patch marketbidder app --type merge -p '{\"spec\":{\"components\":null}}'"
```
