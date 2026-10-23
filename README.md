# socialweb-computer-kcp

socialweb-computer-kcp serves virtual Kubernetes clusters. A tenant cluster's k3s nodes are VMs rented
on the atproto compute market; nodes scale to zero while the control plane keeps serving. One atproto
account can own several clusters.

One binary, `socialweb-computer`, runs every part. An instance is one kcp plus its workspaces: a
requester (`run request`), a bidder, the gateway, a market network, or a local dev instance. State is
kcp objects such as `ComputeVM`, `ComputeContract`, `NodeClaim`, and `HostProcess`.

- Design and decisions: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- Working rules: [`AGENTS.md`](AGENTS.md)
- Plans and backlog: [`docs/plans/`](docs/plans/), [`NEXT.yaml`](NEXT.yaml)
- Build, install, upgrade, association, ingress, troubleshooting: [`CONTRIBUTING.md`](CONTRIBUTING.md)

## Use the hosted gateway

`https://k8s.socialweb.computer` is the production gateway. No account with us, no API key. It is
API-only: authenticate each XRPC call with a service-auth token from your own atproto account.

Create a cluster and download a kubeconfig over XRPC. `goat` is the Bluesky CLI.

```bash
go install github.com/bluesky-social/goat@latest
GOAT_USERNAME=<your handle> GOAT_PASSWORD=<your password> goat account login
GW=https://k8s.socialweb.computer AUD=did:web:k8s.socialweb.computer N=com.publicdomainrelay.temp.cluster
call() { local m=$1 nsid=$N.$2; shift 2; goat xrpc $m $GW $nsid "$@" "Authorization:Bearer $(goat account service-auth --aud $AUD --lxm $nsid)"; }
call procedure createCluster name=demo
call query listClusters | jq '.clusters[] | {name, phase, running}'
call procedure getKubeconfig name=demo ttlSeconds:=28800 | jq -r .kubeconfig > demo.kubeconfig
chmod 600 demo.kubeconfig
KUBECONFIG=demo.kubeconfig kubectl get ns
call procedure controlPlaneDown name=demo; call procedure deleteCluster name=demo
```

`goat xrpc` sends `key=value` as a JSON string and `key:=value` as a JSON number or boolean. The
kubeconfig holds an x509 client certificate from the cluster's OpenBao PKI (CN your DID, default 8 h,
at most 24 h via `ttlSeconds`). Keep it `0600`. `getKubeconfig` on a stopped cluster answers
`ClusterNotRunning`; call `createCluster` or `controlPlaneUp` first. `noExpiry:=true` never expires
until revoked or the cluster is deleted.

Cluster methods (prefix `com.publicdomainrelay.temp.cluster.`): `createCluster`, `getKubeconfig`,
`listClusters`, `controlPlaneUp`, `controlPlaneDown`, `deleteCluster`, `subscribeClusters`,
`revokeIdentity`.

### Watch cluster state

`subscribeClusters` is an atproto event stream: a websocket `GET`, binary frames, DRISL-CBOR header
then payload. Authenticate on upgrade with the same service JWT. `?name=<cluster>` watches one.

```bash
# variables carry over: GW, AUD, N
T=$(goat account service-auth --aud $AUD --lxm $N.subscribeClusters)
websocat -b -n -H="Authorization: Bearer $T" \
  "wss://k8s.socialweb.computer/xrpc/$N.subscribeClusters?name=demo" < /dev/null | xxd
```

On connect the stream sends a snapshot (one `#cluster` per cluster, one `#member` per control-plane
member, one `#nodeClaim` per claim, each with `snapshot:true`), then one `#info` named
`SnapshotComplete`, then live changes. There is no cursor: reconnecting starts a fresh snapshot.
Error frames: `InvalidRequest`, `Unauthorized`, `TokenExpired`, `ClusterNotFound`,
`IdentityRevoked`, `ClusterUnavailable`, `ConsumerTooSlow`.

### Apply objects and read Jobs without a kubeconfig

`computer.socialweb.k8s.kubectlApplyServerSide` applies one object like `kubectl apply --server-side`.
`computer.socialweb.k8s.getJob` reads a Job, its pods and log tails.

```bash
# variables carry over: GW, AUD, N; cluster demo from the block above
K=computer.socialweb.k8s
curl -sS -X POST "$GW/xrpc/$K.kubectlApplyServerSide" \
  -H "Authorization: Bearer $(goat account service-auth --aud $AUD --lxm $K.kubectlApplyServerSide)" \
  -H "Content-Type: application/json" -H "Accept: application/json" \
  -d '{
    "cluster": "demo",
    "namespace": "default",
    "name": "app-config",
    "fieldManager": "kubectl",
    "manifest": {
      "apiVersion": "v1", "kind": "ConfigMap",
      "metadata": {"name": "app-config", "namespace": "default"},
      "data": {"environment": "production", "log_level": "info"}
    }
  }' | jq '{kind, namespace, name, uid, resourceVersion}'
```

