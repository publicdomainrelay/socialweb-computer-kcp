# socialweb-computer-kcp

socialweb-computer rents compute on the atproto compute market and describes it as code. You write an `Infra` or
`EphemeralInfra` object (a list of contracts: firecracker VMs, Deno sandboxes, VMs with an atirc agent) and
`kubectl apply` it to a requester's kcp. The requester posts the RFP, bidders bid, the requester accepts, and the
winner boots the guest from cloud-init `user_data`. Tenant Kubernetes clusters work the same way: a pod that does not
fit makes a NodeClaim, and the NodeClaim rents a VM that joins as a k3s node. The default policy on both sides is
tangled-vouch: a requester and a bidder deal only when each is in the other's `sh.tangled.graph.vouch` graph.

One binary, `socialweb-computer`, runs every part. Each application is a kcp of its own, and its state is kcp objects
(`Infra`, `EphemeralInfra`, `ComputeContract`, `ComputeVM`, `NodeClaim`, `HostProcess`).

- Example objects: [`examples/infra/`](examples/infra/)
- Working rules for agents: [`AGENTS.md`](AGENTS.md)

## Quick start (local)

Your own market network on one Linux host: one kcp with its own PLC, relay, PDSes and a bidder. Nothing here touches
prod. Run as a normal user that owns a subordinate uid range and can open `/dev/kvm` (`dev up` refuses root), with
`kubectl`, `jq` and `ssh` on PATH. Long form: [`docs/quickstart-local.md`](docs/quickstart-local.md).

Clone, build, start the network:

```bash
git clone https://github.com/publicdomainrelay/socialweb-computer-kcp
cd socialweb-computer-kcp
scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc     # or: make dev
SWC=/var/tmp/swc/bin/socialweb-computer
I=qs ST=/var/tmp/qs-state; mkdir -p "$ST"                           # short path: socket paths must fit in 107 bytes
$SWC dev up --instance $I --profile market-network --host-backend proc --bidders 1 --compute-kinds vm,deno --state-dir "$ST"
$SWC dev status --instance $I --state-dir "$ST" | jq -r '.network.status.phase'   # Ready
```

- `--compute-kinds vm,deno` lets the network bidder serve Deno sandboxes; without it the bidder serves `vm` only
- the network bidders run the default policy `bidder-tangled-vouch`, and the network writes the two vouch records each
  of its requesters needs; `dev up` fetches the pinned guest image, or takes `--guest-image-source file://<dir>`

Start a long-lived requester in the network, in its own terminal (or in the background as here). It writes a ready
file and a kubeconfig of its workspace; `--with-gateway` also lets it serve its own tenant clusters:

```bash
$SWC run requester --name qs --network $I --state-dir "$ST" --with-gateway > "$ST/qs.log" 2>&1 &
until [ -s "$ST/requesters/qs/ready.json" ]; do sleep 1; done
eval "$($SWC run requester env qs --state-dir "$ST")"              # KUBECONFIG of the requester workspace
kubectl get marketrequesters.socialweb.computer app -o jsonpath='{.status.phase} {.status.did}{"\n"}'
PUB=$(cat "$(jq -r .sshPublicKey "$ST/requesters/qs/ready.json")")  # the requester's own ssh key
```

A VM ([`vm.yaml`](examples/infra/vm.yaml), an `Infra`), ssh over iroh and over webrtc:

```bash
sed "s|ssh-ed25519 AAAAREPLACE-WITH-YOUR-PUBLIC-KEY you@laptop|$PUB|" examples/infra/vm.yaml | kubectl apply -f -
kubectl wait infras.socialweb.computer/quickstart-vm --for=jsonpath='{.status.phase}'=Deployed --timeout=3m
$SWC run requester ssh qs --state-dir "$ST" --contract infra-quickstart-vm-vm1 --exec 'cat /root/quickstart'
$SWC run requester ssh qs --state-dir "$ST" --contract infra-quickstart-vm-vm1 --transport webrtc --exec 'uname -m'
```

A Deno sandbox ([`deno-sandbox.yaml`](examples/infra/deno-sandbox.yaml), an `EphemeralInfra` that ends when its
contract is released):

