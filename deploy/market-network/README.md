# Market network

A market network is one `MarketNetwork` application: the shared atproto services
of a local market, in one kcp, plus child applications (bidders, requesters,
gateways) that find each other over the atproto market protocol. Bring one up
with `socialweb-computer dev up --profile market-network`. The operator or
developer who runs a market network and the applications it hosts reads this
file.

## What a market network is

A market network is one application, `MarketNetwork`, in one kcp.
`socialweb-computer dev up --profile market-network` brings up the instance (kine,
one kcp slot behind its front, the socialweb-computer core process) and applies
one root object, `MarketNetwork network`, into the workspace `root` of the
instance's kcp. The core process (the `socialweb-computer` process that runs the
controllers, the component runtime and the HostProcesses) runs the composition
controller of `internal/app/marketnetwork`. It composes the root into:

- an OpenBaoServer, `OpenBaoServer network`, that holds the network PKI;
- the network's own components, in `root`: `Plc plc`, `Relay relay`,
  `IngressProxy dispatcher`, `DispatcherTLS dispatcher-tls`, `Pds alice`,
  `Pds bob` and `PolicyEngine policy-engine`, with the Service and Exposure
  objects that publish them;
- one child application for each `spec.apps[]` entry: `bidder-1` .. `bidder-N`
  (`MarketBidder`) and, with `--with-gateway`, `gateway` (`Gateway`).

```
one market network (one dev instance, one kcp)
|
+- root            MarketNetwork network
|                  OpenBaoServer network
|                  Plc plc, Relay relay, IngressProxy dispatcher,
|                  DispatcherTLS dispatcher-tls, Pds alice, Pds bob,
|                  PolicyEngine policy-engine, their Services and Exposures
|
+- root:bidder-1 .. root:bidder-N   MarketBidder app   (components in root:<app>:app)
+- root:gateway                     Gateway            (dev up --with-gateway)
+- root:<name>                      MarketRequester app (run request --network)
+- root:<name>                      SshGateway app      (run gateway ssh --network)
|
+- root:dev:<instance>-<app>        the HostProcess of each child's core
```