```bash
# variables carry over: GW, AUD, K; cluster demo
jq -n '{cluster: "demo", manifest: {apiVersion: "batch/v1", kind: "Job",
  metadata: {name: "hello", namespace: "default"},
  spec: {backoffLimit: 0, template: {spec: {restartPolicy: "Never",
    containers: [{name: "run", image: "docker.io/library/busybox:1.36.1", command: ["sh", "-c", "echo hello"]}]}}}}}' |
  curl -sS -X POST "$GW/xrpc/$K.kubectlApplyServerSide" -H "Content-Type: application/json" \
    -H "Authorization: Bearer $(goat account service-auth --aud $AUD --lxm $K.kubectlApplyServerSide)" -d @- | jq -c '{kind, name, uid}'
until s=$(curl -sS "$GW/xrpc/$K.getJob?cluster=demo&namespace=default&name=hello&tailLines=50" \
    -H "Authorization: Bearer $(goat account service-auth --aud $AUD --lxm $K.getJob)"); \
  echo "$s" | jq -c '{phase, active, succeeded, failed}'; echo "$s" | jq -e '.phase == "Succeeded" or .phase == "Failed"' >/dev/null; do sleep 5; done
echo "$s" | jq -r '.pods[].containers[] | "\(.state) exit=\(.exitCode) \(.log)"'
```

A cluster at zero nodes rents a market VM when you apply a Job or pod.

## Run your own

Install, build, upgrade, associate, uninstall, PDS ingress, and troubleshooting commands are in
[CONTRIBUTING.md](CONTRIBUTING.md). Short map:

| To | Command | Long form |
| --- | --- | --- |
| Install a bidder from a release | `sudo socialweb-computer install bidder --handle <handle>` | [CONTRIBUTING: release bidder](CONTRIBUTING.md#install-a-release-bidder) |
| Associate or skip association | `scripts/associate-bidder.ts` | [CONTRIBUTING: association](CONTRIBUTING.md#bidder-association) |
| Upgrade a bidder | `upgrade --binary <path> --sha256 <digest>`, then `install bidder` | [CONTRIBUTING: upgrade](CONTRIBUTING.md#upgrade-reinstall-uninstall) |
| Install or upgrade the gateway | `scripts/deploy.sh` | [CONTRIBUTING: gateway](CONTRIBUTING.md#deploy-the-gateway) |
| Build a dev binary | `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc` | [CONTRIBUTING: dev build](CONTRIBUTING.md#dev-build) |
| Build a release | `make` | [CONTRIBUTING: release build](CONTRIBUTING.md#release-build) |
| Select PDS ingress | `spec.ingress.provider` | [CONTRIBUTING: PDS ingress](CONTRIBUTING.md#pds-ingress-provider) |
| Inspect a unit or kcp | `units list`, `journalctl`, `kubectl` admin kubeconfig | [CONTRIBUTING: troubleshooting](CONTRIBUTING.md#troubleshooting) |

A bidder needs a 64-bit Linux host with `/dev/kvm`, systemd 253+, cgroup v2 and `/dev/net/tun`.
`install bidder` creates the `swc-bidder` account, its subuid/subgid range and its units. A release
binary needs only `curl` and `sha256sum`; it embeds every asset. The gateway host must already run
Caddy with `/etc/caddy/sites` imported.

## The RFP flow

A requester rents a VM by posting a `compute.vm` record and a signed `market.rfp`; bidders bid; the
requester accepts; the winner boots the guest from cloud-init `user_data`. The guest reports an iroh
ticket and host keys; the requester's ssh dials that ticket. Market records go through the relay.

```text
requester --market.rfp--> relay/PDS <--bid-- bidder (own kcp: MarketBidder, Bid, ComputeVM)
    | accept                                    | cloud-init user_data
    v                                            v
gateway (own kcp: Gateway, Cluster, NodeClaim)   firecracker VM = k3s node
    tenant k3s apiserver (kine + front) <-------- node joins over the iroh link
```

Every listener behind the edge binds port 0 and reports its address into kcp status. Only stable
entry points (Caddy on 443, an application's front) keep a fixed address.

## Inspect

```bash
STATE=/var/lib/socialweb-computer/socialweb
find $STATE /var/lib/socialweb-computer -path '*kcp/admin.kubeconfig' -not -path '*slot-*' -not -path '*shared*'
```

A production application has its own kcp. An admin kubeconfig's server URL ends in
`/clusters/<workspace>`. Full commands: [CONTRIBUTING: troubleshooting](CONTRIBUTING.md#troubleshooting).

## License

Unlicense (public domain).