```bash
kubectl apply -f examples/infra/deno-sandbox.yaml
kubectl wait ephemeralinfras.socialweb.computer/quickstart-deno --for=jsonpath='{.status.phase}'=Deployed --timeout=2m
kubectl get computecontracts.socialweb.computer einfra-quickstart-deno-sbx1 -o jsonpath='{.status.contract.sandbox.stdout}'
```

A tenant Kubernetes cluster of this requester; its node is a VM rented by the same requester under the same DID:

```bash
$SWC run requester cluster qs --cluster c1 --state-dir "$ST"
until TK=$($SWC run requester kubeconfig qs --cluster c1 --state-dir "$ST" 2>/dev/null); do sleep 2; done   # once the Cluster is Ready
kubectl --kubeconfig "$TK" apply -f examples/infra/k8s-cluster.yaml
kubectl --kubeconfig "$TK" wait --for=condition=Ready pod -l app=quickstart-k8s --timeout=8m
kubectl --kubeconfig "$TK" logs deploy/quickstart-k8s                # quickstart-k8s-ok
kubectl get infras.socialweb.computer                                # nodes-<hex>: the node's contract
```

An atirc agent VM ([`agent-vm.yaml`](examples/infra/agent-vm.yaml)) is refused locally before its RFP: the network
requester's handle is `.local`, and irc.fedfork.com mints only for a handle that resolves to the DID. The agent runs
against prod (below). All three kinds in one object: [`mixed.yaml`](examples/infra/mixed.yaml).

```bash
kubectl apply -f examples/infra/agent-vm.yaml
kubectl get computecontracts.socialweb.computer einfra-quickstart-agent-agent1 -o jsonpath='{.status.phase} {.status.message}{"\n"}'
```

Release and clean up:

```bash
kubectl --kubeconfig "$TK" delete -f examples/infra/k8s-cluster.yaml   # the node scales to zero, its contract Released
kubectl delete infras.socialweb.computer quickstart-vm                  # releases the contract first
kubectl delete ephemeralinfras.socialweb.computer --all
kill -INT "$(jq -r .pid "$ST/requesters/qs/ready.json")"                # Ctrl-C: stops the requester, keeps its state
$SWC dev down --instance $I --state-dir "$ST"                           # releases every VM, removes the state
```

## Prod: a long-lived requester

The same objects against prod bidders, with your own atproto account. Prod means prod bidders (each runs
`bidder-tangled-vouch`), plc.directory, the did:key ingress relay xrpc.fedproxy.com and irc.fedfork.com for atirc.
Never set `policy: open`. Long form: [`docs/quickstart-prod.md`](docs/quickstart-prod.md); end to end runbook:
[`docs/acceptance/0013-mixed-prod.md`](docs/acceptance/0013-mixed-prod.md).

### Run or install the requester

In the foreground, signed in by OAuth QR:

```bash
SWC=socialweb-computer ST=~/.tmp/lr; mkdir -p "$ST"; chmod 700 "$ST"
$SWC run requester --name me --state-dir "$ST" -- --atproto-oauth-qr > "$ST/me.log" 2>&1 &
# sign in from the pendingAction url in the log (scan the QR, or scripts/oauth-signin.ts); never paste it anywhere
until [ -s "$ST/requesters/me/ready.json" ]; do sleep 1; done
eval "$($SWC run requester env me --state-dir "$ST")"
```

- identity options after `--`: `--atproto-oauth-qr`, `--atproto-oauth` (browser), or an existing account with an app
  password (`--atproto-identity-mode pdsAccount --atproto-handle <handle> --atproto-pds-url <pds>`); with none it is a
  local PDS (`localRepo`). Contract options are refused: contracts are kubectl objects here
- a restart with the same command (nothing after `--` needed) keeps the DID, the vault and every Held contract (the
  resume waits for its own policy engine, then runs the policy; since rc158)

As a systemd user unit of its own account, in `socialweb.slice` (root, once). The account `swc-requester` is created,
or an existing one is taken over with `--adopt-account`:

```bash
sudo socialweb-computer install requester --name me --dry-run      # prints every step, writes nothing
sudo socialweb-computer install requester --name me                # unit socialweb-computer-requester@me, identity --atproto-oauth-qr
sudo socialweb-computer run requester signin me \
  --state-dir /var/lib/socialweb-computer/swc-requester            # the sign-in URL (prints nothing once Ready)
```