A child application runs in its own workspace `root:<app>` of the network's kcp,
with its components in `root:<app>:app`. It has no kcp or kine of its own. Its
core process is a HostProcess in `root:dev:<instance>-<app>`, placed by the
network's devsup. That core runs the child's controllers and components for its
base workspace `root:<app>`. Its kubeconfig for `root:dev:<instance>-<app>`
(`parent.kubeconfig` in the child's state dir) is a token of the ServiceAccount
`app-core`, which may manage only the HostProcess objects of that workspace.

Once the child's core reports that it adopted its units, the network applies the
child's root object (`MarketBidder app`, `MarketRequester app` or
`SshGateway app`) into `root:<app>`, with the network value (`status.network`)
copied into `spec.network` and the bidders' DIDs into `spec.bidderDids`. The
network then reads back only the child's root status (phase, message, did,
components, listeners). A `Gateway` child applies its own root object from the
spec file the network writes for it (see [Add a cluster gateway](#add-a-cluster-gateway)).
The copied value is how a child learns about the network. The network also lets
the `pds` account of each child read the MarketIdentity objects of the other
children (the ClusterRole and ClusterRoleBinding `market-network-peers`).
Applications still talk to each other only over the atproto market protocol; the
shared kcp is composition and isolation, never a data channel.

The names `global`, `relay`, `alice`, `bob`, `dev` and `app` are reserved: no
child may take them as `apps[].name`.

`OpenBaoServer network` (`deploy/market-network/05-vault.yaml`) holds the network
PKI. It declares a `pki` mount, the `network-pki` policy, the client `components`
(holding only that policy) and the client `owner` (kv mount `owner`). Its storage
is under the instance's state (`openbaoservers/network`). The OpenBaoServer
controller initialises it, writes the unseal keys to a mode 0600 file of its state
and never to kcp, and revokes the initial root token, so no root token is kept.
The composer waits for the server to be Ready with the client `components`, then
reads the root CA of the `pki` mount through the bus (a `vault.pki.issue`
ComponentLink scoped to `pki`).

Every bidder has its own OpenBaoServer `bidder`, in its own workspace
(`deploy/apps/market-bidder/vault.yaml`), applied by the bidder's own core. That
core generates the account password into the vault, signs up for an account of
its own on bob's PDS (`MarketIdentity` mode `pdsAccount`, the PDS URL from
`spec.identity.pdsUrl` of its root object) and reaches its vault only through
the bus. No password or key is in a kcp object, and the network never touches a
child's vault.

Each network component listens on a port the composer leases from the network's
own port registry (`<state>/network/ports.json`, ports 30000 to 39999, each
probed free before it is leased), so the number survives a restart of the core.
Each local Exposure listens on a port of the core's port registry
(`<state>/ports/ports.json`) and records it in `status.address`. Peers find the
addresses by name in `status.network.hosts`.

### TLS and the certificate authority

The network's CA bundle has two parts. One is the root CA of the `pki` mount of
`OpenBaoServer network`. The other is the guest TLS CA that the composer makes
in `<state>/network/guest-tls/` (`ca.crt`, `ca.key`, and the leaf
`relay.localhost.crt` with its key, for `relay.localhost` and
`*.relay.localhost`, valid 30 days and made again within a day of expiry).
`kcp start` generates its own CA in memory and persists only the serving key, so
kcp cannot sign these leaves.

The composer publishes the bundle inline, as PEM, in `status.network.caBundle`.
Every child gets it in its copied network value. Where a process needs a file,
the child writes the bundle to a file in its own state dir.

`DispatcherTLS dispatcher-tls` is the guest TLS front: it serves the
`relay.localhost` leaf and forwards to `IngressProxy dispatcher`. The ingress
Exposures of the PDSes, plc and relay have TLS enabled with the network CA
bundle. Their local Exposures serve plain HTTP on loopback.

### How names resolve

There is no DNS for the network's names. Each name is an entry of
`status.network.hosts`, mapped to a loopback address:

- inside a core, a request to a name that a running component serves goes to
  that component's handler on the bus (`internal/bus`), without a socket;
- every other dial goes through `internal/kit/networkdial`. It builds a host map
  from `status.network.hosts` (plus the guest TLS name). A name, or any subdomain
  of it, maps to its address. An IP literal passes through. Any other name is
  refused, because system DNS is not allowed by default.

The dialer changes only the address it connects to. The request keeps the name
in its URL and its `Host` header, so a service that derives its identity from
`Host` (the PDS for its `did:web` and OAuth issuer) still sees the name it was
configured with.

## Bring up a market network

### Prerequisites

- The `socialweb-computer` binary. A dev build from
  `scripts/build-socialweb-computer.sh --dev --publish <dir>` embeds every pinned
  asset. `--with-gateway` needs the gateway's embedded k3s and external-secrets
  assets; a binary not built by that script refuses it.
- dumbpipe, for any guest access: `run request` and `run gateway ssh` reach
  guests only over iroh. A dev build needs `--dumbpipe-path` (or
  `SOCIALWEB_DUMBPIPE_PATH`) on `dev up` and `run request`; `run gateway ssh
  --network` uses the one the network was brought up with. A release build runs
  its embedded dumbpipe.
- The VM runtime, when the network runs bidders: `--guest-image-source` and
  `--vm-host-backend systemd-user`, plus `--userns-stage-path`, `--pasta-path`,
  `--vmexec-path` and `--firecracker-path`, or the matching `SOCIALWEB_*_PATH`
  environment variables.
- Run as the account that owns the subordinate id range. `dev up` refuses to run
  as root.

### Start the network

Run one command. `--bidders` sets the number of child bidders (default 1, 0 for
none). `--guest-image-source` and `--vm-host-backend` fill the bidders' compute
provider.

```bash
socialweb-computer dev up --instance mn1 --profile market-network --bidders 2 \
  --guest-image-source file:///var/tmp/guest-image --vm-host-backend systemd-user
```

`dev up` brings up the instance's bootstrap layer (kine, kcp behind its front,
the core process) under its own devsup and applies the `MarketNetwork` root
object. It waits up to `--network-timeout` (default 3m) for the network and every
child to be Ready, and prints the result.

`--app-values` names a YAML (or JSON) file merged over the built-in defaults. Its
keys are `bidder` and `gateway`: the NetworkApp fields every bidder or the
gateway gets. For example, the owner handle of the bidders' ComputeVMs:

```json
{"bidder": {"kcp": {"handle": "{app}.mn1.bob.test"}}}
```

`{app}` becomes the bidder's name; make the handle unique per worktree on a
shared account.

N market networks on one host are N dev instances. Every port is probed free
before it is leased, so two `dev up --instance` with different names never
collide.

### Check the network is Ready

```bash
socialweb-computer dev status --instance mn1
```

