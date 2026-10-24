# Acceptance 0001: account modes, per-component pause, and one VM per host

This is the runnable acceptance used for plans 0023 and 0024. It exercises:

- a localRepo bidder;
- an oauthQr bidder that receives the market over the prod PDS firehose;
- a localRepo gateway requester;
- per-component suspend without stopping the GuestControlPlane or the shared IngressProxy;
- typed Bidder settings and fail-closed overrides;
- a tenant k3s cluster that scales from zero to three market VMs and back to zero,
  placing one VM on each bidder host;
- ssh over WebRTC from the CLI requester to a guest on each bidder host (section 9, plan 0034
  ACC34-CLI-SSH).

Run it from a host with `goat`, `kubectl`, `ssh` to `socialweb.computer`, `hat-7`
and `hat-9`, and the credential files named below. It changes the prod gateway
and the bidders. Do not run it without that authority.

This runbook was validated end to end on `v0.0.0-rc81`: three pods on three
distinct tenant nodes, one market VM on each bidder host, paused winners forcing
the next placement, and every VM released when the cluster returned to zero.
Plan 0026 (abstract kinds, the bus with default-deny ComponentLinks) changed
the object names and fields it reads; the commands below use the plan 0026
names. Every kind is read by its full resource name (for example
`marketbidders.socialweb.computer`), because a market kcp binds several
exports that share short names.

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

The requester MarketIdentity lives in `root:app`; the gateway's components
(`Pds/pds`, `Requester/requester`, `GatewayAPI/gateway-api`) live in `root`.
The identity controller associates the requester from the operator's verified
claim record: `scripts/associate-bidder.ts` writes it from the Alice account and
the requester's association watch reads it from the bsky JetStream. The identity
controller reports the result in `status.associations` and the `Associated`
condition. `SOCIALWEB_GATEWAY_APPROVED_ASSOCIATIONS` (comma-separated DIDs in the
gateway env) pre-approves operators in `spec.approvedAssociations`; it is not
needed when the claim is verified. Do not patch the MarketIdentity spec: the
Gateway root owns it and server-side apply reverts the edit.

```bash
ID_ARGS="--kubeconfig $PROD_K --server https://127.0.0.1:6443/clusters/root:app"
MI=marketidentities.socialweb.computer
ASSOC=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl $ID_ARGS get $MI requester -o jsonpath='{.status.conditions[?(@.type==\"Associated\")].status}'")
RI_DID=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl $ID_ARGS get $MI requester -o jsonpath='{.status.did}'")
printf 'requester=%s associated=%s\n' "$RI_DID" "$ASSOC"
if [ "$ASSOC" != True ]; then
  OP_DID=$(deno run -A --no-lock scripts/associate-bidder.ts --creds ~/.aliceoa_bsky_social_source \
    --account ALICE --requester-did "$RI_DID" | jq -r .operatorDid)
  printf 'operator=%s\n' "$OP_DID"
fi
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl $ID_ARGS get $MI requester -o jsonpath='{.spec.approvedAssociations}{\"\n\"}{.status.associations}{\"\n\"}{.status.phase}{\"\n\"}'"
```

Wait until the identity phase is `Ready`, `status.associations` names the
operator DID and the `Requester` child reports `spec.operatorDID` with it.
Without that, every bid is denied by `requester-tangled-vouch` even though
bidder policy runs succeed.

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

Pause a bidder with the root's `spec.suspend`. The Bidder process keeps
running with `spec.suspendParticipation=true`: it withdraws its offering,
declines new RFPs, still fulfils an accept of a bid it published before the
pause, and still reads the market inbox, so it still releases the VMs it runs
(acceptance row A2). The root reports `Suspended`; the
GuestControlPlane, Pds, IngressProxy, Services and Exposures stay Ready.

```bash
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"suspend\":true}}'"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl patch marketbidders.socialweb.computer app --type merge -p '{"spec":{"suspend":true}}'
```

Check both roots and the Bidder spec:

```bash
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.phase}{\"\n\"}{.status.componentSummary}{\"\n\"}'"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.phase}{"\n"}{.status.componentSummary}{"\n"}'
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl get bidders.socialweb.computer bidder -o jsonpath='{.spec.suspendParticipation} {.status.phase}{"\n"}'
```

