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

## Run a bidder

A bidder answers RFPs on the market and boots each guest it wins as a firecracker VM on its host. Two
ways to run one from the same binary: installed under systemd, or in the foreground as your own user.

### Install from a release (systemd)

From the public bucket, with no credentials (the release rollouts run this path):

```bash
HANDLE=<your atproto handle>
BUCKET=https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com
V=$(curl -fsSL https://api.github.com/repos/publicdomainrelay/socialweb-computer-kcp/tags \
  | jq -r '.[].name' | grep '^v' | sort -V | tail -1)
ARCH=linux-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
mkdir -p /var/tmp/swc-release && cd /var/tmp/swc-release
curl -fLO "$BUCKET/releases/$V/binaries/socialweb-computer-$ARCH"
curl -fLO "$BUCKET/releases/$V/binaries/SHA256SUMS"
grep " socialweb-computer-$ARCH$" SHA256SUMS | sha256sum -c -
sudo install -m 0755 "socialweb-computer-$ARCH" /usr/local/bin/socialweb-computer
sudo /usr/local/bin/socialweb-computer install bidder --handle "$HANDLE"
```

`install bidder` creates:

| Item | Where |
| --- | --- |
| Account | `swc-bidder`, in group `kvm`, linger on, one subuid/subgid range (`--vm-slots N`, default 256, 65536 ids each) |
| Unit | `socialweb-computer-bidder@<handle>.service` in `swc-bidder`'s user manager; restarts on exit (`Restart=always`) and starts at boot |
| State | `/var/lib/socialweb-computer/swc-bidder` (`config.json`, `bin/current`, `bin/previous`) |
| kcp admin kubeconfig | `/var/lib/socialweb-computer/swc-bidder/handles/<hash>/kcp/admin.kubeconfig` (root only) |