`dev status` prints the instance record (`.record`, with the state dir in
`.record.layout.stateDir`), devsup's units (`.devsup.units`) and the whole
`MarketNetwork` object (`.network`). The network is Ready when
`.network.status.phase` is `Ready`; see [Conditions](#conditions).

### Rent a guest

`run request --network` adds a `MarketRequester` entry to `spec.apps[]`. The
network places it in its workspace `root:<name>` of the network's kcp and
applies its root object, and the requester runs the RFP flow against the
network's bidders.

```bash
socialweb-computer run request --network mn1 --secrets secrets.json --exec 'cat /root/x'
```

The network copies the network value (with `hosts`) and the bidders' DIDs onto
the requester's root object. The CLI then talks only to the invocation's own
workspace. The guest comes up only through the RFP flow: the `Requester`
component posts the RFP, a bidder bids, the requester accepts, and the winner
boots the guest from cloud-init `user_data`. `--secrets` is written only into
the invocation's own OpenBao, and the guest fetches it from the requester's
secrets server. The CLI reaches the guest only over its iroh ticket, with
`StrictHostKeyChecking=yes` against the host keys the guest reported. Without
`--hold` the CLI releases the contract (vm.delete) and removes the entry from
`spec.apps[]`.

To hold the invocation and manage it later:

```bash
socialweb-computer run request --network mn1 --secrets secrets.json --hold --exec 'cat /root/x'
socialweb-computer run request list
socialweb-computer run request release <name>
socialweb-computer run request resume <name>
```

At the end of a held invocation the entry is marked `stopped`: its core and its
components stop, and its workspace and state dir are kept until `release` or
`resume` starts them again.

### Deploy with Infra and EphemeralInfra

A deployment is a kcp object in a requester's workspace that owns its
`ComputeContract` objects (plan `docs/plans/0029-infra-and-ephemeral-infra.yaml`).
An `Infra` lives until it is deleted; an `EphemeralInfra` ends at
`creationTimestamp + spec.ttlSeconds` or, with `endWhen: Complete` (the
default), once every requested contract is Released or Failed; then it releases
its contracts and deletes itself. Each contract has a desired `state` that you
write and a `phase` that the controller reports:

| state | what happens | phase |
| --- | --- | --- |
| `Staged` (default) | nothing: no ComputeContract, no RFP | `Staged` |
| `Requested` | the controller applies the ComputeContract and the Requester runs the RFP flow | `Requesting`, then `Deployed` (Held) or `Failed` |
| `Released` | the controller writes `spec.release` on the ComputeContract | `Releasing`, then `Released` |

Deleting a deployment releases every contract first (finalizer
`infra.socialweb.computer/release`), then deletes the ComputeContracts and the
record, then the object goes.

A requester app for deployments that stays up (no held guest, no CLI) is one
`spec.apps[]` entry of the network:

```bash
kubectl --server "$KCP/clusters/root" apply --server-side --field-manager socialweb.computer/network-app/infra-r -f - <<'EOF'
apiVersion: socialweb.computer/v1alpha1
kind: MarketNetwork
metadata: {name: network}
spec:
  apps:
  - {name: infra-r, kind: MarketRequester, profile: market-network}
EOF
```

Its workspace is `root:infra-r`. The root links its own account's repo
(`repo-did-plc-...`, the `AtprotoRepo` named in `status.did` of the
MarketRequester) to the deployment kinds: repo.read for the four kinds and
repo.write for their AtprotoRecords, both scoped to `computer.socialweb.infra` and
`computer.socialweb.ephemeralInfra`.

A staged Infra, bound to the requester's own repo:

```bash
kubectl --server "$KCP/clusters/root:infra-r" apply -f - <<'EOF'
apiVersion: socialweb.computer/v1alpha1
kind: Infra
metadata: {name: web}
spec:
  description: one guest
  contracts:
  - {name: vm1, state: Staged}
  record:
    repoRef: {kind: AtprotoRepo, name: repo-did-plc-REPLACE}
    eventSourceRefs: [{kind: EventSource, name: relay-network}]
EOF
kubectl --server "$KCP/clusters/root:infra-r" get infra web -o yaml
```

The record is `at://<did>/computer.socialweb.infra/web` (`status.record.uri`). Request
the guest:

```bash
kubectl --server "$KCP/clusters/root:infra-r" patch infra web --type merge \
  -p '{"spec":{"contracts":[{"name":"vm1","state":"Requested"}]}}'
kubectl --server "$KCP/clusters/root:infra-r" get computecontracts -l infra.socialweb.computer/name=web
```

`spec.contracts` is replaced whole by a patch or a pull, so a patch names every
contract. `spec.secrets` (`[{contract: vm1, secretsRef: {...}}]`) becomes the
ComputeContract's `secretsRef` and is never written to the record.

Two-way sync. Any client of the account may edit the record (for example
`com.atproto.repo.putRecord` to its PDS). The `AtprotoRecordWatch` the Infra owns
sees the commit on the event source, the controller reads the record, and when
only the record changed since the last sync it applies the record's
`description` and `contracts` to the spec (field manager
`socialweb.computer/infra-record`; `status.record.direction: Pulled`). When only
the spec changed it writes the record (`Pushed`), compare-and-swap on the cid it
read. When both changed the record wins and `SyncConflict` is True with reason
`RecordWon`. A record deleted elsewhere deletes the object. A following object
names an existing record with `spec.record.uri` instead of publishing its own.

An account's whole collection, enumerated into child objects (one per record,
named by the rkey):

