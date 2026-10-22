# Market network

A market network is one `MarketNetwork` application: the shared atproto services
of a local market, in their own kcp, plus child applications (bidders,
requesters, gateways) that find each other over the atproto market protocol.
Bring one up with `socialweb-computer dev up --profile market-network`. The
operator or developer who runs a market network and the applications it hosts
reads this file.

## What a market network is

A market network is one application, `MarketNetwork`, in its own kcp.
`socialweb-computer dev up --profile market-network` brings up the instance and
applies one root object, `MarketNetwork network`, into the instance's kcp. The
socialweb-computer core process (the binary process that runs kcp and supervises
the DenoPods and HostProcesses) renders that object through `internal/appctl`
into:

- the network's own services, as DenoPods of the network's kcp;
- an OpenBao server, `OpenBaoServer network`, that issues the services'
  certificates;
- one child application for each `spec.apps[]` entry: `bidder-1` .. `bidder-N`
  (`MarketBidder`) and, with `--with-gateway`, `gateway` (`Gateway`).

```
one market network (one dev instance)
|
+- kcp: the market network
|    root:global   DenoPod plc
|    root:relay    DenoPod relay, dispatcher, dispatcher-tls
|    root:alice    DenoPod pds, verifier
|    root:bob      DenoPod pds
|    OpenBaoServer network
|
+- child kcp: MarketBidder bidder-1 .. bidder-N
+- child kcp: Gateway gateway            (dev up --with-gateway)
+- child kcp: MarketRequester <name>     (run request --network)
+- child kcp: SshGateway <name>          (run gateway ssh --network)
```

A child application runs its own kcp. Its bootstrap layer (kine, one kcp slot on
port 0, the front, its core process) is HostProcess objects in the network's kcp,
in the child's workspace, placed by the network's supervisor. The child's core
process holds a token valid only in that workspace. Once the child's kcp answers,
the network applies the child's root object into that kcp with the network value
(`status.network`) copied onto it, and reads back only the child's root status
(did, listeners, phase). The copy is the only way a child learns about the
network: no child reads the network's kcp, and the child's DNS shim resolves only
its own application's names.

An `OpenBaoServer network`, in the network's own kcp, holds the network PKI. It
has one listener on 127.0.0.1:0 with mutual TLS, file storage under the
instance's state (`openbaoservers/network`), and no clustering. The
socialweb-computer OpenBaoServer controller initialises it, keeps the unseal key
in a mode 600 file of its state, and revokes the initial root token after its
first sync, so no root token is kept. The deno runtime is the client
`denoruntime`: it waits for the server to be Ready, logs in with its client
certificate, and gets a token that holds only the `network-pki` policy. The
application controller reads only the PKI root CA (`pki/ca/pem`) for the CA
bundle it hands the children.

Each workspace names one `OpenBao` object. The namespace `global.default` serves
`root:global`, `relay.default` serves `root:relay`, `alice.default` serves
`root:alice`, and `bob.default` serves `root:bob`. Each namespace's intermediate
CA is signed by the root CA in the OpenBao root namespace, and each DenoPod in a
workspace is issued its serving certificate from that workspace's intermediate.

Every bidder has its own OpenBaoServer `bidder`, in its own kcp
(`deploy/apps/market-bidder/vault.yaml`), placed by the bidder's own core
process. That core process generates the account password into the vault (client
`bidder`, kv mount `bidder`), signs up for an account of its own on bob's PDS
(`MarketIdentity` mode `pdsAccount`, the PDS URL from its root object), and hands
the bidder DenoPod a token that can read only the OpenBaoServer objects of its
own kcp. No password or key is in a kcp object, and the network never touches a
child's vault.

Every service binds port 0. It writes the port it got to `PORT_FILE` in its run
directory; the provider reads it into the DenoPod's `status.address`, and peers
reach the service by name through the kcpdns table, which the provider rewrites
as ports are reported.

### TLS and the certificate authority

The network's certificates come from OpenBao, not from kcp. `kcp start`
generates its CA in memory and persists only the serving key, so it cannot sign
leaves. The OpenBao root CA lives in OpenBao's root namespace, each workspace's
`OpenBao` object gives its namespace an intermediate signed by that root, and
each DenoPod is issued a leaf by its namespace's intermediate, for its own name,
with the loopback addresses on it.