The bidder gets a new DID and waits for its association:
[CONTRIBUTING: bidder association](CONTRIBUTING.md#bidder-association). Check it:

```bash
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  systemctl --user status "socialweb-computer-bidder@$HANDLE.service"
sudo runuser -u swc-bidder -- env XDG_RUNTIME_DIR=/run/user/$(id -u swc-bidder) \
  /var/lib/socialweb-computer/swc-bidder/bin/current/socialweb-computer units list
```

`socialweb-computer status` reads instances that run without systemd (below), not the installed
unit. Upgrade, uninstall, oauthQr and more: [CONTRIBUTING](CONTRIBUTING.md#install-a-release-bidder).

### Run in the foreground (no systemd)

As a normal user, no root at run time:

```bash
socialweb-computer run bidder --handle <your atproto handle> --host-backend proc --fg
```

Every host process (kcp, kine, its front, OpenBao, the core) and every VM is a child of this
process, under its own supervisor (devsup). `--host-backend proc` is required (there is no default);
`systemd-user` runs only as an account `install bidder` made. The profile defaults to `prod-qr`, as
installed.

The account needs, once (root):

| Need | Fix |
| --- | --- |
| `/dev/kvm` read-write | `usermod -aG kvm <user>`, then log in again |
| `/dev/net/tun`, unprivileged user namespaces | `modprobe tun`; on Ubuntu, AppArmor must let the embedded pasta make a user namespace |
| One subuid and subgid range, 65536 ids per VM | `usermod --add-subuids 655360-1703935 --add-subgids 655360-1703935 <user>` (16 VMs; pick a free range) |
| `newuidmap`, `newgidmap` | package `uidmap` (Debian/Ubuntu) or `shadow-utils` (Fedora) |
| `XDG_RUNTIME_DIR` | a login session (ssh or a desktop), or `loginctl enable-linger <user>` |

Not needed: systemd units, linger, cgroup delegation, an open inbound TCP port (every listener binds
`127.0.0.1:0`; public endpoints go out through the did:key ingress relay `xrpc.fedproxy.com`). A guest
with a WebRTC tunnel gets one UDP port from `spec.guestNetwork.webrtcUDPPorts` (default 31000-31999)
forwarded on the host's LAN addresses so same-LAN peers reach it; allow that range inbound for UDP.
`run bidder` checks these first and names the missing one and its fix; `socialweb-computer check-host
-role bidder` lists every check (ignore its systemd rows for this mode).

Identity: the default makes a local PDS repo with a new did:plc on plc.directory, then waits in
`AwaitingApproval` for the association. `--fg` waits without a deadline and prints the DID. Associate
it ([CONTRIBUTING](CONTRIBUTING.md#bidder-association)), or skip association:

```bash
K=~/.local/state/socialweb-computer/dev/b-<handle-with-dashes>/kcp/admin.kubeconfig
kubectl --kubeconfig "$K" patch marketbidder app --type=merge -p '{"spec":{"identity":{"association":"Skip"}}}'
```

To sign in as an existing account instead, add `-- --atproto-oauth-qr`; it prints a QR sign-in URL
and waits for it.

| Item | Where |
| --- | --- |
| Instance | `b-<handle-with-dashes>` (`--instance` overrides) |
| State | `${XDG_STATE_HOME:-~/.local/state}/socialweb-computer/dev/b-<handle-with-dashes>`; `--state-dir DIR` puts it in `DIR/dev/b-<handle-with-dashes>` |
| Sockets | `$XDG_RUNTIME_DIR/swc/` |
| Extracted assets | `${XDG_CACHE_HOME:-~/.cache}/socialweb-computer/runtime` |

Check it from another shell: `socialweb-computer status --instance b-<handle-with-dashes>`.

Stop: Ctrl-C or SIGTERM. It stops the VMs (gracefully, then killed), OpenBao, the core, kcp and
kine, prints a JSON report whose `remaining` is empty, and keeps the state: the next run is the same
DID. If the process dies any other way, devsup sees it go and stops every unit. Without `--fg`,
`run bidder` returns once the bidder is Ready and leaves it running under a detached devsup; stop it
with `socialweb-computer dev down --instance b-<handle-with-dashes> --keep-state`. `dev down` without
`--keep-state` deletes the state and the DID with it.

Caveats against the installed bidder: no restart on a crash or at boot; the VMs end with the process;
no cgroup limits on the VMs (the systemd user manager enforces them for an install); no install
record, so no `upgrade` or `rollback`: the binary is the one you run. A dev market network join and
the details: [CONTRIBUTING: foreground bidder](CONTRIBUTING.md#foreground-bidder-no-systemd).

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
| Run a bidder in the foreground | `run bidder --handle <handle> --host-backend proc --fg` | [Run a bidder](#run-in-the-foreground-no-systemd) |
| Associate or skip association | `scripts/associate-bidder.ts` | [CONTRIBUTING: association](CONTRIBUTING.md#bidder-association) |
| Upgrade a bidder | `upgrade --binary <path> --sha256 <digest>`, then `install bidder` | [CONTRIBUTING: upgrade](CONTRIBUTING.md#upgrade-reinstall-uninstall) |
| Install or upgrade the gateway | `scripts/deploy.sh` | [CONTRIBUTING: gateway](CONTRIBUTING.md#deploy-the-gateway) |
| Build a dev binary | `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc` | [CONTRIBUTING: dev build](CONTRIBUTING.md#dev-build) |
| Build a release | `make` | [CONTRIBUTING: release build](CONTRIBUTING.md#release-build) |
| Select PDS ingress | `spec.ingress.provider` | [CONTRIBUTING: PDS ingress](CONTRIBUTING.md#pds-ingress-provider) |
| Inspect a unit or kcp | `units list`, `journalctl`, `kubectl` admin kubeconfig | [CONTRIBUTING: troubleshooting](CONTRIBUTING.md#troubleshooting) |

An installed bidder needs a 64-bit Linux host with `/dev/kvm`, systemd 253+, cgroup v2 and
`/dev/net/tun`; a foreground one needs no systemd ([Run a bidder](#run-a-bidder)).
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