```bash
kubectl --server "$KCP/clusters/root:infra-r" apply -f - <<'EOF'
apiVersion: socialweb.computer/v1alpha1
kind: InfraCollection
metadata: {name: mine}
spec:
  repoRef: {kind: AtprotoRepo, name: repo-did-plc-REPLACE}
  eventSourceRefs: [{kind: EventSource, name: relay-network}]
EOF
kubectl --server "$KCP/clusters/root:infra-r" get infracollection mine -o jsonpath='{.status.children}'
```

A record that another object already binds is listed with `boundBy` and gets no
child. Deleting the collection deletes its children, so it releases their
contracts and deletes their records; `kubectl delete --cascade=orphan` keeps
both. `EphemeralInfraCollection` is the same for
`computer.socialweb.ephemeralInfra`.

An EphemeralInfra that rents one guest for ten minutes:

```bash
kubectl --server "$KCP/clusters/root:infra-r" apply -f - <<'EOF'
apiVersion: socialweb.computer/v1alpha1
kind: EphemeralInfra
metadata: {name: burst}
spec:
  ttlSeconds: 600
  contracts:
  - {name: vm1, state: Requested}
  record:
    repoRef: {kind: AtprotoRepo, name: repo-did-plc-REPLACE}
    eventSourceRefs: [{kind: EventSource, name: relay-network}]
EOF
```

For an account other than the root's, write the links the root writes for its
own: an `AtprotoRepo` for the account (a `LocalRepo` on a Pds component, or a
`RemoteRepo` with its credentials), then ComponentLinks in the workspace from
the four kinds (selector `kind: Infra`, and so on) to that AtprotoRepo with
`repo.read` and from `AtprotoRecord` objects with the label
`infra.socialweb.computer/kind` with `repo.write`, both scoped to the two NSIDs.

### Add a bidder

`run bidder --network` adds a `MarketBidder` entry to `spec.apps[]`. Its name is
the handle's first label, or `--name`. The command waits until the network
reports the bidder Ready with its did, prints a summary and returns. `--fg` stays
and removes the bidder from the network on Ctrl-C. `--handle` names the owner of
its ComputeVMs; its atproto handle in the network is `<name>.bob.test`.

```bash
socialweb-computer run bidder --profile market-network --host-backend proc --network mn1 \
  --handle mybidder-1.bob.test --guest-image-source file:///var/tmp/guest-image --vm-host-backend systemd-user
```

With `--network`, the market network places the bidder under its own devsup, so
`--host-backend` must be `proc` and `--profile` must be `market-network`;
`run bidder` refuses any other pair.

### Add an SSH gateway