A workload is given its leaf with its chain and a trust bundle carrying the root
and kcp's CA, written as `ca.pem` in the run directory with `DENO_CERT` pointing
at it. One setting, two CAs trusted: peers' leaves, and the API server.
`DENO_CERT` is read at startup, so it is injected rather than installed from
inside the script.

The bidder's values set `tls: trust`: the template writes only `ca.pem` and no
serving leaf. TLS is what lets the relay reach the PDS, which uses `https`.

### How names resolve

Deno resolves `fetch`, `WebSocket` and `Deno.connect` in Rust and does not
consult `Deno.resolveDns`, so the kcpdns shim patches all four entry points, and
`Deno.resolveDns` too, so a caller that asks it directly agrees with the table.
The provider injects `KCP_DNS_TABLE`, mapping every advertised name to its
address, read from each peer's `SERVICE_ARGS`/`SERVICE_ENV`. On a miss the shim
asks the kcp API with `KCP_TOKENS`, one token per workspace minted in the
workspace being read, because a token from one workspace is not authorised in
another. The relay spawns its child with the same shim, because a workload's own
argv cannot carry `--preload`.

The shim preserves the scheme and sets `Host` back to the original name, so a
service that derives its identity from `Host` still sees the name it was
configured with. The PDS does this for its `did:web` and OAuth issuer. The
client authenticates the address rather than the name; every leaf carries
`127.0.0.1` for that reason.

## Bring up a market network

### Prerequisites

- The `socialweb-computer` binary. A dev build from
  `scripts/build-socialweb-computer.sh --dev --publish <dir>` embeds the
  TypeScript closure and the assets. `--with-gateway` needs the gateway's
  embedded k3s and external-secrets assets, which that build provides.
- The VM runtime, when the network runs bidders: `--guest-image-source` and
  `--vm-host-backend systemd-user`, plus `--userns-stage-path`, `--pasta-path`,
  `--vmexec-path` and `--firecracker-path`, or the matching `SOCIALWEB_*_PATH`
  environment variables.
- Run as the account that owns the subordinate id range. `dev up` refuses to run
  as root.

### Start the network

Run one command. `--bidders` sets the number of child bidders (0 for none).
`--guest-image-source` and `--vm-host-backend` fill the bidders' compute
provider.

```bash
socialweb-computer dev up --instance mn1 --profile market-network --bidders 2 \
  --guest-image-source file:///var/tmp/guest-image --vm-host-backend systemd-user
```

`dev up` brings up the instance's bootstrap layer (kine, kcp behind its front,
the socialweb-computer core process) and applies the `MarketNetwork` root
object. It waits for the network to be Ready and prints it.

`--app-values` overrides the NetworkApp fields, for example the owner handle of
the bidders' ComputeVMs:

```json
{"bidder": {"kcp": {"handle": "{app}.mn1.bob.test"}}}
```

`{app}` becomes the bidder's name; make the handle unique per worktree on a
shared account.

N market networks on one host are N dev instances: every listener binds port 0,
so two `dev up --instance` with different names never collide.

### Check the network is Ready

```bash
socialweb-computer dev status --instance mn1
```