Both roots must be `Suspended`; the Bidder reports `true Ready`.

A component override (`{"spec":{"components":{"Bidder":{"suspend":true}}}}`)
pauses only the Bidder's participation: it sets the Bidder's
`spec.suspendParticipation=true`, not `spec.suspend` (suspend stops
participation, never obligations). The Bidder process keeps running: it
withdraws its offering, stops its heartbeat, declines new RFPs and accepts, and
still releases the VMs it runs on `vm.delete`, over XRPC or the firehose. The
root stays `Ready` with `componentSummary`
`{"desired":4,"notReady":0,"ready":3,"suspended":1}` on a localRepo bidder
(Bidder, GuestControlPlane, Pds, PolicyEngine) and the Bidder reports
`true Ready`. A Requester override works the same way. Other component kinds
keep `spec.suspend`, which stops the component. Section 7 uses it.

## 2. Resume hat-7 and point it at the gateway PDS firehose

Read the gateway PDS ingress URL from its Exposure. Then apply it on hat-7 as a
RelayEventSource and name it in the root's `spec.comms.eventSourceRefs`. The oauthQr Bidder advertises no inbound market endpoint; it
receives `market.rfp` and `market.accept` from this stream.

```bash
PDS_URL=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K get exposures.exposure.socialweb.computer svc-pds-pds-ingress -o jsonpath='{.status.address}'")
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl apply -f -" <<YAML
apiVersion: events.socialweb.computer/v1alpha1
kind: RelayEventSource
metadata: {name: gateway-pds}
spec: {url: "$PDS_URL"}
---
apiVersion: events.socialweb.computer/v1alpha1
kind: EventSource
metadata: {name: gateway-pds}
spec: {backendRef: {group: events.socialweb.computer, kind: RelayEventSource, name: gateway-pds}}
YAML
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"suspend\":false,\"comms\":{\"eventSourceRefs\":[{\"kind\":\"EventSource\",\"name\":\"gateway-pds\"}]}}}'"
```

Both objects and `spec.comms.eventSourceRefs` survive a bidder restart: the run
command applies the root without that field, so its server-side apply keeps the
operator's value (D03).