`run gateway ssh --network` adds an `SshGateway` entry to `spec.apps[]`. The
network places it in its workspace `root:<name>` and copies the network value and
the bidders' DIDs onto its root object. The gateway holds an OpenBaoServer
`ssh-gateway` (OAuth sessions and web sessions), the iroh dialer HostProcess
(`socialweb-computer internal iroh-dial`, `internal/irohdial`, on a unix socket
in the SshServer's state dir; the only process that runs `dumbpipe connect`) and
the in-process `Requester` and `SshServer ssh-server` components. The gateway
declares two local Exposures for the `SshServer`: one serves `ssh` and one serves
its HTTP keys API. `run gateway ssh` prints the local ssh Exposure address in
`.ssh` and every local endpoint in `.listeners`.

```bash
socialweb-computer run gateway ssh --host-backend proc --network mn1 --name sshgw \
  -- --test-owner-sessions-file /abs/owner-sessions.json
ssh -p <port of .ssh> -o SendEnv=LC_POLICY <account DID>@127.0.0.1 'hostname'
```

The market network must be brought up with dumbpipe (`--dumbpipe-path` or
`SOCIALWEB_DUMBPIPE_PATH` in a dev build); `run gateway ssh` refuses a network
without it. The owner sessions file is a market-network seam: a JSON object
keyed by account DID of `{service, identifier, password}`, used in place of a
deposited OAuth session. The requester signs in to that PDS with the session,
writes its RFP records there, and mints service-auth tokens through it; it
publishes the badgeBlueKeys record that binds its own signing key. The same
credentials authenticate the keys API: `POST /keys` with HTTP basic auth
(`identifier` and `password`) adds the key the account may use at the ssh
server. The account must have a `badgeBlueKeys` `requester_associate` record for
the ssh key.

Every ssh connection rents its guest through the RFP flow and releases it
(vm.delete) when the command exits. A connection that sends `LC_SESSION=<name>`
holds the guest under that name, reuses it for later connections of the same
account, and releases it after five idle minutes (`sessionIdleSeconds`, default
300) or when the requester stops. That is what lets a connection resume after
`socialweb-computer dev restart mn1 sshgw/ssh-server` without a second RFP. A
connection that does not send `LC_SESSION` keeps the one-shot rent-and-release
behavior.

### Add a cluster gateway

A gateway that runs tenant clusters joins a market network as its own
application:

```bash
socialweb-computer serve gateway kcp --host-backend proc --state-dir /abs/gateway-state \
  --gateway-profile market-network --gateway-spec-file <spec>
```

The spec is a YAML GatewaySpec. Its `network` is the network value
(`dev status --instance mn1 | jq .network.status.network`, which carries the CA
bundle inline) and `marketNetwork.ownerSessionsFile` names the cluster owners'
accounts on the network's PDSes. The gateway's web app hands every node guest the
network's CA bundle. Its core runs a loopback egress proxy and sets
`HTTPS_PROXY` to it, so every vault it places reaches a bidder's port-less issuer
URL through the guest TLS front. Unless `SSL_CERT_FILE` is set, the core writes
the system roots plus the network's CA bundle to
`<state>/tls/outbound-ca-bundle.pem` and sets `SSL_CERT_FILE` to it, so it and
every vault it places verify the bidder's issuer.

`dev up --with-gateway` places the same gateway as a child of the network, in
its workspace `root:gateway` of the network's kcp. Its core runs what
`serve gateway kcp --gateway-profile market-network` runs (the sets, the Gateway
root object, the root vault, the web app, the clusterprovider, the egress
proxy). The network writes its spec (`apps/dev/<instance>-gateway/gateway-spec.json`
under the network's state dir, holding the network value and
`marketNetwork.ownerSessionsFile`), creates the owner account
`<instance>-gateway-owner.alice.test` on alice's PDS (`owner-sessions.json` next
to it, mode 0600), and the gateway brings up the tenant cluster `tenant` of that
owner at zero nodes. A pod applied to that cluster gets a node through the RFP
flow from the network's bidders.

The tenant cluster's nodes are one Infra in `root:gateway`, named
`nodes-<20 hex>` with the label `infra.socialweb.computer/cluster=tenant` and
`spec.clusterRef` naming the Cluster (plan 0031). Each NodeClaim in the
cluster's home workspace is one entry `node-<12 hex>` of it: `spec.contracts`
holds its state (Staged until the join bundle is written, Requested, Released
when the claim is deleted), `spec.nodes` its ownerDid, nodeJoin and
nodeClaimRef, and `spec.secrets` the bundle; the claim's `status.infra` names the
entry and its ComputeContract `infra-nodes-<hex>-node-<hex>`. Read it with
`kubectl get infras -l infra.socialweb.computer/cluster=tenant -o yaml` in
`root:gateway`. Deleting the Cluster deletes its Infra, which releases every node.

Every cluster's nodes are published to the owner's own repo; there is no
opt-in. The NodeClaim controller gives the Infra `spec.record`
(`repoRef: AtprotoRepo/repo-<owner did>` in `root:gateway:app`,
`sync: Publish`), and the record
`at://<owner did>/computer.socialweb.infra/nodes-<hex>` carries each node entry's
name, state, vmName and bidder DIDs and the observed phases, never `spec.nodes`,
`spec.secrets`, user data or the node policy. The gateway composes that repo for
the owner of `owner-sessions.json`: a `pdsAccount` MarketIdentity
`owner-<16 hex>`, a RemoteRepo and the AtprotoRepo, in `root:gateway:app`. For
an owner of a cluster who is not in that file, it composes the
MarketIdentity `owner-<16 hex>` in mode `oauthSessionProxy` instead (label
`apps.socialweb.computer/session-owner`): it reads the owner's web app session
from the root vault's atproto auth mount, and without one it reads
`SessionExpired` with reason `NoSession`, so no repo is composed and the Infra
reports Synced False RecordUnavailable. A session that lacks part of the scope
the web app now requests reads `SessionExpired` with reason `ScopeChanged`; the
Gateway stays Ready and its condition `SessionOwnersReady` names the owner. The
record is push-only: an edit or a delete made on the PDS is written over. The
record goes with the Infra when the cluster's last node goes or the Cluster is
deleted.