- the ready file is `/var/lib/socialweb-computer/swc-requester/requesters/me/ready.json`; an app password goes in once
  through `run requester password me` on stdin (into the requester's own OpenBao), never in a flag, `config.json` or
  the unit; `upgrade`, `rollback` and
  `uninstall requester` as for a bidder
- an account that already runs a core needs no root and no new account: apply
  [`requester-hostprocess.yaml`](examples/infra/requester-hostprocess.yaml) into that core's kcp, and the core places
  `run requester` as one more unit ([`docs/quickstart-prod.md`](docs/quickstart-prod.md) section 7b)

### Vouches

Both policies read `sh.tangled.graph.vouch` records (rkey = the subject DID). On prod they live in people's own repos:

| record | written by | how |
| --- | --- | --- |
| bidder operator -> you | the bidder operator | `goat record create` from the operator account, or [`vouch-bidder-operator.yaml`](examples/infra/vouch-bidder-operator.yaml) in the bidder kcp of a bidder installed with `--bidder-operator-signin` |
| you -> bidder (or its operator) | you | [`vouch.yaml`](examples/infra/vouch.yaml) applied to your requester, or `goat record create` |

Verdicts are cached for 5 minutes, denials too: write both records before the first apply. `vouch.yaml` writes through
your OAuth session only when it was granted the optional scope `repo:sh.tangled.graph.vouch`; otherwise use `goat`.

### Apply Infra and EphemeralInfra

```bash
R=$ST/requesters/me/ready.json
sed -e "s/REPLACE-REQUESTER-REPO/repo-$(jq -r .did "$R" | tr : -)/" -e "s/REPLACE-BIDDER-DID/<bidder did>/g" \
  examples/infra/vouch.yaml | kubectl apply -f -
sed -e "s|ssh-ed25519 AAAAREPLACE-WITH-YOUR-PUBLIC-KEY you@laptop|$(cat "$(jq -r .sshPublicKey "$R")")|" \
  -e "s/REPLACE-BIDDER-DID/<bidder did>/" examples/infra/mixed-prod.yaml | kubectl apply -f -
kubectl wait ephemeralinfras.socialweb.computer/mixedprod --for=jsonpath='{.status.phase}'=Deployed --timeout=5m
$SWC run requester ssh me --state-dir "$ST" --contract einfra-mixedprod-vm1 --exec 'cat /root/mixedprod'
kubectl get computecontracts.socialweb.computer einfra-mixedprod-sbx1 -o jsonpath='{.status.contract.sandbox.stdout}'
```

- [`mixed-prod.yaml`](examples/infra/mixed-prod.yaml) pins a VM, a Deno sandbox and an agent VM to one bidder with
  `bidderDids`; [`vm.yaml`](examples/infra/vm.yaml) and [`deno-sandbox.yaml`](examples/infra/deno-sandbox.yaml) work
  the same way
- a Deno sandbox needs a bidder that serves deno: its operator runs
  `install bidder --compute-kinds vm,deno` (or `run bidder --compute-kinds vm,deno`)
- release: `kubectl delete` the object, or set a contract's `state: Released`

### Agents over atirc

A contract with `spec.agent` ([`agent-vm.yaml`](examples/infra/agent-vm.yaml), `agent1` of `mixed-prod.yaml`) gets a
leased atirc token after the receipt; the guest has `atirc-agent.service` installed and stopped, and a
[`agent-start.yaml`](examples/infra/agent-start.yaml) ContractEvent (`guest.agentStart`) starts it. The agent joins
atirc as `<label>-<rfp rkey>.<your handle>`; release revokes the token before the VM is deleted.

```bash
kubectl apply -f examples/infra/agent-start.yaml                 # for agent-vm.yaml; for agent1 of mixed-prod.yaml:
sed -e 's/quickstart-agent-agent1-start/mixedprod-agent1-start/' -e 's/einfra-quickstart-agent-agent1/einfra-mixedprod-agent1/' \
  examples/infra/agent-start.yaml | kubectl apply -f -
```

The mint needs a requester identity irc.fedfork.com resolves: an OAuth sign-in (signs with the session), an app
password account (`pdsAccount`, signs through that account's PDS), or a local PDS on plc.directory behind its ingress
with `--atproto-handle <a domain you control>` (signs with its own Pds). A `.local` handle is refused before the RFP.
Details: [`docs/quickstart-prod.md`](docs/quickstart-prod.md) section 5, runbook
[`docs/acceptance/0004-vm-atirc-agent.md`](docs/acceptance/0004-vm-atirc-agent.md).

### Tenant clusters

```bash
$SWC run requester --name mk --state-dir "$ST" --with-gateway -- --atproto-oauth-qr > "$ST/mk.log" 2>&1 &
until jq -e .gateway "$ST/requesters/mk/ready.json" >/dev/null 2>&1; do sleep 1; done
$SWC run requester cluster mk --cluster c1 --state-dir "$ST"
until TK=$($SWC run requester kubeconfig mk --cluster c1 --state-dir "$ST" 2>/dev/null); do sleep 2; done
kubectl --kubeconfig "$TK" apply -f examples/infra/k8s-cluster.yaml
```

`--with-gateway` composes the gateway into the requester's own kcp: one identity, no second sign-in, nodes rented
under your DID. `requester.json` records it at the first start of a name, so a restart keeps the gateway and the
clusters; give it at that first start ([`docs/quickstart-prod.md`](docs/quickstart-prod.md) section 8). The hosted
gateway `https://k8s.socialweb.computer` serves tenant clusters too, over XRPC with service auth from your account
([`docs/quickstart-prod.md`](docs/quickstart-prod.md) section 6, [`docs/gateway.md`](docs/gateway.md)).

### Run a bidder

```bash
sudo socialweb-computer install bidder --handle <bidder handle> --compute-kinds vm,deno --bidder-operator-signin <operator handle>
```

The bidder runs `bidder-tangled-vouch`, gets a DID and waits for its association. Install from a release, identity,
association, upgrade and the foreground form (`run bidder --host-backend proc --fg`): [`docs/bidder.md`](docs/bidder.md),
[`CONTRIBUTING.md`](CONTRIBUTING.md#install-a-release-bidder).

### Open items

| item | state |
| --- | --- |
| an atirc agent from a local PDS or a `pdsAccount` identity | built, not yet proven against irc.fedfork.com |

## Docs

| doc | what |
| --- | --- |
| [`docs/FEATURES.md`](docs/FEATURES.md) | what is supported today and how far each item is proven |
| [`docs/quickstart-local.md`](docs/quickstart-local.md), [`docs/quickstart-prod.md`](docs/quickstart-prod.md) | the two quick starts in full |
| [`docs/acceptance/`](docs/acceptance/) | runbooks: [0012](docs/acceptance/0012-infra-quickstart.md) the infra quick start, [0013](docs/acceptance/0013-mixed-prod.md) mixed prod, auth modes, ssh gateway, atirc agent, ... |
| [`docs/protocol/README.md`](docs/protocol/README.md) | the atproto lexicons and the end-to-end sequence |
| [`docs/SYSTEM.md`](docs/SYSTEM.md) | the whole system: repos, hosts, apps, market, guests, identity, tenant clusters |
| [`docs/KCP.md`](docs/KCP.md) | how every application uses kcp: profiles, workspaces, exports, the API catalog |
| [`docs/bidder.md`](docs/bidder.md) | bidder install, identity, association, upgrade |
| [`docs/README.md`](docs/README.md) | every other doc (deploy, gateway, OAuth, host contract, inspect) |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | decisions, measurements, invariants |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | build, release, install, upgrade, PDS ingress, troubleshooting |
| [`docs/plans/`](docs/plans/), [`NEXT.yaml`](NEXT.yaml) | plans and the live backlog |
| [`deploy/market-network/README.md`](deploy/market-network/README.md) | the one-kcp market network |

## Layout

| directory | what it holds |
| --- | --- |
| `cmd/` | one directory per binary (`socialweb-computer`, `socialweb-node`, `socialweb-vmexec`, `socialweb-guest-agent`, ...) |
| `internal/` | the Go packages |
| `pkg/` | packages another module imports (`pkg/embedded`) |
| `api/` | the kcp API types |
| `deploy/` | manifests, sets, generated schemas and exports |
| `examples/infra/` | Infra, EphemeralInfra and the other objects of the quick starts |
| `docs/` | the docs above |
| `rust/` | socialweb-meshd |
| `scripts/` | build, deploy and one-off operator scripts |

## License

Unlicense (public domain).