The root must become Ready and one EventStream must be running:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.phase}{\"\n\"}{.status.message}{\"\n\"}'"
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get eventstreams.events.socialweb.computer,eventsources.events.socialweb.computer"
```

If the MarketIdentity is `AwaitingScan`, run the headless sign-in with Bob's
credential file. Read the current QR url immediately before the command; it
expires after 5 minutes.

```bash
U=$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketidentities.socialweb.computer bidder -o jsonpath='{.status.pendingAction.url}'")
deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.bobvmbuilder_bsky_social_source \
  --account BOB --url "$U" --done "You can close this window"
```

A timeout waiting for the final page text is harmless when the identity is
`Ready` afterwards. The stored scope must be the authoritative bidder set:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketidentities.socialweb.computer bidder -o jsonpath='{.status.phase}{\"\n\"}'"
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
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims.socialweb.computer -o wide"
```

`get nodeclaims -o wide` prints only the name and age; the winner is
`status.contract.winner.did`, the phase `status.phase` (`Bidding`,
`Provisioning`, `Joining`, `Running`, `Released`) and the node's workload
grant `status.grant`. Read the three bidder DIDs once:

```bash
D7=$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.did}'")
D9=$(ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.did}'")
DL=$(sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.did}')
printf 'hat-7=%s\nhat-9=%s\nlocal=%s\n' "$D7" "$D9" "$DL"
```

Only hat-7 is unpaused, so the first winner must be `$D7`:

```bash
WINNER=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims.socialweb.computer -o jsonpath='{.items[0].status.contract.winner.did}'")
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
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get computevms.socialweb.computer -o wide"
```

## 5. One VM per host: resume hat-9 and local

Resume the two paused bidders, then run the same deployment one VM at a time.
After each VM is Ready, pause the bidder that won it; the next scale-up must use
the next host.

```bash
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"suspend\":false}}'"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl patch marketbidders.socialweb.computer app --type merge -p '{"spec":{"suspend":false}}'
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl wait --for=condition=Ready bidders.socialweb.computer/bidder --timeout=120s"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl wait --for=condition=Ready bidders.socialweb.computer/bidder --timeout=120s

kubectl --kubeconfig "$KC" scale deployment scale-check --replicas=1
kubectl --kubeconfig "$KC" wait --for=condition=Ready pod -l app=scale-check --timeout=15m
WINNER=$(ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims.socialweb.computer -o jsonpath='{.items[0].status.contract.winner.did}'")
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
    ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"suspend\":true}}'"
    ;;
  hat-9)
    ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"suspend\":true}}'"
    ;;
  local)
    sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
      kubectl patch marketbidders.socialweb.computer app --type merge -p '{"spec":{"suspend":true}}'
    ;;
esac
```

Wait until that root reports `Suspended` before the next scale-up. The pause is
asynchronous: the root reports `Suspended` only once its Bidder runs the new
generation with `suspendParticipation`; scaling up earlier lets the paused
bidder still bid on the next RFP, and an accept of a bid it already published
is honoured.

```bash
case "$HOST" in
  hat-7) ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl wait --for=jsonpath='{.status.phase}'=Suspended marketbidders.socialweb.computer/app --timeout=300s" ;;
  hat-9) ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl wait --for=jsonpath='{.status.phase}'=Suspended marketbidders.socialweb.computer/app --timeout=300s" ;;
  local) sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
           kubectl wait --for=jsonpath='{.status.phase}'=Suspended marketbidders.socialweb.computer/app --timeout=300s ;;
esac
```

Scale to two; wait, read the second winner, and pause it too:

```bash
kubectl --kubeconfig "$KC" scale deployment scale-check --replicas=2
kubectl --kubeconfig "$KC" wait --for=condition=Ready pod -l app=scale-check --timeout=15m
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims.socialweb.computer -o wide"
```

Repeat the host map/pause block for the new winner. Scale to three; the final
winner must be the one host still unpaused:

```bash
kubectl --kubeconfig "$KC" scale deployment scale-check --replicas=3
kubectl --kubeconfig "$KC" wait --for=condition=Ready pod -l app=scale-check --timeout=15m
kubectl --kubeconfig "$KC" get pods -o wide
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims.socialweb.computer -o wide"
```

Pass condition: three pods are Ready, the NodeClaims name three distinct
winners, and the three `NODE` names in `get pods -o wide` are distinct.

## 6. Resume paused winners, scale to zero, delete the cluster

A paused winner (root `spec.suspend`) still reads the market inbox and
releases its VM on the requester's `vm.delete` (A2). To prove A2, run the
scale to zero below before the resume block and check `computevms` on every
paused host; otherwise resume first. A winner paused with the component
override releases its VM the same way.

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"suspend\":false}}'"
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"suspend\":false}}'"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl patch marketbidders.socialweb.computer app --type merge -p '{"spec":{"suspend":false}}'
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl wait --for=condition=Ready bidders.socialweb.computer/bidder --timeout=120s"
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl wait --for=condition=Ready bidders.socialweb.computer/bidder --timeout=120s"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl wait --for=condition=Ready bidders.socialweb.computer/bidder --timeout=120s

kubectl --kubeconfig "$KC" delete deployment scale-check
for i in $(seq 1 120); do
  nodes=$(kubectl --kubeconfig "$KC" get nodes --no-headers 2>/dev/null | wc -l)
  [ "$nodes" = 0 ] && break
  sleep 5
done
[ "$nodes" = 0 ] || { echo "cluster still has $nodes nodes"; exit 1; }
ssh socialweb.computer "sudo -n -u socialweb env HOME=/var/lib/socialweb-computer/socialweb kubectl --kubeconfig $PROD_K --context root --server $NC get nodeclaims.socialweb.computer -o wide"
```

The claim list is empty (or every claim is `Released`). Check every bidder host:
no Running ComputeVM may remain, and no `socialweb-vm-*` unit may remain for
this cluster.

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get computevms.socialweb.computer -o wide"
ssh hat-9 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H9_K kubectl get computevms.socialweb.computer -o wide"
sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$LOCAL_K \
  kubectl get computevms.socialweb.computer -o wide
```

Delete the cluster and the scratch directory:

```bash
xrpc procedure deleteCluster name="$CL"
rm -rf "$W"
```

## 7. Per-component pause checks (A1-A4)

On the oauthQr bidder, define the heartbeat reader:

```bash
BOB=$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketidentities.socialweb.computer bidder -o jsonpath='{.status.did}'")
PDS=$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketidentities.socialweb.computer bidder -o jsonpath='{.status.pdsUrl}'")
hb() { curl -sS "$PDS/xrpc/com.atproto.repo.getRecord?repo=$BOB&collection=com.publicdomainrelay.temp.market.heartbeat&rkey=self" | jq -r .value.createdAt; }
```

Pause only Bidder and wait until the Bidder runs the paused generation
(`spec.suspendParticipation` is `true` and `status.observedGeneration` equals
`metadata.generation`); a heartbeat sent between the patch and that point is
the old generation's last one:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":{\"suspend\":true}}}}'"
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get bidders.socialweb.computer bidder -o jsonpath='{.spec.suspendParticipation} {.metadata.generation} {.status.observedGeneration}{\"\n\"}'"
```

Then record the heartbeat, wait 180 s, and compare:

```bash
hb
sleep 180
hb
```

`createdAt` must be identical. On hat-7 the root must be Ready with
`{"desired":3,"notReady":0,"ready":2,"suspended":1}`, the Bidder reports
`spec.suspendParticipation=true` and phase `Ready`; GuestControlPlane stays
Ready; the IngressProxy stays Ready; the GuestControlPlane ingress URL answers:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.phase}{\"\n\"}{.status.componentSummary}{\"\n\"}'"
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get guestcontrolplanes.socialweb.computer,ingressproxies.socialweb.computer"
curl -sS "https://$(ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl get exposures.exposure.socialweb.computer svc-guestcontrolplane-guest-kcp-ingress -o jsonpath='{.status.address}'" | sed 's#https://##')/healthz"
```

Resume, wait for Ready, and prove the heartbeat advances:

```bash
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":null}}}'"
ssh hat-7 "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$H7_K kubectl wait --for=condition=Ready bidders.socialweb.computer/bidder --timeout=120s"
hb
```

## 8. Typed settings, drift and fail-closed overrides

On any Ready bidder, set a 5 s heartbeat and a 15 s offering refresh:

```bash
K=$H7_K
HOST=hat-7
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"components\":{\"Bidder\":{\"suspend\":false,\"settings\":{\"bidder\":{\"heartbeatIntervalSeconds\":5,\"offeringRefreshSeconds\":15}}}}}}'"
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl wait --for=condition=Ready bidders.socialweb.computer/bidder --timeout=120s"
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.componentOverrides}'"
```

The override entry must show `"applied":true` and the child generation. Sample
the heartbeat twice, 10 s apart; `createdAt` advances about every 5 s. Read the
offering record `cid` twice, 60 s apart: it must not change. A refresh re-reads
the offering, corrects it only when it drifted and checks relay visibility; four
refreshes of a correct offering write nothing to the PDS.

A direct child edit is reverted and reported:

```bash
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl patch bidders.socialweb.computer bidder --type merge -p '{\"spec\":{\"suspend\":true}}'"
sleep 5
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl get bidders.socialweb.computer bidder -o jsonpath='{.spec.suspend}{\"\n\"}'"
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.conditions}' | jq -c '.[]|select(.type==\"ComponentDrift\")'"
```

The child `suspend` is empty/false and `ComponentDrift=True` names
`Bidder/bidder`. A typo key fails closed:

```bash
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"components\":{\"Bldier\":{\"suspend\":true}}}}'"
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl get marketbidders.socialweb.computer app -o jsonpath='{.status.phase}{\"\n\"}{.status.conditions}' | jq -c '.[]|select(.type==\"ComponentOverridesValid\")'"
```

The phase is `Failed` and the condition is `False/InvalidKey`; no child
generation changed. Remove the typo and the test settings when done:

```bash
ssh "$HOST" "sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG=$K kubectl patch marketbidders.socialweb.computer app --type merge -p '{\"spec\":{\"components\":null}}'"
```

## 9. ACC34: ssh over WebRTC from the CLI on each bidder

Plan 0034 ACC34-CLI-SSH. Each bidder runs a release with plan 0034: its
`socialweb-guest-downloads` asset serves `socialweb-guest-agent` to its guests
at `http://192.0.2.1:<port>/v1/downloads`, and its guest event queue and
`compute.guest.subscribeEvents` are up. The requester is the CLI of the same
release, on this host, against the prod market. The guest reaches
`stun:stun.cloudflare.com:3478` and nothing else is used for ICE.

### hat-7 signs in again after a scope change

An oauthQr session keeps the scope it was granted at sign-in. Plan 0034 added
`com.publicdomainrelay.temp.compute.events.guest.publicKey` and
`com.publicdomainrelay.temp.compute.events.guest.signalWebRTC` to the bidder
and requester role scopes (`internal/atproto/oauth/scope.go`). hat-7's identity
stays `Ready` on its old session after the upgrade, and nothing asks for a new
QR on its own; its PDS then refuses the bidder's writes of those records, so
the guest's publicKey and answer never reach the requester and ACC34 times out
waiting for the guest's publicKey on hat-7. After any release that changes a
role scope, before this section:

1. Check that the qr.fedfork.com client metadata scope covers every bidder and
   requester scope token of the release.
2. End hat-7's old session so the identity reports `AwaitingScan` or
   `SessionExpired` with a `pendingAction.url`.
3. Sign bob in again with the commands of section 2 (read the current QR url
   right before `scripts/oauth-signin.ts`; it expires after 5 minutes), and
   wait for `Ready`.

hat-9 and local are localRepo bidders; their records go to their own PDS and
need no sign-in.

### Per bidder

Run the block once per host, with the other two bidders paused as in section 1
so that only `$HOST` bids. Wait until both paused roots report `Suspended`
before the request.

```bash
SWC=socialweb-computer
R=~/.tmp/acc34
mkdir -p "$R"
chmod 700 "$R"
HOST=hat-7
NAME=acc34-$HOST-$(date +%s)
$SWC run request --state-dir "$R" --name "$NAME" --hold \
  --tunnel ssh:22:webrtc --transport webrtc --exec "uname -a; id" 2>&1 | tee "$R/$NAME-request.log"
$SWC run request ssh "$NAME" --state-dir "$R" --transport webrtc \
  --exec "uname -a; id" 2>&1 | tee "$R/$NAME-ssh.log"
```

Pass conditions for each host:

- `run request` reaches `Held` and the command's output shows `Linux` and
  `uid=`; ssh ran with `StrictHostKeyChecking=yes` against the host keys the
  guest reported (a host key error fails the run).
- The second `run request ssh` (the invocation's kcp started again from its
  state dir) also prints `uid=`.
- Record the Held to shell time: the `seconds` of the `ssh returned` line with
  `"transport":"webrtc"`. On the local bidder in a market network it was 2.16 s
  on a fresh invocation and about 7 s after the invocation's kcp restarted
  (2026-10-24).
- The winner is `$HOST`: the `winnerDid` of `run request list --state-dir "$R"
  --json` for `$NAME` is `$D7`, `$D9` or `$DL` of section 4 for that host.
- No log line holds signaling plaintext or a private key:

```bash
command grep -c -E 'a=candidate|AGE-SECRET-KEY' "$R/$NAME-request.log" "$R/$NAME-ssh.log"
```

  Both counts are 0.

Release, then check the host holds no ComputeVM for it:

```bash
$SWC run request release "$NAME" --state-dir "$R"
```

`run request release` ends `Released`. Read `computevms.socialweb.computer -o
wide` on `$HOST` as in section 6; no ComputeVM of this contract is left. Resume
the two paused bidders as in section 6 and repeat for the next host. ACC34
passes when all three hosts (local, hat-9, hat-7) pass.

When the requester waits for the guest's publicKey or answer and times out, read
the winning bidder's core journal for "the requester refused the market event"
and "submitting the market event to the requester failed" before retrying, and
on hat-7 recheck the scope note above.