The gateway's requester trusts a bidder through its operator's vouches. The
network declares, in the gateway's `root:gateway:app`, one AtprotoRecord
`operator-vouch-<bidder app>` (`spec.repoRef: AtprotoRepo/operator`, label
`apps.socialweb.computer/record-repo: operator`, collection
`sh.tangled.graph.vouch`, rkey and subject the bidder operator's DID from its
MarketIdentity `approvedAssociations`). The gateway composes the AtprotoRepo
`operator` beside it: a RemoteRepo on the owner account's RemotePDS whose
credentials are a `pdsAccount` MarketIdentity `operator` (password and session in
the root vault; for an https PDS its `caRef` and the RemotePDS's name the root,
which publishes the network CA, and both verify against the system roots plus
that CA), and the ComponentLinks from the labelled records to the repo, the repo
to the RemoteRepo, the RemoteRepo to the RemotePDS and the MarketIdentity, and
the MarketIdentity to the root (vault.read on the CA). The AtprotoRecord
controller writes the record and its status (Applied with the uri). The other
direction is declared too: in each bidder's workspace `root:<bidder app>` one
AtprotoRecord `operator-vouch-<gateway app>` (the bidder operator's vouch for the
gateway operator), written the same way through the bidder's AtprotoRepo
`operator`, so a bidder under `bidder-tangled-vouch` (`dev up --app-values` with
`bidder: {policy: bidder-tangled-vouch}`) bids on the gateway's RFPs.

### Point an installed bidder account at the network

An installed bidder account (`install bidder`, host-backend systemd-user) is not
a child of the network: it is its own application and reaches the network only
over the atproto market protocol. Its `config.json` names
`"profile": "market-network"` and `"market-network-file"`, a file holding the
network's status (`dev status --instance mn1 | jq .network.status`): the network
value, with its CA bundle inline, and `bidderPdsUrl`, the PDS where the bidder
makes its account. `run bidder` refuses `--market-network-file` together with
`--network`. Its VM units are units of that account's own user manager, so a
restart of the whole manager (`systemctl restart user@<uid>`) touches only that
account.

## Tear down a market network

```bash
socialweb-computer dev down --instance mn1
```

`dev down` deletes the `MarketNetwork` and waits for it to go. Its finalizer
tears every child down: it deletes the child's root object (the root releases its
ComputeVMs first), then the child's workspace `root:<app>`, then
`root:dev:<instance>-<app>`, which stops the child's core, and then the child's
state. It then stops the network's components and deletes `OpenBaoServer network`.
`dev down` then stops any VM unit a bidder child left (`vmsStopped`, `vmsLeft`),
stops devsup, and kills any process left under the instance (`leftoverKilled`,
`remaining`). `--keep-state` keeps the instance state.

The network keeps its state across a restart of its core: the OpenBao storage,
the port leases, the PLC operation log (`plc.json`) and each PDS's repos and
accounts (`repos.db`, `accounts.db`) in their component state dirs under
`<state>/network/components/`. For a Go change, rebuild and run
`socialweb-computer dev restart mn1 core --binary <dir>/bin/socialweb-computer`;
do not take the network down and up again.

## The MarketNetwork object

`socialweb-computer dev up --profile market-network` applies one
`MarketNetwork network` into the workspace `root` of the instance's kcp. The
types are in `api/v1alpha1/types_application.go`.

### MarketNetwork spec

| Field | Meaning |
| --- | --- |
| `profile` | the built profile, `market-network`; any other value fails the network |
| `suspend` | stop participation; cascades into each child root's `spec.suspend` |
| `guestLabel` | the label the guests carry, defaulting to the instance id |
| `comms` | the market comms mode, passed to every child |
| `ingress` | the PDS ingress selection (see below) |
| `components` | overrides of the network's own components |
| `apps` | the child applications, as `NetworkApp` entries |

### NetworkApp