`dev status` prints the `MarketNetwork` status (`.network`). The network is
Ready when its conditions hold; see [Conditions](#conditions).

### Rent a guest

`run request` adds a `MarketRequester` entry to `spec.apps[]`, places its own
kcp, and drives the RFP flow against the network's bidders.

```bash
socialweb-computer run request --network mn1 --secrets secrets.json --exec 'cat /root/x'
```

The network copies the network value (with `hosts`) and the bidders' DIDs onto
the requester's root object, and the CLI then talks only to the invocation's own
kcp. The guest comes up only through the RFP flow: the requester DenoPod posts
the RFP, a bidder bids, the requester accepts, and the winner boots the guest
from cloud-init `user_data`. `--secrets` is written into the invocation's own
OpenBao and the guest fetches it from the requester's secrets server. The CLI
reaches the guest only over its iroh ticket, with `StrictHostKeyChecking=yes`
against the host keys the guest reported. Without `--hold` the CLI releases the
contract (vm.delete) and removes the invocation.

To hold the invocation and manage it later:

```bash
socialweb-computer run request --network mn1 --secrets secrets.json --hold --exec 'cat /root/x'
socialweb-computer run request list
socialweb-computer run request release <name>
socialweb-computer run request resume <name>
```

At the end of a held invocation the entry is marked `stopped`: its kcp, kine and
core process stop and its state dir is kept until `release` or `resume` starts
them again.

### Add a bidder

`run bidder --network` adds a `MarketBidder` entry to `spec.apps[]`. Its name is
the handle's first label, or `--name`. The command waits until the network
reports the bidder Ready with its did, and returns. `--fg` stays and removes the
bidder from the network on Ctrl-C. `--handle` names the owner of its ComputeVMs;
its atproto handle in the network is `<name>.bob.test`.

```bash
socialweb-computer run bidder --profile market-network --host-backend proc --network mn1 \
  --handle mybidder-1.bob.test --guest-image-source file:///var/tmp/guest-image --vm-host-backend systemd-user
```

With `--network`, the market network places the bidder under its own devsup, so `--host-backend` must be `proc` and `--profile` must be `market-network`; `run bidder` refuses any other pair.

### Add an SSH gateway

`run gateway ssh --network` adds an `SshGateway` entry to `spec.apps[]`, places
its own kcp, and copies the network value and the bidders' DIDs onto its root
object. Its kcp holds an OpenBaoServer (OAuth sessions and web sessions), the
iroh dialer HostProcess (`socialweb-computer internal iroh-dial` on a unix socket
in the gateway's state dir, the only process that runs `dumbpipe connect`) and
the in-process `Requester` and `SshServer` components. The gateway declares two
local Exposures for the `SshServer`: one serves `ssh` and one serves its HTTP
keys API. Both bind port 0; `run gateway ssh` prints the local ssh Exposure
address in `.ssh` and every local endpoint in `.listeners`.

```bash
socialweb-computer run gateway ssh --host-backend proc --network mn1 --name sshgw \
  -- --test-owner-sessions-file /abs/owner-sessions.json
ssh -p <port of .ssh> -o SendEnv=LC_POLICY <account DID>@127.0.0.1 'hostname'
```

The market network must be brought up with dumbpipe (`--dumbpipe-path` or
`SOCIALWEB_DUMBPIPE_PATH` in a dev build). The owner sessions file is a
market-network seam: a JSON object keyed by account DID of
`{service, identifier, password}`, used in place of a deposited OAuth session.
The requester signs in to that PDS with the session, writes its RFP records
there, and mints service-auth tokens through it; it publishes the badgeBlueKeys
record that binds its own signing key. The same credentials authenticate the
keys API: `POST /keys` with HTTP basic auth (`identifier` and `password`) adds
the key the account may use at the ssh door. The account must have a
`badgeBlueKeys` `requester_associate` record for the ssh key.

Every ssh connection rents its guest through the RFP flow and releases it
(vm.delete) when the command exits. A connection that sends `LC_SESSION=<name>`
holds the guest under that name, reuses it for later connections of the same
account, and releases it after five idle minutes or when the requester stops;
that is what lets a connection resume after `dev restart
<gateway>/ssh-server` without a second RFP. A connection that does not send
`LC_SESSION` keeps the one-shot rent-and-release behavior.

### Add a cluster gateway

A gateway that runs tenant clusters joins a market network as its own
application:

```bash
socialweb-computer serve gateway kcp --gateway-profile market-network --gateway-spec-file <spec>
```

The spec's `network` is the network value
(`dev status --instance mn1 | jq .network.status.network`, its CA bundle copied
out where the gateway account can read it) and `marketNetwork.ownerSessionsFile`
names the cluster owners' accounts on the network's PDSes. The gateway's web app
hands every node guest the network's CA bundle, and its core runs a loopback
egress proxy that every vault it places takes as `HTTPS_PROXY`, so a tenant
cluster's vault reaches a bidder's port-less issuer URL through the guest TLS
front. Unless `SSL_CERT_FILE` is set, the gateway's core process writes the system
roots plus the network's CA bundle to `<state>/tls/outbound-ca-bundle.pem` and sets
`SSL_CERT_FILE` to it, so it and every vault it places verify the bidder's
issuer.

`dev up --with-gateway` places the same gateway as a child of the network. Its
core runs what `serve gateway kcp --gateway-profile market-network` runs (the
sets, the Gateway root object, the root vault, the web app, the clusterprovider,
the egress proxy) on its own kcp from the network's bootstrap layer. The network
writes its spec (`apps/dev/<instance>-gateway/gateway-spec.json`, holding the
network value and `marketNetwork.ownerSessionsFile`), creates the owner account
`<instance>-gateway-owner.alice.test` on alice's PDS (`owner-sessions.json` next
to it, mode 0600), and the gateway brings up the tenant cluster `tenant` of that
owner at zero nodes. A pod applied to that cluster gets a node through the RFP
flow from the network's bidders. The gateway's requester trusts a bidder through
its operator's vouches: the network declares, in the gateway's `root:gateway:app`,
one AtprotoRecord `operator-vouch-<bidder app>` (`spec.repo: operator`,
`sh.tangled.graph.vouch`, rkey and subject the bidder operator's DID from its
MarketIdentity `approvedAssociations`), and the gateway writes it into the owner
account's repo through `owner-sessions.json` (status Applied with the uri).
The other direction is declared too: in each bidder's workspace one AtprotoRecord
`operator-vouch-<gateway app>` (the bidder operator's vouch for the gateway
operator), which the bidder core writes into its operator account's repo, so a
bidder under `bidder-tangled-vouch` (`dev up --app-values` with
`bidder: {policy: bidder-tangled-vouch}`) bids on the gateway's RFPs.

### Point an installed bidder account at the network

An installed bidder account (`install bidder`, host-backend systemd-user) is not
a child of the network: it is its own application and reaches the network only
over the atproto market protocol. Its `config.json` names
`"profile": "market-network"` and `"market-network-file"`, a file holding the
network's status (`dev status --instance mn1 | jq .network.status`): the network
value and `bidderPdsUrl`, where the bidder's account `<name>.bob.test` is made.
The CA bundle that file names must be readable by the bidder account; copy it out
of the network's state dir. Its VM units are units of that account's own user
manager, so a restart of the whole manager (`systemctl restart user@<uid>`)
touches only that account.

## Tear down a market network

```bash
socialweb-computer dev down --instance mn1
```

`dev down` deletes the `MarketNetwork`. Its finalizer deletes each child's root
object (a bidder's ComputeVMs first), then the child's workspace in the network's
kcp (the supervisor stops its kcp, kine, front and core process), then the
services and the OpenBaoServer, before devsup stops. It then stops any VM unit a
bidder child left (`vmsStopped`, `vmsLeft`) and any process left under the
instance (`leftoverKilled`, `remaining`). `--keep-state` keeps the instance
state.

The network's OpenBao keeps its PKI hierarchy across a restart of the
socialweb-computer core process, but the PLC directory and the PDSes hold their
data in memory. Take the network down and up again rather than restarting it,
because a restart empties them.

## The MarketNetwork object

`socialweb-computer dev up --profile market-network` applies one
`MarketNetwork network` into the instance's kcp. The types are in
`api/v1alpha1/types_application.go`.

### MarketNetwork spec

| Field | Meaning |
| --- | --- |
| `profile` | the built profile, `market-network` |
| `guestLabel` | the label the guests carry, defaulting to the instance id |
| `apps` | the child applications, as `NetworkApp` entries |

### NetworkApp

| Field | Meaning |
| --- | --- |
| `name` | the child's name (`bidder-1`, `gateway`, an ssh gateway's name) |
| `kind` | `MarketBidder`, `MarketRequester`, `Gateway` or `SshGateway` |
| `profile` | the child's profile |
| `policy` | the bidder policy |
| `firecracker` | the FirecrackerProvider for a bidder |
| `kcp` | the KcpProvider for a bidder (handle, vmHostBackend, guestImageSource, stopGraceSeconds) |
| `options` | application options passed through to the root object |
| `ownerSessionsFile` | the owner sessions file for a gateway or ssh gateway |
| `clusterProvider` | the cluster provider for a `Gateway` |
| `stopped` | the child's kcp is stopped and its state kept |

### MarketNetwork status

| Field | Meaning |
| --- | --- |
| `phase` | `Pending`, `Provisioning`, `Ready`, `Deleting` or `Failed` |
| `message` | the current step or the failure |
| `network` | the network value (see below) |
| `bidderPdsUrl` | the PDS URL the bidders make their accounts on (bob's PDS) |
| `components`, `listeners` | the services' components and listeners |
| `apps` | each child: `name`, `kind`, `instance`, `kcpUrl`, `kcpRoot`, `phase`, `did`, `listeners`, `components` |
| `stateDir` | state directory usage |
| `conditions` | `ServicesReady`, `VerifierPassed`, `OwnKcp` |

The network value (`status.network`, copied onto each child's root object):

| Field | Meaning |
| --- | --- |
| `plcDirectoryUrl` | the network PLC directory URL |
| `ingressProxyHost` | the dispatcher's host, `relay.localhost` |
| `relayUrls` | the relay URLs |
| `firehoseUrls` | the relay firehose URLs |
| `qrOrigin` | the QR session-transfer origin |
| `guestTlsUrl` | the guest TLS front URL |
| `caBundleRef` | the path to the CA bundle the children trust |
| `hosts` | the network's service names and their loopback addresses |

### Conditions

| Condition | Holds when |
| --- | --- |
| `ServicesReady` | every long service DenoPod (plc, relay, dispatcher, dispatcher-tls, both PDSes) is Running and ready, reports at least one listener, and carries the `denopod.deno.computer/run` finalizer |
| `VerifierPassed` | `alice/verifier` Succeeded |
| `OwnKcp` | the network and every child report their own kcp front and kcp root |

The verifier DenoPod creates an account on alice's PDS, writes a record, and
watches the relay carry the commit. Its `Succeeded` phase sets `VerifierPassed`.

A requester invocation reports in its own kcp: the ComputeContract's
`status.contract` (winner, bids, receipt, the guest's host keys, `release` with
the vm.delete status, and `termination` when the bidder ended the contract, for
example a guest that powered itself off). `run request` prints a summary of it.

## The manifest set

The network's services are the `market-network` set in `deploy/sets.yaml`,
applied by `internal/appctl`.

### Manifest files

| File | Kind | What it is |
| --- | --- | --- |
| `00-workspaces.yaml` | `Workspace` | the `global`, `relay`, `alice` and `bob` workspaces (type `denoruntime`) |
| `05-vault.yaml` | `OpenBaoServer` | the network's OpenBao server and its `network-pki` policy |
| `10-rbac.yaml` | `ServiceAccount` | the `deno-runner` service account, applied in each workspace |
| `15-openbao-global.yaml` | `OpenBao` | the `global.default` namespace and its intermediate |
| `15-openbao-relay.yaml` | `OpenBao` | the `relay.default` namespace and its intermediate |
| `15-openbao-alice.yaml` | `OpenBao` | the `alice.default` namespace and its intermediate |
| `15-openbao-bob.yaml` | `OpenBao` | the `bob.default` namespace and its intermediate |
| `20-global-plc.yaml` | values | the `plc` service |
| `30-relay-relay.yaml` | values | the `relay` service |
| `35-relay-dispatcher.yaml` | values | the `dispatcher` service |
| `36-relay-dispatcher-tls.yaml` | `DenoPod` | the guest TLS front for `relay.localhost` |
| `40-alice-pds.yaml` | values | alice's PDS |
| `50-verifier.yaml` | `DenoPod` | the end-to-end verifier |
| `60-bob-pds.yaml` | values | bob's PDS |

The values files (`20`, `30`, `35`, `40`, `60`) are not objects: they are the
values of the service template `deploy/denopods/service.yaml`. The set applies
the PDSes before the relay, before the verifier; the OpenBao objects must report
ready before the workloads.

### The service template

Every service DenoPod (plc, relay, dispatcher, both PDSes, every bidder) is
rendered from `deploy/denopods/service.yaml`. The template fields are:

| Field | Meaning |
| --- | --- |
| `name` | the DenoPod name |
| `entry` | a name in `deploy/ts-entries.yaml` |
| `stateDir` | the service's state directory |
| `serviceAccount` | the service account the pod runs as |
| `tls` | `serve`, `trust` or `none` |
| `args` | the entry's arguments |
| `env` | the entry's environment |
| `permissions` | the entry's Deno permissions |
| `probe` | the readiness probe host and path |

The template's supervisor runs the entry on the pinned deno with
`--no-code-cache --node-modules-dir=none --frozen`, the root `deno.json` and
`deno.lock` (plus `--cached-only` in production), from the source root the deno
runtime names in `SOCIALWEB_TS_ROOT`, never from an absolute host path. Its
working directory is its state dir, never the source tree. Preloads are the
kcpdns shim first, then the entry's own preloads and `--unstable-*` flags from
`deploy/ts-entries.yaml`. The child gets a clean environment: `HOME` is its own
state dir, `TMPDIR` is under it, and `PORT_FILE`, `LISTENERS_FILE` and
`STATUS_FILE` are in its run directory.

Permissions are explicit. `-A` is refused; read and write are the source root,
the run directory, the state dir and the paths the values name. `--allow-run` and
`--allow-ffi` render only with a reason, and the bidder names neither. `internal/manifests`
(`service.go`) checks all of this when it renders.

Every DenoPod carries the `denopod.deno.computer/run` finalizer: the template
renders it, and the deno runtime adds it to any DenoPod that lacks it before it
starts a process, so deleting a DenoPod always stops its deno process.

### Services and addresses

| Workspace | DenoPod | Entry | Address |
| --- | --- | --- | --- |
| `root:global` | `plc` | `hono-plc` | `plc.default.global.svc.kcp.local` |
| `root:relay` | `relay` | `hono-atproto-relay` | `relay.default.relay.svc.kcp.local` |
| `root:relay` | `dispatcher` | `hono-did-key-ingress-proxy` | `dispatcher.default.relay.svc.kcp.local` |
| `root:relay` | `dispatcher-tls` | none (inline script) | the guest TLS front for `relay.localhost` |
| `root:alice` | `pds` | `hono-pds` | `pds.default.alice.svc.kcp.local` |
| `root:bob` | `pds` | `hono-pds` | `pds.default.bob.svc.kcp.local` |
| `root:alice` | `verifier` | none (inline script) | none; runs once to completion |

Every service binds port 0. The service name is
`<name>.<namespace>.<workspace labels>.svc.<service domain>`, for example
`pds.default.alice.svc.kcp.local`. The workspace labels are the logical cluster
path with `root` dropped and the rest reversed, because a colon is not legal in a
DNS label and the path is the only name kcp gives a workspace. The service
domain defaults to `kcp.local`, so `svc` is structural.

Objects carry the cluster ID, not the path. The `kcp.io/cluster` annotation holds
the cluster ID; the path lives on the workspace's own `LogicalCluster` object
under `kcp.io/path`. The provider resolves one to the other once per workspace
and caches it, so a name reads `pds.default.alice` rather than
`pds.default.<cluster-id>`.

Every service runs from this repository: `ts/atproto-market`, `ts/atproto-relay`,
`ts/hono-pds`, `ts/did-key-ingress-proxy`, and the root `deno.json` and
`deno.lock`.

## Troubleshoot a market network

- A service DenoPod must allow the shim's keys (`KCP_SHIM`, `KCP_DNS_TABLE`,
  `KCP_DNS_TABLE_FILE`, `KCP_TOKENS`, `KCP_SERVER`, `KCP_NAMESPACE`) in its
  permissions or environment. The preload runs inside the workload's process, so
  a DenoPod that does not allow them cannot read its own table.
- A namespace with no `OpenBao` object serves plain HTTP. `SERVICE_TLS=true` is
  not enough on its own: the provider finds the authority by looking for the
  single `OpenBao` object in the pod's namespace, and a namespace that names two
  is refused rather than guessed at. Check that each `OpenBao` object reports
  ready before the workloads apply.
- A binding that is not Ready lists empty but rejects creates. Waiting for a
  resource to be listable is not waiting for its binding to be Ready.
- kubectl caches discovery per server, and the cache outlives a schema change. A
  cached `NAMESPACED=false` makes kubectl POST a namespaced resource without a
  namespace. Use a throwaway `--cache-dir` when you read the network with
  kubectl.
- kcp does not send the bookmark a client-go streaming list needs, so an informer's
  initial list never completes and the provider reconciles nothing while looking
  healthy. The provider sets `KUBE_FEATURE_WatchListClient=false` itself.
- A pod's own name has to be seeded into its table: its probe resolves that name,
  and the name cannot be in the informer cache before the pod has started.
- The kcpdns table is written once, at start. `WebSocket` is constructed
  synchronously and cannot fall back to discovery, so a relay started before its
  PDS never learns the PDS name. The market-network set applies the PDS first.

## Limits

- **WebSocket names must be in the table.** There is no discovery fallback for
  `WebSocket`, unlike `fetch`.
- **Real DNS for humans is not available.** A DenoPod cannot carry
  `/etc/resolv.conf`, so `curl pds.default.alice.svc.kcp.local` from a shell does
  not resolve. Only the workloads see these names.
- **The PLC directory and the PDSes hold their data in memory.** A restart
  empties them; take the network down and up again to reset it.
- **Anything that bypasses the four patched entry points** (a native library, or
  a subprocess the workload spawns) does not see virtual DNS.
- **The bidder registers with an XRPC dispatcher.** It needs one reachable at the
  URL it is given; without a dispatcher it comes up but can list nothing.