| Field | Meaning |
| --- | --- |
| `name` | the child's name (`bidder-1`, `gateway`, an ssh gateway's name), a DNS label |
| `kind` | `MarketBidder`, `MarketRequester`, `Gateway` or `SshGateway` |
| `profile` | the child's profile |
| `policy` | the bidder policy |
| `firecracker` | the FirecrackerProvider for a bidder |
| `kcp` | the KcpProvider for a bidder (handle, vmHostBackend, guestImageSource, stopGraceSeconds) |
| `options` | application options passed through to the root object |
| `quota` | the child's quota |
| `ownerSessionsFile` | the owner sessions file for a gateway or ssh gateway |
| `clusterProvider` | the cluster provider for a `Gateway` |
| `components` | overrides of the child's components |
| `suspend` | suspend this child only |
| `stopped` | the child's core and components are stopped; its workspace and state dir are kept |

### MarketNetwork status

| Field | Meaning |
| --- | --- |
| `phase` | `Pending`, `Provisioning`, `Ready`, `Suspended`, `Deleting` or `Failed` |
| `message` | the current step or the failure |
| `network` | the network value (see below) |
| `bidderPdsUrl` | the PDS URL the bidders make their accounts on (bob's PDS) |
| `components`, `listeners` | the network's components and listeners |
| `componentSummary`, `componentOverrides` | the component counts and the state of each override |
| `apps` | each child: `name`, `kind`, `instance`, `kcpUrl`, `workspace`, `phase`, `message`, `did`, `components`, `listeners` |
| `stateDir`, `gc`, `ttl` | state directory usage, garbage collection and TTL |
| `conditions` | see [Conditions](#conditions) |

The network value (`status.network`, copied onto each child's root object):

| Field | Meaning |
| --- | --- |
| `plcDirectoryUrl` | the network PLC directory URL (the plc local Exposure) |
| `ingressProxyHost` | the dispatcher's host and port, `relay.localhost:<port>` |
| `qrOrigin` | the QR session-transfer origin; the market network leaves it empty |
| `caBundle` | the CA bundle the children trust, inline PEM |
| `hosts` | the network's service names and their loopback addresses |
| `comms` | the market comms mode |
| `ingress` | the network's PDS ingress selection: provider, the `dispatcher` ingress ref, and any xrpc or opentunnel settings; children inherit it |

The guest TLS front URL is not a field: it is `https://relay.localhost:<port>`,
with the port of the `dispatcher-tls.default.relay.svc.kcp.local` entry of
`hosts`.

The network has one `IngressProxy/dispatcher` for its PDSes. `MarketNetwork.spec.ingress` selects its
provider; with no spec it is `xrpc-subscription`. `spec.ingress.opentunnel` selects `opentunnel`, and
`spec.ingress.provider` names the provider explicitly; the network PDSes refuse `iroh`. The selected
value is published as `status.network.ingress`; every child root inherits it, and a child's own
`spec.ingress` overrides it. Alice, bob and the PDS of every child app are exposed through the
dispatcher with ingress Exposures.

### Conditions

| Condition | Holds when |
| --- | --- |
| `ServicesReady` | every component the network composes reports ready with its endpoints |
| `OneKcp` | every child reports the network's kcp front and a workspace of its own |
| `VerifierPassed` | the composer's probes answer: plc `/health`, relay, alice and bob `/xrpc/_health`, dispatcher `/.well-known/did.json`, and every bidder and gateway child is Ready |
| `PdsIngressAlice`, `PdsIngressBob` | the PDS's ingress is ready |
| `ComponentsReady`, `ComponentOverridesValid` | the component summary and the overrides |

The network is Ready only when `ServicesReady`, `OneKcp` and `VerifierPassed`
hold. The probes run in the core (`internal/app/marketnetwork/vault.go`); there
is no separate verifier process.

A requester invocation reports in its own workspace: the ComputeContract's
`status.contract` (winner, bids, receipt, the guest's host keys, `release` with
the vm.delete status, and `termination` when the bidder ended the contract, for
example a guest that powered itself off). `run request` prints a summary of it.

## The manifest set

The network's manifests are the `market-network` set in `deploy/sets.yaml`. The
composer applies only its `vault` phase; the network's components are not
manifests but objects the composer writes (see
[How components start](#how-components-start)).

### Manifest files

| File | Kind | What it is |
| --- | --- | --- |
| `05-vault.yaml` | `OpenBaoServer` | `OpenBaoServer network`: the `pki` mount, the `network-pki` policy, the clients `components` and `owner`; phase `vault`, applied by the composer |
| `00-workspaces.yaml` | `Workspace` | the `global`, `relay`, `alice` and `bob` workspaces (type `universal`); phase `workspaces`, which the composer does not apply |
| `10-rbac.yaml` | `ServiceAccount`, `ClusterRole`, `ClusterRoleBinding` | a service account that may get OpenBaoServer objects, and the `pds-components` access of the `pds` account; applied in `root:app` by the `market-bidder`, `market-requester` and `ssh-gateway` sets |

### How components start

The composer writes one object per component (`Plc`, `Relay`, `IngressProxy`,
`DispatcherTLS`, `Pds`, `PolicyEngine`) and one Service and one or more Exposure
objects per published handler, by server-side apply under the field manager
`socialweb.computer/marketnetwork`, each with an ownerReference to the root and
the root labels. It also writes the ComponentLinks those components call
through. Each component's state dir is `<state>/network/components/<Kind>-<name>`.

The component controller (`internal/controllers/component`) in the core
reconciles each object into the component runtime (`internal/component`). The
runtime builds the Go service from the factory registered for its kind
(`internal/stack/componentcore`), starts it in process, restarts it with backoff
when it fails, and reports its phase, readiness and endpoints in the object's
status. A Service names a component and a handler; an Exposure publishes the
Service, local on a loopback port or through an ingress (`internal/exposure`).
Third-party binaries, such as OpenBao and the child cores, run as HostProcess
units instead.

`spec.components[]` overrides a component (for example `suspend`, which stops it).
`socialweb-computer dev restart mn1 [<app>/][<workspace>:][<kind>/]<component>`
restarts one component; the PolicyEngine restarts only with `core`.

### Services and addresses

| Workspace | Component | Host name | Port lease |
| --- | --- | --- | --- |
| `root` | `Plc plc` | `plc.default.global.svc.kcp.local` | `Global/plc` |
| `root` | `Relay relay` | `relay.default.relay.svc.kcp.local` | `Relay/relay` |
| `root` | `IngressProxy dispatcher` | `dispatcher.default.relay.svc.kcp.local` | `IngressProxy/http` |
| `root` | `DispatcherTLS dispatcher-tls` | `dispatcher-tls.default.relay.svc.kcp.local`, serving `relay.localhost` | `DispatcherTLS/tls` |
| `root` | `Pds alice` (handle `alice.test`) | `pds.default.alice.svc.kcp.local` | `AlicePds/http` |
| `root` | `Pds bob` (handle `bob.test`) | `pds.default.bob.svc.kcp.local` | `BobPds/http` |
| `root` | `PolicyEngine policy-engine` | none | none |

The host names keep the shape
`<name>.<namespace>.<workspace labels>.svc.<service domain>` of the workspaces
the services once ran in. They are fixed names in `internal/app/marketnetwork`
and `api/v1alpha1`, not derived from `root`. plc, relay and both PDSes publish a
Service `svc-<kind>-<name>` with a local Exposure `svc-<kind>-<name>-local` and
an ingress Exposure `svc-<kind>-<name>-ingress`. The network value takes the
plc, relay and PDS addresses from the local Exposures, and the dispatcher and
dispatcher-tls addresses from those components' endpoints.

## Troubleshoot a market network

- Start with `dev status --instance mn1`. `.network.status.message` names the
  current step, `.network.status.conditions` the condition that does not hold,
  and `.network.status.apps[].message` why a child is not Ready.
- `socialweb-computer units list --instance mn1` lists the instance's units. The
  core's log is `logs/core.log` in the instance's state dir; a child's is in
  `apps/dev/<instance>-<app>/logs/core.log` under it.
- "waiting for the network's PDS ingress URLs" means `IngressProxy dispatcher`
  has not published the PDSes yet. Every network PDS requires an ingress.
- "the root API of <app> is not served in its workspace yet" means the child's
  core has not bound the application APIs in its workspace; the network retries.
- A dial to a name that is not in `status.network.hosts` fails with
  "the host is not in the network host map": system DNS is not allowed.
- A binding that is not Ready lists empty but rejects creates. Waiting for a
  resource to be listable is not waiting for its binding to be Ready.
- kubectl caches discovery per server, and the cache outlives a schema change. A
  cached `NAMESPACED=false` makes kubectl POST a namespaced resource without a
  namespace. Use a throwaway `--cache-dir` when you read the network with
  kubectl.

## Limits

- **Real DNS for humans is not available.** `curl pds.default.alice.svc.kcp.local`
  from a shell does not resolve. Use the address of that name in
  `status.network.hosts`, or the `status.address` of its local Exposure.
- **Only names in the host map are dialled.** A subprocess or library that does
  not use `internal/kit/networkdial` or the bus does not see the network's names.
- **Every network PDS requires an ingress.** Without `IngressProxy dispatcher` a
  PDS has no public URL and the network does not become Ready.
- **The guest TLS CA is local.** The composer makes it in the instance's state;
  only a client that trusts the network CA bundle verifies `relay.localhost`.
