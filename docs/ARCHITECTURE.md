# socialweb-computer-kcp - architecture

Service which serves virtual Kubernetes clusters. A tenant cluster's k3s **nodes** scale to zero
while its control plane stays live. A single ATProto account may have multiple clusters.

Module `github.com/johnandersen777/socialweb-computer-kcp`, Go. CRDs in API group `socialweb.computer`.

---

## 0. The organizing principle: kcp is part of each application

kcp (the Kubernetes control plane) is a core part of each application, not infrastructure shared
between applications. The one exception is the virtual Kubernetes clusters project, which uses kcp in
the traditional way. This section states the principle and the rules that follow from it.

- **An application instance = its own kcp + a manifest set + a profile.** A bidder handle, a requester
  `run request`, the gateway, a market network, a dev instance. kcp belongs to the application
  instance, never to a machine; a host is only where instances run, and any number run on one host.
- **Application state = kcp objects** in that instance's kcp: ComputeVM, ComputeContract, NodeClaim,
  OpenBaoServer, Postgres, HostProcess, the component kinds (Pds, Plc, Relay, Bidder, GuestControlPlane,
  Requester, GatewayAPI, SshServer, IngressProxy, DispatcherTLS, PolicyEngine) and their Service and
  Exposure objects.
- **Application components** (the servers that would otherwise sit inside one process) = component
  instances that run in process in the core, in the component runtime of `internal/component`, and
  HostProcesses for third-party binaries; both are reconciled from those objects.
- **Modes = profiles over one manifest set.** A bidder in OAuth QR mode against a real account has no
  Pds component; a bidder in the market network has a Pds component of its own.
- **Composition = applying manifest sets, or nesting instances as workspaces** (a parent's workspace
  holds a child instance's HostProcess objects; in a market network the child application itself is a
  workspace of the parent's kcp).
- **Applications talk to each other only over the atproto market protocol.** A shared kcp (the market
  network profile) is composition and isolation, never a data channel.
- **The market network** (`deploy/market-network`) is the seed of this model and the vehicle
  for parallel workstreams: each workstream's live tests run in its own market network, brought up in its own
  git worktree of this repository; the shared real bidder
  (bobvmbuilder) and alice are a final soak only.
- **The one traditional use** is the virtual Kubernetes clusters project this document mostly
  describes (sections 1-4, 9-11): tenant k3s `--disable-agent` control planes fronted by kcp
  (clusterprovider, socialweb-computer-k8s, NodeClaims). Even there, the gateway's kcp is one
  application instance's kcp.

**Port 0 rule.** Every listener behind the edge binds port 0 and reports its bound address into its
own kcp object's status (a HostProcess, kcp slot, kine or front is read back from its single listening
socket). An in-process component does not listen itself: it registers handlers, a Service names the
component and a handler, and an Exposure publishes the Service (`internal/exposure`). A local Exposure
listens on a port the core keeps in its port registry (`<state>/ports/ports.json`, a 64-port block of
20000-29999 chosen by the state dir, `internal/stack/componentcore`), so the number survives a core
restart, and records it in `status.address`. Peers resolve each
other by service name through kcp: an address is application state in kcp, not config. Why: N instances
per machine; no allocator and no bind-close-rebind race; nesting; blue/green slots; isolation (an
application learns addresses only from its own kcp, so it can never reach another instance's components
by accident). Exception: only stable public edge entry points keep known addresses - caddy on 443 and an
application's front that caddy proxies to (the gateway's kcp front on 127.0.0.1:6443, the GatewayAPI web
app front on 127.0.0.1:8788, `internal/stack/gatewaykcp` `DefaultWebAppFront`).
A tenant cluster's members are third-party binaries that take a port number on their command line; for
them the cluster's apiserver front binds port 0 and holds the socket for the life of the cluster, and the
member binds that number beside the hold (section 8), so the number is stable across member restarts
and no other process on the host can take it.

What stays about the machine: `/dev/kvm`, the systemd user manager and its units (production placement
only), `install`, the host contract, and the bootstrap layer that runs an instance's kcp itself.

**How a multi-server process decomposes into kcp:**

- **One root object per application** in its own kcp - MarketBidder, MarketRequester, Gateway,
  MarketNetwork. Its spec carries the application's options and profile; the
  core renders the profile into owned children, and a profile switch deletes what the new profile omits.
- **Which kcp, by profile (user decision 2026-10-17, amending the strict rule of 2026-10-12).** The prod
  profiles keep a strict kcp per application: the gateway on its host, a bidder handle on its host, a
  `run request` without `--network`. The market network profile (dev, CI, soak) runs every application in
  ONE kcp, each application in its own workspace: the network's own
  services in root:global (PLC), root:relay (relay, dispatchers), root:alice and root:bob (PDSes), and each
  child application (a bidder, the gateway, a requester invocation, an ssh gateway) in root:<app>, its
  components in root:<app>:app. One manifest set serves both shapes: in prod it is applied into the
  application's own kcp root; in the network the applier rebases every phase workspace under root:<app>
  (kcpapply.Options.Base; root:*-provider and the sets of scope kcp stay shared) and service names come
  from the phase workspace (`<name>.default.{{.ServiceSuffix}}`). The child keeps its own core (the
  network's dev host places it, as a HostProcess in root:dev:<instance>), scoped to its workspace, with no
  kcp or kine of its own; its components run in process in that core. Children learn
  the network from values the composer copies onto their root objects and from valueFrom references that
  name another workspace of the same kcp (plan 0019).
- **Every CLI option is kcp-native.** Each option is a root-object field or a profile (the TypeScript
  hono-bidder and request-vm-ssh CLIs that came before plan 0026 are gone, and only legacy guest
  transports were dropped). Sign-in is an object, MarketIdentity: the URL to visit, the association to
  approve and an expired session are status that the foreground CLI renders (`internal/appcli`). A
  bidder or requester without a real account keeps its repo in a Pds component of its own (a LocalRepo,
  `internal/pds/local`).
- **kcp only.** No component runs as a standalone process; every run goes through `socialweb-computer`
  and kcp.
- **The requester is a fresh kcp per invocation** whose state dir (kcp root + vault) persists;
  `run request release|resume <name>` restarts that kcp to finish the contract.
- **Secrets only in the application's own OpenBao** - OAuth sessions, repo key, relay keypairs, the OIDC signing key (the identity engine of the bidder's own OpenBao),
  requester key, the gateway's keys; kcp objects hold references.
- **Every listener reports into status** (a component's `status.endpoints`, a Service's
  `status.endpoints`, an Exposure's `status.address`, a HostProcess's `status.listeners`), so no peer
  scrapes stdout for a DID, a port or readiness.
- **iroh is the default and required guest transport.** The ssh gateway moves to iroh; the tunnel,
  fedproxy-ssh, fedproxy-web and old browser-terminal modules, websocat ProxyCommand paths, the
  relay-FQDN guest addresses and the guest package registry are removed. Two other transports are
  opt-in per entry of `spec.tunnels`: transport `opentunnel` (an https URL to a guest port) and
  transport `webrtc` (a datachannel to a guest port through socialweb-guest-agent); each comes back
  as a tagged `vm.onNetwork` with its kind (plans 0032 and 0034, invariant 36).
- **Components:** the bidder's guest-facing endpoints (an allow-list proxy to the bidder's OpenBao for the guest's bind, login and identity token, `POST /v1/events`, `GET /v1/downloads` and the `compute.guest.subscribeEvents` subscription)
  are the GuestControlPlane component of the bidder; the NodeClaim fulfiller is the NodeClaim
  controller (`internal/controllers/nodeclaim`) bound by the gateway's GatewayAPI component.

## 1. The objective, stated once

This objective is the virtual Kubernetes clusters project, the one traditional use of kcp (section 0).

```
kcp + openbao + kine + embedded-postgres + k3s, as gateway-side processes.
A tenant cluster's k3s NODES scale to zero while the kcp + k3s CONTROL PLANE stays live.
```

Per virtual cluster:

```
kine over an external store      <- the datastore
k3s server --disable-agent       <- the apiserver. NOT a node. Always up.
k3s agent(s)                     <- the ONLY nodes. 0 or 1. Provisioned on apply.
```

`--disable-agent` is the piece that makes "zero nodes" a real state rather than a contradiction: a
single-node k3s where the server IS the node can never produce zero nodes.

**One apiserver per store, no exceptions.** kine's revision is a monotonic sequence with dummy fill
records specifically so no second client can create a lagging event, and two apiservers over one
store is unsafe. This constraint forces the shape.

The user and the nodes talk to the SAME API. There is no handover and no second store: the mount's
`status.URL` points at our apiserver at zero nodes and at N nodes alike.

**Zero nodes is not a cluster state.** The cluster stays `Ready` while nodes go to zero.

## 2. Scope boundary

```
IN   the virtual cluster: workspaces, the zero state, the store, the gateway, the trigger,
     identity, reachability, the CRDs, the ports, the lifecycle
OUT  how a guest COMES TO EXIST in production: the requester, the RFP record, the bid, the
     accept, the bidder, the market's money
SEAM ComputeProvider - provision(vm{user_data}, requesterDid, spec?) / destroy(id)
```

The seam is "given this cloud-init, make me a guest and give me back an id". A provider supplies a
CLOUD; it never supplies the CONNECTION SEMANTICS.

`kcp + k3s` in this document are **gateway-side processes**, not market VMs. Where text elsewhere
says "market VM", read it as the substrate a provider made, wherever it came from.

The market is reached through a NodeClaim. A tenant cluster's nodes are rented from the market as the
CLUSTER'S OWNER, through that owner's atproto OAuth session, which the gateway holds. The cluster
provider reaches past the `ComputeProvider` seam: `internal/nodemarket` writes a `NodeClaim` into the
cluster's home workspace, and the NodeClaim controller (`internal/controllers/nodeclaim`, bound by the
GatewayAPI component with the mapper of `internal/gateway/nodeclaim`) turns it into one contract entry
of the cluster's Infra (plan 0031), whose controller applies the ComputeContract that the gateway's
Requester runs through the RFP flow with the owner's session. The winning bidder
boots the guest from cloud-init, the guest joins over the cluster's apiserver ClusterLink, and the
controller writes the claim's status. The `internal/scale` decision that says WHEN a node is needed
is separate.

This does not change:

- The cloud-init is the real composer's (`BuildUserData` plus the module registry in
  `internal/atproto/cloudinit`).
  The substitution is who won the bid, never what the guest is told to install.
- A guest is born from cloud-init `user_data`, never hand-assembled. Provisioning goes through the
  declared path, never a shell. The market substrate adds no provisioning path of its own: it writes
  a record and waits.
- The claim carries PATHS, never secrets. The node host reads the join token, the per-node password
  and the server CA out of the cluster's data dir (files k3s writes), stores them in the cluster's
  OpenBao at `socialweb-nodes/<node>`, and the guest fetches them itself with a JWT login (invariant
  29).

## 3. The zero state - the mechanism

```
zero nodes  -> server up. No kubelet, so it never registers a Node.
               kubectl get pods -> objects, Pending, Unschedulable
apply       -> provision an agent -> it joins the SAME server -> the pod runs
idle        -> agent torn down -> back to zero nodes, server still up
```

The honest failures at zero, which are the correct failures:

```
exec          -> unable to upgrade connection: pod <name> does not have a host assigned
port-forward  -> unable to forward port because pod is not running. Current status=Pending
```

**Because the store is external, the control plane is cattle.** A destroyed server rebuilds against
the same store: no `bootstrap data ... different token`, identical CA fingerprint, objects intact,
and a kubeconfig issued before the rebuild still valid. The token is the only local-disk secret that
matters.

**There is no syncer.** There is one apiserver, not two, so there is nothing to propagate between
them: intent goes in, kubelet sees it. What remains is the binder:

```
podStrategy.PrepareForCreate  ->  Phase: Pending, free
our binder writes             ->  PodScheduled=False, FailedScheduling reason
                                 ...and later spec.nodeName + PodScheduled=True
```

Same reconcile loop in both states.

**Two requirements `--disable-agent` adds**, both from k3s source:

- `--egress-selector-mode`: use `cluster`. It is a static authorizer (node IPs plus cluster/pod
  CIDR) with no per-pod watch dependency. `agent` suffices for apiserver->kubelet but fails for
  service webhooks and aggregated APIs with a 10 s hang rather than a fast refusal. All modes
  produce byte-identical config files - the mode changes the TUNNEL, not the config.
- The identity bundle must be pre-seeded. See section 8.

## 4. The store

**Each cluster's store is `kine` over that cluster's own postgres.** k3s is started with
`--datastore-endpoint http://127.0.0.1:<kine lease>` (`clusterprovider.DaemonURL`) and dials kine as an
etcd endpoint; kine's `--endpoint` is the cluster's postgres DSN (`internal/providermain/main.go`
`storeEndpoint`: the per-cluster postgres registry, unless `SOCIALWEB_CLUSTERPROVIDER_STORE_ENDPOINT`
names an operator-supplied store for every cluster). The same postgres also holds the cluster vault's
storage (`internal/clusterstack/openbao.go`). k3s is never pointed at a bucket: measured against
k3s v1.36.4+k3s1, the S3 form is refused at startup with `unknown driver`, and a `t4` endpoint makes k3s
start its OWN kine against the bucket instead of dialing ours - two writers on one store
(`internal/clusterprovider/stack.go:39-43`, `ErrNoStore`).

**A central store is not used.** `kine` plus `t4` over a central `rustfs` (bucket per tenant, prefix
per cluster) is not built. The store is the per-cluster postgres above, and k3s is never pointed at a
bucket. No `rustfs` name is in the Go code or the pins; the only `t4` is in `stack.go`'s `ErrNoStore`
measurement.

**Several kine over one database are the k3s HA shape.** Measured on
stand-ins (`docs/research/2026-10-11-vm-lifecycle/control-plane-handover.md` 2-3): two k3s servers, each
with its own kine on one postgres and the same token, share CAs and service-account key and both serve;
kine's unique `(name, prev_revision)` is the compare, so a racing second write fails like an etcd compare.
Planned, not built: a k3s/kine upgrade starts a
second slot beside the first on the same postgres for the seconds of a swap behind a per-cluster front.
Section 1's "one apiserver per store" is the rule outside that overlap.

atproto is the durable **mirror**, not the datastore: it cannot be one. No compare-and-swap, no
durable watch (the sequencer resets `seq` to 0 on restart), no compaction, and every write is a
signed commit. What it carries is **intent and identity** - the `Cluster` CR mirror and account
records. Not data, and not portability: the export is deferred.

An empty `--datastore-endpoint` is refused. k3s would otherwise keep its objects in its own sqlite, so
a cluster that scales to zero and comes back comes back EMPTY. The join cannot reach that state:
`clusterstack.Spec.DatastoreEndpoint` is REQUIRED and `StepsFor` refuses an empty value, because
empty is the one value k3s resolves SILENTLY into a brand-new cluster.

**The store is durable against the launcher.** Durability rests on the data directory under
`SOCIALWEB_CLUSTERPROVIDER_STATE_DIR`. "The cluster is gone" means someone removed the data
directory - a caller decision, not an automatic consequence of a process exiting.

## 5. Components

Everything is either a controller-runtime reconcile loop or a deploy config. No new language.

This section lists the components. The table below is the virtual-clusters project, section 0's
traditional exception. It and the component subsections that follow are organized by section 0: each
application instance has its own kcp, manifest set and profile, its components run in process in the
core's component runtime (`internal/component`) and its third-party binaries are HostProcesses, and
listeners behind the edge bind port 0.

| # | Component | Layer | Why it exists |
|---|---|---|---|
| 1 | `Cluster` CRD - the mount object | Go | Carries `status.URL` and `status.phase`, which are the real contract. Owns `Draining`, `conditions[TenantPathShut]`, `conditions[Reachable]`, `status.nodeExpiry` |
| 2 | **Materialiser** - controller watching kcp with a host selector | Go | Places and stops each cluster's member units through HostProcess objects. This IS the gateway's supervisor |
| 3 | Cluster provider | Go | Owns the `Cluster` CR lifecycle: mints the instance token, fetches the identity bundle, sets `status.URL` once the apiserver answers. ONE process serves EVERY cluster in the kcp tree, and a cluster is named inside it by its home workspace, not by the bare name the owner chose |
| 4 | Node provider | Go | Watches the home workspace for intent, actuates one node, writes `Node` state |
| 5 | kcp provider workspace | YAML | `APIResourceSchema` plus `APIExport` for our CRDs; tenants `APIBinding` |
| 6 | k3s cloud-init module | Go `internal/atproto/cloudinit`, `UserDataModule` | Writes the k3s, secrets, iroh and node modules cloud-init runs in a market guest. The only sanctioned way a guest-side component arrives |
| 7 | OpenBao plus ESO | deploy config | The vault, at two levels: a root vault one per deployment and a cluster vault one per ON cluster, both in the production stack. OpenBao holds the per-cluster 13-entry identity bundle and is its AUTHORITATIVE store - the wake reads it out of the vault and materialises the file tree at `<DataDir>/server/tls`+`cred/`, because k3s has no vault plugin and must read files at runtime (sections 9, 14 and 15). ESO materialises vault-backed Secrets into the tenant cluster (row 9); the bundle is custody and wake-time restore, not the runtime location |
| 8 | Cluster links | Go, `internal/clusterlink` + `internal/clusterprovider/links.go` | Two `dumbpipe listen-tcp` members of every cluster's process group, one in front of the k3s apiserver and one in front of the cluster vault, each an iroh endpoint whose key lives in that cluster's vault. A node dials the cluster through them; the gateway holds no per-node connection. Recorded as `ClusterLink` objects |
| 9 | External Secrets pair | pinned upstream binary, `internal/eso` + `internal/clusterprovider/externalsecrets.go` | The External Secrets Operator v2.11.0 run as TWO members of every cluster's process group, beside the control plane: the controller (`external-secrets`) and its admission webhook (`external-secrets webhook`), one digest-pinned binary the release embeds as its `external-secrets` asset. They dial the tenant apiserver with a minted credential and reach the cluster vault on loopback, so an ExternalSecret syncs at ZERO nodes. The tenant keeps only the bundle's CRDs, ClusterRoles and a URL webhook registration; no ESO pod runs in it |

| 10 | Node network | Rust `rust/socialweb-meshd`, Go `internal/tenantnet` + `internal/atproto/cloudinit` + `internal/nodeagent` | Market clusters run `--flannel-backend=none`; each market node gets, from its `user_data`, a bridge CNI conflist on its Node's pod CIDR, cluster-CIDR-external SNAT, the sha256-pinned `socialweb-meshd` (downloaded from the gateway's web app by hash, `internal/nodeagent/host.go`) carrying pod traffic to other nodes over iroh, and `169.254.20.10` for its own CoreDNS; every tenant gets the meshd node reader RBAC and the per-node DNS DaemonSet as baseline (section 9) |

**Not building**, each rejected on evidence: a CNI plugin (the market path writes a conflist for
k3s's bundled `bridge`/`host-local`/`portmap`; the cross-node half is component 10), a CAPI provider, an OIDC issuer, an ATProto
bridge, a syncer, a virtual workspace, a second reverse proxy, a reachability substrate, an underlay,
a warm node pool, an admission plugin (RBAC already enforces the identity classes exactly). A
registry plus volume-commit machinery is not built either and is not rejected on evidence: its
only reason was S3-backed PVs on the central rustfs that is not used (section 4).

### The per-cluster slice of member units

Each cluster is a slice of member units. The cluster provider never forks a member: it writes one
HostProcess object per member into the gateway's own kcp (workspace `root` unless
`SOCIALWEB_CLUSTERPROVIDER_HOSTPROCESS_WORKSPACE` says otherwise), and the HostProcess supervisor in
the core places it on systemd-user (production) or under devsup (proc, dev). The reconcile loop is
still the only supervisor; there is no cluster-group verb and no Pdeathsig. A core or provider
restart or upgrade restarts no member.

```
<prefix>pg-<store>.service                 the cluster's Postgres object, unix socket only
<prefix>cluster-<key with - as _>.slice    one slice per cluster
  <prefix>c-<key>-bao.service              After/Wants the pg unit
  <prefix>c-<key>-front.service            no ordering, Restart=always, fd store (section 8)
  <prefix>c-<key>-kine@a.service           After/Wants the pg unit
  <prefix>c-<key>-k3s@a.service            After/Wants kine@a
  <prefix>c-<key>-ccm.service              After/Wants front
  <prefix>c-<key>-esowh.service            After/Wants front
  <prefix>c-<key>-eso.service              After/Wants front
  <prefix>c-<key>-link-apiserver.service   After/Wants front, Restart=always
  <prefix>c-<key>-link-openbao.service     After/Wants bao, Restart=always
```

- **Names.** `<prefix>` is `SOCIALWEB_CLUSTERPROVIDER_UNIT_PREFIX`: `socialweb-` in production, the
  dev instance's HostProcess prefix in dev (`dev env` prints it). The key's dashes become `_` in the
  slice name, so a dash in a key adds no slice level. The HostProcess object of a member is
  `c-<key>-<member>` (`@` becomes `-`), labelled `socialweb.computer/cluster` and
  `socialweb.computer/member`, with the finalizer `socialweb.computer/cluster-member`.
- **Wants, not Requires.** kine and bao want the pg unit and are ordered after it; a postgres crash or
  restart does not stop them. They also gate on the Postgres object being Ready before they are placed
  (`procgroup.ErrPostgresUnavailable`).
- **Bring-up.** `internal/clusterprovider/units.go` (`UnitsHost`) walks the steps in
  `procgroup.StackOrder` order and then the two links. For each member: run its Prepare, write its
  HostProcess, wait for Running, run its readiness gate against the member's log file, then record the
  gated invocation in the label `socialweb.computer/verified-invocation`. A member whose object is
  Running with that invocation is adopted and not touched. `<dataDir>/controlplane.json` records the
  active slot, the slice, the store unit and each member's unit and invocation.
- **Readiness follows member health.** A pass first asks the host which members are unhealthy: a
  missing object, a phase other than Running, or a running invocation that differs from the verified
  one (the member was restarted by its unit's restart policy). A Ready cluster with an unhealthy member
  is written `Initializing` with Ready=False, reason `MembersUnhealthy`, naming the members. The next
  pass re-places the missing or lost members and re-runs the gate of every unverified one (a restarted
  bao is unsealed there), then the cluster is Ready again once the apiserver answers. A killed link is
  therefore never hidden behind a Ready cluster.
- **Adoption.** On start the provider lists each cluster's member objects and logs them as adopted; it
  reaps nothing and spawns nothing over them. Leases are kept as long as the Cluster CR is.
- **Teardown.** Spec.running false, or a deleted Cluster: every member object is deleted, the
  supervisor stops each unit, and the provider waits until every member is Stopped before it drops
  the finalizers. A deleted cluster then loses its Postgres object (its data directory is kept); an OFF
  cluster keeps it.
- **Blue/green control-plane slots.** A cluster has slots a and b, each a
  `kine@<slot>` + `k3s@<slot>` pair on the cluster's one Postgres object, with its own leases
  (section 8) and its own k3s `--data-dir` (slot a: the cluster data dir; slot b: `<dataDir>/slots/b`);
  only the active slot runs. `controlplane.json` records `activeSlot`, the slots (k3s and kine
  executables, version, ports, data dir, hostname, units) and the front; `StepsFor` builds the active
  slot from it. `units roll cluster <key> --cluster-state-dir <dir> [--k3s-path] [--kine-path]` writes
  `<dataDir>/roll-request.json`; the provider's roller (`clusterprovider.Roller`) takes it when the
  cluster is Ready, under the cluster's lock (Spawn and Reap take it too), and writes
  `<dataDir>/rolls/<id>.json`, which the CLI prints. A roll: k3s minor in {active, active+1} and within
  three minors above every joined guest; the kubernetes EndpointSlice is only the front pair; idle first
  (waits while a non-kube-system pod is Running on a node, up to `--idle-bound`, default 30m; zero nodes
  swap at once); a minor bump takes a `pg_dump` of k3s and openbao to `<dataDir>/backups/<ts>`; the green
  slot's ports are re-leased if another process holds them (`Allocator.Relet`); kine@green is gated on
  its listener and "tables and indexes are up to date", k3s@green on its own port and the first
  kube-system Deployment and on the same server, client and request-header CAs as blue;
  `controlplane.json` is written with the green slot active BEFORE the front switches; then blue's
  k3s and kine objects are deleted, blue's front connections drained, and for 60 s the EndpointSlice,
  the kube-system lease holders and every Node's Ready transition are recorded in the report. Blue's data
  dir and its record stay for a same-minor rollback, which is the next `units roll cluster`. During a
  roll the cluster stays Ready: `Unhealthy` judges only the members `controlplane.json` records. Each
  k3s slot runs under its own hostname `swc-<key, 12>-<slot>` through `internal ns-exec` (util-linux
  `unshare --user --map-current-user --keep-caps --uts --mount`, then sethostname, a bind of
  `<slot data dir>/rancher` over `/etc/rancher`, ambient capabilities cleared, exec k3s; no resident
  code), on both backends, so wrangler's `kube-system/k3s` lease never has two holders under one name.
  The market kube-scheduler elects with a 4 s lease (renew 3 s, retry 1 s). Measured on a dev instance
  (k3s v1.36.4+k3s1, zero nodes): green ready 16-18 s after the request, authenticated `/readyz`
  through the front every 200 ms: 0 failures over both swaps; kube-controller-manager and
  k3s-cloud-controller-manager leases moved to green 22-26 s after blue stopped, wrangler's `k3s`
  lease 52 s (45 s lease, not released on stop).
- **Secrets.** A link's key reaches dumbpipe through `spec.envFiles` of its HostProcess: a 0600 file
  under the cluster's data dir. The key is never written into a kcp object.
- **The cluster's OpenBao.** The `bao` member is an OpenBaoServer object `c-<key>-bao`:
  unit `socialweb-c-<key>-bao.service` in the cluster's slice, After/Wants its postgres unit, postgres
  storage, the cluster's own vault TLS with client certificates optional. Its `spec.declared` is the
  cluster's whole vault state; the core syncs the vault to it. The cluster provider reaches the vault
  only as client `clusterprovider` (policy `clusterprovider`); `<dataDir>/openbao/access.json` names the
  address and the client certificate files, never a token.
- **The core binary as member code.** The ccm and the cluster front run the core's resolved
  executable, `<state>/bin/<sha>/socialweb-computer` on an installed host, so a core upgrade changes no
  code inside a running member and the upgrade's collection keeps that directory while the member runs.
- **Not yet.** Port attribution by cgroup (`internal/portscan`) is not needed while nothing reaps by port.

### The External Secrets controller runs beside the control plane

The External Secrets Operator runs beside the control plane, not inside the tenant cluster. Installed
into the tenant cluster as three Deployments (controller, cert-controller, webhook), it does not work:
at zero nodes, the resting state, their pods are Pending so nothing syncs, and on the market
substrate the store cannot reach the cluster vault at all. Running the operator in the process group
avoids both. The decisions:

- **Two members, one binary.** `procgroup.ComponentExternalSecretsWebhook` then
  `procgroup.ComponentExternalSecrets`, after the cloud controller manager in `StackOrder`. The
  binary is the upstream `ghcr.io/external-secrets/external-secrets:v2.11.0` image's own
  `/usr/bin/external-secrets`, embedded by digest as the release's `external-secrets` asset
  (`internal/versions/versions.json`); the webhook is
  its `webhook` subcommand. The cert-controller is not run: its only job was minting the webhook's
  serving certificate and injecting caBundles, which the provider now does itself.
- **The webhook step installs the tenant half, before either process starts.** Its `Prepare`
  (`internal/clusterprovider/externalsecrets.go`) mints the pair's tenant credential, mints the
  webhook's serving pair, and calls `eso.InstallBeside` with the cluster's admin client against the
  pinned bundle (the `external-secrets-bundle` asset, sha256-checked at release build).
  `eso.SplitBundle` sorts every bundle object three ways and refuses a kind it does not place: KEPT
  (the 25 CRDs and the ClusterRoles `external-secrets-controller`, `-view`, `-edit`,
  `-servicebindings`), RE-REGISTERED (the two ValidatingWebhookConfigurations), RETIRED (everything
  else). Retired objects are deleted first, then the kept ones are server-side applied under
  `socialweb-addons`, then the CRDs are awaited `Established`, because the controller starts an
  informer per kind and an informer for an unserved kind fails its start. The step runs at every
  spawn, so the install is also the migration.
- **Identity and RBAC.** The pair authenticates as `system:socialweb:external-secrets` (group of
  the same name), a client certificate minted from the cluster's CA exactly as the cloud
  controller manager's is, written to `<DataDir>/external-secrets.kubeconfig` (0600) and handed to
  both processes as `KUBECONFIG` and nothing else in their environment. A ClusterRoleBinding
  `socialweb-external-secrets` binds that GROUP to the upstream `external-secrets-controller`
  ClusterRole, so least privilege is upstream's own rule set rather than a fork of it. The
  upstream ClusterRoleBindings to the ServiceAccount `external-secrets` are retired, not kept:
  a binding to a ServiceAccount nothing runs as is one a tenant who creates that ServiceAccount
  inherits. The tenant grant's `socialweb-mint-identity` policy admits the beside username (its
  `__SOCIALWEB_ESO_USER__` placeholder) where it used to admit the in-tenant ServiceAccount.
- **The webhook is registered by URL on loopback.** Each webhook's `clientConfig` becomes
  `url: https://127.0.0.1:<leased webhook port><upstream path>` with `caBundle` the CA the provider
  mints per cluster (`eso.EnsureWebhookTLS`: CA 10 years, key under
  `<DataDir>/external-secrets/webhook-ca.key`, outside the cert dir the webhook reads; leaf 2 years,
  SANs `localhost` and `127.0.0.1`, renewed at a spawn inside its last year). A registration still
  naming a Service is deleted before the URL apply, because the field another manager owns
  survives a server-side apply and a webhook carrying both is refused. MEASURED: the k3s apiserver
  (`--egress-selector-mode cluster`) dials the loopback URL directly at zero nodes. The
  registrations stay `failurePolicy: Ignore`: the pair and the cloud controller manager are member
  units with `Restart=on-failure`, so a dead pair comes back, and a dead webhook under `Fail` would still
  refuse the provider's own store writes and deletes until then.
- **Ports and singletons.** The upstream defaults (`:8080` metrics, `:8082` controller health,
  `:8081` webhook health, `10250` webhook) are one set for the host, so each cluster leases three
  (section 8): the controller's `--live-addr` and the webhook's `--healthz-addr` bind `127.0.0.1`;
  metrics are off (`--metrics-addr=0`: nothing scrapes them, and a leased port
  nobody reads is a lease held for nothing); leader election is off, because the process group is
  already the singleton per cluster. The webhook's `--port` has no host flag and binds every
  interface (measured: `Serving webhook server` with an empty host); no public edge entry point
  forwards to it, and the listener answers only admission reviews under a CA no client but the tenant
  apiserver trusts.
- **Readiness.** The webhook's gate waits for its `/readyz` (which itself checks the serving
  pair) and then dials the serving port verifying the minted CA for `127.0.0.1`, which is what the
  apiserver does. The controller's `/readyz` is a ping that answers before any cache syncs, so its
  gate also waits for the `"Starting workers"` line of the `externalsecret` controller.
- **The store dials the vault on loopback, on every substrate.** `esovault.ApplyVaultStore`
  writes `server: https://127.0.0.1:<vault port>` and the vault CA; no substrate branch keeps
  the store off the market substrate.
- **Clusters leased before the pair.** `StackConfig.WithoutUnleased` drops the pair from the
  restart reap's argv derivation when a cluster's lease-file entry holds none of its ports; the
  bring-up that follows leases them, because the materialiser allocates every purpose at spawn.

Embedded-postgres is a special case: it is a Go **library** that runs the server as a child of
whoever links it, so there is no argv for a step and `processrunner.Start` refuses the empty argv
such a step would have. `procgroup.Options.PostgresUp func() bool` exists for this: the component
holding the library handle answers for it, and a **nil predicate is a refusal, not a skip**.

### Control-plane continuity across upgrades and restarts

The gateway's own state is kcp objects plus host records; the processes that serve a tenant are units
that outlive the core. Each kind of change is continuous by one mechanism, and none of them restarts a
VM or rents one:

- **Core binary** (`upgrade --binary --sha256`, `rollback`, a crash or `kill -9`): only the core unit
  restarts. kcp's store, kine, slots and front are bootstrap units, every cluster member is a unit of
  its slice, so they keep running; the new core adopts them by name and invocation. The cluster
  provider adopts every live claim and claims again a joined Node whose claim was lost; the web app
  resumes in-flight claims from the root vault and re-reads the joined ones. Nothing is written to a
  joined claim, no RFP is posted and no vm.delete is sent.
- **kcp** (`units roll kcp`): a new slot on the same store behind the kcp front, the controller lease
  handed over.
- **k3s and kine** (`units roll cluster`): a green slot behind the cluster's apiserver front, idle first.
- **The cluster links** (`units roll link`): a second listener with the same iroh key, then the old one
  stops.
- **Postgres, OpenBao, ESO, the CCM**: unit restarts; a restarted OpenBao is unsealed on its new
  invocation and synced to its declaration.

The members (the gateway XRPC and the cluster provider, in the core) reach kcp as kcp-admin through a
token file (`<kcp dir>/members.token`), because every kcp start issues a new kcp-admin token. The roll
rewrites the file from the new slot's `admin.kubeconfig` right after the front flips and before the old
slot stops; a member's client re-reads it on its first 401. The core checks the file against the active
slot every 5 s as a fallback and warns when it had to rewrite it.

Readings on hat-8, 2026-10-12 UTC: a staged release gateway (`install gateway --root`, systemd-user,
the core's PATH `/usr/bin:/bin`) with tenant cluster `gc` and one market node rented through the RFP flow
from a market network's `spec.kcp` bidder (guest a VM unit of test-0001), `/readyz` polled through the
cluster's front every second. `upgrade` 15:44:18: core members up 15:44:33, the VM unit's InvocationID,
the claim's uid and the node's Ready transition (15:25:17) unchanged. `units roll kcp` a to b
15:45:09-15:45:22: front flipped 15:45:15.86, lease handed over 15:45:21.6, probe Ready 0.53 s after; it
left the members Unauthorized (their kcp-admin token was the old slot's), fixed by the token file;
after the fix, roll b to a 16:00:51-16:01:02 left the cluster provider Unauthorized for 10 s (6 passes)
until the client re-read the file. `units roll cluster` a to b 16:02:24-16:04:56: the swap waited the
60 s idle bound (the workload pod), green Ready 80 s after start, front flipped 16:03:48.905, blue
stopped 0.79 s later with 303 open connections drained, the EndpointSlice never lost the front pair,
1 failed `/readyz` poll (16:03:49.288), no Ready transition. `units roll link` apiserver a to b
16:05:14-16:05:29: green Ready 10 s after start, the old listener stopped 0.2 s later, the node's lease
renewed through the new listener. A `rollback` to a build without the token file restarted the core
into a loop (bootstrap Unauthorized) while every member unit kept serving and the node stayed Ready.
Between two builds that both carry it: `upgrade` 16:22:45 (members up 16:24:10), `rollback` 16:24:10
(members up 16:24:14 on the previous binary), `kill -9` of the core 16:24:22.1 (systemd restarted it
16:24:27, members up 16:24:29.2). Over the whole run (15:25:22-16:25, 3392 polls): 1 failed `/readyz`
(the cluster roll above), no Ready transition of the market node, the same claim uid and receipt, the
guest's VM unit kept its InvocationID and pid, no RFP and no vm.delete after the first rental.

Stated limits: a user-manager restart, linger removal or a reboot removes every transient unit, so
every cluster cold-starts; the NotReady clock counts from when the path is back, so a node whose path
returns is kept, but it is not zero-downtime. The core's own
listeners (the gateway XRPC, its TLS front, the web app) are down while the core restarts.

### The cluster links

Each cluster has two links, one to the k3s apiserver and one to the cluster vault.

- **What runs.** Per cluster, `dumbpipe listen-tcp --host 127.0.0.1:<target> --ipv4-addr
  0.0.0.0:0` twice (`clusterlink.Argv`): target = the cluster's apiserver front port for
  `apiserver-link`, the vault's held port for `openbao-link`. dumbpipe v0.39.0, the same release
  the guests install (the `dumbpipe` asset in `internal/versions/versions.json`).
- **On or off.** On when `SOCIALWEB_CLUSTERPROVIDER_DUMBPIPE` names the binary; the gateway sets
  it to the extracted `dumbpipe` asset (`internal/stack/gatewaykcp`). Unset leaves every cluster without
  links, and the provider logs `cluster links on=false`
  once at start. Links need the vault in the Stack (`ErrNoLinkSeam`) and a recorder
  (`ErrNoLinkRecorder`).
- **The key.** One ed25519 key per link, generated once and kept in the cluster's OWN vault at KV v2
  mount `socialweb-links`, path `<service>` (`apiserver`, `openbao`), fields `secret` (64 hex, the
  32-byte seed -- exactly the form dumbpipe reads from `IROH_SECRET`; measured: dumbpipe prints
  `using secret key <hex>` when it generates one, and the ticket it prints for a given seed names
  `ed25519.NewKeyFromSeed(seed).Public()`), `endpointId`, `did`. The mount and the provider's policy on
  it are part of the cluster's OpenBaoServer declaration (`internal/clusterprovider/vaultdeclared.go`).
  The first write is check-and-set 0, so two writers can never both
  mint a key; a stored record that does not parse, or whose `endpointId` disagrees with its secret,
  is an error and is NEVER regenerated over (a new key is a new endpoint id and strands every node
  holding the old ticket). The key is read in the step's `procgroup.Step.Prepare`, which runs once
  the steps before it passed their gates, so the vault is up; the secret reaches dumbpipe only through
  a 0600 env file named in the HostProcess `spec.envFiles`.
- **Identity encodings.** `internal/didkey`: endpoint id = lowercase hex of the 32-byte public key
  (what iroh prints); did:key = `did:key:z` + base58btc(0xed 0x01 + key). Both matched the did:key
  spec vector and a dumbpipe-printed ticket when they were built.
- **The ticket.** dumbpipe prints `dumbpipe connect-tcp <ticket>`: `endpoint` + lowercase unpadded
  base32 of a postcard-encoded `EndpointTicket` (byte 0 the variant 0x00, bytes 1..32 the endpoint
  id, then the relay URL and direct addresses). `clusterlink.ParseTicket` reads the id back, and the
  gate opens only when the printed ticket names the vault-held key. The printed ticket's BYTES change
  per bring-up (the relay chosen and the addresses are in it), so it is NOT what is published:
  `status.ticket` is `clusterlink.StableTicket(key)`, the id-only ticket (variant 0x00, the id, zero
  addresses) -- byte for byte what `IROH_SECRET=<seed> dumbpipe generate-ticket` prints -- so it is a
  function of the key alone and never changes while the key lives in the vault. A node
  secret may hold it written once. dumbpipe resolves the id's current relay
  and addresses by discovery; measured live against a real dumbpipe (connect-tcp with the published
  ticket echoes before and after a restart, and the ticket is byte-identical). Decided by Claude, user may veto: the cost is that a dialer with no discovery
  (no DNS to the n0 discovery service) cannot use the ticket; guests reach the internet anyway.
- **The link port.** Not held: dumbpipe binds UDP port 0, and a node dials the link by its ticket,
  so the port number is in no ticket that is published (section 8). The `apiserver-link` and
  `openbao-link` port purposes are retired (`portallocator` `retiredPurposes`).
- **Status.** The link's readiness gate writes the `ClusterLink` `Listening` (spec
  cluster/service/targetPort, status endpointId/did/ticket) with retries, so a kcp outage never reaps
  a cluster. A dead link is a member unit with Restart=always; while it is down or not yet re-verified
  the cluster is not Ready (section "The per-cluster slice of member units").
- **Teardown.** A deleted cluster's two ClusterLinks are deleted after its group is reaped (both the
  all-clusters path and a pass that removes the finalizer). An OFF cluster keeps them: it comes back as
  the same endpoints. The keys stay in the vault's data and go with it.
- **Replacement without a gap.** Each link has two slots; `controlplane.json`
  `links.<service>` names the slot (`a` or `b`) and the dumbpipe it runs, and `StepsFor`
  builds the link from it (member `link-<service>` for slot a, `link-<service>@b` for slot b). `units
  roll link <key> --service apiserver|openbao [--dumbpipe-path]` is carried out by the provider's roller
  (`Roller.RollLink`): the new listener starts in the other slot with the same `IROH_SECRET` env file, its gate waits for the ticket it prints naming the vault's key (and
  records the ClusterLink, whose `status.ticket` is `StableTicket(key)` and so byte-identical), the
  record is switched, then the old listener's object is deleted. A link is never re-pointed for a k3s
  swap: its target is the front. Measured with the pinned dumbpipe v0.39.0 (iroh 1.0.0, sha256
  81010d0a... of the binary in the pinned archive; n0's default relays; client and listeners on one
  host, so the path was direct): a connect-tcp client dialling the endpoint-id-only ticket keeps its
  dials on the OLDER listener while both run (the newer process is not promoted for an established
  client), and its next dial after the older one is stopped reaches the newer one with no dial lost
  (200 ms polls); a client that first dials while both run reaches the newer one only (the address
  record is last writer wins; the newer listener's first answer came 5.2 s after it started). So the
  replacement keeps start-then-stop; no fallback to stop-then-start was needed. On a dev instance a
  replacement (old object deleted 0.17 s after the new one was gated, 8 s after the request) and a
  SIGKILL of the link (Restart=always, a new process 0.19 s later, cluster Initializing for 4.2 s while
  the restarted invocation was re-verified) each lost 0 of ~260 kubectl polls through a connect-tcp
  client on the published ticket.
- **Stated limits.** A ticket carries no direct address, so a dialer reaches the link only through
  discovery and the relay; the IPv4 and IPv6 sockets both take a random port.

### The vault's config is rendered, not shipped

`internal/openbaoctl` renders the config from the cluster's OpenBaoServer spec (`rootvault.ServerConfigWith`:
the same listener and postgres storage, client certificates optional) into its own state dir, mode
0600, rewritten only when it changed, before it ensures the HostProcess that runs `bao server
-config=...`. `clusterstack.Spec.OpenBaoServerConfig` states the same rules for a cluster spec. Each
line is measured on v2.6.2:

```hcl
ui = false
disable_clustering = true
api_addr = "https://127.0.0.1:<leased openbao port>"
listener "tcp" { address       = "127.0.0.1:<leased openbao port>"
               tls_cert_file = "<data dir>/server/tls/openbao.crt"
               tls_key_file  = "<data dir>/server/tls/openbao.key" }
storage "postgresql" {
  connection_url = "postgres://<user>@localhost/openbao?host=<the pg unit's socket dir>&sslmode=disable"
}
```

The listener is loopback on the cluster's leased port, and the port is NEVER defaulted: bao's own 8200
is one port for the whole host, which is the store's byte-identical-DSN defect class. The listener is
TLS against the cluster's own certificate and key, and a spec with either absent is refused: a
listener with no certificate is an ACME/plaintext listener whose
`GET /v1/sys/health` never answers the readiness gate. `disable_clustering = true` removes the cluster
listener (default host-global 8201) and makes "one vault per cluster, no HA" a property of the file.
The file carries **no `disable_mlock`**: 2.6.2 logs `unknown or unsupported field disable_mlock` and
starts anyway, because it does not mlock at all. The config file must be inside the cluster's data
dir, and the certificate and key paths are per-cluster: a shared config is written by whichever
cluster reconciles last, so its port becomes that cluster's.

Storage is the **postgres backend**, and each word of that stanza was read off the bundled `bao`
rather than remembered: `storage "postgres"` answers `unknown storage type postgres`, `storage
"postgresql"` with the cluster's DSN answers, and the health body's own `Storage Type` field then
reads `postgresql`. The key is `connection_url`. The URL is ALWAYS this cluster's own DSN -- the unix
socket directory of the cluster's Postgres object (`pgunit.DSN`) -- and NEVER a defaulted one,
because two clusters' DSNs are otherwise byte-identical. It names the vault's OWN
database, `openbao` (`clusterstack.OpenBaoDatabase`); kine's objects go to the database `k3s`, so the
vault's tables and the objects kine writes never share one database. `OpenBaoServerConfig` refuses a
DSN that is empty, names no unix socket directory, carries no user, is not a postgres URL, or names
any database but `openbao`; `openbaoctl` refuses a `spec.storage.postgres` without an absolute
socketDir, a user and a database. The database itself is created empty (`internal/pgunit`) before the
vault starts (bao creates its own tables); measured on 2.6.2, the first vault write creates
`openbao_kv_store` in `openbao` and nothing in `k3s`. The file backend is not used: 2.6.2 logs it
as deprecated (`storage.file: the file physical backend is deprecated; use bao operator
migrate ... by v2.7.0`) and section 6 decides against it.

### A child's output is drained, never left in a pipe

Every cluster member is a HostProcess unit. Its stdout and stderr go to one log file,
`procgroup.Spec.Log`, set as the HostProcess `spec.output` (`internal/clusterprovider/units.go`):
systemd-user appends to it (`StandardOutput=append:`), devsup opens it itself. No member writes into
a pipe, so a member may write without bound and still reach its exit.

The rule dates from the in-process child runner that came before the HostProcess units, where a
pipe nobody read made the child block inside `write(2)` the moment the kernel's 64 KiB buffer
filled. `k3s server` and `k3s agent` both emit far more than that during startup, so the block lands
mid-boot and the readiness gate reports `k3s server at https://...: no answer` - a readiness fault over
a child that was alive, healthy, and had nowhere to put its log. The failure is also load-dependent
and therefore intermittent, which is the worst property a startup fault can have. Measured: a child
writing 500 KB never finished under a pipe; with a writer it finishes immediately.

The log is bounded: the HostProcess output cap is `MemberOutputCap` (64 MiB, enforced by
`internal/hostprocess` through `internal/logcap`), and a new HostProcess object for the member moves
the old log to `<log>.prev`. **The bound is the point** - a cluster's k3s logs for the life of the
cluster, and a log that grows with it is a leak on the gateway host. A readiness gate reads only the
last 1 MiB of the file (`unitMember.Output`, `memberOutputTail`). The tail is also what a positive
assertion about a node reads: `portallocator.AssertAgentDialledOurPort` needs the agent's own dial
line, and a tail that no longer holds it fails loudly rather than passing on an absence.

### A market bidder is an application instance of one installed account

A bidder host is set up by the one root step `socialweb-computer install bidder`, which creates the
lingering `swc-bidder` account, its subuid/subgid range for per-VM user namespaces and the kvm group
membership, and ends by running `check-host -role bidder` as the account (`docs/host-contract.md`).
Each bidder is then `socialweb-computer run bidder`: it applies its MarketBidder root object with
`--profile prod-qr|market-network|dev-local` into the handle's own kcp, so every handle is its own
application instance (section 0). Its guests are VM units of `internal/vmruntime` under the account's
user manager, born only from the cloud-init `user_data` of the RFP flow. There is no systemd system
template unit per handle, no docker group account and no runner containers.

### Host processes and the VM lifecycle

This subsection describes the design for host processes and the VM lifecycle. It is built
(`internal/hostprocess`, `internal/hostsvc`, `internal/devsup`, `internal/vmruntime`, `internal/install`),
except where a line says it is not. Lines marked "user" are the user's decisions; the rest are
Claude's, open to the user's veto in the plan files.

**Invariant: no socialweb-computer code in a running VM's process tree (user).** The service manager
launches the VMM; our code only does one-shot prep before the unit starts, and the tiny `socialweb-vmexec`
helper creates the tap and `execve`s firecracker, so after start a VM unit's cgroup holds exactly `pasta`
and `firecracker`. There is no `socialweb-nodeboot` and no resident namespace verb; the firecracker
provider's only path is the VM unit of `internal/vmruntime`. The instrument that proves it (every
process in a VM unit's cgroup must hash to a pinned asset) exists as
`socialweb-computer units audit` (`internal/hostsvc/treeaudit`), which exits 1 naming the unit and the
executable that is not pinned. Running it after every boot, restart and upgrade is not built: only the
verb runs it today.

**Scope and platforms (user).** Headless servers; VMs survive a crash or upgrade of the core binary and a
logout; a reboot is Lost (transient units live on tmpfs). This runs on Linux only; the cross-OS seams are
built now: Go interfaces (`hostsvc`, `vmruntime`, `guestnet`), GOOS-tagged files, per-OS
embedded assets compiled in only for the target OS, and a check.sh gate that builds and vets darwin and
windows on every run. Windows (Hyper-V HCS, SCM) and macOS (vfkit, launchd) backends follow. The live proof of this design ran
on hat-8 with systemd 259.9 and firecracker v1.13.1.

**Where processes live (user).** The gateway and the bidders each run in the USER manager of a dedicated
lingering account (`socialweb`; one shared `swc-bidder` account for every bidder handle, units namespaced
by handle). Long-lived children are transient SERVICES (never scopes) started over
`$XDG_RUNTIME_DIR/systemd/private`: a VM per `socialweb-vm-<hid>-<name>.service`, the vaults
(`socialweb-bao-root`, `socialweb-bao-<hid>`), each cluster's postgres (`socialweb-pg-<key>`) and its
member units in `socialweb-cluster-<key>.slice` (kine and bao `After=` and `Wants=` the postgres unit, not
`Requires=`), and
kcp: on the gateway as alternating blue/green slot units (`socialweb-kcp-gw@a|b`) of the unpatched
kcp asset built here from kcp-dev/kcp main (commit 74e9fb9c, versions.json assets.kcp, fetch kind gosrc;
the user decided 2026-10-22, plan 0026 W0-RUNTIME, to run kcp off main), each on its own kine over a dedicated postgres unit on a unix socket
(`socialweb-kcp-gw-pg`, not a CRD object), behind our own L4 front (`socialweb-kcp-gw-front`,
127.0.0.1:6443); on a bidder as ONE slot per handle
(`socialweb-kcp-<hid>@a`) on one kine over sqlite behind a port-0 front, upgraded by a short restart that
VMs never notice, after which the controller re-adopts them. Postgres and OpenBaoServer
stay CRDs (user). Components run inside the core process and stop with it. The core unit
uses the default `KillMode=control-group`; transient units are separate cgroups, so no KillMode reaches
them (proved: a unit survives restart, kill -9 and stop of its starter). Only cpu, memory and pids
delegation is relied on (user); `AllowedCPUs` and `IOWeight` are not enforced in a user manager and are
not used; VM units set `MemorySwapMax=0` with `MemoryMax` (otherwise the guest swaps) and live in their
own slice, outside the `app.slice` Fedora's systemd-oomd watches. Minimum systemd 253 with each property
probed by `check-host` (user).

**One explicit root step (user).** Production setup is `socialweb-computer install
bidder|gateway`, idempotent, with `uninstall`: it creates the account (never created on demand anywhere
else), for bidders one subuid/subgid range and kvm membership, the `user@` cpu delegation drop-in, the
AppArmor profile for the embedded pasta where AppArmor restricts user namespaces, the gateway's
`/etc/rancher/node/password`, the content-addressed binary, `socialweb.slice`, the core unit file and the
bootstrap-layer unit files (kcp's store, kine, slot(s) and front), then linger (after the group and the
drop-in, so the user manager starts with them) and `host-backend: systemd-user` in the account's config.
Every other unit (the vaults, cluster slices and members, VM units) is a transient unit placed at runtime
from a HostProcess object and adopted by name.

**The gateway's install and upgrade.** `install gateway` writes the user unit
`socialweb-computer-gateway.service` (`ExecStart=<state>/bin/current/socialweb-computer serve gateway kcp`,
`Restart=always`, default `KillMode=control-group`, optional `EnvironmentFile=-<state>/gateway.env`) and a
`config.json` that `serve gateway kcp` reads with nothing added by hand: the state dir, `host-backend:
systemd-user`, the asset cache, and the two front addresses caddy proxies to (kcp front 127.0.0.1:6443,
TLS front 127.0.0.1:8443; the web app front's 127.0.0.1:8788 is the prod Gateway spec's default), plus
`unit-prefix` and `gateway-spec-file` when install is given them. A release gateway takes the clusterprovider's external-secrets
binary, its bundle (the embedded asset `external-secrets-bundle`, the release's `external-secrets.yaml`),
dumbpipe, and every tenant cluster's k3s, kine, bao and pg_dump from its own assets, each by the absolute
path under `runtime/<sha256>/`; a release clusterprovider never looks in PATH (a dev build names k3s with
`k3s-path`). `upgrade --binary --sha256` checks the digest, copies the
binary to `bin/<sha>/`, runs its self-check, retargets `bin/current` (the old target becomes
`bin/previous`) and restarts the core unit; every installed and transient unit is adopted, only the core
gets a new InvocationID. `rollback` swaps `current` and `previous` and restarts. After either, `bin/<sha>/`
directories are collected except `current`, `previous`, any directory a running process executes from
(`/proc/<pid>/exe`) and any a unit file names (the account's unit dir and the user manager's runtime and
transient unit dirs). Member code that is the core binary (the ccm and the cluster front) runs the core's
resolved executable, `bin/<sha>/socialweb-computer`, never `bin/current`, so a member keeps its code across
an upgrade and its directory is kept while it runs. `--root <dir>` stages an install for a proof run: the
host files (`/etc`, the linger file, the install record) go under `<dir>`, the state dir is used as given,
the account is the invoking user and the units run in that user's own manager; `--unit-prefix` names every
unit of the installed application, the core included (`<prefix>computer-gateway.service`).

**Each application instance's kcp as its service manager; dev run mode (user).** Long-lived
host processes are `HostProcess` objects in their application instance's own kcp (section 0) reconciled by a
supervisor inside the core (`internal/hostprocess`) onto a placement backend: `systemd-user` (transient units in the account's
user manager, production installs only) or `proc` (dev: a plain supervised process tree under
a per-instance `devsup` that is every unit's parent and child subreaper). Postgres, OpenBaoServer, each
cluster member and each ComputeVM own HostProcess children. Below kcp sits the BOOTSTRAP LAYER - kcp's
store, kine, slot(s), front and the core - which kcp cannot supervise: installed systemd units in
production, devsup units in dev, mirrored into kcp read-only once it answers (the static-pod/mirror-pod
pattern). `host-backend` is required with no default. `socialweb-computer dev up|down|status` runs one
instance per worktree with no root, no install and no systemd: state under
`$XDG_STATE_HOME/socialweb-computer/dev/<instance>/`, its own kcp on port 0 (gateway store sqlite for
quick runs, postgres for the market network and prod-like profiles), leases keyed by instance, the k3s node password
bind-mounted in a per-instance user namespace, AppArmor reported not fixed, one uid for every role.
Instances nest as kcp workspaces (one kcp per application instance; a parent instance that holds its
children's processes later; applications never share a kcp, section 0). Guests in dev still come
up only through the RFP flow.

**Adoption is a tri-state verdict (user).** A restarted core adopts units by name, never re-creates them.
Identity is read from systemd and the kernel (unit, InvocationID, VMM pid and start ticks through a pidfd,
`GetUnitByPIDFD`, firecracker's own `--id` from `GET /`), never from a file the child wrote. Only bus
dial, auth or timeout errors and an unreachable API socket are Unknown (retried, never Lost). A changed
boot id is Lost. A definite mismatch on an active unit is Lost and the unit is stopped at once.

**Guest side (user).** Every guest gets the identical address plan inside its own pasta namespace and
dials a constant host address, so there is no network-index allocator and user_data is final at
create. Guest images (kernel + rootfs) are built per hypervisor and arch by a build-host script that runs
on any Linux host (CI shells out to the same script), pinned by sha256 and
fetched; bidders need no root, chroot, apt or Go toolchain. The seed is a separate pure-Go NoCloud cidata
drive, the rootfs is copied as-is and the guest grows its own root, so no host runs debugfs, e2fsck or
resize2fs. Guest SSH host keys are generated in the guest and reported by it, with proof
of possession, in a bidder-signed `vm.hostKeys` record replicating `vm.onNetwork`; every ssh to a guest
verifies them with `StrictHostKeyChecking=yes`, and no host scans a guest. The guest console goes to a capped file in
the VM's work dir and is deleted with it; tenant data never enters the host journal. Stop is graceful
first: `SendCtrlAltDel` over the API socket, a grace window measured live through the RFP flow, then
`KillSignal=SIGKILL` (firecracker, PID 1 of pasta's PID namespace, ignores SIGTERM); arm64 goes straight
to SIGKILL. The Bidder component runs inside the bidder's core, which shares a uid with the user
manager it places units in; the VM units are what is isolated (below).

**VM isolation without on-demand accounts (user).** The bidder account is shared by every
handle, but each VM unit runs pasta and firecracker in its own user namespace mapped to a distinct 65536-id
sub-range of the account's reserved subuid/subgid range (the rootless-podman pattern through `newuidmap`),
with access to exactly its own work files by ACL, so a VMM escape lands in an unprivileged uid that owns no
bidder file.

**Stated limits.** Handles on one bidder host share a uid for their cores, and nothing but that uid
separates a bidder's core from its user manager (the deno permission sandbox of the TypeScript bidder
before plan 0026 is gone with it). Restarting
`user@<uid>`, removing linger or `loginctl terminate-user` stops every unit and is treated as a reboot.

**Control-plane continuity across upgrades (user).** There is no cutover and no container path: the
binary is deployed fresh. No backwards compatibility, migration, rollback or prod-downtime handling is
planned before the finished solution exists, and the gateway runs no containers. Continuity is for
upgrades of the finished system:
a per-cluster TCP front owns the existing apiserver port (guest tickets, node secrets, kubeconfigs and
`status.URL` never change); a k3s/kine upgrade starts a new slot beside the old on the same postgres,
the front swaps, the old stops, idle clusters first; the market
kube-scheduler elects a leader with a short lease and each slot has its own hostname so k3s's own leader
lease never has two holders; a cluster link keeps its iroh key across restarts; a
restarted gateway (the NodeClaim controller, `internal/controllers/nodeclaim`) resumes Bidding and Provisioning claims from a per-claim secrets-server key in the cluster
OpenBao; and NODE-LOSS counts only time when our own path was up. Design and measurements:
`docs/research/2026-10-11-vm-lifecycle/control-plane-handover.md`.
The gateway's kcp upgrades follow the same pattern behind our own front, not caddy (a
bidder's single kcp slot is restarted instead): slots run
`--enable-leader-election` and `--shard-base-url` = the front, a new slot gets the five shared root files
and starts beside the old on the same kine -> postgres store, the front flips after `/readyz` on the new
slot's own port, the old slot exits, and the Lease `kube-system/kcp-controllers` is deleted so the new
slot leads at once (61 s gap without it, measured); a pg_dump precedes every roll, and kcp release pairs
whose storage versions or bootstrap schemas differ take a planned short outage instead
(`kcp-blue-green.md`). caddy stays public TLS only, is never reconfigured by a
swap, and any caddy config we render pins `install_trust false`.

### compute-provider-kcp: a ComputeVM per guest, reconciled by a Go controller

This subsection describes the compute-provider-kcp design. It is built in Go: the Bidder component
(`internal/services/bidder`) writes a ComputeVM per guest (`computevm.ComputeVMFor`), the ComputeVM
controller (`internal/controllers/computevm`) and the FirecrackerVM controller
(`internal/controllers/firecrackervm`) reconcile it over `internal/computevm`,
`internal/compute/firecracker` and `internal/vmruntime`. The host mechanics it relies on are the
subsection above. The TypeScript provider that came before plan 0026 is gone.

- **The bidder is an application instance with its own kcp (user).** A bidder
  handle = one kcp (one slot on kine-sqlite behind a port-0 front) + the bidder manifest set + a profile,
  rooted at a MarketBidder object. Its guests are ComputeVM objects in that kcp; two handles on one host never
  share a kcp. Requesters and the gateway never read a ComputeVM: what crosses applications is a market record
  (RFP, bid, accept, receipt, onNetwork, vm.hostKeys, vm.terminated). A bidder kcp upgrade is a short restart
  of its one slot that VMs never notice; the controller re-adopts every VM afterwards.
- **ComputeVM plus a Go controller (user).** The Bidder component writes a ComputeVM
  (`computevm.ComputeVMFor`, from `internal/market/bidder` through `internal/services/bidder`); the
  ComputeVM controller prepares, places, adopts and releases it through its backend, a FirecrackerVM
  named in `spec.backendRef`. Spec: backendRef, vm, owner.handle, userData (a SecretRef into the
  bidder's vault at `guests/<name>/user-data`), requester, contract, accept, image, stopGraceSeconds,
  guestLogin, release; no network index. Status is OS-neutral (phase, lostReason, runtime {backend as a
  free string, ref, instance, pid, startTime, hostBootID, vmmID}, network, image, conditions, market) so
  the Windows and macOS backends need no schema revision.
- **What the bidder reads back.** The bidder keeps no guest state of its own: it reads its ComputeVMs
  and Bid objects from its kcp, and a restarted bidder carries on from them. A release is the
  ComputeVM's `spec.release`, written by the bidder from the requester's `vm.delete`. A ComputeVM that
  ends is reported by the ComputeVM controller as a `vm.terminated` StateSync
  (`computevm.EnsureTerminationSync`, reason `computevm.GoneReason`: HostRebooted -> host_reboot,
  OOMKilled -> oom, IdentityMismatch -> identity_mismatch, GuestPoweroff -> guest_poweroff, every
  other -> vmm_exited; `deleted_on_request` for a release). The guest-facing endpoints (the allow-list
  proxy to the bidder's OpenBao, `POST /v1/events`, `GET /v1/downloads` and the guest event
  subscription) are the GuestControlPlane component
  (`internal/services/guestcontrolplane`).
- **A process per VM under a generic runner was rejected, on evidence.** Before plan 0026 a runner of
  the deno runtime SIGKILLed a deleted pod's process group with no release step, and its 5-minute pod
  timeout killed the market network's bidder mid-contract (run 4), while a VM lives as long as its
  contract and needs typed guest fields and `/dev/kvm`. A VM is a ComputeVM with its own controller.
- **ComputeVM is the source of truth.** A bidder that held guests and the network index in memory
  lost its receipts and leaked guests on a restart (the TypeScript firecracker provider before plan
  0026). Now the contract is ComputeVM `spec.contract`, and a restarted bidder re-populates from its
  ComputeVMs plus live VMM state.
- **No socialweb-computer code in a running VM's process tree (user).** Prep is one-shot in the
  controller; each ComputeVM owns exactly one HostProcess (`computevm.ChildName`, `vm-<name>`) placed as a
  transient unit `socialweb-vm-<hid>-<name>` whose tree after start is pasta -> firecracker only:
  on `systemd-user` in production, in the user manager of the one shared lingering `swc-bidder`
  account, surviving core crash, upgrade and logout; on `proc` in dev, as a unit of
  the per-instance `devsup`, the parent and child subreaper of every unit (`internal/devsup/server_linux.go`).
  devsup in a dev tree is the dev backend by design,
  never production. Both backends satisfy `hostsvc.Manager` (`internal/hostsvc/hostsvc.go`, the
  `internal/contract/host` Units interface). Reboot = Lost.
- **Tri-state verdict: Running | Lost | Unknown (user).** `hostsvc.Verdict` and `LostReason`
  (`internal/hostsvc/hostsvc.go`, from `internal/contract/host`). Bus dial, auth or timeout and an unreachable API socket are
  Unknown: a condition, phase unchanged, retried, never Lost. A changed boot id is Lost(HostRebooted); a missing
  unit Lost(UnitGone); a failed or inactive unit Lost(UnitFailed | OOMKilled | VMMExited); an identity mismatch
  on an active unit (InvocationID, pidfd + start ticks, `GetUnitByPIDFD`, firecracker `GET /` id) is
  Lost(IdentityMismatch) and the unit is stopped at once; a unit placed by another backend is Lost,
  never adopted. Why: a false Lost costs a tenant a VM.
  A Lost guest is never booted again; the requester replaces it through a new RFP (user).
- **Stop is CtrlAltDel, then SIGKILL (user).** firecracker is PID 1 of pasta's PID namespace and
  ignores SIGTERM, so a plain stop hangs 45 s. Releasing sends `PUT /actions SendCtrlAltDel`,
  waits `stopGraceSeconds`, then the unit stops with `KillSignal=SIGKILL`; arm64 goes straight to SIGKILL.
  Measured live through the RFP flow: 0.21 s for an idle guest. The finalizer is dropped only after
  the work dir and API socket are gone (32 dirs and 63 GB leaked on hat-8 without it).
- **Per-VM subuid user namespace (user).** `install bidder` reserves one subuid/subgid
  range for the shared account; each VM unit runs pasta and firecracker in its own user namespace mapped to a
  distinct 65536-id sub-range through `newuidmap`, so a VMM escape lands in a uid that owns nothing. No Unix
  account is created on demand.
- **cidata seed (user).** The seed is a separate pure-Go NoCloud cidata drive; the rootfs is
  reflink-cloned as-is and truncated to the contract's disk; the guest grows its own root. No host runs
  debugfs, e2fsck or resize2fs.
- **Identical guest network per pasta namespace (user).** Every guest gets the same address plan
  inside its own pasta user+net namespace and dials a constant host address (pasta's gateway), so the
  network-index allocator of the TypeScript firecracker provider before plan 0026 is gone and user_data
  is final at create.
- **Guest images from a build-host script (user).** Kernel + rootfs per
  hypervisor x arch, built by a script on any Linux host (CI shells out to it), pinned by sha256 and
  fetched; a bidder needs no root, chroot, apt or Go toolchain.
- **iroh is the default and required guest transport (user; relaxed 2026-10-24).** The bidder installs
  no guest package registry, websocat path, tunnel or fedproxy module. The requester may ask, per
  contract, for the opt-in opentunnel module (`spec.tunnels`, plan 0032); its user_data
  installs it, and iroh stays on every guest. Guest SSH host
  keys are guest-reported in `vm.hostKeys` and verified with `StrictHostKeyChecking=yes`.
- **vm.terminated (user).** The bidder reports why and when a guest ended in a signed
  `com.publicdomainrelay.temp.compute.events.vm.terminated` inside the existing market.event envelope
  (5.6 s
  from guest poweroff to the requester's session end, measured live). The ComputeVM controller emits it
  from every Lost verdict and every release through a termination StateSync (`internal/computevm/termination.go`,
  reasons as above); an ended ComputeVM answers a late `vm.delete` 200. vm.terminated and vm.hostKeys are the only protocol additions, with the optional `tag` of
  vm.onNetwork (plan 0032).

## 6. CRDs and the API surface

Every CRD here is INTERNAL. No CRD is user-facing: the user's surface is the mounted Kubernetes API
(standard kinds), and cluster lifecycle is an **XRPC call**. None of these schemas carries a
compatibility promise.

| CRD | For | Writer | Tenant-visible |
|---|---|---|---|
| **Cluster** | the mount object - `is-mount` annotation, `status.URL`/`phase`, `Draining`, conditions, `status.nodeExpiry` | cluster provider, from an XRPC lifecycle call - never a kubectl apply | no |
| **Vault** | OpenBao, per ON cluster over its own embedded-postgres | system | no |
| **Postgres** | embedded-postgres - the vault's store and the one per-cluster local state | system | no |
| **Kine** | not a CRD today: kine is a member of each cluster's process group over that cluster's own postgres (section 4; the "kine plus t4 to central S3" row was the unbuilt rustfs design) | system | no |
| **Node** | one node. The only bound on leaks | node provider | no |
| **NodeClaim** | one node RENTED FROM THE COMPUTE MARKET for a cluster, as that cluster's owner. Carries paths (`tokenFile`, `nodePasswordFile`, `serverCAFile`) and never a secret; the NodeClaim controller is the only writer of its `status` | cluster provider, spec and metadata only | no |
| **ClusterLink** | one iroh listener in front of a cluster's apiserver or vault, named `<clusterKey>-apiserver` / `<clusterKey>-openbao` in the cluster's home workspace. `spec` {cluster (the key), service, targetPort}; `status` {phase `Pending`/`Listening`/`Failed`, endpointId (64 hex), did (did:key), ticket (id-only, a function of the key: byte-stable), message}. Never carries the secret | cluster provider, spec and status | no |
| **AtprotoIdentity** | the DID to identity mapping, in the home workspace | us | no |
| **Account** | the DID to shard map. Not a CRD - a real kcp `Workspace` of an account `WorkspaceType` in root | us | no |

Do **not** use kcp's shipped home (`~`): it is a bare LogicalCluster with no owner and no
`directlyDeletable`, and `LogicalCluster` delete is Forbidden without that field, so the shipped home
can never be deleted.

`ClusterPhase`: `Initializing`, `Ready`, `Unavailable`, `Draining`.
`NodePhase`: `Pending`, `Running`, `Draining`.

### Generated from api/v1alpha1

The Go types in `api/v1alpha1` are the ONLY source of every CRD's shape. `go generate ./api/...`
(`cmd/socialweb-apigen -root .`, logic in `internal/apigen`) writes, and nothing else may write:

| Output | What |
|---|---|
| `deploy/<kind>-apiresourceschema.yaml` | one kcp `APIResourceSchema` per entry of `v1alpha1.APIResources` |
| `deploy/<export>-apiexport.yaml` | one `APIExport` per distinct `Export` (cluster, atprotoidentity, computecontract, computevm, hostprocess, marketidentity, ...), resources in registry order |
| `deploy/apiresourceschema-revisions.txt` | the revision lock: every schema name ever generated and the sha256 of its `spec`, one `<name> sha256:<hex>` line each, in the order they were added (the generator keeps the file order and appends new names); `.gitattributes` merges it with `merge=union`, so two branches that each add a revision never conflict and the merged order needs no regeneration, and a name recorded twice with two hashes is a generator error |
| `api/<group>/v1alpha1/zz_generated.register.go`, `zz_generated.conditions.go` | the scheme registration and the condition helpers of each API group |

How the Go type maps, decided by Claude (user may veto):

- Struct layout comes from reflection over `encoding/json` tags, so the schema is what Go actually
  writes. A field is `required` exactly when its json tag lacks `omitempty`, unless it carries the
  struct tag `apigen:"optional"` (only `ClusterSpec.running`, which is written as `false` and was never
  required). Struct tags, because the no-comments rule forbids kubebuilder markers and so rules out
  controller-gen.
- Enums are read from SOURCE (`go/parser`): a named string type's enum is its typed string
  constants in declaration order. A named string type with no constants
  is a generator error, so an enum cannot silently become a free string.
- `metav1.ObjectMeta` is `type: object`; `[]metav1.Condition` items are `type: object` with
  `x-kubernetes-preserve-unknown-fields`; `int` is `integer`, `int64` adds `format: int64`. A named
  type outside the group's own package is a generator error.
- `apigen.Model.Validate` checks a value by the same rules as the schema plus one kube does not apply:
  an UNKNOWN field is an error, not pruned. `metadata` and conditions stay open.

The first generation reproduced the hand-written Cluster, Node and AtprotoIdentity schemas and both
exports BYTE FOR BYTE; NodeClaim differs only by its name and the added `status.contract` fields.

`deploy/check.sh` (every mode) regenerates into a temp dir and `diff -u`s every output against the
tree: any drift fails with `run: go generate ./api/...`. Protobuf was rejected: a CRD travels through
kube and kcp as JSON validated by OpenAPI, so a second wire format is a second source of truth.

**Schema revisions.** kcp holds `APIResourceSchema.spec` IMMUTABLE (re-measured: an update of a
changed spec under the same name is Forbidden). So a schema is named `v1alpha1-<Revision>.<plural>.<group>`
with `Revision` set per kind in `api/v1alpha1/resources.go`, and the revision lock refuses to generate
a spec that differs from what it recorded under that name: changing a Go type without bumping its
`Revision` fails generation, and so `check.sh`, before any host sees a schema it cannot apply. Old
names stay in the lock forever; old `APIResourceSchema` objects stay in kcp and are harmless.

**What the re-point does to tenants, MEASURED** (kcp v0.33.0 over kine, by an `internal/apigen`
test that plan 0026 deleted with every Go test): a workspace that bound the export while it named `v1alpha1-1` PRUNES
the new fields (a write returns `unknown field` warnings and drops them). Creating `v1alpha1-2` and
re-pointing the export moves that ALREADY-BOUND `APIBinding`'s `boundResources` to `v1alpha1-2` within
seconds, with the identity hash unchanged (so the etcd prefix and every stored claim are untouched),
the pre-upgrade claim intact, and the new fields then persisting. No binding is deleted or recreated:
the migration IS the manifest set (`deploy/sets.yaml`, `socialweb-computer apply`) applying the new
schema and then the re-pointed export, in that order. Without the re-point the binding stays on `v1alpha1-1` and the proof fails.

**NodeClaim `status.contract`** carries, beside the original
`vmUri|rfpUri|acceptUri|receiptUri|receiptCid|submitEventRef` (kept: prod claims hold them and the
no_bids back-off keys on `Failed` + `rfpUri` without `acceptUri`), every field of the contract
state (`v1alpha1.NodeClaimContract`): `version`, `phase`
(`requested|rfp_submitted|winner_selected|accepted|receipt|network|released`), `vmName`,
`requesterDid`, `marketDid`, strongRefs `{uri, cid}` `payload|rfp|policy|bid|accept|receipt`,
`winner {did, submitAccept?}`, `acceptPending`, `receiptOk`, `vmAddress` (relay FQDN or `iroh://`
ticket), `bids`, `release`, `termination`, `hostKeys`, `outcome`, `error` and `timestamps` keyed by
contract phase.
All optional, so every existing claim stays valid. None of it is secret: records are public repo
records, `submitAccept`/`submitEventRef` are bidder endpoints.

**The claim is the durable record a node is released from; the contract carries the flow.** The
Requester component runs a ComputeContract through the RFP flow (`internal/market/requester`, the
contract runner of `runner.go`) and writes the evolving contract state into the ComputeContract's
`status.contract` at each stage (RFP submitted, accepted, receipt, network); each write lands before
the flow moves on, so the receipt is on the contract before the guest boots, and a restarted runner
resumes from the recorded stage. The NodeClaim controller copies `status.contract` onto the claim's
`status.contract` on every transition (`internal/gateway/nodeclaim` `Mapper.Status`), with the legacy
`receiptUri|receiptCid` filled from the receipt strongRef, so the provider's no_bids back-off is
unchanged. A node is released when its claim is DELETED: the NodeClaim controller sets the claim's
Infra entry to Released, the Infra controller sets `spec.release` on the contract, and the Requester
releases it with the state read FROM THE CONTRACT, never from its
memory (section 9, "A market node joins through its cluster's OpenBao, never through ssh").
Decisions, made by Claude, user may veto. The two marked SUPERSEDED are kept for the record:

- A claim needs releasing iff its contract has receipt refs (`receipt` + `submitEventRef`) and its
  phase is not `released`. A released claim keeps its receipt and gains `release {event, payload,
  ok}`; the legacy receipt fields are no longer cleared, the contract phase is what says "done".
- SUPERSEDED (M3): a claim with no live session in this process (startup, or a live claim this
  process did not start) was released with `releaseCompute` from its saved state, reason
  `requester_restarted`. A restarted gateway now adopts a `Joining` or `Running` claim without a write or
  a release, and marks a `Bidding` or `Provisioning` claim `Failed`; the release waits for deletion. A pre-upgrade claim carrying only the legacy fields is released from a
  `ContractState` synthesised from them.
- `Released` only when `release.ok`; an unconfirmed release is `Failed`. A release that threw leaves
  the phase unreleased, so the delete path retries it.
- SUPERSEDED (M3): an unreadable claim at the end of a run deferred the release to the next pass.
  A run no longer releases at its end; an unreadable claim at DELETION defers the release, the
  finalizer stays, and the next pass reads the deleted claim again.
- A deleted claim, or a contract whose `spec.release` is set, asks its run to release
  (`contractRunner.Update`), and the run releases from the state recorded on the contract. The
  decision behind it stays: a flow must not end between the accept and the receipt, because that
  would leave a VM nothing can release.

**Claim layering.** The claim's shape is the Go type `v1alpha1.NodeClaim`, the source of its
schema; the NodeClaim controller (`internal/controllers/nodeclaim`) reads and writes it through the
typed kcp client, and the mapper (`internal/gateway/nodeclaim`) turns a claim and its ComputeContract
into the values of `internal/contract/nodeclaim`. A status write is always of the schema's shape. The
TypeScript claim store that came before plan 0026 is gone.

### Abstract kinds and their contracts

Plan 0026 W0-ABSTRACT. A concept with two or more implementations is an abstract kind plus
concrete kinds (the Cluster API infrastructureRef pattern). The rules below hold for every abstract
kind; the tables after them are the contract of each one.

- A consumer MUST ref only the abstract kind, never a concrete kind.
- An abstract kind MUST have `spec.backendRef`, a `common/v1alpha1.ObjectRef` to exactly one
  concrete kind of its contract, and a contract status with the conditions `Ready` and
  `BackendBound` (`commonv1.ConditionReady`, `commonv1.ConditionBackendBound`).
- Every concrete kind MUST carry the label `contracts.socialweb.computer/<abstract-resource>:
  v1alpha1` on its APIResourceSchema. `APIResource.Contracts` in the group's `resources.go` names
  the abstract resources; apigen writes the label and refuses a contract that names no resource with
  `spec.backendRef`. The keys are `commonv1.ContractLabelAtprotoRepos`, `ContractLabelVaults`,
  `ContractLabelComputeVMs` and `ContractLabelEventSources`; `APIResource.ContractLabels()` returns
  the map a controller sets or verifies.
- The composer (the root for children, the creating component for intent objects) MUST write the
  abstract kind's spec and the concrete kind's spec, and nothing else writes either.
- The concrete kind's controller MUST be the only writer of the concrete kind's status.
- The abstract kind's controller MUST be the only writer of the abstract kind's status. It MUST
  mirror every contract field from the bound backend's status, and MUST NOT compute a contract
  field the backend did not report.
- `BackendBound` MUST be True exactly when `spec.backendRef` resolves to an existing object of a
  kind that carries the contract label; otherwise it is False and `Ready` is False.
- `Ready` on the abstract kind MUST equal `Ready` on the backend, observed at the backend's current
  generation, and MUST be False while `BackendBound` is False.
- A field that only one backend can report MUST live on that concrete kind, never on the abstract
  kind.

**AtprotoRepo** (`api/v1alpha1`, export `applications`), over **LocalRepo** and **RemoteRepo**
(label `contracts.socialweb.computer/atprotorepos`).

| Field | Meaning | Writer |
| --- | --- | --- |
| `spec.backendRef` | a LocalRepo or a RemoteRepo | composer |
| `status.did` | the DID of the repo | AtprotoRepo controller, from the backend |
| `status.head` | the CID of the repo's latest commit | AtprotoRepo controller, from the backend |
| `status.rev` | the rev (TID) of that commit | AtprotoRepo controller, from the backend |
| `status.writable` | true when a RecordWriter of the repo can write now | AtprotoRepo controller, from the backend |
| `status.address` | the PDS base URL the repo is served from | AtprotoRepo controller, from the backend |
| `status.conditions` | `Ready`, `BackendBound` | AtprotoRepo controller |

- LocalRepo `spec.pdsRef` MUST ref the Pds component; it writes through the Pds signer handle and
  holds no token. RemoteRepo `spec.pdsRef` MUST ref a RemotePDS and `spec.credentialsRef` the
  credentials provider (a MarketIdentity) whose lease authenticates the writes.
- Both carry `spec.did`; `spec.authRef` is the session the plan 0026 W2-REPO controllers replace.
- Both report the contract fields plus `status.seq` and `status.keyFingerprint` in
  `AtprotoRepoBackendStatus`, written only by their own controller.

**Vault** (`api/v1alpha1`, export `hostprocesses`), over **OpenBaoServer** (local) and
**RemoteOpenBao** (label `contracts.socialweb.computer/vaults`).

| Field | Meaning | Writer |
| --- | --- | --- |
| `spec.backendRef` | an OpenBaoServer or a RemoteOpenBao | composer |
| `status.address` | the vault API base URL a client dials | Vault controller, from the backend |
| `status.ready` | the vault answers, is initialized and unsealed | Vault controller, from the backend |
| `status.initialized` | the vault is initialized | Vault controller, from the backend |
| `status.sealed` | the vault is sealed | Vault controller, from the backend |
| `status.caBundle` | the PEM CA bundle that verifies `status.address` | Vault controller, from the backend |
| `status.version` | the server version | Vault controller, from the backend |
| `status.conditions` | `Ready`, `BackendBound` | Vault controller |

- `status.caBundle` MUST hold only public CA certificates; a client MUST reload it when it changes.
- No token, unseal key or credential is ever a Vault field; the callee holds them.

**ComputeVM** (`api/v1alpha1`, export `computevms`), over **FirecrackerVM** (label
`contracts.socialweb.computer/computevms`); HcsVM and VzVM join when plans 0011 and 0012 land.

| Field | Meaning | Writer |
| --- | --- | --- |
| `spec.backendRef` | a FirecrackerVM | creator (the bidder) |
| `spec.vm` | the size and role asked for: cpus, mem, disk, role | creator |
| `spec.owner`, `spec.requester` | the account the VM runs under and the requester DID | creator |
| `spec.userData` | the SecretRef of the guest user data | creator |
| `spec.contract` | the market contract refs (receipt, accept, submit endpoint) | creator |
| `spec.image`, `spec.stopGraceSeconds`, `spec.guestLogin` | the image policy, the stop grace, the guest login | creator |
| `spec.accept` | the market accept the VM serves | creator |
| `spec.release` | `requestedAt` and the optional `request` StrongRef: the creator asks for the release | creator (field manager `socialweb.computer/bidder`) |
| `status.phase`, `status.lostReason` | `Pending` to `Running` to `Releasing`, or `Lost` or `Failed` and why | ComputeVM controller, from the backend |
| `status.addresses` | `guest` and `host` addresses of the VM | ComputeVM controller, from the backend |
| `status.bootedAt`, `status.message` | when it booted; the last transition | ComputeVM controller, from the backend |
| `status.conditions` | `Ready`, `BackendBound` | ComputeVM controller (field manager `socialweb.computer/computevm`) |
| `status.market.guest` | the guest's own report: address, reportedAt, host keys | guestcontrolplane (field manager `socialweb.computer/guestcontrolplane`) |
| `status.market.published` | the URIs of the market records published for the guest: onNetworkUri, hostKeysUri | bidder (field manager `socialweb.computer/bidder`) |

- Release MUST be `spec.release` written by the creator; an annotation MUST NOT carry it. The
  ComputeVM controller answers `spec.release` by creating the termination StateSync and deleting the
  ComputeVM; its finalizer deletes the FirecrackerVM, waits for it to go, then drops the guest login
  and the guest identity grants.
- The accept a VM serves MUST be `spec.accept`; an annotation MUST NOT carry it.
- The ComputeVM controller creates the FirecrackerVM named by `spec.backendRef` from the ComputeVM
  spec, owned by the ComputeVM (controller ownerReference), and mirrors its status on each backend
  event. Each field manager above is a const beside the type in `api/v1alpha1/types_computevm.go`;
  each writer applies only its own subfield.
- FirecrackerVM `spec` (owner, vcpu, mem, disk, image, userData, stopGraceSeconds) and `status`
  (network, image, runtime unit identity and VMM, work dir) are the FirecrackerVM controller's
  (`internal/controllers/firecrackervm`); a ComputeVM consumer MUST NOT read them. The backend
  `internal/compute/firecracker` implements `compute.Provider` and takes `host.Units`, a user data
  reader and a Placement as dependencies; it imports no kcp client. Unit state follows systemd D-Bus
  signals (`internal/hostsvc` Watch); no ticker drives it.
- Bid status has one writer, the bidder, by server-side apply under `socialweb.computer/bidder`
  (`v1alpha1.FieldManagerBid`); it binds a Won Bid to its ComputeVM and sets Complete or Failed.

**EventSource** (`api/events/v1alpha1`, export `events`), over **PdsEventSource**,
**RelayEventSource** and **JetstreamEventSource** (label `contracts.socialweb.computer/eventsources`).

| Field | Meaning | Writer |
| --- | --- | --- |
| `spec.backendRef` | one of the three backends | composer |
| `status.cursor` | the last sequence number the source delivered | EventSource controller, from the backend |
| `status.lastEventAt` | when that event arrived | EventSource controller, from the backend |
| `status.lag` | how far the cursor is behind the upstream head | EventSource controller, from the backend |
| `status.address` | the upstream the source is subscribed to | EventSource controller, from the backend |
| `status.conditions` | `Ready`, `BackendBound` | EventSource controller |

- PdsEventSource `spec.repoRef` MUST ref an AtprotoRepo and subscribes through its Subscriber
  handle. RelayEventSource refs a Relay component (`spec.relayRef`) or a user endpoint (`spec.url`).
  JetstreamEventSource has a user endpoint (`spec.url`).
- An EventStream MUST ref its source through `spec.sourceRef` (an EventSource) and carries only its
  `filter` (collections, DIDs), cursor and sink; no URL of a source is an EventStream field.

### The XRPC surface

How a user gets a kubeconfig. Seven procedures under `com.publicdomainrelay.temp.cluster.`:

```
createCluster       create the named cluster and record the intent to run it, in one call.
                    Idempotent. The create-on-read path getKubeconfig walks, then spec.running
                    = true -- set FIRST, on a context the caller's hang-up cannot cancel.
                    Answers the phase it FINDS, Initializing included: it never waits for the
                    control plane and never answers an error for a cluster that is coming up.
controlPlaneUp      bring the named cluster's control plane up. Idempotent. 429 + Retry-After
                    while Initializing. Refused once Draining. DOES NOT provision nodes.
controlPlaneDown    the inverse
getKubeconfig       the credential the user runs kubectl with
deleteCluster       the lifecycle end
listClusters        this account's clusters
revokeIdentity      revoke an identity: write the deny record first, then fan out
```

Auth is a service-auth token whose `aud` is the service and whose `lxm` is the method NSID. A
verifier on this path MUST be passed a real NSID: it compares against the token's `lxm`, and a nil
there skips the check.

**A caller that wants a usable cluster gets it in ONE call.** `createCluster` exists because the only
other way to a live cluster was two round trips through two different verbs - `getKubeconfig`, whose
kubeconfig a caller that is provisioning has no use for, and then `controlPlaneUp` - and because a
caller had to know that order. It is not a new path: it calls the same `revocation.CreateOnRead`
around `Store.Ensure` that `getKubeconfig` does, and then records `spec.running = true` itself, so the
deny is consulted before anything is created. THE ORDERING IS THE CONTRACT: the running write comes
first and reads nothing back, and it is made on a context the caller's cancellation cannot reach
(`context.WithoutCancel`, `internal/gateway/gateway.go`), because the gateway's web app abandons a
call at 30 s (`internal/gatewayhttp/cluster.go`) and an abort on that call must not
land before this write, which would leave a cluster whose tree exists and whose `spec.running` is
false, which no pass ever brings up. The phase is the ANSWER, not a refusal: a cluster whose control plane is still coming
up is a 200 carrying `Initializing` (a market join is minutes away by construction), and only a
Draining cluster is refused, from the view the write returns. `controlPlaneUp` keeps the stricter
reading - 429 + Retry-After while Initializing - because a caller that asks the control plane up by
name is asking about the control plane. `getKubeconfig` keeps its create-on-read, which is what makes
a first login work at all.

**`getKubeconfig` is served by a granter the composition root must be handed.** The granter
(`kcpclient.KubeconfigGranter`) is the client certificate issuer `clientcert.Issuer`, which
`internal/gatewaymain` hands the kcp client. A caller may also ask for a chosen credential lifetime;
the settings and the reasoning behind each are in section 15.

**A routed verb is not a working verb.** A verb can be on this surface, routed and documented while
the composition root hands its client no granter, so it answers `ErrNoKubeconfigGranter` to every
caller that reaches it. It fails CLOSED, which reads as a gap rather than a hole, and only a live
call that reads the ANSWER, not the route, shows it.

**The tenant path is gated on `status.phase == "Ready"`, and our front owns that gate.** kcp's
mount controller reads the referenced object's PHASE, not its conditions, before it will serve
(measured: `phase: Initializing` gave `WorkspaceMountReady=False(MountObjectNotReady)` and a 503).
Tenant requests no longer cross the kcp mount (below), so the same gate is now ours and these are
MUST rules:

- The client certificate front MUST admit a tenant request only while the `Cluster`'s
  `status.phase` is `Ready` and it has no `deletionTimestamp` (`kcpclient.ClusterGate`), answering
  503 otherwise and 404 when the `Cluster` is absent or its `spec.account` hashes to another account.
- The front MUST follow that gate by a watch on the `Cluster` (`kcpclient.FollowClusterGate`), MUST fail
  closed on a read error, and MUST close every open request (watches included) for a cluster whose
  gate it observes closed.
- The tenant path is SHUT when, and only when, the `Cluster`'s own state has closed the gate for at
  least `materialiser.PathGateGrace`: `deletionTimestamp` set, or `phase` not `Ready` with the `Ready`
  condition non-True since its `lastTransitionTime`. `materialiser.shut` MUST NOT read an HTTP status
  code; a 502, 503 or 404 from anything is never "shut".

**The gateway acts in a tenant cluster as its owner.** `kubectlApplyServerSide` and `getJob` need a
credential for the cluster's owner. The gateway signs one per call from the cluster's own client CA
under `<ClusterStateDir>/<key>/server/tls` (CN the owner's DID, O `socialweb.computer:cluster-owners`,
10 minutes), dials the apiserver front whose address `front.json` reports, and the `socialweb-owner`
ClusterRoleBinding of `deploy/tenant-grant.yaml` gives that group `cluster-admin`. No ServiceAccount
token and no stored credential is involved, and the certificate is never written to disk.

**No tenant request crosses the kcp mount.** kcp main (kcp-dev/kcp #4427, `pkg/server/mountproxy.go`)
verifies the mount target's certificate against the system roots plus `--mount-proxy-server-ca-file`,
authenticates the caller itself, strips `Authorization` and forwards `X-Remote-*` over its own mount
proxy client certificate. A tenant k3s presents its own server CA and authenticates its own
ServiceAccount tokens, so through the mount every tenant request is a 502 (target CA) or a 401 (the
k3s token at kcp). The rules:

- Every request for `/clusters/root:<account>:<cluster>:clusters[/...]` MUST be served by
  `internal/clientcertfront`, in dev (the front is `SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST`) and in prod
  (Caddy's `@clientcert` and `@tenant` matchers send it there over the edge client certificate; every
  other path goes to kcp). The external URL is unchanged, so a kubeconfig's `server` is still
  `<public host>/clusters/<home>:clusters`.
- The front MUST forward to the address the cluster's `front.json` names, verifying the cluster's own
  `server/tls/server-ca.crt`, with the path below `:clusters` and the query unchanged, and MUST strip
  every inbound `X-Remote-*` and `X-Socialweb-Client-Certificate` header.
- A request with a client certificate MUST pass the certificate checks of section 7 and goes by
  request-header auth over `client-auth-proxy`, with `Authorization` removed.
- A request without one (or from the edge without the certificate header) MUST carry
  `Authorization: Bearer ...`, MUST pass the deny lookup of the `Cluster`'s `spec.account`, and is
  forwarded with `Authorization` UNCHANGED over a transport that presents no client certificate, so
  k3s authenticates the bearer itself.
- The tenant liveness probe (`Registry.TenantPath`, `clusterprovider.ProbeTenantFront`) MUST dial the
  `front.json` address directly, verifying the cluster's server CA; its result is reported, never
  counted as shut.
- The revoker (`kcpclient` `FanOut`, `DeleteServiceAccount`) MUST reach the tenant directly at its
  front with a 10 minute owner credential signed from the cluster's client CA (`Options.Tenant`); a
  tenant it cannot reach is `Cold`, never a completed delete.
- The kcp mount (`Workspace` `clusters` with `spec.mount.ref` to the `Cluster`, `spec.URL` from
  `status.URL`) MAY stay for kcp-side listing and discovery only; nothing may depend on kcp forwarding
  through it.

Why kcp changed: GHSA-c8w2-fgvx-vhv4 (CVE-2026-61682) let a client-supplied `X-Remote-Group`
through the front proxy; the fix strips it, #4238 added the mount proxy client certificate, and
#4427 makes kcp authenticate every caller before it forwards to a mount (v0.33.1 fell back to
`InsecureSkipVerify` and forwarded an unauthenticated caller's `Authorization`). The supported
contract is a target that trusts kcp's identity headers; it cannot serve tenant credentials that only
the tenant k3s can verify, so tenant traffic stays off the mount. Plan 0015's reason ("kcp's local
proxy runs before authentication and forwards every client header") described v0.33.1 and is
superseded by this section.

**The negative rule, which is a design invariant:** never configure a cluster's apiserver to
trust request-header identities from the kcp mount proxy. The only request-header client a tenant
trusts is its own `client-auth-proxy`, held by the front.

## 7. Identity and auth

- **Bearers and certificates both go through the client certificate front, never through the kcp
  mount.** Caddy (prod) or the client certificate front itself (dev, market network) ends TLS for
  every `/clusters/<home>:clusters/...` request. A client certificate MUST be checked by the front
  (`internal/clientcertfront`) against the cluster's client CA, the PKI CRL, the deny record and the
  `ClusterClientCertificate` of its serial (it must exist and not be revoked) and is forwarded to k3s
  by request-header authentication (client-auth-proxy, X-Remote-User = the CN, X-Remote-Group =
  `socialweb.computer:cluster-owners`). A bearer MUST be forwarded unchanged to the cluster's own
  front and is authenticated by k3s alone; the front adds only the cluster gate and the deny lookup.
  Both MUST pass the cluster gate of section 6. No JWKS, no `--authentication-config`.
- **ServiceAccount tokens via TokenRequest** for the machine class.
- **The DID to ServiceAccount mapping** is the tenancy boundary, and it has exactly ONE spelling: a
  DID's ServiceAccount is `did-` plus the lowercased unpadded base32 of the DID
  (`kcpclient.ServiceAccountName`), which is a valid DNS-1123 label because base32 emits only
  `[a-z2-7]` and never a hyphen or a digit outside that range. The deployed grant carries it as the
  subject of the `socialweb-owner` ClusterRoleBinding, so the spelling cannot be changed later without
  a migration.
- **The grant is rendered for the cluster's OWNER, and the owner can act in it.** The subject's name
  is not a literal in the deployed file: `deploy/tenant-grant.yaml` carries
  `__SOCIALWEB_OWNER_SERVICE_ACCOUNT__` and the bootstrap substitutes `ServiceAccountName` of the
  `Cluster`'s `spec.account` - the same DID the mount path is derived from - before it parses the
  document. `RenderTenantGrant` REFUSES a document with no placeholder rather than applying it, so a
  grant scoped to a name nobody owns cannot be applied by accident. The file also carries a
  `ClusterRoleBinding` `socialweb-owner` binding that ServiceAccount, and the group
  `socialweb.computer:cluster-owners`, to `cluster-admin`: it is the owner's own cluster, and until
  this existed a real user's minted token authenticated and was authorized for NOTHING, because no
  `did-*` ServiceAccount had any RBAC.
- **A revocation that only deletes objects does not revoke.** Deleting a ServiceAccount is a ~10 s
  interruption, not a revocation: a stale, name-keyed RoleBinding resumes grants the moment the
  object is recreated. Revocation needs a consultable deny record, and it is per-DID.
- **The token is per cluster INSTANCE**, not per cluster. A new agent cannot join an orphaned server,
  because it does not hold that instance's token. This is what keeps an orphan harmless.

**The tenant credential is minted from the ClientCA.** `cred/admin.kubeconfig` is derived, not stored,
so the provider derives it: it reads `<DataDir>/server/tls/client-ca.{crt,key}` and signs a fresh
ECDSA P-256 client certificate with CN `system:admin`, O `system:masters`. That is the identity k3s's
own admin kubeconfig carries, and it is the arm of the grant's carve-out that writes Secrets -
`deploy/tenant-grant.yaml`'s `socialweb-mint-identity` policy admits a Secret create in the tenant
namespace only for `request.userInfo.username == 'system:admin' && 'system:masters' in request.userInfo.groups`, or the
External Secrets controller `system:socialweb:external-secrets` (the identity of the pair beside the
control plane, section 5; the pair is not the in-tenant ServiceAccount
`system:serviceaccount:default:external-secrets`, which a tenant who creates that ServiceAccount would
inherit). Writing a Secret in the tenant namespace is the provider's act, through its own admin
credential for the grant and through the External Secrets controller for the vault-backed Secrets it
materialises; no TENANT subject is admitted. `system:masters` is the apiserver's
built-in superuser group for client certificates, so
the credential is cluster-admin-equivalent on the tenant apiserver, and it is the **provider's**: the
provider applies the tenant grant, reads the tenant cluster's Nodes and Pods for the scale observation,
and deletes the tenant-side Node object on teardown.

**Nothing k3s generated is read.** The rejected route was the k3s-written admin kubeconfig under the
data dir: its server URL and SANs are unverified against the address a node dials, and
`/etc/rancher/k3s/k3s.yaml` is shared state on a host that runs several clusters.

**Which source mints it follows the SUBSTRATE, and not the mere presence of a seal.** There are two
sources: the cluster's own `ClientCA` on disk, and the cluster's vault, whose PKI engine issues from a
copy of that same CA imported into it. The choice is an explicit input to the seam
(`clusterprovider.CredentialSource`), derived by the caller from where the cluster's nodes are placed.
For `market` the credential is always signed from `client-ca.{crt,key}` on disk and the vault is never
consulted: a market node's machine is not this host, the cluster vault in that path exists for the
guest's custody and wake material rather than for the provider's own credential, and minting from it
on every pass puts a second TLS dependency between the provider and the tenant apiserver for no gain.
For `firecracker` and `qemu` nothing changes: the vault issues when the cluster has left unseal
material beside its data dir, and the ClientCA signs when it has not. A caller that names no source is
refused (`ErrCredentialSourceUnknown`) rather than defaulted, because the default is the thing this
seam exists to remove.

**Minting refuses rather than downgrades.** No ClientCA on disk is a refusal, never a fallback to a
wider credential. A CA key that is not its certificate's key is refused *before* anything is signed,
because that pair still signs and the failure would otherwise surface as an authentication error at
the first apply. An empty CN or O is refused, because it authenticates as the empty username or as an
unprivileged user. A non-https URL is refused, because the URL's host is the name the apiserver's
certificate must carry. The rule holds ACROSS the two sources as well as within each: a cluster whose
ClientCA cannot sign is refused with the reason, and the vault is not tried instead.

**The provider's credential never reaches a user.** It is a certificate credential signed by the
cluster's client CA with `system:masters`. The user path is the kubeconfig of `getKubeconfig`: a client
key signed from the cluster's own `client-ca.{crt,key}` on disk when they exist (`internal/clientcert`),
or by the cluster's OpenBao PKI as the fallback (role `kubeconfig-user`, at most 24 h), CN the user's
DID, O `socialweb.computer:cluster-owners`, which the client certificate front admits only while its
`ClusterClientCertificate` exists and is not revoked.

**A workspace ServiceAccount does not leave its workspace.** Its token carries
`authentication.kcp.io/scopes: [cluster:<origin>]` and kcp refuses it in every other logical cluster
before RBAC is consulted - measured both directions, with a real TokenRequest token: a provider
token is denied in a tenant workspace even with a matching ClusterRoleBinding there, and a tenant
token is denied in the provider workspace. `--as=` impersonation does NOT reproduce it (it sets the
username and not the scope extra), so an impersonation probe cannot answer the question and an RBAC
probe that uses one is not evidence. And the refusal is not RBAC at all: a token is refused by
every workspace it was not minted in, which no subject string can spell.

The consequence is load-bearing: `internal/kcpclient` addresses root, the account workspace, the home
workspace and the mount from ONE `rest.Config`, so **the runtime cannot be any of the four provider
ServiceAccounts**. The cluster provider holds a credential kcp does not confine to a workspace - the
admin kubeconfig the operator already installs - and the four ServiceAccounts exist for a future
split into four identities per workspace, not for today's single client. That is a deliberate
privileged gateway-side credential: it is never handed to a tenant, and the tenant's own path is the
client certificate front with a short-lived client certificate.

RBAC cannot enforce the identity classes for a runtime that spans workspaces; the SCOPE does, and it
enforces them by refusing the client rather than by shaping it.

**The identity bundle is exactly 13 entries:**

```
tls/server-ca.crt      tls/server-ca.key
tls/client-ca.crt      tls/client-ca.key
tls/request-header-ca.crt   tls/request-header-ca.key
tls/etcd/server-ca.crt tls/etcd/server-ca.key
tls/etcd/peer-ca.crt   tls/etcd/peer-ca.key
tls/service.key        cred/passwd        cred/ipsec.psk
```

`cred/admin.kubeconfig` is NOT one of them: it is DERIVED from the ClientCA, deterministic. Nor is
the instance token, which is per-instance state.

**Pre-seeding a list without request-header-ca and all four etcd CAs makes k3s REGENERATE those and
INVALIDATE every kubeconfig and SA token.** The count is load-bearing: `internal/identitybundle` refuses a bundle
whose size is not `len(identitybundle.Paths)` (`ErrIncomplete`).

### A market node's secrets live in its cluster's OpenBao, behind a JWT role

This subsection describes how a market node's secrets are stored and read. Names are in
`internal/vaultlayout/nodejwt.go`, the vault writes in `clusterprovider.NodeVault`
(`internal/clusterprovider/nodesecrets.go`: the policy, the JWT mount and the role go into the
cluster's OpenBaoServer declaration, `internal/clusterprovider/vaultdeclared.go`, and the core's sync
writes them; the secret itself is written as the vault client `clusterprovider`), the order in
`internal/nodemarket/secrets.go`. Only the market substrate wires it (`nodeHostsOptions.NodeSecrets`,
set in `internal/providermain/main.go`); a firecracker or qemu guest keeps its cert-auth path (`node-secret-<node>`,
`secret/data/k3s/node/<node>`), which is a different policy namespace and a different mount.

| what | name | written | removed |
|---|---|---|---|
| KV v2 mount | `socialweb-nodes` | enabled on first use if `sys/mounts` lacks it | never |
| node secret | `socialweb-nodes/data/<node>` | before the claim, once `ClusterLink <clusterKey>-apiserver` reads `Listening` with a ticket; else by the first later pass that sees one | release (`DELETE socialweb-nodes/metadata/<node>`, every version) |
| policy | `node-<node>`: `path "socialweb-nodes/data/<node>" { capabilities = ["read"] }` | before the claim, always | release |
| JWT auth mount | `jwt-<first 12 hex of sha256(issuer)>`, type `jwt`, `oidc_discovery_url` = `bound_issuer` = issuer | when a claim first carries `status.grant` with that issuer | disabled by the reconcile once no live claim's grant names it and it has no role left |
| JWT role | `<node>` under that mount: `role_type jwt`, `bound_subject` = grant subject, `bound_audiences` = [grant audience], `user_claim sub`, `token_ttl 5m`, `token_policies [node-<node>]` | the first pass after the NodeClaim controller writes `status.grant` | release, under every `jwt-<12 hex>` mount |

The secret's fields are `token` (the pinned join token `K10<sha256 hex of serverCA>::server:<k3s
token file, trimmed>`, composed by `nodemarket.PinnedJoinToken`, so
`k3s agent --token` takes it verbatim), `nodePassword` (the per-node password
`nodemarket.MintOrReadNodePassword` minted at Provision, trimmed), `serverCA` (the PEM of
`<data dir>/server/tls/server-ca.crt`, verbatim), `apiserverTicket` (the link's `status.ticket`),
`advertiseAddress` and `apiserverPort` (an integer; the claim's own `spec` values). A secret exists
only when it is whole: the guest may read "present" as "complete".

Decisions, made by Claude, user may veto:

- **The secret waits for the link; the claim does not.** A link that is not `Listening` holds back
  the secret, never the claim, and never the pass: Provision writes the policy and the claim and
  returns, and `confirmHeld` calls `SyncSecrets` on every held market node each pass, which writes
  the secret once a ticket exists (logged once per distinct reason, not per pass). A policy that
  cannot be written fails the Provision before the claim, so nothing is rented that could never
  read its join material.
- **The grant is the NodeClaim controller's; the role name is not recorded in status.** The provider
  never writes `status.grant.role`; the role name is deterministic (the node name). `EnsureLogin`
  declares the grant on the cluster's OpenBaoServer, and a pass over an unchanged grant changes
  nothing in the declaration.
- **A mount goes with its last live grant.** The declaration names a `jwt-<12 hex>` mount while a
  live node's grant names its issuer, so the sync removes a mount only once no live grant names it.
- **Revocation is best-effort and bounded** (`nodemarket.RevokeTimeout`, 10 s), runs right after the
  claim delete is accepted and before the wait for the NodeClaim controller, and only logs a failure: the machine is
  what costs, and a leftover role binds one workload subject the released machine no longer runs.
- **The issuer must answer from this host.** OpenBao fetches `<issuer>/.well-known/openid-configuration`
  when the config is written and the JWKS at login; a bidder issuer the cluster vault cannot reach
  is an `EnsureLogin` error in the pass log, retried every pass.

How a guest logs in, which is the requester's and the guest's half (`claim.json`, written by the
guest's cloud-init from `internal/atproto/cloudinit/k3sjoin.go`, carries `jwtMount`,
`role`, `audience`, `kvMount`, `kvPath`): ask `guest-login.sh token <audience>` for a token of the
bidder's OpenBao identity/oidc whose `aud` is the grant's audience (`api://socialweb-node?actx=<clusterKey>/<node>`)
and whose `wid_sub` claim is the grant's subject, then `POST /v1/auth/jwt-<12 hex>/login` with `{"role": "<node>", "jwt": "<token>"}`, then
`GET /v1/socialweb-nodes/data/<node>` with the returned `client_token` in `X-Vault-Token`
(`internal/nodeagent`). Measured against a real OpenBao v2.6.2 (`bao server -dev`) and an ES256 OIDC
issuer when it was built: the node's own token logs in and reads its path; a token for another
subject or audience is refused; the node's token gets 403 on another node's path; after `Revoke` the
role, policy and secret are gone, the login is refused, the other node's policy and the mount remain.

### The cluster OpenBao is reconciled against the live claims

The node entries below are part of the cluster's OpenBaoServer declaration: `NodeVault.Reconcile` sets
that declaration to the live claims, and the core's sync removes whatever no declaration names.

The table above is what is written; this is what is removed. `Revoke` alone is eager and therefore
skippable: a provider restart or crash between a claim going
away and its `Revoke` leaves a role, a policy and a secret behind, a bidder's issuer is a fresh
`did:key` per restart, so every issuer a cluster has ever seen would otherwise leave its `jwt-*` mount
forever (read live: 5 `jwt-*` mounts, 1 live node).
`(*clusterprovider.NodeVault).Reconcile` (`internal/clusterprovider/nodesecrets.go`) takes
`live map[string]*v1alpha1.NodeClaimGrant` for ONE cluster's OpenBao and rewrites the node part of
that cluster's OpenBaoServer declaration to it (`setVaultNodes` in
`internal/clusterprovider/vaultdeclared.go`): for every live node the policy `node-<node>`, the KV
entry `socialweb-nodes/<node>` and, when the claim carries a usable grant, the mount
`jwt-<12 hex of sha256(grant.Issuer)>` with the role `<node>` under it. It touches only the
declaration's entries of those naming patterns (`vaultlayout.NodeFromJWTPolicy`,
`vaultlayout.IsNodeJWTMount`, the `socialweb-nodes` KV mount); every other policy, mount and entry
of the declaration stays. The core's sync of the OpenBaoServer then makes the vault equal the
declaration. The method is declared as `nodemarket.NodeSecretsReconciler`, a sibling of
`NodeSecrets` rather than a fourth method on it, so a substrate's secrets implementation still has
one method per verb and the provider type-asserts it the way `confirmHeld` asserts `secretSyncer`
(`internal/providermain/vaultsync.go`); the provider calls it with `live` = the claims it holds plus
the `Adoptable` claims in the cluster's home workspace (`liveNodeGrants`), after `adoptClaims` at
adoption and on every pass, rate-limited to at most once per cluster per `VaultSyncInterval` (30 s).

## 8. Ports

Every per-cluster port is chosen by the kernel and held by the cluster's apiserver front (member
`front`) for the life of the cluster. Nothing checks a port and binds it later. The front binds its own
listener on `BindAddress:0`, keeps it in its fd store and records it in `front.json`; the HostProcess
supervisor reports it in the front's `status.listeners`, and the provider reads the `k3s-secure` port
from there. Every other purpose below is a HOLD: the provider asks the front (control op `hold`), the
front binds `[::]:0` dual-stack with `SO_REUSEADDR` off, sets `SO_REUSEADDR` after the bind, never
listens, and stores the socket in its fd store (`FDNAME=hold.<purpose>`, `FDPOLL=0`). The member
(k3s, kine, openbao, external-secrets: third-party binaries that take a port flag and accept no
socket) binds that number beside the hold. The holds are reported in the Cluster
`status.front.heldPorts`; every member's own listeners are in its HostProcess
`status.listeners`. `internal/portallocator` only records which port the front holds for which
purpose (`leases.json`); there is no port range and no probe.

| purpose | for |
|---|---|
| `postgres` | the cluster's embedded-postgres - if embedded bostgres doesn't work, use real postgres |
| `openbao` | the vault |
| `kine` | kine's listener |
| `k3s-secure` | the cluster's apiserver front (`internal apiserver-front`, member `front`): the port a node, the kcp mount, the provider, the CCM and ESO dial, and `--advertise-port` of every slot |
| `k3s-slot-a`, `k3s-slot-b` | the `--https-listen-port` (supervisor) of k3s slot a and slot b behind the front; never advertised |
| `k3s-apiserver-a`, `k3s-apiserver-b` | the `--apiserver-port` (a hidden k3s flag) of slot a and slot b: the apiserver listener on `127.0.0.1`, which k3s otherwise puts on `https-listen-port + 1` |
| `kine`, `kine-b` | kine's listener of slot a and slot b |
| `k3s-controller-manager`, `k3s-scheduler`, `k3s-cloud-controller-manager` (slot a), the same names with `-b` (slot b) | the k3s control-plane components' secure ports |
| `external-secrets-health` | the External Secrets controller's `--live-addr`, on `127.0.0.1` |
| `external-secrets-webhook` | the External Secrets webhook's TLS `--port`, the one its `ValidatingWebhookConfiguration` names as `https://127.0.0.1:<port>`; it binds every interface, because the upstream flag has no host |
| `external-secrets-webhook-health` | the External Secrets webhook's `--healthz-addr`, on `127.0.0.1`. Both members' metrics are off (`--metrics-addr=0`), so no metrics port is leased |

**The apiserver front owns the `k3s-secure` port.** Each cluster runs
member `front`: `internal apiserver-front`, the same L4 front kcp uses (`internal/apiserverfront`), bound
on `BindAddress:<k3s-secure>`, its control socket `<dataDir>/front.sock` (or
`<stateDir>/fronts/<hash>.sock` when that path is over 107 bytes) and its state `<dataDir>/front.json`.
k3s listens on its slot lease and advertises the front pair (`--advertise-address` unchanged,
`--advertise-port` = the front port), so `status.URL`, every node secret's `apiserverPort`, the link's
`--host`, every kubeconfig and the kubernetes EndpointSlice keep their number across a k3s swap. The
backend is chosen per new connection; open connections stay on their backend. The front never fails
over by itself: the k3s gate points it at its slot after `/readyz` and the first kube-system Deployment
answer on the slot port, then checks `/readyz` through the front and that the kubernetes EndpointSlice
names only the advertise address and the front port. Its listening socket is kept in the backend's fd
store (systemd `FDSTORE=1`; devsup on proc), so a front restart refuses no connection and resets only
open ones. `AssertMarketAdvertiseInvariant` takes a `portallocator.Advertisement`: advertise port ==
front lease, link target == front lease, no slot listen port inside the front lease.

**The vault's lease is ONE port.**
Every known purpose is one port wide (`Purpose.Width`), `openbao` included. The config the vault is given
(`clusterstack.OpenBaoServerConfig`, section 5) sets `disable_clustering = true`, which removes the
cluster listener and makes "one vault per cluster, no HA" a property of the file. Both the listener
stanza and `api_addr` take the same `Lease.Port`, and no code in the tree reads `Lease.Port + 1`. The
k3s pair is two holds, `k3s-slot-<s>` and `k3s-apiserver-<s>`, because the apiserver binds
`*:<https-listen-port>` AND
`127.0.0.1:<https-listen-port + 1>`, a kernel-picked port has no free neighbour to count on, and
`--apiserver-port` moves the second
listener (measured: `127.0.0.1:41000`, `[::1]:41000` and `127.0.0.1:41500` with `--https-listen-port
41000 --apiserver-port 41500`).

**`advertise` and `kubelet` are retired purposes, and neither is a second fact.** The advertised port
IS the bound port: `K3sAPIPorts` answers both questions from the `k3s-secure` lease, and the invariant
refuses an `--advertise-port` that is not the port the apiserver holds. `kubelet` is not leased: a node
is not a process in THIS host's kernel and network namespace, so the port is the guest's own. Both are
in `retiredPurposes` (with the ssh and link purposes): leases for them are dropped from an existing
lease file on open, and `Allocate` refuses them as unknown purposes. Section 15 holds both
arguments in full, including what the kubelet lease was for.

**The invariant that matters:** an unleased or colliding advertise **port** routes a node's tunnel to
ANOTHER CLUSTER'S APISERVER, and that surfaces as an **x509 name error**. The certificate is not the
fault; the lease is. The advertised port is not chosen and is not separately leased: it IS the port the
`k3s-secure` lease holds, so the two cannot disagree.

**A node binds no port of this host's, so a node owns no lease.** Every port its guest binds, the
kubelet's included, is bound inside the guest's own network namespace. MEASURED on this host with a
guest running, nothing binds 10250, 10248, 10249, 10251 or 10256, the host's k3s runs `--disable-agent`
and so has no kubelet at all, and pasta is started without `-t`/`--tcp-ports`, so no guest port is
published and the host cannot initiate a connection to a guest.

**Only ONE port serves the API, and it is the one a node must dial.** Measured: `k3s server
--disable-agent` binds `*:<https-listen-port>` AND `127.0.0.1:<https-listen-port + 1>` as a PAIR -
which is why each slot holds two ports - and in steady state it binds NOTHING on
`--advertise-port`. `/cacerts` answers 200 on the listen port and 401 on the advertise port for every
credential form, so an agent dialed at the advertise port loops on `failed to get CA certs:
Unauthorized`. The node's `--server`, `status.URL`, the tenant credential, the readiness gate and the
hardening seam all name the listen port; hardening through the advertise port is what kept the
process group from ever becoming ready.

**N control planes per host: proven. N nodes per host: refuted.** Two `--disable-agent` servers ran
concurrently, each with its own kine and store, both serving - `kubectl get ns` against each - with the
port pair above and disjoint control-plane secure ports. A node is the opposite: `127.0.0.1:6444`
collides FATALLY and the loser is served the other cluster's CA; there is one
`/run/k3s/containerd/containerd.sock` (hardcoded - `--data-dir` moves only `root`; measured
cross-contamination, cluster A's containerd served `RunPodSandbox` for cluster B's pods), one
`flannel.1` whose NAME is not redirectable and whose reconfiguration panicked the other agent
(`vxlan_network.go:217`), one `8472/udp` socket, one `/run/flannel/subnet.env`, one IPAM pool, one set
of `KUBE-*` chains and one `/var/lib/kubelet/pod-resources`. All four candidate fixes were refuted.
That list is the measured case for a VM per node.

**The hold, measured** (Linux 7.2, in a private network namespace with a
1000-port ephemeral range): a second exclusive hold of a held port is refused; a member binds
`127.0.0.1:P`, `[::1]:P` or `:P` beside the hold and is dialled; a second listener is refused while
the member listens; the member binds again after it restarts; 998 port-0 binds exhausted the range
and none got a held port; 200 outgoing connects took none. A cluster link's dumbpipe binds UDP port 0
(`--ipv4-addr 0.0.0.0:0`); a node dials it by its ticket, so its port is not held.

**The bind probe (before the holds).** Before the front held every port, the allocator chose a port
from a range and probed it; the holds replaced the probe. Its lesson is why a hold binds the
WILDCARD and sets `SO_REUSEADDR` only after the bind (`internal/apiserverfront/fdstore_linux.go`; the
one exception re-binds a known port over a TIME_WAIT socket). A probe that relies on
Go's `SO_REUSEADDR` does not coexist with a wildcard binder: a LISTEN socket conflicts with a wildcard
or a specific bind whatever the prober's `SO_REUSEADDR` is. The fault is the ADDRESS, not the flag:

```
holder bound 127.0.0.2:P, LISTENing            -> old probe said FREE   (a wildcard bind is blocked)
holder bound 0.0.0.0:P with SO_REUSEADDR, not yet LISTENing
                                               -> old probe said FREE   (a wildcard bind is blocked)
holder bound [::]:P, ipv6-only, LISTENing      -> old probe said FREE   (a wildcard bind is blocked)
```

The later probe bound the WILDCARD with `SO_REUSEADDR` cleared; both halves were load-bearing, and the
old probe leased `35167` while it was already bound. `/proc`'s LISTEN table was not a substitute: it
has no row at all for a bound-not-listening holder.

**Unleased per-node ports.** `127.0.0.1:6444` (k3s agent supervisor), `:10248` (healthz) and `:10010`
(containerd) are unleased, and `6444` is host-fixed, so two agents on one host collide on it.

## 9. A node

**The contract**, which is durable and substrate-independent:

- a node is one long-lived command;
- closing it is the teardown, and it is the ONLY teardown - one code path, no leak surface beyond
  "did we close it";
- nothing is ever read back out of a node. We generate the token and set the node name ourselves.
- `--node-name` is set from the `(cluster, node)` pair, never learned by a round trip. This is not
  hygiene: IP reuse lets a dead node shadow a live kubelet and break `logs`/`exec` with a 502.

**The mechanism**, which is substrate-specific. The design has had three; exactly one is still
reachable, and the other two are recorded because the evidence that removed them is the evidence that
keeps them removed:

| | |
|---|---|
| **a market guest** | **THE node path.** The node is a VM rented from the COMPUTE MARKET through a `NodeClaim` the clusterprovider writes and the NodeClaim controller (`internal/controllers/nodeclaim`, bound by the GatewayAPI component) fulfils under the cluster owner's own atproto OAuth session. **`internal/nodemarket`**, `SOCIALWEB_CLUSTERPROVIDER_SUBSTRATE=market`. NOT a guest of this host: no subnet lease, no pasta, no VMM, no node image and no `/dev/kvm` is involved. The clusterprovider's whole part in the node is the claim; the gateway's Requester runs the RFP flow, the winning bidder boots the guest from its cloud-init, and the NodeClaim controller writes the phase back; the guest itself dials the cluster's OpenBao and apiserver through the cluster's ClusterLinks and binds the advertised address (`SOCIALWEB_CLUSTERPROVIDER_MARKET_ADVERTISE_ADDRESS`, default `198.18.255.254`) on its own loopback (section 9, "A market node joins through its cluster's OpenBao, never through ssh"). Confirming is the tenant Node appearing for a claim the NodeClaim controller left `Joining`, not a local process being alive |
| **a guest of this host** | RETIRED. A VM this host started itself -- `firecracker` or `qemu` behind the same `scale.Nodes` seam, each a VMM in its own network namespace, booting a node image built here and joining through an agent the image carried. Both values were refused and deleted: the market path with a local bidder covers the same market network, so a second way to place a node was a second way to be wrong about where a node is. The guest the MARKET rents is booted on the bidder's host by the bidder's own path (`internal/compute/firecracker`, `internal/vmruntime`, `internal/guestimage`, `internal/guestrootfs`, `internal/nodeimagemain`), not by any code of the retired substrate |
| **local process** | DELETED. A `k3s agent` child process, the deleted `internal/nodelocal`, behind the same `scale.Nodes` seam. REFUTED as the substrate, three ways, all measured: `k3s agent` is fatal as a non-root user (`agent requires additional privilege if not run with --rootless`); it needs root for `/run/k3s/containerd`, `/var/lib/kubelet/device-plugins`, `127.0.0.1:6444` and the host `KUBE-*` chains, none of which `--data-dir` moves; and postgres is fatal AS root (`initdb: cannot be run as root`), so a host that runs the store cannot run a node |

**`SOCIALWEB_CLUSTERPROVIDER_SUBSTRATE` accepts exactly one value, `market`, and every other name is
refused at startup.** `checkPlacement` names `market` as the only placement implemented; the two
retired names get a refusal that names what replaced them, because a
name from before the retirement would otherwise read as a typo. `market` is the one value that was
never a guest: it takes no subnet range, mounts no guest gateway, and its `checkPlacement` demands no
VMM, no agent, no image and no pasta. The variables only the retired substrates read --
`FIRECRACKER`, `QEMU`, `NODE_AGENT`, `IMAGE_OUT_DIR`, `IMAGE_CONFIG`, `NETWORK`, `NODE_DISK_MIB` --
are not parsed into the Config, and setting one is refused rather than ignored, since an
operator who set one and saw no complaint would believe it was carrying it.

The local-process mechanism is **DELETED, not merely refuted**: `internal/nodelocal` and the
`process` value of `SOCIALWEB_CLUSTERPROVIDER_SUBSTRATE` are gone, so a host can no longer be
configured into it. The `scale.Nodes` seam stays, with one implementation behind it, because
the seam is what made a second substrate cheap and a third possible. The measurements that refuted the
local-process mechanism are in the git history: `git log` on the deleted `experiments/process-substrate/`
carries them, with the commit messages that say what each one established.

**History.** The paragraphs from here to "`k3s server` is deliberately NOT a node" record what was
measured on the retired guest-of-this-host substrate: its node images (the host's own kernel and
modules), the image builder, pasta namespaces and snapshots. That code is gone, and "now" in them
means at the time. The market guest's image is built from `deploy/guest-image` with a kernel from
`deploy/guest-kernel` that has no loadable modules (`CONFIG_MODULES=n`, section 12).

**A guest kernel is obtainable with no privileges at all, and this was measured.** Firecracker boots
a kernel YOU supply, and the CI kernels cannot run a k3s node: all three fail `k3s check-config` with
`STATUS: 4 (fail)` - `NETFILTER_XT_MATCH_IPVS`, `XT_MATCH_COMMENT`, `XT_MATCH_MULTIPORT`,
`XT_MATCH_STATISTIC`, `IP_VS`, `IP_SET`, `NF_TABLES_INET` and `NFT_MASQ` are all absent, so BOTH
kube-proxy modes are unusable. Building a kernel needs `flex`, `bison` and `libelf`, which are not
installed and need root. The way through is that **the host's own kernel already has every one of
those symbols as modules**: extract the ELF from `/boot/vmlinuz-$(uname -r)` (zstd; the payload
starts at byte offset 21196) and carry the host's own `.ko.xz` files in an initramfs, with `boot_args`
that omit firecracker's default `nomodule`. Inside such a guest, `k3s check-config` returns
**`STATUS: pass`**. No build, no root, no packages - and this is the single biggest de-risking of the
whole substrate.

**A guest kernel's module set is a MEASURED set, and `k3s check-config` cannot make that
measurement.** The image's module list is what the guest agent loads before k3s starts, and it is a
measurement rather than a preference. Measured on this host: with `xt_conntrack` absent from the
list, kubelet inside the guest failed to create the KUBE-FIREWALL chain - the one that drops packets
to 127.0.0.0/8 arriving from a non-loopback interface:

```
"Failed to ensure rule to drop invalid localhost packets in filter table KUBE-FIREWALL chain"
err="error appending rule: exit status 1: Warning: Extension conntrack revision 0 not supported,
missing kernel module? iptables: No chain/target/match by that name."
"Failed to initialize iptables rules; some functionality may be missing." protocol="IPv4"
```

and the same for IPv6 with `ip6table_mangle` absent. **`k3s check-config` returned RC=0 both before
and after.** It reads the kernel CONFIG file staged beside the kernel and reports
`CONFIG_NETFILTER_XT_MATCH_CONNTRACK: enabled (as module)` without ever asking whether that module is
LOADED. It audits the config the kernel was built with, not the kernel the guest is running - so a
node can be Ready, and a green check, with a firewall rule missing. iptables 1.8.11 (legacy) no
longer autoloads xtables extensions, which is why a module has to be NAMED rather than left to the
first rule that needs one. The instrument that names this class of defect is the guest console, and
nothing else.

The list is also bounded by what the kernel was built with, and that bound is not negotiable from
here: k3s tries `modprobe nft-expr-counter` and fails because `CONFIG_NFT_COUNTER` is not set at all,
so there is no module to add; it is harmless only because kube-proxy runs in iptables mode. Three
further check-config complaints (`CONFIG_RT_GROUP_SCHED`, `CONFIG_CRYPTO_GHASH`,
`CONFIG_INET_XFRM_MODE_TRANSPORT`) are compile-time gaps of the same kind.

**The same instrument found three more, and they were not cosmetic.** With `ipt_REJECT`, `ip6t_REJECT`
and `xt_nat` absent, kube-proxy's `iptables-restore` failed on the rules that target them, so the
in-cluster `10.43.0.1:443` had no ClusterIP, and every system pod that talks to the apiserver - the
provisioner and the metrics server - crash-looped on `dial tcp 10.43.0.1:443: connect: connection
refused`. The pods were scheduled and started, so the symptom is two services down rather than a
cluster that will not come up. iptables 1.8.11 (legacy) does not autoload these either.

**The list is complete because it is DERIVED, not because it grew - and the weaker instrument was
hiding the rest.** `iptables` inside this guest does not load an extension on demand, and
`iptables-restore` stops at the FIRST extension it cannot find, so discovering names by running and
reading the console is a treadmill: one name per boot, dozens of boots, and a guest whose firewall
is incomplete for as long as nobody looks. Both sides are enumerable at build time. The supply side
is the kernel's own tree: `modules.dep`, `modules.builtin` and `modules.alias`, which is the
kernel's `<ipt_|ip6t_|xt_><EXTENSION> -> module` table. The demand side is the guest's own console,
because a failed restore dumps its ENTIRE payload - 401 rule lines in one run - and the extensions
in it are the demand.

That derivation found two more, and the way it found them is the important part: `xt_NFLOG` and
`xt_physdev`, absent from the list while `-j NFLOG --nflog-group 100` and
`-m physdev --physdev-is-bridged` sat in kube-router's network policy payload.

**And with them missing, the three system pods reached `1/1` Running, with zero restarts, and stayed
there for 1m43s** - while kube-router's network policy controller aborted its whole sync. So pod
readiness is the WEAKER instrument for this class of defect, and a longer module list is not
evidence either. The acceptance is: pods Ready AND held for a stability window AND the guest's own
console free of a failed `iptables-restore` or an aborted policy sync. The failure is racy, too -
one of two boots hit it with the old list - which is what explains two agents measuring the same
three pods as `1/1` and as `0/1` with restarts and both being right.

The names that cannot be left to a same-name module are the ones to watch, and they are few: the
target `REJECT` is `ipt_REJECT` and `ip6t_REJECT` (there is no `xt_REJECT`); `DNAT` and `SNAT` are
`xt_nat` (there is no `xt_DNAT`); `NFLOG` and `physdev` are `xt_NFLOG` and `xt_physdev`. The builder
now refuses a config that names no provider for an extension the cluster's rules use, and refuses a
name the kernel does not carry, so this is caught at build time rather than one boot at a time. What
that cannot do is prove a name NECESSARY: five names have been proven by a measured failure -
`ipt_REJECT`, `ip6t_REJECT`, `xt_nat`, `xt_NFLOG`, `xt_physdev` - and the remaining ~43 are carried
for modes and matches these boots did not exercise.

**Why nothing loaded on demand: the kernel execs a modprobe
the rootfs did not have.** The guest boots the build host's Fedora kernel, built with
`CONFIG_MODPROBE_PATH="/usr/bin/modprobe"` (Fedora merged sbin into bin), over an Ubuntu 24.04 rootfs
whose modprobe is `/usr/sbin/modprobe` only. Every `request_module()` the kernel makes -- the
autoload of an nf_tables expression, an xt match, a logger -- exec'd a path that did not exist, so the
only modules a guest ever had were the ones `systemd-modules-load` or a userspace `modprobe` on PATH
loaded. Measured on the local market network (the plan 0005 M3 live gate):
`/proc/sys/kernel/modprobe` read `/usr/bin/modprobe` from a pod, `nft_limit` and `nft_log` were not
in `/proc/modules`, and kube-router's network policy controller aborted every sync with
`iptables-restore v1.8.10 (nf_tables): line 61: RULE_APPEND failed (No such file or directory): rule
in chain KUBE-POD-FW-*`; line 61 of the dumped payload is
`-m mark ! --mark 0x10000/0x10000 -j NFLOG --nflog-group 100 -m limit --limit 10/minute`, which
iptables-nft 1.8.10 writes as the NATIVE expressions `log group` and `limit` (`nft --debug=netlink`
on the same ruleset in a user+net namespace on the build host: `[ log group`, `[ limit rate`), and
those need `nft_log` and `nft_limit` -- not `xt_NFLOG` and `xt_limit`, which the module list carried
and which iptables-nft never asks for. So the derivation above was complete for the xtables path and
blind to the nf_tables one, and a deny-all NetworkPolicy left a pod reachable. The builder now reads
`CONFIG_MODPROBE_PATH` from the kernel config it stages and, when that path does not resolve inside
the stage (symlinks resolved within the stage, never on the build host), links it to the rootfs's own
modprobe (`/usr/bin/modprobe -> /usr/sbin/modprobe`), and refuses a rootfs with no modprobe at all
(`linkKernelModprobe` of the retired `cmd/socialweb-nodeimage`). With it, the same gate on a rebuilt
image: `nft_limit` and `nft_log` loaded on demand, no aborted sync on the console, and a deny-all
ingress policy refused the client 9 s after it was applied. The module list stays as a preload, not
as the only way a module gets in. `nft-expr-counter` still fails k3s's own modprobe and is harmless:
the counter expression is built into `nf_tables.ko` on this kernel (no `CONFIG_NFT_COUNTER`, no
`nft-expr-counter` alias, and the rules above carry `counter` expressions that load).

**And the same missing module is why a node's own cluster services die, which is the failure that
looked like a network problem.** `iptables-restore` is ALL-OR-NOTHING: when kube-proxy's ruleset names
the first extension the guest has no module for, the entire `KUBE-SERVICES` chain is dropped -
including the DNAT rule for `default/kubernetes`. A packet to `10.43.0.1:443` then leaves by the
default route and is refused, and every kube-system component that talks to the apiserver dies on it:

```
metrics-server            panic: unable to load configmap based request-header-client-ca-file:
                          Get "https://10.43.0.1:443/api/v1/namespaces/kube-system/configmaps/
                          extension-apiserver-authentication": connect: connection refused
local-path-provisioner    level=fatal msg="Error starting daemon: invalid empty flag
                          helper-pod-file ... Get ".../configmaps/local-path-config": refused"
coredns                   0/1, never Ready, same address
```

So a missing module presents as three unrelated services failing on one address, and the cause is
visible only in the guest's console 7.6 s in, before any pod needs it. The node itself is unaffected:
it registers, reaches `Ready`, and runs a pod - which is why this hid behind a green four-step proof.

**Two hypotheses were wrong here and are recorded so they are not re-run.** The first: that the
kubelet's LEASED port breaks these pods, because the ecosystem assumes 10250. Refuted three ways -
every failing and passing boot used the lease (`10251`); two boots identical but for the port both
reach `1/1` and hold; and structurally the apiserver takes the kubelet's address AND port from the
Node's own status (`status.daemonEndpoints.kubeletEndpoint`), with k3s starting its metrics-server
`--kubelet-use-node-status-port`, so 10250 is not assumed. The endpoint that looked like evidence,
`https://10.42.0.2:10250/readyz`, is the kubelet's readiness probe OF THE METRICS-SERVER POD, because
k3s gives that pod `--secure-port=10250` - it points at the victim, not the cause. The second: that
the failure was intermittent, because two agents measured the same three pods as `1/1` and as `0/1`.
The reconciliation is that the console reads differ by how much was read - the captured consoles are
bounded copies (~90-123 KB) and a failing run prints only the last 60-80 lines, so a tail read is
indistinguishable from a clean console. Neither agent's measurement was wrong; one of them was
looking at a truncated file.

**The kubelet port lease is retired.** Its recorded justification is that two k3s AGENTS as local
processes on one host contend for host-global ports (`listen tcp 0.0.0.0:10250: bind: address already
in use`), and the fourteen host-global resources measured alongside it are all properties of a node
that is a process in the host's network namespace - which is the substrate this design deleted. A node
is now a guest with its own kernel and its own netns: measured, nothing on the host binds 10250, 10248,
10249, 10251 or 10256 while a guest runs, the host's k3s runs `--disable-agent` and has no kubelet at
all, and pasta is started without `-t`/`--tcp-ports` so no guest port is published and the host cannot
initiate a connection to a guest. What kept the lease alive after that was that `perNodeNetwork`
derived each guest's address FROM the leased port, making it the only per-node-varying input the
composition root had. The leased CIDR replaced exactly that, which is what made the lease removable,
and it has been removed: `PurposeKubelet` is deleted, `kubelet` is in `retiredPurposes`, and the
guest's agent passes no `--kubelet-arg` at all, so the kubelet binds its own default inside its own
namespace. Nothing is lost by that, because no part of the ecosystem assumes a port: the apiserver
takes the kubelet's address AND port from the Node's own status
(`status.daemonEndpoints.kubeletEndpoint`), and k3s starts its metrics-server with
`--kubelet-use-node-status-port`. MEASURED: two boots identical but for the port (10250 vs 10251) both
reached `1/1` system pods and held. MEASURED ONCE MORE, with the flag removed entirely - one
uninterrupted run against a real `k3s server --disable-agent`: k3s passes NO `--port` argument to the
kubelet at all, the guest registered as a Ready node **12.9 s** after `InstanceStart`, its cluster's
three system pods (`coredns`, `local-path-provisioner`, `metrics-server`) reached **`1/1` with zero
restarts** 44.5 s after instance start and held for **1 m 47 s**, the node reports its own kubelet
endpoint as `{"Port":10250}`, `kubectl top nodes` reads it through that port (`215m` CPU, `169Mi`),
and the guest console is clean on every firewall signature. What that run also needed, and what
nothing enforces: the image had to be REBUILT first. The guest's kubelet argv is decided by the agent
inside the image, and the image's recorded inputs did not include that agent at the time, so a boot on
the previous image passed `--kubelet-arg port=0` - the removed field read back as Go's zero value -
and the kubelet refused to start with `port (--port) 0 must be between 1 and 65535`. The rule
underneath the lease survives it, restated as invariant 8.

**The guest never reaches a registry, which is stronger than giving it a resolver.** The image builder
pulls the images the cluster's own system components use - with `go-containerregistry`, as Go, no
daemon, no credential - and stages them as docker-save tarballs under `<k3s_data_dir>/agent/images/`,
where k3s's agent imports every tarball BEFORE it starts the kubelet. Measured: the import lines are
in the guest's own log at 27-150 ms each, and the whole console contains zero occurrences of
`ErrImagePull`, `ImagePullBackOff`, `no such host`, `dial tcp` or `failed to resolve reference`. The
system pods reach `Running` in a guest whose `/etc/resolv.conf` is still one byte, which is what makes
the property provable rather than merely likely - they cannot have fetched anything.

The list is explicit in the operator's config rather than derived at build time, and it WAS derived
from the shipped binary before being written down: read out of the manifests `k3s server` extracts,
with `pause` taken from a string inside the binary because no manifest names it. Five images, 61.5 MiB
staged and 62.0 MiB of rootfs, against a 1500 MiB headroom. Three of the resolved digests appear
verbatim as blob names inside upstream's own air-gap tarball for this version, which is independent
confirmation that the same images were resolved. One trap the verification caught: containerd
normalizes seeded names through docker's `ParseDockerRef`, so emitting `index.docker.io/rancher/...`
imports under a name no pod asks for - a silent `ImagePullBackOff`.

Two things this does not cover, both named rather than implied: the `svclb` image for a LoadBalancer
Service is created by the service controller on demand and cannot be found by reading manifests, so
the first tenant LoadBalancer Service will hang until it is added; and a k3s bump moves both the list
and the digests.

**pasta is the rootless network path, and it does hand out privileges.** Measured: under
`pasta --config-net` the child runs as uid 0 in a fresh user and network namespace with a full
capability set, and `ip tuntap add ... mode tap` SUCCEEDS there - while the same command in the host's
own namespace is denied. So a guest gets a tap, a bridge and an address without root, and it reaches
a HOST-LOOPBACK-ONLY listener through the gateway address. That is what lets the apiserver stay off
the network: bind loopback, advertise a host address the apiserver will accept for the in-cluster
`kubernetes` Service endpoint, and add the gateway address as a `--tls-san`.

**A guest joins, and this is now the measured destination rather than the direction of travel.** A
k3s agent inside a firecracker guest registers against the host's real `k3s server --disable-agent`:

```
liveweb-node1   Ready   <none>   0s   v1.36.4+k3s1
INSTANCE_START_TO_READY_S  7.4   10.6   9.5
```

Three runs. The time is measured ON THE HOST from the firecracker `InstanceStart` timestamp to the
moment `kubectl` first reports `Ready`, because a guest's own account of its own elapsed time is not
evidence: an earlier round of this work spent real effort chasing a reported 332-second boot that was
a literal `echo` in an init script, printed 0.29 s after the VM started. The spread is k3s's startup,
not the VM. Inside the guest, kubelet logs `Starting flannel with backend vxlan`, registers the node,
sets the pod CIDR, and the kubelet it started binds the port it is given - a leased `10251` in this
measurement, and its own default since the lease was retired (the kubelet lease paragraph above).

Supporting numbers on the way there: a `k3s server --disable-agent` on the host is ready in
**1.1-1.9 s**; the guest's namespace reaches the apiserver. And **a kernel is validated by restoring
it, never by comparing versions**: 6.18.48 cannot be restored at all on this AMD host (`Bad FPU state
... restore_fpregs_from_fpstate` -> panic). That is how 6.1.186 became the pin for the CI-kernel route
discussed below, a route this design then abandoned in favour of the host's own kernel - which is
what actually ships, which is why the guest that joined runs the host's kernel, and which turns out
to matter for snapshot restore, below.

**Boot and control-plane costs, recovered from the retired experiment tree before it went**, and
worth keeping because two of them closed lines of work rather than opening them:

```
kernel boot medians   0.270 / 0.341 / 0.336 s   6.1.141 / 6.1.186 / 6.18.48
kernel sizes          41.9 / 45.0 / 27.8 MB     spread 0.07 s
  -> KERNEL CHOICE BOUGHT NOTHING. The 27.8 MB kernel is not faster than the 45 MB one.
root device           initramfs 0.303 s   ext4 0.763 s   squashfs 0.763 s   (delta +0.460 s)
  uncompressed 1.44 MB cpio ties 0.86 MB gzip cpio
  -> the cost is the virtio-blk DEVICE plus the mount path, NOT decompression, so compressing
     the initramfs harder buys nothing
snapshot              create 168.24 ms, state 31 KB, mem 512 MB; loads 5.81 / 6.16 ms
control plane         cold 26.79 s   PRE-SEEDED 1.28 s   restart 1.23 s
                      SIGTERM -> exit 0.50 s, first listener ~0.11 s, 503 -> 401 gap 26.68 s
  -> the 26.79 s was attributed to k3s GENERATING ITS CAs, and that attribution is REFUTED by a
     control experiment on the process substrate. A rebuild that must author its own CAs pays
     within +/-0.24 s of one that is handed them -- sometimes FASTER -- so there is no ~25 s of
     CA authoring to collect, and k3s's own CA generation is ~0.2 s here. What the cold figure
     actually contains is a fresh DATASTORE: 3.7-5.6 s of it disappears when only the store is
     warm and the bundle is absent. The 1.28 s pre-seeded figure reproduces (1.135-1.139 s); the
     26.79 s cold one does NOT (4.97-6.75 s), so the two were measured under different store
     conditions and comparing them was wrong. The identity bundle's measured value is the
     IDENTITY it preserves -- a CA fingerprint byte-identical across a teardown and rebuild,
     where a control's changed every time -- not a time saving. Without it every rebuild is a new
     cluster under the same name, and every prior kubeconfig and ServiceAccount token stops
     validating silently.
flannel recovery      killing agent B leaves A's pods Running again 75 s later; 6 veths on cni0
                      shared by both clusters' pods
```

**Restoring a snapshot instead of booting is REFUTED as the provisioning primitive, and the numbers
are not close.** Measured on this host against the same image:

```
cold boot          InstanceStart -> Ready   9.079   9.158   10.768 s
snapshot freeze    with the agent running   0.698   0.719     0.731 s
snapshot load      firecracker's own count  3.96 ms   (0.247 s for the whole path, pasta included)
LOAD -> Ready                               9.974 s
```

The load is **2.6 % of the operation**. Everything else is k3s reconnecting and re-reporting itself,
so a 4 ms restore buys a 250 ms head start on a 9.5 s job. Counted honestly from the other end it is
worse still: a node whose VM dies is not noticed by the cluster for **53.9 s**, so a cold boot from
nothing beats waiting for the monitor and then restoring.

**And it cannot make a NEW node at all, which is the part that kills it.** Restore does not re-run
boot-time units, so the guest's own identity adoption never happens: `kcp-agent.service` does not
re-run, `mountNodeDisk` and `adoptIdentity` never run, and no agent start marker appears in a restored
console. A snapshot frozen as `liveweb-node1` - with its recorded node-disk path deliberately
re-pointed at another node's disk before the load - comes back as **`liveweb-node1`**. So restore can
thaw a node the cluster still knows and nothing else. Two consequences follow directly: a control
plane can only ever scale a restored node back up, never out, and **a deleted Node object is never
re-created**, because the kubelet only retries `updateNodeStatus` against a node that is not there and
makes no registration attempt. Since a snapshot is also valid only for the control-plane INSTANCE it
was frozen against (the join token is per instance), the mechanism's scope is one node, one name, one
cluster incarnation.

Two facts worth keeping even though the mechanism is gone. **A per-node disk CAN be attached to a
restored guest**: a drive `PUT` whose backing file exists is accepted and the load that follows
succeeds, so the constraint is not that the drive slot must exist in the snapshot. What is true is
that a drive `PUT` which FAILS leaves the VMM unable to load *any* snapshot (`Loading a microVM
snapshot not allowed after configuring boot-specific resources`), which is a nasty failure shape:
one rejected device poisons every later attempt. And **restorable FPU state is a property of the
kernel, not of the guest**: restoring the host's 7.2.7 kernel panics on the guest's first syscall
return (`exc_general_protection` in `restore_fpregs_from_fpstate`, reached from `switch_fpu_return`),
and `noxsave` removes the panic on this kernel - an earlier note that `noxsave` merely converts the
panic into a hang was measured on 6.18.48 and does not hold on 7.2.7. That leaves two requirements in
direct conflict: k3s needs the host kernel's netfilter symbols, and a restorable guest wants a kernel
whose FPU state this host can actually restore. `noxsave` is an escape hatch, not a resolution, and
its effect on a k3s workload is unmeasured. Firecracker v1.13.1 also carries the pre-1.15.1 kvm-clock
bug: a restored guest's monotonic clock jumps forward by the pause duration.

**Two failures that presented as plumbing, and were one cause: the `pasta` child inherits pasta's own
SELinux domain.** `/usr/bin/pasta` is `pasta_exec_t`, so a child of it runs in `pasta_t` unless the
program it execs is a type whose own policy transitions it away. In `pasta_t`, `ip tuntap add` is
denied `CAP_NET_ADMIN` and `open` on a console file in `/tmp` is denied - and, the detail that made
this expensive, `ifconfig_t` cannot write to the pipe `pasta_t` owns, so the error TEXT of both
failures was silently discarded and arrived as an empty string. `pasta link in namespace: ""` and a
permission error with no message were the same bug. The child is now `/bin/sh -c 'exec ...'`, and
**the reason that works is not the one this document first gave.** Measured: on this host `/bin/sh`
is `system_u:object_r:bin_t:s0`, NOT `shell_exec_t` - `/usr/bin/bash` is the `shell_exec_t` one. What
actually holds is that the gateway itself runs in `unconfined_t`, so exec'ing a `bin_t` file does not
transition at all. The fix is incidental rather than principled: a gateway deployed under a systemd
service domain would get the original failures back, including the silent one.

What survives the domain changing is a POSITIVE PROBE rather than a label. The namespace is treated
as configured only once the child has been observed to find pasta's own upstream link, and nothing is
mutated before that, so a child without the privileges the work needs fails at the observation
instead of half-configuring a VMM. Its power is bounded and worth stating: it refuses a child that
was unshared WITHOUT pasta's configuration, but a child that was never unshared at all sees the
host's own interfaces and would pass - so it establishes "unshared, with pasta's config" rather than
"provably pasta's namespace". A failure that writes nothing on either stream is also now a failure
that names its own emptiness, because the original bug was exactly that: a denial whose account was
denied too.

**pasta also hands its child a PID namespace.** The guest-side code reported its VMM as pid 17 while
the host saw 2188474, so a record naming that pid named nothing. The record now resolves the HOST pid
from its own identity, so the record, the startup sweep and the liveness lookup agree by construction
rather than by three callers happening to do the same thing.

**One writable ext4 serving as both template and instance was a correctness bug, not hygiene.** The
guest's writes - k3s's extracted data dir, `/etc/rancher/node/password`, generated keys - persisted
into the file the next boot read, and the next boot failed `k3s check-config` with
`sha256sum: does not match`. Two VMs on one ext4 would also have corrupted each other. The image is
now a TEMPLATE and every node gets its own clone at `<dataDir>/rootfs.ext4` (`cp --reflink=auto`,
free on this filesystem), removed on teardown. Overlayfs is the alternative; reflink needs no
guest-side mount work and no extra drive.

**A host that died at its first step and a host that is merely slow look identical until the clock
runs out.** The guest's console is read for named fatal markers (`Kernel panic`, `KCP_INIT fatal`,
`KCP_PREFLIGHT fatal`, `KCP_AGENT fatal`, `KCP_AGENT_EXIT rc=`), and confirming a join returns
immediately with the guest's own words instead of waiting out the timeout. This is what turned a
missing k3s binary from a four-minute mystery into an instant, named error. The guest agent writes
the same account to `/dev/console` directly as well as to a file, so a dead or rate-limiting journald
cannot turn a guest's only report of its own failure into silence.

**A host prerequisite that cost real time:** the repository directory carried the SELinux type
`user_tmp_t` instead of `user_home_t`. Under pasta's namespace that makes binaries built in the tree
unexecutable and repo paths unreadable, and it presents as `permission denied` on unrelated-looking
operations. `restorecon -R` on the tree and its parent is the fix, and a host that runs this substrate
should be checked for it.

**What the VM substrate requires, and what it costs.** Firecracker's CI kernels are under a
DATE-based artifact prefix; a version-numbered prefix such as `firecracker-ci/v1.13/` is STALE and
still served, and a year-old kernel from it BOOTS PERFECTLY, which is exactly why the mistake is
invisible. Pin by the newest date prefix.

The current kernels carry `VXLAN=y`, `NF_TABLES=y`, `VETH`/`BRIDGE`/`TUN=y` and `OVERLAY_FS=y`, so
k3s's networking is reachable - with ONE hard exception: `CONFIG_NETFILTER_XT_MATCH_COMMENT` is
UNSET, and kube-proxy's iptables mode appends `-m comment --comment` to essentially every rule it
writes. It is on kubeadm's required-kernel-config list for that reason. That left two routes - build
a kernel, or build `xt_comment.ko` against the 6.18 CI kernel (the only one with `CONFIG_MODULES=y`)
and ship it in the rootfs - and BOTH were abandoned for the cheaper route stated above: boot the
host's own kernel with the host's own modules. What survives from this finding is the rule rather
than the route: **a guest kernel's module set is a MEASURED set**, and the measurement is
`k3s check-config` run inside the guest, plus reading the guest's own console for the iptables
failures that check-config does not catch. `CONFIG_IP_VS` being unset is irrelevant: ipvs mode is
deprecated and being removed.

Firecracker's DEFAULT cmdline passes `nomodule`, so a distro kernel whose virtio drivers are modules
needs both a `boot_args` override and an initramfs - which makes building a kernel the better route,
not the worse one. `VIRTIO_FS` is unset in both kernels, so a virtiofs root is not available.

The guest needs a REAL init: k3s runs the kubelet with `--cgroup-driver=systemd`, so systemd must be
managing cgroups inside the guest. A busybox-init guest proves rootless boot and networking and is not
the shape a node needs.

Networking is rootless either way; what changes is whether the host's apiserver has to leave loopback.
With a userspace gateway such as `pasta` the guest reaches the host's loopback services through the
gateway address, so the listener stays on loopback and only the SAN grows: `--tls-san` carries the
gateway address the guest dials. That is why bind and advertise are now separate values.

The point of the VM is not isolation for its own sake: it is that every one of those host-global
resources is PER GUEST inside it, so N nodes fit on one host where a local process allowed one. The
node contract above is unchanged by the substrate - one long-lived command (the VMM), closing it is
the teardown, nothing is read back out, `--node-name` is set from the `(cluster, node)` pair and the
token still travels as a `0600` file.

**The seam must not give the dial lifecycle ownership.** If `Close` also destroys, then `Wait` and
`Close` become indistinguishable in effect, and the contract above collapses: the node is supposed to
die BECAUSE THE COMMAND EXITED, not because we asked. The refusal was therefore explicit, and it lived in a control that nothing reached: the package that
carried it, `internal/nodetransport`, is DELETED, because a control nothing reaches is
indistinguishable from a control that is not there. The rule it enforced is kept here as a rule,
which is where it always belonged: **the substrate must not destroy the node it dials.** "The
substrate must not destroy this" has to be a value a caller is handed, never a convention a caller
has to remember. A lifecycle-owning dial puts a destroy inside the transport, which is precisely
where nothing asserts it is never called.

Note the control's reach, which is why it is gone: nothing in the tree ever imported it, so that
refusal is recorded in section 15 alongside the package that carried it. A control nothing reaches is indistinguishable from a control
that is not there, which is why it is stated here as a rule rather than trusted as a guard.

The token never rides argv: it goes in a `0600` file named by `--token-file`. An argv value is
readable from `/proc` by every local user, and k3s derives its bootstrap storage key from the join
token, so a leaked token is a leaked cluster.

**For a guest the token must not ride the kernel command line either, and the reason is not obvious.**
The command line reaches a console TWICE: firecracker logs the body of its own `PUT /boot-source`
request, and the kernel prints `Kernel command line:`. A `kcp.token=...` argument is therefore
written to `guest.console` in plaintext, twice, every boot. The per-node secret travels instead on a
per-node read-only virtio-blk disk, written `0600` on the host at provision time and destroyed at
teardown, carrying the join token and the `(cluster, node)` identity. Nothing secret reaches a
console, k3s still takes it from `--token-file`, and the disk is what makes a snapshot CLONE-able:
the image stays generic because the per-node half is injected.

**The join token is also the node's k3s password, written to three paths, and that reuse is recorded rather than latent.**
`adoptIdentity` writes the same `node.Token` to the file k3s is handed by `--token-file`, to
`/etc/rancher/node/password` and to `<data-dir>/agent/node-password`: k3s reads the second as the
node password it presents when it registers, so one value is both the cluster join secret and the
credential the kubelet authenticates with. The v1 ceiling is stated rather than hidden: every node
of a cluster shares one node password, which is harmless at 0-or-1 nodes and is inherited by the
first multi-node change, where a per-node password must be minted instead.

**The image is a directory, not a file.** It holds `vmlinux`, `initramfs.cpio`, `rootfs.ext4` and
`image.json`, and the boot line travels WITH the kernel inside `image.json`, so a kernel and the command line
that suits it cannot be separated by an operator choosing two paths. The builder refuses to write a
2 GB image that is missing k3s, the agent, the guest document, the units or the kernel config, which
fails in seconds instead of after a boot.

**The record's identity is supplied by the substrate, and it is not a local-process idea.**
The retired `internal/noderecord` matched a token SUBSET against `/proc/<pid>/cmdline` after confirming the pid's
start time, and a record whose identity does not match is NOT OURS and is never signalled - a pid
reuse must stay a refusal, not a kill. A local agent is `{"agent", nodeName}`; a VMM is
`{"firecracker", "--api-sock", <sock>}`, and the socket path is per node, so one node's record can
never match another's VMM. An empty identity matches nothing rather than everything, which is the
only safe reading of "every token present".

**The guest runs with SELinux off**, `selinux=0` on its command line and `SELINUX=disabled` in the
root filesystem where the base image carries a config. A node's guest boots the host's OWN kernel,
which is built with SELinux, while its minimal root filesystem has no policy to enforce, so leaving
it on is at best noise and at worst an unlabelled denial that surfaces as a permission error
somewhere unrelated to its cause - which is exactly the shape of the two `pasta_t` failures above.
The security boundary for a node is the microVM, not SELinux inside a guest that has no policy. This
is guest-only; the host's policy is untouched.

`k3s server` is deliberately NOT a node: it runs gateway-side with `--disable-agent`. Only the worker
scales to zero.

**A Node's cloud identity is the cloud controller manager's, and it runs on the host.** A guest joins
as a Kubernetes Node with no cloud identity: an empty `spec.providerID` and no labels for an
autoscaler to group by. The controller stamps each Node it observes with
`spec.providerID = socialweb://<cluster>/<node>` and the label
`node.socialweb.computer/cluster=<cluster>`, and deletes a Node whose instance record is absent -
the guest is gone. It reads the guest set from `<DataDir>/nodes.json`, which the provider writes
(atomic temp-plus-rename) on every provision, teardown and reap, and once at startup as `[]`.

Two things about that snapshot are load-bearing and neither is obvious. An UNREADABLE snapshot is an
error and patches nothing, because invariant 14 applies and deleting a Node out from under its own
running work is the cost of getting it wrong. But an ABSENT snapshot and an empty array are the same
answer to the reader - `(known=false, err=nil)`, the delete path - so writing `[]` protects nothing
on its own; what protects a live Node is that the snapshot LISTS it. The file is infrastructure and
not a cache, and the ordinary path is what keeps the absent case out of reach: the provider writes it
before the controller can be asked anything.

The identity is SCOPED, never the superuser, and the provider grants it `get`, `list`, `watch`,
`patch`, `update` and `delete` on `nodes` once k3s is up. That grant is not decoration: a scoped
identity with no binding is authenticated-and-denied, so the informer never syncs, the readiness gate
times out, and the failure reads as a controller that will not start rather than as a missing
binding.

### A market node joins through its cluster's OpenBao, never through ssh

Plan 0005 M3-SECRETS-TICKET, M3-UNITS and the gateway half of M3-NODE-LOSS, built to the plan's
`m3_contract`. The gateway holds no connection to a market guest and opens no ssh session to one; the
guest dials its cluster through the cluster's ClusterLinks and reads its join secrets from the
cluster's OpenBao. Decisions not in the contract are marked "decided by Claude, user may veto".

**What the NodeClaim controller and the Requester do per claim** (`internal/controllers/nodeclaim`
with the mapper of `internal/gateway/nodeclaim`, bound by the GatewayAPI component; the Requester
component, `internal/services/requester` over `internal/market/requester`):

The NodeClaim controller maps. It runs no market flow, holds no PDS session, no claim key and no
secrets key, and since plan 0031 it writes no ComputeContract. Each Cluster with a NodeClaim has one
Infra `nodes-<first 20 hex of sha256(<home workspace>/<cluster>)>` in the Gateway application's own
workspace (`spec.clusterRef` names the Cluster, label `infra.socialweb.computer/cluster`; no
ownerReference, because kcp garbage collection is per logical cluster). The NodeClaim controller is the
only writer of its spec: one entry `node-<first 12 hex of sha256(claim uid)>` per claim, Staged while
the join bundle is not written, Requested once it is, Released when the claim is deleting or ended,
with the node fields (`ownerDid`, `adopt`, `nodeJoin`, `nodeClaimRef`) in `spec.nodes` and the bundle
in `spec.secrets`, both kcp-only. The Infra controller applies the entry's ComputeContract
`infra-<infra>-<entry>` and runs its lifecycle; the NodeClaim controller reads that contract by name,
maps its status back onto the claim and names the entry in `status.infra`. A claim that is gone drops
its entry; an Infra with no entry left, or whose Cluster is deleting, is deleted, and its finalizer
releases every node. The Requester component of the same application carries the flow.

- *NodeClaim controller, per claim.* It names the entry's contract `infra-nodes-<20 hex>-node-<12 hex>`, puts the
  finalizer on the claim, reads the ClusterLink `<clusterKey>-openbao` from the claim's own workspace
  (a link with no `status.ticket` fails the claim before any market
  round) and the cluster OpenBao CA (`<dirname(spec.tokenFile)>/openbao/tls/ca.crt`, or the path the
  provider publishes), and writes the join bundle as one KV entry
  `contracts/<contract>/bundle` in its own vault mount. The bundle holds the files
  `/etc/socialweb/node/openbao-ticket`, `openbao-ca.pem` and, when the `<clusterKey>-apiserver` link
  is listening, `apiserver.env` (`NODE_NAME`, `ADVERTISE_ADDRESS`, `APISERVER_PORT`,
  `APISERVER_TICKET`). None is a credential: the tickets are the links' public halves and the CA is
  public. The entry, and so the contract, carries `spec.vmName` (the node name), `spec.ownerDid`, `spec.secretsRef` (the
  bundle), `spec.nodeJoin` (the cluster key and the pinned socialweb-meshd build), the bidders to
  admit and deny, the policy and `spec.vmReadyTimeoutSec`.
- *Requester, per contract.* The Requester watches ComputeContracts, admits at most
  `maxConcurrent` of them and `perAccount` of one account (`RequesterSettings`), and runs the RFP flow
  for each under the owner account's delegated session; it reaches the owner's repo and the vault
  only through bus handles (`internal/services/requester/wiring.go`). The node-join contract adds the `k3s` and k3s-join user-data
  modules and wraps the secrets capability: on the grant it writes `status.grant`
  = `{issuer, subject, audience: api://socialweb-node?actx=<clusterKey>/<node>, role: <node>}` and
  appends `claim.json` = `{node, cluster, jwtMount, role, audience, kvMount, kvPath}` to the bundle
  (`jwtMount` = `jwt-<first 12 hex of sha256(issuer)>` needs the winning bid's issuer). The wrapped
  capability's RBAC record also names a role `<role>-node` whose `aud` is that audience, so the bidder
  keeps a GuestIdentityGrant (api `socialweb-node`, actx `<clusterKey>/<node>`) and the guest can get the
  token; an audience that is not `api://<api>?actx=<actx>` gets no grant. The cluster vault's JWT role
  binds `wid_sub` (the user claim) with `bound_claims`, because the `sub` of an OpenBao identity token is
  the entity id. A grant write
  that fails leaves the inner capability unauthorized and the contract `Failed`, releasable from its
  receipt. The Requester waits up to `vmReadyTimeoutSec` (default 120) from the accept for the guest to fetch its
  bundle (`status.secretsFetchedAt`), then writes phase `Held`; a guest that never fetches ends
  `Failed` with the reason and keeps its receipt.
- *Mapping.* `Requesting` is claim phase `Bidding` until the accept, then `Provisioning`; `Held` with
  `secretsFetchedAt` is `Joining`; a contract `Released` with `status.contract.termination` is claim
  `Lost`; `Failed` is `Failed` with the contract's message. `status.grant` and `status.contract` are
  copied on every transition; a claim in `Joining` or `Running` changes only on `Lost`, `Failed` or
  `Released`, so the provider's `Running` is never overwritten.
- *Release.* When a claim is deleted the NodeClaim controller sets `spec.release` on its contract
  (`Mapper.Settle`), waits until the contract is `Released` or `Failed`, then deletes the contract and
  the bundle and removes the claim's finalizer (claim phase `Released` when the VM was released or
  none was held, else `Failed`). A contract that is deleted while it holds a VM releases the VM first.
  A contract whose claim is gone is released or deleted as an orphan (`Mapper.Orphan`).
- *Adoption.* A claim the provider recreates for a node that already joined (`Adopting`) becomes a
  contract with `spec.adopt`: no RFP; the Requester finds the receipt and the bidder-signed
  `vm.hostKeys` in the owner's records and holds it `Held` (`contractRunner.runAdopt`), which the
  NodeClaim controller maps to `Running`.

**A restarted gateway loses nothing.** Every step the Requester takes is in `status.contract` of its
contract; a restarted Requester resumes each contract from it (no second RFP) and a contract that
already fetched its bundle is only watched for the bidder's `vm.terminated`. A restarted NodeClaim
controller finds each claim's contract by name and maps it again.

**A restarted provider never reaps; it adopts** (M3-NODE-LOSS, provider half). Decided by Claude,
user may veto. Before this, the provider's shutdown reap (`nodeHosts.reap`) called the market host's
`Reap`, which DELETES the claim, so every provider stop (every deploy) made the gateway destroy every
market machine; and a provider that started held nothing, so a node that survived anyway was never
confirmed, lost, scaled in, torn down or counted in flight, and its name could be claimed again.
Now:

- *Shutdown leaves market nodes.* `nodeHosts.reap` returns without touching a market host (logged
  `leaving the market nodes running for the next provider process`). The guest needs no session of
  ours; its agent and links are `Restart=always` and reconnect when the next process brings the
  cluster's group back. A cluster switched OFF or deleted still releases every node
  (`reapCluster`), and the guest substrates still reap on shutdown, because their guest is a child
  of this host.
- *Adopt at startup, before the first pass.* `join`/`joinAll` call `nodeHosts.adopt` for every
  cluster right after the empty instance snapshot is written, so the snapshot names the adopted
  nodes before the cluster's cloud controller manager starts (it deletes every Node the snapshot
  does not name). A failed adoption is a warning and the next pass retries.
- *Adopt on every pass, from the list it already reads.* `nodeHosts.reconcile` lists the claims of
  the cluster's home workspace each pass for the `Released` rule; the same list goes through
  `adoptClaims`, so any live claim of the cluster that nothing holds is held from that pass on. No
  extra kcp read per pass. An OFF cluster, which gets no scale pass, adopts once
  (`adoptOnce`, one list until a reap that acted resets it) before `reapCluster`, so a cluster
  switched off while the provider was down still releases its machines.
- *What is adopted.* A claim whose `spec.cluster` is the cluster key, whose name carries the
  `<cluster>-node<N>` ordinal, whose phase has not ended (not `Failed`, not `Released`) and which
  is not being deleted. Pending, `Bidding` and `Provisioning` claims are adopted too: the NodeClaim controller owns
  their end, and the given-up rule tears them down when it writes `Failed`. An ended claim is never
  adopted; the `Released` rule still deletes its Node.
- *How.* A `nodemarket.Host` is built for that name exactly as `Provision` would build it, then
  `Host.Adopt(claim)` marks it provisioned WITHOUT writing the claim, the policy or the secret: the
  claim exists, so its policy was written before it (Provision writes the policy first); a
  `Joining` or `Running` claim's guest already fetched its secret, so the secret is marked written;
  an earlier phase's secret may have been held back for a link, so it is written on the next
  `SyncSecrets` (an identical rewrite at worst). The JWT role is ensured once from `status.grant` on
  the next pass, as for any node whose grant this process has not yet seen. The host is retained at
  its ordinal, marked live, counted in `started` (so `Provision` never takes the primary path over
  an adopted `node1`), and the snapshot is rewritten. A never-provisioned primary host is dropped
  when adoption creates or meets the cluster's entry, so it cannot be counted in flight.
- *Then every rule applies unchanged.* confirm (`ConfirmJoined`, `markJoined`), the node loss rule,
  per-node scale-in (`Held` is set from the held names), teardown to zero, `inFlight` (an adopted
  `Joining` claim with no Node is in flight, a reported one is not, so each adopted node counts once)
  and `nextOrdinal` (held names are present, so the next claim is named past them).
- *Not proven live:* a prod deploy restarting the provider with market nodes up, the guests
  re-registering within the minute the loss rule allows after the cluster's group comes back (the
  node lifecycle controller starts the NotReady clock when the group restarts, not when it stopped).

With the gateway half above, both the gateway and the provider can restart without reaping a live
market node (`internal/providermain/adopt.go`, `internal/nodemarket`).

**What the guest runs** (`internal/atproto/cloudinit`, `K3sJoinModule`, built in Go). The module writes
the resolver, the sysctl file, `/etc/socialweb/node/join.json`, the unit files and one pinned-download
bootstrap shell. The bootstrap fetches `/downloads/<sha256>/socialweb-node-<arch>` from the gateway's web app,
checks the sha256, installs it 0755 and execs `socialweb-node units start`; everything after that is
the one Go binary `cmd/socialweb-node` (`internal/nodeagent`), built on the shared
`internal/systemdunit` library that HostProcess/hostsvc also use. The pin is the contract's
`spec.nodeJoin.nodeAgent` (`ComputeContractNodeJoin.NodeAgent`), filled from the
`socialweb-node-x86_64` and `socialweb-node-aarch64` downloads the web app serves. Every unit carries
`StartLimitIntervalSec=0`.

| unit | after | does |
|---|---|---|
| `socialweb-node-bootstrap` | `network-online` | fetch and verify the pinned `socialweb-node`, then `units start`; oneshot |
| `socialweb-node-prep` | `network-online`, `setup-secrets` | `socialweb-node join`: workload-token exchange, OpenBao JWT login, KV read, the `0600` files and `meshd.env`; then supervise the OpenBao and apiserver dumbpipe links |
| `socialweb-node-dns-link` | prep | `169.254.20.10/32` on `swdns0` |
| `socialweb-pod-masquerade` | prep | `net.ipv4.ip_forward=1` and the cluster-CIDR-only SNAT rule |
| `socialweb-pod-cni` | prep | poll the Node's podCIDR, link the CNI plugins, write the bridge conflist with `encoding/json` through a rename |
| `socialweb-node-diagnose` | prep | print containerd's CNI status to the console |
| `socialweb-meshd-install` | prep | fetch the pinned meshd from the gateway's web app, verify and install it 0755 |
| `socialweb-meshd` | install, prep | run meshd with the kubelet kubeconfig and secret |
| `socialweb-k3s-agent` | prep, DNS link, masquerade | `k3s agent` as before |

The join's credentials are fetched by the Go agent itself: it runs `guest-login.sh token <audience>` for
the claim's audience, logs in at `/v1/auth/<jwtMount>/login` with `{role, jwt}` and
`--cacert openbao-ca.pem`, reads `/v1/<kvMount>/data/<kvPath>`, and requires `token`, `nodePassword`
and `serverCA`. It writes `server-ca.pem`, `token`, `node-password` and `apiserver.env` under
`/etc/socialweb/node/` and `/etc/rancher/node/password`, all `0600`; the token is the `K10` form unless
the stored token already starts with `K10`. The advertise address, port and ticket are validated before
they reach a unit's command line. The k3s agent is ordered after prep, the DNS link and masquerade, and
never after meshd, its install or the CNI: those wait for the Node the agent registers.

Gone: the `lib/cloud-init-plugin-k3s-join` TypeScript module, its three `0700` scripts and four units.
The separate `setup-secrets`, k3s-install and iroh modules remain cloud-init shell modules; the
join path's own logic is the Go binary.

**MEASURED locally end to end, 2026-10-07** (this reading predates the Go node agent; section 15 records
the rewrite's reading; the plan 0005 M3 live gate, a Go test that plan 0026 deleted,
PASS 753.57s, finished 23:55:13Z; the vendored market network held up by `accept.sh`'s `ACCEPT_HOLD_DIR`,
and the deployed control-plane container `socialweb-k8s:m3live` built from the tree running against it, so
prod's own entrypoint and provider are what was measured). One Deployment on a tenant cluster took the
whole path: both ClusterLinks Listening and the cluster Ready 23:49:18Z, the claim written 23:49:21Z after
them, `status.grant` 23:49:24Z, the OpenBao JWT role the same second, a firecracker guest booted from the
RFP's cloud-init 23:49:27Z, the `--secrets` bundle fetched 23:49:32Z, the OpenBao JWT login over the
openbao link and the KV read 23:49:37Z, the tenant Node Ready with the workload pod Running on it and the
claim `Joining` 23:49:47Z - 29s after the Deployment was applied - then a control-plane restart with the
node up, which adopted the claim (same uid, node Ready, guest up, no `vm.delete`) for 2m30s, and then the
workload's deletion, the claim's deletion, `vm.delete` accepted at 23:55:11.733Z, the guest gone 1s later
and the cluster back to zero with the control plane Ready. Zero ssh sessions: no line of the web app's log
names one, and the guest's sshd records only the compute provider's own unauthenticated pre-auth TCP
probe of port 22. Three defects, all found by this gate and all closed with tests watched to fail first:
the web app of the time re-derived a claim's cluster key instead of taking `spec.cluster` from the provider (87562e8,
main 657a915); the requester waited out its firehose when a bidder that had already dispatched the accept
answered `submitAccept` with no receipt, ending `receipt_invalid` with a VM nobody could release
(atproto-market a1b467e: the winner's repo is asked for the `market.receipt` naming this accept); and
`setup-secrets.sh` appended `==` to every JWT payload, which GNU `base64 -d` rejects when the payload is
0 or 3 mod 4 (atproto-market 117ab30). Two things the gate had to name rather than assume: the image
store's newest image had been rebuilt with no preinstall manifest and so carried no cloud-init, while the
bidder reuses a stale image when it has none for its configuration (the gate now reuses only images that
carry cloud-init and refuses to run when none does); and a node's bridge conflist has to land in
`/etc/cni/net.d` with the plugins linked into `/opt/cni/bin`, because under `--flannel-backend=none`
containerd reads the stock directories rather than k3s's agent directory (3919df6).

**What the NodeClaim controller needs from the provider** (the interface, as built):

1. The ClusterLink `<clusterKey>-openbao` in the claim's home workspace with `status.ticket` set
   (a bare dumbpipe ticket) BEFORE the claim is written; otherwise the claim fails at once.
   The ClusterLink `<clusterKey>-apiserver` is read the same way and delivered to the guest in the
   same bundle when it holds a ticket, which it does on every cluster the provider rents nodes from;
   when it does not, the guest reads that ticket from the KV entry below instead, and joins serially.
2. A published path for the cluster OpenBao CA (a `NodeClaim` spec field such as `openBaoCAFile`, or
   a ClusterLink status field), or the guarantee that `<dirname(tokenFile)>/openbao/tls/ca.crt` is it
   and is readable by the gateway's core.
3. KV `socialweb-nodes/<node>` with `token` (bare, or the full `K10` form), `nodePassword`, `serverCA`
   (the exact bytes of `server-ca.crt`, since k3s pins its hash), `apiserverTicket` (bare),
   `advertiseAddress` (IPv4) and `apiserverPort` (number or digit string).
4. JWT mount `jwt-<sha12(status.grant.issuer)>` and role `<node>` bound to `status.grant.subject` and
   `status.grant.audience`, as the contract says, and an OpenBao serving certificate valid for
   `127.0.0.1`.
5. `nodemarket.ConfirmJoined` accepts a claim in `Joining` or `Running`; the NodeClaim controller
   writes `Joining`, and the provider confirms the join from the tenant Node.

### Pods reach pods on other market nodes through socialweb-meshd

Market guests cannot reach each other: each sits behind its own NAT or pasta, so k3s's flannel
overlay has nothing to run over (the limit "A node cannot reach another node" in NEXT.yaml). The
user's decision: QUIC datagrams over iroh, a tun device, each peer Node's podCIDR routed to that
Node's iroh endpoint id, k3s with `--flannel-backend=none` and a bridge CNI for the node-local half.
`rust/socialweb-meshd` (a cargo workspace at `rust/`, the first Rust in this repo) is that daemon.
It is built and proved on hat-8, and the Go cloud-init module installs it on every market guest (the integration, below).
A mesh probe
scaled `test-cluster-alice-0001` from 0 to 2 market nodes, pods reached pods across the nodes both ways,
and the node without CoreDNS resolved through 169.254.20.10; a NetworkPolicy denied cross-node traffic.
The binary ships the meshd downloads (item 6
below), and the first two-node proof on the binary path is in section 15.

**Identity.** One ed25519 secret per node, read from `--secret-file` as 64 hex characters (the
`IROH_SECRET` encoding dumbpipe and the cluster links use), generated 0600 through a rename if absent.
The endpoint id is the public key in lowercase hex; the did:key is multicodec `0xed01` + key,
base58btc, `z` prefix - the same spelling as `internal/didkey`, and the Rust tests carry the same two
vectors (the did:key spec vector `did:key:z6MkiTBz1ymuepAQ4HEHYSF1H8quG5GLVVQR3djdX3mDooWp` and the
dumbpipe v0.39.0 `IROH_SECRET` -> endpoint id vector). The secret is node-local and never leaves the
guest: a node's mesh identity lives and dies with the guest, so nothing about it goes through
OpenBao. `socialweb-meshd identity --secret-file F` prints `{endpoint, did}`.

**Peers are Nodes.** With `--kubeconfig` (the kubelet's own, `/var/lib/rancher/k3s/agent/kubelet.kubeconfig`)
meshd merge-patches its own Node with `socialweb.computer/mesh-endpoint=<hex>` and
`socialweb.computer/mesh-did=<did:key>`, lists and watches all Nodes, and relists (rebuilding the
client, so a rotated kubelet certificate is picked up) after every watch. The route table, computed
by `RouteTable::compute` from a Node list and unit-tested on Node lists:

- every OTHER Node with `spec.podCIDRs` (else `spec.podCIDR`) and a valid `mesh-endpoint` is one route
  per pod CIDR to that endpoint; a Node missing either is skipped silently (it has not joined yet);
- this node's own pod CIDRs are never routed, and come from its own Node object;
- rejected and reported in `/status`: a malformed endpoint, an endpoint equal to this node's own, a
  `mesh-did` naming a different key than `mesh-endpoint`, a pod CIDR overlapping this node's, and
  BOTH of two peers whose pod CIDRs overlap;
- `socialweb.computer/mesh-addrs` (comma-separated `ip:port`) is a read-only dialing hint; meshd never
  writes it. It is how the tests dial directly with discovery off.

`--peers-file` replaces the kube source with a NodeList JSON file (`kubectl get nodes -o json` shape),
re-read when its mtime changes, and writes nothing. It exists for the network-namespace proof.

**The node authorizer does NOT let a node list Nodes.** Measured against k3s v1.36.4+k3s1 with a
`system:node:node-a` client certificate minted from the cluster client CA: `list nodes` answers 403
`node 'node-a' cannot read all nodes, only its own Node object` (AuthorizeNodeWithSelectors). Every
tenant cluster therefore needs `rust/socialweb-meshd/deploy/node-reader-rbac.yaml` - ClusterRole
`socialweb-meshd-node-reader` (`nodes` get/list/watch) bound to group `system:nodes`. With it the same
identity lists, watches and patches its own Node's annotations (node authorizer plus NodeRestriction
allow annotations on one's own Node), and patching ANOTHER Node is 403, which meshd turns into exit 4.
Until the binding exists meshd logs `list_nodes_forbidden` with the manifest's path, stays not ready,
and retries. The binding is ours and installed by us, so it belongs in the addon baseline, not intent.

**The Kubernetes client is hand-rolled, not kube-rs.** meshd makes three calls (list, watch, merge
patch). iroh already brings reqwest, rustls and ring, so a client over those plus serde_yaml for the
kubeconfig adds almost nothing to a static musl binary; kube-rs would add k8s-openapi and a second
TLS configuration. The kubeconfig reader follows `current-context`, resolves file paths against the
kubeconfig's directory, takes `*-data`, `token` and `tokenFile`, and REFUSES
`insecure-skip-tls-verify` and a cluster with no CA, because the Node list decides where pod traffic
goes. `tls-server-name` is refused as unsupported.

**Data plane.** A tun device (`--tun-name`, default `swmesh0`, `IFF_TUN|IFF_NO_PI`, so one read is one
IP packet). The tun takes the network address of each own pod CIDR as a host address (`10.42.1.0/32`
for `10.42.1.0/24`, the flannel.1 convention; host-local IPAM never hands out `.0`), and each peer pod
CIDR is routed `dev swmesh0 src <that address>`, so traffic the node itself originates leaves with a
source inside its own pod CIDR and the reply comes back through the mesh. One IP packet is one QUIC
datagram, with no header: the connection already authenticates the sender's endpoint id.

- outbound: longest-prefix match of the destination over peer routes; a destination with no route,
  or a source outside this node's own pod CIDRs, is dropped and counted by reason;
- inbound: the sending endpoint must have a route, the source must be inside THAT endpoint's pod CIDRs
  and the destination inside ours, else dropped and counted; a connection from an endpoint no Node
  routes to is closed at accept (`connections_refused`);
- connections are dialed lazily on the first packet for a peer, kept, and reused in both directions
  (an accepted connection is used for sending too); a per-peer queue holds 512 packets, a dial has
  15 s, a failed dial backs off 1 s doubling to 30 s and drops packets meanwhile; a peer leaving the
  Node list has its connection closed and its routes removed. ALPN `socialweb.computer/meshd/0`.
- The node-local half is untouched: own pod CIDR traffic never reaches the tun.

**The tun MTU is 1160.** noq's largest datagram is `current_mtu - (1 flag + 8 CID + 4 PN + 16 AEAD tag)
- 9 (datagram frame bound)`, and `current_mtu` is the minimum over a connection's live paths, starting
at QUIC's 1200. A relay path stays at 1200, so with relays on the floor is 1200 - 38 = 1162; 1160
leaves two bytes. Measured: `max_datagram_size` 1162 on a 1228-byte underlay (a 1200-byte UDP payload
path) and 1414 on a 1500-byte one after MTU discovery. The bridge CNI's `mtu` must be 1160: a
don't-fragment packet above it is refused by the node with frag-needed (proved), and a fragmentable
1400-byte ping crosses as fragments (proved).

**A crashed peer is reachable again in about 15 s.** A peer that is SIGKILLed and restarts with the
same key leaves the other side holding a connection nobody answers; one-way traffic waits for that
connection's idle timeout. Measured 30.8 s at iroh's default 30 s idle timeout and 15.4 s after
meshd set it to 15 s (three of iroh's 5 s heartbeats). A clean stop sends CONNECTION_CLOSE and the
peer re-dials on its next packet.

**Relay and discovery** are iroh's n0 preset by default (pkarr publish, DNS lookup, n0 production
relays), since guests have outbound internet: `--relay default|disabled|<url,...>`,
`--discovery n0|none`, `--bind ip:port[,...]`. No inbound port is needed. The proofs run with relays
and discovery off in network namespaces; the n0 path itself is iroh's and was not exercised here.

**Health and lifecycle.** JSON log lines on stderr (tracing, `--log-filter`, default
`info,iroh=warn,...`); `GET /status` (JSON: identity, own pod CIDRs, routes with connected flag and
max datagram size, installed kernel routes, rejections, counters), `/metrics` (Prometheus text) and
`/healthz` on `--metrics-listen`, default `127.0.0.1:9469` (a non-loopback address is warned about).
Ready - and `READY=1` to `$NOTIFY_SOCKET` - means the tun is up, the own Node is seen with a pod CIDR,
its annotations are visible through the watch (kube mode), and the routes synced. Exit codes:
0 SIGTERM/SIGINT, 1 a task ended, 2 usage, 3 secret or peers file, 4 kubeconfig refused or own-Node
patch forbidden, 5 tun create/up/read, 6 iroh bind, 7 status bind. A missing kubeconfig, an
unreachable apiserver, a forbidden list and an unregistered own Node are retried, not fatal.

**Build.** `rust/build.sh [x86_64] [aarch64]` writes `rust/dist/socialweb-meshd-<arch>` and
`socialweb-meshd-<arch>.sha256` (sha256sum format): static musl, ring's C compiled by `clang
--target`, linked by the toolchain's `rust-lld`, release profile `opt-level=s`, fat LTO, one codegen
unit, `panic=abort`, stripped, with the checkout, registry and sysroot paths remapped. Two different
target directories on hat-8 produced identical bytes; identity across hosts is not measured, so the
pin a user_data carries is the `.sha256` written next to the binary it ships. blake3 is forced to its
pure-Rust backend because its NEON C includes libc headers and there is no aarch64 musl sysroot.
`rust/dist/` is not committed. `deploy/check.sh` runs `cargo fmt --check` and `cargo test` in `--local`
and `--full` (skipped with a reason without cargo; `--full` fails instead), and adds the network-namespace and
k3s proofs in `--local`/`--full` when `sudo -n` works.

**The integration.** Every guest piece is
written by the Go cloud-init module (`internal/atproto/cloudinit`, `K3sJoinModule`) into the
`user_data` the RFP flow carries; nothing reaches a guest any other way. Decisions not in the meshd author's list are
marked "decided by Claude, user may veto".

1. *Server.* `clusterstack.Spec.ServerArgv` adds `--flannel-backend=none --cluster-cidr=10.42.0.0/16`
   when `MarketDialled` (a market cluster, the only kind since the guest substrates were retired).
   Decided by Claude, user may veto: the cluster CIDR is named
   explicitly, at k3s's own default (so a running cluster's CIDR does not change), because the
   guest's masquerade rule names it and must not depend on a k3s default; kube-router's network
   policy controller is KEPT (no `--disable-network-policy`), since it works on pod IPs over any
   bridge and the meshd inbound check keeps pod source addresses. The real k3s v1.36.4 binary accepts
   the flags, and a live zero-node market cluster read `--allocate-node-cidrs=true
   --cluster-cidr=10.42.0.0/16` on the controller manager k3s runs, so a Node gets the `spec.podCIDR`
   the bridge waits for.
2. *Bridge CNI on the Node's pod CIDR.* host-local has no "range from the Node" mode (that is
   Calico's IPAM; flannel feeds host-local through its own `subnet.env`), so decided by Claude, user
   may veto: `socialweb-pod-cni` (after `socialweb-node-prep`, whose `join` writes `NODE_NAME`) polls
   `k3s kubectl --kubeconfig /var/lib/rancher/k3s/agent/kubelet.kubeconfig get
   node $NODE_NAME -o jsonpath={.spec.podCIDR}` every 2 s for 3 minutes (the node authorizer lets a
   kubelet read its own Node), refuses anything that is not an IPv4 CIDR, and writes
   `/var/lib/rancher/k3s/agent/etc/cni/net.d/10-socialweb-bridge.conflist` through a rename:
   `bridge` `cni0` (`isGateway`, `ipMasq: false`, `hairpinMode`, `mtu: 1160`) with `host-local`
   `ranges [[{subnet: <podCIDR>}]]` and a default route, then `portmap`; cniVersion 1.0.0, the
   plugins k3s bundles. Until it exists the kubelet reports the network not ready and nothing is
   scheduled there; `Restart=on-failure` retries a Node that has no CIDR yet. Stated limit: the
   conflist is written once; a Node deleted and re-registered with a different CIDR is not followed
   (the CCM deletes a Node only when its claim is gone, which also releases the machine).
3. *Masquerade only what leaves the cluster.* `socialweb-pod-masquerade` (oneshot) sets
   `net.ipv4.ip_forward=1` (also persisted in `/etc/sysctl.d/90-socialweb-node.conf`) and adds, once
   (`-C` before `-A`), `iptables -w -t nat -A POSTROUTING -s 10.42.0.0/16 ! -d 10.42.0.0/16 -m comment
   --comment socialweb-pod-egress -j MASQUERADE`; `iptables` joins the module's packages.
4. *Node reader RBAC in every tenant.* `internal/tenantnet` embeds a byte-identical copy of
   `rust/socialweb-meshd/deploy/node-reader-rbac.yaml`, and the provider's tenant add-on pass
   (`internal/providermain/eso.go` `tenantAddOns`, run by the bootstrap on every provider
   process's first Ready of each cluster) server-side applies it under the `socialweb-addons` field
   manager BEFORE it needs the cluster vault, then measures the baseline. Live: a
   `system:node:<n>` in `system:nodes` is denied `list nodes` before the apply and allowed after it
   (`RBAC: allowed by ClusterRoleBinding "socialweb-meshd-node-reader"`). RBAC objects are not
   workloads, so they never enter intent.
5. *meshd itself.* With a pin, the module writes `socialweb-meshd-install` (oneshot, after
   `network-online`, `Restart=on-failure`): it picks the pin for `uname -m` (x86_64/amd64 or
   aarch64/arm64), keeps `/usr/local/bin/socialweb-meshd` if a node image already put a binary with
   that sha256 there, and otherwise downloads `<download base>/downloads/<sha256>/socialweb-meshd-<arch>` into
   a temporary file beside the target, refuses bytes whose sha256 is not the pin, and renames it in
   0755. The unit is `rust/socialweb-meshd/deploy/socialweb-meshd.service` byte for byte, plus the drop-in `socialweb-meshd.service.d/10-socialweb-node.conf`:
   `After=`/`Wants=` the install and `socialweb-node-prep`, `StartLimitIntervalSec=0`.
   The `join` process in `socialweb-node-prep` writes `/etc/socialweb/node/meshd.env`
   (`SOCIALWEB_MESHD_NODE_NAME=<node>`, `SOCIALWEB_MESHD_KUBECONFIG=<the kubelet kubeconfig>`), since
   it is the process that learns the node name. `socialweb-k3s-agent` is ordered after
   `socialweb-node-dns-link` and `socialweb-pod-masquerade` and NEVER after meshd, its install or
   `socialweb-pod-cni`: all three wait for the agent's Node, so ordering the agent after any of them
   is a deadlock (`internal/nodeagent/units.go`).
6. *Where the binary comes from.* The gateway's web app serves pinned downloads at `/downloads/<sha256>/<name>`
   (`internal/gatewayhttp/downloads.go`, `LoadPinnedDownloads`):
   every file in its downloads dir with a `sha256sum`-format `<name>.sha256` sidecar, held in memory,
   served only at its own hash and name with `cache-control: public, max-age=31536000, immutable`
   and `etag "<sha256>"`; a sidecar that names another file, or bytes that do not hash to it, stop
   the gateway at startup. The meshd pin is the sidecar hash of the shipped `socialweb-meshd-x86_64` and
   `socialweb-meshd-aarch64` (`MeshdPinsOf`), and the download base is the GatewayAPI component's
   `publicOrigin`, or in a market network the web app's local address as a guest reaches it
   (`guestnet.HostAddress`, `192.0.2.1`, the address at which a guest reaches its host's loopback
   through pasta). The downloads come from the
   socialweb-computer binary: its asset `socialweb-computer-k8s-downloads` (versions.json, built by
   `scripts/build-web-app-downloads.sh` in the release build and pinned by its tree sha256) holds both
   meshd builds and both `socialweb-node` builds, each with its sidecar, and the Gateway root hands the
   extracted tree to the GatewayAPI component as its downloads dir. The NodeClaim mapper writes the
   node-agent pin into the contract's `spec.nodeJoin.nodeAgent`; the bootstrap fetches them by the
   same hash. The Gateway root names the two meshd builds as required downloads
   (`gateway.GatewayRequiredDownloads`), so a GatewayAPI component that would offer no meshd, or has
   no download base, fails to start (`internal/services/gatewayapi/config.go`). The guest image does
   not embed meshd; the install path that keeps an embedded binary is ready for when it does.

Not carried by the mesh: node addresses. Traffic to another node's InternalIP (NodePort on another
node, hostNetwork pods, kubelet ports) has no route; apiserver-to-kubelet and apiserver-to-pod
traffic already rides k3s's own agent tunnel (`--egress-selector-mode cluster`). Every market node
reports the same InternalIP (172.30.0.2, its pasta address), so the apiserver dials a kubelet by its
Hostname address (`kubelet-preferred-address-types=Hostname`), which the k3s join module makes equal to
the node name; the tunnel routes a host name to the agent session of that node. IPv6 pod CIDRs are
routed and checked by the code but untested.

### Every market node resolves names on its own CoreDNS

A Pod's resolver was the cluster's one CoreDNS on one node, so on a multi-node cluster every Pod off
that node failed to resolve (NEXT.yaml limit, measured on five nodes). Now the resolver is on the
Pod's own node, so it needs no cross-node path at all, the mesh included.

- *The address.* `socialweb-node-dns-link` (oneshot) creates the dummy interface `swdns0` and puts
  `169.254.20.10/32` on it, idempotently. Decided by Claude, user may veto: a guest unit and not an
  init container, because the address is node plumbing that must exist before the kubelet starts
  Pods that resolve through it, and a tenant-side init container would need `NET_ADMIN` on the host
  network.
- *The kubelet.* `socialweb-k3s-agent` passes `--kubelet-arg=cluster-dns=169.254.20.10` and
  `--node-label=socialweb.computer/node-local-dns=true`, and is ordered after the address unit. The
  server's `--cluster-dns` is not used: it is also the ClusterIP of k3s's `kube-dns` Service, which
  must be inside the service CIDR. `resolv-conf` is left at k3s's choice (`/etc/resolv.conf`, which the
  module writes with the provider's resolver and public fallbacks, and which has no loopback entry).
- *The DNS.* `internal/tenantnet/manifests/node-local-dns.yaml`, applied with the node reader in
  every tenant: ServiceAccount, ClusterRole (list/watch endpoints, services, pods, namespaces,
  endpointslices), binding and Corefile ConfigMap `socialweb-node-dns` in `kube-system`, and a
  DaemonSet of the same name: host network, `dnsPolicy: Default`, `priorityClassName:
  system-node-critical`, tolerating every taint, selected by `socialweb.computer/node-local-dns=true`,
  image `rancher/mirrored-coredns-coredns:1.14.6@sha256:1ba6f472...`, k3s's CoreDNS security context, probes on
  `169.254.20.10:8080/health` and `:8181/ready`. The Corefile binds only `169.254.20.10`, serves
  `cluster.local` and the reverse zones from the apiserver through the `kubernetes` plugin (in-cluster
  config: the `kubernetes` Service's ClusterIP, which kube-proxy on the node DNATs to the advertise
  address the apiserver link carries on `lo`), and forwards everything else to `/etc/resolv.conf`.
- *Why the label.* Decided by Claude, user may veto: the DaemonSet is applied in EVERY tenant
  regardless of substrate, and the label is what keeps it off a node that has no `169.254.20.10`
  (a guest-substrate node, a node that joined with an older user_data): there it would crash-loop on
  `bind`. The address, the label and the cluster CIDR must stay equal across the cloud-init module,
  `internal/nodeagent` and `internal/tenantnet`.
- *Baseline, never intent.* The DaemonSet is applied by `socialweb-addons`, so the baseline measures
  it (read live at zero nodes), and a DaemonSet is
  never intent anyway (section 10); its pod leaves its node idle for scale-in.
- *k3s's own CoreDNS stays.* Decided by Claude, user may veto: `--disable=coredns` is not passed. The
  `kube-dns` Service (`10.43.0.10`) keeps answering anything that names it explicitly, and its pod is
  a k3s add-on that never holds a node. Retiring it is a separate change.
- *Stated limits.* A node that has just become Ready starts its DNS pod and its first workload side
  by side; a workload that resolves before the DNS pod is ready fails (the image is seeded, so it
  usually wins, but nothing orders the two). The fix is a startup taint the node lifts once its DNS
  answers; open. No live multi-node reading yet (ACC-MESH).

**What changed on prod at the deploy after 2026-10-07** (history: the gateway then ran as a container
and the web app was TypeScript). The provider builds each
cluster's argv when it spawns the group, and a deploy restarts the container, so every market
cluster's k3s server comes back with `--flannel-backend=none --cluster-cidr=10.42.0.0/16`, and the
first Ready of each cluster in the new process re-runs the bootstrap, which applies the node reader
and the per-node DNS and re-measures the baseline. Measured 2026-10-07 on hat-8 with k3s
v1.36.4+k3s1: a `--disable-agent` server started with the default (vxlan) backend, stopped, and
started again on the same datastore with `--flannel-backend=none --cluster-cidr=10.42.0.0/16`
answered `/readyz` `ok` in 2.1 s, logged no `critical configuration` and no fatal line, and still
held the ConfigMap written before the switch; at zero nodes there is no flannel state anywhere (the
backend lives only in the agent and its Node annotations). A cluster with flannel nodes still up
would keep them on vxlan with no path to new nodes; prod read zero nodes at its last check. The web app
had to ship in the same deploy: its image carried the meshd pair (`build.sh k8s` built it then, which
needs cargo, rustup's `x86_64-unknown-linux-musl` and `aarch64-unknown-linux-musl` targets and clang
on the build host, or `KCP_MESHD_DIST`), and `PUBLIC_ORIGIN` is already set by `scripts/deploy.sh`.
Image size: 1,711,865,393 bytes, +17.2 MB for the two binaries.

### `k3s server` argv



```
--disable-agent                        the keystone
--disable=traefik                      leaves the manifest controller free otherwise
--data-dir <dir>
--datastore-endpoint <dsn>             REQUIRED; empty is a silent new cluster
--egress-selector-mode cluster
--advertise-address <addr>             what a node dials
--bind-address <loopback addr>         NOT the advertise value. k3s defaults it to 0.0.0.0, which is
                                       EVERY tenant's apiserver on every interface of the host.
                                       Bind is loopback; advertise is an address the GUEST can
                                       reach, and the two being separate is the whole point: a node
                                       that is a guest dials the pasta gateway, never the loopback.
                                       Making them one value is correct only for a node on the
                                       host, which is the mechanism this design refuted
--tls-san <the address a node dials>   repeated; carries the gateway address, so the certificate
                                       covers what the node actually connects to
--advertise-port <the front's port>    the `k3s-secure` hold the apiserver front owns (section 8).
                                       A separate advertise lease once existed for a forward that
                                       this cluster does not have, and it is retired: dropped from
                                       an old lease file on open, refused on allocate. Measured with
                                       the two split, before the front: the EndpointSlice reads
                                       <gateway>:<a port nothing binds> -- 63014, connection
                                       refused, while the listen port answers 401 -- coredns sits
                                       0/1 for five minutes, local-path-provisioner and
                                       metrics-server exit 2 into CrashLoopBackOff, and kubectl
                                       logs answers `proxy error ... code 502`. With one port:
                                       three system pods 1/1 with zero restarts, and kubectl logs
                                       arrives through the apiserver's own tunnel
--https-listen-port <the slot's hold>  `k3s-slot-<s>`, behind the front; never advertised
--apiserver-port <the slot's hold>     `k3s-apiserver-<s>`, the slot's loopback apiserver listener
--token-file <0600 path>
--write-kubeconfig <DataDir>/server/cred/admin.kubeconfig
--flannel-backend=none                 market clusters only: pods cross nodes through socialweb-meshd
--cluster-cidr=10.42.0.0/16            market clusters only: named because the guest masquerades it
--kube-apiserver-arg=kubelet-preferred-address-types=Hostname
                                       market clusters only: every guest reports InternalIP
                                       172.30.0.2, so the apiserver dials the kubelet by node name
--kube-apiserver-arg=audit-policy-file / audit-log-path / -maxsize / -maxage / -maxbackup
--kube-apiserver-arg=service-account-issuer, api-audiences
--kube-controller-manager-arg=secure-port, --kube-scheduler-arg=secure-port,
--kube-cloud-controller-manager-arg=secure-port
--kube-scheduler-arg=config <DataDir>/kube-scheduler.yaml
                                       market clusters only: podInitialBackoffSeconds and
                                       podMaxBackoffSeconds at 1 s and leaderElect false, from a
                                       file this host writes
                                       before the server starts
```

A direct `--audit-policy-file` on the server is SKIPPED by k3s with only a warning, which leaves the
cluster SILENTLY UNAUDITED and starting normally. The audit flags must ride `--kube-apiserver-arg`.
An unarmed kcp is the failure mode nobody sees from a green suite.

The next three paragraphs were written for the retired local substrates (a local process and a guest
of this host); a market node dials the advertise address on its own loopback through the apiserver
link (section 9). The rules they state still hold.

**The address a node DIALS belongs to the substrate, and it is not the same address for both.** A
node that is a local process reaches an address this host carries. A node that is a guest reaches
NONE of them: it sits behind a tap in its own network namespace, and its only path to the host is the
gateway its default route names - which is why a guest reaches the host's loopback through that
gateway and cannot reach the host's LAN address at all. So the guest's dial target is built from the
substrate's gateway rather than from an operator-chosen host address; k3s advertises that same
gateway for this substrate; and the gateway is in `--tls-san`, because the guest dials it and a
certificate that does not cover what the node connects to reads as an x509 error rather than as a
missing SAN.

The host-carried-address check is therefore narrowed to the substrates where it is true rather than
deleted, and what replaces it for a guest is not another address property but POSITIVE EVIDENCE:
`AssertAgentDialledOurPort`, run against the node's own account of what it dialled, because a node
joining is not evidence that it joined THIS cluster. That control had been written and unreached
since before the substrate existed.

**Every guest gets its own address, taken from the range its cluster leases.** The shared constant was
this host's own LAN address, handed to every node - one node works and two collide into two Node
objects with one InternalIP, with flannel on each treating the other's pod CIDR as its own. A range is
compared against the host's own networks before it is leased, so no guest is ever handed this host's
address; the gateway is the range's address 1 and the node is `1+ordinal`, and `Lease.Host` refuses a
number outside the range rather than returning one.

### The QEMU substrate

**Historical.** Commit 9d85916 ("market only: the local substrates were retired") deleted
`internal/nodeqemu` along with the other local substrates; no code in the tree boots a QEMU guest, and
nodes come only from the market. What follows is kept as the record of the 2026-09-24 measurements and
describes code that no longer exists.

`internal/nodeqemu` was a second substrate behind the same `nodeHost` seam, selected by
`SOCIALWEB_CLUSTERPROVIDER_SUBSTRATE=qemu`. It boots the SAME image as firecracker - the kernel,
the initramfs and the rootfs `internal/nodefirecracker.LoadImage` resolves from the builder's
configuration - because a node image is a set of portable files, not a firecracker artifact. The
guest is started with a `qemu-system-x86_64` command line:

```
-name <node name>                    the per-node token the record is matched by (see below)
-m <memMiB>  -smp <vcpu>             the machine size, from the image manifest
-machine q35,accel=kvm:tcg           kvm where the host has it, and a plain fallback rather than a
                                     refusal where it does not: the host contract checks for kvm, and
                                     a boot that finds none is a boot that is merely slow
-display none  -serial stdio  -monitor none
-kernel <vmlinux>  -initrd <initramfs>  -append "<manifest boot args>"
-drive file=<rootfs>,if=none,id=rootfs        -device virtio-blk-pci,drive=rootfs
-drive file=<node-disk>,if=none,id=nodedata,readonly=on -device virtio-blk-pci,drive=nodedata
-netdev tap,id=net0,ifname=<tap>,script=no,downscript=no
-device virtio-net-pci,netdev=net0,mac=<guest mac>
```

The rootfs clone is writable and the per-node identity disk is read-only, exactly as under
firecracker, so the guest still finds its token on the disk by label rather than on any command line.

**The network is the SAME mechanism, not a QEMU mode.** pasta gives the namespace child a link
carrying one of the host's addresses; `nodevm.WireNamespace` creates the tap and bridges it onto
pasta's own link, and refuses to configure the VMM until the namespace has been observed to carry
that link. Both substrates start pasta the same way - `nodevm.PastaArgv` - and both exec the same
`kcpagent namespace <params.json>` inside it; the params file's `kind` field decides which VMM the
agent runs. There is no `-netdev user`, no host-forwarding rule and no QEMU-only address: the guest
dial address, the gateway, the tap, the bridge and the per-node address all come from the same range
`internal/subnetallocator` leases to the cluster, so a QEMU guest is addressed exactly as a
firecracker guest and reaches the host's apiserver only through the leased gateway.

**The record's identity is the node name on the command line.** `internal/noderecord` matches a
token SUBSET against `/proc/<pid>/cmdline` after confirming the pid's start time. Firecracker's
identity is its API socket; QEMU has no API socket, so its identity is `{"-name", <node name>}` -
the node name is already per cluster and per node, qemu is started with it, and a record whose
identity does not match is NOT OURS and is never signalled. A gateway that restarts reaps a QEMU
guest the previous one booted by reading the same record and matching the same `-name`, which is the
whole of the "reap by identity" contract.

**What is refused, and why each refusal is a value that cannot be in force.** `qemu` demands the
QEMU binary, the node agent and the image store and configuration, exactly as `firecracker` demands
its own binary; `checkPlacement` refuses a qemu placement missing any of them, and it refuses a
firecracker-only input under qemu (and a qemu-only input under firecracker) because a VMM binary for
the substrate that was NOT selected is never executed, and an operator who set one and saw no
complaint would believe it was carrying it. `--advertise-address` is refused under qemu for the same
reason it is refused under firecracker: a guest reaches none of this host's addresses, so the value
cannot be in force.

**The shared half lives in `internal/nodevm`, and neither substrate imports the other.** The types
that are true of both mechanisms (`Image`, `Guest`, `Network`, `NodeDocument`, `NamespaceParams`),
the namespace plumbing that wires pasta's link to the guest's tap, the per-node identity disk, the
rootfs clone, the console's fatal-marker watch and the `/proc` identity scan are one package. `cmd`
is the only place that knows both substrates: it loads one image and builds one `nodeSubstrate`,
and `nodeSubstrateFor` chooses the implementation. A cross-substrate import is a coupling this
avoids rather than a convenience it takes.

**A node image is built for one VMM, and refuses to boot under the other.** The builder's
configuration carries `vmm` (`firecracker` or `qemu`, absent meaning firecracker), and the VMM is a
recorded input: it is in `BuildInputs` and in `BuildConfig`, so two images of the same inputs for
different VMMs have different fingerprints and never collide in the store. The image's manifest also
carries `vmm`, so the image is self-describing, and a substrate refuses an image built for the other
one - in `checkPlacement`, where the builder's own configuration is held to the selected substrate,
and again in `nodeSubstrateFor`, where the loaded manifest is held to it. That refusal is the point:
both VMMs boot the same kernel and hand the guest the same files, so a cross-substrate boot is not
caught by the kernel, comes up, joins and reaches Running, and the only place the mismatch is written
down is this field. What genuinely differs by VMM is the virtio TRANSPORT the guest's block device
arrives on - firecracker hands it over virtio-mmio, qemu over virtio-pci - and the builder derives
which transports the image must be able to load from the real module tree rather than asserting a
rule: on kernel 7.2.7-200.fc44.x86_64 `virtio_pci` and `virtio_blk` are BUILT IN (present in
`modules.builtin`, absent from `kernel/drivers/`), while `virtio_mmio` is a loadable module the
firecracker configuration already stages. So a qemu image needs no transport module at all here and a
firecracker image needs the one it already names; the artifact sets are otherwise identical. The
builder refuses a VMM whose transport the named tree carries neither built in nor loadable, and
refuses a VMM whose transport is a module the initramfs does not name.

**Status: the QEMU argv, identity, placement and per-VMM image are exercised by unit tests, and the
four-step proof runs under QEMU on this host.** `internal/nodeqemu` boots a real guest only when
`SOCIALWEB_NODEQEMU_IMAGE`, `SOCIALWEB_NODEQEMU_AGENT` and a qemu binary are supplied along with
pasta and `/dev/kvm`, and skips when they are not; a unit test boots a stand-in executable instead
and asserts the command line and the published pid, so no test needs real QEMU.

**The four-step proof was run under both substrates on 2026-09-24, from scratch, each one
uninterrupted run, nothing deleted by hand.** The QEMU run used the qemu image at
`kcp-image/out/sha256-3f9b63b082a3fbf7b92a49160c8e2697ed2b726dad8ca8048ef4171c9998e654` (built from
`kcp-image/nodeimage-qemu.json`, `vmm=qemu`), an agent built from the same tree, and
`/dev/kvm` present, so the guest was booted with `-machine q35,accel=kvm -cpu host`. All four
transitions hold: the control plane reached Ready with zero nodes (STEP 1); `kubectl get nodes` was
empty and the cluster stayed at zero across 10 passes 2 s apart over 20 s (STEP 2); a tenant workload
applied at zero nodes gave `Provision, nodes=1`, the qemu guest booted and its k3s joined as a Ready
node, and the tenant pod ran on `liveweb-node1` (STEP 3); deleting only the tenant workload gave
`Teardown` after the hold, `kubectl get nodes` was empty again, and the control plane was still Ready
and still listed its namespaces (STEP 4). The firecracker run was re-measured the same day against a
freshly built firecracker image and the same agent, so the two numbers below share one method and one
image.

| measurement | firecracker | qemu |
|---|---|---|
| `INSTANCE_START_TO_READY_S`, host-timed from the VMM's instance record to the Node's Ready condition, resolution 250 ms | **5.460** | **7.256** |
| wall time of the whole proof | 98.79 s | 114.39 s |
| historical baseline (2026-09-23, older image, firecracker's `InstanceStart` timestamp) | 9.749 / 9.609 | - |

The qemu guest is ~1.8 s slower to Ready than firecracker's on this host. The two start clocks are
the same kind of host timestamp but not the same instant: firecracker's `StartNanos` is written when
its API's `InstanceStart` succeeds, qemu's when the VMM process is started, so qemu's interval
includes the process spawn and device setup that firecracker's does not - the real gap in guest boot
is therefore somewhat smaller than the table's difference.

One operational condition the first firecracker run exposed, and which is pre-existing rather than
qemu-specific: the node's data directory must be short enough that `<dataDir>/fc.sock` fits the unix
socket path limit (108 bytes), or firecracker's socket cannot be dialled and the guest never starts.
The proof's default `TMPDIR` satisfies it; a very long temporary or state path does not.

One prerequisite the qemu live boot exposed earlier: the host-side agent binary must be built from
the same tree as the gateway, because the namespace parameters carry `kind` and `vmm` rather than
firecracker's fields, and an older prebuilt agent refuses them. The guest-level measurements recorded
above - the module set, the iptables failures, the Ready timing - were taken under firecracker and
are not re-derived here; the qemu run above is the full-flow proof, not a re-measurement of those.

### The host platform

Execution is Linux only; darwin and windows are seams that compile, vet and refuse by name. Plan 0006
M0-XOS-GATE made that true and keeps it so: `deploy/check.sh` builds and vets the tree for darwin/arm64,
darwin/amd64, windows/amd64, windows/arm64 and linux/arm64 on every run (default tags, so no embedded
blob is needed). The OS-neutral packages (`api/...`, `clusterprovider`, `cloudprovider`,
`nodeprovider`, `nodemarket`, `materialiser`, `kcpclient`, `gateway`; the `vmruntime`
and `hostsvc` interfaces, `kcpapply` and the ComputeVM controller core) import no go-systemd, netlink,
firecracker SDK or `internal/fcapi`. An untagged OS-specific file fails the
cross-OS build on the commit that adds it.

`internal/hostos` is primitives only. Every darwin body that has no portable answer returns
`"<what> is unsupported on darwin; see plan 0012"` wrapping `errors.ErrUnsupported`; nothing implements
macOS or Windows VM behaviour.

| seam | Linux | darwin (plan 0012) | Windows (plan 0011) |
|---|---|---|---|
| `hostos` lock (`lock_unix.go`, `lock_windows.go`) | `flock` | `flock` (same file, `//go:build unix`) | `LockFileEx`/`UnlockFileEx` |
| `hostos` process identity (`proc_*.go`) | `/proc/<pid>/stat` start ticks and `/proc/<pid>/cmdline`; `kill(pid, 0)`, `SIGTERM`, `SIGKILL`, `SIGHUP` | refuses: ErrUnsupported | `OpenProcess` + `GetProcessTimes`, the command line via CIM, `TerminateProcess`; no reload |
| `hostos.BootID` (`bootid_*.go`) | `/proc/sys/kernel/random/boot_id` | refuses | refuses |
| `hostos.LocalPortRange` (`portrange_*.go`) | `/proc/sys/net/ipv4/ip_local_port_range` | refuses | `netsh int ipv4 show dynamicport tcp`, falling back to 49152-65535 |
| `hostos` process attributes (`sysproc_*.go`) | `DieWithParent` is `Pdeathsig: SIGKILL`; `NewProcessGroup` is `Setpgid` | `Setpgid`; `DieWithParent` is empty (ponytail: no Pdeathsig) | `CREATE_NEW_PROCESS_GROUP`; `DieWithParent` is empty (ponytail: a job object) |
| `apiserverfront` hold (`fdstore_*.go`) | `[::]:0` dual-stack, `SO_REUSEADDR` set after bind | refuses (no hold) | refuses (no hold) |
| `portscan` (`scan_*.go`) | `/proc/net` and `/proc/<pid>/cmdline` | refuses | `netstat -ano` and the command line via CIM |
| `guestdisk` rootfs clone (`clone_linux.go`, `clone_other.go`) | `FICLONE` reflink, else a `SEEK_DATA`/`SEEK_HOLE` sparse copy | plain copy | plain copy |
| `check-host` facts (`internal/hostcontract`, `gather_linux.go`, `gather_other.go`) | the full host contract, firecracker included | the portable facts only | the same |

The capability predicates `Firecracker`, `QEMU`, `NetworkMode`, `Accel` and `AccelWHPX` are gone from
the tree: nothing declares or reads them.
`check-host` names the VM runtime compiled into the build. The service-manager, VM-runtime and
guest-network seams (`hostsvc`,
`vmruntime`, `guestnet`) are Linux-only bodies behind interfaces; the Windows (HCS, HNS, SCM) backends are
plan 0011 and macOS (vfkit, launchd) is plan 0012. See section 5, "Host processes and the VM lifecycle".

What follows is the measurement of the four-step proof, on the local substrates retired since.

**What was observed, and what was not.** On 2026-09-24 the four-step proof was run on this Linux host
as the capability matrix: a Go test that plan 0026 deleted ran both substrates in one process, and both passed in 210.86 s -- firecracker `INSTANCE_START_TO_READY_S=5.423` and qemu
`INSTANCE_START_TO_READY_S=6.915` (host-timed from each VMM's instance record to the Node's Ready
condition, 250 ms resolution), each through all four transitions. The second substrate's run exposed
a test-harness defect rather than a substrate one: `liveports.TakeForProcess` opened a slot lock and
did not retain the handle, so a second subtest in the same process could take the first's slot and
the two runs collided on ports. The handle was then retained for the process, and the runs used
separate slots. A second, deeper defect turned a failed placement into an unbounded hang: the
in-process child runner of the time (`processrunner.Process.Wait`) waited for the goroutines copying the child's stdout and stderr to
reach EOF, not only for the direct child to exit, and a node's VMM is a descendant of pasta that
inherits those pipes. Signalling pasta alone left the guest holding the write end, and the proof
blocked for 37 minutes in `sync.Once.Do` inside `Wait` -- past its own context, which `Wait` does
not observe. `WaitDelay` then bounded the drain to five seconds after the child exits, and a reaped
process was not an error. MEASURED after both fixes: the matrix ran clean twice, both substrates in
one process -- 210.86 s (firecracker 5.423 s, qemu 6.915 s) and 205.33 s (firecracker 5.166 s, qemu
13.489 s). Everything in this document that is measured was measured on Linux, and the Windows
column above is **design and cross-compilation only**: no Windows binary
was executed on this Linux host, so no Windows boot, no WHPX boot, no user-mode networking path and
no Windows process-identity read has been observed to work. On 2026-09-24 `GOOS=windows GOARCH=amd64
go build ./...` and `go vet ./...` were clean, which proved the tree compiled and type-checked for
Windows, and nothing more; 7c4f741's untagged reflink clone broke that, and plan 0006 M0-XOS-GATE
restored it and gates it. The runtime paths that carry the most risk are the ones no compiler can check: the
CIM/PowerShell command-line read (it shells out per lookup, and a localized or denied environment
breaks it), `TerminateProcess` standing in for a graceful stop, `LockFileEx`'s byte-range lock
against `flock`'s whole-file semantics, WHPX actually being available on a given Windows host, and
slirp's `10.0.2.2` reaching the apiserver's loopback listener. Each is a place a Windows host must
be measured before this document may claim it works.

## 10. The scale decision

```
observe  ->  decide  ->  perform
```

`internal/scale` is a pure `Decide(Observation, State, now, Options) -> Decision`. It asks whether
ANYTHING is running, not whether a specific pod is. **The narrow predicate cannot scale to zero:** a
predicate that can only fire upward cannot express it. So the intent test is deliberately coarse, and
a list failure is an ERROR, never an empty observation - an unreadable cluster is not an idle one,
and acting on that reading tears a node down under its own work.

`internal/scaledriver` performs the decision and owns the one thing `Decide` cannot: the state a
decision carries BETWEEN passes. `State.Nodes` is deliberately NOT remembered; it is re-read from the
Node CRs, because those CRs are the idempotency record and a node that died must be seen to be gone
rather than trusted from a variable. What is remembered is `IdleSince` - **without it there is no
hold at all**, because `Decide` would see an idle cluster for the first time on every pass and never
reach it.

**The state advances only on success.** A driver that recorded a node as up because a call was
attempted leaves a cluster with intent and no node, permanently.

**Both halves of the observation come from ONE call.** The objects decide whether there is intent and
the count decides whether a node exists; read at different moments they can disagree - a pod counted
before its node's CR landed is a cluster that provisions twice, and the reverse tears a node down
under its own work.

**Node phase semantics, and where their boundary is.** `Pending` is
  written before the command exists and `Draining` on every teardown path, and `Running` is written
  only by `ConfirmJoined`, which the composition root calls after the tenant cluster's own node list
  reports the node. So `Running` means k3s registered it, not merely that a command is up. The scale
  decision does not read the phase at all - it counts the tenant cluster's Nodes - so a node stuck in
  `Pending` past the join timeout is a warning and not a wedge: the next pass re-observes and
  confirms it.


The count is the tenant apiserver's own Nodes, which is what `kubectl get nodes` reads, so the number
the decision acts on is the number an operator can check. That is the *dead* direction that matters:
a node that died without its CR being updated would be counted alive by the CR route, which is the
direction that stops a cluster ever reaching zero.

**The tenant's Node object outlives the agent, so the teardown must delete it.** A k3s agent does not
remove its own Node object when it stops - kubelet does that only when a cloud provider is configured,
and none is. So a teardown that stops the process and skips the delete leaves the count above zero
with no intent, and `Decide` calls teardown again on every pass for ever: the cluster is never
observed at zero nodes, which is the objective. The delete belongs to the teardown, after the guest
is stopped, tolerating NotFound as success; a provision deletes nothing. It cannot live in
`nodelocal`, which has no tenant client, so it lives in the
composition-level `scale.Nodes` that wraps the host - and a nil tenant client is an explicit refusal,
never a skipped cleanup, because a skipped cleanup and a completed one look identical from a green
suite.

`SOCIALWEB_CLUSTERPROVIDER_SCALE_HOLD` is the down-direction grace. It is a parameter rather than a
constant because the number has not been derived by measurement.

**The decision carries a target total, and the boolean that reaches zero is untouched.** The
narrow-predicate argument above is not weakened by counting: `Intent` stays a boolean, and a
SEPARATE `Demand(o)` counts the work as a pod count, so an error in the count cannot make zero
unreachable - it can only ask for the wrong number of nodes. `Target(o, opts)` is
`ceil(Demand / PodsPerNode)` clamped into `[1, MaxNodes]` when there is intent, and `0` when there
is not, which is the only place the ratio is applied. `Decision.Nodes` is therefore a target total
rather than a delta, and the driver reconciles the cluster's actual count - re-read from the Node
CRs, never remembered - to it.

**Our own add-ons are a MEASURED baseline, and a cluster holding only them has nothing deployed.**
Add-ons this provider installs into a tenant cluster are workloads like any other, and at zero nodes
their pods are Pending, so they would otherwise be intent for ever: a cluster that had run one
pipeline would never return to zero. The
rule is a delta, measured rather than listed. The installer applies with its own server-side field
manager, `socialweb-addons` (`clusterprovider.AddOnFieldManager`), and right after the apply the
provider lists the Deployments, StatefulSets, DaemonSets and Jobs whose `managedFields` carry an
`Apply` entry from that manager, hashes each one's LIVE spec as read back (so the apiserver's
defaulting is inside the hash), and writes the result to `addon-baseline.json` in the cluster's data
directory. That file is provider state; a tenant cannot edit it. `scale.Narrow` then drops, before
`Intent` and `Demand` see them, every workload whose kind, namespace, name AND spec hash match an
entry, and every pod whose controller resolves to one. A baseline workload whose spec has changed
(the tenant scaled our operator to two) is NOT dropped: that is a tenant's request. A workload of
the same name in another namespace is not ours. The baseline is re-measured on every add-on pass
(each `Bootstrap`). There is no list of add-on names here for the same reason the k3s add-on rule has
none. Non-workload objects (CRDs, RBAC, webhooks) are not recorded, because nothing the scale
decision reads is one of them. The External Secrets operator now runs beside the control plane
(section 5), the tenant holds its CRDs, ClusterRoles and webhook registration and no
workload, and the measured baseline is EMPTY: the rule stays because it is what keeps any future
add-on from pinning a node.

**A pod and the workload that controls it are ONE unit of demand.** `Demand` counted a Pending pod
and also its Deployment's replica, so three one-replica Deployments with their three Pending pods
demanded six and, at one pod per node, asked for six nodes. `scale.Narrow` drops every pod whose
controller chain (Pod -> ReplicaSet -> Deployment, or Pod -> StatefulSet / DaemonSet / Job) names a
workload present in the observation; the workload's own want is the count. A bare pod, and a pod of
a ReplicaSet no Deployment controls, still count as themselves, because nothing else counts them.
The ReplicaSets are read only to resolve that chain; they are not a predicate kind and the
`Observation` still ranges over exactly the five it did.

**`MaxNodes` defaults to 10, and a ceiling below one node is REFUSED rather than clamped.** Named
defaults are `DefaultMaxNodes = 10` and `DefaultPodsPerNode = 8`, resolved in `Options` the same way
`hold()` resolves the grace, and settable per cluster as
`SOCIALWEB_CLUSTERPROVIDER_SCALE_MAX_NODES` and `SOCIALWEB_CLUSTERPROVIDER_SCALE_PODS_PER_NODE`. The
refusal is on `n < 1`, not on `n < DefaultMaxNodes`: a ceiling under one is not a smaller cluster, it
is a configuration whose only consistent reading is "never place the work", while a ceiling of three
is a smaller cluster and is taken. A five-job GitLab run measured what one node means for a runner
with `concurrent = 5` (section 15); `scripts/deploy.sh`
passes 10 and one pod per node, so a cluster serving this runner asks for the nodes the jobs need.

**Scale-in is per node: a node holding nothing beyond the baseline for the hold is cordoned,
drained and released, one per hold.** The rule has a pure half and a side
effect half:

- *What a node holds.* `scale.Loads(nodes, observation, replicaSets, baseline)` reads, per tenant
  Node, `Busy` (live pods bound to it that are not baseline), `Evictable` (live baseline pods bound
  to it: our add-ons' workloads by the measured baseline, and k3s's own add-ons by
  `AppliedByTheControlPlane`) and `Incoming` (a Pending pod with no node yet whose
  `status.nominatedNodeName`, `nodeSelector kubernetes.io/hostname`, or single-valued required
  hostname / `metadata.name` affinity names this node). Pods that FOLLOW the nodes are never counted
  on a node: a DaemonSet's pods and mirror (static) pods (`kubernetes.io/config.mirror`). Terminal
  pods are not counted. A Pending pod naming no node goes into `Placement.Unplaced` with its creation
  time. The observer computes this in the same call as the rest of the observation, so the per-node
  half and the intent half cannot disagree.
- *The decision.* `scale.Decide`, with intent and the counted nodes above the target, keeps a
  per-node idle clock in `State.NodeIdleSince` (the first pass that saw the node with `Busy == 0` and
  not `Incoming`; a busy pass deletes it). It returns `Release` naming ONE node, `Nodes: count - 1`,
  when that node is held by this provider (`NodeLoad.Held`), idle, and idle for the whole hold
  (`SOCIALWEB_CLUSTERPROVIDER_SCALE_HOLD`, default 2 m). Among several it picks the emptiest
  (fewest `Evictable`), then the newest (latest `creationTimestamp`), then by name. A release
  restarts the idle clock of every node left, so three idle nodes over a target of one go one per
  hold: one at 2 m, the next at 4 m, the third kept. No release happens while a Pending pod with no
  node is younger than the hold (or has no creation time): the scheduler may be about to bind it to
  any idle node. A pod Pending longer than the hold with idle nodes present is not going to fit
  them, and holding every node for it would pin the cluster. The target is never undercut: the
  branch is only reached when `count > target`, and the target is at least one while there is
  intent. With NO intent the old path is unchanged: the whole-cluster hold, then `Teardown` to zero.
- *The side effects (`internal/providermain/scalein.go`).* `scaledriver` performs `Release`
  through `scale.Releaser` (an actuator that is not one is `ErrNoReleaser`, never a whole-cluster
  teardown). `tenantNodes.Release` cordons the Node (`spec.unschedulable`), re-observes the cluster,
  and if the node took work between the decision's read and the cordon (Busy or Incoming) it
  uncordons and keeps it. Otherwise it evicts, through the Eviction API so disruption budgets hold,
  every live pod bound to the node that does not follow the nodes; a refused eviction uncordons and
  keeps the node. Then the release is `teardownHeld`, the path the node loss rule and the giving-up
  rule already use: claim deleted, the gateway's Requester releases with `vm.delete`, the node's vault role, policy
  and secret revoked, the tenant Node deleted. A node the provider uncordons is only one it cordoned
  itself; an operator's cordon is left alone. Evicted pods are not waited on: the tenant Node is
  deleted and their controllers recreate them elsewhere.
- *Decided by Claude, user may veto:* one node per hold rather than every idle node at once (each
  release can move baseline pods onto the nodes left); the clock is in memory, so a provider restart
  restarts every node's hold (the safe direction); only nodes this provider process holds are
  released, and a market node that outlived a provider restart is held again by adoption (section
  9, "A restarted provider never reaps; it adopts"), so it is released like any other; a failed
  drain uncordons rather than leaving a cordoned node counted.

**A DaemonSet is never intent, and never demand.** A DaemonSet's desired
count follows the nodes, so with one tenant DaemonSet any cluster that had a node kept intent for
ever and never reached zero. `Intent` and `Demand` do not read
`Observation.DaemonSets` at all, and skip every pod whose controller is a DaemonSet or that is a
mirror pod, whether or not its DaemonSet was in the same read. A DaemonSet-only cluster is a cluster
with no intent: it holds its nodes for the hold and goes to zero. The `DaemonSets` field stays in the
observation because the baseline narrowing ranges over it.

**A custom resource is intent only while an operator of ours that runs IN the tenant cluster has not
reconciled it (the rule "workloads beyond the baseline plus our operators'
CRs not yet reconciled" is the user's, the mechanism decided by Claude, user may veto).**
`scale.Operators` is a registry of `{Group, Kind, ReadyCondition, InCluster}`. A CR counts only if
its group/kind is registered with `InCluster: true` and it is not reconciled: its `ReadyCondition`
is not `True`, or that condition or `status.observedGeneration` is older than
`metadata.generation`, or it is being deleted behind a finalizer. Each such CR adds one to `Demand`
and makes `Intent` true, so it asks for one node, on which its operator runs and reconciles it.
An operator that runs beside the control plane reconciles at zero nodes, so its CRs need no node
and are never registered as in-cluster. `scale.OurOperators` ships EMPTY: ESO runs beside the
control plane, and no other operator of ours runs in a tenant cluster. Leftover
namespaces, RBAC and ConfigMaps never hold nodes. `NodeObserver` lists each in-cluster kind through
its `CustomResources` lister and refuses to observe at all when the registry names an in-cluster kind
and no lister was supplied, so a future entry cannot silently never count.

On the retired guest substrates the ceiling also met the cluster's own `/24`: every node took an
address from the range `subnetallocator` leased. A market node takes no address of this host's.

Plan 0026 deleted the Go tests that held these rules (three held market nodes over a fake tenant:
the DaemonSet-only node went at 2 m, the node holding a k3s add-on pod at 4 m with that pod evicted,
the node running work was kept). NOT yet read: a live market cluster releasing a node.

The reading for this is a table case: the same observation that decided `Provision, Nodes: 1`
before now decides `Provision, Nodes: <target>` with `MaxNodes` raised, and every existing case at
`MaxNodes == 1` is byte-for-byte what it was. What is NOT yet read is a live cluster holding more
than one node at once - `NEXT.yaml` AUTO-1 stays open for that, and `docs/plans/0002-scale-to-n-nodes.md` names the
instrument: three concurrent pods, three joined nodes, and a return to zero.

**A market that takes no bid backs the cluster off, exponentially, capped at ten minutes (decided by
Claude, user may veto).** MEASURED on prod: with
no bidder answering, the provider wrote a NodeClaim, the gateway's fulfiller got `no_bids` after its
30 s bid window, the claim ended Failed, `confirmHeld` gave the node up, and the next scale pass
claimed again -- a new public `market.rfp` about every 50 s per missing node, 49 RFPs and 86
`no_bids` in one hour. The rule:

- *What counts.* A claim that ends `Failed` with `status.contract.rfpUri` set and
  `status.contract.acceptUri` empty (`nodemarket.MarketDeclined`): the Requester asked the market and no
  bid was taken. That is the requester's whole `NO_CONTRACT_OUTCOMES` class -- `no_bids`,
  `bids_denied` and `policy_rejected` -- and all three loop identically, each re-ask a fresh public
  RFP, so all three back off. A claim that failed after an accept (the guest died, the join never
  forwarded) keeps today's behaviour: the market did answer, and the next pass asks again at once.
  So does every non-market substrate, which never reports a declined end.
- *Why that shape and not a reason field.* The claim is classified from structured record URIs the
  NodeClaim controller already writes, never from `status.message` text. A new `status.reason` was rejected because
  `APIResourceSchema.spec` is immutable and, MEASURED (`internal/versions/versions.json`), a schema
  change does not reach existing APIBindings: every cluster home bound before the bump would prune
  the field and silently never back off. The Requester keeps `rfpUri` and writes no `acceptUri` on a
  `no_bids` end, and the NodeClaim controller copies both onto the claim. Status stays one writer per
  half: the NodeClaim controller writes the claim's status, the provider only reads it.
- *The wait.* `scale.BackoffDelay(n) = 15 s * 2^(n-1)` for the n-th declined or failed end in a row, capped
  at `scale.DefaultBackoffCap = 5m`: 15 s, 30 s, 1 m, 2 m, 4 m, 5 m, 5 m ... One confirm pass that
  sees several declined claims counts ONE end, because they are one answer from one market at one
  moment. The wait runs from the pass that saw the end. `Options.BackoffBase` / `BackoffCap` default
  to `scale.DefaultBackoffBase` and `DefaultBackoffCap`.
- *Where it lives.* `scale.State.Backoff` is pure state; `scale.Decide` returns `None` with
  `Decision.BackoffUntil` set while `st.Nodes < target` and the wait holds. The decision's reason
  says the cluster is backing off, until when (RFC 3339 UTC), how many ends in a row, and the claim's
  own message, and the provider's `cluster provider: node scale` line carries `backoffUntil`.
  `scaledriver.Driver.Declined(cluster, reason)` records an end (and logs `backing off market
  provisions for this cluster` with `until`, `wait`, `why`); `Driver.Joined(cluster)` clears it. The
  composition root (`wiring.go` `scale`) feeds both from `nodeHosts.confirm`'s outcome before the
  scale pass, `Joined` first so a pass that sees one node join and another declined still backs off.
- *Resets.* A node of the cluster newly confirmed joined (the tenant cluster reports it and its
  claim reads Running; a node already joined does not re-reset on every pass), or the cluster's
  intent dropping to none (`State.Next` keeps the back-off only while the decision carries intent).
  A provision does NOT reset it: the count of ends outlives the provision it allowed, so the next
  end waits longer.
- *Memory only.* The back-off is held in the provider process. A provider restart starts every
  cluster from no back-off, so the first pass after a restart may post one RFP at once; that costs
  one RFP per restart, not one per pass.

`tenantNodes.retain` restores the primary slot when it is empty: dropping ordinal 1 unconditionally would leave a
first node claimed again after the first was given up unheld, so its claim would then never be
confirmed, never released, never counted in flight and never seen ending, and it could not back
anything off.

**A market node NotReady for `NodeLossGrace` is released, and its Node deleted (provider half; the
user's number was 90 s, and 76c4c864 shortened it to 1 minute).** In `tenantNodes.confirmHeld`, after the given-up rule
and the per-pass `SyncSecrets`, each held market node's tenant Node is read (`readTenantNode`). If
its `Ready` condition is `False` or `Unknown` and `now - lastTransitionTime >= NodeLossGrace` (1 minute, `internal/providermain/nodes.go`),
the node goes through `teardownHeld`, the same path a scale-down takes: the claim is deleted, its
vault role, policy and secret are revoked, the NodeClaim controller has the Requester release the
machine with `vm.delete` and drops its finalizer, then the tenant Node is deleted so the scheduler stops binding pods to it and evicted
pods reschedule; the node is reported in `confirmOutcome.lost`, and the scale pass that follows in
the same tick counts one node fewer and claims a replacement if the work still needs one. Decisions,
made by Claude, user may veto:

- *The clock is the Node's own `lastTransitionTime`*, written by the tenant cluster's node-lifecycle
  controller on the same host, so a provider restart does not restart the grace. A condition with a
  zero transition time falls back to the first pass that saw it not Ready, held in memory.
- *Not covered:* a node the tenant cluster never reported (no Node object) -- the confirm and
  giving-up rules own it, however long it has been -- and a Node with no `Ready` condition yet (its
  kubelet has not posted a status). A Ready node is never lost, whatever its transition time.
- *Held nodes only.* The rule acts on nodes this provider process holds. A market node that outlived
  a provider restart is held again by adoption at startup and on every pass (section 9, "A restarted
  provider never reaps; it adopts"), so the rule covers it from the first pass after a restart.
- *It blocks the cluster's pass* for as long as `teardownHeld` waits for the NodeClaim controller
  (`ClaimReleaseTimeout`), exactly as the giving-up rule and the scale-down already do.

**A market cluster's kube-scheduler runs with a 1 s pod backoff, written into the cluster's data dir
before the server starts.** The scale-from-zero path has one
wait this project does not own: kube-scheduler holds every pod it could not place -- and a cluster
with zero nodes cannot place any -- for a backoff that starts at 1 s and doubles, capped at 10 s.
The node that finally makes such a pod placeable does not cancel that wait, so the pod that waited
for the first node can wait again after it arrives, which is the whole latency this milestone is
about. Market clusters therefore name `--kube-scheduler-arg=config=<DataDir>/kube-scheduler.yaml`,
rendered by `clusterstack.KubeSchedulerConfig` (pure) and written by
`clusterprovider.WriteKubeSchedulerConfig` before the steps are built, exactly as the OpenBao config
is; the file sets `podInitialBackoffSeconds` and `podMaxBackoffSeconds` to 1 and carries a
`clientConnection`, because a `--config` file makes kube-scheduler IGNORE the deprecated
`--kubeconfig` flag k3s still passes. A stack that dials no market has no zero-node window and its
argv names no config file.

**The backoff was not the 16 s; leader election was.** With the 1 s
backoff deployed the first pod still waited 15 s after its Node was Ready. k3s `--disable-agent` starts
kube-scheduler only once a node registers ("Starting controller" 02:53:17.22Z), and the new scheduler
then waited out the leader lease its previous incarnation still held ("Attempting to acquire leader
lease" 02:53:17.35Z, "Successfully acquired lease" 02:53:33.08Z, kube-scheduler's 15 s default lease).
Each cluster runs exactly one k3s server, so there is nothing to elect: the same file now sets
`leaderElection.leaderElect: false` (the `--config` file owns leader election, so a flag would be
ignored). The 1 s backoffs stay, as they cost nothing and bound the wait once the scheduler runs.

The two flags the plan preferred do not exist in this kube-scheduler: MEASURED against k3s
v1.36.4+k3s1, `--pod-initial-backoff-seconds` and `--pod-max-backoff-seconds` are absent from
kube-scheduler's own `--help` (which lists `--config` and the deprecated
`--pod-max-in-unschedulable-pods-duration`), and a non-numeric value for the first is not rejected,
so the flag form is not a flag this binary defines. The file form is read: an unreadable config
(garbage bytes) makes the scheduler exit -- k3s's log records `scheduler exited` while the server
itself stays up, so the failure an operator sees is a cluster whose pods are never placed, not one
that failed to start. Both halves were measured against the real binary (a pod created at zero nodes
was placed on the node that arrived, 7.7 s end to end including the cluster's start). What this does
NOT claim: on this host a pod waiting for a node was requeued
~50 ms after the node became schedulable with the DEFAULT backoff in every shape tried (a node
becoming Ready, a taint leaving it, a label arriving), so the 16 s read on prod is not reproduced
here and this change bounds the worst case rather than an observed wait.

## 11. Teardown, and the gate

Teardown is gated, because a teardown that races a running job is a lost write, and after rotation
the objects are gone.

**`status.nodeExpiry` decides whether the gate can compare:**

```
absent / unknown     -> REFUSE. A gate that cannot compare must not proceed.
explicit 0           -> PROCEED. There is no expiry to compare against.
non-zero reachable   -> refuse if the gate would outlive it
```

**Conflating absent with `0` makes a deliberately-unlimited cluster undeletable.**

`0` means UNLIMITED (`clusterprovider.NodeExpiryUnlimited`). The trade: it removes the only automatic
bound on an ORPHAN. Containment then rests on token-per-instance, the Requester's session accounting
(`RequesterSettings.sessionCapSeconds`), and our own teardown path - **there is no timer backstop**.

**A producer reports; it never guesses.** The producer surface cannot express an estimate: it returns
a value *and whether it has one*, so "nothing to report" is a bit rather than a sentinel, and a
source that forgets to report errs toward refusing rather than toward proceeding. `0` is the report of
a node with no session cap at all (the local-process mechanism was one, before it was deleted), and it
is a fact about that node, not a guess. That is why absent and `0` are
both reachable and mean opposite things. A guessed deadline that is late reproduces exactly the silent
loss the gate exists to prevent, and a guess is indistinguishable from a report in the schema, so it
would remove the refusal the gate is. The report is written **only when the object carries no value**:
the settled shape of the `Cluster` value is a roll-up over the node CRs, and a second writer that
revises a report is the one direction that loses data. A stale earlier value can only refuse a
teardown; it can never permit one that outlives the truth.

**A gateway restart with live children: adopt by record, never by guess.** Every cluster member is a
HostProcess unit, and its record is the HostProcess object, not a process scan: a restarted provider
lists each cluster's member objects, adopts a Running one whose invocation matches the label
`socialweb.computer/verified-invocation`, and re-places or re-gates the rest (section 5, "The
per-cluster slice of member units"; `UnitsHost.Adoption`). An adopted unit has one owner, the object,
so it cannot look alive to one owner and dead to another. Before the HostProcess units, a gateway
restart reaped its children instead: a process group was found by its leased ports through
`internal/portscan`, and a node by a `node.json` pid record; that path is gone with the local
substrates.

A market node is adopted the same way: its machine is the bidder's, its guest needs no session of
ours, and its owner of record is the `NodeClaim` in the cluster's home workspace, which the NodeClaim
controller alone releases and only on deletion. The claim is the single record, and a restarted
provider holds it again from that record without writing it (section 9, "A restarted provider never
reaps; it adopts").

A deleted or switched-off cluster is reaped by deleting its member objects (`UnitsHost.Reap`), and
the HostProcess supervisor stops each unit; the provider waits until every member is Stopped before
it drops the finalizers. Its Postgres object stays for an OFF cluster and goes with a deleted one.

**Reap does not reach the store.** The cluster's store is its Postgres object (`<prefix>pg-<store>`),
a unit of its own: deleting a cluster's members leaves it, and the purposes the members hold are the
front's (section 8), none of them a node's, because a node binds no port of this host's.

**One Host per cluster, for the life of the process.** A node actuator built fresh on each call cannot
scale to zero: `Provision` in one pass and `Teardown` in a later pass act on different objects, so the
teardown finds no handle, stops nothing, and the node keeps running. The composition root memoises one
host per cluster and hands back the same one thereafter, under a mutex because passes may overlap.
Memoisation also holds the cluster's `k3s-secure` lease for the process lifetime, which is what keeps
`status.URL` meaningful after OFF: a port re-leased to another cluster is a silent misroute on this
cluster's next ON.

**A shutdown leaves the members running.** `Provider.Shutdown` (`internal/providermain/wiring.go`)
stops no member unit and no Postgres object: the next provider adopts them. Its node-host reap skips
the market substrate entirely: deleting a market claim is what makes the gateway destroy the machine,
so a shutdown that reaped market hosts would release every rented node on every deploy (watched
before the change: 2 claims deleted and both nodes' vault state revoked; none after). A cluster
switched OFF or deleted still releases its market nodes through `reapCluster`.

**Every cluster, not the one this process was told to join.** `SOCIALWEB_CLUSTERPROVIDER_JOIN` names
a single cluster by its bare name and keeps working exactly as it did. `SOCIALWEB_CLUSTERPROVIDER_JOIN=*`
is the mode the gateway needs, because its web app lets any atproto user create a cluster and NOTHING
materialises one unless a process joins it: the provider lists `registry.Refs` - which walks root,
the accounts, the homes and the clusters - and the Cluster controller (`internal/controllers/cluster`)
reconciles EVERY cluster, each on its own watch and deadlines (`deploy/obligations.txt`), with at most
`MaxConcurrentBringUps` (4) bring-ups at once (`internal/providermain/wiring.go`). A failure in one
cluster is logged with the cluster's name, home and error and requeued on its own: one cluster that
cannot come up must not hold back the others. Startup walks every `Cluster` CR to decide which members
to adopt or bring up, tree-wide; shutdown leaves every member running for the next provider.

**One process per host still holds ONE apiserver per store (invariant 1).** Every cluster gets its own
state dir, its own port leases, its own Postgres object, its own slice of member units and its own
`Host` entry, and the `Host` is memoised by the same key the state dir is named by, so a pass in one
cluster and a teardown in a later pass act on the same object. Two clusters named `ci` by two
different accounts are two entries in every one of those maps.

**A market claim does not stall the pass.** `tenantNodes.Provision` writes the `NodeClaim` and returns.
It does NOT poll the tenant cluster for the node first, and the distinction is the whole reason the
all-clusters mode is usable: a market machine is absent for minutes, and one new
claim would otherwise block every other cluster in the tree for the whole timeout, once per cluster,
serially. Joining is confirmed on a LATER pass, by `nodeHosts.confirm`, which runs before that
cluster's scale pass: a node whose tenant Node object has appeared is confirmed through the same
`ConfirmJoined`, and a node whose claim reads `Failed` is torn down there - the claim released and the
tenant Node deleted - so the failure is reported and the cluster may ask the market again rather than
counting a machine it will never get.

## 12. Code layout

This section maps the code as it is. Every directory under `api/`, `internal/` and `cmd/` appears
once in the blocks below. The layout is the plan 0026 layers (`docs/plans/0026-abc-control-plane.yaml`,
and "Architecture rules (plan 0026)" in AGENTS.md). A new transport is a new sibling package, not a
branch inside an existing one.

### The layers, and how the checker assigns them

`scripts/check-architecture.sh` runs `go run ./hack/archcheck`. The checker gives every package one
layer from its path alone (`hack/archcheck/layers.go`). An import may only point down this order:

```
api <- contract <- kit <- transport <- impl <- controllers <- app <- cmd
```

| Path | Layer | Notes |
| --- | --- | --- |
| `api/**` | api (common) | types, contract status, refs; no logic, no I/O |
| `internal/contract/**` | contract (abc) | imports api, other contracts and the standard library only; no third-party import |
| `internal/kit/**` | kit (common helpers) | no concept |
| `internal/controllers/runtime/<pkg>` | transport | the dynamic client and unstructured objects are allowed only here and in `internal/apigen` |
| `internal/controllers/**`, and the package `internal/controllers/runtime` itself | controllers (adapters) | one reconciler per kind |
| `internal/app/**`, `internal/stack/**`, `internal/devcore/**`, `internal/<x>main` | app (factory) | `<x>main` is a package directly under `internal/` whose name ends in `main` |
| `cmd/**`, `hack/**`, `internal/apigen/**`, `internal/lexgen/**` | cmd (CLI and tools) | |
| every other `internal/**` | impl | this includes `internal/bus`, the component runtime and the adapters under `internal/services/**` |
| a directory with no Go package | assets | rank 0, so any layer may embed it |

The backend rule. A package under one of the backend prefixes of `hack/archcheck/layers.go`
(`internal/compute`, `internal/vault`, `internal/pds`, `internal/atprotorepo`, `internal/events`,
`internal/vmruntime`, `internal/hostsvc`, `internal/fcapi`, `internal/guestnet`, `internal/guestdisk`,
`internal/guestimage`, `internal/policyengine/runs`, `internal/relay`, `internal/ingress`,
`internal/ssh`, `internal/dispatcher`, `internal/gateway`, `internal/guestcp`, `internal/market`,
`internal/lease`, `internal/postgresctl`, `internal/ttlctl`, `internal/clusterprovider`,
`internal/tenantstore`) must not reach, directly or through any import, client-go, controller-runtime, multicluster-runtime, kcp-dev,
unstructured objects, `internal/bus`, `internal/controllers`, `internal/app`, `internal/kcpserver`,
`internal/kcpclient`, `internal/kcpdatastore` or `internal/stack`. Every backend package at the depth
`internal/<concept>/<impl>` must hold a compile-time assertion against an `internal/contract`
interface (`var _ compute.Provider = (*Provider)(nil)`). The host libraries (`internal/vmruntime`,
`internal/hostsvc`, `internal/fcapi`, `internal/guestnet`, `internal/guestdisk`, `internal/guestimage`,
`internal/postgresctl`, `internal/ttlctl`, `internal/clusterprovider`) are exempt from the assertion.

### Abstract kinds, and where each lives

A consumer refs only the abstract kind. The abstract kind names its concrete kind in
`spec.backendRef`, and its controller mirrors the backend's contract status.

| Abstract kind | Concrete kinds | Controllers | Backends | Contract |
| --- | --- | --- | --- | --- |
| AtprotoRepo | LocalRepo (on the Pds component), RemoteRepo (on a RemotePDS plus a credentialsRef) | `internal/controllers/atprotorepo`, `internal/controllers/localrepo`, `internal/controllers/remoterepo`, `internal/controllers/remotepds`; `internal/controllers/repos` registers them with `internal/controllers/pds` | `internal/pds/local`, `internal/atprotorepo/remote`, `internal/atprotorepo/scoped`, `internal/pds/remote` | `internal/contract/atprotorepo`, `internal/contract/pds` |
| Vault | OpenBaoServer (local), RemoteOpenBao | `internal/controllers/vault`, `internal/controllers/openbaoserver`, `internal/controllers/remoteopenbao` | `internal/vault/openbao`, `internal/openbaoctl`, `internal/remoteopenbao` | `internal/contract/vault` |
| ComputeVM | FirecrackerVM (HcsVM and VzVM wait for plans 0011 and 0012) | `internal/controllers/computevm`, `internal/controllers/firecrackervm` | `internal/compute/firecracker`, `internal/computevm`, `internal/vmruntime` | `internal/contract/compute` |
| EventSource | PdsEventSource, RelayEventSource, JetstreamEventSource; an EventStream refs an EventSource | `internal/controllers/eventsource`, `internal/controllers/pdseventsource`, `internal/controllers/relayeventsource`, `internal/controllers/jetstreameventsource`, `internal/controllers/eventstream`; `internal/controllers/events` registers them | `internal/events/pds`, `internal/events/relay`, `internal/events/jetstream`, `internal/events/delivery` | `internal/contract/events` |

The types are in `api/v1alpha1` (AtprotoRepo, LocalRepo, RemoteRepo, Vault, OpenBaoServer,
RemoteOpenBao, ComputeVM, FirecrackerVM), `api/pds/v1alpha1` (RemotePDS) and `api/events/v1alpha1`
(EventSource, its three backends, EventStream).

### The bus and the component runtime

`internal/bus` is the declared bus. `bus.Resolve[T](ctx, b, from, to, capability)` returns a typed
contract handle. The caller is the reconciled object or the component instance; System is allowed only
for the callers in `deploy/bus-bootstrap-callers.txt`. A call that no accepted ComponentLink allows
fails with `ErrLinkDenied` (`internal/bus/links.go`). `internal/component` is the component runtime: a
running component instance, its Service, readiness and endpoints, and the registry of running
components by workspace, kind and name. `internal/controllers/component` reconciles a Component object
into it, and `internal/stack/componentcore` runs it with the controller manager in every core.

### The map

api (common):

```
api/apiresource/             the APIResource and PrinterColumn metadata that each group's resources.go fills for apigen
api/common/v1alpha1/         shared types: ObjectRef, LocalObjectRef, StrongRef, SecretRef, CredentialsRef, the contract labels
api/v1alpha1/                the socialweb.computer group: application roots, components, market, identity, records, VM, vault, cluster, node and host kinds, and the deployments (Infra, EphemeralInfra, InfraCollection, EphemeralInfraCollection; plan 0029)
api/events/v1alpha1/         events.socialweb.computer: EventSource, PdsEventSource, RelayEventSource, JetstreamEventSource, EventStream
api/exposure/v1alpha1/       exposure.socialweb.computer: ComponentLink, Exposure, Service
api/pds/v1alpha1/            pds.socialweb.computer: RemotePDS
api/policy/v1alpha1/         policy.socialweb.computer: PolicyEngine, PolicyTrigger, PolicyWorkflow, PolicyWorkflowRun
api/kcpgvr/                  GroupVersionResource vars for the foreign groups (kcp tenancy, apis and core; Kubernetes coordination, authentication, rbac)
```

contract (abc):

```
internal/contract/atprotorepo/   Repo: record reader, writer and subscriber; commits, list pages
internal/contract/clusterhome/   the account hash, home path, mount path and cluster key of a cluster's home workspace
internal/contract/component/     component key, endpoints, Service, readiness, restart policy, phase
internal/contract/compute/       Provider: shape, provision request, instance
internal/contract/dispatcher/    Proxy
internal/contract/events/        Source, Stream, Sink, Event, Filter
internal/contract/gateway/       the web user, sign-in, sign-out, records and account agent of the gateway
internal/contract/guestcp/       the guest control plane: inputs, guests, grants, onNetwork report, relay
internal/contract/host/          host effects: unit backend, unit spec, status, events, identity, observation, limits
internal/contract/ingress/       ingress Provider, Registrar, registration
internal/contract/lease/         Slots, Rolls
internal/contract/market/        market roles: identity, peers, offers, contracts, intents and their observer
internal/contract/nodeclaim/     NodeClaim: claim, links, settlement, mapper (infra and entry names, entry state and fields), binder
internal/contract/pds/           Accounts, Sessions, Sync, ServiceAuth, Authenticator
internal/contract/policy/        Engine, Evaluator, Runner, verdicts
internal/contract/relay/         Relay, Crawler
internal/contract/ssh/           ssh Server, Runner, account, PTY
internal/contract/tenantstore/   Eraser: removes one ServiceAccount and its token Secrets from a tenant cluster's datastore
internal/contract/vault/         KV, Leases, PKI, PKI backend and provisioner, health
```

kit (common helpers):

```
internal/kit/authretry/      retries a call once after an auth error and invalidates the credential
internal/kit/b64/            loose base64 and JSON normalisation
internal/kit/binding/        an APIBinding target and its key
internal/kit/childproc/      child process tail, secret filter, kill, backoff
internal/kit/clock/          sleeps and time formatting
internal/kit/conds/          condition set and status helpers
internal/kit/debounce/       a debouncer
internal/kit/fsx/            JSON file read and write, exists
internal/kit/fswatch/        one inotify watcher per process, multiplexed across subscribers
internal/kit/hostport/       host, port, URL and did:web helpers; listen on a host and port
internal/kit/httpclient/     external HTTP and TLS clients
internal/kit/httpjson/       JSON requests and responses with size and status errors
internal/kit/jsonx/          JSON equality
internal/kit/kcp/            service labels, service FQDN and handler URL in kcp
internal/kit/kcprest/        a rest config for one logical cluster
internal/kit/lease/          a renewing value source that fetches once per expiry
internal/kit/logx/           log level parsing
internal/kit/loopback/       a loopback bridge
internal/kit/networkdial/    dialers and HTTP transports that resolve through a host map; TLS; websocket
internal/kit/opt/            positive-or-fallback option values
internal/kit/servetls/       serving TLS from files and refs
internal/kit/sha256sum/      file and byte sha256
internal/kit/strs/           string helpers
internal/kit/supervise/      a supervised goroutine group with restart backoff
internal/kit/tlsreload/      reloading certificate pools and key pairs
internal/kit/ttlcache/       a TTL cache with single-flight fetch
internal/kit/wsconn/         websocket URL parsing and dial
```

transport:

```
internal/controllers/runtime/            the controller runtime every reconciler runs on: manager, workspaces, server-side apply under a field manager, finalizers, rate-limited backoff, shared status, events (ranks as controllers)
internal/controllers/runtime/kube/       the typed kcp client: workspace clients, apply, get, list-watch awaits, the scheme
internal/controllers/runtime/kcpapply/   applies a manifest set to kcp and reports each change, wait and phase
internal/controllers/runtime/manifests/  loads, renders and rebases manifest sets: values, service suffix, name grants, waits
```

impl, the bus and the component runtime:

```
internal/bus/                     the declared bus: typed handles, caller identity, the handler registry, the ComponentLink check
internal/component/               the component runtime: instance, Service, readiness, endpoints, registry, credentials, HTTP client, trust
internal/component/kcpclients/    the kcp client set a component gets, from the store or a kubeconfig
internal/component/ports/         the port lease registry that a component's listeners bind through
```

impl, the backends of the abstract kinds and the contracts:

```
internal/atprotorepo/remote/      RemoteRepo: a repo on a remote PDS with its credentials
internal/atprotorepo/scoped/      narrows a repo to the record routes it covers
internal/pds/identity/            the Pds identity store and authenticator
internal/pds/local/               the Pds component backend: accounts, sessions, sync, repo and service auth on internal/atproto/pds
internal/pds/remote/              the RemotePDS backend: session, accounts, sync, service auth, credentials
internal/vault/openbao/           the Vault backend: the OpenBao client, admin, PKI, the local server and RemoteOpenBao access
internal/remoteopenbao/           probes a RemoteOpenBao (health, seal status, auth, mounts) and writes Reachable, Unsealed, Authenticated and Authorized
internal/compute/firecracker/     the compute.Provider for FirecrackerVM
internal/events/delivery/         the delivery loop, progress tracker and cursor store the event sources share
internal/events/jetstream/        the JetstreamEventSource backend
internal/events/pds/              the PdsEventSource backend: subscribeRepos through an AtprotoRepo
internal/events/relay/            the RelayEventSource backend
internal/relay/local/             the Relay component backend on internal/atproto/relay
internal/ingress/iroh/            the iroh ingress provider
internal/ingress/opentunnel/      the opentunnel ingress provider
internal/ingress/xrpcsubscription/  the xrpc-subscription ingress provider
internal/dispatcher/tlsproxy/     the DispatcherTLS backend: a TLS front that proxies a guest to the dispatcher by allowed host
internal/guestcp/openbao/         the guest control plane on OpenBao: grants, issuer, store
internal/guestcp/relay/           the guest control plane's relay
internal/ssh/gateway/             the SshServer backend: the ssh gateway
internal/ssh/ownersession/        owner ssh sessions into a guest
internal/lease/local/             the lease contract on local files: slots, ports, subnets, front, rolls, holders
internal/market/bidder/           the bidder role: offering, policy, bids, provision, guest report, submit, StateSyncs
internal/market/participant/      what both roles share: inputs, crawl, comms, credentials, records, policy, service auth, and the StateSync outbox
internal/market/requester/        the requester role: RFP, accept, inbox, events, sessions, secrets, runner
internal/marketidentity/atproto/  the MarketIdentity backend: accounts, association, OAuth, vault state store
internal/marketpresence/          bidder presence heartbeats and MarketPresence names
internal/gateway/                 the gateway's tenant API: cluster view, feed, store, revoker
internal/gateway/custody/         the custodial web sign-in client
internal/gateway/nodeclaim/       maps a NodeClaim to an entry of its Cluster's Infra: secrets policy, pins, lost messages
internal/policyengine/runs/       the policy run backend: run client and decision parser
```

impl, atproto:

```
internal/atproto/cloudinit/       the Go cloud-init modules and BuildUserData: k3s join, secrets, k3s and iroh modules, the module registry
internal/atproto/infra/           the computer.socialweb.infra and computer.socialweb.ephemeralInfra record types (generated by lexgen)
internal/atproto/ingress/         the XRPC ingress protocol: service auth, directory, subscriber, tunnel over the relay, caller, and the relay server (the IngressProxy component and `internal ingress-relay`)
internal/atproto/market/          the market lexicon types (generated by lexgen), records, signer and attestations, discovery, heartbeat, comms, repo store
internal/atproto/oauth/           the atproto OAuth server side: client metadata, DPoP, JWT, scopes, server metadata, store
internal/atproto/oauthclient/     the atproto OAuth client: PKCE, callback, discovery, DPoP, QR session transfer, session store
internal/atproto/pds/             the PDS server: accounts, auth, blobs, repo handlers, sequencer, subscribeRepos, sync, well-known, service auth
internal/atproto/pds/files/       the file-backed blob, key and secret stores of a PDS
internal/atproto/pds/plcsubmit/   submits PLC operations over HTTP
internal/atproto/pdsclient/       the PDS client: local and remote PDS, token and DPoP sources, resolver, service auth, subscribe
internal/atproto/plc/             the PLC directory: documents, signed operations, genesis and tombstone, the server
internal/atproto/relay/           the relay: slurper, sequencer, frames, store, verify
internal/atproto/repo/            the atproto repository: MST, CAR, commits, codec, block store, sqlite record store
internal/atproto/serviceauth/     service-auth tokens: sign, verify, admin, lxm from the path
internal/atproto/xrpc/            the XRPC client: bearer, service-auth and DPoP auth, errors, sync
internal/atprotoauth/             verifies an atproto service-auth token for one XRPC method: signature, audience, method, admitted DID methods, a 5-minute lifetime cap
internal/didkey/                  did:key encode, parse and verify (ed25519, secp256k1)
internal/xrpcserver/              the XRPC server: lexicon routes and subscriptions
internal/oauthcallback/           the registry of OAuth callback handlers
internal/sshtransport/            ssh transport: key authorizer from PDS records, identity resolution, guest and owner sessions
```

impl, exposure, events and status:

```
internal/exposure/                the Exposure publisher, the ComponentLink link policy, the ingress registrar
internal/eventstream/             EventStream matching, dial, the market sink, record watch sources
internal/statusstate/             status sources: PLC identity, relay health, vault credentials and certificates
internal/statesync/               the StateSync GVR; the outbox is in internal/market/participant
```

impl, compute, guests and nodes:

```
internal/computevm/               the ComputeVM host side: guest report, published state, host keys, guest login, termination, vault refs, supervisor
internal/vmruntime/               the Linux VM runtime: prep and one transient VM unit per guest (swc-userns-stage -> pasta -> socialweb-vmexec -> firecracker); `internal vm start|inspect|stop|list` is its CLI
internal/fcapi/                   the firecracker API config and client
internal/guestdisk/               clones and grows the guest rootfs and writes the seed
internal/guestimage/              the pinned guest image store: names, pins, fetch and verify by sha256, GC
internal/guestimagerelease/       picks the guest image source under the release base (guest-images/<target>/<pinned image.json sha256>, then releases/<version>, then main/<commit>)
internal/guestnet/                guest network wiring: pasta argv, MAC, relay targets, the onNetwork URL
internal/guestrootfs/             the guest rootfs build spec (bakes, preinstalls) behind `nodeimage guest-rootfs`
internal/guestseed/               the guest seed image and its user_data modules: guest login, iroh transport, onNetwork reporter, accept bundle
internal/guestidentity/           guest identity: bid config, grants, OIDC, guest login, RBAC sync, rate limit
internal/nodeagent/               the guest node agent behind cmd/socialweb-node: join, CNI, diagnose, hostname, DNS link, masquerade, meshd install
internal/policyaction/            the ComputeVM policy action: writes the ComputeVM that a policy run asks for
internal/infra/                   a deployment's declaration, its record codec and hash, the two-way sync decision, the contract lifecycle and the EphemeralInfra end rule
```

impl, clusters:

```
internal/clusterprovider/         the tenant cluster backend: per-cluster stack and units, bundle, front, holds, links, roll, scheduler, tenant API and credential, vault, postgres, ESO
internal/clusterkcp/              the kcp and tenant API side of a cluster: grants, kubeconfig, CCM grant, ESO install, hardening, node observer, add-on baseline
internal/clusterstack/            the file layout and spec of one tenant cluster stack: TLS, credentials, kubeconfigs, OpenBao, scheduler
internal/tenantstore/kineetcd/    tenantstore.Eraser over kine's etcd v3 API: the pre-start erase of a revoked identity's ServiceAccount
internal/clusterlink/             ClusterLink keys and dumbpipe tickets
internal/cloudprovider/           the cloud-controller-manager logic: node patches from the instance snapshot, provider IDs, pressure
internal/scale/                   the scale decision: baseline, backoff, operators, scale-in, unschedulable pods
internal/scaledriver/             performs the scale decision and keeps its state
internal/nodemarket/              places a node as a NodeClaim in the cluster's home workspace, reads its status for the join wait, closes it by deleting the claim, keeps the k3s node-password file
internal/nodeprovider/            derives a node's DNS-1123 name from the cluster key and the node id
internal/tenantapply/             applies objects to a tenant cluster as its owner
internal/tenantnet/               the tenant node network objects: node-local DNS, node reader RBAC
internal/tenantnet/manifests/     their manifests
internal/identitybundle/          the cluster identity bundle: harvest, sum, cluster-uid
internal/kubeconfig/              builds a client-certificate kubeconfig
internal/eso/                     the External Secrets Operator: bundle, webhook TLS, install beside a cluster
internal/esovault/                the ESO SecretStore for a cluster's vault
internal/clientcert/              records and revokes tenant client certificates, named by the cluster key
internal/clientcertfront/         the edge front that admits tenant client certificates to kcp
internal/revocation/              the revocation plan: effects per cluster, deny, permit, settle
internal/plcresolver/             resolves a did:plc or did:web to its key IDs for the revocation sweep; NXDOMAIN is an outage, not an absence
internal/plcsweep/                the periodic pass that lists DIDs, resolves them and revokes those whose keys are gone
internal/sweepkcp/                the kcp side of that sweep: the worklist from workspaces and AtprotoIdentity records, the deny, the fan-out
```

impl, vault:

```
internal/openbaoctl/              an OpenBaoServer's config, unseal keys, plugins, grants, atproto auth and client policies
internal/baosync/                 diffs and applies the desired OpenBao mounts, auth, policies and plugins
internal/rootvault/               the root vault's paths: storage, config, TLS, client certificates
internal/vaultcert/               vault TLS: the clients CA, client certificates, root and served vault TLS, reload
internal/vaultlayout/             node secret paths, roles and policies (cert auth, node JWT)
internal/credentials/             resolves a credential ref through vault clients built from the kcp store
```

impl, kcp, the store and the host:

```
internal/kcpserver/               runs kcp slots: admin and front kubeconfig, member tokens, roll, kcp version pairs
internal/kcpclient/               the kcp client for homes, clusters, agents, client certificates, cluster links, MarketIdentity and the feed
internal/kcpdatastore/            the kine and postgres datastore of kcp: argv, unit specs, backup retention
internal/storemaint/              store maintenance in the core: kine autovacuum, sqlite and postgres readings
internal/pgunit/                  a postgres install and server: data dir, HBA, DSN, wire readiness
internal/postgresctl/             runs the server of a Postgres object
internal/apiserverfront/          the L4 front that holds kcp's listener fds across slot rolls, and its control socket
internal/tlsfront/                a TLS front with a self-signed or given key pair
internal/bootstrap/               the units that run before kcp exists: profile, observe, ensure, roll, drift
internal/bootstrap/table/         the table of those units per app (gateway, bidder, dev)
internal/hostcontract/            the host contract that check-host evaluates: facts and roles
internal/hostlifecycle/           the file authority behind KcpInstallation, HostInstall and HostRelease
internal/hostos/                  the one platform seam (locking, process identity, liveness, termination, command line, boot id, port range, SysProcAttr); darwin refuses with ErrUnsupported
internal/hostprocess/             reconciles a HostProcess into a unit spec, adopts it by name, mirrors remote objects
internal/hostsvc/                 the systemd user unit backend over D-Bus: units, identity, probes, listen ports, env files
internal/hostsvc/treeaudit/       audits the process tree against the units
internal/systemdunit/             the one unit-file model and writer, restart policy and backoff
internal/systemdunit/notify/      sd_notify: ready, status, stopping
internal/notify/                  the core's sd_notify ready and watchdog
internal/materialiser/            the per-cluster process reconciler: reap, allocate ports, spawn, identity guard
internal/procgroup/               a process group: spec, members, steps
internal/portallocator/           port ranges, purposes and leases, and the advertise address checks
internal/portscan/                finds listener pids (procnet, lsof)
internal/subnetallocator/         allocates guest subnets from a pool and checks host collisions
internal/nsexec/                  `internal ns-exec`: a private hostname and bind mounts
internal/irohdial/                `internal iroh-dial`: dials an iroh ticket through dumbpipe
internal/ttlctl/                  the TTL expiry of finished objects
internal/logcap/                  log rotation caps
internal/diskusage/               disk usage of a path
internal/install/                 install, upgrade, uninstall and GC of a gateway or bidder host
internal/install/templates/       the unit, slice and drop-in templates it writes
internal/instance/                the instance token and the datastore plan it implies
internal/assets/                  the release assets: embedded blobs, store, extract, GC, dev cache
internal/assets/assetpack/        packs the asset blobs (a package main)
internal/assets/blobs/            the per-platform blobs: linux_amd64/, linux_arm64/
internal/versions/                versions.json: component versions, release assets, guest image inputs
internal/buildstamp/              the build identity: dev or release, commit, dirty state
internal/capabilities/            the verb tree of socialweb-computer and what a platform lacks for a verb
internal/clicfg/                  the declarative CLI options: spec, env, config.json
internal/progress/                progress lines and bars for a TTY
internal/devcheck/                checks of a dev host
internal/devinstance/             a dev instance: id, state and runtime roots, layout, units, stand-ins, gateway child
internal/devsup/                  `internal devsup`: the per-instance subreaper of every dev unit, and the offline sweep
internal/gatewayhttp/             the gateway's HTTP: cluster API, downloads, OAuth, pins
internal/appcli/                  the CLI's MarketIdentity client, watch and prompt renderer
```

impl, the Go port of kcp-libs:

```
internal/kcplibs/common/clientlimit/   client rate limits on a rest config
internal/kcplibs/common/ref/           a cluster ref: key, base host, cluster URL
internal/kcplibs/impl/kcpstore/        the kcp object store client used across the code
```

impl, the policy engine:

```
internal/policyengine/atproto/          the policy record client
internal/policyengine/bundled-actions/  the embedded policy actions and workflows: tangy/<action>/ (TypeScript index.ts and its built dist/index.js) and workflows/
internal/policyengine/cache/            the record and verdict cache
internal/policyengine/controller/       engine, workflow, run and trigger reconcile logic
internal/policyengine/deno/             resolves the deno that runs the actions; deno_embedded/ holds its zip in an embed_deno build
internal/policyengine/executor/         the executor: gha-lite and deny
internal/policyengine/models/           requests, results, annotations, cache files, workflows
internal/policyengine/registry/         the action registry
internal/policyengine/status/           phases and conditions
internal/policyengine/trust/            the trust snapshot and resolver
internal/policyengine/workflow/         parses and runs a gha-lite workflow
```

controllers (adapters: kcp objects to contract calls):

```
internal/controllers/atprotoidentity/       AtprotoIdentity: status.state from the account's Clusters (RevocationPending, ColdDeferred, Revoked)
internal/controllers/atprotorecord/         AtprotoRecord: writes the record to its repo and tracks the objects it references
internal/controllers/atprotorecordwatch/    AtprotoRecordWatch: watches repos for matching records and writes the children
internal/controllers/atprotorepo/           AtprotoRepo: binds the backendRef (LocalRepo or RemoteRepo) and mirrors its contract status
internal/controllers/atprotosession/        AtprotoSession
internal/controllers/childapplication/      ChildApplication: the child workspace and its components
internal/controllers/cluster/               Cluster: the cluster pass, and the provider controller on internal/clusterprovider
internal/controllers/clusterroll/           ClusterRoll
internal/controllers/component/             Component: runs it in the component runtime and reports phase and endpoints
internal/controllers/componentlink/         ComponentLink: reports Accepted and ResolvedRefs and puts an accepted link into the bus
internal/controllers/computevm/             ComputeVM: the FirecrackerVM backend, the termination StateSync, the guest report
internal/controllers/events/                registers the event source, event stream and record watch controllers together
internal/controllers/eventsource/           EventSource: binds the backend kind; the helpers the three backend controllers share
internal/controllers/eventstream/           EventStream: connects an EventSource to a sink
internal/controllers/exposure/              Exposure: publishes a service through its ingress
internal/controllers/firecrackervm/         FirecrackerVM: places and runs the guest through internal/compute/firecracker
internal/controllers/guestcontrolplane/     GuestControlPlane: its inputs from ComputeVM, GuestIdentityGrant, MarketIdentity, OpenBaoServer and Exposure
internal/controllers/guestidentitygrant/    GuestIdentityGrant
internal/controllers/hostinstall/           HostInstall
internal/controllers/hostrelease/           HostRelease
internal/controllers/infra/                 Infra and EphemeralInfra: their ComputeContracts (staged, requested, released), their AtprotoRecord and AtprotoRecordWatch, the pull of a record edited elsewhere
internal/controllers/infracollection/       InfraCollection and EphemeralInfraCollection: listRecords of an account's collection into child objects
internal/controllers/jetstreameventsource/  JetstreamEventSource
internal/controllers/kcpinstallation/       KcpInstallation
internal/controllers/localrepo/             LocalRepo: a repo on a local Pds component
internal/controllers/marketidentity/        MarketIdentity: account, OAuth sign-in endpoints, verified associations
internal/controllers/marketintents/         forwards Bid, ComputeVM, StateSync, ComputeContract, Exposure and MarketIdentity changes to the market roles as intents on the bus
internal/controllers/marketpresence/        MarketPresence
internal/controllers/nodeclaim/             NodeClaim: its entry of the Cluster's Infra (the Infra's only spec writer), its bundle, links and Exposure
internal/controllers/openbaoserver/         OpenBaoServer
internal/controllers/pds/                   provides the Pds component's handles on the bus
internal/controllers/pdsaccount/            PdsAccount
internal/controllers/pdseventsource/        PdsEventSource
internal/controllers/plcidentity/           PlcIdentity
internal/controllers/policy/                registers the policy engine, trigger, workflow and run controllers together
internal/controllers/policyengine/          PolicyEngine and its runner
internal/controllers/policytrigger/         PolicyTrigger
internal/controllers/policyworkflow/        PolicyWorkflow
internal/controllers/policyworkflowrun/     PolicyWorkflowRun
internal/controllers/portlease/             PortLease
internal/controllers/postgres/              Postgres
internal/controllers/relayeventsource/      RelayEventSource
internal/controllers/relaystatus/           RelayStatus
internal/controllers/remoteopenbao/         RemoteOpenBao: status only; it never writes the remote vault
internal/controllers/remotepds/             RemotePDS, and its handles on the bus
internal/controllers/remoterepo/            RemoteRepo, and the credentials it provides
internal/controllers/repos/                 registers the repo controllers (AtprotoRepo, LocalRepo, RemoteRepo, RemotePDS) together
internal/controllers/service/               Service
internal/controllers/subnetlease/           SubnetLease
internal/controllers/tenantnode/            a tenant cluster's Node objects, through internal/cloudprovider
internal/controllers/ttl/                   deletes expired objects and reports what it deleted per kind
internal/controllers/vault/                 Vault: binds the backendRef (OpenBaoServer or RemoteOpenBao); roots and secrets on the bus
internal/controllers/vaultcertificate/      VaultCertificate
internal/controllers/vaultcredential/       VaultCredential
```

adapters, XRPC and HTTP routes to contract calls (impl by the checker's path rule):

```
internal/services/bidder/              the Bidder component: routes over internal/market/bidder
internal/services/dispatchertls/       the DispatcherTLS component over internal/dispatcher/tlsproxy
internal/services/gatewayapi/          the GatewayAPI component: the gateway's tenant XRPC API over internal/gateway
internal/services/guestcontrolplane/   the GuestControlPlane component's routes over internal/guestcp
internal/services/ingressproxy/        the IngressProxy component over the internal/ingress providers
internal/services/pds/                 the Pds component over internal/pds/local
internal/services/plc/                 the Plc component over internal/atproto/plc
internal/services/relay/               the Relay component over internal/relay/local
internal/services/requester/           the Requester component: routes over internal/market/requester
internal/services/sshserver/           the SshServer component over internal/ssh/gateway and internal/ssh/ownersession
```

app (factory): the roots and the composition code:

```
internal/app/                     Register: the five root controllers on the controller manager (componentcore calls it once)
internal/app/compose/             what the roots share: desired child specs, apply and drift, ownership and adopt, ComponentLinks, vault, repo, pds, records, streams, operator associations
internal/app/gateway/             the Gateway root: web app account, requester identity, root vault
internal/app/marketbidder/        the MarketBidder root
internal/app/marketnetwork/       the MarketNetwork root: child roots, workspaces, vault, guest TLS, vouches, child VMs; dev up applies, waits for and deletes it
internal/app/marketrequester/     the MarketRequester root
internal/app/sshgateway/          the SshGateway root
internal/stack/bidder/            `run bidder`: the bidder's spec, kcp and core
internal/stack/componentcore/     the component core: the controller manager, every controller registration, providers, component supervision
internal/stack/gatewaykcp/        `serve gateway kcp`: the gateway's kcp, the bootstrap cluster, the clusterprovider env
internal/stack/gatewayssh/        `run gateway ssh`: the ssh gateway's spec, kcp and listener
internal/stack/request/           `run request`: invocations, contract summaries, the requester's kcp
internal/devcore/                 `internal dev-core`: one dev instance's core: components, host processes, postgres, OpenBao, store maintenance, log caps, TTL scope
internal/foldmain/                the Invocation a verb body runs with (args, env, stdio, the resolved bin/<sha>/ path, the verb words) and the re-exec argv it builds
internal/ccmmain/                 `internal cloud-controller-manager`; the clusterprovider re-execs it as `<bin/sha path> internal cloud-controller-manager` with its env rebuilt
internal/checkhostmain/           `check-host`
internal/gatewaymain/             `serve gateway xrpc`: the gateway's config and wiring
internal/ingressrelaymain/        `internal ingress-relay`: the standalone did-key ingress relay of xrpc.fedproxy.com (unix socket behind caddy, sd_notify, graceful stop); scripts/deploy-xrpc-relay.sh installs it
internal/nodeimagemain/           `nodeimage guest-rootfs|guest-manifest`, the two Go steps of scripts/build-guest-image.sh, compiled only into a dev build
internal/providermain/            `serve provider` and `migrate`: the clusterprovider's config, wiring, startup, adopt, roll, scale-in, teardown
internal/tenantcredentialmain/    `internal tenantcredential`
internal/vaultmain/               `vault apply-root`
```

Each `internal/<x>main` is the body of a host verb behind `Main(ctx, foldmain.Invocation) int`;
`cmd/socialweb-computer` dispatches the verb to it with the raw args.

cmd (CLI) and tools:

```
cmd/socialweb-computer/           the multi-call CLI; every host verb lives here (dev, run, apply, install, upgrade, units, serve, vault, check-host, internal)
cmd/socialweb-node/               the guest node agent (internal/nodeagent), built by scripts/build-web-app-downloads.sh
cmd/socialweb-vmexec/             the VM-unit exec asset
cmd/socialweb-apigen/             runs internal/apigen (`go generate ./api/...`)
cmd/openbao-plugin-auth-atproto/  the OpenBao auth plugin for atproto OAuth login (its own module)
cmd/openbao-plugin-auth-sshsig/   the OpenBao auth plugin for ssh signature login (its own module)
internal/apigen/                  generates the APIResourceSchemas and APIExports under deploy/ and the zz_generated Go files from api/
internal/lexgen/                  generates the Go lexicon types (internal/atproto/market and internal/atproto/infra zz_generated.lexicons.go) from lexicons/ (a package main)
hack/archcheck/                   the architecture checker behind scripts/check-architecture.sh
```

Outside Go:

```
deploy/                manifests and manifest sets (apps/, market-network/, eso/), the generated schemas and exports, the audit policies, obligations.txt, bus-bootstrap-callers.txt, check.sh, the guest image and kernel inputs (guest-image/, guest-kernel/), swc-userns-stage/
rust/                  a cargo workspace; rust/socialweb-meshd is the per-node pod mesh daemon that runs INSIDE a market guest (section 9), rust/build.sh its static musl build, rust/zig its cc and ar wrappers
scripts/               the build, deploy and stage scripts; the only TypeScript left in the repository is scripts/*.ts (oauth-signin.ts, associate-bidder.ts, publish-spaces.ts) and the policy actions under internal/policyengine/bundled-actions/tangy/
```

`internal/assets/blobs/.work/` is a scratch directory of the asset build, not tracked by git.

**A cluster's key is derived once, from its home workspace.** `clusterhome.Key`
(`internal/contract/clusterhome`) is the only spelling: it takes a home workspace path
`root:<accountHash>:<name>` - `clusterhome.HomePath` builds it from the account DID and the cluster
name - and returns `<accountHash>-<name>`, which is a DNS-1123 subdomain. Every
place a cluster is identified INSIDE a provider uses it and nothing else: the state dir
(`pdr-<key>`), the node data dir (`<key>/nodes/<node>`), the `Cluster` field of every port and subnet
lease, the node name `nodeprovider.NodeName` derives, the `nodes.json` instance snapshot, the
`scaledriver` state map, the tenant-credential mint file, the `NodeClaim` name and the process-group
attribution in the startup sweep. Both sides reach it through `internal/contract/clusterhome`: the
clusterprovider from a `Ref` (`Ref.Key()` in `internal/clusterprovider/registry.go`), the gateway from
the `(did, cluster)` pair a mint binding carries (`clusterhome.Key(clusterhome.HomePath(did, cluster))`
in `internal/clientcert`), so the file the clusterprovider writes and the file the gateway reads are
named by one function. The bare name is still what a `Cluster` CR, a `Node`
CR's `spec.cluster` and a `NodeClaim`'s `spec.cluster` carry, because those live inside one home
workspace and are unique there; what the key keeps apart is everything OUTSIDE it.

`internal/providermain/migrate.go` is why an existing single-cluster deployment keeps working.
Before the first pass, and only when the CR tree names a given bare name exactly ONCE, it renames
`<state dir>/pdr-<name>` to `pdr-<key>`, `<state dir>/<name>` to `<key>` (the per-node data
dirs under it), the mint credential `<mint dir>/<name>` to `<key>`, and re-keys that cluster's
port and subnet leases in place, preserving their ports and ranges. A name two accounts share is left
alone and reported: which home it belongs to is not knowable from the name, and renaming it onto one
of them would hand that cluster the other's data. It is a one-way move - a downgrade finds the keyed
names and starts from an empty state dir - and the cost of the node names changing with the key is
recorded in section 15.

Binaries: `socialweb-computer` (the gateway, the clusterprovider, the vault and check-host are its
verbs) and `socialweb-vmexec`; `socialweb-node` is the guest agent, built by
`scripts/build-web-app-downloads.sh` and fetched by the guest's bootstrap. Outside `cmd/`,
`rust/build.sh` builds `socialweb-meshd`, which runs inside a market guest beside the k3s agent. The one placement is
`internal/nodemarket`, whose node is a claim rather than a process; the VM runtime
(`internal/vmruntime`) is what the BIDDER runs.

`check-host` is the host contract and nothing else (`internal/hostcontract`, `docs/host-contract.md`):
`install` runs it as the account, and it probes the gateway or bidder host the binary runs on. It does
not carry the retired local substrates' build report (the node image store, kernel, module tree, kvm,
VMM and in-container postgres checks).

`internal/versions/versions.json` pins component versions, the release assets and the guest image
inputs; `scripts/stage-assets.sh` and `socialweb-computer` read it.

### The guest image, and the live gate

A guest image is a kernel, a rootfs and `image.json` per hypervisor and arch. `scripts/build-guest-image.sh`
builds it on any Linux host with docker or podman from the pinned inputs in `versions.json`
`.guestImages`; its Go steps are the dev-build verbs `socialweb-computer nodeimage guest-rootfs|guest-manifest`.
Each output is pinned by its sha256, never embedded in the binary, and fetched and verified by
`internal/guestimage`. It is a TOOL, never a controller, and the image boots
`root=/dev/vda` with no initramfs init. The bidder's VM runtime boots it; the gateway places no image,
because its node is a claim.

The live gate is a live run. A guest comes up only through the RFP flow on a market network
(`deploy/market-network`, `internal/app/marketnetwork`).

### The market network

A market network is one `MarketNetwork` application in its own kcp: the shared atproto services of a
local market plus child application roots (bidders, requesters, gateways) that find each other over
the atproto market protocol. `socialweb-computer dev up --profile market-network` brings one up.
`internal/app/marketnetwork` is its root: it composes the network's own components (Plc, Relay, Pds,
IngressProxy, DispatcherTLS, OpenBaoServer), the child roots in their `root:<app>` workspaces, the
network vault, the guest TLS and the vouches. `deploy/market-network/` holds the network's workspace,
vault and RBAC manifests and the README for its operator. The vendored deno-kcp market example that
this subsection once described, with its TypeScript services, is gone from the repository.

### Access links, state syncs and remote vaults

**A `ComponentLink` is the rule that admits a component call** (`api/exposure/v1alpha1/types_componentlink.go`, `internal/bus/links.go`, `internal/exposure/links.go`, `internal/app/compose/links.go`). It lives in the workspace of its target. `spec.from` names a caller by ObjectRef or by a label selector, `spec.to` names the target, and `spec.capabilities` lists the calls it admits, each a name plus an optional scope; `spec.suspend` takes it out. `internal/controllers/componentlink` reports `Accepted` and `ResolvedRefs`, and only an accepted link admits a call on the bus; a call that no accepted link allows fails with `ErrLinkDenied`. `internal/app/compose/links.go` generates links from the concrete component refs in a root's desired specs: wildcard refs (`*`) are skipped, only component kinds are targets, and `PolicyEngine` is excluded.

**A `StateSync` is the durable runtime outbox** (`api/v1alpha1/types_statesync.go`, the outbox in `internal/market/participant/outbox.go`). Its spec names the source, sink, direction, payload, `idempotencyKey`, conflict and deletion policies, optional ack paths, retry limits and suspend; its status carries the phase, observation, ack, cursor, attempts and conditions. Observe is the authority: the controller applies only when the observation differs, `Apply` is idempotent under the key, and the object becomes Acked only after an observation matches the desired hash. Deletion is `Retain`, `DeleteRemote` or `AckThenDelete`. The status holds refs, CIDs, hashes and metadata, never secrets or payload bytes. The bidder runs the controller with the `atproto.market.event` adapter. The ComputeVM controller creates a `vm.terminated` sync before teardown; its finalizer waits for Acked, then drops after the timeout so the sync outlives the VM. The adapter writes a deterministic payload and `market.event` rkeys. The bidder also writes the `onNetwork` and `hostKeys` report syncs.

**A `RemoteOpenBao` is vault access separate from an `OpenBaoServer`'s lifecycle** (`api/v1alpha1/types_remoteopenbao.go`, `internal/vault/openbao/remote.go`, `internal/remoteopenbao`, `internal/controllers/remoteopenbao`). Both are concrete kinds of the abstract `Vault` (`internal/controllers/vault`). A remote ref is refused with `ErrRemoteDisabled` unless remote access is enabled, uses token or AppRole auth, requires https unless `AllowInsecureHTTP` allows http, and reads its credentials and CA through SecretRefs. The controller probes `sys/health` and `sys/seal-status`, authenticates, checks the declared mounts and capabilities, and writes only status and the conditions `Reachable`, `Unsealed`, `Authenticated` and `Authorized`; it never writes the remote vault. The roots compose a `Vault` whose `spec.backendRef` names their own OpenBaoServer (`internal/app/compose/vault.go`), so local OpenBao access remains the default.

### Composition roots: internal/app (plan 0026 W3-ROOTS)

`internal/app/{marketbidder,marketrequester,gateway,sshgateway,marketnetwork}` are the five app roots; each is one
composition controller on `internal/controllers/runtime`, registered by `internal/app.Register`, which
`componentcore` calls once from its controller manager (every core runs the manager for its own workspaces, and a
root reconciles only the roots of its core's base workspace). `internal/app/compose` is what they share:

- **Writers.** A root applies every child spec, every ComponentLink it needs and its own status by server-side
  apply under the field manager `socialweb.computer/<root kind lowercase>`. A child edited by another manager is
  re-applied: the root applies without force first, and on a field conflict records a `ChildEditReverted` Warning
  Event in the child's workspace (namespace `default`) and applies again with force. Root status is applied with the
  root's manager and never carries `status.market`, which the participant component reports under its own manager.
  Sub-managers of the root: `.../adopt` (owner references on intent objects), `.../identity` (the MarketIdentity
  OAuth endpoints), `.../records` (operator AtprotoRecord status), `.../restart` (the restart annotation).
- **Ownership.** Every child in the root's own workspace carries a controller ownerReference to the root and the
  labels `apps.socialweb.computer/root`, `root-kind` and `root-cluster`; the root watches every child kind by those
  labels. `compose.Adopt` puts the root's ownerReference on unowned Bid, StateSync, MarketPresence and
  ComputeContract objects (and on infra its sets created). A ComputeVM is owned by its Bid, a won Bid stays open
  while its VM lives, and a root's deletion first releases its ComputeVMs, so no TTL on a short-lived object reaches
  a running guest. Cross-workspace children cannot carry an ownerReference: the Gateway's requester MarketIdentity,
  its association watch and records and its market streams live in `root:app`, and a MarketNetwork's child roots
  live in `root:<app>`; they carry the root labels (including the root's logical cluster), are pruned by label and
  are torn down by the root's deletion before it lets go of its finalizer.
- **Links.** A root writes the ComponentLinks its children use, each in the workspace of its target: spec refs
  between components (`xrpc.call`, `stream.dial`), Service and Exposure to their target, policy.run, vault.*,
  repo.*, pds.*, pds.serviceauth, events.* and its own calls as the caller (the root reaches its Vault and its Pds
  through the bus to bootstrap the app account). It waits for its own links to be Accepted before it calls.
- **Suspend.** `spec.suspend` on a root stops participation, never a process: the Bidder and Requester components
  get `spec.suspendParticipation` (no new RFPs, bids, accepts or offerings; the bidder withdraws its offering),
  while releases of running VMs, termination StateSync acks and credential renewal continue. A suspended root
  applies only the children it already has, runs no account, claim or record bootstrap, prunes nothing and reports
  phase `Suspended`. A MarketNetwork suspend cascades into each child root's `spec.suspend`; `spec.apps[].suspend`
  and `spec.components[].suspend` can only add suspension (the latter stops that component's process). Each suspend
  field has one writer: the composer of that object.
- **Operator associations (D21).** `spec.identity.approvedAssociations` on a root is composed into the requester or
  bidder MarketIdentity spec and into the Requester component's `operatorDID`; the MarketIdentity controller only
  reports `status.associations` (declared plus watch-verified). The prod gateway reads them from
  `SOCIALWEB_GATEWAY_APPROVED_ASSOCIATIONS`, so a restart keeps them.

## 13. Invariants

A change must not break any of these. Each exists because something failed.

1. **One apiserver per store.** kine's revision cannot tolerate a second writer.
2. **`--disable-agent`.** Without it "zero nodes" is not a state.
3. **The token never rides argv.** k3s derives its bootstrap key from it; `/proc` is world-readable.
4. **The identity bundle is exactly 13 entries.** A short list makes k3s regenerate CAs and
   invalidate every credential.
5. **`--datastore-endpoint` is required and non-empty.** Empty is a silent new cluster.
6. **Audit flags ride `--kube-apiserver-arg`.** Direct flags are skipped with a warning, leaving the
   cluster silently unaudited.
7. **The advertise port comes from a lease.** A colliding one points a node at another cluster's
   apiserver and reads as an x509 error.
8. **A node's ports are its own, and no lease of the process group covers one.** The kubelet's is
   bound inside the guest's own network namespace, under its own kernel, and no port it binds is
   published on this host. So a node holds no lease to give back: a node's teardown releases nothing,
   and the cluster's own leases - its `k3s-secure` port first - outlive it, because `status.URL` stays live after OFF. This replaces "the kubelet lease is released
   on the NODE's stop, and is not in `DefaultPurposes`", whose purpose is retired: there is nothing
   left to release and nothing left to keep out of a list.
9. **Absent `nodeExpiry` refuses teardown; explicit `0` proceeds.**
10. **Nothing is read back out of a node.** The token and the name are ours.
11. **Never request-header auth against the mount, and no tenant request through it.** Tenant
    requests go through the client certificate front to the cluster's own front.
12. **A revocation needs a consultable deny record**, not just a deleted object.
13. **Secrets never ride argv or `user_data`, and a market node's credentials cross only OpenBao JWT over
    the cluster link.** `user_data` is readable from inside the guest. The NodeClaim controller's join bundle hands a
    market guest through the `--secrets` capability the material a guest may hold before it has authenticated to
    anything: the cluster OpenBao's CA, `claim.json`, and the two capability tickets - the OpenBao
    link's ticket, and the apiserver link's ticket with the node's advertise address and port. A
    capability ticket is one that is useless on its own: the cluster's apiserver still refuses a
    connection without the k3s token, and OpenBao still refuses one without the JWT login. Delivering
    the apiserver ticket with the OpenBao ticket is what lets the guest dial the apiserver while it is
    still exchanging its workload token for that login, instead of waiting for the ticket to come back
    out through the OpenBao it is still dialing. The join token, the node password and the server CA
    are credentials, and they are read by the guest itself from the cluster's OpenBao, after a JWT
    login bound to its workload subject and its claim's audience, over its own dumbpipe link to that
    OpenBao. The apiserver ticket is read from that same KV entry when the bundle does not carry one,
    and the guest then joins the serial way. Nothing reaches a guest over ssh: the gateway opens no
    session to any guest.
    The provider half (KV, policy, mount, role, revoke at release) is in section 7; the guest login is
    `auth/jwt-<12 hex>/login` as role `<node>`, reading `socialweb-nodes/data/<node>`.
14. **A list failure is an error, never an empty observation.**
15. **The state advances only on success.**
16. **No comments in code.** Reasoning belongs in this document. The only comments are the
    `ponytail:` form and functional comments a tool reads (`//go:` directives, generated-code headers,
    code generator markers), and every byte is ASCII. This is a written rule; nothing enforces it.
17. **A gate must be able to fail.** A check whose failure and success look alike is not a check, and
    a probe that cannot fail proves nothing.
18. **The substrate seam does not own the lifecycle.** Closing a node must not be the same act as
    destroying it, or the teardown semantics stop being observable.
19. **A child's output goes to a file, never through a pipe a caller may forget.** A pipe whose
    reader never reads blocks the child once the kernel buffer fills, and the symptom is a readiness
    timeout over a healthy process. A cluster member's output is its HostProcess log file, capped and
    read as a bounded tail (section 5).
20. **The provider's tenant credential is minted, never copied.** It is a client certificate signed
    by that cluster's ClientCA and valid only against that cluster. The k3s-written admin kubeconfig
    is never read: it is shared state on a multi-cluster host, and its server name is unverified
    against the address a node dials.
21. **A cluster's members are not stopped by the provider's own stop.** They are HostProcess units
    that outlive the core and the provider, and the next provider adopts them. Teardown is the
    explicit deletion of the member objects, and it is what stops the units and deletes the
    tenant-side Node object. A member stopped by the gateway's own SIGTERM would bypass both and leave
    a store needing crash recovery.
22. **A workspace ServiceAccount does not leave its workspace.** Its token's scope confines it to the
    logical cluster it was minted in and kcp refuses it everywhere else before RBAC is consulted, so a
    ClusterRoleBinding naming it elsewhere grants nothing and no subject string changes that. Any
    grant whose effect is expected in another workspace is mis-designed, not mis-spelled - which is
    why the runtime holds a credential kcp does not confine.
23. **A spawn failure says why.** The reconciler carried the op and dropped the error, so a process
    group that never came up read as "the cluster is Initializing" and the readiness gate's own
    reason - and the component log under the cluster's data dir - were the only account of it.
24. **A snapshot is a template, never an instance.** Every node boots its own clone of the rootfs.
    One writable image serving as both carries a guest's writes into the next boot, and it presents
    as a corrupted image rather than as shared state; two live VMs on one file corrupt each other.
25. **A boot command line is a console.** Anything on it is written to the guest's console at least
    twice - firecracker logs the body of its own boot-source request, and the kernel prints
    `Kernel command line:` - so no secret may ride it. A market guest's secrets come from OpenBao
    (invariant 13).
26. **A process's identity is read from the service manager and the kernel, and an empty identity
    matches nothing.** A unit is matched by its name and InvocationID, a VMM by its pidfd start ticks
    and its own id (section 5, "Adoption is a tri-state verdict"). Matching every token present, read
    literally, would match every process on the host. A pid whose identity does not match is NOT OURS:
    a reuse stays a refusal and never becomes a kill.
27. **A guest's own account of its own elapsed time is not evidence.** Time a guest by the host's
    clock. A reported 332-second boot was a literal `echo` in an init script, printed 0.29 s after
    the VM started, and it sent this work after a 30x non-issue.
28. **A failure already visible in a guest's console is not waited for.** Named fatal markers end the
    wait immediately, with the guest's own words. A timeout that runs to its end over a host that
    died at its first step reports the timeout instead of the cause, and the two are indistinguishable
    until the clock runs out.
29. **A market node's credentials never ride the claim or the join bundle.** The join token, the per-node
    password, the cluster's server CA and the apiserver ticket live in the cluster's OpenBao at
    `socialweb-nodes/<node>`, and the guest fetches them itself (invariant 13). A `NodeClaim` is a
    cluster-scoped object in the owner's home workspace, readable by anything that can read that
    workspace, so it carries only what is not secret: the PATHS the provider reads and the grant that
    authorizes the guest's fetch (`issuer`, `subject`, `audience`, `role`). The join bundle carries the
    two capability tickets and nothing else: an OpenBao ticket alone buys a dial to OpenBao, and that still
    needs the JWT login, and an apiserver ticket alone buys a dial to the apiserver, and that still
    needs the k3s token, which only OpenBao holds. They also never ride argv, `user_data` or a boot
    command line, for the reasons invariants 13 and 25 already give. This replaces "a market node's
    secrets cross only the SSH session's stdin" (plan 0005 M3).
30. **A cluster is its home workspace, not the name inside it.** Two accounts may each name a cluster
    `ci`; they are two clusters, with two state dirs, two sets of port and subnet leases, two sets of
    node names, two mint credentials and two process groups. Anything that identifies a cluster
    OUTSIDE its home - a state dir, a lease, a node name, a snapshot, a mint file, a process group -
    carries the key `kcpclient.ClusterKey(home)` = `<accountHash>-<name>` and never the bare name. A
    pass over one cluster must never key anything by a name two workspaces can share, because the
    failure mode is not a collision error: it is one cluster reading another's leases and state dir
    and reporting them as its own.
31. **A claim does not wait for the machine.** Provisioning a market node writes the `NodeClaim` and
    returns; the join is confirmed on a later pass. A blocking wait inside a pass that serves every
    cluster in the tree stops being a local delay and becomes a global one, and the thing being waited
    for is minutes away by construction.

32. **A node is counted by the cluster that has it, never by the claim that asked for it.** The count a
    scale decision reads is the tenant cluster's own node list, plus - under a substrate whose
    `Provision` returns before the machine exists - the claims this process holds whose nodes that list
    does not name. A claim's phase is not a count: the NodeClaim controller writes `Joining` when the guest has
    fetched its join secrets, and the Node object it is registering appears in the cluster's list only
    after its agent's connection through the apiserver link is served, so a phase-keyed count has a
    gap with a duration in it and every claim that crosses the gap together is counted nowhere at all
    (section 15).

33. **A guest kernel loads no module on demand.** The market guest's kernel is built with every
    feature the node needs built in (`deploy/guest-kernel`, `CONFIG_MODULES=n`). On the node images
    before it, a module the kernel could not autoload left iptables-nft's native `log` and `limit`
    rules failing with ENOENT and NetworkPolicies silently not enforced (section 15, plan 0005
    M5-NETPOL).
34. **An ingress provider selected by `spec.provider` fails closed.** `xrpc-subscription`, `iroh` and
    `opentunnel` are distinct packages; a missing or non-executable dumbpipe, a missing opentunnel
    command, a missing `spec.iroh` or `spec.opentunnel` block for the selected provider, or an unknown
    provider is an error. The component does not silently start a different provider, and an
    `ingressRef` that resolves to a component with no registrar is an error rather than another
    provider.
35. **A `submit*` XRPC handler enqueues and returns.** The handler parses the body, writes the intent
    durably into kcp - a `StateSync` (`api/v1alpha1/types_statesync.go`, `internal/statesync`), a
    status field, or the requester's inbox - under an idempotency key, and answers. It resolves no DID
    or service, calls no PDS, relay, OpenBao or VM, provisions nothing, verifies no signature and
    delivers no event in the request path. A worker or controller
    (the StateSync outbox of `internal/market/participant/outbox.go`, the requester inbox of
    `internal/market/requester/inbox.go`) owns that work
    and retries it under the same key. The worker calls the counterpart's `submitEvent` XRPC only when
    the counterpart record names one: the bidder from the Accept record's `submitEvent`, the
    requester from the bidder Receipt record's `submitEvent`; with no ref the firehose carries the
    event. A timeout does not make a remote call a request-path operation (section 15).
36. **iroh is the default and required guest transport; a tunnel is an opt-in per contract.** Every
    guest gets the iroh module, the requester's ssh dials the iroh ticket in `status.contract.vmAddress`
    by default, and a contract is Held on the iroh address and the host keys alone. The other two
    transports exist only for the entries of `spec.tunnels`: transport `opentunnel` serves the
    guest's `127.0.0.1:<port>` at an https URL (a `vm.onNetwork` kind `https`), and transport
    `webrtc` bridges a datachannel to it through socialweb-guest-agent (a `vm.onNetwork` kind
    `webrtc` whose address is the agent's DTLS fingerprint). Each comes back signed by the bidder into
    `status.contract.networks` with its tag and kind (plans 0032 and 0034). The requester's tunnel
    module carries config only; the bidder installs opentunnel and socialweb-guest-agent from its own
    GuestControlPlane `/v1/downloads` over the host-loopback map, checked by sha256, and a guest
    fetches no binary from the internet. WebRTC signaling is age ciphertext in
    `compute.events.guest.signalWebRTC`, sealed to a `compute.events.guest.publicKey` of the other side;
    no SDP, ICE candidate, IP address or private key is in a record, kcp object or log, and the only
    STUN server is `stun:stun.cloudflare.com:3478`.
37. **A guest reaches its host's loopback only on its ComputeVM's `spec.hostPorts`.** socialweb-vmexec
    loads the nf_tables bridge table `swcguest` in the guest's namespace before it wires the tap; a VM
    whose table does not load does not start (plan 0034).

## 14. Open architectural gaps

These are the things the architecture does not yet answer, and this list is the work. What is settled - a
decision taken, a defect closed, a measurement recorded - is in section 15 instead, and the two are
separated deliberately: a work list padded with history reads as complete, which is how a bullet that was
already closed, or one whose subject the design has since deleted, survives re-reading. On 2026-10-23
every item was read again against the plan 0026 tree: the items that existed only because of the removed
TypeScript runtime, and the items that later work had closed, moved to section 15 ("Gaps of the removed
TypeScript runtime" and "Items closed by later work"). The items of the earlier batches, from 2026-09-24
to 2026-10-12, are closed; section 15 holds their readings (REL-7, PASTA-1, CHK-1, VAULT-1, VAULT-2,
ESO-2, ESO-4, FLAKE-1, GL-1, GL-2 and MARKET-2 by name), and each plan's decisions are in its file under
`docs/plans/`.

**The live backlog is `NEXT.yaml`, which this section mirrors: an item is written here when it is opened
and moves to section 15, with its readings, when it is closed.** An entry belongs here when the design
does not answer a question the code already asks, when a reading the architecture depends on has no
instrument, or when a settled decision turns out to have been recorded only in a comment. Every entry
closed so far was found by reading the code against this document, or by running it, rather than by
reading the document. The practice worth keeping: the artefact was run and each guard was watched to
fail, rather than the code read against this document alone.

- **Plan 0013 soak paused 2026-10-19 06:28Z (third reading, partial).** `dev-soak-3` ran rc18 code
  1 h warm-up + 2 h 43 min of the fit window at ~1.1 RFP/s: 15,311 RFPs created, 0 failed; 227 tenant
  cycles, 0 failed; 286 VM contracts, 21 failed around chaos; host OOM 0, MemAvailable 82 GB, swap 0.
  Flat by the fit rule: 183 of 209 series, including the gateway requester heap (40-47 MB), its
  sockets (41-47, burst peak 17,077), all repo KV files, OpenBao stores, object counts, record counts
  except three small drifts, kine rows at the network level, and the host disk. **Still growing**, all
  on the installed bidder's systemd-user side: `handles/3de3bcd364/kcp` 240 -> 507 MB (+110 MB in
  2.7 h), its kine file 44 -> 80 MB and rows 5,336 -> 7,826, the bidder's kcp unit memory 1,110 ->
  1,474 MB, its OpenBao HostProcess 63 -> 71 MB, and the guest-kcp pod 76 -> 80 MB. Three series the
  fit called NO are cumulative counters, not gauges: Bids Expired, Bids Lost and gc objects; the fit
  script needs counter differencing. Logs dirs and the host journal grow under their rotation caps.
  These are pre-plan-0026 readings: guest-kcp is now the GuestControlPlane component and runs in the
  bidder core's process, so its series is part of the core's memory. A full 3 h reading on a
  post-0026 build and the counter-differencing fix are needed before 0013 can close.
- **Which bidders get a direct submitRfp (no plan attached; user, 2026-10-18).** A requester posts its
  market.rfp record, and relay/firehose watchers see it; the direct `submitRfp` XRPC is only a faster path and
  the path to bidders that do not watch the firehose. There is no recipient cap and no fill order today. A
  future policy-engine plan decides which bidders get a direct submitRfp and in what priority; plan 0017 no
  longer carries a cap (its M2 was removed).


- **The kcp store keeps a bounded history by mechanism; the plateau reading is open (plan 0013
  M3-STORE-COMPACTION, plateau read by M4-SOAK-READING on 2026-10-18).** Four mechanisms, each read live on
  hat-8 (test-0001, own `dev up` instances, no VM):
  - Compaction stays the apiserver's. kine compacts to the revision the apiserver wrote, and never within
    its last 1000 revisions (`compactIter`, kine v0.17.1), so a quiet store keeps a deleted object's rows
    until 1000 more revisions pass. Reading: 150 ConfigMaps of 400 KB created, patched and deleted left 300
    rows of 400 KB in the store after two compaction ticks; 1300 small writes later the next tick removed
    them. The row count and file size therefore plateau at about the live set plus the last 1000
    revisions, not at the live set.
  - sqlite (a bidder, and the dev instances that use kine-sqlite). The DSN carries `_auto_vacuum=incremental`
    (`kcpdatastore.SqliteParams`; the driver sets it before `journal_mode`, so a new file gets it on its first
    connection). Compaction deletes rows and leaves free pages; with `auto_vacuum=NONE` the file never
    shrinks. `internal/storemaint` runs in the core (`devcore.Run`, every core: bidder, gateway, dev), first
    pass after 30 s and then every 5 minutes: it opens the file as a second connection (modernc.org/sqlite,
    pure Go, busy timeout 30 s), runs `PRAGMA incremental_vacuum(2000)` until the freelist is empty, then
    `PRAGMA wal_checkpoint(TRUNCATE)`, which is what makes the main file shrink under WAL. Reading, the
    `quick` profile: a fresh store answers `auto_vacuum` 2 and `journal_mode` wal; after the churn above the
    pass at 11:35:33 freed 449 pages (247058432 -> 245219328 bytes), at 11:40:34 freed 162, and at 11:45:35,
    one tick after the 1000-revision window passed the churn, freed 59079 pages: `fileBytesBefore`
    246345728, `fileBytesAfter` 4063232.
  - **Only a store created by this binary is incremental.** `auto_vacuum` of an existing file cannot change
    without a full `VACUUM`, and a prod deployment preserves nothing (AGENTS.md, Prod deployments), so no
    `VACUUM` runs: an old file reports `autoVacuum: none` in `status.store` and `incremental_vacuum` is
    skipped for it. The plan item's risk line, a one-time `VACUUM` at the next start, is not built.
  - postgres (the gateway's kcp store). `kcpdatastore.PostgresSettings` (`autovacuum=on`, `track_counts=on`)
    is in the store unit's argv and in `postgresctl.DefaultSettings` (tenant cluster postgres);
    the core sets the kine table's `autovacuum_vacuum_scale_factor=0.02` and `..._threshold=1000` on its
    first successful pass (`ALTER TABLE kine SET (...)`, once kine has made the table) and then reads
    `pg_total_relation_size`, `n_live_tup`, `n_dead_tup`, `autovacuum_count`, `last_autovacuum` over the
    unix socket through `pgunit.Query`. Reading, `prodlike` profile: first read `totalBytes` 253779968,
    deadTuples 54; the pass at 11:31:47 showed `autovacuums` 1, `lastAutovacuum` 18:27:24Z, deadTuples 0;
    the pass at 11:41:47 deadTuples 587 again after more churn. Vacuum keeps the table's size, it does not
    return it to the file system; the table size plateaus, it does not fall.
  - `status.store` (`v1alpha1.StoreUsage`: kind, path, bytes, freeBytes, rows, deadTuples, autoVacuum,
    vacuums, reclaimedBytes, lastVacuumAt, sampledAt) is on `ApplicationStatus`, so the root object of a
    MarketBidder, MarketRequester, Gateway and SshGateway carries it next to `status.stateDir`;
    the four schemas moved a revision. `socialweb-computer status` prints it for the MarketBidder,
    MarketRequester and Gateway roots. Reading: a `market-network` instance's bidder and gateway roots
    (both kine-sqlite in that profile) showed `autoVacuum: incremental`, bytes and rows; a Gateway object
    applied to the `prodlike` instance showed `kind: kine-postgres`, `autoVacuum: "on"`, bytes 254844928,
    rows 1685, `vacuums` 1.
  - kcp starts with `--event-ttl=1h` (`kcpserver.EventTTLFlag`; `CheckArgv` refuses a slot without it).
    Reading: the running `kcp start` command line carried it. The flag is accepted, hidden from `--help`.
    No controller or component in this tree creates an Event object (searched again at the plan 0026
    tree for EventRecorder, `events.k8s.io` and a record broadcaster), so every controller writes status only.
  - Not read: the plateau of file size, kine rows and kine table size under M4-SOAK-READING load. This
    item's `done_when` is that reading, so the plan item stays `todo`. A child kcp of a market network is
    covered because its core also runs `devcore.Run`.

- **Disk use of a long-running instance is bounded by rule; the soak reading is open (plan 0013
  M3-DISK-RETENTION; the kcp audit logs that the 2026-10-18 soak found unbounded are bounded now, section 15).** The
  rules, by owner (the run directory rule of the removed runtime is in section 15, "Gaps of the removed TypeScript
  runtime"):
  - File logs. Rotation is copytruncate with one `.1` copy that keeps the newest `cap` bytes
    (`internal/logcap`), safe because every writer opens with O_APPEND (devsup, systemd `append:`). The
    core rotates every `*.log` in its logs dir and the devsup logs over 64 MiB (`logcap.DefaultCap`):
    `logcap.Keep` makes one pass at start and then a pass on file-system events, debounced to one a
    minute (`logcap.DefaultEvery`). The HostProcess supervisor enforces `spec.output.capBytes` (64 MiB
    when unset) on its watched output files.
  - The journal. A user manager cannot get its own journald size limit: journald keeps user units in
    `user-<uid>.journal` files that share the host's SystemMaxUse pool (default 10% of the file system,
    at most 4 GiB), and LogNamespace= is refused for units of a user manager. So `install` does not
    change the host's journald.conf (the bidder host is shared); it writes `LogRateLimitIntervalSec=30s`
    and `LogRateLimitBurst=10000` into the core unit templates, which caps the message rate of each
    core whatever the host's journald.conf says (journald still scales a burst by free disk space). Units that write
    to a file (kcp, kine, the front, postgres, OpenBao, the VMM) are under the file rules above.
  - Guest images (`<state>/images/<hypervisor>/<arch>/<manifest sha256>`). After it tears a VM down, the
    FirecrackerVM controller (`internal/controllers/firecrackervm`) asks the VM runtime to collect the
    store (`vmruntime` `CollectImages`). An image stays when a FirecrackerVM status names it, a VM work
    dir under `<state>/handles/*/vms/*` boots it, this binary's versions.json pins it, or `bin/current`
    or `bin/previous` names it (`version --json` lists `guestImages`). Everything else goes, and
    `.fetch-` scratch dirs older than an hour go too. No VM teardown means no collection.
  - Unchanged: the asset cache GC and the VM work dir collection (an orphan work dir goes after
    `OrphanGrace`).
  - The reading. Each application's appctl (`internal/app/compose`) samples its state dir at most every
    3 minutes (`StateDirSampleEvery`; bytes on disk from st_blocks, file count) and writes the last
    sample into `status.stateDir` of its root object with the next status write. M4-SOAK-READING
    records the series; the item closes when it is flat.

- **PORT-1: the kcp process piles up loopback connections to the fixed-port front of an old tenant
  cluster until the gateway host has no ephemeral port left (opened 2026-10-15 from a live reading on the
  prod gateway host; `NEXT.yaml` PORT-1).** `listen tcp 127.0.0.1:0` failed host-wide with "address already
  in use" because about 28000 loopback connections from the kcp process were open to the fixed-port front
  of each of two old tenant clusters (canary on port 20000, goat-demo on port 20018), which exhausted the
  ephemeral range for every port 0 listener on the host. The relay in `internal/apiserverfront/front.go`
  closes both ends when either end closes, so the connections are not held by the front; they are held on
  the kcp side, as dials from kcp to the front of an old cluster. Deleting the two clusters released the
  ports and the host recovered. The origin is not found: which kcp component dials the front (the mount,
  the kubeconfig mount child, a controller informer) and why it opens a new connection instead of
  reusing or closing one is open. Every listener on the host binds port 0, so one cluster front that
  leaks connections takes the whole host down: a host-wide failure with a per-cluster cause. The
  instrument that closes it: the count of established loopback connections from the kcp process to each
  cluster front on the gateway host, read with `ss -tn state established '( dport = :<front port> )'`,
  stays flat across one hour with the cluster idle, for a healthy cluster and for a cluster whose
  apiserver is stopped, and the count of sockets in the ephemeral range shows headroom while a cluster is
  deleted and recreated.
- **A HostProcess has a memory limit; a component has none, so one component's leak takes the whole core
  (plan 0013 M3-DENOPOD-LIMITS, rewritten for plan 0026 on 2026-10-23).** Every HostProcess unit gets a
  `MemoryMax` and `MemorySwapMax=0`: postgres 1 GiB (`pgunit.MemoryMax`), kine 512 MiB
  (`kcpdatastore.KineMemoryMax`), a kcp slot 3 GiB (`kcpserver.SlotMemoryMax`), the front 256 MiB
  (`kcpserver.FrontMemoryMax`), OpenBao 512 MiB (`openbaoctl.MemoryMax`), a stand-in 1 GiB
  (`bootstrap.StandInMemoryMax`); VM units carry the VM's memory plus overhead, and a HostProcess object
  that sets none gets 2 GiB (`hostprocess.DefaultMemoryMax`). The `dev` backend (devsup, `proc`) enforces
  no unit limit by design. The components (Pds, Plc, Relay, Bidder, Requester and the rest) run in the
  core's own process, in the component runtime of `internal/component`, and the core unit carries no
  `MemoryMax` (the installed templates set `Delegate=yes` and `OOMPolicy=continue` and no limit). A
  root's `spec.components` may carry `ComponentLimits`; appctl resolves and reports them, and writes
  `LimitsEnforced=False` (reason `ReasonComponentLimitsInactive` in `internal/app/compose/status.go`)
  because the in-process runtime cannot enforce them. So a component that leaks grows the core until the
  kernel or systemd stops it, and every component of that core restarts with it; nothing names the
  component that breached. The per-process limits that the removed runtime enforced, and their readings,
  are in section 15 ("Gaps of the removed TypeScript runtime"). What would close it: a per-component
  bound the runtime can enforce in process (a heap budget or a bounded queue per component), or a
  component kind that runs as its own HostProcess unit, and a reading of a breach on an installed core.
- **Open after the 3 h soak (plan 0013 M4-SOAK-READING, 2026-10-18; the readings are in section 15).** Each is a series that
  kept growing, with its measurement and the smallest fix named.
  - The hono-pds block store, the requester's vm.delete and market.event records, the requester's orphan records, the bidder's
    bid record cap, the requester heap and the gateway requester's retention are closed (section 15, "hono-pds collects
    unreachable blocks" and "The requester's orphan records, the bidder's bid sweep, the requester heap and the gateway
    requester's retention"). Left open from the dev-bound-1 reading and the second 3 h soak on dev-soak-2 (2026-10-18, section 15,
    "Plan 0013 M4-SOAK-READING: second 3 h soak, systemd-user"); each is named with its slope over 20:35Z to 23:36Z:
  - Requester native memory (the bursts of ESTAB connections to the dispatcher) and the bidder PDS growing to its limit are
    closed (section 15, "The contract watch's getRecord bursts", read on gl-runner-1 after this soak). Left from that reading,
    taken before plan 0026: the soak requester's RSS read +75.6 MB an hour (300 to 362 MB) with its fds flat and its heapUsed
    at 75 to 119 MB while its ComputeContracts still rose (2441 to 2702). The Requester component
    (`internal/services/requester` over `internal/market/requester`) now runs in the core's process, so the series to read is
    the core's RSS over a window past the contract TTL.
  - Deleting a tenant cluster takes more than 4 minutes in some cycles under 8 lanes (the driver's 4-minute wait timed out 30
    times; the clusters were gone later; the gateway core logged "a step of deleting this cluster has not completed" 67 times).
  - Every status write logs `Warning: unknown field "status.store"` (1063 of 3000 lines of the network core log): some root kind
    lacks `status.store` in its schema. At HEAD the MarketBidder, MarketRequester, Gateway and SshGateway schemas carry it and
    `deploy/marketnetwork-apiresourceschema.yaml` does not, nor does `MarketNetworkStatus`; not re-read since plan 0026.
  - The node claim index, the pod cgroups, the spec-change restart, the bidder's bid TTL and the relay backlog are closed
    (section 15, "Gaps of the removed TypeScript runtime" and "Items closed by later work").

- **A market network's children cannot run under the systemd-user backend (plan 0013 ACC13-NO-HOST-OOM, read on dev-soak-2,
  2026-10-18; rewritten for plan 0026 on 2026-10-23).** `dev up` refuses `--host-backend systemd-user` (`bringUp` in
  `cmd/socialweb-computer/dev.go`: "dev places every unit under devsup (proc); systemd-user is for install"). So every
  HostProcess of a market network (kcp slots, kine, OpenBao, the fronts) runs under devsup with no `memory.max`, and only the VM
  units go to the user manager (`--vm-host-backend systemd-user`). `run bidder --host-backend systemd-user` runs only for an
  account that `install bidder` made (`bidder.CheckAccount` in `internal/stack/bidder`). The second soak therefore read a breach
  on a second bidder made with `install bidder` and pointed at the network with `--market-network-file`. The components of a
  market network run in its cores' processes and have no limit on any backend (see the item above). The change that closes it
  lets `dev up` place its children's cores and HostProcesses as user units (Delegate=yes, OOMPolicy=continue) and carries the
  backend into the child records.

- **The bidder's kcp wait on the hot path: the prod reading is open (plan 0009 ACC9-LATENCY, 2026-10-17).**
  The cuts and the readings on dev-linux-1 are in section 15 (M4-LATENCY-READINGS item 10): the kcp wait
  went from 30 / 42 to 11 / 13 ms p50 / p95 (4 at a time, one series of two at 25.7) and from 30 / 93 to
  12 / 16 serial. What is left is the prod bidder's own `status.market.kcpWait` over 256 RFPs after the
  deploy. If its p95 is over 20 ms at a host load under 16, the next cut is the Bid create beside the
  bid config and payload PDS writes. If it holds, this item closes into section 15.

- **The Windows bidder: the seams as they landed (plan 0011 M0-REVISIT, 2026-10-16, read at 79f40adb;
  re-read against the plan 0026 tree on 2026-10-23).** `GOOS=windows GOARCH=amd64 go build ./...` and
  `go vet ./...` are green (`deploy/check.sh --fast` builds windows/amd64 and windows/arm64). The plan was
  written against names that did not land:
  - The VM seam is `vmruntime.Runtime` (Prepare, Placed, Reserve with a `Placer`, Await, Start, Inspect
    returning an `Observation`, Shutdown, Stop, List, Cleanup, Watch, Load, CollectImages). The verdict
    type is `hostsvc.Verdict` (an alias of `internal/contract/host`'s). The service-manager seam is
    `hostsvc.Manager`; its `UnitSpec` is an argv plus cgroup limits, so it cannot describe an HCS system.
    The FirecrackerVM backend (`internal/compute/firecracker`, driven by
    `internal/controllers/firecrackervm`) does not call the runtime alone: `Reserve` hands a `Placer`
    that creates a `HostProcess` child, the supervisor starts the unit through `Manager`, and the backend
    reads liveness, orphans, exits and events from `Manager`. A Windows VM backend therefore needs the
    backend to ask the runtime, not the manager (item M1-CONTROLLER-RUNTIME-OWNED).
  - `status.runtime` is `{backend, ref (the unit name), instance (the InvocationID), pid, startTime (start
    ticks as a string), hostBootID, vmmID}`; `Backend` is a free string in the API, but `hostsvc.ParseBackend`
    accepts only `systemd-user` and `proc`.
  - kcp is a pinned asset (at HEAD built here from kcp-dev/kcp main at 74e9fb9c, version
    `v1.36.0+kcp-v0.33.0-162-g74e9fb9c03f778` in `internal/versions/versions.json`) in its own unit
    behind kine on sqlite (cgo) and our front, not in-process. The assets table has no windows/amd64 kcp, kine or any other entry
    (`platforms["windows/amd64"]` is status seam with zero assets). The `proc` backend is `devsup`, which
    is Linux-only. The bootstrap layer is installed systemd units.
  - The guest image is a `guestImages` target built by `scripts/build-guest-image.sh`, never embedded, fetched
    and verified by `internal/guestimage`; `hyperv-x86_64` is a reserved target name. It has no initramfs:
    the kernel boots `root=/dev/vda` from the rootfs ext4 and cloud-init reads a pure-Go NoCloud cidata
    drive (`internal/guestseed`). There is no `socialweb-nodeinit` in the VM path.
  - The guest network is constant: guest 172.30.0.2/30, gateway 172.30.0.1, MAC 06:00:ac:1e:00:02, host
    address 192.0.2.1 (`internal/guestnet`, pasta maps 192.0.2.1 to the host loopback). A guest reaches the
    host loopback only on the TCP ports of its ComputeVM's `spec.hostPorts` (default none): socialweb-vmexec
    loads an nf_tables bridge table `swcguest` in the guest's namespace before it wires the tap, which accepts
    those ports to 192.0.2.1, resets every other TCP connection to it, drops all else to 192.0.2.1 and
    127.0.0.0/8, and drops every frame from the tap to the namespace's own stack; pasta runs with
    `--no-map-gw` and no port forwards. The bidder sets `spec.hostPorts` from its `GuestHostPorts`, and in a
    market network adds the guest TLS port and the ports of the user_data's 192.0.2.1 and `*.localhost`
    URLs (`guestseed.HostPortsOf`). There is no network
    index allocator. An HNS NAT network per host cannot give every guest this plan; M0-HNS-NETWORK measures
    what Windows can do with it.
  - iroh is the default and required guest transport; guests have no JSR registry or websocat, and a
    tunnel module (opentunnel or socialweb-guest-agent, installed by the bidder from its own
    `/v1/downloads`) only when the contract's `spec.tunnels` asks for it (plans 0032 and 0034).
  - The core binary installs to `bin/<sha>/` with `bin/current` and `bin/previous` (not `versions/<sha256>/`);
    assets are `runtime/<sha256>/`, collected when unreferenced. `install` is behind `install.System` and
    `UserManager`, which are systemd and account shaped (`system_other.go` refuses on !linux). The bidder
    core is `Type=notify` with a watchdog (`internal/notify`).
  - The components run in the core's process (`internal/component`), so they need no process seam on
    Windows. Plan 0011 names a runner that no longer exists. The policy engine still starts its embedded
    deno as a child process to run the policy actions (`internal/policyengine/workflow`), outside the
    HostProcess units.
  - Windows stubs today: `hostsvc.Open`, `OpenProcClient`, `ListenPort(s)`; `vmruntime.ForHost`;
    `hostos.BootID`; `install.NewSystem`, `SelfCheck`, `RestartOwnUnits`, `NewStagedSystem`;
    `hostcontract.Gather`; `guestnet.Wire`; `devsup`; `nsexec`; `apiserverfront` hold;
    `cmd/socialweb-vmexec`; the release asset blobs (`assets_release_unsupported.go`). `hostos` process
    identity, lock, port range and `DieWithParent` (empty) have Windows bodies.
  - Decision (decided by Claude, user may veto): on Windows every non-VM host process of a bidder (kine,
    kcp slot, front, OpenBao, the policy engine's deno) is a child of the bidder service inside a
    kill-on-close job object. Only VMs outlive the core, and HCS owns them. This replaces the plan's
    pre-installed vault service.
  - Decision from the user (2026-10-16): the Windows desktop is never rebooted, shut down or signed out. A
    reboot-only proof is `blocked` and its closest no-reboot equivalent is measured and labelled.
- **An application owns its PDS: the PDS is the Pds component in its own kcp (plan 0018, opened
  2026-10-17; rewritten for plan 0026 on 2026-10-23).** Bob's bsky.social account hit the hosted cap of
  35000 write points per 24 hours because the prod bidder (MarketIdentity mode oauthQr) wrote every market
  record into Bob's own repo. Since plan 0018 the bidder's MarketIdentity is mode localRepo and its repo is
  a LocalRepo on the application's own Pds component (`internal/services/pds` over `internal/pds/local`;
  the repo kinds are in section 12). appctl writes the repo key once into the application's OpenBao
  (`identity/bidder/keys`, field `repoKeyHex`, `internal/app/marketbidder` and `internal/app/compose/vault.go`);
  the Pds component serves the repo on port 0 and, through the app's IngressProxy, on the public URL in
  the DID document's `atproto_pds` endpoint. Records the application declares are AtprotoRecord objects
  that appctl composes in Go (`internal/app/compose/records.go`: the operator vouch, the association
  record, `OnMissingReference: Delete`); `internal/controllers/atprotorecord` writes each to its repo,
  keeps it Pending with condition ReferencesResolved False, reason MissingReference, while a runtime
  reference is unresolved, rewrites it when a referenced value changes, and deletes it with the object
  (finalizer `atprotorecord.socialweb.computer/repo`). The association is dynamic (see "The operator
  comes from the event stream" below). The gateway requester side: the Gateway root composes the same
  Pds component for the MarketIdentity `requester` (mode localRepo, AtprotoRecordWatch `association`
  from `deploy/apps/repo-pds/association-watch.<profile>.yaml`), with its key at
  `identity/requester/keys` (`internal/app/gateway/vault.go`), so rfp, compute.vm, accept and rbac
  records no longer go into alice's bsky.social repo; spec.ownerDid stays the account of a contract for
  the per-account limits. People still sign in to the web app through the atproto auth mount; only the
  identity that posts RFPs changed. Plan 0018 is marked done. Open: the live proof's readings were not
  written here, and it is not read whether prod requesters find the bidder DID through the relay index
  or need it in the Gateway's `bidderDids` (the operator's vouches name Bob, not the bidder DID).
- **The operator comes from the event stream, not from the config (plan 0018 M4-DYNAMIC-ASSOCIATION,
  opened 2026-10-17).** Neither the Gateway spec nor the MarketBidder spec names an operator; `operator`
  (handle or DID) is an optional allow-list on both. Records are public and a new DID is visible on
  plc.directory, so a claim also carries a nonce: random, single-use, kept in the application's OpenBao
  (`identity/<name>/claim`), shown only in the MarketIdentity status `pendingAction.url`
  (`<qrOrigin>/#bdr|plc=<DID>&n=<nonce>&s=<service>`, admin-only kcp status). The operator writes, from
  any atproto client, one `com.publicdomainrelay.temp.badgeBlueKeys` record in its own repo:
  {keyId: the application's DID, challenge: the operator's DID, service: bidder_associate |
  requester_associate, nonce, createdAt}. The kind AtprotoRecordWatch (export marketidentities) is the
  mirror of AtprotoRecord: spec {repo, collection, match: [{fieldPath, value | valueFrom}], source:
  {eventSourceRefs}}, status {records: [{did, handle, uri, cid, value, verifiedAt}], cursors:
  [{source, cursor}], conditions: Verified}. The AtprotoRecordWatch controller
  (`internal/controllers/atprotorecordwatch`, in the core since plan 0026) reconciles each watch: it
  writes an EventStream child per source (`internal/eventstream` plans them; relay subscribeRepos with a
  client-side filter, Jetstream `wantedCollections` in `internal/events/jetstream`), takes the matches
  the streams report only as triggers, and verifies the records through its backend's `Verify`, which
  fetches each with com.atproto.repo.getRecord from the author's PDS; only a verified record stays in
  status. The condition Verified is False with reason MatchPending or StreamNotReady until the streams
  are ready, and VerifyFailed after a failed pass, which is retried after 30 s (`VerifyRetry`); a passing
  watch is verified again every 5 minutes (`VerifyInterval`; both listed in `deploy/obligations.txt`).
  Each source resumes from its cursor in status. `deploy/apps/repo-pds/association-watch.<profile>.yaml`
  declares `association`: keyId equals valueFrom MarketIdentity status.did, service equals the
  application's service; its sources are the EventSources the root composes: the network relay
  (`relay-network`, from the network value's relay host) or, with no relay host, two public Jetstream
  instances (`jetstream-bsky-us-east`, `jetstream-bsky-us-west`). The MarketIdentity reconciler
  (mode localRepo, `spec.associationWatch`) waits on the watch: the first verified record whose author
  is its challenge and that carries the current nonce (or whose author is the allow-listed operator)
  wins; it is written to `spec.approvedAssociations` and the nonce is deleted. Then the identity is
  locked: a later claim needs an admin patch of `spec.approvedAssociations`. The association record
  (`onMissingReference: Delete`) follows `approvedAssociations[0]`. Revocation: once the watch is
  Verified and no record of an associated DID is left, the reconciler removes that DID from
  `spec.approvedAssociations` (condition Associated False, reason Revoked), the association record leaves
  the repo, and a fresh nonce is shown. A restart keeps the association because the watch re-verifies
  the operator's record. The associateConfirm call stays a fast path for an allow-listed operator.
  Open: the did-key-associator QR page (qr.fedfork.com) does not yet copy `n` into the record it writes.
- **A prod tenant owner's repo through the web app session (plan 0031 REC9-PROD-OWNER-SESSION).** Built: the
  MarketIdentity mode `oauthSessionProxy` (`spec.did`, `spec.sessionRef` {Vault `root-vault`, mount
  `auth/atproto`, path `session/<did>`}) reads the session the openbao-plugin-auth-atproto mount holds
  through the bus capability `vault.atproto.session` (contract `vault.AtprotoSessions`, the handle logs in
  as the root vault client `atproto-session-proxy`, policy `atproto-session-proxy`, scope the one session
  path the ComponentLink grants); its pds.Authenticator asks the plugin's proof endpoint for one access
  token and one DPoP proof per request, and only the plugin refreshes the session. No session, or an
  expired or revoked one, is phase SessionExpired with reason NoSession or SessionInactive and
  pendingAction signInAgain. A session must hold the scope that the sign-in which produced it requests
  today: for this mode that is the gateway web app sign-in, whose scope (`compose.GatewayAtprotoScope`,
  the same value the atproto plugin config requests) the Gateway root writes into `spec.sessionScope`;
  the plugin's recorded granted scope is compared with it (`oauth.MissingScopes`), and a session that
  lacks part of it is SessionExpired with reason ScopeChanged ("the web app session of <did> lacks
  scopes the gateway web app now requests (...); sign in to the web app again"). The other OAuth modes
  compare with the scope their own flow requests (the core's role scope; for oauthQr the bidder scope
  the associator grants, not the QR page's union). A stale owner pauses only its own Infra writes: its
  RemoteRepo is not Ready with the identity's message, its Infra records are not Synced, and the
  Gateway root stays Ready with condition SessionOwnersReady False naming the owner. The identity
  returns ErrSignInAgain, so the runtime's rate-limited backoff (at most 5 minutes) reads the plugin
  again; a new sign-in makes it Ready with no other step (the plugin pushes no session change).
  The Gateway root composes it, as `owner-<stamp>` in `root:<gateway>:app`
  with label `apps.socialweb.computer/session-owner`, for each DID that opted a Cluster in
  (`spec.publishInfra`, not deleting) and is not in the market network owner sessions file nor the
  requester's own, and keeps it while an infra AtprotoRecord names its repo; once the identity has reported
  the session's PDS (Ready, or SessionExpired after that) it adds the RemotePDS of the session's PDS, the RemoteRepo and the AtprotoRepo `repo-<did>`. The web app
  scope carries `repo:computer.socialweb.infra` (create, update, delete), so owners sign in again once.
  A signed-in owner opts in through the web app too: `POST /api/clusters/publish` {name, publish} calls
  the same SetPublishInfra as the XRPC procedure. Open: the prod reading of done_when (a market network gateway has no public-origin, so no web sign-in).
- **The market network runs its applications in one kcp: what is left (plan 0019).** The design as built and
  its readings are in section 15, "The market network is one kcp (plan 0019, 2026-10-18)". Open: the web
  app's account and home workspaces (`root:<account>:<home>`, the tenant clusters) still sit at the root of
  the network's kcp, not under root:gateway (kcpclient, the clusterprovider registry, sweepkcp and the
  tenant credential paths take `root` as their base); the composer still copies the network value and the
  bidder DIDs onto the root objects (no record carries them); the per-child core holds the network's admin
  kubeconfig (the shared kcp isolates components by workspace RBAC, not the cores); the host map of a core's
  component runtime (`component.NetworkHostMap` in `internal/component/network.go`, built from the
  component registry) holds the names of every workspace whose components that core runs, each name
  carrying its workspace; its lookup (`Address`) takes no caller, so a component can resolve another
  workspace's names.
  ACC19-ONE-KCP is marked done in plan 0019, but the integration tests it named were deleted by plan 0026,
  so the one-kcp network has no standing check until plan 0027.

## 15. Settled decisions, and the defects they closed

- **A tenant cluster's nodes are one Infra (plan 0031, 2026-10-24).**
  Before: every NodeClaim got a ComputeContract `nodeclaim-<20 hex>` that the NodeClaim controller
  applied, released (spec.release), deleted and orphan-swept itself, listing every ComputeContract of
  the gateway workspace on each reconcile and mapping contracts back through `spec.nodeClaimRef`; the
  Gateway root's teardown was DeleteAccountClusters, then a wait on `compose.HeldContracts`. After: the
  NodeClaim controller is the only spec writer of one Infra per Cluster with a claim (`nodes-<hex>` in
  the gateway workspace, `spec.clusterRef` from the claim's Cluster ownerReference, because the claim's
  `spec.clusterRef.name` is the cluster key), one entry `node-<hex of the claim uid>` per claim (Staged
  until the bundle is written, Requested, Released on the claim's deletion), the node fields in the
  kcp-only `spec.nodes`; the Infra controller runs the ComputeContract `infra-<infra>-<entry>`;
  `status.infra` names the entry; `ComputeContract.spec.nodeClaimRef` is gone. A Cluster that is
  deleting gets its Infra deleted at once, and a root deletes its deployments (`compose.DeleteDeployments`)
  before its contracts and components: the Gateway after DeleteAccountClusters, the MarketRequester
  first (plan 0029 R8). An unbound Infra keeps its record (AtprotoRecord `deletionPolicy: Retain`,
  `status.record.direction: Retained`; plan 0029 U1). Readings (own network nodes31, `--bidders 1
  --with-gateway`, dev builds of the branch over f8da10c9, 2026-10-24 UTC):
  - ACC31-SCALE-UP-TWO: a 2-replica Deployment with hostname anti-affinity (applied 05:32:12) made two
    claims; their entries went Requested at 05:36:12.55 and 05:36:12.59, both contracts Held and the
    Infra `nodes-bbba9dd001a611921f2f` Deployed "2 deployed" at 05:38:21.859, the claims Running at
    05:38:32.752 and 05:38:36.814 with `status.infra` naming their entries, both tenant nodes Ready, each
    contract owned by the Infra with nodeJoin, ownerDid and the bundle secretsRef; no `nodeclaim-*`
    contract was written.
  - ACC31-SCALE-IN-ONE: replicas 1 at 05:42:15; after the hold the entry `node-8b685c737dff` went
    Released (05:44:15.725), its contract Released 05:44:15.819, the claim went 05:44:15.884, the entry
    was dropped 05:44:15.912 and the contract deleted 05:44:15.935; its guest was gone by 05:44:32; the
    other entry, contract (uid e6edc9eb), claim (uid b6f9ef8a) and node stayed Requested, Held, Running
    and Ready.
  - ACC31-CLUSTER-DELETE: Cluster `tenant` deleted 05:47:31.394; `node_claim_infra_deleted` 05:47:31.475,
    contract Released 05:47:31.528, Infra gone 05:47:31.567, claim gone 05:47:41.515, Cluster gone
    05:47:42.741, the guest gone at 05:49:04 after its graceful stop.
  - ACC31-GATEWAY-ROOT-DELETE: cluster `busy` with a Running node and pod; Gateway root deleted
    05:51:51.694; Deleting "1 Cluster(s) tearing down" 05:51:54.908, its Infra deleted and the contract
    Released 05:51:55.042, Infra gone 05:51:55.119, "13 component(s) stopping" 05:52:06.551, Requester
    deleted 05:52:06.727, root gone 05:52:07.712 (16.0 s; plan 0028 M4 read 17.7 s); the guest went at
    05:53:26; no Gateway, Infra or ComputeContract was left.
  - ACC31-R8-ROOT-DELETE: MarketRequester root `root:infra-r` with Deployed Infras r8 (bound) and u1
    (unbound) deleted 05:41:52.538; Deleting "2 deployment(s) releasing" 05:41:52.759, both contracts
    `requester_released` by 05:41:52.775, both Infras gone by 05:41:52.864, then "8 component(s)
    stopping" 05:41:53.319, root gone 05:41:55.482; the guests were gone at 05:41:56; the repo then held
    the u1 record only (r8's was deleted).
  - ACC31-U1-UNBIND: `spec.record` removed from the Deployed Infra u1 at 05:39:19.356 (and u3 at
    05:47:01.413 on the final build): `infra_record_unbound` and `atprotorecord_retained` within 0.1 s,
    the AtprotoRecord, its watch and deliver link gone, the Infra Deployed with its guest Running, the
    record still listed by listRecords, `status.record.direction: Retained`.
  Open: a status apply that sets a field to null (`internal/controllers/runtime` `dropped`) left
  `status.record` in place on this kcp (the field's managedFields entry is an Update operation of the
  same manager), so a status field another pass removes may not go; read before relying on it.

- **A deleted ComputeContract releases its guest (plan 0028, moved from section 14, 2026-10-24).**
  Before: a ComputeContract carried no finalizer, so a delete removed it at once and the requester saw
  only Gone: `Requester.Observe` dropped its caches and never started a release, the runner kept holding
  in `waitForRelease`, and no vm.delete was sent. A won Bid never expires, so the guest held a host core
  until someone removed it by hand. Two deleters reached this path: a direct delete of a held contract,
  and the deletion of a MarketRequester root, whose delete pass stopped the Requester before kcp garbage
  collection deletes the contracts it owns. R0-REPRO reading (own market network p28, `--bidders 1`,
  test-0001, dev build of c30c3dab): run 1 held `root:r0a` contract `contract` at 03:42:13Z and deleted
  it directly at 03:43:38Z; the object was gone at once, the requester logged nothing after the delete,
  and at 03:48:46Z (5 min 8 s later) the requester repo held 0 vm.delete records, Bid
  `bid-80d63ec51b896211ef8b` was Won, ComputeVM and FirecrackerVM `fc-xxyudiphteqt-d5414751` were
  Running and unit `socialweb-vm-20b17a6d47-fc-xxyudiphteqt-d5414751.service` active. Run 2 held
  `root:r0b` at 03:45:03Z and deleted its MarketRequester root `app` at 03:46:17Z; the Requester logged
  `requester_contract_left_for_restart` at 03:46:18Z, the contract went by garbage collection at
  03:46:19Z, and at 03:51:21Z (5 min 4 s later) the repo held 0 vm.delete records, the Bid was Won and
  ComputeVM `fc-6hawypma76qe-46183fe5` Running. The market network applied a new root `app` in
  `root:r0b` within seconds; its Requester found no contract. Both guests were removed by hand
  (ComputeVM delete) at 03:51:33Z.
  After: the ComputeContract forwarder of `internal/controllers/marketintents` holds the finalizer
  `computecontract.socialweb.computer/release` (`v1alpha1.FinalizerComputeContract`) and returns Waiting on
  a deleting contract while the requester's `Requester.releaseWait` answers a wait: the requester lets go
  when the contract is Released, never wrote an accept and has no runner, or its `status.release.deadline`
  passed after at least one attempt, when it writes Failed "vm.delete was not acknowledged by <deadline>
  (the contract TTL): <last error>" (the plan 0020 bound, which the Go port had left as Releasing). With
  no requester subscribed the finalizer stays only while `ComputeContract.HoldsGuest` is true, so a
  Released or never-accepted contract never blocks a workspace delete. Gone stops the runner (its context
  is cancelled and it writes and logs nothing more) and logs `requester_contract_gone_unreleased` when the
  finalizer was removed by hand. The MarketRequester delete pass deletes the root's ComputeContracts and
  reports Deleting "<n> ComputeContract(s) releasing" before it stops a component; a deleting intent
  object now requeues the roots of its workspace (`intentWatch` in `internal/app/compose/root.go`), which
  it did not for an owned object. The Gateway delete pass stopped its Requester with no look at the
  NodeClaim contracts of its tenant clusters (M4 read: unsafe for a direct delete of the Gateway root;
  the network's teardown deletes the account workspaces first). It now first deletes every Cluster in
  the account workspaces (`compose.DeleteAccountClusters`; it writes no Cluster spec) and reports
  Deleting "<n> Cluster(s) tearing down" while any is left, a Cluster event requeuing a deleting
  Gateway root (`compose.TeardownWatch`); each Cluster teardown deletes its NodeClaims, which the
  still-running Requester releases, each bounded by its release deadline; then it waits while
  `compose.HeldContracts` counts a contract that holds a guest, and only then releases its records and
  stops its components. The requester logs `requester_release`,
  `requester_release_retry`, `requester_released` and `requester_release_bound`. Readings (same network,
  dev builds of this branch, 2026-10-24 UTC):
  - ACC28-DIRECT-DELETE: `root:a1` Held 03:58:25; delete 03:58:37.628; Terminating with the finalizer
    03:58:37.738, Releasing 03:58:37.763, Released 03:58:38.000, object gone 03:58:38.036; exactly one
    vm.delete record; ComputeVM `fc-cz5yawblk7nf-d871c971` gone 03:58:39.801 with its unit and work dir;
    the Bid has `status.termination` (03:58:38Z) and Complete=True ComputeVMGone.
  - ACC28-DELETE-BEFORE-ACCEPT: `root:a5b` (denyBidderDids names the one bidder) "collecting bids"
    04:02:53; delete 04:02:59.813, gone 04:02:59.950; the repo holds one rfp, 0 accept and 0 vm.delete
    records; no ComputeVM of that requester appeared.
  - ACC28-DELETE-WHILE-STOPPED: `root:a2` Held with its core stopped (spec.apps[].stopped); delete
    03:59:35, Terminating with the finalizer for 61 s while the guest ran; requester started 04:00:20;
    `requester_release` 04:00:36.244, Released 04:00:36.324, object gone 04:00:36.336, one vm.delete
    record, ComputeVM gone 04:00:37.647.
  - ACC28-ROOT-DELETE: `root:r1` with `contract` and `contract2` Held on bidder-1; root (uid 5265cb66)
    deleted 04:04:49.441; Deleting "2 ComputeContract(s) releasing" 04:04:52.028, "1 ..." 04:04:52.065,
    both Released by 04:04:52.041, "8 component(s) stopping" 04:04:52.466; both FirecrackerVM backends
    released (04:04:52.344, 04:04:52.442) before the Requester was deleted (04:04:52.785); the ComputeVM
    objects went 04:04:53.477 and 04:04:53.773; the root was gone 04:04:54.368; two vm.delete records.
  - ACC28-BOUND: bidder-1 stopped 04:06:30; `root:b1` deadline 04:09:49 by spec.ttlSecondsAfterFinished;
    delete 04:06:46; attempts 1 to 6 each HTTP 404 (`requester_release_retry`), attempt 4 after a
    requester restart; Failed "vm.delete was not acknowledged by 2026-10-24T04:09:49Z (the contract TTL):
    HTTP 404" 04:09:49.047, object gone 04:09:49.054.
  - ACC28-FORCED-REMOVAL: `root:f1` with bidder-1 stopped; delete 04:06:50.218 and a merge patch of
    `metadata.finalizers: null`; one `requester_release_retry` 04:06:50.324, then
    `requester_contract_gone_unreleased` (phase Releasing, its receipt) once at 04:06:50.353 and no
    further line for the contract until its core stopped at 04:07:05 (the 5 s retry did not run).
  - M4-GATEWAY-ORDER, before the Cluster teardown (network p28g, main 93f4de91): with the tenant
    cluster "tenant" holding one Running node and a running pod, the Gateway root deleted at
    04:24:44.665 stayed Deleting "1 ComputeContract(s) still hold a guest" for 3 min 15 s (the wait had
    no bound of its own); it went on only after the pod delete and the scale-in deleted the NodeClaim
    (04:30:00.424): Released 04:30:00.654, Requester deleted 04:30:01.728, root gone 04:30:03.193.
  - M4-GATEWAY-ORDER, after (network p28h): the same tenant with a Running node and a running pod; the
    Gateway root deleted at 04:52:59.216; Deleting "1 Cluster(s) tearing down" 04:53:02.353, the Cluster
    Draining 04:53:02.348; NodeClaim deleted 04:53:12.526, `requester_release` 04:53:12.556,
    `requester_released` 04:53:12.742 (one attempt), contract Released 04:53:12.767 and gone
    04:53:13.006; FirecrackerVM backend released 04:53:12.947; Cluster gone 04:53:15.421; Deleting
    "2 AtprotoRecord(s) ..." 04:53:15.522, "13 component(s) stopping" 04:53:15.857, Requester deleted
    04:53:16.146, root gone 04:53:16.894 (17.7 s in all). The ComputeVM went at 04:54:44.847 after the
    k3s guest's graceful stop; no guest, VM unit or account workspace was left.
  Open: `run request --exec` ends its ssh session and stops the requester's core as soon as the contract
  leaves Held, so a contract deleted under a running `run request` is released only when the invocation
  runs again; a root waits for the bidder's release answer, not for the guest's stop, so a ComputeVM can
  outlive the root by its graceful stop.

- **One live atproto session per account, owned by the MarketIdentity controller (plan 0030,
  2026-10-24).** The bobvmbuilder oauthQr bidder on hat-7 lost its OAuth session at
  2026-10-24T03:19:38Z (invalid_grant "Invalid refresh token"). The session secret at the
  MarketIdentity `spec.sessionRef` had five readers that each built their own live
  `oauthclient.Session` and could refresh and save it: the MarketIdentity controller (a new Open on
  every reconcile), the RemoteRepo credentials provider (a second Open in the same core), the Bidder
  (`spec.pdsAccessToken`), the RemotePDS (compose copied the ref into `spec.auth`) and the Requester
  (`spec.pdsAccessToken`, or `oauth-session.json`). The refresh token rotates on every refresh, so a
  copy holding a spent token that saved its data put the spent token back. Now
  `internal/marketidentity/atproto.Accounts` holds one `internal/pds/identity.Authenticator` per
  MarketIdentity; a reconcile, a sign-in and every bus resolver use that one session, and only it
  refreshes (`oauth_session_refreshed`, did and session id, no token). The controller provides
  `pds.Authenticator` (`pds.session`) and `pds.ServiceAuth` (`pds.serviceauth`) on MarketIdentity;
  the RemoteRepo signs its PDS requests through the first, the Bidder and the Requester mint service
  auth through the second (`participant.PDSServiceAuth`), each admitted by a ComponentLink its root
  composes. `pdsAccessToken` left the Bidder and Requester specs, RemotePDS `spec.auth` holds no
  identity session, and the per-process reload of e671cd9e went with the copies. The stored format did
  not change. The gateway web app session was not a copy: openbao-plugin-auth-atproto holds it and is
  its only refresher. Three defects of the market network surfaced in the live run and were fixed: the
  ingress tunnel dropped every response header but Content-Type (an OAuth token request never saw its
  DPoP nonce), the in-tree resource server checked DPoP proofs only against its issuer URL (a client on
  the PDS's loopback listener always got invalid_dpop_proof), and the MarketIdentity controller used a
  bare HTTP client that could not reach an authorization server behind the network ingress.
  Readings (market network oauth30, `--bidders 0`, bob's PDS `spec.oauth.accessTokenTTLSeconds` 60, a
  bidder outside the network by `run bidder --market-network-file` in mode oauthBrowser for an account
  on bob's PDS, signed in by a scripted POST to `/oauth/authorize` with an atproto localhost client_id
  because the PDS cannot fetch metadata behind the network ingress, UTC 2026-10-24): signed in 04:59:15;
  `oauth_session_refreshed` every 30 s from 04:59:45 to 05:19:15 (40 lines, one session id), one line per rotation, every line from the
  marketidentity-controller, no invalid_grant or SessionExpired; the offering was written at 04:59:18;
  `run request --network oauth30 --hold` got the oauth bidder's bid and accept at 05:07:54, the receipt
  at 05:08:11 and Held at 05:08:13; `run request release` reached Released at 05:08:51 and the bidder
  deleted the guest the same second. An earlier run logged 26 refreshes, two of its bidder core restarts
  reopening the one stored session, with no invalid_grant. The Bidder and Requester specs
  carry no `pdsAccessToken`, the composed RemotePDS has an empty `spec.auth`, and the only ComponentLinks
  to the MarketIdentity are the Bidder's `pds.serviceauth` and the RemoteRepo's `pds.session`. The
  bidder's OpenBao has no audit device, so who reads the path is shown by code, not by an audit log.

- **The guest image is published once, under the key of its pins, on 2026-10-23.** Every tag and
  `main` push rebuilt `firecracker-x86_64` from source (about 15 minutes of the `guest-image` job) and
  uploaded the same 1.5 GiB rootfs again under `releases/<version>` or `main/<commit>`, although the
  build is reproducible and the pins rarely move. The image now lives at
  `guest-images/<target>/<sha256 of the pinned image.json>/`; `image.json` names the sha256 of the
  kernel and the rootfs, so its own sha256 names the whole image. The `guest-image` job reads that key
  without credentials: present, it fetches the three files, checks each against its pin and checks the
  input sha256s the published `image.json` records (kernel source, base, kernel config, builder
  Dockerfile, `deploy/guest-image/rootfs.json`) against the tree, and neither
  builds nor hands publish an artifact; absent, or when a fetch or a hash fails, it builds as before and
  publish uploads to the key with `publish-spaces.ts --content-addressed`, which writes only what the key
  does not already hold. `guestimagerelease.Source` tries the key first and keeps `releases/<version>`,
  `main/<commit>`, `main/latest` and the newest release after it, so a binary published before the key
  and every image already under those prefixes still resolve; no consumer was found that needs a new
  per-release copy. Reading taken before the first CI run: the key answered 404, the three files under
  `main/fd1a644/guest-image/firecracker-x86_64` hashed to their pins, and a build of this commit resolved
  its image from a local server holding only the key, then from the public bucket through `main/latest`.
- **Gaps of the removed TypeScript runtime, closed by plan 0026 removing it (moved from section 14,
  2026-10-23).** Each was open in section 14 because of a mechanism that plan 0026 deleted: the TypeScript
  services ran as pods of a deno runtime in the core, each pod in its own run directory and, on an
  enforcing host, its own cgroup. Since plan 0026 the components run in the core's process, in the
  component runtime of `internal/component`, and host processes are HostProcess units. No Go code at
  HEAD names the run directories, the TypeScript tree variable or the restart annotation.
  - An upgrade did not restart the pods that ran the old assets (2026-10-19: after rc19 the gateway's
    requester still logged the rc17 `bidder_discovery` shape, because a core restart adopted every running
    pod and an upgrade changed neither the spec generation nor the restart annotation). The fix recorded
    the TypeScript tree in each run and replaced a run whose tree differed; its prod reading was pending.
    Now a component is a goroutine of the core, so the core that an upgrade restarts runs every component
    from the new binary.
  - A pod's spec change (its args or memory) did not restart it; only the restart annotation did. Now
    `Runtime` in `internal/component/runtime.go` stops and starts a component when its spec generation
    changes, and `dev restart` writes `spec.restartedAt` (`internal/stack/componentcore/source.go`,
    `internal/app/compose/apply.go`), which is a spec change and so a new generation.
  - The cgroup of an exited pod was left behind (62 empty `pod-<run id>` cgroups after the 60-restart
    breach; only a core start cleared them). Components get no cgroup of their own, so there is none to
    leave. The core's cleanup of the pod cgroups at exit and its `supervisor` leaf went with them.
  - The per-pod memory limit (plan 0013 M3-DENOPOD-LIMITS). Before: a pod had `spec.resources.memory`, ran
    with the V8 heap flag at 80% of it and, on systemd-user, in its own cgroup with `memory.max`; a breach
    restarted that pod alone and was counted in `status.restartReasons` (read on hat-8, 2026-10-15: a 128 Mi
    pod `hog` restarted 6 times in 40 s with `HeapLimitExceeded: 6` while its neighbour kept its run id and
    the core stayed up; on a real user unit the pod that allocated off-heap buffers was killed alone with
    `OOMKilled`). The mechanism is gone with the pods. The HostProcess unit limits stay, and the missing
    per-component limit is open again in section 14 in its current form.
  - Disk: the run directories under the state dir (removed with their object, swept after an hour,
    stdout and stderr rotated over 16 MiB), the per-closure `deno/` (6 GB on prod) and `ts/` (4.2 GB) trees
    that old builds seeded, and 26,645 exited run dirs on prod. Nothing at HEAD writes them. The one deno
    left is the policy engine's, which runs the policy actions: `internal/policyengine/deno` extracts its
    embedded binary once per deno version into `policy_engine/deno-embedded/<version>` under the user cache
    dir.
  - The relay backlog fell to 10000 frames and the relay pod got 1Gi; the relay RSS read 253 to 316 MB (414
    at most, +15 MB an hour), flat by the fit rule. That was a reading of the relay pod. The Relay is now a
    component in its core and has not been re-read.

- **Items closed by later work (moved from section 14, 2026-10-23).** Each was still listed as open in
  section 14 after the work that closed it landed.
  - `scripts/validate-candidate.sh` could not pass. Before: lane b waited for the Go integration suite
    that plan 0026 deleted, and lane c upgraded from v0.0.0-rc15 across a change of shape and asserted a
    `runs other assets` log line that no code prints any more. Nothing called the script. Now it is
    deleted (2026-10-24, user decision): a candidate is validated by the CI run on its tag and by the
    acceptance runbook `docs/acceptance/0001-auth-modes.md` on prod and every bidder host.
  - PDS ingress per app (plan 0022). Before: the API carried `spec.ingress` and `Pds.spec.requireIngress`,
    but no app root created the IngressProxy and nothing gated account creation on a Ready ingress, so an
    app PDS could have a loopback service endpoint. Now plan 0022 is done (W1 and W2 items done, closed by
    1ab24182 on 2026-10-22 with live PDS proofs on prod and every bidder host): the app roots compose the
    `ingress` IngressProxy (`compose.AppIngressName`), the PDS spec takes its public URL from it
    (`compose.AppPdsSpec`), and the Gateway root keeps the PDS waiting while `requireIngress` is set and no
    public URL is known (`internal/app/gateway/gateway.go`), with reason IngressProviderNotReady while the
    IngressProxy is not Ready (`internal/app/compose/pds.go`).
  - A retired kcp slot's audit logs were never removed (prod's state dir held 62 GB, 46 GB of it
    `kcp/slot-a/audit-*.log` of the slot that rc14's roll retired). Fixed 2026-10-18 by 1810e4a3: a roll
    removes the retired slot's `audit-*.log` files (`internal/bootstrap/rollinstalled.go`, log line "removed
    the audit logs of the retired kcp slot").
  - kcpapply refused an object that `kubectl annotate` had touched, which was how an operator restarted one
    pod: prod's gateway apply failed ("the object was applied by kubectl and is never adopted") and the web
    app went 502 until the managed-fields entries were cleared by hand. Fixed by 8623a675 (2026-10-18):
    `refuseKubectl` in `internal/controllers/runtime/kcpapply` tolerates a kubectl manager that owns only
    metadata (`annotationsOnly`) and still refuses one that owns spec or status. A component restart is now
    `spec.restartedAt`, written by `dev restart`, not an annotation.
  - The object-count quota defaults were retuned by the soak (plan 0013 M2-QUOTAS, 2026-10-18): Bids 10000
    (was 20000), because 10000 Bids put the bidder kcp at about 2.1 GiB of its 3 GiB limit (0.14 to 0.15 MB of
    RSS per Bid on a base of 610 to 680 MB). At HEAD `compose.QuotaDefaults` (`internal/app/compose/infra.go`)
    is Bids 10000 and ComputeVMs 64 on a MarketBidder, ComputeContracts 1024 on a Gateway and on a
    MarketRequester, PolicyWorkflowRuns 500 per application workspace, and Components 16 per component kind;
    the DenoRuns quota went with the DenoRun kind, and `NodeClaims` 64 is declared but no quota entry uses
    it. The root's `spec.quota` overrides each (`-1` removes the limit).
  - The web app's node claim index kept one ConfigMap per home ever created (201 at 22:33Z on 2026-10-18,
    +55 an hour in the network kine). Closed by 6768644f (2026-10-18): a deleted cluster's index ConfigMap
    is removed and a sweep removes entries whose cluster is gone. Not re-read.
  - An installed bidder took no bid TTL from its config.json (the second soak bidder ran with the 86400 s
    default until a unit drop-in passed one). Now the TTL is `Bidder.spec.bidTtlSeconds` (default 86400),
    and a MarketBidder root sets it through `spec.components`, key `Bidder`, `settings.bidder.bidTtlSeconds`
    (plan 0024 W0-W3, below), so no flag or config.json field is needed.
  - The requester retried the release with a new event (plan 0020). Before: one non-2xx answer to the
    direct submitEvent of a vm.delete (a 404 from the did-key-ingress-proxy while the winning bidder was
    away, job 618) made the runner write releaseFailed and stop, so the guest outlived its contract, and
    every release wrote a new vm.delete and a new market.event record. Now the release loop of
    `contractRunner` (`internal/market/requester/runner.go`) records the event once and resends the same
    uri and cid until a 2xx, writing Releasing with the attempt and the last answer, up to the contract's
    deadline. ACC20-BIDDER-BACK and ACC20-ONE-EVENT were marked done on 2026-10-17 (1526b2e9) and the plan
    closed (752df79d); their readings were not written here.
  - The node half inside the control-plane container (a STATED LIMIT: a containerized subnet allocator
    sees only the container's networks and would lease a range that collides with the host's), and two
    concurrent proofs in two containers taking the same ports (`liveports.LockDir()` is per container).
    Both bounded a container that no longer exists: 0006 M5-RETIRE deleted the container path and
    `deploy/control-plane-container/` (cd68b0ae, 2026-10-13). The allocator's one reach site is still the
    materialiser's teardown release (`OpReleaseSubnet`), and `subnetallocator.Config.HostNetworks` is still
    injectable. `internal/liveports` has no importer at HEAD, since plan 0026 deleted the Go tests.

- **A revoked identity's ServiceAccount is erased from the datastore before k3s starts (plan 0026
  A9, opened and closed 2026-10-23).** Before: revokeIdentity deleted the did- ServiceAccount at once
  in a running cluster, and a stopped cluster recorded ColdDeferred; the cluster provider deleted the
  account at wake through the tenant apiserver, so from the apiserver's first answer to that delete
  (about 5 s in the live run, 20:16:50Z to 20:16:56Z) the stale token got 200 directly at the
  loopback apiserver. Now the k3s step's Prepare, after kine is ready and before the k3s unit is
  placed, reads the owner's identity state and, when it is revoked, deletes
  `/registry/serviceaccounts/<ns>/<name>` and every `/registry/secrets/<ns>/<name>-token-*` key over
  kine's etcd v3 API (`internal/tenantstore/kineetcd` behind `internal/contract/tenantstore`, wired
  in `internal/providermain` as `StackConfig.Revoked`), each with a mod-revision compare. A failed
  read or delete fails the step, so bring-up retries on the runtime's backoff and k3s never starts
  with the account present. The at-wake API delete and RevocationSettled stay the confirmation: the
  provider then reads the account absent (reason ServiceAccountAbsent) and the atprotoidentity
  controller moves the identity to Revoked. A cluster that was running at revoke time and restarts
  later runs the same erase, which finds nothing. After, read live on this host (market network
  `fixwindow`, one gateway, two non-exempt accounts; probes every 150 ms with the stale token at the
  front's advertise port, the k3s slot port and the kube-apiserver port): cluster b stopped, revoked
  (ColdDeferred), woken by `spec.running`; kine ready 22:21:09.045Z, the erase 22:21:09.096Z (8.6 ms,
  deleted true), k3s placed 22:21:09.098Z, first answer 22:21:09.253Z (503), 401 from 22:21:09.972Z,
  RevocationSettled ServiceAccountAbsent and identity Revoked 22:21:16Z, Ready 22:21:17.6Z; 2538
  probes from 22:21:02Z to the teardown at 22:23:17Z, no 200 (connection refused, a failed TLS
  handshake at the front, 503 from the k3s supervisor, then 401). Cluster a, revoked while running
  and restarted: erase deleted false, 401 from the first answer, no 200 in 1055 probes.
  The non-revoked account's cluster k woke in the same minute with no erase, its ServiceAccount the
  same uid, and its token 200 from the first apiserver answer (22:21:30.6Z).

- **Per-component suspend and typed overrides on the app roots (plan 0024 W0-W3, 2026-10-22).** The
  five app roots and `NetworkApp` accept `spec.components`, keyed by `Kind` for a singleton or
  `Kind/name`. appctl resolves every entry against the root's desired component set before it writes
  a child, and fails the pass closed on an unknown or ambiguous key, a settings block for another
  kind, an unsupported field or a negative value. It writes the child spec with
  `root.spec.suspend OR override.suspend`, so root suspend stays the master kill switch. Typed
  settings reach the Bidder heartbeat, offering refresh, bid TTL and policy poll; the Requester
  concurrency, contract/bid/receipt timings, sessions, directory cache and OAuth scopes; the
  GuestControlPlane login mount, role, grant/issuer timings, host-key attempts, relay toggle and
  HostKeyRef; the PDS blob/request limits, contact email and OAuth TTLs/scopes; the SshServer limits;
  the GatewayAPI node poll; the IngressProxy timeouts; the Relay crawl interval; and the PolicyEngine
  limits, trust and cache. Root status carries `componentSummary`, `componentOverrides`, and the
  `ComponentsReady`, `ComponentSuspended`, `ComponentOverridesValid`, `ComponentDrift` and
  `LimitsEnforced` conditions. A per-component suspend does not propagate to that component's
  Service or Exposure or to the shared IngressProxy. `ComponentLimits` resolves and reports but the
  in-process component runtime does not enforce it; the `LimitsEnforced=False` condition says so.
  Live proof on this host, hat-7, hat-9 and the prod gateway is W4 and has not run yet.

Not gaps. Each entry was open once, and each says what settled it and what the settlement was - and,
where it matters, that it was settled AFTER the entry was written. That timing is kept because it is the
case nobody re-reads: a gap still listed as open reads as honest incompleteness, which is why a bullet
can outlive the work that closed it, and why one whose subject has been deleted can sit in a list of open
questions for hours without anyone stopping on it. The measurements stay with the entries rather than
being tidied away, because a number without its condition is how the 332-second boot outlived its own
refutation.

- **The pds sockets, the gateway's orphan MarketIdentity and the uncounted OOM kills of the second soak (2026-10-19, read on
  dev-sock-1, c-32, a one-kcp market network with the gateway under `soak load` at 1.1 RFP/s, 3 VM lanes and 4 tenant lanes, plus
  a bidder made with `install bidder --allow-dev-binary` and pointed at the network).** Three items of section 14 closed:
  - alice-pds and bob-pds did not leak; `soak sample` did (099ec6c5). `ss -tanp` per pid each minute, with the peer of each socket
    resolved to its process: with no sampler both held 16 to 20 ESTAB sockets (the cores' clients, the relay, the dispatcher); with
    the old `soak sample --every 1m` the sampler's own process held 1 more inbound keep-alive per PDS each minute (10 after 10
    minutes, 00:10Z to 00:20Z: alice 6 to 15, bob 18 to 28). `pdsSample` built a new `http.Transport` per sample and never closed
    it, and a Deno.serve listener keeps an idle keep-alive connection while its client does. With `CloseIdleConnections` the
    series stayed flat for 63 minutes under the load (00:21Z to 01:23Z: alice 4 to 8, mean 5.6; bob 16 to 19, mean 17.8). The
    second soak's +59 an hour is that sampler; the steps down at its core kills were the cores' own pooled connections.
  - The orphan `root:gateway:app/requester` was a report without an owner link, not a leak (55682b85). The Gateway controller
    wrote that MarketIdentity into `root:app` with no label, and `adoptChildren` adopts only in the root's own workspace; the
    object lives as long as the `app` workspace, which carries the Gateway's owner reference. Every MarketIdentity that appctl
    writes now carries `apps.socialweb.computer/root`. Removing the label by hand brought back the soak's reading (orphanTotal 1,
    example `root:gateway:app/requester`); a gateway core restart wrote the label again and the next scan read 0. The Gateway's
    status read orphanTotal 0 in all 62 per-minute samples of the churn run (80 tenant cluster cycles, 121 VM contracts).
    ACC13-NO-ORPHANS stands. bidder-1's scan caught a ComputeVM between its create and its adoption 9 times (a new name each
    time, none left at the end).
  - An OOM kill of an adopted run now counts (a0e475c2). The pods of an installed core live in its runs unit and outlive a core
    restart; the new core adopts each run without a wait goroutine, so no done.json was written and the kill counted as
    `Restarting`. In the second soak the installed pds run of 19:11:45Z, adopted at the 19:27Z core restart, died at 20:22Z
    (`Restarting`), the run of 20:22Z, adopted at the 21:22Z kill, died at 21:43Z (`Restarting`), and only the run of 21:43Z died
    in the core that started it (`OOMKilled`). Observe now reads `oom_kill` and `memory.max` of an adopted run's pod cgroup and
    writes done.json, and the settled-cgroup sweep keeps the cgroup of a run that a DenoPod still names. Six forced kills
    (`memory.max` of the run's pod cgroup set to 8M) on the installed bidder, four of them after a `kill -9` of its core: the
    kernel logged 6 `oom-kill` events in `pod-pod-*` cgroups, and `restartReasons` read pds `OOMKilled: 4`, guest-kcp
    `OOMKilled: 1`, policy-engine `OOMKilled: 1`.

- **The contract watch's getRecord bursts: the gateway requester's native RSS and the pds DenoPod's growth (plan 0013
  M4-SOAK-READING, 2026-10-18, commits fdeb7697, b97422d3, 9b600a20).** Before: the gateway requester DenoPod's RSS rose 227 to
  550 MB in 90 minutes with its heap flat at 45 MB, and in the second soak an installed bidder's pds DenoPod (512Mi) was
  OOM-killed at 496 to 499 MB anon 81 to 85 minutes into each run (the next entry). Cause: `openContractWatch` opens a dedicated relay watch per contract
  and calls getRecord on every bid, market.event and receipt the watch sees, before it can tell whether the record is its own.
  A dedicated watch started 1000 events back from the stored cursor (`resumeCursor`'s overlap), and a requester with no stored
  cursor (the gateway's) started with no cursor, which this relay answers with its whole backlog (10000 frames). So each new
  contract replayed hundreds of records and fetched each one at once: 830 concurrent getRecord calls per contract on the soak
  requester and 300 to 700 on the gateway requester (a fetch wrapper's in-flight peak, every 10 s). Each call took its own
  keep-alive connection to the network dispatcher (`ss`: 850 to 900 ESTABLISHED, idle about 60 s, reaped after 90 s, a sawtooth).
  The bidder's pds DenoPod served all of them through its ingress. The heap stayed small and the RSS was native: the requester's
  connection buffers and the glibc main arena (`[heap]` in smaps, which a burst raises and never returns). The change: a
  dedicated watch starts at the newest cursor that any watch of this process has seen, or the stored cursor with no overlap, and
  the shared stream, once it is set up again after a teardown, starts there too. The 1000-event overlap stays for the first
  connect after a restart. After the change the fetch peak is 4 to 9 in flight, apart from one burst at the start of each process
  (the first replay). hono-repo-pds logs `pds_memory` every minute (`--memory-log-ms`). Reading on droplet gl-runner-1 (48 vCPU,
  94 GB), account test-0001: one `dev up --profile market-network --bidders 1 --with-gateway --vm-host-backend systemd-user`,
  the app values of dev-bound-1 (bid TTL 1800 s, gateway retention 900 s), `test/soak load` at 1.1 RFP/s, a VM contract every
  40 s (3 lanes), 2 tenant lanes, contract TTL 40 minutes. Load from 20:59Z to 23:22Z: 9371 RFPs, 211 VM contracts held and
  released, 74 tenant cluster cycles, 0 failed. /proc was sampled every minute. Before is 21:00Z to 21:37Z (the pds to 21:10Z,
  before a heap snapshot raised its RSS by 140 MB). After is 22:20Z to 23:22Z (the requesters were restarted onto the change at
  22:14Z and the pds at 21:41Z). The fit is the section 15 soak rule:

| series | before (start, end, slope per hour) | after (start, end, max, slope per hour) | after plateau |
| --- | --- | --- | --- |
| gateway requester RSS (MB) | 147, 168, +29.8 | 146, 154, 155, +6.4 | yes |
| gateway requester heapUsed / external (MB) | 38 to 46 / 6.8 | 42 to 43 / 6.8, flat | yes |
| gateway requester fds | 42, 337, +334 | 45, 42, 48, -1.2 | yes |
| soak requester fds | 185, 893, +850 | 41, 43, 49, +0.1 | yes |
| bidder pds RSS (MB) | 172, 227 in 11 minutes, +322 | 218, 252, 252, +18.8 | yes |
| bidder pds heapUsed (MB) | 36 to 60 | 46 to 76, +2.3 | yes |
| bidder pds external (MB) | 6.5 to 8.7 | 19, 38, 38, +18.2 | NO (the firehose backlog filling, below) |
| bidder pds fds | 30 to 33 | 30, 30, 34, +0.5 | yes |
| bidder pds RSS, 00:15Z to 01:16Z (MB) | | 227, 273, 275, +11.3 | yes |
| bidder pds external / brk heap, 00:15Z to 01:16Z (MB) | | 46.5 to 46.9, +0.3 / 79 to 81, +1.0 | yes |
| gateway requester RSS, 00:15Z to 01:16Z (MB) | | 146, 155, 155, +5.2 | yes |

  The pds external rise is the hono-pds `FirehoseSequencer` backlog: a heap snapshot shows each retained Uint8Array as a
  backlog frame's `blocks`. The backlog is bounded at 10000 frames or 64 MiB of blocks, about 5 KB a frame here, so it fills in
  about 2.5 hours at 1.1 bids a second and stopped at about 47 MB (the 00:15Z to 01:16Z rows, load running to 01:16Z: 16895
  RFPs, 382 VM contracts released, 132 tenant cycles). Heap snapshots (`node:v8` `writeHeapSnapshot`) raise the pod's
  RSS by about 140 MB that it keeps (glibc), so a reading that takes one must not be fitted across it.

- **The four disk and record series of the second soak plateau (plan 0013 M3-DISK-RETENTION, M3-PDS-RECORD-RETENTION, 2026-10-19).**
  The second soak (below) left four series growing; each had one cause:
  - DENO_DIR `gen/blob` (+23.6 MB an hour): the bidder's scope gate (`--policy open`) runs the gha-lite workflow, whose node
    action (`dist/index.js`, 300 KB) ran from a `text/typescript` blob URL with a new uuid. Deno transpiled it and cached a 1 MB
    emit under `gen/blob` per run; the scope cache's 5 minute positive TTL times two requesters is 24 runs an hour.
    `runActionInWorker` now makes a `text/javascript` blob unless the action main ends in `.ts`, so a JavaScript action
    emits nothing (6daeb0f4).
  - One `pg-<cluster>.log` per cluster ever made: nothing removed it. A Postgres deleted with `spec.deleteData` (every tenant
    cluster's) now removes `pg-<name>.log` and its rotated `.1` with its data directory (33d370c6).
  - The repo kv files (+1.2 to +4.0 MB an hour): deno kv makes its sqlite file with `auto_vacuum` none, so the file keeps its
    high-water size. Every core now runs `storemaint.KeepDenoKv` over its `components` dir and the network state dir: a deno kv
    database (sqlite magic plus the `kv` and `data_version` tables) is converted once (`PRAGMA auto_vacuum=INCREMENTAL`,
    `VACUUM`), then `incremental_vacuum` and a `wal_checkpoint(TRUNCATE)` run every 5 minutes (9db73adb, eb5a164c). It runs in
    the core process, never in the deno process, because two sqlite libraries in one process break each other's posix locks.
    A conversion under a deno kv writer at about 14000 writes a second gave no writer error and a clean `integrity_check`.
  - com.fedproxy.rbac records of deleted clusters: only `releaseCompute` deleted them, and only for the capabilities it was given;
    the orphan scan's unclaimed release passes none, and a flow that throws after the accept or a contract deleted while its
    requester is down never releases. The requester's orphan scan now lists the rbac records too and removes those of a VM that
    is settled with no contract, and those whose VM no compute.vm record and no ComputeContract names (b1299ba9).
  Reading: droplet dev-disk-1 (c-32, sfo2), `dev up --profile market-network --bidders 1 --with-gateway --vm-host-backend
  systemd-user`, app values `bid-ttl-sec` 1800 and `record-retention-sec` 900, the `test/soak` load of the second soak
  (1.1 RFP/s, a VM contract every 40 s on 3 lanes held 20 s, contract TTL 40 minutes, retention 900 s, 4 tenant lanes) from
  00:00Z, chaos every 7 minutes (`dev restart` over six DenoPods, `kill -9` of the gateway core every 42 minutes), `du` and
  sqlite pragmas every 5 minutes, fitted 01:05Z to 02:11Z (14 samples) with the second soak's fit rule. In the window: 4356 RFPs,
  99 VM contracts, 58 tenant cycles (118 in all, none failed).

| series (window 01:05Z to 02:11Z) | start | end | max | slope per hour | plateau |
| --- | --- | --- | --- | --- | --- |
| network DENO_DIR `gen` (MB); `gen/blob` files | 9.72; 0 | 9.92; 0 | 9.92; 0 | +0.20; 0 | yes |
| gateway `pg-*.log` files (116 removed with their clusters) | 5 | 5 | 5 | -0.1 | yes |
| soak requester repo kv + wal (MB) / rows | 62.4 / 59528 | 67.7 / 70444 | 80.3 / 113080 | +3.0 / +6519 | yes; the file follows the rows |
| bidder-1 repo kv + wal (MB) / rows | 40.1 / 44094 | 48.4 / 52674 | 59.2 / 67100 | +2.0 / +1465 | yes |
| gateway repo kv + wal (MB) | 4.3 | 1.6 | 6.4 | +1.5 | yes |
| repo kv free pages after a pass | 0 | 0 to 4787 | 4787 | | freed every 5 minutes |
| gateway repo com.fedproxy.rbac | 2 | 4 | 5 | +0.8 | yes: the live clusters; the scan removed 6 left by chaos |

  The bidder-1 repo file fell from 55.0 to 34.3 MB when its Bid records passed their TTL (00:50Z to 00:55Z), which the old file
  never did. The logs dirs still grow below their 64 MiB rotation caps; the network PDS and relay kv files were flat before.

- **Plan 0013 M4-SOAK-READING: second 3 h soak, systemd-user (2026-10-18, fitted 20:35Z to 23:36Z; ACC13-NO-HOST-OOM holds, M3-DISK-RETENTION,
  M3-PDS-RECORD-RETENTION, M4-SOAK-READING and ACC13-BOUNDED do not).** Host: droplet dev-soak-2 (c-48, 48 vCPU, 94 GB, Ubuntu 24.04,
  sfo2), a dev build of v0.0.0-rc17 (fdf71e31). Two backends ran side by side, and every row below says which:
  - `[proc]`: the market network `soak2`, `dev up --profile market-network --bidders 1 --with-gateway --vm-host-backend
    systemd-user` as test-0001, app values `bid-ttl-sec` 1800 on the bidder and `record-retention-sec` 900 on the gateway. Its
    cores, kcp, kine, OpenBao, postgres and DenoPods run under devsup with no cgroup limit (`dev up` cannot place them under
    systemd-user, section 14); only bidder-1's VM units are user units.
  - `[sd]`: a second bidder `bidsd.bob.test`, made with `install bidder --allow-dev-binary` for the system account swc-bidder and
    pointed at the network (config.json `profile` market-network and `market-network-file` the network's MarketNetwork status;
    a unit drop-in adds `-- --bid-ttl-sec 1800`). It is the prod shape: the `socialweb-computer-bidder@` unit (Delegate=yes,
    OOMPolicy=continue), kcp, kine, the front and OpenBao as user units with MemoryMax, each DenoPod in its own `pod-<run id>`
    cgroup with `memory.max`, VM units in its user manager. It won 29 of the 60 VM contracts alive at 22:20Z (bidder-1 31).
  Load (the `test/soak` driver as user units, 18:55Z to 23:36Z): 1.1 RFP/s, a VM contract every 40 s on 3 lanes held 20 s,
  contract TTL 40 minutes, requester retention 900 s scanned every 60 s, 4 tenant lanes (create a cluster, a pod scales a node out
  through the RFP flow and in, delete the cluster). OpenBao tidy at 15 minutes on every server (`soak tidy` every 10 minutes for
  the network's, a patch for the installed bidder's); the other sweeps at their defaults, which are the first soak's (bidder
  record sweep 10 minutes, storemaint 5 minutes, run dirs every minute with a 1 h keep, guest images every 10 minutes). In the
  window: 11948 RFPs, 263 VM contracts (247 released, 17 failed `no_bids` around chaos), 165 tenant cycles (4 cut by the driver's
  stop, none failed). Chaos (a root unit): a `dev restart` every 7 minutes over seven network DenoPods (25 in the window, all
  rc 0), `kill -9` of the bidder DenoPods of both bidders every 42 minutes (4 each), `kill -9` of a core every 42 minutes
  (bidder-1's once, the gateway's twice, the installed core twice; systemd restarted the installed core). Sampled every minute:
  `soak sample`, `ss` and `ps` per pid, `smaps_rollup`, threads and fds of the requester and pds DenoPods, the installed
  bidder's unit and pod cgroups, objects and repo records; the state dir broken down by subdirectory every 10 minutes. Fit:
  least squares after warm-up (1 h after the installed bidder settled at 19:32Z), flat when |slope x window| <= max(2 x residual
  sd, 5 % of the mean).
  Memory breach (ACC13-NO-HOST-OOM), 19:18:58Z: the installed bidder's DenoPod `bidder` limit was set from 1536Mi to 96Mi with a
  `deno.computer/restart` (a spec change alone does not restart a pod). The pod was OOM-killed 60 times in 120 s
  (`status.restartReasons` `OOMKilled: 60`; kernel "Memory cgroup out of memory ... memory.oom.group set"), then the limit went
  back. 16 of the 18 swc-bidder processes kept their pids (the core, kcp, kine, the front, OpenBao and its plugin, the other three
  pods); only the bidder pod's two were replaced. MemAvailable read 82.9 GB before, 83.9 during and 84.8 after. Under load the
  installed pds DenoPod (512Mi) breached by itself three times (20:22Z, 21:43Z, 23:08Z) and was restarted alone each time. No
  global OOM in the run, swap 0, MemAvailable at least 76.5 GB, the host vmstat `oom_kill` counter counted only those pod cgroup
  kills (189 killed tasks, 3 per OOM kill of a pod). No quota was hit (0 refused creates); the M2-QUOTAS reading (a 10-Bid quota declined the 11th RFP with reason
  capacity) and the first soak's Bid quota hit stand for the quota clause.

| series (window 20:35Z to 23:36Z) | start | end | max | slope per hour | plateau |
| --- | --- | --- | --- | --- | --- |
| [proc] network kcp RSS (MB) | 1691 | 1732 | 1839 | +21.0 | yes |
| [proc] network kine RSS | 448 | 421 | 584 | -7.5 | yes |
| [proc] network core / OpenBao | 95 / 119 | 98 / 120 | 100 / 121 | +0.7 / +0.4 | yes |
| [proc] tenant clusters' postgres (under the network) | 2434 | 2872 | 2881 | -26.9 | yes |
| [proc] relay / dispatcher DenoPods | 253 / 636 | 316 / 646 | 414 / 804 | +15.1 / +16.6 | yes |
| [proc] alice-pds / bob-pds / PLC DenoPods | 139 / 137 / 140 | 146 / 143 / 140 | 147 / 144 / 147 | +2.4 / +2.2 / +0.1 | NO / yes / yes |
| [proc] alice-pds / bob-pds sockets | 127 / 118 | 293 / 294 | 293 / 295 | +58.6 / +59.1 | NO (section 14) |
| [proc] bidder-1 core | 139 | 75 | 224 | +2.7 | yes |
| [proc] bidder-1 bidder / guest-kcp / policy-engine DenoPods | 252 / 154 / 116 | 229 / 149 / 118 | 330 / 164 / 119 | -7.1 / +0.2 / -0.2 | yes |
| [proc] bidder-1 pds DenoPod | 325 | 273 | 338 | +1.7 | yes by the fit; +67 to +151 per run (section 14) |
| [proc] gateway core / web app / pds DenoPods | 94 / 234 / 199 | 97 / 243 / 193 | 102 / 244 / 207 | +0.5 / +6.4 / -0.1 | yes |
| [proc] gateway OpenBao (root and tenant vaults) | 704 | 706 | 712 | -8.8 | yes |
| [proc] gateway requester DenoPod | 417 | 679 | 978 | +62.4 | NO: +345 / +388 per run, heap flat (section 14) |
| [proc] soak requester DenoPod | 355 | 1552 | 2157 | +249 | NO: up to +2281 per run, heap 85 to 92 MB (section 14) |
| [sd] installed kcp / kine / core / front / OpenBao RSS | 1066 / 85 / 85 / 39 / 120 | 1011 / 84 / 132 / 39 / 125 | 1116 / 158 / 230 / 40 / 126 | +2.1 / +1.6 / +4.6 / +0.0 / +1.5 | yes |
| [sd] installed bidder / guest-kcp pod RSS | 232 / 158 | 303 / 159 | 327 / 161 | -1.5 / +0.8 | yes |
| [sd] installed pds pod RSS | 267 | 270 | 531 | -17.3 | NO: OOM at 512Mi every 81 to 85 minutes |
| [sd] installed kcp unit memory.current (with page cache) | 1225 | 1363 | 1500 | +65.2 | NO; its RSS is flat (row above) |
| [sd] empty pod cgroups | 4 | 3 | 5 | -0.0 | yes; 62 after the breach, cleared only by a core start (section 14) |
| [proc] network kine live keys | 6245 | 6344 | 6546 | +50.4 | yes |
| [proc] network kine.db (MB) | 105 | 98 | 142 | +1.6 | yes |
| [proc] Bids (bidder-1) / ComputeContracts (soak requester) | 2379 / 2701 | 2306 / 2702 | 2590 / 2940 | +1.6 / -3.7 | yes |
| [proc] ComputeVMs / HostProcesses / NodeClaims | 4 / 109 / 3 | 3 / 117 / 4 | 6 / 120 / 4 | -0.2 / -1.4 / -0.1 | yes |
| [proc] core/v1 objects (the node claim index ConfigMaps) | 291 | 454 | 454 | +54.7 | NO (fixed on main, 6768644f) |
| [sd] installed Bids / gc objects | 2381 / 2396 | 2303 / 2329 | 2612 / 2630 | -2.0 / -1.1 | yes |
| [sd] installed kine rows / file (MB) | 8001 / 60.4 | 6150 / 45.2 | 8087 / 61.4 | -128 / -1.3 | yes |
| [proc] OpenBao storage bidder-1 / gateway (MB) | 17.1 / 17.0 | 17.3 / 17.0 | 17.6 / 17.1 | +0.1 / -0.0 | yes |
| [sd] installed OpenBao storage (MB) | 16.6 | 17.3 | 17.5 | +0.1 | yes |
| [proc] state dir total (MB) | 12168 | 12115 | 17193 | -144 | yes |
| [proc] bidder-1 VM work dirs / gateway tenant cluster dirs | 7792 / 1318 | 7467 / 1392 | 12594 / 1393 | -227 / -10.2 | yes |
| [proc] network DENO_DIR (gen/blob) | 211 | 282 | 282 | +24.3 | NO (section 14) |
| [proc] network / gateway / bidder-1 logs dirs | 20.6 / 10.2 / 5.0 | 53.4 / 28.7 / 11.5 | 53.4 / 28.7 / 11.5 | +11.0 / +6.1 / +2.6 | NO: below the 64 MiB rotation caps; the gateway's keeps a pg log per cluster |
| [sd] installed stateDir (status) | 8237 | 5437 | 12440 | +34.3 | yes |
| [sd] installed kcp dir (audit logs) | 317 | 486 | 572 | +60.1 | bounded: reached maxbackup 4 x 100 MB, 572 then 486 |
| [proc] soak requester repo kv (MB) / compute.vm / market.rfp | 56.0 / 1164 / 1167 | 64.7 / 1033 / 1039 | 64.7 / 1280 / 1253 | +4.0 / -12.3 / -7.1 | kv NO; records yes |
| [proc] soak requester repo market.accept / market.event / vm.delete | 25 / 25 / 22 | 25 / 25 / 25 | 28 / 28 / 28 | -0.7 / -0.8 / +0.1 | yes |
| [proc] bidder-1 repo kv (MB) / market.bid / market.event / market.receipt | 39.5 / 2854 / 135 / 49 | 42.7 / 2513 / 99 / 35 | 42.7 / 3067 / 170 / 60 | +1.2 / -7.4 / -4.3 / -1.7 | kv NO; records yes |
| [sd] installed repo kv (MB) / market.bid / market.event / market.receipt | 35.4 / 2794 / 120 / 42 | 44.2 / 2766 / 136 / 47 | 44.2 / 3015 / 153 / 53 | +3.0 / +24.2 / +4.8 / +1.7 | kv NO; records yes |
| [proc] gateway repo kv (MB) / compute.vm / market.accept | 6.2 / 16 / 16 | 6.3 / 17 / 17 | 6.3 / 20 / 20 | +0.1 / -0.4 / -0.1 | yes |
| [proc] gateway repo com.fedproxy.rbac | 7 | 20 | 21 | +5.3 | NO: records of deleted clusters (section 14) |
| host MemAvailable (MB) | 80127 | 77722 | 84193 | -293 | yes |
| host load average 1 min / swap used | 16.6 / 0 | 15.8 / 0 | 25.2 / 0 | -0.1 / 0 | yes |
| host journal (MB) | 44.4 | 77.0 | 77.0 | +11.2 | NO; the host's SystemMaxUse pool bounds it |

  Root status after the driver stopped (23:36Z): bidder-1 2286 objects, the soak requester 2676, the installed bidder 2329, each
  with `orphanTotal` 0 and `stuckTotal` 0; the gateway 18 objects with one orphan (the MarketIdentity `root:gateway:app/requester`,
  section 14) and 0 stuck. The TTL controller had deleted 3773 objects in bidder-1, 9867 in the soak requester and 437 Bids in the
  installed bidder (whose older Bids were annotated to 1800 s at 19:32Z).
  Done_when read: ACC13-NO-HOST-OOM holds (the breach above, three more breaches under load, each restarting only its pod; the
  host never short of memory). ACC13-BOUNDED and M4-SOAK-READING do not hold: the requester DenoPods' native memory, the pds
  DenoPods' memory, the network PDS sockets, the node claim index ConfigMaps, the gateway repo's rbac records, the DENO_DIR
  `gen/blob` and the repo kv files keep growing (section 14). M3-DISK-RETENTION does not hold: the state dir totals are flat in
  both backends, but by subdirectory `gen/blob` (+23.6 MB an hour), the per-cluster pg logs (one per cycle) and the repo kv files
  (+1.2 to +4.0 MB an hour) keep growing, and the logs dirs have not reached their caps yet. M3-PDS-RECORD-RETENTION does not hold
  for the gateway repo (20 rbac records of deleted clusters after about 12000 RFP-only, 263 VM and 165 tenant node contracts); the
  soak requester's, bidder-1's and the installed bidder's repos hold only live and retained records.

- **The gateway kcp memory and the gateway OpenBao storage under tenant cluster churn (plan 0013 ACC13-BOUNDED, 2026-10-18).**
  Both were open in section 14 after M4-SOAK-READING (below): kcp 599 to 1134 MB in 3 h, OpenBao storage 17 KB per cluster cycle.
  Host: droplet dev-churn-1 (c-32, sfo2), account test-0001, `dev up --profile market-network --bidders 1 --with-gateway`, the soak
  driver's tenant lanes (6 lanes: create a cluster, Ready, a pod scales a node out through the RFP flow and in again, delete the
  cluster, wait until it is gone), samples every minute (`soak sample`, `ss` per pid, `/proc` RSS and fds per pid, kcp heap
  profiles from `/debug/pprof/heap`, the root vault listed with a root token generated from its unseal keys).
  Causes, each fixed on main:
  (1) The kcp memory was not the churn. kcp v0.33.0 (`pkg/server/localproxy.go`) proxies a request for a mounted workspace
  through a new `http.Transport` per request unless `--mount-proxy-client-cert-file` is set, and that transport never closes its
  keep-alive connection. The cluster provider's `MountStatus` probe (`GET /clusters/<mount>/version`, every pass, about 25 a
  minute per cluster) therefore left one kcp connection, and one pair on the cluster's apiserver front, per probe; a long-lived
  cluster (the soak's bootstrap cluster, prod's `canary`) grew without bound, and prod ran out of ephemeral ports (112939
  loopback connections). Every slot root now holds a self-signed client pair and `kcpserver` passes both mount proxy flags, so
  kcp uses one shared transport (90 s idle timeout); `CheckArgv` refuses an argv without them (c883e72d). Read side by side on
  the droplet with no churn and one tenant cluster: the main build's kcp went from 48 to 220 established connections in 9
  minutes, its fds from 386 to 687 sockets and its RSS from 720 to 738 MB in 10 minutes (108 MB an hour, the soak's slope), and the
  tenant front reached 1260 sockets in 20 minutes; the fixed kcp held 17 to 27.
  (2) The apiserver front closes a connection pair after 5 minutes with no bytes either way, so a client that leaks silent
  keep-alive connections cannot exhaust the host again (d116cb41; a client-go HTTP/2 connection pings every 30 s, so this does
  not cut those).
  (3) The gateway core minted a new tenant client certificate about once a second (the node observer and the node reconcile),
  and client-go keeps one transport per distinct certificate for the life of the process: 90 open connections to the tenant
  front and 3 TLS handshakes a second. A tenant client is kept per cluster for 5 minutes, keyed by Cluster UID and instance
  (755e8bea): 2 connections after. Every `openbao.Client` and vault admin client also built its own transport with no idle
  timeout (63 core connections per cluster vault); they close an idle connection after 90 s (47134c70).
  (4) The OpenBao storage: the requester runner writes `contracts/<name>/secrets-key` into its vault mount for every contract
  (in the gateway, the root vault's `requester/` KV mount, one entry per node a tenant cluster ever rented, metadata and
  versions) and nothing removed it. Every 5 minutes the requester controller now lists `contracts/` and removes the key of a
  name that is no ComputeContract, runner or waiting contract (96a9b60d); its first pass removed 6. The per-cluster vault
  directories (`openbaoservers/c-<key>-bao`) and data directories are purged with the cluster (read: only the live clusters'
  directories exist after 122 deletions). Tidy (token and lease) covers what is left: 3 token accessors and 2 cert login leases.
  (5) An upgrade did not apply a new kcp argv: the core adopted the running slot, so prod ran slot a from Oct 12 without the
  mount proxy flags after rc15 until a hand `units roll kcp`. An installed core now compares the active slot's `/proc` cmdline
  and executable with the slot spec it renders and the kcp asset link, and on a difference runs the installed roll under the
  roll lock (an executable change checks the kcp compat records first), then adopts the new slot; a refusal is logged with
  the `CheckArgv` reason (558cba23). Read on droplet dev-churn-2: `install gateway` with a9fdef3d, `upgrade` to 8b96e1ca, the
  core logged `argvChanged` with the refusal and rolled a to b in 12 s, slot b runs with both flags, and a second core restart
  adopted b without a roll.
  Readings after the fixes, fitted 03:05Z to 04:18Z (73 minutes after 15 minutes of warm-up; 128 clusters created and 122
  deleted since 02:46Z, 0 failed; the same fit rule as M4-SOAK-READING):

| series | start | end | max | slope per hour | plateau |
| --- | --- | --- | --- | --- | --- |
| kcp RSS (MB) | 913 | 999 | 999 | +48.5 | NO |
| kcp RSS, 03:35Z to 04:18Z (MB) | 974 | 999 | 999 | +33.3 | yes |
| kcp in-use heap, pprof (MB) | 349 | 337 | 351 | (3 profiles) | yes |
| kcp established connections | 27 | 23 | 27 | +0.3 | yes |
| kcp socket fds | 164 | 160 | 166 | -0.2 | yes |
| tenant front (bootstrap cluster) established | 8 | 6 | 8 | +0.4 | yes |
| tenant front socket fds | 26 | 24 | 28 | -0.2 | yes |
| lane cluster fronts established (6 clusters) | 136 | 208 | 214 | +0.2 | yes |
| gateway core RSS (MB) | 94 | 102 | 114 | +5.3 | yes |
| gateway core socket fds | 279 | 162 | 402 | +1.7 | yes |
| OpenBao processes established | 450 | 413 | 511 | -2.7 | yes |
| gateway openbaoservers dir (KB) | 17648 | 17712 | 17736 | -3.3 | yes |
| kcp logical clusters | 26 | 26 | 28 | +0.0 | yes |
| kcp workspaces | 29 | 31 | 33 | -0.2 | yes |
| kcp APIBindings | 97 | 97 | 103 | +0.1 | yes |
| Clusters / ClusterLinks / HostProcesses | 7 / 10 / 138 | 7 / 14 / 160 | 7 / 14 / 160 | +0.0 / -0.2 / +1.5 | yes |

  The root vault's file storage read 129 files and 104.8 KB at 02:50Z and 118 files and 90.9 KB at 04:18Z; its KV entries were the
  6 live contracts' keys, 5 live bundles and the identity and kcp entries. The kcp RSS climbs toward a steady state while its live
  heap stays at 340 to 350 MB (the Go runtime keeps freed pages); the last 44 minutes are flat. The network kine's live keys rose
  1206 to 1454 (Bids +104 and Events, which end by their TTLs; the gateway's own kinds are flat). Not settled in the window and not
  part of these two series: the gateway requester DenoPod RSS (restarted at 03:48Z for (4)) and the web app DenoPod RSS (179 to
  243 MB); the gateway requester's connections to the dispatcher are a sawtooth (50 to 750, 2200 in the burst after a restart)
  that the fetch pool drains, and an unread body on a non-2xx getRecord no longer holds one (ed00b815). Also found: on a fresh
  droplet the `socialweb-computer-k8s-downloads` asset stages to tree baf256f7 (the socialweb-meshd binaries differ by 240
  bytes), not the pinned 01030c9a, so a droplet's dev build needs that pin changed in its own checkout. Cause: ring's C was
  compiled by the host clang (Fedora 22.1.8, Ubuntu 18.1.3). Closed by 13473a6b: rust/build.sh compiles it with the zig cc and
  zig ar of a zig it pins by sha256; the tree is acb85490 on Fedora 44 and on Ubuntu 24.04. ACC13-BOUNDED's gateway
  kcp and OpenBao parts hold on this reading.

- **The requester's orphan records, the bidder's bid sweep, the requester heap and the gateway requester's retention (plan 0013
  M3-PDS-RECORD-RETENTION and M3-DISK-RETENTION, 2026-10-18, commits 8dad7806, b517683f, 1be95509, feccd06d, a60721ac,
  efa7e5cd).** The four series that section 14 kept open after dev-gc-1. Causes and changes:
  (1) Orphan records. Every 30 s the controller's resync lists the ComputeContracts and called `forget()` on every runner whose
  name the list lacked. A contract created while the list was in flight had already been launched from the watch, so it was taken
  for deleted: the runner set deleted and released, skipped every later status write and ended with "the contract was released
  before a VM was held", and the object kept no `status.contract`. Its compute.vm and market.rfp were named by nothing, so
  retention never removed them. That is one orphan per resync under load (45 in 50 minutes on dev-bound-1 before the fix, +110
  an hour on dev-gc-1). A name that `observe()` saw at or after the list began is no longer forgotten (8dad7806). The same pass
  found that the gateway's requester, whose records are in its application's repo PDS since plan 0018, compared each ref with the
  local requester PDS's DID and removed nothing at any retention; refs are matched with the DID that `deleteRepoRecord` writes
  to. A request that throws now hands its state to the runner (`ComputeRequestFailed`), and an at-capacity round removes its rfp
  and compute.vm before the next round (b517683f). As a backstop, the orphan scan removes a settled compute.vm that no
  ComputeContract claims once it is older than the record retention, with the rfp, accept and events that name it. It reads one
  listing per pass, stops after 60 s, logs `requester_orphan_records_removed`, and covers a deleted NodeClaim's contract
  (1be95509).
  (2) Bid sweep. The cap of 100 deletes a pass was set for the bsky.social write budget, which no longer applies since plan 0018.
  The sweep now deletes every due record each pass in `applyWrites` batches of 100, with a 5-minute bound per pass, and lists
  every page (oldest first) until that bound (feccd06d).
  (3) Requester heap. `createATProtoEventStreamsClient` pushed each dedicated watcher's stream watchers into the client's
  `watchers` list and never took them out on close. Every contract watch after the first batch stayed reachable with its
  `openContractWatch` closure (a60721ac). The unfixed requester (network "bound", same host) read heapUsed 266 MB at 06:59Z,
  326 MB at 07:09Z and 368 MB at 07:19Z, with its ComputeContracts flat at 2640: about +300 MB an hour, which reaches the
  792 MB limit in about 2 h. The fixed one reads flat (below).
  (4) Gateway retention. A Gateway spec option mapped `to: requester` in `deploy/apps/option-map.yaml` goes to the gateway's
  requester DenoPod. `record-retention-sec` is the first such option; the prod default of 7 days stays (efa7e5cd).
  The requester also logs `requester_memory` every minute (its V8 heap and the sizes of its per-contract maps), and the soak
  driver gained `orphans` and `release`.
  Reading on droplet dev-bound-1 (c-48, sfo2): one `dev up --profile market-network --bidders 1 --with-gateway
  --vm-host-backend systemd-user`, with app values setting the bidder's `bid-ttl-sec` 1800 and the gateway's
  `record-retention-sec` 900. The `test/soak` driver ran at 1.1 RFP/s, a VM contract every 40 s (3 lanes), 2 tenant lanes,
  contract TTL 40 minutes, requester retention 900 s scanned every 60 s, sampled every minute. The requesters were restarted at
  07:00Z and 07:22Z onto the DID and resync fixes, and the fit runs from 07:40Z to 09:10Z (the section 15 soak rule). Load from
  06:30Z to 09:10Z: 10559 RFPs, 239 VM contracts (237 released, 1 failed), 79 tenant cluster cycles (2 failed while the gateway
  requester restarted). `run request --network` reached the guest at 06:30Z and at 09:10Z. After the resync fix no contract
  ended "released before a VM was held"; the backstop's 39 removals were the orphans written before it. Readings:

| series | start | end | max | slope per hour | plateau |
| --- | --- | --- | --- | --- | --- |
| requester repo kv file (MB) | 63.2 | 63.2 | 63.2 | +0.0 | yes |
| requester repo: compute.vm | 1019 | 1039 | 1044 | +15.1 | yes |
| requester repo: market.rfp | 1020 | 1031 | 1042 | +14.3 | yes |
| requester repo: market.accept / market.event / vm.delete | 25 / 25 / 24 | 24 / 24 / 23 | 25 / 25 / 24 | +0.0 | yes |
| requester DenoPod heapUsed (MB) | 101 | 98.8 | 119 | +2.2 | yes |
| requester ComputeContracts (controller) | 2701 | 2701 | 2702 | -0.2 | yes |
| requester DenoPod RSS (MB) | 410 | 454 | 472 | +21.6 | NO (heap flat; section 14) |
| bidder repo kv file (MB) | 48.7 | 51.5 | 51.5 | +1.5 | yes |
| bidder repo: market.bid | 2491 | 2457 | 3040 | -25.4 | yes |
| bidder repo: market.event / market.receipt | 177 / 62 | 196 / 68 | 251 / 88 | +6.7 / +0.2 | yes |
| bidder DenoPod RSS (MB) | 351 | 323 | 354 | -2.5 | yes |
| bidder pds DenoPod RSS (MB) | 312 | 389 | 454 | +28.8 | NO (section 14) |
| gateway repo kv file (MB) | 5.5 | 5.5 | 5.5 | +0.0 | yes |
| gateway repo: compute.vm / market.rfp / market.accept | 6 | 10 | 10 | +0.3 | yes |
| gateway requester DenoPod RSS (MB) | 227 | 550 | 698 | +283 | NO (native, heap flat at 45 MB; section 14) |
| state dir total (MB) | 5354 | 13662 | 13662 | +568 | yes |

  The bidder's sweep deleted about 680 market.bid records and 125 to 160 trail records each pass (scanned about 3050), so Bids
  stay at bid rate times TTL. The repo kv files sit at their SQLite high-water marks (section 15, hono-pds entry).

- **hono-pds collects unreachable blocks, and the requester's retention removes the vm.delete and market.event records (plan
  0013 M3-DISK-RETENTION and M3-PDS-RECORD-RETENTION, 2026-10-18, commits 508d79fc and 58d5bc38).** Before: the 3 h soak read
  the bob PDS kv file at +213 MB an hour and the requester repo at +171 MB an hour, because `DenoKvStorage` never deleted a
  block, and the requester's compute.events.vm.delete and market.event records at +86 an hour each, because `retention.ts`
  removed rfp, payload and accept only. The change: `DenoKvStorage.put` writes a key-only index entry (`["blockidx", cid]`, time
  and size) beside each block; `Repo` runs a mark and sweep at most every 5 minutes after a write: it marks each head's commit,
  every MST node and every record CID the nodes name, and removes each indexed block outside that set that was written more
  than 10 minutes ago. A firehose frame carries its own blocks and `getRepo` serves only the head, so older revisions are not
  kept; the 10 minutes cover a write in flight. Each delete checks the index entry's versionstamp, so a block written again
  during the sweep stays; the first pass indexes the blocks of a store written before the index. The factory logs
  `repo_block_gc` (blocks, bytes, removed, kv file bytes before and after, pass ms). This covers every repo built on
  `createRepoFactory`: hono-pds, the requester's repo, each application's pds DenoPod (hono-repo-pds) and the ssh app PDS. The
  requester's retention also removes `state.release.payload`, `state.release.event` and `state.submitEventRef` (refs in its own
  repo only) and skips a contract whose runner is running, so a release that is still retrying keeps the event plan 0020
  resends. Reading on droplet dev-gc-1 (c-32, sfo2): one `dev up --profile market-network --bidders 1 --with-gateway`, the
  `test/soak` driver at 1.1 RFP/s, a VM contract every 40 s (3 lanes), 2 tenant lanes, contract TTL 40 minutes, requester
  retention 900 s scanned every 60 s, sampled every minute, fitted from 03:50Z (one hour of warm-up) to 06:04Z with the
  section 15 soak rule. Since plan 0018 the bidder's and the gateway's repos are their pds DenoPods'
  `components/pds/repo.sqlite3`, so the network's bob-pds and alice-pds kv files are idle (0.2 and 0.4 MB, flat). Load to
  06:04Z: 12869 RFPs, 254 VM contracts (239 released), 86 tenant cluster cycles with a node out and in through the RFP flow, and
  two `run request --network` runs that reached the guest. Readings:

| series | start | end | max | slope per hour | plateau |
| --- | --- | --- | --- | --- | --- |
| requester repo kv file (MB) | 64.6 | 66.2 | 66.2 | +0.8 | yes |
| requester repo: compute.events.vm.delete | 24 | 21 | 25 | -10.1 | bounded, 18 to 25 (the fit reads a step down as NO) |
| requester repo: market.event | 24 | 22 | 26 | -8.9 | bounded, 19 to 26 (as above) |
| requester repo: market.accept | 25 | 22 | 26 | -6.8 | bounded, 19 to 26 (as above) |
| requester repo: compute.vm | 1097 | 1307 | 1454 | +111 | NO (orphans, section 14) |
| bidder repo kv file (MB) | 43.0 | 80.9 | 80.9 | +17.5 | NO (live market.bid, section 14) |
| gateway repo kv file (MB) | 5.9 | 9.2 | 9.2 | +1.5 | NO (7-day retention, section 14) |
| state dir total (MB) | 11472 | 7889 | 13629 | -2533 | yes |

  The requester's last pass: 28306 blocks (20.0 MB) kept, 3621 of them reachable from the head, 9772 blocks (7.2 MB) removed, kv
  file 66.2 MB before and after, 6.8 s. The bidder's: 22612 kept (18.7 MB), 16532 reachable, 2284 (1.6 MB) removed, 2.4 s. The
  kv file does not shrink: SQLite reuses the freed pages, so the file stays at its high-water mark, about 3.5 times the kept
  bytes. A local check of 6 rounds of 300 creates and 300 deletes of 20 KB records read the file flat at 20.6 MB from round 2 on,
  with every record readable after a reopen. No `repo_block_gc_failed` line and no missing block in 3 h 30 min. The 125
  hono-pds tests pass. The soak driver was ported to the one-kcp network (5d72f50d) and samples each application's repo
  PDS (8718ddc2).

- **Plan 0013 M4-SOAK-READING: a 3 h accelerated soak of a market network with a gateway, read on a DigitalOcean c-32 droplet
  (2026-10-17 22:12:48Z to 2026-10-18 01:13Z; M4 stays `doing`, ACC13-NO-ORPHANS is `done`).** Host: droplet dev-soak (c-32,
  32 vCPU, 64 GB, Ubuntu 24.04, sfo3, /dev/kvm), account test-0001, a dev build of the tree, one `dev up --profile market-network
  --bidders 1 --with-gateway` plus a long-lived MarketRequester child (`soakreq`) that the driver `test/soak` (Go: `load`, `sample`,
  `tidy`, `inspect`, `hammer`) adds to the network and feeds with ComputeContract objects. The driver and the sampler ran as user
  units on the droplet; the sampler wrote one JSON line per 5 minutes to `/var/tmp/soak-data/samples.jsonl` (not `/var/tmp/soak`,
  which the dev build mirrors over). Accelerated settings (the user changed the item to 3 h on 2026-10-17): Bid TTL 1800 s plus the
  300 s bid window (a Bid lives about 35 minutes, so it turned over 5 times), ComputeContract TTL 40 minutes, requester record
  retention 900 s scanned every 60 s, the bidder's record sweep every 10 minutes, storemaint every 5 minutes, run directories swept
  every minute with a 1 h keep, guest images collected every 10 minutes. OpenBao tidy: the default interval is 6 h, so at 23:12:52Z
  `soak tidy` set `spec.tidy.interval` (token and lease tidy) and `spec.declared.pkiTidy[].interval` (pki auto-tidy) to 15 minutes on
  every OpenBaoServer of the network, the three applications and the tenant vaults; the bidder's server status read `lastRunAt`
  23:12:53Z and runs every 15 minutes after. kcp store backups are written only on a kcp roll, and a market network never rolls a
  one-slot kcp, so M1-BACKUP-RETENTION was not exercised here.
  Load over the three hours: 11909 RFP-only contracts (a ComputeContract annotated `socialweb.computer/rfp-only` stops after its RFP
  is submitted; the bidder still bids), 270 VM contracts (263 held and released, 7 failed with `no_bids` or a `vm.delete` 404 across
  bidder restarts), 226 tenant cluster cycles (the driver makes a cluster, a pod scales a node out through the RFP flow, the node
  scales in, the driver deletes the cluster; 8 lanes). That is 12.4k RFPs and about 500 contracts, above the item's counts scaled to 3 h
  (6250 RFPs, 250 contracts). Phase 1 (22:12:48Z to 22:13:48Z): 0.6 RFP/s, one tenant lane, a VM contract every 40 s (3 lanes). Phase 2
  (22:13:48Z to the stop at 01:14Z): 1.1 RFP/s and 8 tenant lanes, which is per hour 3970 RFP-only contracts, 90 VM contracts and 75
  cluster cycles. Chaos: 25 DenoPod restarts through `dev restart` (5 each of bidder, guest-kcp, gateway requester, gateway web app and
  requester), then at 01:09Z `kill -9` of the bidder DenoPods and at 01:11Z `kill -9` of the bidder core and the gateway core; each came
  back (the provider placed the pods again, the network placed the cores again and read Ready). `dev restart core` was not used: see the
  limit below. Host: vmstat idle 55 % on average (25 to 80 %), memory used 18 GB on average (24 GB at most), MemAvailable at least 38 GB,
  load average 17 (31 at most), no swap, oom_kill 0.
  Fit: least squares over the samples from 23:12:48Z (one hour of warm-up, longer than a Bid lives); a series is flat when
  |slope x window| <= max(2 x residual standard deviation, 5 % of its mean). The record rows are fitted from 00:20Z, after the two
  retention fixes below. The tenant clusters' processes (k3s, kine, postgres and OpenBao of each cluster) are counted under the
  network's application; the driver keeps about 8 clusters at a time, so those rows are noise around a lane count, and the state dir
  total is dominated by the VM work dirs that come and go, which is why it reads flat while its parts below do not. Readings:

| series | start | end | max | slope per hour | plateau |
| --- | --- | --- | --- | --- | --- |
| bidder application, all processes (RSS MB) | 1941 | 1999 | 2117 | +30.3 | yes |
| bidder kcp | 989 | 995 | 1080 | +10.2 | yes |
| bidder kine | 91 | 106 | 136 | +10.2 | yes |
| bidder DenoPod | 249 | 278 | 324 | +2.3 | yes |
| guest-kcp DenoPod | 154 | 152 | 157 | -1.8 | yes |
| bidder core | 157 | 163 | 297 | +9.2 | yes |
| bidder OpenBao | 127 | 131 | 132 | +2.5 | yes |
| requester application, all processes | 2122 | 2243 | 2454 | +58.2 | yes |
| requester kcp | 959 | 1036 | 1054 | +48.2 | NO |
| requester DenoPod | 619 | 698 | 932 | +42.5 | yes |
| requester core | 167 | 128 | 219 | -34.9 | NO |
| gateway application, all processes | 3390 | 3641 | 3936 | +94.2 | yes |
| gateway kcp | 943 | 1108 | 1134 | +104.1 | NO |
| gateway requester DenoPod | 499 | 352 | 636 | -70.0 | yes |
| gateway web app DenoPod | 226 | 237 | 243 | +7.7 | yes |
| gateway core | 272 | 300 | 379 | +9.1 | yes |
| gateway OpenBao (root vault and tenant vaults) | 861 | 963 | 1078 | +21.2 | yes |
| gateway postgres | 221 | 259 | 259 | +8.6 | yes |
| network kcp | 659 | 656 | 660 | -1.0 | yes |
| network core | 110 | 112 | 115 | +0.9 | yes |
| bob PDS DenoPod | 744 | 779 | 845 | +22.6 | yes |
| alice PDS DenoPod | 185 | 213 | 254 | +18.5 | NO |
| relay DenoPod | 429 | 718 | 781 | +139.3 | NO |
| PLC DenoPod | 137 | 137 | 138 | -0.0 | yes |
| dispatcher DenoPod | 158 | 161 | 162 | +2.0 | yes |
| network OpenBao | 121 | 122 | 122 | +0.7 | yes |
| Bid objects (bidder kcp) | 2398 | 2353 | 2405 | -28.5 | yes |
| ComputeContract objects (requester kcp) | 2702 | 2686 | 2715 | -10.2 | yes |
| ComputeVM objects | 4 | 5 | 9 | +0.4 | yes |
| bidder gc scan: objects | 2426 | 2388 | 2455 | -25.9 | yes |
| requester gc scan: objects | 2711 | 2696 | 2723 | -9.7 | yes |
| gateway gc scan: objects | 96 | 93 | 120 | -5.3 | yes |
| bidder kine rows | 6438 | 6741 | 8492 | +177.1 | yes |
| bidder kine file bytes | 42188800 | 43956907 | 54951936 | +1002092.0 | yes |
| requester kine rows | 6832 | 6908 | 7684 | +33.5 | yes |
| requester kine file bytes | 41483605 | 42893995 | 47624192 | +812285.6 | yes |
| gateway kine rows | 3157 | 3217 | 4435 | +34.1 | yes |
| gateway kine file bytes | 19934549 | 19554304 | 27082752 | -343698.2 | yes |
| bidder OpenBao storage (MB) | 18 | 19 | 19 | +0.2 | yes |
| gateway OpenBao storage (MB) | 19 | 21 | 22 | +1.2 | NO |
| requester OpenBao storage (MB) | 0 | 0 | 0 | +0.0 | yes |
| network OpenBao storage (MB) | 1 | 1 | 1 | +0.0 | yes |
| state dir total (MB) | 18561 | 27695 | 41607 | +4484.5 | yes |
| bidder VM work dirs (MB) | 8856 | 11669 | 25682 | +980.4 | yes |
| gateway kcp dir, audit logs (MB) | 3291 | 7891 | 8435 | +2812.8 | NO |
| bidder kcp dir, audit logs (MB) | 302 | 607 | 650 | +187.5 | NO |
| requester kcp dir, audit logs (MB) | 201 | 393 | 414 | +117.9 | NO |
| network kcp dir, audit logs (MB) | 149 | 324 | 346 | +106.8 | NO |
| gateway tenant cluster dirs (MB) | 1648 | 1926 | 2239 | +58.5 | yes |
| bob PDS kv file (MB) | 224 | 585 | 628 | +222.2 | NO |
| requester repo kv file (MB) | 173 | 470 | 505 | +182.8 | NO |
| relay kv file (MB) | 0 | 0 | 0 | +0.0 | yes |
| host MemAvailable (MB) | 49129 | 45789 | 54093 | -1396.9 | yes |
| host load average 1 min | 17 | 17 | 31 | -0.2 | yes |
| host oom_kill counter | 0 | 0 | 0 | +0.0 | yes |
| host swap used (MB) | 0 | 0 | 0 | +0.0 | yes |

  Records in the PDS repositories (counts by collection, fitted from 00:20Z):

| series | start | end | max | slope per hour | plateau |
| --- | --- | --- | --- | --- | --- |
| bob repo: compute.config.wif.simple | 2552 | 2527 | 2788 | -31.2 | yes |
| bob repo: compute.events.vm.hostKeys | 98 | 99 | 115 | +5.5 | yes |
| bob repo: compute.events.vm.onNetwork | 197 | 199 | 231 | +9.2 | yes |
| bob repo: compute.events.vm.terminated | 97 | 96 | 110 | +1.6 | yes |
| bob repo: market.bid | 2540 | 2485 | 2782 | -60.3 | yes |
| bob repo: market.bids.free | 2552 | 2512 | 2793 | -55.5 | yes |
| bob repo: market.event | 378 | 384 | 450 | +16.9 | yes |
| bob repo: market.receipt | 100 | 101 | 116 | +4.4 | yes |
| requester repo: compute.events.vm.delete | 195 | 260 | 264 | +86.4 | NO |
| requester repo: compute.vm | 1917 | 1910 | 1939 | -7.9 | yes |
| requester repo: market.accept | 116 | 115 | 118 | -0.5 | yes |
| requester repo: market.event | 195 | 260 | 264 | +86.4 | NO |
| requester repo: market.rfp | 1905 | 1900 | 1931 | -4.8 | yes |

  Seven defects the soak found and the tree now fixes, each proven by a short run on the droplet, in the order found.
  (1) hono-pds loaded and rehashed its whole record tree on every read and write (`collectBlocks` serialised and stored every MST
  node on each `set`, `getRecord` and `listRecords` rebuilt the tree, `diff` walked two whole trees), so at 1000 bids its heap hit the
  512 Mi limit, it restarted on a new port and the relay lost it. Bench of 4000 records: write 117 ms to 4.4 ms, getRecord 79 ms to
  0.1 ms. Nodes loaded from the store or already put are marked persisted and skipped, a repo keeps its latest tree for reads, and a
  write forks it. This is the only change to `ts/hono-pds/lib/abc/atproto-repo/mst.ts`; the need is that the soak could not run a PDS at 0.6
  RFP/s without it; the 125 hono-pds tests pass. (2) The relay deno was at 25 GB after 30 minutes: hono-pds sends a commit frame as
  JSON with `blocks` as an object with one key per byte, a requester's compute.vm record carries the whole cloud-init user_data so a
  frame is about 4 MB of JSON, the relay kept 50000 parsed frames and stringified each again for each of 40 subscribers, and a closed
  `subscribeRepos` socket left its live subscription collecting every later frame. No subscriber reads `blocks` (they read the ops and
  fetch the record by key), so the relay forwards a frame with empty blocks and ends the live loop of a closed socket, and both
  sequencers also stop at 64 MiB; relay RSS after the change: 115 MB at 9 minutes against 2137 MB. (3) The ingress proxy closed a
  subscriber after any single request went 30 s without an answer, so one slow `submitRfp` evicted the bidder's websocket (89
  reconnects in 9 minutes, a third of the RFPs answered 404, 20 % of VM contracts failed); it now evicts only a subscriber that
  answered nothing in the window (0 `ws_close` in the bidder pod afterwards). (4) A bidder pod killed one second after an accept
  left the ComputeVM running with no owner and the Won Bid with no `status.computeVM`; the ComputeVM controller now binds the two
  by the `socialweb.computer/accept` annotation (the gc scan then read 0 in 32 of 38 samples and 1 or 2 ComputeVMs without an owner
  in the other 6, once for three samples in a row around 23:10Z, and 0 after the driver stopped). (5) The bidder removed bids,
  payloads and bid configs but never the receipt, event and vm event records of an ended contract, which grew by 130 receipts and 500
  events an hour; the sweep now removes a receipt older than the record TTL whose bid record is gone, and every event whose receipt
  is not live, with its payload (the bidder repo went to a sawtooth of 90 to 110 receipts and 370 to 450 events). (6) The requester's
  record retention removed 50 contracts' records a pass, below 100 contracts a minute, and a contract object deleted by its TTL is
  never swept; with 500 a pass the repo went from 2553 to 1874 compute.vm records and stayed there. (7) kcp's audit log keeps every
  rotated file (`--audit-log-maxbackup=0`, `--audit-log-maxage=0`): the gateway slot wrote 100 MB every two minutes under cluster
  churn and held 8.4 GB after 3 h (35 GB after 8 h in the earlier run), and the other slots 330 to 650 MB; `kcpserver` now passes
  maxage 1 and maxbackup 4. Proof on a fresh `dev up --profile quick` instance with the change: 5.49 million requests from 64
  clients rotated the log about 40 times and the directory held the current file plus four of 100 MB, 488 MB in all. That fix is
  read on one quick kcp, not on the soak.

  What did not plateau, each a section 14 item: the kcp audit logs above (fixed, not re-read in a soak), the hono-pds kv files (the
  store never deletes a block: bob 628 MB and the requester's repo 505 MB after 3 h, +213 and +171 MB an hour), the gateway kcp RSS
  (599 to 1134 MB in 3 h, 827 to 1740 MB in 7 h of the earlier run, 100 to 130 MB an hour at 75 cluster cycles an hour, about 2 MB per
  cluster made and deleted; the slot limit is 3 GiB), the gateway OpenBao storage (17.7 to 21.2 MB, 1.2 MB an hour with tidy every 15
  minutes, 17 KB per cluster cycle), the requester's vm.delete and market.event records (kept for ever, +86 an hour at 90 VM contracts
  an hour), and the relay and alice-pds RSS inside the window (the relay stepped from 430 to 720 MB at 00:55Z while its backlog filled;
  bob-pds peaked at 845 MB, above its 512 Mi limit, which the dev backend does not enforce). After the reading the market network set
  raises bob-pds to 2Gi and the relay to 1Gi, and the relay backlog cap falls from 50000 to 10000 frames; those three are numbers
  from this reading, not re-read.
  Readings for plan 0018 (each application's repo PDS becomes its own DenoPod): the PDS DenoPod is single-threaded and is the first
  ceiling. At 1.1 RFP/s the bob-pds process used 14 % of one core in the first hour and 36 to 43 % after, against the bidder DenoPod
  at 4 to 6 %, the requester at 11 to 20 %, the relay at 3 to 10 %, the network core at 148 to 244 % (cluster churn), the gateway
  core at 93 to 114 %, the bidder core at 72 to 93 % and the kcp processes together at 161 to 205 %. A probe every 6 s from the droplet
  (a new TLS connection each time; `describeServer`, `listRecords` with limit 1 and `getRecord`, 1577 of each) read p50 46 to 51 ms
  and p95 54 ms in the first hour and 134 to 166 ms after, maximum 7.4 s. The bidder's `status.market.rfpToBid` over its last 256
  RFPs read p50 0.2 to 0.36 s and p95 0.3 to 14.8 s, and `kcpWait` p95 27 to 34 ms. An earlier pass on the same tree at 2.7 RFP/s with 13
  tenant lanes put the bob-pds main thread at 85 % and `rfpToBid` p95 at 54 s, and the bidder could not answer a `submitRfp` within
  the requester's 10 s, so the rate one bidder and one hono-pds sustain on this host is about 2 RFP/s (1.1 was held for the readings
  above). ComputeVMs peaked at 12 of the 16 VM slots.
  Limit found, not a product defect: `dev restart core` on a market-network instance restarts every network DenoPod on a new port
  and empties the PLC, so the children lose their relay and every DID; the first chaos step that restarted the network core ended
  that run at 22:02Z (after 8 h of healthy readings, which are kept). The soak therefore restarts pods and kills the child cores.
  M2-QUOTAS retune from the plateaus: per Bid the bidder kcp holds 0.14 to 0.15 MB of RSS and the store 10 to 15 KB and 2 rows
  (base 610 to 680 MB), per ComputeContract the requester kcp holds 0.13 MB and the requester DenoPod 0.23 MB. At the 3 GiB kcp limit
  20000 Bids would be 3.4 GiB, so `QuotaDefaults.Bids` is 10000 (about 2.1 GiB). ComputeVMs 64, ComputeContracts 1024 and 64 and
  NodeClaims 64 stay: the peaks read were 12 ComputeVMs, 5715 contracts in a requester whose quota the driver raised, and 1 NodeClaim
  per cluster. DenoRuns and PolicyWorkflowRuns stayed at 0 in the soak and stay at 200 and 500, unread. No quota hit happened in the soak;
  the earlier M2-QUOTAS reading (a quota of 10 Bids declined the 11th RFP, kcp RSS unmoved) stands.
  Done_when read: ACC13-NO-ORPHANS holds (at 01:18Z, after the driver stopped: bidder 2061 objects, requester 2435, gateway 20, each
  with `orphanTotal` 0 and `stuckTotal` 0). ACC13-NO-HOST-OOM holds for its last clause only: the host never ran out of memory
  (MemAvailable at least 38 GB, oom_kill 0 in 3 h and in 8 h); the DenoPods ran under the dev backend, which sets the V8 heap flag
  and no cgroup limit, so a memory breach was not re-read, and no quota was hit. ACC13-BOUNDED does not hold (the unflat series
  above), M4-SOAK-READING does not hold, M3-DISK-RETENTION's reading is contradicted (the kcp audit logs and the PDS kv files are
  state that never ends), and M3-PDS-RECORD-RETENTION holds for the bidder repo (every collection flat after the fixes) and not
  for the requester repo (vm.delete and market.event stay). M3-STORE-COMPACTION holds for the sqlite stores (the bidder kine file 40
  to 59 MB and 6.4k to 8.5k rows, flat while 2400 Bids turned over; the gateway store flat in 3 h; in the 8 h run the gateway store
  went from 1428 to 3660 rows) and is unread for the gateway postgres, which a market network does not run (the tenant clusters'
  postgres is the one in the rows above). The archived 14:01Z to 22:02Z run (8 h, 8 to 13 lanes, 0.6 to 2.7 RFP/s, before fixes
  4 to 7) shows the same shape: bidder, requester and network kcp flat (1.0, 1.05 and 0.65 GB), the gateway kcp at 1.76 GB and rising.

- **Plan 0016 ACC16-TIMING and ACC16-NO-REGRESSION: createCluster to Ready was waiting on the kcp client rate limit, not on the CPU (2026-10-17 UTC).**
  Host: a DigitalOcean s-4vcpu-8gb droplet (4 vCPU, 8 GB, Ubuntu 24.04, sfo2) shaped like the prod gateway, one market
  network with a gateway and one bidder, the bootstrap tenant cluster as the createCluster, read in the gateway core log
  from "the bootstrap cluster exists" to the first `cluster provider: status` with phase Ready. Before (main at ebedcc74):
  61.5, 62.0, 57.4, 57.5, 57.3 s, median 57.5 s. With the five-answer /readyz streak of ROLL-1 (main at 1b87e578): 65.1,
  61.3, 63.3 s. After (this change): 35.9, 34.5, 33.4, 37.4, 32.1 s, median 34.5 s. Cause: the clients of the cluster
  provider, the HostProcess supervisor, the Postgres controller and the tenant add-ons were built from a rest.Config
  without QPS, so client-go limited them to 5 requests a second. Each of the 15 port holds (a Get and an Update of the
  front HostProcess) took 0.4 s, 6 s in all, and every burst of ESO CRD applies and tenant add-on calls queued behind the
  same limiter. `kcpserver.FrontRestConfig` and the tenant and admin clients now set 50 qps and burst 100. Four smaller
  changes: the Postgres controller wakes on a new Postgres object and polls at 200 ms while one is not Ready (it ticked
  every 3 s, three ticks per store, 10 s from object to Ready, now about 3 s); the cluster's Postgres object is created
  before the front is placed and awaited after it, so initdb runs beside the front; the ESO webhook and controller
  share one tenant-half install (`stepNeeds`: the controller no longer needs the webhook ready, only the install, which
  both Prepares run once) and its CRDs are applied 8 at a time; a pass that spawned is followed by one at once, and
  `place` polls at 100 ms. A k3s step gates on the unauthenticated and controllers checks only: the five-answer streak
  stays in `WaitSlot`, which only the roll calls, so the roll guarantee of ROLL-1 is the code it was. Member timeline
  before and after (seconds after createCluster; P placed, R ready): front P 8.1 R 11.1, kine P 17.7 R 21.2, k3s P 21.3
  R 33.8, ccm R 35.9, esowh P 46.6 R 51.2, eso R 56.3, Ready 57.4; after front P 2.9 R 5.4, kine P 6.3 R 10.5, k3s P 10.6
  R 25.7, esowh and eso P 30.1, ccm R 30.8, eso R 32.8, Ready 33.4. One run with every change but the 50 qps: 53.3 s.
  CPU: `taskset` on the `dev up` process tree (the units of an instance are not in the user slice, so a cgroup cpuset on
  the user slice changed nothing). 2 vCPU: main with the streak 99.9 and 87.4 s, the change 62.8 s; this matches prod's
  78 to 117 s, so prod behaves like 2 vCPU. 4 vCPU: 57.5 (63.3 with the streak) before, 34.5 after. On a c-16 droplet
  (dedicated cores) 16 vCPU: 28.1 and 23.4 s after, 59.6 s before; 8 vCPU: 28.1 and 23.4 s after. Before the change CPUs
  did not help (59.6 s on 16 vCPU against 57 to 63 s on 4); after it, 4 to 8 to 16 vCPU buys about 25 percent. Resizing
  the prod droplet is not needed for ACC16-TIMING. Fail fast: with kine's argv replaced by /bin/false in a scratch
  build (not committed), kine@a was placed at 10:18:23.3 and the pass reported "kine@a of 4r81tsr8-tenant is Failed:
  hostprocess: ... kine@a.service failed after placement (result start-limit-hit, status 1)" at 10:18:32.9, 9.6 s later
  (the unit's own crash-loop budget, 5 starts at RestartSec 2); k3s and every member after kine were never placed, the
  next passes retried at the 2 s tick with the same message, and `dev down` left no unit, process or state. Killing
  k3s with SIGKILL during its start instead is restarted by its unit and is not a failure (the gate waited for the new
  k3s, Ready 33.3 s). Not done, and why: the hold of each port records itself on the front HostProcess and in the lease
  file one purpose at a time; batching that is a change of the held-port logic, which plan 0016 stop_and_ask reserves,
  and it now costs about 0.5 s; initdb --no-sync saves under 1 s and is left out. `deploy/check.sh --fast` on the droplet:
  gofmt, build, apigen, vet and deno check pass; the windows cross build stops in test/soak (`syscall.Stat_t`), a file
  of main that this change does not touch. Not read on prod; the prod reading is the lead's.

- **ROLL-1 closed: a rolled k3s slot got the apiserver front before its own `/readyz` was green (2026-10-17 UTC).**
  Opened by the acceptance check ACC6-CP-SWAP. Reading before, on dev-linux-1, a market network with a gateway and one
  bidder, a tenant cluster with one market node and one running pod, two pollers every 200 ms through the cluster front
  (one with a kept-alive connection, one with a new connection and a client certificate per poll), `units roll cluster
  <cluster> --idle-bound 5s`: greenReadyAt 20.4 s after the request, front flipped 0.006 s later, old slot stopped 1.2 s
  after that; the new connections got 500 from 07:32:26.6 to 07:32:30.9 (22 of 481), the first three also
  naming `poststarthook/rbac/bootstrap-roles` (the first two `poststarthook/scheduling/bootstrap-system-priority-classes`
  as well), all of them `poststarthook/apiservice-discovery-controller`; the kept-alive poller failed 8 of 479 from 07:32:28.5, after its
  connection to the old slot closed. Cause: `WaitSlot` (`internal/clusterprovider/front.go`) passed when `waitK3s`
  (`internal/clusterprovider/steps.go`) saw 200 or 401 from an anonymous `/readyz` on the slot port, and when
  `waitK3sControllers` found a Deployment in kube-system, which the old slot had already written to the shared store.
  Neither says the new apiserver ran its post-start hooks, and the roll flipped the front on that. Fix:
  `WaitSlot` also calls `waitK3sReadyz`, which needs `/readyz` to answer 200 five times in a row (250 ms apart) with the
  slot's admin client certificate before the front is pointed at the slot. This holds for the first start (the k3s step
  gate in `steps.go`) and for the roll. The old slot serves until the flip, and the front keeps established connections
  on it until it is drained, as before. Reading after, same run, three at once on one host: 0 failed of 532 and 0 of 528,
  0 of 537 and 0 of 533, 0 of 527 and 0 of 528 polls from 3 s before the request to 10 s after the report; the roll took
  95.1 s, 96.1 s and 95.1 s of which 10 s the idle bound and 60 s the watch; greenReadyAt 28.6 s, 29.1 s and 28.7 s after
  the request (about 8 s more than before because three clusters started at once, and 1.3 s for the five answers);
  flip to old stop 0.95 s, 1.6 s and 1.6 s; the node's Ready transition time, the NodeClaim phase and receipt, and the
  ComputeVM were the same before and after. Plan 0006 ACC6-CP-SWAP is done.

- **The plan-level acceptance checks of plans 0006 to 0009 and 0013 to 0016, read on 2026-10-17 UTC (main at
  80998ef9).** Host: dev-linux-1 (Ubuntu 24.04, 48 vCPU), up to six market networks at once, VM account test-0001; the
  gateway networks needed the web app downloads asset staged unpinned (its tree sha256 differs from the pin off Fedora),
  and the sign-in test needed Chrome and kubectl on the host. Integration tests, each PASS: `TestARentedVMSurvivesABidderKillAndRestart`
  226 s, `TestRequestSecretsReachTheGuestThroughTheRFPFlow` 217 s, `TestTenantClusterScalesFromZeroOnMarketVMsAndBack` 389 s
  (0 ssh lines), `TestBidderAnswersEachRFPOnceAcrossARestart` 308 s,
  `TestKubeconfigClientCertificateWithoutCredentialHelper` 170 s with kubectl, `TestWebAppSignInThroughThePluginKeepsTheCookieAcrossARestartAndIssuesACertificate`
  384 s. The hostsvc contract suite passed on systemd-user and on proc. One check failed (ROLL-1, closed in the entry below).
  Manual runs on top of the harness. Certificates (market network, gateway, no bidder): `getKubeconfig` without
  `ttlSeconds` gave 8 h, 86400 gave 24 h, 60 gave 1 min; 59, 86401, 0, -5 and 999999 were 400 InvalidRequest; k3s saw the
  caller DID as user and `socialweb.computer:cluster-owners` as group; injected `X-Remote-User`/`X-Remote-Group`/`X-Remote-Extra-*`
  headers were ignored; the k3s `client-admin` certificate of the same client CA was refused (403); `revokeIdentity` refused the
  old certificate 0.08 to 0.17 s after the call, `deleteCluster` 0.16 s; the revoked serials were in `pki/certs/revoked`;
  OpenBao builds no CRL (`pki/crl/rotate` warns that the imported issuer lacks CRL signing key usage), so the plan text
  now says the revoked list. The issued client key was in no file under the network state dir, no journal line, no process
  argument or environment and no kcp object (8 kinds). Sign-in (market network, gateway, bidder): after `kill -9` of the
  gateway core (back 0.6 s later, 4 DenoPods restarted, 9 member processes kept their pids) the same cookie answered
  `GET /session` 5.9 to 6.5 s later and the claim kept its uid and Running phase; `DELETE /session` made the PDS log a call to
  `/oauth/revoke` (log line added in the mirror only), the cookie then got 401, and a downloaded certificate was refused
  0.32 s after `revokeIdentity`; the cookie token was in no journal line and no process, the user's repo held 14 records
  in 4 collections and none held a token or key, and the only text files with `PRIVATE KEY` were 28 TLS and kcp `*.key`
  files of the infrastructure. The web app's DenoPod environment holds only projected file paths; its vault client
  `client-socialweb-computer-k8s` reaches `socialweb-computer-k8s/*` only and that mount held only `contracts/<name>/bundle`.
  Vaults (gateway network, one bidder, one node): with a node the cluster vault held one `jwt-<sha12>` mount, its role,
  the policy `node-<n>` and the `socialweb-nodes/<n>` entry, equal to the OpenBaoServer declaration; the same after a restart of
  the bidder DenoPod, after raising `signingKeyRotation` to 7 (three keys published) and after `kill -9` of the gateway
  core; 45 s after the pod was gone the cluster vault held approle, cert and token only, the bidder vault held no `guests/`
  entry and no `guest-<computevm>` policy, the root vault held no `contracts/` entry. Only the root vault (atproto plugin)
  and the bidder vault (sshsig plugin) set a plugin directory. Bidder (market network, two guests): each guest's pasta and
  firecracker ran in their own user namespace under host uids 721896 and 656360 inside test-0001's range 655360 + 1048576,
  no file under the state dir was owned by those uids, and neither uid could read the bidder's state, kcp, vault keys or the other
  guest's work dir; `kill -9` of a firecracker made its ComputeVM Lost 0.21 s later, the unit was inactive, the ComputeVM
  was released by the bidder and the `vms/` directory held the live guest only; `units audit` passed after boot and after
  a bidder restart; ssh with the reported host key worked, with another key and with no entry it failed `Host key verification
  failed`; no log of the network held an ssh session line. Deno argv: 42 service processes, none with `-A`, all
  with `--deny-read` and `--deny-write` of `/dev/kvm` and `/run/user/<uid>`, the policy engine with no `KCP_TOKENS` and no
  projected read, no process with `--allow-ffi`, and `--allow-run` only on the 20 launchers of `deploy/denopods/service.yaml`.
  Installed bidder (staged install of account swc-acc joined to a market network, two held guests, UTC 07:03 to 07:04):
  `systemctl --user kill -s KILL` of the bidder, `systemctl --user restart` and `upgrade --binary` to a build with another closure
  each brought it back Ready in 12 s, 4 s and 4 s with three DenoPods once each, the two VM units kept their InvocationID,
  pid and start ticks, both ComputeVMs stayed Running, `units audit` passed each time, after the upgrade every DenoPod ran closure
  109e962f and the closure 013dc7eb stayed on disk; `run request release` then gave vm.delete 200 for both with no ComputeVM,
  VM unit or work dir left. The qr.fedfork.com client metadata scope covers every bidder and requester scope token (25 against 24).
  Not shown by these runs: a second bidder process on the same kcp (the relay ingress sent every RFP to the second
  process, so no duplicate arrived) and a gateway `upgrade` with a signed-in cookie.
- **Plan 0012 is paused by the user (2026-10-16); what M0 settled and what it did not, as of 2026-10-17 UTC.**
  Done and committed: M0-REVISIT, M0-HOST-BASELINE and M0-VFKIT-ASSET (entries below). One pre-probe was run
  before the pause, outside any item: a system-domain LaunchDaemon with `UserName` the non-admin account
  `socialweb-m0012` ran the ad-hoc-signed thin vfkit (virtualization entitlement only) with `--bootloader
  efi,variable-store=...,create`, one vCPU, 512 MiB, a virtio-serial log file, a REST socket and no disk, no
  kernel and no network; it is a framework probe and not a guest, and the RFP rule that every guest is born from
  cloud-init was not touched. vfkit logged `virtual machine is running` and, in the same second, `VM is stopped`
  (the firmware had nothing to boot), and launchd read last exit code 0. So Virtualization.framework accepted a VM
  start in a job with no GUI session and a non-admin user while the owner's console session stayed open. It does
  not say whether a VM survives the death of its submitter, runs with no console session at all (the Mac may not
  be signed out, so that cannot be measured here), runs in the user/<uid> domain of an account with no login,
  or what its networking does; those stay M0-VZ-LIFETIME and M0-LAUNCHD-DOMAIN. Findings that change later items:
  a bidder on a Mac needs kcp, kine (sqlite, cgo, built here in 13 s) and bao as darwin assets and a launchd
  `hostsvc.Manager`, and `dev up` (the market network) is Linux only because `devsup` is, while the child
  bidder's `vmHostBackend` option already lets a network place VMs with another backend; a Mac market network
  therefore needs a darwin `devsup` (about 1000 lines on Linux: subreaper, pidfd and /proc become kqueue, process
  groups and sysctl) or a remote network that this Mac joins, and the second is not supported today because every
  network service binds 127.0.0.1:0 and peers find it by a per-application DNS shim; a darwin guest kernel needs
  virtio-pci (the firecracker configs have virtio-mmio only) and `vz-arm64` is the name reserved for it in
  versions.json, to be built on Linux by `scripts/build-guest-image.sh` from `firecracker-aarch64.config` plus
  PCI; no firecracker-aarch64 image was found in the Spaces bucket at main/35e91d21 (only x86_64 under
  `main/<commit>/guest-image/`; other commits were not listed), so the arm64 rootfs would be built too. A c-16 build droplet was created for
  that build and deleted again before it did any work.
- **Plan 0012 M0-VFKIT-ASSET: vfkit as an asset, shipped as the Red Hat universal file, extracted by the core's
  own store and run by a daemon user, read on 2026-10-17 UTC (05:25 to 05:40) on the Mac of M0-HOST-BASELINE.**
  Source: `https://github.com/crc-org/vfkit/releases/download/v0.6.4/vfkit`, 66431936 bytes, sha256
  `0ed83fc8ca7aa708598835480dba1362406aa7cd1dab3b27464eb76327d9652d`, equal to the digest GitHub shows for the
  asset; the release also has `vfkit-unsigned`, not used. It is pinned in `internal/versions/versions.json` as
  asset `vfkit` (platform darwin/arm64, `kind file`, `archive raw`, tree sha256 28d3b7e3..., rawBytes 66431936,
  zstdBytes 29401851 measured with `assetpack tar` and `zstd -q -19 --long=27 -T0`). The file is a universal
  Mach-O (x86_64 + arm64) signed `Developer ID Application: Red Hat, Inc. (HYSCB8KRL2)`, hardened runtime
  (flags 0x10000), entitlements network.client, network.server and com.apple.security.virtualization;
  `codesign --verify --strict` passes and `spctl --assess --type open --context context:primary-signature`
  answers "accepted, source=Notarized Developer ID". Decision from the readings: ship it as is. It needs no
  signing step, keeps Red Hat's provenance and notarization, and already carries the virtualization entitlement;
  the cost against the arm64 slice alone is 66.4 MB against 31.9 MB raw and 29.4 MB against 15.1 MB zstd.
  Alternatives measured and not chosen: the arm64 slice (offset 34504704, size 31927232, sha256 84d9dd06...) is
  byte-identical to `lipo -thin arm64` (run twice) and keeps a valid signature that spctl also accepts as
  Notarized Developer ID, so a Linux build could cut it without any Apple tool; an ad-hoc re-signed thin file
  (`codesign --force -s - --entitlements <virtualization only>`, sha256 d5582077..., flags 0x2, no team id) runs
  but spctl rejects it. Extraction used the real `internal/assets.Store.Resolve` (a throwaway program, not
  committed) with a pin built from `assets.TreeDigest`: 45 ms for the thin file and 96 ms for the universal one,
  a new inode inside a `.tmp-<random>` directory renamed to `<root>/<tree sha256>/vfkit`, store root mode 0700,
  no extended attribute on the result, `codesign --verify` valid after extraction. Run as the non-admin account
  `socialweb-m0012` (uid 480), the extracted ad-hoc file ran as a system-domain LaunchDaemon with `UserName`
  that account: `vfkit version: v0.6.4`, last exit code 0. Quarantine, three outcomes with the
  `com.apple.quarantine` attribute set by `xattr -w` (values `0083;<time>;Safari;`, a faked download mark): the
  Red Hat thin file ran at once, exit 0 and the version line, no prompt; the ad-hoc thin file did not return in
  25 s and had to be killed (the lead saw syspolicyd kill it with 137, "Terminating process due to Gatekeeper
  rejection"), and the same bytes extracted without the attribute ran, so a binary with no developer identity
  is blocked by Gatekeeper when it carries the attribute; the third outcome, the same ad-hoc file approved by the
  user, was not run, because an approval of a test copy with no developer identity proves nothing. Our writer
  copies no extended attributes (a quarantined source extracted to a file with none), so extraction never
  propagates quarantine. `spctl --assess --type execute` says "rejected" for every bare command-line tool,
  signed or not, so it is not the arbiter. Not read: the entitlement at VM start for the Red Hat file in a daemon
  (the pre-probe in the M0 pause note below ran the ad-hoc file).

- **Plan 0012 M0-HOST-BASELINE: the Mac, and what the landed tree does on it, read on 2026-10-17 UTC (05:21
  to 05:26).** Host: Mac17,4 (hostname Johns-Air), Apple M5, 10 cores (4 performance, 6
  efficiency), 24 GiB (`hw.memsize` 25769803776), macOS 27.2 build 26B5101f, Darwin 27.2.0, SIP enabled,
  FileVault ON, `kern.hv_support` 1, Xcode 27.1 (also the Command Line Tools package 27.0), go 1.26.4 (go.mod
  switches to 1.26.6 and kine to 1.26.9 by toolchain download), deno 2.9.7, limactl 2.2.0, 32 GiB free of 460.
  A console session of the owner has been open since Oct 9 (`who`: console), the machine has not rebooted
  since (`kern.boottime` Oct 9 20:53), `pmset` reads `sleep 1` held off by sharingd, caffeinate and powerd
  assertions (the Mac sleeps one minute after the last assertion goes; nothing here may release them), and
  `sudo -n` works for the owner. `socialweb-computer` builds for darwin/arm64 (go build ./cmd/socialweb-computer) and
  `socialweb-computer --help` marks, per verb, "no backend here ... (plan 0012)": every `dev` verb, `install`,
  `uninstall`, `check-host`, `run bidder`, `run request`, `serve gateway kcp`, `status`, `units *`,
  `internal vm *`, `internal devsup`, `internal hostprocesses`. What runs: `version --json` (revision, assets
  none in a dev build, guest images firecracker/aarch64 and firecracker/x86_64 only), `manifests list|render`,
  `apply`, `migrate`, `serve gateway xrpc`, `serve provider`, `internal apiserver-front`, `internal iroh-dial`,
  `nodeimage guest-*`. `check-host` answers "no systemd user manager on darwin/arm64". Third-party binaries on
  darwin/arm64, measured by hand (they are not guests): kcp v0.33.0 (`kcp_0.33.0_darwin_arm64.tar.gz`, sha256
  eb6c31c9... equals the release checksums.txt; the binary is 173 MB) started with the exact flags of
  `kcpserver.SlotArgv` over kine v0.17.1 on a unix socket: admin.kubeconfig after 4 s, listener
  127.0.0.1:60090 from `--secure-port=0 --experimental-bind-free-port`, controllers synced 13 s after start,
  RSS 413 MB kcp and 62 MB kine, both stop on SIGTERM. kine has no darwin release; `CGO_ENABLED=1 go install
  github.com/k3s-io/kine@v0.17.1` builds a working arm64 binary with sqlite in 13 s (55 MB). OpenBao v2.6.2
  `openbao_2.6.2_darwin_arm64.tar.gz` (sha256 4e495376... matches checksums.txt) runs, and `bao server -dev
  -dev-listen-address=127.0.0.1:0` binds a port 0 listener (60491) whose number appears in no log line (the log
  prints `:0`), so reading a bound port needs a darwin `hostsvc.ListenPort` (today a refusal reading /proc;
  `lsof`, `netstat -anv` or libproc are the candidates). `deno check --node-modules-dir=none` of
  `ts/atproto-market/hono-plc/mod.ts` and `hono-bidder/mod.ts` passes on deno 2.9.7. Release assets that exist
  for darwin/arm64: vfkit v0.6.4 (66 MB universal x86_64+arm64, signed Red Hat Developer ID with
  `com.apple.security.virtualization`), gvproxy v0.9.0 (`gvproxy-darwin`, 27 MB), dumbpipe v0.39.0
  (`dumbpipe-v0.39.0-darwin-aarch64.tar.gz`), deno, bao, kcp. A unix socket path of 103 bytes binds and 104
  fails with EINVAL on darwin (sun_path is 104), while `kcpdatastore`, `pgunit`, `devinstance`, `apiserverfront`,
  `irohdial` and `vmruntime` hold 107: every one of those constants and the install state dir
  (`/var/db/socialweb-computer/bidder/<h>/...` is long) must become OS-dependent. A home directory is mode 750
  (`drwxr-x---+`), so a daemon user cannot execute anything staged under the owner's `~/.tmp`: spike jobs run
  from a root-owned directory (`/private/var/db/socialweb-m0012`). The process-group question, measured with a
  stand-in parent that starts one child with `Setpgid` and one without, as a system-domain launchd job with
  `UserName` a non-admin account (`socialweb-m0012`, uid 480): SIGKILL of the job process took the plain child
  with it, and the `Setpgid` child (the shape of `execrunner` `osProcessHost.prepare`) was re-parented to launchd
  (ppid 1), survived the SIGKILL and survived `launchctl bootout`, and only a root `kill` removed it. So
  the plan's claim holds: on darwin the DenoPod runner must not give each run its own process group, or the
  deno children outlive a crashed bidder. Not run, because nothing places them yet: an OpenBaoServer object, the
  embedded deno with a closure, a DenoPod through the runner (`runner_darwin.go` refuses); they are the
  acceptance of M1-HOSTUNIT-LAUNCHD, M1-BOOTSTRAP-DARWIN and M1-DENOPOD-RUNNER-DARWIN.

- **Plan 0012 M0-REVISIT: the darwin seams as they landed, and what the plan missed, on 2026-10-16.** Read at
  socialweb-computer-kcp 79f40adb (vendored ts/hono-compute-provider and ts/atproto-market at that commit);
  plans 0006 (45 items), 0007 (32) and 0008 (16) are all done. `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build
  ./...` and `go vet ./...` are clean. Seam names, expected by the plan and as landed: the VM host interface is
  `internal/vmruntime.Runtime` (Prepare, Reserve, Await, Start, Inspect, Stop, Shutdown, List, Cleanup, Watch,
  Load, CollectImages) over `hostsvc.Manager` (Start, Inspect, Stop, ResetFailed, List, Identify, Check,
  StopSlice, Watch, Capabilities, Close); the Verdict is `hostsvc.Verdict` (Unknown, Running, Lost) with a
  `LostReason`; the placement backends are `hostsvc.Backend` `systemd-user` and `proc` (devsup), and
  `launchd` is the new one; the only Runtime is `firecracker` in `firecracker_linux.go`, and `ForHost`,
  `hostsvc.Open`, `devsup.Run`, `install.NewSystem` and `hostcontract.Gather` return ErrUnsupported or an empty
  answer on every other OS. `status.runtime` is `{backend, ref (the unit name), instance (the systemd
  InvocationID), pid, startTime (the decimal start ticks), hostBootID, vmmID}`; the ComputeVM controller
  (`internal/computevm`, a client-go loop) places a VM as a `HostProcess` child through the supervisor of
  `internal/hostprocess`, adopts it by name after a restart, and writes Lost with a reason. The binary install
  is `bin/<sha256>/` with `bin/current` and `bin/previous` (`internal/install` Upgrade, Rollback, GC; the plan
  said `versions/<sha256>/`); asset dirs are `runtime/<sha256>/` and `internal/assets.Collect` takes them out
  only when no loaded unit and no process names them. Guest images are pinned in `versions.json
  guestImages.targets`, built by `scripts/build-guest-image.sh`, fetched by `internal/guestimage` and never
  embedded (the plan said embedded darwin-only blobs); `vz-arm64` is reserved there, and the
  firecracker-aarch64 kernel config has virtio-mmio and no virtio-pci. The bidder unit is `Type=notify` with
  `WatchdogSec`, `Delegate=yes`, `Restart=always`, and the bidder core brings up kcp, kine (sqlite), the port-0
  front and the slot as units of the host-unit seam before the ComputeVM controller runs. Darwin stubs today:
  `hostos` ProcStart, Cmdline, Alive, Signal, Reload, Terminate, BootID, LocalPortRange; `portscan`
  (`scan_darwin.go`); `execrunner` `osProcessHost` and the cgroup limiter; `hostsvc` Open, ListenPort,
  OpenProcClient; `vmruntime.ForHost`; `devsup`; `install` System, SelfCheck, RestartOwnUnits;
  `hostcontract.Gather`; `assets.ProcessesOfUID`; `apiserverfront` fd store; `guestnet.Wire`; `nsexec`;
  `socialweb-vmexec`. `guestdisk.Clone` and `assets` syncTree have portable fallbacks (plain copy), and the lock
  is `flock` on every unix. The plan's text was amended for: the Runtime and Manager names and the files they
  live in, the install layout, the guest image path, the cloud-init module set (secrets, k3s, iroh,
  poweroff-ends-vm after 0008 M1-STRIP-LEGACY-TRANSPORTS), BootID on darwin (`kern.boottime`) for the
  HostRebooted reason, and one new item, M1-BOOTSTRAP-DARWIN, because a bidder runs its own kcp and kine and the
  plan never said a Mac needs them (and the plan excluded kine). Two gaps are recorded inside existing items:
  launchd has no watchdog, so the core must exit on its own stall (M2-BIDDER-DAEMON-DARWIN), and the bidder core
  asks the DenoPod runner for cgroup memory limits, which darwin cannot give (M1-BOOTSTRAP-DARWIN).
  `dev up` (the market network) is Linux only because devsup is, so a Mac bidder joins a market network that
  runs elsewhere through `run bidder --market-network-file`, or the market network is ported later. The user's
  2026-10-16 rule that the Mac is never rebooted, signed out or put to sleep splits the reboot, logout and sleep
  proofs of M0-LAUNCHD-DOMAIN, M0-REBOOT-LOST-POWER, M3-LIVE-SURVIVE and M3-LIVE-REBOOT-LOST into a
  no-reboot equivalent that is run and a reboot-only remainder that stays blocked.

- **A deleted cluster leaves no slice, no state directory and no failed unit, and the prod canary is a user
  timer on hat-8, on 2026-10-15 (plan 0010 G3-PROD-CANARY).** After the delete fix a deleted cluster
  still left an empty `socialweb-hp-gw-cluster-<key>.slice` (loaded, active, no process), its state
  directory `clusterprovider/<key>` with the `nodes` subtree (the per-node data dirs of
  `NodeDataDir` live under the bare key, while `ClusterDataDir` is `pdr-<key>`, so `PurgeData` never
  reached them), and, when the Postgres unit had crash-looped, a failed unit with no HostProcess. Three
  changes: `UnitsHost.PurgeData` also removes `<state>/<key>` (the key must be one path element); the
  HostProcess supervisor's orphan sweep resets a failed unit of its prefix whose HostProcess is gone; and
  after each pass the supervisor stops a slice named `<prefix>cluster-*` that it saw hold a HostProcess
  and that no listed HostProcess is in or under any more (`StopSlice`; no such unit is a success, so a
  slice systemd already dropped costs nothing, and a failed stop is retried on the next pass). The sweep
  is limited to the cluster slices on purpose: the bootstrap, core and VM slices hold units that are not
  HostProcesses, and stopping one of them stops those too. A slice the supervisor never saw (left by an
  earlier provider process) is not swept. Read on the dev market network (proc backend, which has no
  systemd slices, so the slice stop is read from the code path and not run against systemd): the
  `canary-dry` cluster's `<key>` directory and `pdr-<key>` directory were present while it ran and both
  gone once it was unlisted; the supervisor logged the empty-slice stop once for each of the three clusters
the run made (canary-dry, the leaked canary-to and canary-dry2) and never again, so the sweep does not loop.
  The canary of G3-PROD-CANARY is `scripts/prod-canary.ts` run by the user timer in `deploy/canary/` on
  hat-8 (the lead's decision, the user may veto): the credential files stay on that host and no secret goes
  to GitHub or GitLab, so there is no scheduled pipeline, runner or long-lived canary cluster. Each run
  makes `canary-YYYYMMDD` through the gateway's XRPC, applies a Job, deletes the cluster and checks it is
  unlisted and the bidder's ComputeVM is gone. Dry run on the market network's gateway (one bidder, one
  firecracker guest): preflight 0.2 s, cluster Ready 72 s, Job Succeeded 42 s after apply, unlisted 24 s
  after the delete, 140 s in all; a wrong audience exits 1 with the 401 in the result line, and a 20 s
  total timeout exits 1 after a best-effort delete.

- **The requester's RBAC records are the source of the guest's identity grants, on 2026-10-15 (plan 0009
  M3-RBAC-TO-OPENBAO).** For each live guest the bidder reads the `com.fedproxy.rbac` records of its
  requester's repo (the DID resolved through the PLC directory, `listRecords`) when it accepts, on every
  firehose event for that collection and DID, and every `--compute-provider-kcp-guest-identity-resync-sec`
  (30 s; the market network's requester keeps its repo in its own process, no relay crawls it and its
  commits are not on the firehose, so the read is also the floor under a lost event). A role of a record
  whose `sub` is the guest's subject (`actx:<bidder>:plc:<requester>:role:<role>`) and whose `iss`, when
  it names one, is the bidder's issuer becomes one `GuestIdentityGrant` per audience (api and actx from
  its `aud`, scopes from its policy names, claim `wid_sub` = the subject); a grant no record names any
  more is deleted, a record that cannot be read leaves the held grants alone, and the `guest-kcp` grant
  never depends on a record. The openbao controller does the rest as before: the grant's role and its path
  in `guest-<computevm>` appear and disappear with it. `guest-identity-grants-kcp` (implementation: the
  grant client, the registry of live guests, the read and the sync) is wired by
  `hono-factory-compute-provider-kcp`; the choice of grants from records is the pure
  `grantWishesFromRbac` of `guest-identity-abc`; `rbac-atproto` is the record reader (`resolvePDS`,
  `listRBACRecords`) and `checkRBACPolicy`, with no provisioner and no per-call gate (the
  bidder-side record that gated `/v1/oidc/issue` went with it). The requester now publishes the record the
  secrets capability needs (`GuestCapability.rbacRecord`) beside its ssh-key record, both under the
  record keys `ssh.<vm name>` and `secrets.<vm name>` (`rbacRecordKey`), and `releaseCompute` deletes them
  before it sends `vm.delete`. Readings (market network, one bidder, test-0001, UTC): the secrets run
  (`TestRequestSecretsReachTheGuestThroughTheRFPFlow`) PASS in 206 s with the grants taken from the
  requester's two records (`rbac_created ssh.compute-...`, `capability_rbac_created`, a token for
  `api://ATProto?actx=<requester>` answered 200, the secret delivered); `kubectl get guestidentitygrants`
  listed both grants APPLIED true with their roles; a release deleted both records at 19:29:34.161 and .166 and
  sent `vm.delete` at .177. The record change itself is not observable in that network (the release
  restarts the requester's endpoint in the same second, so no read of the emptied repo came back before
  the guest was gone), so the change was driven through the production code against the live bidder's
  kcp with a stand-in PLC and PDS and the real `requesterRbacChanged` entry point: record for api X ->
  grants `guest-kcp` and `X` created at 19:36:59.49 and the controller wrote the policy and both roles at
  19:36:59.525, applied seen 204 ms later; record changed to api Y -> `Y` created and `X` deleted 2 ms
  after the event and the controller replaced the role at 19:37:01.110 (1.4 s); record removed -> the
  grant deleted 2 ms after the event, both roles and `guest-gi09-synthetic` deleted at 19:37:02.705
  ("no declaration names it"), and `identity/oidc/role` listed nothing afterwards. Deleting a ComputeVM
  leaves no grant, role or policy (item 1's release readings). The same drive against a held guest
  (network gi9e, the guest asking for its requester's audience once a second): the stand-in sync took the
  record away at 19:41:03.5 and the guest's answers went 200, 200, then 403 from 19:41:04 on, until
  19:41:31, when the bidder's own 30 s read saw the requester's real records again and re-granted
  (200 from 19:41:31). Not read live: a firehose event for the collection (the wiring subscribes and
  calls `requesterRbacChanged`, which the stand-in runs used).

- **The guest's workload identity is issued by the bidder's OpenBao, on 2026-10-15 (plan 0009
  M3-GUEST-IDENTITY-OPENBAO).** The OIDC issuer that guest-kcp ran in TypeScript is gone with its signing
  key, JWKS, nonce store, rotation code, provisioning token, `/v1/oidc/prove`, `/v1/oidc/issue`, the RBAC
  gate and the host-key reporter; `oidc-issuer-hono`, `compute-provider-digitalocean`,
  `compute-provider-firecracker` and `compute-provider-local` left the workspace (nothing but the legacy
  providers used the issuer). The bidder handle's OpenBaoServer (`deploy/apps/market-bidder/vault.yaml`)
  now declares `identityOIDC` (key `guest`, RS256, `rotation_period` 24 h, `verification_ttl` 48 h) and the
  openbao controller keeps the key, every `identity/oidc/role` and one policy per guest in its declaration
  like its mounts (baosync collection `identity/oidc/role`, so a role no grant names is deleted). guest-kcp
  writes `identity/oidc/config` `issuer` = its relay's ingress URL when it is ready, so a token's `iss` is
  `<ingress>/v1/identity/oidc` (OpenBao appends that path to the configured issuer; the host and the bidder's
  ingress are the ones verifiers already trust) and the bid's `wif.simple` config names exactly that
  `issuer_uri`. guest-kcp is a path allow-list proxy with a token bucket per role, per token and for the
  public paths: `POST /v1/auth/<mount>/{bind,challenge,login}`, `GET /v1/identity/oidc/token/<role>` (the
  guest's own `X-Vault-Token`, role `guest-<computevm>-<16 hex of sha256(audience)>`), the discovery
  document and the keys; every other path is a 404 and `POST /v1/on-network` stays. A successful bind makes
  guest-kcp read the bound ed25519 key from `auth/sshsig/role/<computevm>` and write it as
  `status.market.hostKeys` (retried until recorded), which the bidder publishes as `vm.hostKeys`; the guest
  reports no other key type any more. The guest side is one script, `guest-login.sh` (cloud-init-common
  `guestLoginModule`): no argument binds once and logs in, `login` logs in again, `token <audience>` prints
  an identity JWT and logs in again when its token is refused; the secrets module and the on-network
  reporter call it, and `POST /v1/on-network` takes a JWT for audience `api://guest-kcp?actx=<computevm>`
  that guest-kcp verifies against the keys it proxies. What a guest may ask for is a new kind,
  `GuestIdentityGrant` (spec computevm, entity `guest-<computevm>`, api, actx, scopes, claims; status
  applied, role, audience, appliedAt, lastError; a name `<computevm>-<12 hex>`, an ownerReference to its
  ComputeVM, and the ComputeVM controller also deletes them when it releases the guest): the bidder writes
  one for `guest-kcp` at accept and one for each audience `api://<api>?actx=<did>` the requester's RBAC records name for the guest, carrying claim
  `wid_sub` = `actx:<bidder>:plc:<requester>:role:<role>`, and the openbao controller turns each into the
  role (client_id = the audience, a static claim template, ttl 5 m) and lists its token path in the policy
  `guest-<computevm>` that the guest's sshsig role carries as `token_policies` (the plugin's entity is named
  by OpenBao, so the policy is per guest rather than templated by entity name). The `sub` of an OpenBao
  token is the entity id, so a requester's authorizer (`secrets-oidc`) checks the claim `wid_sub` against
  the subject its grant names and still verifies `iss`, `aud` and the signature against the issuer's
  published keys. `spec.signingKeyRotation` of the MarketBidder now calls `identity/oidc/key/guest/rotate`
  (the bidder client holds that one path); OpenBao publishes the next key ahead of its use.
  Readings (market network, one bidder, test-0001, UTC): the
  secrets run (`TestRequestSecretsReachTheGuestThroughTheRFPFlow`) PASS in 234 s, `dev up` Ready in 87.7 s,
  the guest ran the command 74.4 s after `run request` started with the secret from `--secrets`, one
  host key reported, release `vm.delete` 200 and no ComputeVM, VM unit or work dir left. On a held guest
  of network gi9a: a token for the requester audience (kid f90616b7...), then GET of `/v1/sys/health`,
  `/v1/auth/token/lookup-self`, `/v1/identity/oidc/key/guest`, `/v1/sys/policies/acl` and
  `/v1/identity/oidc/role/x` through guest-kcp: 404 each, lookup-self with the guest's token: 404, a token
  for a role of another guest: 403. `kubectl get guestidentitygrants`: two grants, both APPLIED true, roles
  `guest-fc-6bede27ba239-11204dd5-0090f544101f70e6` and `...-09af04415a875a01`; the openbao controller
  logged `put-policy guest-fc-...` and the two role writes at 18:55:27Z. `signingKeyRotation: 1` patched
  at 18:56:08Z (appctl logged the rotate); the guest's next token had kid 8a0e9892... and its
  `setup-secrets.service`, restarted, fetched the secret again from the requester's server. The release
  at 18:57:01Z deleted both roles and the policy ("no declaration names it") and `kubectl get
  guestidentitygrants` listed none.

- **A deleted cluster is torn down completely, and a cluster of the same name gets new CAs, on
  2026-10-15 (plan 0015 M2-FRESH-CA-PER-CREATION).** Before this, `deleteCluster` left the members
  running and every byte of the cluster on disk: a Cluster created through the bootstrap path carried no
  finalizer, so the delete removed the object at once and nothing reaped it, and a Cluster created
  through `createCluster` was reaped but its data dir, its OpenBaoServer dir and its Postgres data stayed,
  so a new cluster of the same name was seeded with the old CAs. Now the provider's pass for a deleting
  Cluster (`internal/providermain` `one`) reaps the members, deletes the ClusterLinks, sets
  `spec.deleteData` on the Postgres object and deletes it (the Postgres controller removes the data
  after the unit stops), removes the cluster data dir and the OpenBaoServer dir
  (`SOCIALWEB_CLUSTERPROVIDER_VAULT_STATE_DIR`, defaulted by the gateway to its state dir), forgets the
  cached instance token, and only when the Postgres object is gone releases the ports and the subnet and
  removes the Cluster finalizer. The bootstrap Cluster now carries the finalizer. The identity bundle is
  bound to the Cluster `metadata.uid` by the file `cluster-uid` in the bundle dir (`internal/identitybundle`
  `BindClusterUID`): `GuardFor` writes it on the first start of a creation and refuses a start whose bundle
  names another uid with "identity bundle belongs to an earlier creation of this cluster"; a bundle with no
  uid is adopted once. Readings (market network `wf15ca`, one gateway, test-0001, UTC 2026-10-15): the
  bootstrap Cluster `tenant` Ready, client-ca.crt sha256 19:DD:23:7B..., a no-expiry kubeconfig worked
  (`get ns`); `deleteCluster` stopped every member, purged the data dir, the vault dir and the Postgres
  data, and removed the Cluster 14.8 s after the request; `createCluster tenant` came Ready with client-ca.crt
  sha256 63:5B:EE:F2..., and all 13 identity bundle entries differ from the deleted creation; the old
  no-expiry kubeconfig was refused by the client certificate front with "chain: x509: certificate signed by
  unknown authority" (the new vault holds no revoked serial, so the CA path alone refused it) while a
  kubeconfig of the new creation authenticated; a bundle copied back from the deleted creation made the
  restart fail before k3s with the bind refusal.
- **A deleted cluster completes after a core restart, and a failing delete step backs off, on
  2026-10-15 (plan 0015 M3-PROD-READING).** On prod, `deleteCluster` of clusters created before the last
  gateway upgrade was accepted and never finished: the Cluster stayed, its Postgres and OpenBao units stayed
  active, and the pass repeated "removed the ClusterLinks" and "deleted the Postgres object" every pass
  forever. Cause: the HostProcess supervisor stops the unit of a deleted HostProcess in its orphan sweep
  (`internal/hostprocess/supervisor.go` `Pass`), and the sweep only stops units this supervisor placed. A
  unit adopted after a core restart (same InvocationID, `Identify` false) was never marked placed, so the
  Postgres child HostProcess and the vault HostProcess, which carry no finalizer and vanish at once on
  delete, left their units running; the Postgres controller then waited for the server to stop forever and
  the Cluster finalizer never cleared. Now `markAdopted` marks every active unit of a supervisor-placed
  HostProcess as placed on each reconcile, a failed stop is logged, and the first pass of a new core still
  stops every unit that has no object (which also recovers a gateway already stuck). The deleting pass
  moved to `internal/providermain/teardown.go`: it logs the old "deleted" lines only when the step
  completed, reports the Postgres object's phase and message while the store is still there, retries
  freely for 6 passes, then backs off from 4 s doubling to 5 m, and writes Cluster condition
  `TeardownBlocked` (reason `TeardownStepFailing`) with the step, the attempt count, the next attempt time
  and the error. Readings (market network `cd1`, one gateway, test-0001, 2026-10-15): with the build before
  the fix, the bootstrap Cluster `tenant` Ready, the gateway core killed and restarted (all 10 member and
  Postgres units adopted with the same pids), `kubectl delete cluster tenant`: after 45 s the Postgres and
  OpenBao units were still active, `tenant` was still listed, and the journal showed the two lines every
  2 s. The fixed binary then replaced the core: its first pass logged "the HostProcess of a placed unit is
  gone; stopping the unit" for both, all units went inactive and the data dirs were removed, and the
  finalizer cleared 4 s later. Second cycle on the fixed binary: `create` Cluster `tenant` running, Ready
  in under 60 s with 10 units, core restart (10 units adopted), delete: units, data dirs and the Cluster
  were all gone 40 s after the request (finalizer cleared 22 s after it).
- **The bidder carries one compute provider, the kcp provider, and keeps its tombstones on the Bid, on
  2026-10-15 (plan 0009 M4-LEGACY-BIDDER-CUT).** `hono-factory-market-bidder-app` and `hono-bidder` no
  longer import `compute-provider-digitalocean`, `compute-provider-firecracker`,
  `compute-provider-local`, `market-bidder-contract-store-fs` or `market-identity-vault-file`; the
  options that fed them (`--compute-provider-digitalocean-*`, `--compute-provider-local*`,
  `--compute-provider-firecracker*`, `--contract-store-dir`, `--identity-vault-file`,
  `--oauth-session-file`, `--ca-cert-pem`) are gone from `cli-args-env.ts`, `deploy/apps/option-map.yaml`
  and `deploy/apps/market-bidder/bidder.values.yaml`. The packages stay in the workspace as libraries and
  the closure of `hono-bidder` no longer reaches them. The bidder's providers are `kcp` and
  `denoWorker`; a bidder without an OpenBao identity vault keeps its sign-in in memory. The one thing
  the file store did that nothing else did was the tombstone of an ended contract (a late `vm.delete`
  answers 200 `alreadyTerminated` from it). It moved to `Bid` `status.terminated` (`BidTerminated`:
  `receiptUri`, `receiptCid`, `acceptAuthor`, `reason`, `at`; Bid schema revision 3): the Bid whose
  `status.accept` is the contract's accept holds it, `createBidTombstoneStore`
  (`market-bidder-bids-kcp`) reads them back at start with the same 30 day TTL and 1024 capacity the
  file store had, and `market-bidder-contract-store-kcp` takes that store as its `tombstones`. A
  ComputeVM cannot hold the tombstone, because the contract store deletes the ComputeVM when the
  contract ends. Readings (market network, one bidder, `TestRequestSecretsReachTheGuestThroughTheRFPFlow`
  as the live run, test-0001, UTC): `dev up` Ready in 74.0 s and 77.7 s; `run request --secrets` exit 0
  in 67.0 s and 97.8 s, the guest ran the command 56.2 s after it started and held the secret; release
  answered `vm.delete` 200 and left no ComputeVM, VM unit or VM work dir; `dev down` left nothing. The
  bidder log of a fifth run: `bidder_contract_store` with `tombstones: "Bid status.terminated"` at
  18:21:47.152Z, the ComputeVM released at 18:24:09.448Z, and `bidder_tombstone_on_bid` (reason
  `deleted_on_request`, the Bid `bid-40376f5ec8ba61a87033`) 51 ms later; no `bidder_tombstone_*` warning.

- **An apply waits for its objects to be served, and adopts a schema that is already there, on
  2026-10-13 (branch `task/install-bringup`).** The first cold
  bootstrap of a kcp printed `kcpapply ... APIResourceSchema ... already exists` and
  `... is not served in root:socialweb-provider` and treated both as fatal, because discovery was run
  once and its answer was believed: an informer that had not caught up yet read as a kind that does not
  exist. `servedFor` (`internal/kcpapply/kcpapply.go`) now re-runs discovery until every object of the
  set is served or the Applier's `WaitTimeout` passes, logging the missing path, kind and attempt as it
  waits, and `applySchema` adopts an existing `APIResourceSchema` whose `spec` matches (raising
  `ErrSchemaChanged`, and naming the field, only on a real difference). Both are waiting on the
  apiserver to agree with a manifest that is already correct, so the fail-fast rule does not apply: the
  deadline is what fails, and it fails with what it was waiting for. The wider effect is that every
  apply path - `dev up`, `run request`, `run bidder`, the gateway - blocks up to that timeout instead of
  returning an error the caller had no way to act on. Readings were then taken on the
  host that had the incident, and they cover the two paths differently. Across every core restart since
  the change went live the same count is 0, and what those restarts exercise is the adoption: `applyOne`'s
  `Get` finds the live schema, so `applySchema` skips `Create` and compares `spec`. They cannot exercise
  `already exists` at all - that is raised only where the `Get` missed and the `Create` then hit an object
  that was there, which is the race the cold start runs into. Nor do they cover the wait, because a unit
  that kept its kcp never ran discovery against kinds it had never served. That path is the other reading:
  on a slot wiped to
  nothing - its kine store and its run directory removed,
  the kine, front and slot units stopped, one `install bidder` - the same count is 0: a kcp that had
  never served those kinds brought them up and answered `/readyz` on a new port.

- **A bidder takes its guest image from a published release, not from its own bytes, on 2026-10-13 (branch
  `task/install-bringup`).** The incident's bidder had no image at all, and the fetch that would have
  given it one was a 1.5 GiB download that printed nothing until it finished. The image is now fetched and
  verified during `install bidder`, with progress, and a build that has no release of its own still finds
  one: `guestimagerelease.Source` tries `releases/<version>`, then `main/<commit>`, then `main/latest`,
  then the newest `releases/v*` by version order. Each candidate's `image.json` is verified against the
  sha256 pinned for it in `internal/versions/versions.json` before the source is chosen, and the kernel
  and rootfs that source serves are verified against their own pins as they are fetched, so a wrong or
  stale release is refused rather than booted - at selection for the manifest, and at fetch for the two
  large files. That is what makes falling back safe. The progress was read on a terminal too, over a pty
  with the store set aside: 13 renders of one line, advancing from 17% to 100% and ending on a newline, with the
  line padded so the drop from `1024.00 MiB` to `1.00 GiB` at the unit boundary leaves no tail of the
  older line on screen. Reading taken on the host, with the stored source removed so the search had
  to run: a HEAD of the manifest key under the build's own `releases/` and `main/` prefixes answered 404,
  a HEAD of it under `main/latest` answered 200, the search resolved there, and the store it fetched held
  `firecracker-x86_64`'s three pinned files at their pinned byte counts (43 726 920, 1 543 503 872 and
  5 822) under a directory named the pinned manifest sha256. A candidate that answers anything other than 200 or 404 -
  a 403 on a prefix the bucket does not serve publicly, a 5xx - used to end the search there, which made
  the fallback unreachable on a bucket that keeps one prefix private; such a candidate is now recorded and
  skipped, and a refusal that finds nothing names the candidates that could not be read beside the ones
  that were absent. `main/latest` is a pointer, not a
  place: it tracks the newest pin, so a binary whose own commit is behind it finds nothing there but a
  mismatch and has to be given `--guest-image-source` to reach an image of its own vintage. A note in this
  tree from earlier the same day
  records `main/*` answering 403; re-probed before this reading was written, the bucket answers 200 to a
  listing of `main/` and of `releases/` and to a read of a named key, so that 403 is not reproducible now
  and is left as the open question it is rather than as a property of the bucket.

- **kine's sqlite runs with WAL, on 2026-10-13 (branch `task/install-bringup`).** A bidder's first kcp start lost its race with kcp's own bootstrap deadline
  on this store: kine logged `Slow SQL: INSERT INTO kine(...)`, one transaction took 717 ms, one storage
  write passed its 10 s timeout, and `etcdserver: mvcc: required revision has been compacted` repeated.
  The kine directory held `kine.db` and no `-wal` file while kine held a connection open. kine applies
  its own `DefaultParams` only when `DataSourceName` is empty, and this repository always passes a
  non-empty `sqlite://` path, so WAL was never on. `KineSqliteArgv` now appends kine's own five
  parameters (`_journal_mode=WAL`, `_synchronous=NORMAL`, `_busy_timeout=30000`, `_txlock=immediate`,
  `_stmt_cache_size=20`; kine's `cache=shared` is left out because it is not a key this driver reads -
  the driver (mattn/go-sqlite3, the version kine v0.17.1 pins) parses the query into its own settings
  and then drops it from the file name it opens, so only its real keys have any effect, and
  `_cache_size` is a different thing), in the sqlite builder
  `KineSqliteArgv`
  alone, because the postgres builder `KinePostgresArgv` goes through the same `kineArgv`. Compaction
  deliberately stays the apiserver's:
  `--compact-interval` defaults to 0, in kine's own flag set (`pkg/app/app.go`, kine v0.17.1, the version
  `internal/versions/versions.json` pins; the binary is installed, not vendored, so the flag appears
  nowhere in this tree), and kine's `Compact` is a documented no-op once it
  is positive - "manual compact is a no-op unless automatic compaction is disabled"
  (`pkg/logstructured/sqllog/sql.go`) - so setting it would move compaction to kine's own ticker, the
  reverse of the design in `docs/research/2026-10-11-vm-lifecycle/kcp-blue-green.md`. The recurring
  `mvcc: required revision has been compacted` warning is the apiserver compacting with a watcher behind
  it. The DSN is carried in the kine unit's generated environment file, not in the unit itself, and that
  file was written once and never updated - so a binary that changed a parameter could not reach a host
  that had already installed, which is the one thing a storage fix must not be. install now writes that
  file through the same byte comparison as a unit file and reports it, and the running kine keeps the
  DSN it started with until that unit restarts, which a reboot does: install must not restart the kine
  unit on its own, because the apiserver reading through it would lose its storage mid-flight - which is
  why the storage change lands on a host at its next start, and on a fresh install at once.
  Both instruments were then read on that host. First on the pinned kine binary started on the DSN
  install had just written: its store held `kine.db-wal` beside `kine.db` and `kine.db-shm`, and
  `pragma journal_mode` answered `wal`, while the store the *running* kine had opened held neither -
  which is the before reading for the paragraph above. Then on the bidder's own store: once that kine
  unit was restarted onto the rewritten environment file, its directory held `kine.db-wal` too.

- **The bidder unit is `Type=notify` with a watchdog, on 2026-10-13 (branch `task/install-bringup`; the
  readings this bullet names were taken on that host; a claim's reading is named where it has one - the
  watchdog's is the `SIGSTOP`, the apply deadline has none, as the paragraph there states - and a claim
  with no reading named is code-verified only).** A core that wedged - `applyRoot` retrying `ApplyRoot` forever and
  logging once a minute - looked alive to systemd for as long as it did not exit, and a unit that was
  `Type=exec` with no `WatchdogSec` could not tell. `applyRoot` is now the core's in-process member with
  a three-minute deadline inside `applyRoot`, measured from there and therefore after `ensure` has had the
  ready budget; failing that deadline ends the core with a
  non-zero exit for `Restart=always`;
  `internal/notify` sends `READY=1` once, after the core has applied its MarketBidder root object, so the
  unit is not started until that object exists and `enable --now` cannot return before then. Sending that
  readiness is still not the bidder's application phase: sign-in and the MarketBidder condition
  come later, so anything that depends on a SERVING bidder must wait on the kcp it serves, as `install`'s
  bring-up step does by re-proving the slot's `/readyz`. That step proves readiness only when the core
  published a slot port, a slot record with no active port is a refusal, and a handle skipped while
  another answers is a refusal too, because a partial bring-up is not the success the step claims. Only
  "no record at all" stays
  not-needed, and that case is not a failure because the install tests drive a fake systemd with no core
  behind it, so a fake-backed install can never serve `/readyz`. A re-run over a unit that is still
  starting waits for it to settle rather than skipping it, so a wedged core cannot be reported as a
  finished install. Measured on the host: a unit whose process was killed with `Restart=always` reports
  `activating/auto-restart` for the whole `RestartSec` gap, so `waitStarted` keeps waiting on the restart
  it is already tracking. For a unit whose process was killed with `Restart=always`, the only window
  that reads as ended is the instant between the process dying and systemd scheduling the restart, which
  is what `StartEndedGrace` of ended samples covers, so a re-run that lands in it does not report a
  converging core as finished with a failure. `WATCHDOG=1` comes from the
  core's own loop rather than a ticker of its own, because a private ticker would keep pinging while the
  core was wedged - refreshed at each tick, at every bring-up step, after each observed unit, on
  every readyz poll, and at every poll of a wait for postgres, for kine's socket or for a unit's bound
  port, so no wait inside a runtime `Ensure` can starve the ping. That matters because systemd pings twice per `WatchdogSec`, so
  what has to exceed the longest legitimate gap is half the value, not the value: the gap is now one poll
  of whichever wait the core is in - milliseconds - at every budget up to the ceiling, rather than the
  ready budget a blocking wait could previously have spent. A tick
  that spends minutes inside `Ensure` still refreshes it. The startup bounds hold by construction rather
  than by convention: install derives the unit's `TimeoutStartSec` from the effective ready budget - the
  flag or env value, else the one already in `config.json`, else the default - plus `BidderStartGrace`, the sum
  of the items the core must clear before `READY=1` that the ready budget does not already cover
  (`RootApplyDeadline` 3m, the instance-lock wait 2m, a stale-lease handover 12s), the rest of the
  six minutes - 48s - being margin, and
  derives its own start-job budget from that, so `startBudget()` > `TimeoutStartSec` > ready budget +
  `RootApplyDeadline` for any budget an operator sets, and both budgets have a ceiling
  (`MaxReadyBudget` 10m, `MaxNetworkBudget` 30m) that install refuses to go over: the budget sizes the
  unit's start timeout and its watchdog, so without a ceiling one mistyped flag would give a wedged core
  an hour before systemd restarted it, which is the hang this branch removed. The ceiling is checked
  against the value in effect, not against every value on disk, and a passing flag beats a stored one -
  so a host whose `config.json` holds an absurd budget is fixed by passing a sane flag once, which
  rewrites that key, rather than by editing the file by hand. install and the core read the same
  effective budget, so the two cannot disagree at install time; editing `config.json` afterwards by
  hand moves the core's budget without moving the unit's fixed `TimeoutStartSec`, and the way to change
  both is to run install again with the flag, which rewrites the key and restarts the unit. `CONFIG_PATH_SOCIALWEB_COMPUTER`
  is how the *installed unit* finds its `config.json` - its `Environment=` sets it - while the root
  install step reads `<state-dir>/config.json` itself, which is why a budget edited into that file by
  hand is read by the next install (and refused if it is over the ceiling) and a release base edited
  there is not. Raising `--ready-timeout` therefore raises the unit's timeout with it instead of inverting the
  order and letting systemd preempt the diagnostic. The template became `Type=notify` with
  `TimeoutStartSec` and `WatchdogSec` at that derived value (420s at the default budget) and `--fg`. That
  flag has no reader on the path the installed unit takes - `installedCore` is resident with or without
  it - and is pinned anyway because the standalone and in-network paths do read it, so a unit whose
  backend ever resolves elsewhere stays resident instead of exiting 0 into a restart loop. `RootApplyDeadline` stays at three minutes rather than following the ready budget: `applyRoot` starts
  after `ensure` has finished, retries its own `Patch` of the root object with a second between attempts, and hands each
  attempt whatever is left of the three minutes, so the window is up to about 180 attempts and the value
  is a judgement about how long a cold host may take to accept that object rather than a quantity derived
  from another. The failure it replaced was not a bound that was too small but no bound at all - the old
  loop retried for ever and logged once a minute, which is how twenty-five minutes passed in silence.
  Missing the deadline exits the core non-zero, systemd restarts it, and install prints the command that
  reads the reason out of the journal, because install reports systemd's own state rather than the log: the
  step table ends at the failing step with a `journalctl --user -u <unit>` line for the operator to run.
  What the value rests on is a measurement rather than a guess: on this host the root object's apply took
  `attempts=4, after=3s` on the coldest bring-up there was - the slot wiped to nothing - and
  `attempts=1, after=0s` on every warm restart, so the window is sixty times the worst apply seen here.
  Its *firing* is the one thing on this branch not read on a host, and the attempt to read it says why: freezing the front - which looked like the way in, because `applyRoot` reaches the slot
  through it while `ensure` reads the slot's `/readyz` directly - stops the core sooner than that, since
  the front also carries the writes the core's own controllers make (`kcpstore: write denopods status`
  fails, the core stops, systemd cycles it, `NRestarts` climbs). That measured `fail fast and be restarted`
  for the third time rather than the deadline. Reading it needs a front that answers `/readyz` and refuses
  the root object's `Patch`, which no host here can be put into without also breaking the store.
  That is the trade this branch made deliberately: a bound that is occasionally too tight and always
  visible beats a retry that is invisible for twenty-five minutes. `TimeoutStartSec` is finite on purpose: systemd
  arms the watchdog only once startup completes, so an infinite start timeout would leave a core that
  never sent `READY=1` in `activating` for ever - not killed, not restarted, and invisible, which is the
  original silent hang moved from `active` into `activating`. Cancelling a context would not have failed
  the core:
  `devcore.Run` returns nil on `<-ctx.Done()`, so the members channel is the path that exits non-zero.
  WatchdogSec is that same derived value rather than the thirty seconds first planned, because one tick runs
  `Observe` on every unit - each manager call bounded at 10 s by `hostsvc.DefaultCallTimeout`, the settle
  loop at `bootstrap.RestartSettle` = 3 s - and, when a unit is lost, a full `Ensure`, bounded by the
  ready budget (1 m by default). Instrument: `SIGSTOP`
  on the core is restarted by systemd and `NRestarts` grows. `SubState=running` under `Type=notify` is
  the readiness itself, because a core that never sent `READY=1` stays `activating`; `StatusText` stays
  empty and proves nothing, since the core sends `READY=1` and `WATCHDOG=1` and never `STATUS=`. That
  instrument was read on the host that had the incident: the unit ran with `WatchdogSec=7min`, `kill
  -STOP` on its main pid was followed by a new main pid and `NRestarts` 0 to 1 - `Result` then read
  `success` because systemd had already restarted it, which is the point: a core that stops answering is
  killed and restarted instead of lingering.
  The unit that waits for that readiness is only correct for the binary that sends it, so `install`
  refuses when `bin/current` is not the binary it was invoked as (`ErrBinaryMismatch`): the same unit
  started under a binary that never notifies stays in `activating` until the start timeout and then
  restarts for ever, which is the original silent hang in a new place. Switching a running install is
  `upgrade`'s job - it stages the new binary, self-checks it, keeps `bin/previous` and restarts the units -
  so install names that command and stops instead of writing a unit the host cannot start. A first
  install, and a re-run with the same binary, are unaffected. An install that rewrites `config.json`, a
  unit file, or a bootstrap drop-in now restarts what that rewrite reaches, because a running core reads
  `config.json` once at startup: without the restart the file says one thing and the process does
  another, which is the silent-ineffectiveness the branch exists to remove. `config.json` is one file
  every core unit this run names reads, so rewriting it restarts all of them; a unit file reaches its own
  template, so a bidder's `@.service` reaches every instance of it while a drop-in reaches only the unit it
  sits under, and adding a second handle starts that instance's unit and leaves the running one alone. A re-run that writes nothing restarts nothing. `upgrade` to a binary that is already
  `bin/current` still does not restart anything - a healthy core is not disturbed for nothing - and it
  now says so, instead of leaving an operator whose units are down with no next step. The guest-image step is bounded the same
  way: an image already in the store is verified against the pin and used, one that no longer matches is
  removed and fetched again, and the removal happens only after a source has been resolved, so a run that
  names no source leaves the store as it found it rather than deleting the only image it had. A caller
  that names neither a source nor a release base at all gets not-needed rather than a refusal, because
  that is what the install tests are - they drive a bidder with no image - and
  `Options.GuestImageBaseResolved`, which the CLI always sets, is what tells such a caller from an
  operator whose cleared base resolved to the empty string and is refused. The release base reaches the
  two sides by different routes and they are not the same value: install takes it from its own flag or
  env, defaulting to the bucket, and writes that key to `config.json` only when it was passed, while the
  core reads the key out of `config.json`. A base edited into that file by hand is therefore used by the
  running bidder and ignored by the next install, which is why the file is not the place to set it.
  A budget edited there is the other way round: install reads those out of the same file, so the edit is
  honoured, and refused if it is not a duration, if it is not positive, if it is over the ceiling, or -
  for the network budget - if it is under the thirty seconds the search of the release base needs. `check-host`, the last step,
  runs under `HostCheckBudget` rather than the signal-scoped context, so every wait an install makes has
  a deadline. The account-management and AppArmor commands these steps exec - `useradd` and `userdel` when
  installing and uninstalling the account, `gpasswd` and `apparmor_parser` either way - are killed after
  `CommandBudget` (a constant, not an option: a minute is already pathological for these) or as soon as
  the install's own context is cancelled, and their output is kept in the error, so a hung name service
  or a wedged `apparmor_parser` fails the step with what it printed instead of holding the install open.

- **The ssh gateway is an application of its own, and reaches guests over iroh with no --allow-run, on
  2026-10-12 (plan 0008 M1-SSH-GATEWAY-IROH proof, M4-RUN-GATEWAY-SSH).** `socialweb-computer run
  gateway ssh` applies a root object `SshGateway app` into the gateway's own kcp (with `--network`, the
  market network places that kcp and copies the network value and the bidders' DIDs onto the root
  object). Its set `ssh-gateway` renders an OpenBaoServer (OAuth sessions, the requester key and the
  app PDS key, never the state dir), the HostProcess `iroh-dial` and the hono-socialweb-computer-ssh
  DenoPod. Decided here, user may veto: the DenoPod spawns nothing. A guest keypair is generated in
  TypeScript (ssh2), and the iroh dial goes to a unix socket in the gateway's state dir where
  `socialweb-computer internal iroh-dial` runs `dumbpipe connect <ticket>` per connection, so the
  DenoPod has no `--allow-run` (the dumbpipe ProxyCommand still carries every session; it runs in
  the HostProcess). The DenoPod's env grant is a list (the service template renders `--allow-env=<list>`
  and `--ignore-env`, so the npm closure's stray reads of `process.env` see nothing). The requester evaluates
  gha-lite policies net-only, as the bidder does, because the full sandbox spawns deno. Readings,
  market network `sgnet` on hat-8, bidder handle `ssh-gateway-1.bob.test`: the gateway CLI under
  `--allow-run=dumbpipe` 17:26:07Z-17:26:44Z ran `echo SWC_GUEST_OK; hostname` in guest
  `fc-4a6d2f4f1847-1b49ba68` over `dumbpipe connect <ticket>` (exit 0, vm.delete 200); the same CLI
  with the env allow-list and the dialer socket, no `--allow-run`, 17:50:41Z-17:51:26Z, guest
  `fc-190fabcd2c04-89e62991`, exit 0, vm.delete 200; `run gateway ssh --network sgnet` on head ec8d56d4
  Ready at 18:14:20Z (listeners 127.0.0.1:44747 http and 127.0.0.1:33393 ssh, iroh-dial Running), the
  DenoPod argv `--allow-net --allow-env=<17 names> --allow-sys=hostname`, no `-A`, `--allow-run` or
  `--allow-ffi`; an ssh session through it 18:14:20Z-18:14:42Z ran in guest `fc-666488117425-373efffe`,
  exit 0, vm.delete 200; `dev down` left no process and no VM unit.

- **NetworkPolicies were not enforced on market nodes; fixed in the image builder on 2026-10-10.
  PROVEN ON THE LOCAL MARKET NETWORK, prod pending (plan 0005 M5-NETPOL).** Prod read over debug ssh at
  02:02Z: kube-router's `Aborting sync ... RULE_APPEND failed (No such file or directory)`. Root cause:
  the Fedora kernel execs `/usr/bin/modprobe` for every on-demand module load and the Ubuntu rootfs
  has modprobe only at `/usr/sbin/modprobe`, so `nft_limit` and `nft_log`, which iptables-nft 1.8.10
  needs for kube-router's `-j NFLOG -m limit` rule, never loaded (section 9's module derivation, "Why
  nothing loaded on demand"). Before: market network gate, image `sha256-51e20605`, `kernel.modprobe` =
  `/usr/bin/modprobe`, no `nft_limit`/`nft_log` in `/proc/modules`, every policy sync aborted, a
  deny-all ingress policy left the server answering for the full 2 min. After: image
  `sha256-9b6879e3` built by the fixed builder (`/usr/bin/modprobe -> /usr/sbin/modprobe` in the
  rootfs), `nft_limit` and `nft_log` loaded, zero aborted syncs, the policy refused the client 9 s
  after it was applied and deleting it restored the fetch. Not explained here: prod's
  `iptables -S` answering `iptables: Incompatible with this kernel` (iptables-nft maps errno 0 to that
  text); re-read it on prod after the rebuilt image ships.

- **The local substrates were retired, and the guest binary was split, on 2026-10-07. CLOSED (plan
  0005 M1-MARKET-ONLY and M1-AGENT-SPLIT).** Two things were true at once and each was the reason for
  the other. First, the clusterprovider carried three substrates and only one was used: `firecracker`
  and `qemu` were guests THIS host started, with their own VMM, network namespace, subnet lease,
  guest gateway, node image and identity disk, and `market` - a VM rented from the compute market and
  built by the winning bidder's cloud-init - had already replaced both as the production path. A
  second way to place a node is a second way to be wrong about where a node is, so `internal/nodeqemu`
  and the provider half of `internal/nodefirecracker` are deleted, `GuestDialled`, `GuestGateway`,
  `nodeVaultStage`, the per-node network allocation, `AssertGuestAdvertiseInvariant`,
  `dialFromAGuest`, the guest advertise invariant and the startup check that an image matches the
  configuration that names it are gone with them, and
  `SOCIALWEB_CLUSTERPROVIDER_SUBSTRATE` now accepts `market` alone: the retired names are refused with
  a message that says the day they were retired, because a name from before the retirement otherwise
  reads as a typo. What the guest half still needs is KEPT, in the packages the bidder builds and runs:
  `internal/nodevm`, `internal/nodefirecracker`'s image/manifest/namespace halves, `socialweb-nodeboot`
  and `socialweb-nodeimage`. Second, `socialweb-nodeagent` was one binary carrying both halves of a
  node's network - a host half (the `namespace` verb, run inside pasta) and a guest half (the `init`
  verb for the initramfs, and an `agent` verb that mounted the node's identity disk and exec'd k3s) -
  and on a market guest only the `init` verb can run: there is no identity disk, so the unit that ran
  `agent` exited `KCP_AGENT fatal` on every boot of every market guest, for ever. The binary is now
  `socialweb-nodeinit` with the `init` verb alone; `agent.go`, `audit.go`, `/kcp/kcpagent` and
  `kcp-agent.service` are deleted from the image, and the image configuration's `agent_package`,
  `agent_binary` and `agent_go_flags` are `init_package`, `init_binary` and `init_go_flags`, refused
  under the old spellings rather than accepted. The image fingerprint changes with the input's name
  and content, which is the point: an image's name is a hash of what its build read.
  `cmd/socialweb-rig-inputs` and `deploy/test-rig` existed only to hand the local substrates their
  `/dev/kvm`, `/dev/net/tun`, two images and a live gate, and were deleted with them; the market test
  market network (`deploy/market-network`, `internal/marketnetwork`) is what proves a node end to end now,
  and it proves it through the RFP flow this project exists for.

- **A cluster's kubeconfig mount child is written from the Cluster CR's `status.URL`, by a kcp system
  privileged identity, on every reconcile pass. CLOSED TWICE: first a mount that served kcp itself,
  then a mount nothing could write at all.** The first defect: `kcpclient.Ensure` POSTed the Cluster CR
  and then POSTed the mount child at `MountPath` back to back, both without reading anything the CR
  reports. kcp's `workspaceSpecUpdater`
  (`kcp/pkg/reconciler/tenancy/workspacemounts/workspacemounts_reconcile_updater.go`) copies the
  MOUNTED OBJECT's `status.URL` into the workspace's `spec.URL`, and returns
  `reconcileStatusStopAndRequeue` with `unable to read .status.URL` when the object has none; a mount
  workspace's own `status.URL` is empty by design. The CR's `status.URL` is written later, by the
  clusterprovider's `Joined` (`internal/clusterprovider/provider.go`: `o.Cluster.Status.URL = at.URL`),
  so a child created in that gap inherited nothing. The requeue is rate limited by the controller
  (`c.queue.AddRateLimited(key)`), so the healing pass recedes rather than arriving promptly, and in
  production it did not arrive at all in the observation window. kcp then served the mount ITSELF:
  `kcp-admin` was accepted, `kubectl get nodes` returned the placeholder Node the provider wrote, and
  every object on the real tenant apiserver was invisible. Downstream, the SPA/runner kubeconfig
  showed an empty cluster and the clusterprovider re-claimed a node from the market every 2 s because
  it never saw the join. MEASURED in production on `root@socialweb.computer` (ssh port 1997):
  `kubectl --kubeconfig=/var/lib/socialweb/kcp/admin.kubeconfig
  --server=https://127.0.0.1:6443/clusters/root:<account>:<cluster> get workspace clusters -o
  jsonpath={.spec.URL}` was empty on the original child; after deleting and recreating that child by
  hand the same read gave `https://127.0.0.1:20003`, and traffic then reached the tenant apiserver,
  which began asking for credentials -- the tenant k3s refusing kcp's admin token, the correct
  refusal. That fix moved the write into `Ensure` (`ensureMountChild`), where a CR that reported no
  `status.URL` was waited for (30 s, `clusterURLTimeout`) and an existing child with an empty
  `spec.URL` was PATCHed.
  The second defect, found by the acceptance run on 2026-10-08 (plan 0005 M6-SPA-CREATE), is that this
  writer is the WRONG IDENTITY and sits in the WRONG PLACE. MEASURED in production: kcp answers
  `workspaces.tenancy.kcp.io "clusters" is forbidden: spec.URL can only be set by system privileged
  users` to a create or a patch of that field from `kcp-admin` -- the user of the `root` context of
  `$KCP_ROOT/admin.kubeconfig`, which is what `SOCIALWEB_KCP_KUBECONFIG` names and what the gateway,
  the provider and every read in this repo use -- and the same object created with
  `--context system:admin` (kcp's `shard-admin`, context `system:admin`, cluster
  `.../clusters/system:admin`; kcp writes both contexts into the one admin kubeconfig it generates)
  succeeded and the mount served at once. So a `getKubeconfig` or `listClusters` on a cluster whose
  child was missing failed the whole request, and the 30 s wait for `status.URL` inside the create
  spent the door's entire 30 s budget: the abort then landed before the gateway wrote anything else.
  THE DECISION, and it is a decision about identity: **`spec.URL` on a mount child is written by one
  named client of its own, and by nothing else.** `kcpclient.MountChild`
  (`internal/kcpclient/kcpclient.go`, `NewMountChild`) exposes exactly one method,
  `EnsureMountChild(ctx, home, name, url)`, which CREATES the child at
  `<home>/clusters` with `spec.URL` + the `Cluster` mount ref, or PATCHes `spec.URL` when the child
  exists with an empty one, and refuses an empty `url` outright (`ErrNoMountURL`) rather than writing a
  child kcp will never address. It is built from the kcp context
  `SOCIALWEB_CLUSTERPROVIDER_MOUNT_CONTEXT` (default `system:admin`; `kcpclient.ShardAdminContext`,
  loaded by `kcpclient.ConfigFromContext`) in `cmd/socialweb-clusterprovider` (`mountChildWriter`),
  and the provider refuses to join without it (`cmd/socialweb-clusterprovider` `ErrNoMountChild`).
  EVERY OTHER kcp call keeps the unprivileged identity: `Options.Host` overrides the config's server,
  so the context contributes the credential alone. The writer is called from the reconcile pass that
  already reads every Cluster's status (`Provider.addressMount`, `internal/clusterprovider/provider.go`),
  right after the status write, so the child is created -- or repaired -- as soon as the pass publishes
  a `status.URL`; a refused write FAILS the pass (`pass.Err`) rather than reporting a cluster whose
  kubeconfig reaches the wrong apiserver. `kcpclient.Ensure` now stops at the Cluster CR: it never
  reads the CR back and never waits for `status.URL`, because a market cluster's node is minutes away by
  construction (section 13 invariant 31) and a create path that polls for it spends its caller's whole
  budget (this deleted `ensureMountChild`, `waitForClusterURL`, `clusterURLRead` and
  `clusterURLTimeout`). On the gateway side the same defect closed the other way round:
  `gateway.Server.CreateCluster` records `spec.running` FIRST, on a context the caller's hang-up cannot
  cancel (`context.WithoutCancel`), and then reports the phase it reads back -- `Initializing` is a
  phase in a 200 response, not a refusal -- because `running=false` after a create is a cluster no pass
  ever brings up. The port a cluster's apiserver answers on is leased per cluster and not per instance
  (`portallocator`: `leaseKey{cluster, purpose}`), so the URL written once stays right across an
  instance bump; that is why the repair is for an EMPTY `spec.URL` and not for a mismatch.
  Instruments, all in `internal/kcpclient/kcpclient_test.go` unless named:
  `TestTheMountChildIsWrittenByItsOwnWriter` (four subtests: an existing child with an empty
  `spec.URL` is PATCHed from the CR's URL and NOT re-created; a missing child is created WITH the URL
  and the mount ref; three passes create it exactly once and patch nothing once it carries the URL;
  an empty URL is refused and writes nothing), `TestEnsureStopsAtTheClusterCRAndNeverWaitsForAURL`
  (Ensure POSTs no child and reads the CR back zero times), `TestEnsureCreatesTheTree` (seven
  requests, no child), `TestAConfigFromAContextIsThatContextsIdentity` and
  `TestAContextThatIsNotThereIsRefused` (`internal/kcpclient/context_test.go`), and
  `TestAPassAddressesTheMountChildOnceTheCRReportsAURL`, `TestEveryPassRepairsTheMountChildItFinds`
  and `TestAMountChildThatCannotBeWrittenFailsThePass` (`internal/clusterprovider/provider_test.go`).
  The Ensure-side subtests and `TestEnsureCreatesTheTree` were watched to FAIL against the old
  create-on-read wait (`Ensure POSTed the mount child 1 time(s)`, `Ensure read the Cluster CR back N
  time(s)`) and the provider-side three against a stubbed `addressMount`, so the readings are the
  change and not the ambient result. KNOWN LIMITS: a pass that reports `pass.Foreign` (an apiserver
  answers at `status.URL` and this process holds no group for it) writes no mount child, because such
  a pass writes nothing at all; and in `join` mode (`SOCIALWEB_CLUSTERPROVIDER_JOIN` naming one
  cluster) only that cluster is reconciled, so a cluster created in the SPA gets its child from a
  `join-all` provider -- `scripts/deploy.sh` sets `SOCIALWEB_CLUSTERPROVIDER_JOIN=*` (line 97) with
  `SOCIALWEB_CONTAINER_BOOTSTRAP=0`, which is the shape the plan's production reading requires: the
  CR reported a `status.URL` and only the child was missing. NOT VERIFIED against production:
  this is a source change the owner deploys, and the two live-gated mount proofs
  (`internal/kcpclient/mount_child_live_test.go`, `internal/clusterprovider/live_mount_test.go`) and
  `internal/kcpclient/live_apiserver_test.go` were updated to drive `EnsureMountChild` from the
  `system:admin` context but were NOT RUN. `NEXT.yaml` MOUNT-1.


- **A market cluster's tenant credential is signed from its own ClientCA, and the choice of source
  belongs to the substrate. CLOSED, and it closed the k8s door.** Before, `MintTenantCredentialForDataDir`
  chose its source from `ReadVaultSeal(dataDir)` alone: a port and a root token meant the vault, and
  nothing asked where the cluster's nodes ran. A market cluster therefore minted its tenant credential
  through its own custody vault over TLS on every clusterprovider pass, and the tenant never
  materialised. MEASURED in production, container `socialweb-k8s` on `root@socialweb.computer`, on
  every 2 s pass: `cluster provider: the pass failed ... no tenant credential for
  test-cluster-alice-0001: the tenant credential could not be signed: issuing the tenant credential
  from the vault at https://127.0.0.1:20001: openbao: transport failure: POST
  /v1/pki/issue/tenant-superuser: tls: failed to verify certificate: x509: certificate signed by
  unknown authority (possibly because of "x509: ECDSA verification failure" while trying to verify
  candidate authority certificate "socialweb-vault-tls-ca")`. The scale driver observes the tenant
  through this credential, so with the pass failing no tenant was ever observed, a pending pod wrote
  no NodeClaim, and no node was rented. The material the credential needed was on disk the whole time:
  `server/tls/{client-ca.crt,client-ca.key,server-ca.crt}` exist for that cluster, and the provider
  wrote them. After, `CredentialSource` is an explicit argument to `MintTenantCredentialForCluster`
  and `MintTenantCredentialForDataDir`, `internal/providermain/main.go` derives it from the
  substrate, and an unnamed source is refused rather than defaulted. `market` signs from the ClientCA
  and never opens the vault; `firecracker` and `qemu` are unchanged and still reach for the vault when
  a seal exists. The refusal-not-fallback rule is unchanged on both paths. Instruments, all in
  `internal/clusterprovider/tenantcredential_test.go` unless named: `TestTheCredentialSourceFollowsTheSubstrate`
  (`internal/providermain/market_test.go`) reads the derivation for all three substrates;
  `TestTheMarketSourceSignsFromTheClusterCAAndNeverReachesForTheSeal` seeds a ClientCA beside a seal
  whose port nothing answers at, and it was watched to FAIL with the production error shape above
  (`the tenant credential could not be signed: issuing the tenant credential from the vault at
  https://127.0.0.1:<port>: openbao: transport failure: POST /v1/pki/issue/tenant-superuser: ...
  connect: connection refused`) when the market source was made to fall through to the vault, so the
  reading is the change and not the ambient result; `TestTheMarketSourceRefusesWithNoClientCAWhateverTheSealSays`,
  which refuses a ClientCA-less data dir with the seal present and again with it removed and requires
  the two refusals to be the same one, so the seal is shown to change nothing; and
  `TestTheCredentialSourceMustBeNamed`. The guest half is `TestTheVaultIssuedTenantCredentialChainsToTheImportedClientCA`,
  unchanged in what it proves and now naming `CredentialSourceVaultIfSealed`. NOT VERIFIED against the
  production deployment: the container was inspected read-only, and this is a source change the owner
  deploys. `NEXT.yaml` MARKET-2.

- **The live gate now runs end to end in the rig, and it derives the tenant it runs against. CLOSED,
  and the derivation is what closed it.** `deploy/test-rig/inside.sh` used to refuse before the gate
  because eight inputs "the product cannot derive" were unset; that premise was wrong. The container
  provisions the tenant those inputs describe, so `cmd/socialweb-rig-inputs` now reads them out of the
  running product: it lists the account workspaces and Cluster CRs through the kcp admin kubeconfig at
  `SOCIALWEB_KCP_KUBECONFIG` and takes the account DID and the cluster name from the CR, the tenant
  admin kubeconfig from `clusterprovider.ClusterDataDir(stateDir, cluster)/server/cred/admin.kubeconfig`,
  the registry workspace from `SOCIALWEB_GATEWAY_DEVKEY_REGISTRY` and the registry namespace the gateway
  itself uses (`default`), and the granted `did-*` ServiceAccount from the tenant grant's token rule.
  Nothing is fabricated: a value whose source is absent is left unset and the preflight names it. The
  rig also removes its container and volume on exit now, including on failure and interrupt, with
  `KCP_RIG_KEEP=1` to keep them for debugging; before this it leaked `socialweb-rig` for eleven hours.
  Running it exposed four real defects, each fixed at the root rather than in the test:
  (1) `container-entrypoint.sh` named the account workspace and the home independently of the DID, so
  the cluster it created at `root:socialweb:live` was unreachable at the product's canonical path
  `AccountHash(did):cluster` and carried no deny record -- `kcpclient.HomePath`, the gateway's whole
  read path and the live proofs all use that path. It now names the account workspace `AccountHash(did)`
  and the home the cluster, writes the account's `AtprotoIdentity`, and defaults the DID to
  `did:web:example.com`, the DID the shipped tenant grant scopes, so the account's ServiceAccount and
  the grant's `resourceNames` are one name.
  (2) the gateway's PLC sweep revoked the very account the deployment had just provisioned -- a `did:web`
  whose document 404s reads "the account is gone" -- so the bootstrapped record is labelled
  `socialweb.computer/sweep=exempt` and `sweepkcp`'s worklist skips it. Pinned by
  `TestASweepExemptIdentityIsNotSwept`, which failed before the skip existed.
  (3) the tenant grant named a `did-*` ServiceAccount nothing created, so the mint proof's TokenRequest
  404'd; `clusterprovider.Bootstrap` now creates the ServiceAccounts the grant's token rule names,
  pinned by `TestBootstrapEnsuresTheServiceAccountsTheGrantNames`.
  (4) `portallocator`'s cross-process helper was also a top-level `Test` that skipped when it was not the
  child, and a skip is not a pass, so the gate failed on it; the child body moved into `TestMain`, which
  prints no skip.
  And one live proof was unrunnable as written: `TestTheNotFoundDiscriminatorAgainstALiveApiserver`
  drove the tenant through the kcp mount with the kcp admin kubeconfig, which the mount does not
  authenticate (the mount forwards a BEARER header to the tenant and nothing else), and it hardcoded a
  `did`/`cluster` no run provisioned. It now takes the deployment's derived DID and cluster and issues a
  tenant bearer through a short-lived probe ServiceAccount and token Secret, then deletes the
  DID-derived SA twice so the 404 discriminator is exercised for real. MEASURED 2026-09-25 on this host,
  16 cores, `deploy/test-rig/run.sh`: the product stack reached Ready in 30 s; the gate command
  `SOCIALWEB_REQUIRE_LIVE=1 go test ./... -count=1 -v` inside the rig printed **736 PASS, 0 FAIL, 0 SKIP**
  and `PROOF: the live gate passed with no skips`. Per test: the four-step proof PASS in 234.18 s
  (firecracker 116.41 s, `INSTANCE_START_TO_READY_S=5.452`; qemu 117.77 s, `=7.424`), the subnet proof
  PASS 5.531 s (alpha) and 5.220 s (beta), the live firecracker join PASS 16.03 s, the audit PASS
  32.67 s, the gateway-answered-on-its-own-link PASS 2.68 s, the system-pods PASS 116.54 s, the live
  qemu boot-and-reap PASS 2.10 s, the mount-shutdown PASS 32.03 s, and every vault and ESO test PASS.
  The four derived-tenant proofs PASS: the renewal path 0.08 s, the mint credential 0.01 s, the live
  bootstrap 0.02 s, the mount child 0.03 s. `deploy/check.sh --full` on the HOST still cannot pass and
  this is not a defect: the live artefacts (the kcp admin kubeconfig, the tenant kubeconfig and the
  node images) live in the container's state volume, so the rig is the gate and `check.sh` is the
  host-loop.

- **The advertise address is an ALLOCATED value, and this entry was wrong twice. WRONG, not open.** It
  asked whether the substrate's advertise address is "a tap/bridge address created once by the operator
  or an existing host address", and what it costs in exposure. There is no such substrate to choose a
  value for: `firecracker` is the only name `nodeSubstrateFor` builds and every other name is refused,
  and `--advertise-address` is REFUSED outright for a guest rather than accepted and ignored, because it
  cannot be in force. The value is the first usable address of the range the cluster holds, allocated
  from `internal/subnetallocator` - no flag, environment variable or config file can pick a cluster's
  range, because a range two operators can set differently is the same collision in a tidier place. Its
  second claim is refuted by the code too: `--bind-address` does NOT equal the advertise address. It is
  pinned to loopback, and the readiness gate dials the address the component was told to LISTEN on, so
  a guest-reachable advertise address is not a guest-reachable listener and the exposure question is
  moot. That is what the entry was reaching for when it recorded the rule; the rule is now the guest's
  own and INVERTED, because a guest reaches NONE of this host's addresses, so `AssertGuestDialAddress`
  refuses an address this host carries. What survives from the entry is its measurement: the deleted VM
  substrate owned a constant, `192.168.5.2`, which this host does not carry and which nothing answers,
  and the store DSN built from it never connected. The one measurement still live is the apiserver
  rejecting a loopback in-cluster `kubernetes` Service endpoint - `subsets[0].addresses[0].ip: Invalid
  value: "127.0.0.1": may not be in the loopback range` - and the leased gateway is the address that
  satisfies it.
- **`nodeExpiry` has a producer at BOTH ends now. CLOSED - and it was closed after this entry was
  written, which is why it is here rather than in section 14.** All three pieces the entry called
  unwritten were written later the same evening it was written: `Registry.WriteNode` now carries a whole
  `v1alpha1.NodeStatus` rather than a phase, so the per-node report has a channel; the node recorder
  writes `Node.status.nodeExpiry` from the substrate's own report, and a substrate with no door reports
  an explicit `0`, so a local cluster is deletable; and the `Cluster` value IS the roll-up - the minimum
  over live nodes with `0` counting as infinity, `Draining` nodes skipped, the empty set as `0`, one
  live node absent as absent - performed by `NodeExpiryRollup`, wired as `Options.NodeExpirySource` and
  re-derived every pass rather than only when a node changes. The entry's last sentence is the part that
  is still true: the door's own report was already parsed (`internal/sessionreport`), so what remained
  was the two writes - and `sessionreport`, that parser, was reached by nothing, which is why it is
  DELETED rather than wired (this section, the unwired controls). The door stays out of scope (section
  16), so its report surface has no parser and needs none.
- **The identity bundle is seeded and harvested at `<DataDir>/server`, and the fingerprint survives a
  rebuild - measured.** `identitybundle.Harvest`/`Seed` are called from
  `clusterprovider.HarvestBundle`/`SeedBundle`, both rooted at `<DataDir>/server` - the same root
  `clusterstack.TLSDir` puts `tls/` under - so a seed lands where k3s looks. `Seed` runs from the
  Identity guard, which rides the step BEFORE k3s and only when the datastore already holds a
  bootstrap row; `Harvest` runs after k3s's Ready gate. `bundle_test.go` pins the root itself: seed,
  then assert every entry sits at `clusterstack.TLSDir(dataDir)`.

  This bullet replaces one claiming the bundle "has no persistence", which was not merely stale but
  **self-contradicting**: the same file already listed `Harvest`/`Seed` as wired, and the claim was
  added in the very commit that added `bundle.go` and both callers. A gap list can be wrong in the
  CLOSING direction, and that one was - which is why this section is now re-read against the code
  rather than trusted.

  Measured on the process substrate, one uninterrupted test, three runs: cold (fresh store and fresh
  dir) **4.97-6.98 s**, pre-seeded (warm store, `server/` wiped, bundle seeded) **1.134-1.139 s**, and
  a CONTROL rebuild with the bundle removed, which must author its own CAs, **1.128-1.378 s**. The CA
  fingerprint was byte-identical across a teardown and rebuild in every run; the control's was not.
  The bundle's value is the identity it preserves, not a time saving - see the timing block in
  section 9 for the attribution this refutes.
- **`status.URL` is written while `Initializing`, and that is LOAD-BEARING, not sloppy.** The URL is
  deterministic from the lease, so the first pass publishes it with the intent; the provider's next
  pass PROBES that URL to decide `Reachable`, and a URL withheld until the apiserver answered would
  mean nothing to probe and a cluster that never leaves `Initializing`. So the two fields must be read
  together and neither is redundant: **`status.URL` says where it will be, `status.phase` says whether
  it is there.** The hazard is real but belongs to the mount, not to this writer: kcp's mount gate
  reads the phase (above), so a loosened gate would expose an unprobed port - which is the argument
  for keeping the gate rather than for withholding the URL.
- **`Joined` remains test-only.** Deliberately: with the group live the second pass's `on()` path
  reaches `claimReady` through the decider, so this substrate needs no external join to answer. The
  external-join path is reachable only from `provider_test.go`.
- **A guest JOINS, so time-to-usable-node is measured: 7.4, 10.6 and 9.5 s.** A k3s agent inside a
  firecracker guest registers as a Ready node against the host's real `k3s server --disable-agent`,
  host-timed from `InstanceStart` (section 9). The two blockers this entry used to attribute to "the
  prototype harness's own" were NOT harness bugs, and the correction matters: one was an environment
  fact (`pasta`'s child inherits the `pasta_t` domain, which denies CAP_NET_ADMIN and silently
  discards the error text through a pipe it cannot write) and one was an architecture fact (one
  writable rootfs serving as both template and instance, which persisted a guest's writes into the
  next boot). Both are fixed, and both would have recurred in production because neither was in the
  harness. **The composition-root join is now established too**, and it is a test rather than a
  scripted run: `internal/providermain/live_fourstep_test.go` holds
  `TestTheFourStepProofRunsThroughTheCompositionRoot`, which lives in the package that owns the
  composition root, takes the substrate as a VALUE rather than reading it from the environment, and
  derives all four transitions from scratch. It skips until seven documented variables name the store
  the builder published into, the builder's own configuration that names the image in that store, a
  firecracker binary, the guest agent, k3s, kubectl and an image the guest already carries
  - the last one because the guest reaches no registry, so the tenant image cannot be derived from
  anything the test can see, and requiring it is honest where guessing would produce a test that
  fails for a reason it does not state. **It has now run, and it passes**: `PASS` in 99.48 s, with
  `INSTANCE_START_TO_READY_S=9.749` host-timed from firecracker's `InstanceStart` to the Node's
  `Ready` condition, and the tenant pod pulling NOTHING (`Container image
  "rancher/mirrored-pause:3.10.2" already present on machine`), which is the sealed-image property
  holding inside the objective's own proof rather than only in the substrate test.

  **It found two bugs in itself on first execution, and both are the same lesson.** `kubectl get
  nodes --no-headers` reports an empty list on STDERR, so a harness that combines the streams counts
  `No resources found` as one node - which meant the "empty cluster" step could never pass, and the
  first run died on it after 362 s. And step 4 had been logging "the control plane still answers" on
  the strength of a reconcile returning no error, which establishes nothing about the phase or the
  cluster's own objects; it now requires the same `Ready` phase step 1 reached AND reads the
  namespaces back. A guard that cannot fail the way it claims to is the failure this whole document
  keeps rediscovering, and it was found only by running the thing.

  One thing the guard deliberately does NOT depend on: the k3s add-on pods sit at `0/1` with
  restarts while the guest console is CLEAN on every firewall signature, so that is not the module
  list. `Intent` stamps those pods from their add-on ReplicaSet, so they never hold the node - which
  is exactly the objective, and step 4 proves it. **Pod readiness and the objective are different
  questions, and a green guard is not a working cluster DNS.** The same join IS now measured through
  the composition root, by the four-step proof below, with a tenant `Cluster` CR, real leases and a
  minted tenant client; the sentence that used to stand here said the two had never been measured
  together, and it had already been overtaken by the run this entry records.
- **The 332 s boot was fabricated, and the real number is 0.300 s - and this entry's evidence is in
  HISTORY, not in the tree.** The claim is right and its citation does not open: `run/initrd/init` was
  never committed to this repository at any point, and the artifact that measured it was deleted by the
  commit that retired `experiments/`, once `internal/` had come to implement it. What that artifact
  recorded, and what is recorded here instead of a path nobody can open: `init` line 10 is a literal
  `echo Boot took 332.33 seconds` with no clock read and no computation, printed 0.29 s after
  `InstanceStart`, so it could not have been a measurement. An Ubuntu guest with systemd reaches a login
  prompt in **1.401 s**, host-timed from firecracker's own `InstanceStart` rather than from the guest's
  own clock - the same rule invariant 27 states - and that number survives only in the same deleted
  file. Boot was never the problem, which is worth stating because a fabricated measurement sent this
  work after a 30x non-issue.
- **The resolver gap is CLOSED and verified, and the entry that recorded it argues with itself.**
  Measured in the image template: `/etc/resolv.conf` is EMPTY - one byte, a newline - so the guest has
  no nameservers, which is exactly what `lookup registry-1.docker.io: Try again` is, and every pod
  sandbox fails because the `pause` image cannot be pulled. The reason it hid so well is that
  registering with the apiserver needs no DNS, so the node reaches `Ready` and the four-step proof's
  third step looks much closer than it is. `pasta --config-net` configures DNS in the NAMESPACE and
  cannot reach a file inside the guest's rootfs, and pasta proxies DNS at the gateway, so the fix is
  for the guest's agent to write its own resolver pointing at the gateway address it is already given.
  That write is implemented, on the production guest path rather than behind a flag: the agent's
  `configureResolver` writes `/etc/resolv.conf` from the gateway, and it is called unconditionally as
  the guest's agent starts. It is verified rather than assumed, in the four-step proof below: the guest
  console carries `KCP_AGENT_RESOLV=nameserver 192.168.0.1`, and a busybox image was pulled and started
  on that node, which is impossible without a working resolver.

  This entry used to say, in one sentence, that nothing in `cmd/socialweb-nodeagent`,
  `cmd/socialweb-nodeimage` or `internal/nodefirecracker` configures a resolver at all - and then, three
  sentences later, that the write was implemented and unverified. The premise was never updated when the
  conclusion was, so the same entry claimed the gap was absent and present at once, and the "unverified"
  half outlived the run that verified it. Both halves are recorded here because that shape is worth
  recognising: a premise kept while its conclusion moves is how a document ends up arguing with itself,
  and the next reader cannot tell which sentence is the current one. What is still true of the original
  disjunction is that the image builder does not write a resolver: the template ships the empty file on
  purpose and the guest's agent replaces it at boot.
- **The reconciler's readiness gate dialled an address nothing was listening on, and this is the
  failure shape that hides best.** Measured: the gate dialled the ADVERTISED address while the
  apiserver binds loopback, and from this host the advertised address is refused. So the gate could
  never pass: every pass blocked in `Spawn -> gate -> waitK3s` for the gate timeout (~91 s) and then
  reaped and respawned the process group it had just started, on a loop. That presents as "the
  cluster is Initializing" for ever, which reads as a slow component rather than as a wrong address.
  Fixed by dialling the BIND address. The general lesson: a readiness probe must dial the address the
  component was told to LISTEN on, never the one it was told to ADVERTISE, and the two are different
  values here by design.
- **The four-step proof is COMPLETE, measured through the composition root.** Against real kcp, with
  the substrate chosen by configuration (`SOCIALWEB_CLUSTERPROVIDER_SUBSTRATE=firecracker`), in one
  uninterrupted run:

  1. the control plane reaches `Ready` at its leased apiserver port with zero nodes, and
     `TestTheControlPlaneReachesReadyAtZeroNodes` drives that against real leased ports, real kine, a
     real per-cluster postgres and a real `k3s server --disable-agent`, in under three seconds;
  2. `get nodes` is empty;
  3. a node joins ON DEMAND - the scale decision made it from the tenant cluster's own intent
     (`action=Provision, nodes=1, "intent and no node"`), never from an operator; the guest
     registered as `liveweb-node1 Ready` with `INTERNAL-IP 192.168.0.10`, kernel
     `7.2.7-200.fc44.x86_64`, `containerd://2.3.4-k3s1.36`; and a pod ran on it
     (`docker.io/library/busybox:1.36`, pulled inside the guest). **`INSTANCE_START_TO_READY_S=9.609`**,
     host-timed from firecracker's own `InstanceStart` in `instance.json` to kubelet's node-Ready
     transition;
  4. it returns to zero: the decision goes `Teardown` past the hold and then `None`, `get nodes` is
     empty, the guest and its data dir are gone, and the control plane STILL ANSWERS - `/readyz` ok,
     namespaces still listed.

  The resolver gap is closed and verified rather than assumed: the guest console carries
  `KCP_AGENT_RESOLV=nameserver 192.168.0.1`, and the busybox image was pulled and started on that
  node, which is impossible without a working resolver.

  What skips by DEFAULT is `TestTheFourStepProofRunsThroughTheCompositionRoot`, and it was not weakened
  into a pass: it skips until seven documented variables name the store the builder published into, the
  builder's own configuration that names the image in that store, a firecracker binary, the guest
  agent, k3s, kubectl and an image the guest already carries. This entry used to attribute that to a
  test named `TestANodeJoinsAndTheClusterReturnsToZero`, which does not exist - it was deleted, with the
  file that held it, six hours and 109 commits BEFORE this entry was written, and its name survived only
  in this document. The description was of the right test; only the name was stale, which is the harder
  kind to notice, because a reader who greps for it finds nothing and concludes they are looking at the
  wrong branch.
- **Intent is the tenant's workload, not anything running - and getting this wrong made the objective
  unreachable.** `scale.Intent` counted every pod `Pending`, `Running` or `Unknown`, and a k3s cluster
  always holds its own coredns, metrics-server and local-path-provisioner: `Pending` at zero nodes,
  `Running` at one, so intent was true in BOTH directions. An empty cluster therefore provisioned a
  node it had no reason to want and never released it, which presents as a scale bug rather than as an
  intent bug. The baseline was measured rather than assumed, on k3s v1.36.4+k3s1 with nothing applied
  through kcp: namespaces `default`, `kube-node-lease`, `kube-public`, `kube-system`, `users`; three
  Deployments (coredns, local-path-provisioner, metrics-server) each `replicas=1`, their three
  ReplicaSets, their three `Running` pods, and no StatefulSets, DaemonSets or Jobs at all.

  The discriminator is measured too, and the measurement is what makes it correct: k3s marks what its
  own manifest controller applies with `objectset.rio.cattle.io/owner-gvk =
  "k3s.cattle.io/v1, Kind=Addon"`, and **that marker holds on the Deployments and the ReplicaSets but
  NOT on the pods**, which carry only their pod-template labels - so a marker-only rule would have
  missed exactly the objects `Intent` reads. The rule resolves it through the owner chain instead: a
  pod whose `ownerReferences` name a marker-carrying ReplicaSet is the control plane's own. There is
  deliberately no list of add-on names and no list of namespaces: a list of names goes stale silently
  the first time k3s ships a fourth add-on, and a list of namespaces would stop a tenant who applies
  into `kube-system` from ever holding a node.

  The converse property was proven in the same run, because a filter that excludes too much turns
  "nodes scale to zero" into "nodes are never provisioned" and reads as success at zero: a tenant pod
  that cannot be scheduled IS intent and holds the node. Measured, one uninterrupted run, nothing
  deleted by hand: an empty cluster sits at zero with the add-ons `Pending`; a tenant pod gives
  `Provision`, the node comes up and the pod runs; an unschedulable tenant pod gives "intent, and a
  node is already counted"; and deleting ONLY the tenant workload gives "nothing running, holding the
  node", then `Teardown, nodes=0, nothing running for 3m0.002514861s, past the 3m0s hold`, with those
  three Deployments still deployed and untouched, after which `get nodes` is empty, `/readyz` is ok,
  and the guest and its data dir are gone.
- **The local-process node is REFUTED as the substrate; the node is a VM.** Three processes, three
  measured answers, and they cannot be reconciled on one host:

```
k3s server --disable-agent   must read and write /etc/rancher/node/password, which is NOT under
                             --data-dir and no flag moves it. Needed MODE, not content: the file's
                             value is never checked, because no node registers through this server
                             and so the k3s-<node> Secret that would disagree with it is never
                             created (measured: kube-system holds only k3s-serving, get nodes is
                             empty). Runs happily as a plain user once that file is theirs
k3s agent                    "Error: agent requires additional privilege if not run with
                             --rootless: not running as root" -- fatal, measured from the node's
                             own agent.log. /run/k3s/containerd, /var/lib/kubelet and 6444 are
                             root-level host resources it does not relocate via --data-dir
postgres                     "initdb: error: cannot be run as root" -- fatal
```

  A host that runs the store therefore CANNOT run a node, and the node cannot share a host with
  another node either - so no ordering of privileges fixes it. The control plane needs no privilege
  beyond being able to read and write ONE file, which is a one-time ownership decision by the
  operator rather than a per-run requirement, and with the node inside a VM nothing on this host
  runs as root at all. `--rootless` is the cheaper alternative and is unmeasured: it moves the
  kubelet's paths into a user namespace, which is exactly where flannel's VXLAN and the containerd
  socket would have to live too.
- **The vault is not in the default component set, and that is now a DECISION rather than a gap.** The
  config is still rendered and `bao server` still runs for a stack that names `ComponentOpenBao`, but
  `PurposeOpenBao` is no longer in `materialiser.DefaultPurposes`, and the startup check asks
  `PurposesFor(stack)` rather than the global list - it had been answering "is this purpose bound"
  from a list that says nothing about the stack in use. The reason is not that the vault is
  unfinished but that **nothing consumes it**: measured, a fresh vault from a rendered config answers
  `GET /v1/sys/health` **501** `{"initialized":false,"sealed":true}`, so the readiness gate
  (200/429/473) refuses; and there is no secrets flow to finish, since nothing enables a KV mount,
  nothing writes a bundle into a vault, and ESO's `SecretStore` path is hardcoded to `secret`. The
  identity bundle's real home today is the file tree at `<DataDir>/server/tls`, which persists and
  pre-seeds on its own.

  What blocked it was the unseal-key policy, and the reason to stop rather than choose one is that
  **every available answer defeats the purpose**: auto-unsealing with a key the gateway holds makes
  the vault only as strong as the gateway, and Shamir shares written beside the data they protect
  achieve the same thing. A component that starts, fails its own readiness gate and provides nothing
  is worse than absent - it occupies a lease and it parks a cluster in `Unavailable` for a reason
  unrelated to the cluster. It comes back when something needs a secret, and the unseal policy is
  decided then, against a real consumer.

  Worth recording so the hazard is not overstated later: this was **latent, not live**. The
  composition root already built `Stack{ComponentKine, ComponentK3s}`, so no production cluster was
  parked in `Unavailable` by the vault. What remained was the declarations and a library default that
  contradicted the decision, which is a trap for whoever configures a stack next.
- **`deploy/install-provider.sh` fails at its last gate, and the gate it fails at is not the one this
  entry named. PARTLY WRONG.** The script does not hold that gate: its last line runs
  `apply-permission-inventory.sh`, `set -e` propagates the status, and that script ends `exit $fail` -
  so the install exits 0 when every expectation it measures holds, and "exits 1" is a statement about
  one run rather than about the script. `IdentityValid=True` is a PRECONDITION the script waits for,
  not something it asserts or prints: two `until` loops poll for it and the only mention of the string
  is the timeout message that exits 1 if it never arrives. And the failing gate's own closing text
  contradicts the reason this entry gives for the failure. What it says a failure means is one of the
  credentials the DESIGN CHOSE - the runtime's kcp credential or its per-tenant minted certificate -
  "and never that a workspace ServiceAccount needs a wider binding". So the entry describes the finding
  the gate exists to prevent rather than the reason the gate fails. That finding is real and is in
  section 7: a workspace ServiceAccount token is confined to its origin logical cluster and refused
  elsewhere before RBAC is consulted, so no subject string lets one serve the runtime. It is a gate
  doing its job, not a broken install, and it means an install is not "green" until the credential
  question is answered in the deploy, not just in this document.
- **Multi-node is out of scope for v1.** The underlay verdict makes single-node the shape, and
  `scale.Decide` assumes it harder than this entry said: `Provision` returns a literal `Nodes: 1`, a
  cluster with intent and any node already counted gets `None`, teardown is all-or-nothing to zero, and
  the `scale.Nodes` seam itself has no count - an actuator can only be asked to BE the node.
  `nodeID` is the package constant `"node1"`, so every cluster gets exactly one node, named
  `<cluster>-node1`, and the per-node machinery that would generalise (the ordinal, the per-node address
  off the cluster's range) is only ever called with ordinal 1.
- **An image did not record the guest agent it carried, and closing that closed the family it belonged
  to. CLOSED, and measured twice: once by the failure, once by the fix.** The record weighed the
  kernel, its config, the base image, the k3s binary, the module NAMES and the seeded digests - and not
  the agent, which is the binary that decides the guest's kubelet command line, its resolver, its
  identity and its interface. So an agent change could not move an image's name, and the consequence
  was not theoretical: the builder answered `up to date, nothing built, nothing pulled` and was RIGHT
  about every input it tracked, the image that booted then ran the previous agent against a new node
  disk, that agent read a removed field as Go's zero value, passed `--kubelet-arg port=0`, and the
  kubelet refused to start outright (`port (--port) 0 must be between 1 and 65535`), so the guest never
  joined. Nothing in that sequence named the agent, and the first instinct is to debug the kubelet: a
  refusal that cannot name the thing that is actually stale is the failure mode, not a missing check.

  The record now carries the agent's path and **the digest of the binary the build just produced** -
  the binary and not its source, because the flags and the toolchain are inputs to that binary and no
  digest of a source tree covers either. It carries the module tree's path and a digest over every file
  in it, path and contents and symlink targets, because the build copies that tree into the image whole
  (section 12). And it carries the configuration values that were free to change under a name that did
  not move: the boot arguments, the guest's k3s data directory, the rootfs label and its headroom, the
  agent's package and its build flags. Those are recorded as VALUES and not as a digest of the
  configuration file, because the file is canonical in places the bytes are not - a module list
  reordered is the same module list, and a digest of the bytes would rebuild 2.3 GB for a change
  nothing made.

  What closes the CLASS rather than these instances is what matters more: a test holds the builder's
  read set and the record's field set equal in both directions, so a configuration field the build
  reads and the record does not mention fails the suite, and a record field nothing reads fails it too.
  It was checked the way a gate has to be - by removing the agent's declaration and watching the test
  fail - and it is the shape section 12's fingerprint test already had, applied to the enumeration as
  well as to the mutation. VERIFIED through the ordinary path rather than a hand-repointed one: the
  same command over the same configuration that answered `up to date` answered `building` and produced
  a different name, the normally-built image records the digest of the agent beside it, and a guest
  booted from it reached `1/1` system pods with zero restarts, reported `{"Port":10250}` for its own
  kubelet, served a proxied `/healthz`, and held.

- **The unwired controls were settled ONE AT A TIME, and the settlement was not the same for all five.
  Two were deleted, on this repo's own rule that a control nothing reaches is indistinguishable from
  absent, and the explanation of why each existed is kept here rather than in the commit: a deletion that
  takes its reasoning with it leaves the next reader unable to tell residue from a missing feature.**
  `processrunner.Reach` and `processrunner.Identity` are DELETED, and they were not a missing wire -
  every production call site passed `Identity` (`clusterprovider/steps.go`, the pasta line in
  `nodefirecracker/node.go`, and the materialiser's own test helper), so the mapper mapped nothing
  anywhere. It was VM residue: the substrate this design abandoned reassigned a guest's ports on every
  boot, so a caller that could not ask "what port is this guest's 6443 on this time" could not connect -
  and a guest's ports are its own network namespace now, so there is nothing to map. Deleted with it: the
  type, the identity function, the `Spec` field, the `Process` field, the accessor, and
  `TestReachDefaultsToIdentityAndIsOverridable`, whose own failure text ("an off-host substrate reassigns
  on every boot, so a caller that cannot override Reach cannot connect") is the sentence that dates it.
  `internal/nodetransport` is DELETED: it is an unwired REFUSAL (`ErrSecondSession`, "a node is one
  connection; a second session is a second VM"; `ErrSubstrateDestroy`, "a node is not destroyed by the
  substrate, or the VM's disappearance would prove the harness instead of the provider") whose only
  importer was itself, and the arm it was written for is the property the substrate does not have - with
  the node in a VM, teardown is the guest's own. The RULE that refusal encoded - the seam must not give
  the dial lifecycle ownership, so `Wait` and `Close` stay distinguishable, and one node is one
  connection - is prose in section 9 and stays there; what is deleted is the code that asserted it, which
  nothing called. `internal/sessionreport` goes with it, and not because
  it was unused: it parses the market's SSH front-door report, the deadline line, which is the only
  honest source of `nodeExpiry`, and its only non-test importer was `nodetransport`. Section 16 puts the
  door out of scope and says its report surface is not, so a parser of that surface has no importer and
  cannot get one without bringing the door back in scope. What is NOT lost with it is the invariant its
  test guarded: "a node whose deadline is UNKNOWN must not clear the teardown gate" is enforced live by
  the materialiser ("status.nodeExpiry is unknown and the apiserver has answered; refusing to tear down
  against a gate that cannot compare") and measured by `TestTeardownRefusesWhenTheGateCannotCompare` -
  the deleted test was over the parser, not over the gate. Two were KEPT, each with a caller worth naming
  rather than an assumption that one exists. `internal/versions/versions.json`'s reader is
  `internal/versions/versions_test.go`, which is not a self-test: it is where the DEPLOY config's guards
  live - both audit policies are metadata-only, the deployed `kcp-audit-policy.yaml` matches its copy,
  the `kcp` entry pins `--feature-gates=WorkspaceMounts=true` (ALPHA and default-off, so with it off
  every mount silently stops being served), the `APIResourceSchema` covers every field of the Go type,
  and the workspace type binds an export that exists - NINE test functions in one file, and deleting the
  manifest deletes all nine. `internal/devicekey` is the only implementation of a declared production
  seam, `kcpclient.DeviceKeyGranter`, and its caller is the LIVE proof
  `TestTheRenewalPathAgainstRealInfrastructure` in `internal/tenantclient/live_renewal_test.go`, gated on
  six `SOCIALWEB_LIVE_*` variables - the only place ADR 0020's Grant -> Resolve -> Issue has ever run
  against real infrastructure. Deleting it would delete the seam's only implementation and the proof of
  that path together. **That ruling stands, and the rest of the fact about the seam is now settled: the seam is WIRED, and
  the endpoint became what the surface already said it was.** The gateway was constructed without a
  granter - `cmd/socialweb-gateway/main.go` passed `Options{Host: ..., RestConfig: ...}` and no
  `DeviceKeys` - so in production `kcpclient.Client.deviceKeys` was nil and `Client.Kubeconfig` took its
  guard branch and returned `ErrNoDeviceKey`. That method is not internal plumbing: section 6's XRPC
  surface serves it as `getKubeconfig`, "the credential the user runs kubectl with", from `xrpcserver`
  through `gateway.GetKubeconfig`. So a real gateway had an endpoint that was routed, documented and
  unit-tested, and refused every call it would ever receive. The shape of the failure is why it read as
  a note rather than an alarm: it fails CLOSED, with a message that says what is missing, so it was
  never a hole in the boundary - it was an absence that reads as a presence, which is this document's
  recurring defect.
  Wiring beat removing the endpoint because the seam was already complete and already proven.
  `internal/devicekey` generates the did:key, writes the binding row, signs the credential and renders
  the kubeconfig, and the store `buildDeps` already constructs - `internal/tenantclient.Client` - IS
  the `Binder`, because its `Record(ctx, tenantclient.Row) error` is the interface's one method, word
  for word. What was missing was one argument and four policy values. Removing the endpoint would have
  deleted a working seam to make the document agree with the wiring, and ADR 0020's Grant -> Resolve ->
  Issue would have lost its only non-test counterpart.
  The four policy values are configuration with NO defaults, which is what `devicekey.New` exists to
  enforce: `SOCIALWEB_GATEWAY_DEVICE_KEY_AUDIENCE`, `SOCIALWEB_GATEWAY_DEVICE_KEY_TTL`,
  `SOCIALWEB_GATEWAY_DEVICE_KEY_EXEC_COMMAND` and `SOCIALWEB_GATEWAY_DEVICE_KEY_EXEC_ENV_NAME`. A
  device credential's audience, lifetime, helper path and environment variable name are choices an
  operator takes, so a missing one is a refusal at `load()` rather than a lifetime nobody chose. The
  audience MUST equal the first of `SOCIALWEB_GATEWAY_AUDIENCES`, and a mismatch is refused rather than
  reconciled, because the mint verifies the credential against that audience and the divergence would
  otherwise surface as every handed-out kubeconfig minting Unauthorized. The consequence is deliberate
  and is the same gate `ErrNoMintStore` already is: a gateway configured without these four variables
  refuses to start.
  **A routed verb is not a working verb, and the suite now says so twice.** `TestEveryVerbIsBound` asks
  only that each declared verb is not 404, which a verb that is routed and permanently non-functional
  passes - the same shape as `k3s check-config` returning 0 over a missing module, which is why it
  passed over this endpoint for as long as this entry sat open.
  `TestBuildDepsWiresTheDeviceKeyGranterFromTheRealImplementation` calls the REAL `buildDeps` and fails
  on `ErrNoDeviceKey`, naming the guard; `TestEveryVerbTheLexiconsDeclareIsAnswerableWithTheRealDeps`
  drives every verb the lexicons declare past AUTHENTICATION with a real service-auth token and requires
  its ANSWER, not its presence, and derives its table from the lexicon files so a verb added to the
  surface cannot escape it. Against the un-wired tree the first reports the guard by name and the second
  reports `getKubeconfig answered 500`, while `TestEveryVerbIsBound` still PASSES - and that last
  measurement is the one that makes the pair worth having, because it is the difference between a test
  that could fail and a test that never did.
- **The identity bundle's home is the FILE TREE at `<DataDir>/server`, and section 5's component 7 said
  otherwise because a document outlived the code it described.** The bundle is the 13-entry
  `tls/`+`cred/` tree under `<DataDir>/server` - the same root `clusterstack.TLSDir` puts `tls/` under -
  seeded and harvested there by `clusterprovider.HarvestBundle`/`SeedBundle`, with a CA fingerprint
  measured byte-identical across a teardown and a rebuild (section 7, and the timing block in section 9).
  OpenBao was never its home: `PurposeOpenBao` is not in `materialiser.DefaultPurposes`, a fresh vault
  from the rendered config answers `GET /v1/sys/health` **501** `{"initialized":false,"sealed":true}` so
  the readiness gate (200/429/473) refuses it, nothing enables a KV mount, nothing writes a bundle into a
  vault, and ESO's `SecretStore` path is hardcoded to `secret`. So the ESO/KV-mount flow is not
  unfinished business: it is UNNECESSARY until a secret exists that the file tree cannot hold, and the
  unseal policy is decided then, against a real consumer. Component 7 now says what the code does.
  **SUPERSEDED 2026-09-25: the file tree is still where k3s reads the bundle, and the vault is now its
  authoritative store.** That is not a reversal of the measurement above but of its conclusion, and the
  mechanism is the one the paragraph describes as unnecessary becoming necessary. The vault is in the
  production stack, it is initialised and unsealed, it holds all 13 entries at `secret/data/k3s/identity`,
  and the wake reads them from it: `SeedBundleForWake` (`internal/clusterprovider/bundle.go:72`) tries
  `SeedBundleFromVault` FIRST, materialises `<DataDir>/server/{tls,cred}` from the vault, and only falls
  back to the file tree when the vault holds nothing (a first bring-up, before the harvest has written
  one). The fallback is deliberately narrow: when the vault's unseal material EXISTS but is unreadable,
  the code REFUSES rather than falling back, because a fallback there would let k3s author NEW CAs under
  the same name and silently invalidate every credential issued before the rebuild
  (`internal/clusterprovider/vault.go:632`). Two tests pin the direction:
  `TestTheWakeMaterialisesTheIdentityBundleFromTheVaultNotTheFileTree` and
  `TestTheWakeFallsBackToTheFileTreeWhenTheVaultIsEmpty`, both PASS. What the flip does NOT change is
  durability: the vault's storage and its unseal material sit on the same state volume as the
  materialised tree, so the vault buys authority -- one store of record, custody, and a wake-time
  restore that cannot silently regenerate CAs -- and not off-host backup. Losing the volume still loses
  all three together.
- **The vault's lease is width 1, and the second port was never read.** `purposeWidth[PurposeOpenBao]`
  was 2, spelled "api + cluster", while the config the vault is given sets `disable_clustering = true`,
  which removes the cluster listener and makes "one vault per cluster, no HA" a property of the file.
  Both the listener stanza and `api_addr` take the same `Lease.Port`; no code reads `Lease.Port + 1`; and
  with the cluster listener gone nothing binds it. A leased port that is never bound is not harmless: it
  is a port carved out of the per-host range no later cluster can use, and a reader of the lease file has
  to work out which of the two ports is real. Section 8 records the measurement that separates this from
  `PurposeK3sSecure`, which stays 2 because the apiserver's pair is MEASURED as two bound listeners. The
  fixtures that had pinned 2 - portallocator's `TestJ`, portscan's fold fixture, and the clusterprovider
  step leases - pin 1 now.
- **A reconcile pass is accepted as unbounded at v1, with a named trigger: a decision with a condition,
  not an open question.** Measured: ONE PASS TOOK 6.7 s AGAINST A 2 s TICK, because the tenant bootstrap
  runs synchronously inside the pass, so the tick is a floor rather than a period. At 0-or-1 node that is
  accepted: one cluster, one node, and the pass is not on a tenant's critical path. It is still a
  scheduling decision nobody has made at N, and the trigger that reopens it is precise - a pass whose
  duration can exceed the tick BY DESIGN, which is what happens once a cluster can hold more than one
  node. `scale.Decide` returns a literal `Nodes: 1`, `nodeID` is the package constant `"node1"`, and
  multi-node is out of scope for v1 (section 10), so the condition cannot be met today and the decision
  holds until it can.
- **A restored guest is a TEMPLATE, and no resume-triggered re-identification is worth having: the
  mechanism cannot scale out, and the cold boot is faster than the cluster noticing the node died.**
  Snapshot restore is refuted as a provisioning primitive, by measurement rather than by taste:
  restore-to-Ready **9.974 s** against cold **9.079 / 9.158 / 10.768 s**, so a restore buys nothing in
  time; and a restored guest cannot make a NEW node, because restore does not re-run boot-time units, so
  the identity adoption the guest's own agent performs - reading the per-node read-only virtio-blk disk
  this design settled on, in place of firecracker's MMDS, which appears in no `.go` file - never runs, and
  a snapshot frozen as one node comes back as that node even when its recorded disk path is re-pointed at
  another node's disk. What stayed open was whether ANY resume-triggered re-identification is worth
  having, and the answer is no: the mechanism cannot scale out, so re-identification would serve a node
  the cluster ALREADY knows, and a cold boot (9.079-10.768 s) is faster than the cluster notices the node
  died. One part of the old entry stays UNVERIFIED rather than resolved: on 6.1.186, the pinned-kernel
  route this design abandoned in favour of the host's own kernel, the guest does NOT log `crng reseeded
  due to virtual machine fork`, so clones may share RNG state - unmeasured on the kernel that actually
  ships, and a reason to keep validating it.
- **The GATEWAY's verdict on a normally-built image is measured now, not pinned only by unit tests.**
  The image record carries the agent's digest, and the normally-built image at
  `/home/johnandersen777/kcp-image/out/sha256-bbee5b4ab2a616e43bebf3212ebe50ac956e800e372e083c4488fb0f761b28a8`
  records `agentBinaryDigest sha256:cabe50a056285cefcc53aadd7795531ddc823d7227d8fce96f635b978327882d` beside
  the module tree's digest. The run that booted that image before exercised the SUBSTRATE directly, which
  does not call `CheckInputs`, so the composition root's own verdict on a normally-built image was a
  unit-test fact. It is a measurement now: the four-step proof ran against THAT image, env-gated, with the
  substrate named by configuration, and **PASSed in 97.65 s** - the step that derives a node from the
  image is what reaches `nodeSubstrateFor -> manifest.CheckInputs()`, and a guest booted from it, or no
  node would have joined. All four transitions held in one uninterrupted run: (1) the control plane
  reached `Ready` at its leased apiserver port **https://127.0.0.1:62702** with zero nodes; (2) `get
  nodes` was empty, and stayed empty across passes; (3) a node joined ON DEMAND - `Provision, nodes=1`,
  "intent and no node" - as `liveweb-node1 Ready <none> ... v1.36.4+k3s1`, with the tenant workload
  running on it; and (4) the cluster returned to zero with the control plane STILL `Ready` at the same
  URL and still listing `default`, `kube-node-lease`, `kube-public`, `kube-system` and `users`. The run
  prints the four transitions and no host-timed instance-start-to-ready, and its ports come from
  `liveports.FourStepProof` in `internal/liveports` (flock-held slot) rather than from a remembered
  number - the package exists because a port block picked from memory collided, and the fingerprint in
  that image's path is 71 characters, so the image is named by its digest and never by a glob.
- **`shut` means the tenant's apiserver was NOT REACHED, not "not 2xx", and the gate now holds against a
  serving cluster where it used to reap.** The defect and the fix are one substitution: `shut` was
  `o.PathStatus != 0 && (o.PathStatus < 200 || o.PathStatus >= 300)`, which a 401 satisfies, and the
  provider's probe through the mount can never get a 2xx because the tenant apiserver refuses `/version`
  to callers it cannot authenticate - so the gate read a SERVING cluster as shut. It is now
  `o.PathStatus != 0 && !reached(o.PathStatus)`, where `reached` is 2xx, 401 or 403. The code table is
  stated with its basis rather than asserted: **0 HOLD** (transport failure to kcp, so there is no answer
  about the mount at all); **2xx HOLD** (the mount served); **401 HOLD, MEASURED** - and it is a reach
  rather than a guess because the BODIES prove it: the direct apiserver GET and the through-the-mount GET
  return the identical k8s Status JSON, while kcp's own refusal (503) and the proxy's (502) carry
  different or empty bodies; **403 HOLD by judgement, not by measurement** - kcp's localproxy returns
  only 404/429/5xx before forwarding and no path was found where kcp answers 403 on a mount itself, so
  this is the code treated generously ON PURPOSE, because if it ever occurs the gate holds longer and
  that is the safe direction; **404 SHUT, MEASURED** (kcp answers 404 for a workspace path it cannot
  resolve, and nothing there proves a backend); **502 SHUT, MEASURED** (the proxy with no backend, the
  dead direction); **503 SHUT, MEASURED** (kcp's own `Not available.` before forwarding); and any other
  non-zero code SHUT, failing closed on the unproven. The error direction is chosen rather than
  inherited: ambiguity resolves to reached-and-hold, because "stuck and billing beats silent loss" is
  settled and a reap on a guess is not. `TestOnlyAnUnreachedPathIsShut` pins the predicate behaviourally
  at the materialiser, both directions. The before-and-after is the reading that matters, both taken from
  the same live instrument over the same cluster: BEFORE the fix the gate read
  `phase=Draining ops=[tenant-path-shut reap release-ports release-subnet remove-finalizer] PathStatus=401`
  48 ms after the delete, with the group up and serving; AFTER it read
  `phase=Draining ops=[shut-tenant-path await-tenant-path-shut] PathStatus=401` at 12 ms with no finalizer
  released and nothing reaped, and the tenant apiserver was STILL ANSWERING when that pass finished. The
  mount turned 503 at 218 ms, the reap came on a LATER pass at 260 ms reading 503, and the group died
  last. That order is what section 11 always claimed and only a fake had ever produced.
- **The Cluster finalizer is installed by the create path and released by the pass that reaps, and
  installing it exposed a second defect that would have made things worse.** `FinalizerCluster` was
  declared and RELEASED (`materialiser.go:368` sets `RemoveFinalizer`) but never ADDED, so
  `DeletionTimestamp` was unreachable and `Draining` was written by no production path. What the fix had
  to confront is that `RemoveFinalizer` had NO CONSUMER either: setting the field on a Result that
  nothing reads would have left every deleted Cluster terminating for ever, with `Refs` still listing it
  and every later pass reaping a dead cluster - a leak traded for a leak. So the fix is three parts, not
  one: `Clusters.RemoveFinalizer` on the interface, `Registry.RemoveFinalizer` on the implementation, and
  the call in `Provider.reconcile`. Six fake implementers gained the method, enumerated by the compiler
  rather than by search. `Registry.RemoveFinalizer` removes only THIS repository's finalizer and guards
  the write with a JSON Patch `test` operation on the list it read, so a concurrent writer produces a
  refused patch rather than a clobbered list. One bounded gap is noted rather than papered over:
  `kcpclient.Ensure` returns early on `AlreadyExists`, so a CR created before this change keeps no
  finalizer - there is no deployment to migrate, which is why it is a note and not a convergence write.
  And the measurement now supplies nothing production does not: the mount proof asserts the CR ARRIVES
  from `kcpclient.Ensure` already carrying the finalizer and fails if it does not, with a message saying
  a self-supplied finalizer would keep passing over the broken path - then measures the CR being deleted
  at 261 ms once the reaping pass releases it.
- **A `Draining` Cluster CR shuts the mount BEFORE its process group dies, and the reading came from a
  real kcp started inside a test.** This is the question the whole of section 11's gate was built around,
  and it is settled by measurement rather than by inspection:
  `TestTheMountShutsBeforeTheTenantProcessGroupDies` starts kcp as the CURRENT uid in its own temp root
  directory - `--feature-gates=WorkspaceMounts=true`, with a temp `kine` on a sqlite file behind
  `--etcd-servers` - deliberately not the root-owned `/var/.kcp`, and deliberately not an embedded etcd,
  because etcd is host-fixed on 2379/2380 and no flag moves it. It applies `deploy/`'s APIResourceSchemas,
  APIExports and WorkspaceTypes into `root:socialweb-provider`, builds the tenant tree with
  `kcpclient.Ensure`, and behind it runs the real reconciler, the real `Registry`, real leased ports,
  real postgres and kine and a real `k3s server --disable-agent`. Three answers are distinguishable at
  that path and the CONTROL establishes all three, which is what makes the third a reading rather than a
  default: a serving cluster answers **401** with the tenant apiserver's own Status body; after the group
  is SIGTERMed the mount answers **502** with an empty body, which is section 14's long-recorded dead
  direction reproduced from the real mount; and a `Draining` CR answers **503 `Not available.`**, kcp's
  own refusal, with the backend never contacted. Writing `Draining` into the CR while the group was up and
  its apiserver answering moved the mount off 401 in **206 ms**, and writing `Ready` back brought it to 401
  again - so the PHASE is what shut it. The phase is read as a string by a `switch` whose only case is
  `Ready`, so the mechanism is phase-agnostic above it: `Unavailable` and `Initializing` measure 503 too,
  and `Connecting`/`Unknown` cannot be stored at all because this repository's own APIResourceSchema
  enumerates four phases. The reconciler is therefore correct here, and the reason this took so long to
  get is stated plainly in section 14: a fake registry was answering the only question anyone asked it.
  Two things about the instrument are worth keeping: the whole test is **27 s**, needs no root and no
  `/dev/kvm`, and carries **no env gate** - it skips only when `kcp`, `kine`, `k3s` or `postgres` is
  absent, with a skip message that says a skip here is this question staying open, because an env-gated
  test that has never run is worth nothing and this repository has been bitten by exactly that.
- **The control plane runs as a non-root container, and the zero-node proof passes INSIDE it.** This is
  the measurement the container exists for. `TestTheControlPlaneReachesReadyAtZeroNodes` - the real
  reconciler against real leased ports, real kine, a real per-cluster postgres and a real
  `k3s server --disable-agent` - ran in the image and PASSED, in 9.50 s (and 7.67 s on an earlier
  image) against a ~6 s host baseline, with STEP 1 reaching `Ready` at `https://127.0.0.1:31202` and
  STEP 2 reading a live control plane with zero nodes. Two runs, both PASS. The image takes no
  permissive flag: not `--privileged`, not `--security-opt`, not `--cap-add`, not `--device`, not
  `--network host`, no userns remap. It runs `USER 1000:1000`, deliberately the host user's uid so the
  mounts are readable and the lock directory lines up, and it is built from the host's own family so
  host-built binaries run. Run against itself, `check-host` answers 9 HOLDS, 1 MISSING and 10 MUST BE
  PASSED IN and exits non-zero - which is the honest reading of a control-plane-only image and exactly
  what the third category was designed to say: the kernel, the module tree, `CLONE_NEWUSER`,
  `/dev/kvm`, the firecracker binary and the image store are the container's EDGE, and `pasta` is the
  one thing MISSING, because egress for the node half is not this image's business.
  **The failure it caught first is worth more than the pass.** The first in-container run failed with
  k3s logging `Sending HTTP/1.1 400 response: node password not set` for ever while `/readyz` stayed
  503 and the cluster sat in `Initializing`. The cause was an EMPTY `/etc/rancher/node/password`: it
  opens read-write, so every check that opens it says yes, and the server needs it non-empty. So the
  requirement is that the file be NON-EMPTY and not merely writable, the image now writes 32 random
  bytes into it, and `check-host` reports an empty file as `MISSING` with that k3s line as its
  evidence. It is the same shape as everything else here - a thing that opens clean and is wrong - and
  it was found by running the proof in the new environment rather than by reasoning about it.
  **Its validation script runs twice in a row, and the host-contract step it runs is falsifiable.**
  `deploy/control-plane-container/validate.sh` names its two containers, and `--rm` removes a container
  only on a NORMAL exit: a run ended by a timeout, a Ctrl-C or a supervisor being stopped leaves
  `kcp-zeronode` behind holding the name, and the next run dies with `Conflict. The container name
  "/kcp-zeronode" is already in use` - a validation that fails for a reason it does not state. That was
  observed rather than anticipated: one run held the name and blocked the next. The fix is a
  `docker rm -f <name>` before each `docker run`, guarded so a name that is not there cannot abort under
  `set -e`. That is chosen over unique per-run names and over dropping `--name` because it is the only
  one correct UNDER A KILL: a suffixed name still leaks one stale container per kill and a pid- or
  `$RANDOM`-derived suffix can collide, since pids recycle and a container from a previous boot still
  holds the name; dropping the name leaves an ever-growing pile of anonymous stopped containers and
  gives up the one label a human uses to find a run that is still alive; and `rm -f`, unlike either,
  also reaps a still-RUNNING orphan whose owner is gone, freeing the ports it holds rather than leaving
  them for the next run to trip over. The second step is gated now on a POSITIVE assertion rather than
  an exit-code allowlist, because `check-host` is DESIGNED to exit non-zero here - the image is the
  control-plane half, so it reports something like `9 HOLDS, 1 MISSING and 10 MUST BE PASSED IN` - and
  the `|| true` that allowed for that swallowed every OTHER ending too: a crash, a missing binary, an
  OOM kill, all printing identically while the step gated nothing. The script captures the output,
  requires the summary line `N checks: A HOLDS, B MISSING, C MUST BE PASSED IN`, fails with that
  expectation named when it is absent, and fails when the total is not the sum of the three categories.
  **`check-host`'s third exit code is what makes this load-bearing**: it exits 2 when it cannot read the
  host well enough to check anything, printing `MISSING host.facts` and NO summary line - so a `{0,1}`
  allowlist would have caught that by luck and a `{0,1,2}` allowlist would have passed it silently. The
  line is the instrument, and its absence is the failure.
- **Only what is inside the build context is ordered by the build graph, and the artifact leaves the
  build as files.** The build system hashes what it is given, so the stage COPIES the operator's
  configuration into itself at the path the configuration names rather than mounting it: a copy is
  content-addressed, so its bytes enter the cache key and a change to it re-runs the stage instead of
  answering a cache hit over the previous image. Everything the configuration reads that cannot move
  into the context - this host's `/boot`, its `/lib/modules/<release>`, the base image and the artifact
  directory - is mounted read-only at the exact paths it spells, which is what makes the container's
  build compute the same inputs as the host's. Two measurements forced this shape rather than being
  chosen: a named build context is mounted READ-ONLY (`Read-only file system` on a write into one), so
  no stage can write the store through a mount; and `/boot` cannot even BE a context (`error from
  sender: open /boot/efi: permission denied`, mode 0700), so its two files are staged. The artifact
  therefore leaves through an `extract` stage and `--output type=local,dest=<out_dir>`, landing
  `<out_dir>/<fingerprint>/{vmlinux,initramfs.cpio,rootfs.ext4,image.json}` - exactly the paths
  `LoadImage` derives from the configuration, so the runtime's lookup is the same check it always was.
  The cost is stated rather than hidden: the artifact is written twice more than one build needs, and
  the builder must hold both at once.
  **The record covers the configuration's path and every value it transcribes**, which is
  `modules`, `initramfs_modules`, `images`, every file digest, the whole module-tree digest,
  `boot_args`, `k3s_data_dir`, `rootfs_label`, `rootfs_headroom_mib`, `agent_package` and
  `agent_go_flags` - and not `firecracker`, `stage_dir`, `out_dir` or `image_cache_dir`, none of which
  is image content. Two consequences follow, and both are stated rather than discovered. The same bytes
  at another path is another name, because the path is an input. And **the agent binary carries Go's
  VCS stamp, so a commit that changes no agent source still moves every image's name**: one agent
  source at three repository states gave `cabe50a0` (40781b3+dirty), `790f97a2` (e88b2f3+dirty) and
  `c9300ed9` with `-buildvcs=false` (reproducible twice, and byte-for-byte what the container's own
  stage produced). Both of the first two were `+dirty`, meaning editing any file in the repository - a
  document, a test - renamed every node image and cost a 2.2 GiB rebuild for no change in image
  content. The name must be a function of what determines image content, and a repository dirty flag is
  not one, so `-buildvcs=false` is set in `agent_go_flags`. Traceability is not lost by it:
  `agentBinaryDigest` identifies the source by the content that produced it, which is a stronger
  identity than a revision label.
- **The node image was the one input to a boot that nothing built, and the runtime derives it now.**
  `SOCIALWEB_CLUSTERPROVIDER_IMAGE` named a LITERAL directory and the substrate then ran
  `CheckInputs` over it, so an operator had to compute a fingerprint by hand and paste it into
  configuration - and every way that goes wrong is silent, because a stale fingerprint is a directory
  that exists and validates, so a node boots from an old image and nothing says so. The old guard was
  narrower than the defect: `checkPlacement` did stat the three image files at config load, so a wholly
  wrong path was caught, and what passed silently was a STALE name. `CheckInputs` could not close it,
  because it holds the image to the CONFIGURATION and never to the name the configuration's own inputs
  give. The name is derived now from the same inputs the build fingerprints, and `Fingerprint` stays
  load-bearing as the runtime's answer to "does this name exist" while `CheckInputs` stays the answer
  to "is what is at it the build of the configuration in front of me" - one fact with two readers, which
  is why neither was deleted. A configuration whose inputs moved no longer names a stale image at all:
  it names a directory no build has written, the substrate refuses and says which command writes it, and
  the image built before the change keeps its own name in the store as the rollback it is. The three
  checks the runtime now performs are the manifest's own record hashing to the name - which the runtime
  did not have before and which is the piece that closes the class - then `CheckInputs`, then the
  kernel, initramfs and rootfs being present. The digesting that costs is paid once, at startup, in
  `nodeSubstrateFor` rather than in `load()`, which still refuses an unset value with no default. One
  honest cost is recorded rather than hidden: the runtime digests 2.3 GB twice, once in `Fingerprint`
  and once through `CheckInputs` -> `CompareImageToConfig`, and with the name check in place the second
  pass is provably redundant for the file, tree and list dimensions and load-bearing only for the
  manifest's operative fields. One requirement moved with it and operators must know: the four-step
  proof now needs the builder's CONFIGURATION, not merely a built image directory, because an image is
  only valid relative to the configuration whose inputs name it.
- **The node image is built by the control plane's own container build, and the host contract is one
  binary that runs in both places.** Two decisions, taken together because each is what makes the other
  checkable. The first answers a question this document had left implicit - what builds the node image
  and when - and the answer is that it is a stage of the same multi-stage build that produces the
  control-plane image, so the build graph orders it instead of an operator's memory doing so (section
  12). The second is the instrument: `socialweb-computer-kcp-check-host` is a single binary that runs
  the whole host contract, and it is the reason the first decision can be validated rather than
  asserted. Each check reports one of three things - HOLDS, MISSING, or MUST BE PASSED IN - and that
  third category is why one binary serves both environments: on a host the last two collapse into "this
  host cannot run this", and inside a container MISSING means the image is wrong while MUST BE PASSED IN
  means the container is correctly declaring its edge. The checks are drawn only from failures this
  design has already paid for: the guest kernel's measured module set, the xtables extension set that
  makes `iptables-restore` all-or-nothing, `/dev/kvm`, `CLONE_NEWUSER` and pasta, the env-provided
  binaries, free space for a 2.3 GB image, `postgres` as non-root, and the SELinux label whose absence
  presents as an unrelated permission error. What the binary does NOT do is define any of those facts a
  second time, and it does not do it in more places than first written here: the module check is
  `internal/kernelmodules` (the builder's own check, moved out of `cmd/socialweb-nodeimage` so the
  builder and this binary share one definition rather than restating it - the builder's `modules.go` is
  now a wrapper), the pool and the host-network enumeration are `internal/subnetallocator`'s
  `DefaultPool`/`BlockAt`/`HostNetworks` using the same `Overlaps` the allocator's own admission makes,
  the image store is read through `internal/nodefirecracker.LoadImage` - the provisioner's own
  config->fingerprint path, so the check and the boot cannot disagree about which image a configuration
  names - the postgres probe is `internal/clusterprovider`'s own `StartPostgres`, and the lock directory
  is `internal/liveports`'s `LockDir`. This is the reason the count here is worth stating at all: a
  second definition of one fact is how the two drift, and the fingerprint's missing agent was exactly
  that drift. The container that follows is the control-plane half only -
  kcp with its own root directory, kine, a per-cluster postgres and `k3s server --disable-agent` - run
  non-root, because `postgres` refuses root and because "nothing here needs root" is a claim the
  container should prove by construction rather than restate. It takes no permissive flags: not
  `--privileged`, not a wholesale seccomp disable, not the host root mounted in. If a capability turns
  out to be needed, it is granted narrowly and what the grant's width costs is stated. Its validation is
  the strongest reading available without a node: `TestTheControlPlaneReachesReadyAtZeroNodes` drives
  the real reconciler against real leased ports, a real kine, a real per-cluster postgres and a real
  `k3s server --disable-agent` and passes in about 6 s on the host, so running it INSIDE the container
  makes portability a measurement - a pass there is the result, and a failure there is the finding.
  This is also what section 14's last gap was waiting for: the one thing the fakes cannot answer, a live
  kcp serving the mount under `--feature-gates=WorkspaceMounts=true` with an `APIExport` and a tenant
  `APIBinding`, is a thing the repo can now stand up rather than a thing it stands up nowhere.

- **DEV-2.** The development tools were built with go1.23.7 and could not type-check this go1.26.4
  module: gopls, staticcheck and govulncheck all reported errors that do not exist and missed those
  that do, so the repo had no working static analysis and every editor diagnostic was untrustworthy.
  This is the real cause behind the recorded `stale_gopls` trap, whose rule ("the COMPILER is the
  authority") worked around the symptom rather than the tool build. All three were reinstalled with the
  module's toolchain. The before-and-after is one reading: `staticcheck ./...` printed 10 `(compile)`
  lines, each saying the tool was built with go1.23.7 and analysed nothing, and prints 0 now. The
  instrument is `staticcheck ./... 2>&1 | grep -c '(compile)'` returning 0 together with
  `go version -m $(which gopls)` naming go1.26.x.
- **DEP-1.** go.mod pinned `go 1.26.4` while the standard-library fixes that landed in `go 1.26.6` were
  absent, leaving six reachable standard-library vulnerabilities. The pin moved to `1.26.6`. The
  before-and-after: `govulncheck ./...` reported 6 reachable findings and reports 0 now, with 3
  remaining in required modules that are never called. The instrument is `govulncheck ./...` exiting 0
  with no reachable finding.
- **SEC-5.** `apply-permission-inventory.sh` compared the certificate's and the key's public key by
  hashing whatever openssl printed, so when openssl failed it hashed an empty string on both sides, a
  junk certificate and a junk key compared EQUAL, and the "does not parse" branches were unreachable.
  The public key is now captured only on a successful parse. Before and after, three fixtures: a
  junk/junk pair, a valid/junk pair and a mismatched valid pair each `note_fail` and exit 1, where
  before the junk pair passed; a matching pair exits 0. The instrument is those fixtures, which fail if
  the empty-input hash returns.
- **REL-3.** `deploy/control-plane-container/validate.sh` treated the last `go test` exit code as the
  verdict, and `go test` exits 0 on a `--- SKIP` and on `[no tests to run]`, so the validation could
  pass over a proof that never ran. The script now requires a `--- PASS: $TEST` line and refuses both
  skip forms. Three fixtures exercise it, the whole script was driven with stub `go`/`docker`, and a
  real image run gave rc=0 with a 7.20 s PASS. The instrument is a fixture that skips the named test and
  must make the script exit non-zero.
- **REL-2.** `shut` was `PathStatus != 0 && !reached`, so 300, 429, 500 and 504 reaped a serving
  cluster while only 404, 502 and 503 are measured shut; the section 15 text that said "any other
  non-zero code SHUT" was the contradiction being fixed. `shut` is now an allowlist of 404/502/503, and
  300/429/500/504 HOLD with no reap. The new `TestUnmeasuredCodesAreNotShut` failed before and passes
  after, the live mount proof still PASSes (26 s), and the now-dead `reached` helper was deleted. The
  instrument is that test, plus the live mount proof reading `Draining ops=[shut-tenant-path
  await-tenant-path-shut]` with the apiserver still answering.
- **REL-1.** `Probe` returned `(status, true)` for ANY answer, so a control plane whose `/readyz`
  answered 503 was read Reachable and stayed Ready. `reachable` is now `apiserverIsAnswering(code)`
  (200/401). The new `probe_test.go` pins 200/401 reachable and 503/500 not, failed before and passes
  after, and the clusterprovider and materialiser suites PASS. The instrument is `probe_test.go`.
- **SEC-1.** Service-auth tokens of any lifetime were accepted, and a 30-day token worked, so a stolen
  token stayed valid with no bound and no replay record. `MaxServiceAuthLifetime` (5 minutes) with
  `ErrLifetimeTooLong` now refuses one over the cap and accepts one exactly at it; the xrpcserver and
  gateway test mints were over the cap and moved to the constant. A `ponytail:` comment records that a
  validated token stays replayable until its short exp, with a jti set keyed by exp as the upgrade path.
  The instrument is the lifetime check's own tests, which must reject beyond the cap and accept at it.
- **LINT-1.** With staticcheck finally working, it read 16 findings: two tests whose assertion could
  never fail, a dead production constant, dead fields, and `time.Now().Sub`. All 16 were cleared, and
  the vacuous seam checks became compile-time interface assertions plus value checks. `staticcheck ./...`
  is now empty and exits 0. The instrument is `staticcheck ./...` printing nothing and exiting 0.
- **SEC-2.** The gateway served plaintext HTTP on whatever address it was given, including a public one.
  `load()` now refuses a non-loopback host: `0.0.0.0:8080` and `:8080` are refused, `127.0.0.1:8080`
  and `[::1]:8080` accepted. The instrument is a `load()` test over those four addresses, with a
  `ponytail:` comment naming the terminator ceiling.
- **COR-1.** `buildDeps` indexed `cfg.Audiences[0]` before the guard that checks the slice is non-empty,
  so the guard could never fire and nil audiences panicked instead of returning the written error. The
  length guard moved above the call. Nil audiences now returns the written error, pinned by
  `TestBuildDepsRefusesNilAudiencesWithoutPanicking`. The instrument is that test.
- **DEV-3.** The gateway's env file was undocumented and the handoff's settings count was short by
  seven. `deploy/socialweb-gateway.env.example` now lists all 16 `load()` settings, and
  `TestTheEnvExampleLoads` reads the constants out of `config.go` and fails when one is missing, naming
  it. The instrument is that test, which fails when a setting is deleted from the example.
- **SEC-4.** `install-kcp.sh` installed a kcp binary trusted on transport alone. `KCP_SHA256` is now
  pinned to the real sha256 of the v0.33.0 release tarball and checked with `sha256sum -c` before
  extraction, so a corrupt tarball exits 1 before install; there is no upstream signed manifest, so the
  pin was verified by direct download and hash. The instrument is the digest check itself, over a
  deliberately corrupted tarball.
- **REL-5.** The kcp unit used `After=network.target` with no `Wants=` and no `RestartSec`, so at boot
  kcp died on "no default routes found" and gave up after five fast restarts. The unit now uses
  `network-online.target` with `Wants=` and `RestartSec=5s`, installed through `install-kcp.sh`. Boot
  confirmation is operator-side and was not run here. The instrument is a reboot whose `systemctl
  status kcp` shows the unit started after the network came online.
- **REL-6.** The three live proofs used fake registries that answered 200/unconditionally from
  `Spec.Running`, so they could not detect the mount defect they exist to find. All three now answer
  401 live, 503 Draining and 502 reaped from host liveness and `DeletionTimestamp`, and none returns
  200; the live mount proof PASSes. Two honest limits are recorded: the 503 branch is unexercised and
  `subnetRegistry.MountStatus` is never invoked. The instrument is the three fakes answering from
  liveness rather than from `Spec.Running`.
- **DEP-2.** The container base was `fedora:44`, a bare tag, so the base could move under the same name.
  It is pinned to
  `fedora:44@sha256:2cdfedd312a3c92e0d9932ee18d9d2dd80b00fa0bb4ebdaa86736195b5eaa974`, the manifest
  index over four architectures, confirmed identical across buildx and skopeo reads. The instrument is a
  build that resolves the digest rather than the tag.
- **DEV-1.** The no-comments and no-non-ASCII rule had no instrument, so it read as held while broken:
  two `//nolint:gosec` comments violated it. `test/nocomments_test.go` now holds
  `TestNoCommentsAndASCII`, which walks `api`, `internal`, `cmd` and `test` with `go/parser` and fails
  on any comment that is not a `//go:` directive or in a `ponytail:` comment group, and on any byte
  above 0x7F. The two comments were removed. The test was watched failing on a scratch `// x` comment
  (`internal/scratch_comment.go:3:1`) and on a 0xE2 byte (offsets 26, 27 and 28), then passing.
  Invariant 16 now carries the test name. The instrument is that test.
- **REL-4.** "`go test ./...` all green" hid fourteen live tests that SKIP, the four-step proof among
  them, because the only reading anyone reports is the exit code, which is 0 on a SKIP.
  `internal/livegate` now holds `Require` and `Short`: `SOCIALWEB_REQUIRE_LIVE=1` turns every live skip
  into a `t.Fatalf` carrying the same message the skip would have. The before-and-after is two
  readings: `SOCIALWEB_REQUIRE_LIVE=1 go test ./cmd/socialweb-clusterprovider` exits 1 naming
  `TestTheFourStepProofRunsThroughTheCompositionRoot`, where it used to exit 0, and plain `go test
  ./...` still exits 0. The helper is imported only by `_test.go` files, so no production code can
  reach it. The instrument is that env-gated failure.
- **ITER-1.** `-short` did nothing: no test read it, so every edit either ran the live proofs or ran
  nothing, and `internal/clusterprovider` alone measured 71.15 s. `livegate.Short` now skips the long
  tests under `-short`, and `SOCIALWEB_REQUIRE_LIVE=1` overrides that skip into a failure, so the fast
  loop cannot be mistaken for the gate. The reading is `go test -short ./internal/clusterprovider`:
  71.15 s before, 2.28 s wall after, with `SOCIALWEB_REQUIRE_LIVE=1 go test -short` still failing. The
  instrument is `-short` over the long suite together with that override.
- **SEC-3.** Both units ran as root on the L0 host: the internet-facing gateway held uid 0 only to read
  `/var/.kcp/admin.kubeconfig`, and kcp needed no privilege at all. The gateway unit is now
  `DynamicUser=yes` with `LoadCredential=kcp.kubeconfig:/var/.kcp/admin.kubeconfig` and
  `Environment=SOCIALWEB_KCP_KUBECONFIG=%d/kcp.kubeconfig`; the kcp unit runs `User=kcp` with
  `/var/.kcp` owned by that user. `grep '^User=root'` over both files prints nothing, and
  `systemd-analyze verify` adds no new warnings over the pre-change units (its two gateway warnings
  pre-existed and are out of scope). One finding is kept because it changes what an operator must do:
  settings from an `EnvironmentFile=` OVERRIDE the unit's `Environment=`, so an installed env file that
  sets `SOCIALWEB_KCP_KUBECONFIG` would replace the credential path at run time.
  `deploy/socialweb-gateway.env.example` therefore does NOT set that key, and `TestTheEnvExampleLoads`
  now FAILS if the key is added back. The migration hazard is recorded rather than left latent: a host
  that ran kcp as root has root-owned 0600 files under `/var/.kcp` that block the new `kcp` user. The
  installed-unit reading is operator-side and was not run here. The instrument is the two greps, the
  verify comparison, and the test that fails when the key returns.
- **REL-7.** The `pasta` namespace holder is never recorded, so this entry asked whether a hard provider
  kill leaves it behind. Measured with real pasta and no code change, the answer is no: pasta SURVIVES
  a `kill -9` of the provider, but it lives only as long as its child, the agent, which exits when the
  VMM exits, and the VMM IS recorded and reaped, so the sweep tears the chain down and no netns holder
  persists. Before and after were the experiment itself: after the provider kill all three of pasta,
  the agent and the VMM were alive; after the VMM was killed all three were gone and `pgrep -a pasta`
  was empty, reproduced twice. The residual pre-record window became PASTA-1 in section 14. The
  instrument is that kill sequence with its output recorded.
- **SEC-6.** The guest writes the cluster join token as its k3s node password (`GuestTokenPath`,
  `/etc/rancher/node/password`, `<data-dir>/agent/node-password`), so one value carries two roles and
  every node of a cluster shares one node password. Recorded rather than latent, in section 9 with the
  v1 ceiling: harmless at 0-or-1 nodes, inherited by the first multi-node change, where a per-node
  password must be minted instead. The instrument is `grep -n 'node-password' docs/ARCHITECTURE.md`
  printing that section 9 line.
- **DEV-8.** This section claimed to hold no open gaps while the live backlog held eleven, which is the
  list-outliving-its-work defect in reverse: a list emptier than the work. It now carries the open list
  and names `NEXT.yaml` as the live backlog, and the instrument is `sed -n '/^## 14\./,/^## 15\./p'
  docs/ARCHITECTURE.md | grep -c 'PASTA-1'` printing at least one.
- **DEV-4.** Section 12 never named six live internal packages, and with no comments in code their
  purpose was stated nowhere: `atprotoauth`, `execplugin`, `nodeprovider`, `plcresolver`, `plcsweep`
  and `sweepkcp`. Each now has a line in section 12 stating what it does. The instrument is `for p in
  $(go list ./internal/...); do grep -q "$(basename $p)" docs/ARCHITECTURE.md || echo $p; done`
  printing nothing, where before it printed six lines.
- **DEV-6.** Four backlog entries described work already done (the memory file's salvage entry, the
  image-consumer wiring, the `nodeExpiry` producer and STEP 2 hammering), as did the code references to
  `internal/nodetransport` and `processrunner.Reach`, both gone. Each was struck in the memory file and
  in `handoff.yaml` with its evidence line; `handoff.yaml` had gone missing from the tree and was
  restored from this session's record. The instrument is `grep -n 'Record the salvage list'` over the
  memory file printing nothing under its "Open" heading, where before it printed one line.
- **DEV-7.** A 144-file `https:` kubectl cache tree had returned and `.gitignore`'s `/https:*/` hid it,
  turning a visible defect into an invisible one. The tree was deleted and the ignore removed, so a
  recurrence shows in `git status`. The producer is still unidentified and is recorded as unverified:
  no tracked Go or shell file sets a cache dir or runs kubectl with its home set to a server URL
  relative to the repo. The instrument is `test ! -e 'https:'` after a full `go test ./...` and one run
  of each deploy script.
- **DEV-5.** `README.md` pointed at a `LICENSE` file that did not exist, so the public-domain grant it
  claimed was not in the tree. The Unlicense text is now at `LICENSE`. The instrument is
  `test -f LICENSE && grep -q 'unlicense.org' LICENSE`, which before failed at `test -f`.
- **SEC-3 (follow-up).** `deploy/socialweb-gateway.env.example` was created by DEV-3 with all 16
  `load()` settings, and it carries the kubeconfig-key guard above: it does NOT set
  `SOCIALWEB_KCP_KUBECONFIG`, so an installed env file cannot override the gateway unit's credential
  path, and `TestTheEnvExampleLoads` fails if the key is added back.
- **PASTA-1.** A provider killed between `processrunner.Start` (`internal/nodefirecracker/node.go:98`)
  and `recordLocked` (line 112) left no `node.json`, so a fresh provider's sweep read `ErrNotExist` and
  reaped nothing, and the firecracker VMM plus its pasta netns holder survived. The fix was chosen by a
  measurement rather than a guess: real pasta was run and both `kill -9` and `kill -TERM` of pasta LEFT
  its child alive, while killing pasta's child made pasta exit, so recording pasta would not have closed
  the window; the process whose death cascades is pasta's child, the namespace child. The namespace
  child now writes its own single-process record (pid, start time and `NamespaceIdentity`, which is
  `["namespace", <dataDir>/namespace.json]`) as its first instruction, before it wires the namespace or
  starts a VMM, so the record exists even if the provider dies before doing anything; the provider's
  existing `recordLocked` still overwrites it with the VMM record on the normal path. `LivenessOf` and
  `Reap` now verify the recorded pid against EITHER the VMM identity or the namespace identity, still by
  pid, start time and cmdline, so reap-never-adopt is unchanged: a record matching no identity, carrying
  no identity, or with a mismatched start time is still left untouched. A stand-in test drove
  `Host.Provision` into the window (pasta stand-in forks, the child records itself, no VMM), then
  asserted a record existed, that it verified as `LivenessOurs`, and that `noderecord.Reap` killed the
  recorded pid. Before, `node_test.go:442: pasta started and its namespace child is running, but no
  record names that child: ... node.json: no such file or directory`, and the new tests are
  `TestReapKillsARecordedNamespaceChildOnlyUnderItsOwnIdentity` and
  `TestTheSweepReapsANamespaceChildAPreviousGatewayLeft`; after, PASS. The existing not-ours tests still
  pass and still assert nothing is signalled. One remainder stays uncovered and is stated: between pasta
  forking its child and the child reaching its first instruction there is still no record, but no VMM
  can exist yet and the child records itself regardless of the provider's death, so only a transient
  pre-exec shell can be orphaned. The instrument is the stand-in test that drives `Host.Provision` into
  that window and the sweep test that reaps a namespace child a previous gateway left.

- **The deployment is ONE container. The host runs a container runtime and nothing else.** Until this
  decision the repo shipped two substrates at once: the control-plane half was built and proven as a
  non-root container, while the DEPLOYED shape was host systemd units (`deploy/install-kcp.sh` for kcp,
  `deploy/socialweb-gateway.service` for the gateway), a manually started `socialweb-clusterprovider`,
  and each cluster's process group as children on the host. That split is what produced SEC-3 and REL-5
  -- two hardened host units -- and a list of operator steps (install the unit, `chown /var/.kcp`, point
  an env file at a credential path, reboot) that exist only because the process runs on the host. The
  deployment scenario is now ONE image and ONE `docker run`. The image starts kcp, the gateway, the
  credential plugin and the clusterprovider; the clusterprovider starts each cluster's postgres, kine
  and `k3s server --disable-agent` INSIDE the same container; the firecracker guest is the only thing
  outside a process boundary. The grant the node half needs is MEASURED and stated rather than
  described: the four-step proof passes inside the container with `--device /dev/kvm` (the VMM),
  `--device /dev/net/tun` (the guest's tap), `--security-opt seccomp=unconfined` (Docker's default
  profile blocks `CLONE_NEWUSER`) and `--cap-add NET_ADMIN,SYS_ADMIN` (the namespace wiring pasta and
  the tap need). That is wider than "one narrow grant" and is recorded as what it costs: no
  `--privileged`, no `--network host`, no host root mounted in. With those four, `check-host` answers
  `19 checks: 19 HOLDS, 0 MISSING, 0 MUST BE PASSED IN` inside the image. A narrower set is an open
  question to be measured, not assumed. The host systemd units are retired rather than maintained.
  The acceptance is the four-step proof running inside that container -- PASS, 99.12 s, with the node
  joining and being reaped -- because a deployment that cannot run the proof is not the deployment,
  and an operator step that only exists because of where a process runs is a step this removes rather
  than documents. The reachability gap is closed the same way: the gateway serves plaintext and binds
  container loopback, so the container terminates TLS itself -- a terminator on the container's
  published port forwarding to the loopback gateway, using an operator certificate when one is
  supplied and a self-signed one whose SHA-256 fingerprint is logged otherwise. The plaintext hop
  never leaves the container, SEC-2's refusal is untouched, and `docker restart` on the same volume
  now comes up Ready rather than refusing (the provider adopts its store before deriving the argv
  that names the DSN, and a recycled pid no longer looks like a live postmaster). Which grants the
  node half needs, the terminator's ceiling (`socat`, single-backend, no client certificates) and any
  narrower grant set are stated as measurements to make, not as assumptions.
- **Consequence of the one-container shape, stated rather than discovered.** The container's network
  namespace is its own, so the subnet allocator sees fewer networks than the host does and its
  host-network overlap refusal is narrower than the host's (a stated limit, closed with the container,
  "Items closed by later work" above); the allocator's `HostNetworks` seam is where a real collision
  would be handed in. `liveports.LockDir()` is likewise per-container (the second limit, closed the
  same way). Neither has a production path that depends on it. The node
  image is built inside the image, so the host needs no `/lib/modules`, no kernel and no firecracker
  binary at run time; the build still reads this host's kernel and module tree, which is what makes the
  guest's module set measured rather than assumed.

- **The two-level vault layout is DECLARED in Go, and the unseal policy is DECIDED rather than
  deferred. This entry states where each secret lives, what unseal costs, and the one conflict it
  does not resolve.** The layout is `internal/vaultlayout`, plain Go values later code turns into
  API calls, and `vaultlayout.Validate` FAILS on a policy referencing a path no mount provides, a
  consumer with no role, a role referencing a missing policy, a PKI role whose max TTL exceeds its
  mount's, or a secret class assigned to NEITHER vault or to BOTH. The test drives every one of
  those to a failure, so the check is known to be able to fail.

  **ROOT vault - one per deployment, in the container beside kcp, on the state volume.** It holds
  the cross-deployment credentials and nothing per-cluster: the gateway's kcp admin kubeconfig for
  kcp's own API (`<KCP_ROOT>/admin.kubeconfig`, token-only, `shard-admin`, which never rotates -
  ADR 0020), kept at `secret/data/socialweb/kcp/admin`; kcp's adopted mechanism-B material - the
  `client-ca` certificate kcp is handed as `--client-ca-file` (ADR 0027) and the `.admin-token-store`
  hash of the `shard-admin` token, at `secret/data/socialweb/kcp/client-ca` and
  `secret/data/socialweb/kcp/admin-token-store`; a central-store credentials slot at
  `secret/data/socialweb/store/rustfs` left from the unbuilt rustfs design - no `kine` rode it, each
  cluster's store is its own postgres (section 4), and plan 0006 M1-STORE-CLEANUP deleted it; the deployment's ATProto
  device-key audience and registry binding (`SOCIALWEB_GATEWAY_DEVICE_KEY_AUDIENCE`,
  `SOCIALWEB_GATEWAY_DEVKEY_REGISTRY`), at `secret/data/socialweb/devicekey`; and the container TLS
  terminator's operator certificate and key (`SOCIALWEB_CONTAINER_TLS_{CERT,KEY}`), at
  `secret/data/socialweb/container/tls`. The root vault also carries the PKI mount `pki/`, whose
  role `kcp-client` issues the gateway's scoped client certificate under the `socialweb.local`
  domain, client-auth only, key usage digital signature plus key encipherment, `max_ttl` 720 h
  against the mount's 8760 h. The CA private key is generated inside that mount and never readable;
  only its certificate is handed out. Root vault unseal material lives OUTSIDE the vault,
  operator-held.

  **CLUSTER vault - one per ON cluster, started in that cluster's process group beside the
  embedded-postgres ADR 0025 sequences.** It holds per-cluster identity custody and nothing
  cross-deployment, so its KV paths are cluster-relative: the 13-entry identity bundle at rest at
  `secret/data/k3s/identity`, the exact `identitybundle.Paths` list section 7 measures; the instance
  join token at `secret/data/k3s/instance-token`; the postgres role and password (`postgres.pw`,
  section 5) at `secret/data/k3s/postgres`; and the per-cluster SSH key at `secret/data/k3s/ssh`,
  kept only while the substrate that dials a market guest over SSH is in use. The tenant credential
  is not here: it is derived from the bundle's `client-ca.{crt,key}` at mint time, never stored
  (section 7, ADR 0023).

  **The unseal policy, decided.** Both vaults auto-unseal from a key file on the state volume that
  holds their data, written `0600` and read by the process that starts the vault. Stated plainly
  rather than buried: this is a real weakening. An attacker who reads the state volume gets the
  ciphertext AND the unseal key in one read, so the vault's encryption is integrity at rest against
  a stolen disk, not confidentiality against a host that is already compromised; on the cluster
  vault the volume is the cluster's own data directory, which the gateway process already holds.
  What makes it acceptable at v1 is that the volume is the deployment's own state, the container is
  the trust boundary, and the alternative - operator-held Shamir shares - would make every cold
  start a manual step, which for a cluster vault that comes up per ON cluster is an operator page
  per cluster. What would change it: the day a vault secret's compromise outlives the host it sits
  on - a root secret a tenant can reach, a CA key used to forge rather than to restore - the root
  vault moves to Shamir shares held off-host or to a KMS/HSM seal, and the cluster vault follows
  only if its bundle's loss stops being recoverable by re-provisioning the cluster. `ponytail:`
  auto-unseal from a file on the same volume makes the vault exactly as strong as the volume; move
  the unseal file to a separate trust domain when a vault secret's loss exceeds the cost of an
  operator-held unseal.

  **The conflict, stated not resolved.** ADRs 0020, 0023 and 0025 decided the identity bundle's
  durable home is OpenBao: ADR 0023 records the harvest into `secret/data/k3s/<cluster>/identity`,
  and ADR 0025 puts the `sha256` bundle verify between `Vault.Ready` and `k3s start`. Sections 5,
  7 and the entry below decided the opposite, and the CODE does today: the bundle is the file tree
  at `<DataDir>/server/tls` plus `cred/`, seeded and harvested by `identitybundle.Harvest` and
  `identitybundle.Seed`, because k3s reads `client-ca.key` and its siblings from disk at RUNTIME -
  `kube-controller-manager`'s `cluster-signing-*-client-key-file` and k3s's own cert handlers
  (ADR 0020) - and a cold start with the key absent OVERWRITES `client-ca.crt` with a fresh,
  self-signed CA. OpenBao can therefore be the bundle's AT-REST CUSTODY and its wake-time source,
  never its runtime location. "OpenBao secures everything" means every secret has a vault home and
  a least-privilege reader; it does not mean k3s reads its identity from a vault, and it does not
  mean the file tree is going away.

- **The two-level vault is real, not a launcher.** OpenBao was in this repository as a launcher that
  was never switched on: config rendered, argv, a lease, bring-up order and a readiness gate, and
  nothing that initialised it, unsealed it, enabled an engine, wrote a policy or role, or consumed a
  secret. The deployment image did not even carry the `bao` binary. Both levels now run and hold
  secrets, and the measurements are these, all taken in the one container.
  - **Root vault**, one per deployment, a child of the entrypoint on the fixed loopback port 8200
    (`internal/rootvault`), storage on the state volume. It is initialised 5-of-3, unsealed, and
    carries `secret/` as kv-v2 and `pki/`; the policies and AppRoles are the ones
    `internal/vaultlayout` declares, and the gateway's cross-workspace kcp admin kubeconfig is stored
    at `secret/data/socialweb/kcp/admin`. `cmd/socialweb-vault apply-root` does this after kcp is
    ready and before the gateway starts, and fails the container if it cannot. The unseal material is
    `$STATE_DIR/openbao/root-unseal.json`, mode 0600. Least privilege was measured rather than
    asserted: a token minted through the gateway AppRole reads its own secret and is refused 403 on
    another path. Re-running `apply-root` succeeds and leaves the PKI root unchanged. Its listener is
    MUTUAL TLS, not plaintext (VAULT-2 below): the entrypoint's `socialweb-vault prepare-root` step
    generates the deployment CA, the server certificate and the client certificate under
    `$STATE_DIR/openbao/tls/` before `bao server` starts, and the listener stanza carries
    `tls_require_and_verify_client_cert = true` with `tls_client_ca_file` pointed at that CA, so a
    client with no certificate, or one from another CA, is refused in the handshake.
  - **Cluster vault**, one per ON cluster, in the per-cluster process group on its leased port (20001
    in the run below, beside postgres 20000 and k3s 20003). It is initialised and unsealed, carries
    `secret/` and `pki/`, the cluster policies `identity-bundle`, `instance-token`, `postgres` and
    `ssh`, the AppRoles `cluster-data`, `cluster-identity` and `cluster-node`, and it holds all 13
    identity-bundle entries at `secret/data/k3s/identity`. Its PKI engine IMPORTS the cluster's own
    `client-ca` so issued certificates chain to the CA the tenant apiserver already trusts, and the
    tenant client credential is now issued from the vault: `TestTheControlPlaneReachesReadyAtZeroNodes`
    passes with a vault-issued credential, and a test proves the leaf chains to the imported CA. The
    unseal material is `<cluster data dir>/openbao/unseal.json`, mode 0600.
  - **What is honestly not yet true, stated rather than implied.** The unseal material lives on the
    same volume as the ciphertext, so the vault is integrity-at-rest against a stolen disk and not
    confidentiality against a host that can read the state volume; k3s still reads `client-ca.key` and
    its siblings from the file tree at `<DataDir>/server`, so the vault is durable CUSTODY and a
    wake-time source, never the runtime location -- the ADR-versus-architecture conflict recorded
    above is exactly this; the vault's store is now the postgres backend on the cluster's own
    embedded-postgres, database `openbao`, and `procgroup`'s refusal to start OpenBao without that
    store is a dependency met on every path -- a deployment whose kine store is external still starts
    the postgres for the vault, so "no local postgres" no longer means "no vault"; three root secret
    classes (`kcp-admin-token-store`, `kcp-client-ca`, `central-store-credentials`) have no producer in
    the container yet and are logged with named reasons rather than faked; and the layout's `ecdsa`
    key type is translated to OpenBao's `ec` at the boundary because the PKI role endpoint rejects the
    former. `tenant-superuser` is an extra PKI role beyond the layout, added because the declared
    `kcp-client` role structurally cannot issue the `system:admin` common name.
  - **Terraform was not used.** Section 5 states the rule: everything is a controller-runtime
    reconcile loop or a deploy config, and no new language. The vault configuration is Go, driven by
    `internal/vaultlayout`'s declarative layout, applied by `cmd/socialweb-vault` and by the
    clusterprovider's vault step.
- **`apply-root` presents a client certificate to the root vault, the transport the cluster vault
  already had. CLOSED.** Before, the root-vault config rendered `api_addr = "http://127.0.0.1:8200"`
  and `tls_disable = true`, and `cmd/socialweb-vault` reached it with `openbao.New(addr, token)` over
  plaintext, so the cross-deployment credentials -- the kcp admin kubeconfig and the device-key
  binding -- rode a transport any local process could dial. After, `internal/rootvault.ServerConfig`
  renders `api_addr = "https://127.0.0.1:8200"` and one listener with `tls_disable = false`,
  `tls_cert_file = "$STATE_DIR/openbao/tls/server.crt"`, `tls_key_file =
  "$STATE_DIR/openbao/tls/server.key"`, `tls_client_ca_file = "$STATE_DIR/openbao/tls/ca.crt"` and
  `tls_require_and_verify_client_cert = true`. The deployment's own CA -- not any cluster's -- signs
  BOTH the server certificate, whose SANs cover `127.0.0.1` and the container's hostname, and the
  single client certificate `cmd/socialweb-vault` presents. `vaultcert.EnsureRootVaultTLS` reuses the
  per-cluster CA and signing code rather than a second crypto implementation, writes `ca.crt`,
  `server.crt`, `server.key`, `client.crt` and `client.key` at mode 0600 under a 0700 `tls/`
  directory, and reuses them across restarts, regenerating only when a certificate is inside the
  renew window or a required SAN is gone. The entrypoint's `prepare-root` step generates the material
  and the config before `bao server` starts; `apply-root` loads the client certificate and the CA from
  the state volume. `internal/openbao` gained `WithClientCertificate(tls.Certificate)` as the client
  half of the trust `WithCACertPool` already carried, and the one `apply-root` client and the layout
  API's own HTTP client present it. The root token remains the AUTHORISATION layer for the admin
  operations; the certificate is transport, not authority, and was not weakened to make mTLS easier.
  The readings, all on this host with bao v2.6.2: **(a)** the rendered config carries TLS on and no
  `tls_disable = true`; **(b)** a client presenting NO certificate is refused in the handshake
  (`remote error: tls: certificate required`); **(c)** a client certificate from a DIFFERENT CA is
  refused (`remote error: tls: unknown certificate authority`); **(d)** `apply-root` completes
  presenting the generated certificate and a second run leaves the `pki/ca/pem` bytes unchanged;
  **(e)** two `EnsureRootVaultTLS` calls return the same CA and the same server certificate, the
  server leaf chains to that CA for `serverAuth` and the client leaf for `clientAuth`, and the SANs
  cover `127.0.0.1` and the test hostname (the control that removes a covered host flips the reading
  and regenerates). `SOCIALWEB_LOCAL=1 go test ./cmd/socialweb-vault -run
  'TestTheRootVaultRefusesClientsWithNoOrAnotherCertificate|TestApplyRootPresentsTheGeneratedClientCertificateAndIsIdempotent'`
  PASS 0.11 s and 0.18 s; `deploy/check.sh --local` 0 fail, 13 artifact-gated skips; the end-to-end
  gate `deploy/test-rig/run.sh` exits 0 on the committed tree with **761 PASS, 0 FAIL, 0 SKIP** and
  `PROOF: the live gate passed with no skips`, which is the proof the mutual-TLS path runs in the
  product image: the entrypoint's `prepare-root` and `apply-root` both completed before the gateway
  started, and the two live tests pass 20 times over under `-count=20` in that image.
- **The dev loop was measured, and it was rebuilt around the measured cost.** Across 32 subagent
  logs the loop spent its time in four places: 72 poll loops containing `sleep`, each blocking a
  turn for 300-400 s; 94 `docker logs`/`exec`/`inspect` calls, most of them inside those loops; 49
  reads of this document, then 2331 lines; 8 failed edits; 3 subagent calls that died on the
  nesting depth limit; and 2 sessions that ended with a build still running. Four changes followed.
  The loop is fast by default and the gate can fail: `deploy/check.sh --fast` (about 9 s end to
  end; a bare `go test -count=1 ./...` is 5.6 s, where it was 73.8 s before), `--local` for the
  proofs this host carries (about 75 s, and it NAMES each of the 14 artifact-gated tests it
  skipped), and `--full`, which fails on any skip of a live input and therefore only passes where
  the artifacts are present -- on this host it exits non-zero naming the missing firecracker and
  kubeconfig inputs. An earlier revision of this change made the artifact-gated `Require` skip
  even under the full gate, which is the REL-4 defect again: a function that cannot fail is not a
  gate. `Require` now fails under `--full`, `RequireLocal` fails under `--local` when an input the
  host is assumed to carry is absent, and the skip it can take is named rather than silent. A readiness helper replaces the sleep loops:
  `deploy/wait-ready.sh <container> [timeout-seconds]` polls `docker logs` at 1 s under a hard
  timeout (default 60), returns 0 on this provider's own readiness line
  (`"msg":"cluster provider: status"` with `"phase":"Ready"`), and prints the last 40 log lines and
  exits non-zero on timeout or container exit. Measured on this host: a container whose stack
  reached Ready returned 0 in 30 s (30.03 s wall); a container that never emitted the line returned
  1 in 3.1 s with its last log lines, where the loop it replaces would have blocked for 300-400 s
  and still not failed; and a container that exited first was caught immediately on the same
  non-zero path. A working-rules file, `AGENTS.md` at the repo root, names the one small file that
  answers each question -- the objective, the layout, the invariants, the decisions, `NEXT.yaml`,
  the clusterprovider and gateway environment, how to run the container -- so a subagent reads one
  file instead of ten large ones and instead of this document end to end, and states the loop
  rules, the edit discipline, the no-work-in-flight rule and the standing constraints. The node
  image build is now layered so it is not paid on every container build: the `nodeimage` stage
  COPYs only the inputs the guest agent build reads (`go.mod`, `go.sum`, `cmd/socialweb-nodeagent`,
  `api`, `internal`) instead of the whole repo, so the 48-module check and the 4 GiB image build
  stay cached across unrelated edits. Measured: a `build.sh computer` that re-ran the nodeimage
  stage took 62 s; the next one took 13 s with `[nodeimage] RUN` and `[extract] COPY` reported
  CACHED; editing `cmd/socialweb-gateway/main.go` took 49 s and left the nodeimage stage CACHED
  while only `gobuild` re-ran. The ceiling is stated where it belongs: if the node agent ever
  imports a new top-level package outside `api`/`internal`/`cmd`, the agent build fails loudly and
  that directory must join the COPY list.
- **The per-node mTLS work broke the deployment it was meant to secure, and the fix is one
  listener, not two.** The first shape gave the cluster vault a second listener on the cluster's
  gateway address so the guest could reach it. Binding that address needs root or
  `net.ipv4.ip_nonlocal_bind`, so `bao` exited, the loopback listener died with it, and the
  container sat at `phase: Initializing` for ever -- measured on the documented USER-1000 run as
  `openbao never became ready: ... dial tcp 127.0.0.1:20001: connect: connection refused`. The
  same shape also failed the guest for a different reason: pasta forwards guest traffic to the
  host LOOPBACK, where the plaintext admin listener sat, so the guest got `server gave HTTP
  response to HTTPS client`. One listener on `127.0.0.1:<leased port>` with TLS ON fixes both:
  the certificate carries SANs for `127.0.0.1` and the gateway address, the admin client speaks
  HTTPS and trusts the vault CA (`internal/openbao.WithCACertPool`), and the guest still dials
  the gateway address because pasta lands it on loopback. Measured after: the container reaches
  `phase: Ready` in 30 s as `uid=1000(kcp)` with no `--user 0`, no sysctl and no host route
  (`deploy/wait-ready.sh` exited 0 in 29.99 s), and a real firecracker guest read its own secret
  through CERT auth -- `KCP_AGENT_VAULT node=socialweb-node1 path=secret/data/k3s/node/socialweb-node1
  state=read`. The lesson is the batch's own: the change that added a listener for a guest was
  never run as a guest or as USER 1000 until it had already broken both.
- **External Secrets Operator is wired to the cluster vault, with two costs stated.** The
  `ClusterSecretStore` and `ExternalSecret` are applied to the tenant apiserver by the
  clusterprovider after `Bootstrap` (`internal/eso`), reading `deploy/eso/*.yaml` from the image
  and pointing at `https://<gateway>:<port>` with a client certificate minted from the cluster
  vault. Two things had to be true and are recorded rather than hidden. Upstream ESO's two
  validating webhooks default to `failurePolicy: Fail`, and their pods are Pending while the
  cluster has zero nodes, so the apiserver rejected the `ClusterSecretStore` with `no endpoints
  available`; the deployed bundle sets `failurePolicy: Ignore`, whose cost is that CRD validation
  is best-effort while the controller is down. And the ESO identity's policy had to gain
  `auth/token/lookup-self` (read) and `auth/token/revoke-self` (update), because the provider
  validates its token before reading a secret and the CERT role disables the default policy;
  without them the store reported `InvalidProviderConfig` with a 403. The ESO identity is still
  denied a node's secret path and the identity bundle, and a node's identity is still denied
  `lookup-self`. The controller runs at one node and is Pending at zero; end-to-end
  materialisation of `Secret/users/openbao-demo` is now MEASURED. Two things stood between the
  accepted objects and a materialised Secret, and both are closed. First, nothing authored the value
  the ExternalSecret reads: `vaultcert.WriteESODemoSecret` existed but was called by no production
  path, so `secret/data/k3s/eso-demo` was empty and the ExternalSecret had nothing to copy. The
  clusterprovider's tenant add-ons now write it (`{"demo": "openbao:<cluster>"}`) before minting the
  ESO certificate. Second, the tenant grant's `socialweb-mint-identity` ValidatingAdmissionPolicy
  admitted only `system:admin` to create Secrets in `users`, so the External Secrets controller
  (`system:serviceaccount:default:external-secrets`) was refused and the ExternalSecret read
  `Ready=False reason=SecretSyncedError`, message `secrets "openbao-demo" is forbidden:
  ValidatingAdmissionPolicy 'socialweb-mint-identity' ... denied request`. The policy's Secrets arm
  now also admits that one controller ServiceAccount: it is the provider's materialiser, not a tenant
  credential, and no tenant subject was added for either verb. The arm names the ESO install namespace
  `default`, which is where `deploy/eso/install.sh` puts the controller; installing ESO in another
  namespace needs this arm and the message updated to match. Measured with a container built from
  this tree and a fresh volume, with no hand apply of any object: `socialweb-node1` Ready,
  `deployment/external-secrets` 1/1, the store `Ready=True reason=Valid` ("store validated"), the
  ExternalSecret `Ready=True reason=SecretSynced` ("secret synced"), `Secret/users/openbao-demo`
  (ON A FIRECRACKER CLUSTER: the store's URL is the cluster's subnet gateway, which that substrate
  binds and routes and `market` does not -- see ESO-3 in section 14, measured failing on production)
  `data.demo` = base64("openbao:socialweb"), and `demo` = `openbao:socialweb` at
  `secret/data/k3s/eso-demo` in the cluster vault -- the Secret equals the vault value. The node was
  booted by the scale driver rather than a test harness: ESO's Deployment is not addon-owned, so its
  Pending pods read as intent and the next node-scale pass provisioned a firecracker guest that joined
  as `socialweb-node1`, which is where the controller then scheduled. Instrument correction: the
  acceptance test names a store condition of type `Valid`, which ESO v2.11 does not emit -- it uses
  `Ready` with `reason=Valid` -- so `...conditions[?(@.type=="Valid")].status` reads empty even when
  the store is valid; the same jsonpath with `@.type=="Ready"` returns `True`.
  Applying 25 CRDs needs `--server-side --force-conflicts`, because two of ESO's CRDs exceed the
  262144-byte client-side annotation limit.
- **The tenant add-ons applied an External Secrets store to a cluster no operator was installed
  into.** The pass applies the `ClusterSecretStore` and the `ExternalSecret` through `internal/eso`,
  and ESO's own install lives in `deploy/eso/install.sh` -- a script the image carries and NOTHING
  ran: no Go path exec'd it, the container entrypoint has no ESO step, and
  `SOCIALWEB_CLUSTERPROVIDER_ESO_MANIFEST_DIR` names only the directory the two YAML files are read
  from. On a cluster the provider brings up, the apiserver therefore served no
  `external-secrets.io/v1` and answered the store apply with `the server could not find the requested
  resource`: measured in production on a market cluster as `clusterprovider: installing the tenant
  add-ons for test-cluster-alice-0001 after its bootstrap: eso: applying the ClusterSecretStore
  openbao: the server could not find the requested resource`, once every 2 s, with `kubectl get crd`
  against that tenant apiserver answering `No resources found`. The pass now reads the store resource
  itself (`eso.Installed`: one `List` of `clustersecretstores` with `Limit: 1`), and a 404 from it
  means the operator is absent, so the pass runs the shipped installer against the tenant apiserver
  with a kubeconfig rendered from the provider's own tenant credential
  (`clusterprovider.Kubeconfig`) and re-reads the resource BEFORE applying the store; a resource that
  answers means the installer does not run again, so a steady cluster pays one `List` per pass. The
  probe is deliberately a RESOURCE read and not a discovery read: the same request is the one the
  production failure is made of, so its classification is the measured one, and the alternative was
  measured to be wrong on this deployment -- kcp answers every `/apis*` path with
  `forbidden: User "kcp-admin" cannot get path "/apis/external-secrets.io/v1": access denied` for a
  tenant credential, so a `ServerResourcesForGroupVersion` probe would have turned a 404 into a 403
  and failed the pass for a second reason. A cluster the installer cannot reach now fails with a
  message naming the group and the installer's own output rather than with an unknown kind. Two costs
  are stated rather than hidden: the installer fetches the pinned upstream bundle (v2.11.0) from its
  release URL at tenant bring-up, so a provider host with no route to github.com fails the pass with
  curl's error naming the host, and `ESO_BUNDLE` is the escape hatch that points it at a local copy;
  and `bash`, `curl` and `kubectl` are inputs of this step, all three carried by the image today.
  Watched to fail: with the install moved after the apply,
  `TestTheOperatorIsInstalledBeforeTheStoreIsApplied` recorded
  `secrets,clustersecretstores,externalsecrets,install` against the wanted
  `install,secrets,clustersecretstores,externalsecrets`, and `TestAFailedInstallLeavesTheStoreUnapplied`
  showed the store going out over a cluster with no operator. `NEXT.yaml` ESO-3.
- **The clusterprovider's test package did not build, so the loop's own gate could not run.** Commit
  c57d870 added the `CredentialSource` parameter to `MintTenantCredentialForDataDir` and left two
  calls in `internal/clusterprovider/vault_tls_growth_test.go` at three arguments, so `go vet` and
  `go test ./...` refused the package on `main` as well as on this branch, and what a reader would
  have seen is a compile error rather than a test result. Both calls name
  `CredentialSourceVaultIfSealed`, which is the path that test exercises: it mints against a running
  vault after the tenant add-on pass has asked the vault's certificate to cover one more host.

- **The container build now produces the qemu node image too, and the live gate runs inside a
  privileged, ephemeral rig. SETTLED 2026-09-25.** Before: the `nodeimage` stage built one image from
  one configuration, so the qemu image was a host-built artefact a fresh container did not have; and
  `SOCIALWEB_REQUIRE_LIVE=1 go test ./...` had no self-contained way to run, because the live inputs
  are paths -- the two images, the two VMM binaries, the agent -- that only a host holding the
  artefacts could name. After: a second stage, `nodeimage-qemu`, builds the qemu image from
  `nodeimage-qemu.json`; `extract` copies both stages' `/dist` into one root; the `computer` image
  carries both images and both runtime configurations. `cmd/socialweb-rig-inputs` derives the inputs by
  reusing `internal/nodefirecracker.LoadImage` and the builder's own configuration parsing rather than
  restating the fingerprint, and `deploy/test-rig/run.sh` starts the image through the product's own
  `container-entrypoint.sh` and runs the gate inside it. MEASURED 2026-09-25: one `build.sh computer`
  produced the firecracker image
  `sha256-6f0163f5b34f4b7c793709cdbb90c74a9df700466f9d8ed6c9dedb2569319e3e` and the qemu image
  `sha256-e356da745efe1dab4b6c233045f33b491cc7cfe0851fdaa5856501991a565afd` in 122 s wall, and the
  runtime configurations resolved to the same two names inside the container, which is the cross-check
  that the build-time and runtime configurations fingerprint alike. The product stack was Ready in 33 s;
  the qemu guest boot `TestALiveQEMUGuestBootsAndIsReaped` passed in 0.93 s and the firecracker guest
  boot `TestLiveTheGuestsGatewayIsAnsweredOnItsOwnLink` passed in 2.47 s. The security boundary is
  unchanged and stated where it was: the relaxed flags (`--device=/dev/kvm --device=/dev/net/tun
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined`, with no added capability)
  exist only on the rig's own command line, and the strict `validate.sh` proof was re-run against the
  new image as non-root with no device, no capability and no host network, printing
  `PROOF: --- PASS: TestTheControlPlaneReachesReadyAtZeroNodes (8.08s)`. NOT VERIFIED: the whole
  `SOCIALWEB_REQUIRE_LIVE=1 go test ./...` gate was not run to a passing verdict, because a tenant
  cluster and its identity inputs are the operator's; the rig names the eight underivable variables and
  the tests they gate, and runs a named subset on request.

- **Stopping a process that had already exited returned its non-zero exit status. CLOSED
  2026-09-25.** `processrunner.Process.Stop` signalled the child, then waited, and swallowed a
  non-zero exit only when the signal was DELIVERED. When the child had already exited, `Signal`
  returned `os.ErrProcessDone` and `Stop` returned `Wait`'s raw `*exec.ExitError`, so stopping a
  node whose process was already gone failed with `exit status 1`. Before: in the container rig,
  `TestALiveQEMUGuestBootsAndIsReaped` failed on teardown with `nodeqemu: stopping the process
  hosting node liveweb-node1's guest: exit status 1` in two consecutive runs (0.90 s and 2.40 s),
  including with `-p 1`, while every `internal/nodefirecracker` live test passed. The cause is the
  ORDER inside `nodeqemu.Teardown`: it reaps the guest VMM by identity, the guest's death takes
  pasta with it, and only then is `Stop` called on pasta, which has already exited. The host path
  passed the same code because pasta was still alive when it was signalled, so the
  `*exec.ExitError` branch swallowed the same non-zero status. After: the `os.ErrProcessDone`
  branch falls through to that same wait-and-swallow path, so a process that has already exited is
  stopped successfully and only a process that refuses to die returns an error. MEASURED
  2026-09-25: the unit test `TestStopSucceedsWhenTheChildAlreadyExitedNonZero` (a child that exits
  7, waited for, then `Stop` must return nil) failed before with `Stop: exit status 7` and passes
  after; in the rig `--- PASS: TestALiveQEMUGuestBootsAndIsReaped (0.91s)` and the combined
  `SOCIALWEB_REQUIRE_LIVE=1 go test ./internal/nodeqemu ./internal/nodefirecracker -count=1 -p 1`
  passed both packages with no `--- FAIL`; on the host
  `TestTheFourStepProofRunsThroughTheCompositionRoot` passed both substrates in 218.68 s
  (firecracker 99.76 s, qemu 118.92 s). The rig entry above recorded the qemu boot as passing in
  0.93 s before this fix: where it passed, pasta had not yet exited when it was signalled, which is
  the timing dependence the fix removes.

- **The change that made the loop fast broke the container's own validation, and the gate could not say
  why.** Reducing the default test run to the fast loop made `RequireLocal` skip unless
  `SOCIALWEB_LOCAL`, `SOCIALWEB_LIVE` or `SOCIALWEB_REQUIRE_LIVE` is set. `deploy/control-plane-container/validate.sh`
  drives `TestTheControlPlaneReachesReadyAtZeroNodes` with none of them, so the test SKIPPED and the
  script failed every run -- the instrumentation that proves the container was disabled by the change
  that was meant to speed it up. It was found not by running it but by the qemu agent reading the
  parent tree against its own. Fixed by passing `-e SOCIALWEB_LOCAL=1` into that container run:
  `validate.sh` now prints `PROOF: --- PASS: TestTheControlPlaneReachesReadyAtZeroNodes (8.39s)` and
  exits 0, still showing the cluster vault holding all 13 identity-bundle entries. The second half of
  the same defect was that `check.sh --full` printed only the `--- FAIL` lines, so an operator saw a
  test name and not the input it wanted: it now prints each failing test's own message, which names the
  missing variable, as `--full` does above on this host. The instrument now says what it needs.

- **CHK-1 was refuted before it was worked, and the refutation is the entry.** A build log printed
  `MISSING host.qemu`, and an item was opened saying `check-host` misreports a host that runs qemu
  guests. Running the check instead of reading the line settled it: on this host `check-host -config
  nodeimage.json` answers `21 checks: 21 HOLDS, 0 MISSING`, and inside the product image the line reads
  `HOLDS host.qemu` with the binary present (installed at Dockerfile:145-146 and symlinked into
  `/usr/local/bin`). The MISSING line came from the nodeimage BUILD stage, which installs only `tools`
  and needs no qemu, and `-build` is explicit that only the checks a build reads decide its exit
  status (`main.go:71-72`), so the stage's 10 MISSING are runtime facts it does not need. No code
  changed. The lesson is the batch's oldest one applied to a claim rather than to code: the item was a
  sentence greener than the fact, and an item is not real until the artefact has been run.

- **The vault's store was `file` while the design, the guard and the deployment all said the
  per-cluster postgres.** Section 6 states the vault's store is its own embedded-postgres and section
  9 recorded the file backend as being on an expiry, but `OpenBaoServerConfig` rendered `storage
  "file"` under `<data dir>/openbao/data`. The guard in `procgroup` refused OpenBao unless the local
  postgres was up -- true of the design, false of the config as rendered -- and `PurposesFor` and
  `openStore` leased and started no postgres when `SOCIALWEB_CLUSTERPROVIDER_STORE_ENDPOINT` named an
  external store, so such a deployment silently started no vault at all. BEFORE: the rendered config
  carried `storage "file" { path = "<data dir>/openbao/data" }` and a fresh vault's health body read
  `Storage Type file`; a stack with an external store spawned no postgres and its vault step never
  came up. AFTER: the config carries `storage "postgresql" { connection_url = ... }` and the health
  body reads `Storage Type postgresql`; the vault's database is `openbao` while kine's is `k3s`, so
  `TestTheClusterVaultKeepsItsDataAcrossARestartOnPostgres` finds `openbao_kv_store` in `openbao` and
  `to_regclass('openbao_kv_store')::text = 'absent'` in `k3s`; the same test starts a vault, unseals
  it, writes a KV value, restarts the vault on the same store and reads the value back;
  `TestAnExternalKineStoreStillStartsThePostgresTheVaultNeeds` shows an external kine store leaving
  kine on the operator's endpoint while the per-cluster postgres still starts for the vault;
  `procgroup` still refuses a vault whose store is not up, and its message now names the store; and
  `TestAVaultPointedAtAnotherClustersStoreDoesNotAuthenticate` shows a vault holding one cluster's
  unseal material cannot open another cluster's store. Instrument: `deploy/check.sh --local` on this
  host runs the vault tests against real postgres and bao (13 artifact-gated tests named as skipped,
  none of them these), and the live gate runs them with `SOCIALWEB_REQUIRE_LIVE=1`.

  The backend name and its key were read off the bundled `bao` v2.6.2 rather than remembered:
  `storage "postgres"` answers `unknown storage type postgres`, and `storage "postgresql"` with a DSN
  starts and its health body reports `Storage Type postgresql`. The decoupling chosen for an external
  store is to KEEP running the per-cluster postgres for the vault: section 6 makes that postgres the
  vault's store, so a deployment whose kine rides central S3 still owes the vault its own store, and
  silently starting no vault was the one outcome wrong under either reading. `Stack.NeedsLocalPostgres`
  is true when kine writes locally OR the Stack contains OpenBao, and only a stack with neither skips
  the postgres; a nil `VaultStoreDSN` for a vaulted stack is refused by name (`ErrNoVaultStore`).

  MIGRATION, both paths stated. The clean path is `bao operator migrate` from the old `storage "file"`
  config to the new postgres one, which preserves the root token and every secret; it needs the old
  config, the new one and a stopped vault. The path IMPLEMENTED is the cheaper fallback, which needs
  no code: the postgres store comes up uninitialised, `EnsureVault` sees `initialized:false` and
  re-initialises, writing new unseal material and a new root token, and the next guarded pass
  re-harvests the 13-entry identity bundle that still sits in the file tree at `<DataDir>/server`.
  Its consequence, stated rather than improvised: the vault is RE-AUTHORISED (new root token, new
  unseal keys) and the CAs are UNCHANGED, because the bundle comes from the file tree, not from the
  vault; the old file store is left on disk at `<data dir>/openbao/data` and is no longer read.

- **The privileged gate could run against a tree that moved under it, and that is how it lied about the
  vault store.** `deploy/test-rig/run.sh` mounts the repo read-only at `/src` and runs `go test ./...`
  from it, so the tree it tests is the tree on disk, changing while it runs. The vault agent launched
  the rig before committing; the run reported
  `FAIL: TestAnExternalKineStoreStillStartsThePostgresTheVaultNeeds` with `postgres on port 20000
  exited with exit status 1` -- 20000 being the port the live deployment holds in that same container --
  against a version of the test that never became a commit. The committed test leases
  `liveports.PostgresWindows` (31300-31399), which cannot hand out 20000, and it PASSes in 1.73s; the
  re-run against the committed tree read `1026 PASS, 0 FAIL, 0 SKIP`. The guard is now structural rather
  than a habit: the rig refuses a dirty tree unless `KCP_RIG_ALLOW_DIRTY=1`, records HEAD and the
  uncommitted-path count before the run, and re-reads both afterwards, failing with
  `the result describes a tree that is not any commit` if either moved. Watched failing first: one
  appended newline to `README.md` made it refuse before any build. The lesson is not "run the artefact"
  but the harder version of it -- run the artefact at a revision, because a reading that names no commit
  can be trusted wrongly in either direction, and here it produced both a false red and a commit made on
  a red gate.

- **VAULT-1's premise was half wrong, and running `kcp` is what showed it.** The item said to produce
  "kcp's client CA and its admin token store from the kcp root directory". A real `kcp start` writes
  `admin.kubeconfig  .admin-token-store  apiserver.crt  apiserver.key  sa.key  audit.log`: the
  shard-admin token store is real, at `<KCP_ROOT>/.admin-token-store`, and there is NO client-CA file --
  `--client-ca-file=<missing>` fails because kcp generates none. A producer reading
  `$KCP_ROOT/client-ca.crt`, which is what this item would have been implemented as, would never have
  fired and the class would have kept reading as a gap. The real source is the root vault's own PKI CA
  at `pki/ca/pem`, the CA the `kcp-client` role issues the gateway's certificate from
  (`internal/vaultmain/sources.go:48`). The token store is written as the hex key `token-hash`.
  `central-store-credentials` was required only when `SOCIALWEB_CLUSTERPROVIDER_STORE_ENDPOINT` named an
  external store and was otherwise reported NOT-APPLICABLE on a distinct line, never as the silent
  "not written"; a partial set of the four variables was a hard error, because a half-configured store
  is not a store. The class and its four variables were deleted by plan 0006 M1-STORE-CLEANUP. Readings: `apply-root` logs 0 `root secret not written`; `client-ca` reads back
  byte-identical to `pki/ca/pem`; `admin-token-store` equals `xxd -p .admin-token-store`; deleting that
  file exits 1 naming the path. The correction is the entry: an item is a claim about where a fact
  lives, and this one named a file that `kcp` does not write.

- **The vault's tenant PKI role pins the organization, so the only identity the vault can issue is
  the superuser's.** `internal/clusterprovider/tenantcredential.go` had one path to a tenant
  credential and it produced `system:admin` / `system:masters` - the reason being that the
  vault-backed branch issues through the PKI role named by `VaultPKIRoleTenant`, and that role
  pins `organization: system:masters` on every certificate it signs, while the non-vault branch
  hardcoded the same pair. A CI runner or a controller that took that credential would hold
  cluster-admin on the tenant, which is the "wider credential" `ErrNoClientCA`'s own text refuses
  to reach for, and it makes any RBAC written against the runner decorative. The vault path
  cannot be widened either: the PKI role is `pki/roles/tenant-superuser` (written by
  `writeTenantPKIRole` in `internal/clusterprovider/vault.go`, named by `VaultPKIRoleTenant`) and
  its role config pins `organization: system:masters`, so every certificate it issues carries that
  group regardless of what the caller asks for. Closed by adding a second, disk-only entry point -
  `MintTenantCredentialForClusterAs(ctx, stateDir, cluster, server, commonName, organizations,
  validFor)` - which signs with the cluster's ClientCA directly and takes no vault path, so an
  arbitrary CommonName and Organizations are honoured. The superuser entry points are unchanged;
  the two are now distinguishable by name rather than by argument. Readings: a mint with
  `COMMON_NAME=gitlab-runner` and `ORGANIZATIONS=socialweb:runners,system:authenticated` yields a
  certificate whose parsed subject carries exactly those, with a single verified chain to the
  signing ClientCA; the same test asserts the CommonName is not `system:admin`. A second reading,
  taken because the CLI parsed a `VALID_FOR` it never used - a knob that looked like it worked and
  did not - is the certificate's own `NotAfter` at `VALID_FOR=1h30m`: `delta=1h29m59s`, read off
  the written `tls.crt` rather than off the configuration. The fact is recorded here rather than
  left in the code as a comment, per the rule that a decision recorded in a comment is deleted the
  next time comments are swept.

- **A real GitLab pipeline runs a job on a tenant cluster. CLOSED 2026-10-05.** Before: every proof
  in this tree stopped at the Kubernetes API. The GitLab Runner's own API calls were never made by
  a runner, so "GitLab CI works here" rested on a vendored document and on a Pod shape a test
  applied by hand. After: `TestAGitLabPipelineRunsOnATenantCluster`
  (`internal/providermain/live_gitlab_pipeline_test.go`) stands up a tenant cluster at zero
  nodes, mints a tenant credential from that cluster's own ClientCA, writes the runner's
  `config.toml` with `executor = "kubernetes"` and the credential as `cert_file`/`key_file`/
  `ca_file`, starts the real `gitlab-runner` binary against a real GitLab CE, commits a
  `.gitlab-ci.yml` through the API and triggers a pipeline. MEASURED 2026-10-05: PASS in 34.12 s.
  The job pod's own `Pending` state drove the scale decision (`intent and no node`), a node joined,
  GitLab's runner logged `Waiting for pod ... to be running on the node liveweb-node1`, two job
  pods were observed carrying `spec.nodeName = liveweb-node1`, and the pipeline reported
  `success`. Three things about this are worth keeping. First, the runner had to be told the
  apiserver's LOOPBACK address with a client certificate, not the kcp mount with a bearer: the
  mount is a reverse proxy and the exec/attach paths CI needs are not guaranteed to survive it,
  which is the same fact `NEXT.yaml` GL-2 records from the other side. Second, the harness drives
  the scale decision only when a test asks it to, so a pipeline that merely runs while the harness
  waits leaves its pod Pending for ever - the first attempt failed exactly that way and the fix is
  in the test, not in the product. Third, a job Pod runs INSIDE a guest, so `CI_REPOSITORY_URL`
  naming the GitLab host's loopback resolves to the guest's own loopback and the clone fails with
  `exit code 128` and a `script_failure` that reads like a broken runner; the proof sets
  `GIT_STRATEGY: none`, and the three ways out are recorded in `docs/gitlab/03-jobs.md`.
  NOT closed by this: the CLI half of GL-1 (GL-2 has since closed - see section 15).

- **The cloud controller manager now runs, and it is IN the process group. CLOSED 2026-10-05.** A
  controller that nothing starts is a design and not an installation, and that was the state:
  `internal/cloudprovider` and `cmd/socialweb-cloud-controller-manager` existed, were unit-tested,
  and nothing ran them. Closed by making the controller the fifth member of a tenant cluster's own
  process group. `procgroup.ComponentCloudControllerManager` sits after `k3s server` in both
  `DefaultOrder` and `StackOrder`, so `Reap` stops it first; it is present by default in the
  composition root. It is PORTLESS - it dials the apiserver and listens on nothing - so
  `portallocator` gained no purpose and no lease. Its credential is minted scoped, from the cluster's
  own ClientCA, into `<DataDir>/cloud-controller-manager.kubeconfig` (invariant 20 forbids the k3s
  admin kubeconfig); its cluster, credential and snapshot paths ride `Spec.Env` and never argv
  (invariants 3 and 13); and its readiness gate is its own `phase=Ready` log line, taken after the
  Node informer syncs - it binds no port, so that line is the only signal, and it is a positive
  marker that can fail (invariant 17). MEASURED 2026-10-05:
  `TestTheFourStepProofRunsThroughTheCompositionRoot` PASSES in 110.45 s with the fifth component,
  and its STEP 3 asserts `kubectl get node liveweb-node1 -o jsonpath={.spec.providerID}` reads
  `socialweb://liveweb/liveweb-node1` with the label `node.socialweb.computer/cluster=liveweb`.
  TWO DESIGN CLAIMS DID NOT SURVIVE CONTACT WITH THE CODE, and both are recorded because each would
  have shipped as a plausible sentence. First, RBAC was not a follow-on: a scoped identity with no
  binding is authenticated-and-denied, so the informer never syncs and the gate times out. The
  provider now creates the ClusterRole and ClusterRoleBinding for the controller's group once k3s is
  up, in the same change that introduced the identity. Second, the snapshot's absent-versus-empty
  distinction does not exist in the reader: `internal/cloudprovider/fileinstances.go` answers
  `(known=false, err=nil)` for an absent file AND for an empty array, so both are the delete path.
  Writing `[]` protects nothing; what protects a live Node is that the snapshot LISTS it. The sharp
  edge is stated rather than smoothed over - the snapshot file is load-bearing infrastructure and not
  a cache, and a snapshot that reads as absent is read as "every guest is gone". A snapshot that
  exists and cannot be parsed IS an error and patches nothing (invariant 14); the absent case is not,
  and that asymmetry is the risk this entry leaves behind (closed by plan 0005 M6-LIMITS: an absent
  snapshot is now an error, the entry at the end of this section). Split out rather than folded in:
  the stated limit in `NEXT.yaml` for a portless process-group member, which cannot be found by a
  record-based sweep and is therefore not swept.

- **The scale decision could not express more than one node. CLOSED 2026-10-05.** Before: `Decide`
  returned `Provision, Nodes: 1` for any intent, so one pending pod and fifty pending pods decided
  identically, and a second concurrent CI job waited rather than being placed. After: `Demand(o)` is
  a separate pod-work COUNT, `Options.MaxNodes` (default 1) and `Options.PodsPerNode` (default 8)
  bound `Target` at `ceil(Demand / PodsPerNode)` clamped to `[1, MaxNodes]`, and `Decision.Nodes` is
  that target total. `scaledriver` calls `Provision` once per missing node until the re-read count
  reaches the target, and each node gets a distinct id (`node1`, `node2`, ...) whose ordinal is read
  back from the tenant cluster's own Node list rather than remembered. `Intent` stays a boolean and
  `Demand > 0` agrees with `Intent` on every fixture, so the zero direction is untouched. MEASURED
  2026-10-05: `TestThreeGitLabJobPodsScaleToThreeNodes` (`live_scalemax_test.go`) PASSES in 95.65 s
  with `SOCIALWEB_CLUSTERPROVIDER_SCALE_MAX_NODES=3` and `..._SCALE_PODS_PER_NODE=1` - the decision
  reads `Provision, Nodes: 3` with reason `intent and no node`, `kubectl get nodes` reports three,
  three GitLab Runner job pods reach `Succeeded`, and past the 1m hold all three are reaped with the
  control plane still `Ready`. A second, independent run of the same test reported 90.01 s; the two
  runs used different leased apiserver ports (62102 and 62402), which is the same point the four-step
  reading makes - the port is not a constant. Unit readings: `Target` is 0 at no demand, 1 at one
  pod with
  `PodsPerNode == 8`, 3 at seventeen pods, and 4 at a hundred pods with `MaxNodes == 4`, while the
  default ceiling pins seventeen and a hundred pods to 1; at the default ceiling every existing
  `Decide` case reproduces its previous action and count and the two reachable reasons are
  byte-identical to the old strings; a failed scale-out stops the loop and leaves `State.Nodes` at 0;
  the demand/`Intent` agreement case covers 20 fixtures. The two prior live proofs were re-run after
  the harness change and still pass: the GitLab job pod in 84.93 s and the four-step in 100.53 s with
  `INSTANCE_START_TO_READY_S=5.420`. WHAT THIS DOES NOT SHOW, stated because the numbers read
  stronger than they are: the test asserts the CLUSTER reached three nodes, not that three guests
  each hosted work - the scheduler placed all three pods on `liveweb-node1`, which the design permits,
  so the evidence for three simultaneous guests is three `Provision` calls plus `kubectl get nodes`
  listing three, together with an independent observation of two firecracker processes during the run.
  No negative control was run either: that the test would fail with `MaxNodes` unset is argued from
  `Target`'s ceiling rather than measured. Partial scale-in was deliberately not implemented then (plan
  0002 section 4.4), so the target only grew while intent existed; superseded 2026-10-07 by per-node
  scale-in (plan 0005 M4-SCALE-IN, section 10).

- **The live backlog was declared in four places and did not exist.** `AGENTS.md:18`,
  `README.md:262`, `handoff.yaml:155` and section 14 of this document all name `NEXT.yaml` as the
  live backlog - section 14 calls itself its mirror and carries the syntax to read it - and
  `ls NEXT.yaml` answers `No such file or directory`. The file is not gitignored; it is absent.
  This is the reverse of the defect this document keeps recording: not a list that outlived its
  work, but a work list that never had a file, so the five references pointed at a page no operator
  could read while the open items themselves lived only in section 14. Closed by writing the file
  rather than by striking the references, because section 14 already held the items and the
  references were the correct design: `NEXT.yaml` now carries ESO-2 and the four items this
  iteration opened, each with the instrument that would close it. The reading is the two commands
  that disagree: the four references, and `ls` on the path they name. The class is worth keeping in
  view because it is the same one a green check is - a pointer with no referent reads as presence.

- **The door's verbs: createCluster, a chosen kubeconfig lifetime, the public front, and an owner
  who can act.** Four things stood between a real user and a usable cluster, and each was a gap
  between two parts that were individually finished. First, `createCluster`
  (`com.publicdomainrelay.temp.cluster.createCluster`) did not exist, so the k8s door reached a live
  cluster only by calling `getKubeconfig` for a kubeconfig it would throw away and then
  `controlPlaneUp`, in that order, and had to know the order. It is now one verb: it walks
  `getKubeconfig`'s create-on-read path (`revocation.CreateOnRead` around `Store.Ensure`, so the deny
  is consulted before anything is created) and then the desired-state flip `controlPlaneUp`
  performs, and answers the phase. It declares the same six errors as `controlPlaneUp`, including the
  retryable `ClusterInitializing` with its `Retry-After`. `getKubeconfig` KEEPS its create-on-read:
  that is what makes a first login work. THE CONTRACT CHANGED on 2026-10-08 (plan 0005
  M6-SPA-CREATE): `createCluster` no longer RUNS `controlPlaneUp`. It records `spec.running = true`
  first, on a context the caller's hang-up cannot cancel, and answers the phase it finds --
  `Initializing` is a phase in a 200 response and not a refusal, and the lexicon keeps declaring the
  error for the callers that still handle it. `controlPlaneUp` keeps the stricter reading (429 +
  `Retry-After` while Initializing), and `ClusterInitializing` is the only retryable error, so the
  two verbs differ on exactly one thing: one is asking the control plane up by name, the other is
  provisioning. `errForDeny` also stopped claiming every failure was the identity store: only
  `revocation.ErrDenyUnreadable` maps to `ClusterUnavailable "the identity store could not be read"`,
  and everything else from `Store.Ensure` answers `ClusterUnavailable` carrying its own cause
  (`"the cluster could not be prepared: <cause>"`) while the gateway logs the full error. Readings:
  the answerability check in `cmd/socialweb-gateway`
  now drives seven verbs, `internal/xrpcserver` drives `createCluster` past authentication and
  validates its output against the lexicon, and `internal/gateway` asserts one `Ensure` and a
  `Running` desired state, a second call still idempotent, and a revoked identity refused with zero
  creates. Second, a device credential's lifetime was whatever the deployment chose and a caller
  could not ask for anything else. `getKubeconfig` now takes an optional integer `ttlSeconds`, signed
  by `internal/devicekey` with the requested TTL when one is given and with
  `SOCIALWEB_GATEWAY_DEVICE_KEY_TTL` when none is. `SOCIALWEB_GATEWAY_DEVICE_KEY_MAX_TTL` is the
  ceiling, and when it is unset it EQUALS the TTL, so the default answer to "may a caller extend its
  own credential" is no. A value below 60s or above the ceiling is `InvalidRequest` and is REFUSED
  rather than clamped, because a caller that asked for a lifetime and silently got a different one
  has been told something false about the credential in its hand; the check runs BEFORE the create
  path, so a refused request writes nothing. A gateway with no ceiling configured refuses any
  explicit lifetime rather than granting an unbounded one. Readings: tables in `internal/gateway`
  (absent, floor, ceiling, one below, one above, zero, negative, no ceiling at all) and in
  `internal/xrpcserver` (the same window driven through the real lexicons and the real transport).
  Third, the kubeconfig a user held pointed at the address the GATEWAY uses.
  `SOCIALWEB_GATEWAY_KCP_PUBLIC_HOST` now names the public kcp front in the rendered `server:`
  (`<public host>/clusters/<MountPath>`), and `SOCIALWEB_GATEWAY_PUBLIC_MINT_URL` adds
  `SOCIALWEB_MINT_URL=<value>` to the exec block's env, which `cmd/socialweb-credential` reads and
  which nothing had ever set. The gateway's OWN kcp client keeps `SOCIALWEB_KCP_HOST`, so the
  loopback address and the user-facing address are now two settings instead of one that could not be
  both; with either public setting unset the rendered document is byte-for-byte what it was. Both are
  validated as absolute https URLs at startup, because client-go silently drops the exec credential
  plugin for a server that is not https, and `kubeconfig.AssertClean` still passes on the document
  that carries the mint URL. Readings: `internal/kubeconfig` asserts the env entry is present when
  configured and absent when not; `internal/kcpclient` asserts the granter is handed the public host
  with the trailing slash trimmed and the requested TTL, and that no request reaches kcp from a verb
  that only derives a path; `internal/devicekey` asserts a requested 90s really is what gets signed.
  Fourth, and the one that made the rest useless: the tenant grant scoped its mint Role to ONE
  hardcoded ServiceAccount, `did-mruwiotxmvrduzlymfwxa3dffzrw63i`, and no `did-*` ServiceAccount had
  any RBAC at all, so a real user's minted token authenticated and could do nothing.
  `deploy/tenant-grant.yaml` is now a TEMPLATE: it carries `__SOCIALWEB_OWNER_SERVICE_ACCOUNT__`
  where the derived name goes, and `Bootstrap` renders it with `ServiceAccountName` of the `Cluster`'s
  `spec.account` - the owner, and the same DID the mount path is derived from -
  via `kcpclient.RenderTenantGrant`, which refuses a document with no placeholder. The grant grew a
  `ClusterRoleBinding` `socialweb-owner` binding that ServiceAccount to `cluster-admin`, because it is
  the owner's own cluster and the credential has to be able to create namespaces, exec into pods and
  drive a GitLab runner. The `gwmint` Role is NOT widened: it is still scoped by `resourceNames` to
  exactly one derived name, and the owner's power comes from its own binding rather than from the
  mint's, so the tenancy boundary section 7 states is unchanged. Readings: `internal/tenantclient`
  renders the deployed file and still fails on a `*` or an underived `resourceName`;
  `internal/clusterprovider` asserts every `resourceNames` entry and the binding's subject are the
  owner's derived name, that the binding's `roleRef` is `ClusterRole cluster-admin`, that rendering
  for a different owner produces different names, and that a second `ensureGrantServiceAccounts`
  pass creates nothing new; `cmd/socialweb-rig-inputs` renders before it derives
  `SOCIALWEB_LIVE_GRANTED_SA`. The re-apply of the objects themselves is a server-side apply and is
  not exercised here: the client-go dynamic FAKE cannot apply a patch it did not first create, so a
  test of it would be a test of the fake. WHAT THIS DOES
  NOT SHOW, stated because the unit readings are strong and the live ones do not exist here: no run
  in this tree has yet created a cluster through `createCluster`, followed a kubeconfig whose server
  is a public front, or measured that a real user's minted token can `create namespaces` against a
  real tenant cluster through that binding. The instrument is the container-mode stack plus the k8s
  door: `createCluster` once, a `kubectl` through the public host, and a job whose script needs
  `pods/exec`. Until that runs, the owner's power is asserted from the document and the API, not
  measured.

- **A node can be rented from the market: NodeClaim and the market substrate.** Before this, a node
  was either a guest THIS host booted (firecracker, qemu) or nothing, and section 2 declared the
  market out of scope at the `ComputeProvider` seam. A tenant cluster's nodes are now rented as THE
  CLUSTER'S OWNER through that owner's single atproto OAuth session, held in a separate Deno process
  (the k8s door) that this repo does not build. The hand-off is a new CRD and one new substrate.

  The CRD is `NodeClaim`, `socialweb.computer/v1alpha1`, cluster-scoped in its workspace, written by
  the clusterprovider into the CLUSTER'S OWN HOME workspace - the logical cluster `Ref.Home` the
  registry already derives - and listed by the door across `clusters/*` through the `clusters`
  APIExport's virtual workspace, which is why the resource is in `deploy/cluster-apiexport.yaml`,
  schema `v1alpha1-2.nodeclaims.socialweb.computer` (was `v1alpha1-1` until M2-CODEGEN; see section 6,
  "Generated from api/v1alpha1"), applied by `deploy/install-provider.sh`. Its
  spec is `cluster`, `nodeName`, `ownerDid`, `apiserverPort`, `advertiseAddress`, `tokenFile`,
  `nodePasswordFile`, `serverCAFile`, `denyBidderDids`; its status is `phase`
  (`Pending|Bidding|Provisioning|Joining|Running|Failed|Released`), `contract`
  (`vmUri|rfpUri|acceptUri|receiptUri|receiptCid|submitEventRef`, plus since v1alpha1-2 the requester's
  whole `ContractState`), `fqdn` and `message`. The Go side
  writes spec and metadata and NEVER status: `setPhase` on the Go side, the door's `replaceStatus` on
  the other, and `kcpclient.WriteNodeClaimTo` patches an existing claim's spec only when it differs,
  so a live claim is not rewritten on every pass.

  The substrate is `internal/nodemarket`, selected by `SOCIALWEB_CLUSTERPROVIDER_SUBSTRATE=market`.
  `Provision` mints-or-reads the per-node password at `nodevm.NodePasswordPath(DataDir)`, records
  `Pending` on the existing Node CR, writes the claim and RETURNS - it does not wait for the market,
  because a claim takes minutes and the scale pass runs every 2 s for every cluster. `ConfirmJoined`
  reads the claim and requires `Running`: the door writes that only once the node's own agent reached
  its apiserver, so a tenant cluster reporting the node before then is a node something else
  registered. `Fatal` returns the claim's `status.message` while the phase is `Failed`, so the join
  wait ends with the door's own words instead of a timeout (invariant 28). `Teardown` and `Reap` delete
  the claim and then WAIT, bounded by
  `SOCIALWEB_CLUSTERPROVIDER_MARKET_RELEASE_TIMEOUT` (default 2m), for it to disappear, because the
  door removes its finalizer only after the market VM is deleted: closing is not destroying
  (invariant 18), and a claim still there when the wait runs out is a machine still being paid for and
  is reported as a failed release rather than a completed one. `NodeExpiry` is the explicit `0` that
  means UNLIMITED, not nil: no door session cap applies to this substrate, because the door holds the
  owner's session for the node's whole life, and an ABSENT expiry refuses teardown for ever
  (invariant 9).

  Three smaller decisions came with it, each of which had to be made rather than assumed. **(a)** The
  address a market node dials is `SOCIALWEB_CLUSTERPROVIDER_MARKET_ADVERTISE_ADDRESS`, default
  `198.18.255.254`: not loopback, because k3s refuses a loopback address for the in-cluster
  `kubernetes` Service endpoint, and NOT an address this host carries, because the guest puts it on
  its OWN loopback and the door reverse-forwards it to the apiserver, which stays bound to
  `127.0.0.1`. That is a third case in `portallocator`'s advertise invariant
  (`AssertMarketAdvertiseInvariant`), because neither existing case fits: the local-process case
  REQUIRES a host-carried address and the guest case requires one of the cluster's own subnet range.
  The same address goes into `--tls-san`, for the reason the guest gateway does: the certificate has
  to cover the address the node's kubelet actually connects to, or the join reads as an x509 name
  error rather than as a missing SAN.
  `SOCIALWEB_CLUSTERPROVIDER_ADVERTISE_ADDRESS` is refused under this substrate, and so are the VMM
  binaries, `NODE_AGENT`, the image settings and `NETWORK=pasta`: none of them can be in force here,
  and an operator who set one and saw no complaint would believe it was carrying it. **(b)** The node
  count a scale pass OBSERVES now includes every node this clusterprovider HOLDS whose own cluster
  does not yet list it - because a market node is absent from its tenant cluster's node list for
  minutes while the pass runs every 2 s, and without that the decision reads zero nodes and asks
  again on every tick. `internal/scale.Decide` is unchanged; the counting happens in the observe
  closure `internal/providermain/main.go` hands `scaledriver`. This was first written as
  "held hosts whose claim is not yet `Running`", keyed on the claim's phase, and that predicate was
  itself the defect closed as MARKET-4: the door writes `Running` when the node's agent reaches the
  apiserver through the forward, which is a moment BEFORE the Node object appears in the cluster's
  node list, so a claim in that gap was in no list at all. The predicate is now the tenant cluster's
  own node list, and it is the only list that can be. **(c)** The startup sweep
  (`sweepNodeRecords`) skips the walk under this substrate with a logged decision, because a market
  node's machine is not a process on this host and no identity can be derived for it; what that leaves
  open is the sweep MARKET-1 has since closed (section 15).

  The instrument is `go test ./...`. New: `internal/nodemarket` (the claim's fields and paths
  survive `Provision`; a second `Provision` leaves one claim; `ConfirmJoined` refuses anything short
  of `Running` and carries the door's message for `Failed`; a teardown the door never releases is a FAILURE and one it does is not;
  a failed read during a release is not a claim that is gone), `internal/kcpclient` (the claim is
  created in the home workspace with the door's exact field names, is not rewritten when its spec
  matches, is patched when it does not and never through `/status`, and an absent claim decodes to
  nil), `api/v1alpha1` (the wire field names and the seven phase literals), `internal/portallocator`
  (the market advertise invariant, and that it rejects loopback, the unspecified address and every
  address this host carries), `internal/clusterstack` (a stack cannot carry both dials), and
  `cmd/socialweb-clusterprovider` (the market substrate builds with no VMM, agent, image, pasta or
  `/dev/kvm`; two scale passes over a `Pending` claim ask for exactly ONE node; a claim is counted
  until its own cluster's node list names it and not one moment earlier; a second pass over claims
  that have all reached `Running` with none of their nodes listed rents nothing; the teardown issues
  exactly one claim delete; the
  sweep is a no-op; and the market join needs nothing but its own advertise address).
  `internal/scale`, the four-step proof and the live GitLab tests are untouched and still use
  firecracker.

- **One provider serves every cluster, and a cluster is named by its home.** The k8s door writes
  `root:<accountHash>:<name>` plus a `Cluster` CR plus the `:clusters` mount for every cluster any
  atproto user creates, and NOTHING materialised one unless a process was started with
  `SOCIALWEB_CLUSTERPROVIDER_JOIN` naming it. Worse, a cluster was looked up by its bare name, so two
  accounts that each named a cluster `ci` shared ONE state dir, ONE set of port leases, ONE set of
  node names, ONE `nodes.json` and ONE `scaledriver` state map - a collision that reports as one
  cluster reading another's state rather than as an error. The key is now
  `kcpclient.ClusterKey(home)` = `<accountHash>-<name>` (invariant 30), used for the state dir, the
  port and subnet leases, the node names, the snapshot, the scale driver key, the mint credential, the
  `NodeClaim` name and the startup sweep's process-group attribution (section 12). `JOIN=*` reconciles
  every `Cluster` in the tree each pass, serially, with one failure logged and skipped rather than
  returned (invariant 23 says why it is logged at all); startup already walked the whole tree and now
  the shutdown reap does too, by asking the `LocalHost` which groups it holds. An existing
  single-cluster deployment is migrated by rename at startup, and only where the bare name is
  unambiguous (section 12). Two costs are recorded rather than hidden. **(a)** The node names change
  with the key, so a cluster that already had nodes under `<name>-node1` sees `<key>-node1` after the
  migration and its `nextOrdinal` no longer recognises the old name. The tenant Node object is not
  renamed, because renaming it is a write into a running cluster's apiserver and the shell that
  carried the node outlives the rename; the old node is reaped by the ordinary zero-state path and
  re-provisioned under the new name, which is the same work an OFF/ON cycle does. **(b)** A
  `NodeClaim` written before the migration is named for the bare cluster and is not renamed, so it is
  released and re-claimed once.
  Two smaller defects closed with it. `tenantNodes.Provision` no longer polls the tenant cluster for
  a market node before returning: a market machine is absent for minutes and the poll is 30 s, so a
  single new claim blocked every other cluster in the tree for the whole timeout, once per cluster,
  serially. The market substrate now returns once the claim is written and the snapshot updated, and a
  later pass confirms the join or fails the node on `Fatal` (invariant 31, section 11); the guest
  substrates are unchanged. And the market advertise default moved from `100.127.255.254` to
  `198.18.255.254`: the old value is inside `100.64.0.0/10`, which is Tailscale's CGNAT space, so a
  tailnet peer can hold exactly that address and the guest's loopback forward collides with the
  tailnet. `198.18.0.0/15` is RFC 2544 benchmarking space, never routed on the internet, and
  `AssertMarketAdvertiseAddress` still refuses loopback, the unspecified address and every address this
  host carries.

  The instrument is `go test ./...` plus `deploy/check.sh --fast`. New:
  `cmd/socialweb-clusterprovider` (two accounts naming a cluster `ci` get distinct state dirs, leases,
  node names and mint paths; an all-clusters pass over three CRs, two ON and one OFF, brings up
  exactly the two and reaps the OFF one; a cluster whose CR cannot be read does not hold back the
  others; a market `Provision` returns in well under a second with no tenant Node present and still
  writes exactly one claim; a later pass confirms a node the tenant cluster reports and tears down one
  whose claim reads `Failed`; a legacy state dir, node dir, mint credential and port and subnet leases
  are migrated to the key; a bare name two accounts share is left where it is). `internal/portallocator`
  and `internal/subnetallocator` gained `Rekey`, `internal/materialiser` gained `LocalHost.Clusters`,
  and the four-step proof and the live tests are untouched.

- **The cluster vault's certificate can be asked to cover one more host without any client losing the
  CA it trusts. CLOSED, and the CA key written beside the CA is what closed it.**
  `vaultcert.EnsureVaultTLSCertificate` minted a brand new CA and leaf whenever the hosts it was asked
  for were not already on the leaf, then wrote `ca.crt` over whatever was there, and it never wrote the
  CA key at all - so a CA could not be reused and every host-set change forced a new anchor. Two
  callers ask a market cluster's vault for different hosts: bring-up asks for
  `SOCIALWEB_MARKET_ADVERTISE_ADDRESS` and loopback (`internal/clusterprovider/stack.go`), and the
  tenant add-on pass asks for the `10.100.0.0/16` gateway `internal/subnetallocator` leases and
  loopback (`internal/providermain/eso.go`). So the second call replaced `ca.crt` while the
  vault the first call had configured was still listening and serving the leaf it loaded at start,
  which OpenBao does not re-read. Every later client built its pool from the new `ca.crt` and verified
  it against the old leaf. Reproduced against a real `bao`: the mint failed with `x509: certificate
  signed by unknown authority (possibly because of "x509: ECDSA verification failure" while trying to
  verify candidate authority certificate "socialweb-vault-tls-ca")`, and it stayed failed for the life
  of the process because the trust anchor had moved under a listener that never re-read it. The CA key
  is now written 0600 beside `ca.crt`, the CA is minted only when there is none, and a caller that
  needs one more host re-signs the leaf under the SAME CA to cover the union of the hosts the leaf
  already names and the hosts asked for. Readings, on one run of the reproduction: the CA's SHA-256
  first eight bytes were `d6e56d5aea1e43d7` before the tenant add-on pass and `d8491e1f7319d3b2` after
  it, and the mint failed; with the change the CA stayed `829b657ffabfe2a0` across the same pass and
  the mint succeeded against the same running vault. Pinned by
  `TestTheVaultTLSCAOutlivesTheHostsItsCertificateCovers`,
  `TestTheVaultTLSCertificateCarriesEveryHostItHasBeenAskedFor` and
  `TestTheVaultTLSCAKeyIsWrittenBesideTheCAItSignsWith` in `internal/vaultcert`, all three of which
  fail against the old writer, and by `TestTheVaultStillMintsAfterTheTenantAddOnPassAsksForAnotherHost` in
  `internal/clusterprovider`, which starts a real `bao` and reproduces the production message verbatim.
  A cluster whose `ca.crt` predates the CA key is kept rather than re-anchored - a leaf that is still
  usable is returned as it is, because replacing the anchor breaks every client and every node
  certificate already issued - and the host such a cluster cannot cover is section 14's VAULT-TLS-1.
  The cluster vault and the root vault deliberately differ here, and the difference is the listener:
  `EnsureRootVaultTLS` REPLACES its whole material, CA included, when the host set no longer matches
  (`TestRootVaultTLSRotatesWhenAHostItMustCoverIsGone` pins it), and that is sound because every root
  vault client re-reads `LoadRootVaultTLS` before it dials; a cluster vault's clients do not, and the
  `bao` serving the leaf reads it once. So the cluster vault grows the leaf under a stable CA and the
  root vault rotates both. Neither is the other's bug.

- **The tenant add-ons pass awaited an operator rollout a cluster with zero nodes can never produce,
  so it stalled 180 s every pass.** Before: the provider ran `deploy/eso/install.sh`, whose last act
  was `kubectl -n default rollout status deployment/external-secrets --timeout=180s`. That
  deployment's pods cannot be scheduled until a node exists, and the node is created by the
  node-scale pass, which runs AFTER the reconcile pass in the same loop
  (`internal/providermain/wiring.go`: `p.pass(ctx, ref, key)` then
  `p.scale(ctx, key, ref.Name, true)`), so the wait outlives the very pass that would satisfy it.
  It is a stall rather than a deadlock - the wait sits inside `if ! ...; then`, so the script exits
  0 with a warning - but a cluster at zero nodes cannot bring itself up promptly and every pass pays
  it. Measured in production at zero nodes: `docker exec socialweb-k8s ps -eo pid,etime,args` showed
  `bash /opt/socialweb/deploy/eso/install.sh` at 01:35 beside
  `kubectl -n default rollout status deployment/external-secrets --timeout=180s` at 01:32. After:
  `install.sh` honors `ESO_SKIP_ROLLOUT_WAIT` (default 0, so a human running the installer still
  gets the rollout diagnostic) and `internal/providermain/eso.go` sets it to 1, so the
  install returns as soon as the bundle is applied and CRD establishment - which needs no node - is
  the only thing that must happen before the store is applied. Nothing else in the pass moves: the
  gate is still the probe that already exists, `eso.Installed` reading one `ClusterSecretStore` with
  `Limit: 1`, and `InstallOperator` still fails the pass by name with `ErrNoOperator`, naming
  `external-secrets.io/v1`, when the group does not serve after the install, so an operator whose
  CRDs never come up is refused rather than passed silently. The change is a flag on the repo's own
  installer rather than the bundle applied from Go: `install.sh` is a file of this repo (added in
  `init`), while the pinned upstream bundle is the v2.11.0 `external-secrets.yaml` it fetches at
  runtime and applies unchanged with its webhooks rewritten to `failurePolicy: Ignore`; that bundle
  is untouched. Reapplying the manifests from Go would have put the version pin and the webhook
  rewrite in a second place and left the shipped installer without a caller, which is the shape
  ESO-3 closed. Watched to fail: with the provider's `ESO_SKIP_ROLLOUT_WAIT=1` removed,
  `TestTheInstallDoesNotWaitForTheOperatorToRoll` recorded
  `-n default rollout status deployment/external-secrets --timeout=180s` against the wanted
  `apply --server-side --force-conflicts -f -`, and
  `TestAnInstallThatLeavesTheOperatorUnservedFailsThePassByName` pins the refusal by group name.
  `NEXT.yaml` ESO-4.

- **A market cluster's scale pass re-asked the market for one node instead of naming the nodes its
  target needed, every 2 s, for as long as a claim was in flight. CLOSED, and the ordinal that names
  a node is what closed it.** `tenantNodes.nextOrdinal` named the next node from the TENANT cluster's
  `Nodes()` list alone (`internal/providermain/nodes.go`). That list holds only nodes whose
  k3s agent has already joined, and a market `Provision` writes its `NodeClaim` and returns before the
  machine exists (invariant 31, section 11), so every provision inside one pass read the same stale
  list and resolved to the SAME ordinal: `scaledriver` asked for `target - observed` nodes and
  `inFlight` correctly counted the claims already pending, but the loop wrote the one claim again for
  each node it still wanted and never advanced past it. The count was right and the naming was wrong.
  MEASURED in production on `root@socialweb.computer` (ssh port 1997), with
  `SOCIALWEB_CLUSTERPROVIDER_SUBSTRATE=market`, `SCALE_PODS_PER_NODE=1` and a target of 6:
  `docker logs socialweb-k8s` carried four `nodemarket: node claimed from the market` lines for
  `...-node2` per 2 s pass (240 in two minutes) beside one
  `scaledriver: node count changed action=1 nodes=6 reason="intent, and 2 nodes are counted for a
  target of 6"`; the cluster's home workspace `root:2wjeqtgx:test-cluster-alice-0001` held two claims,
  `node1` `Running` and `node2` `Provisioning`, and the tenant apiserver reported one node, `node1`.
  So `observed` was 2 (one joined plus one in flight) and the four provisions that aimed at 6 all
  landed on `node2`. The `429` the door logs (`node_claim_list_failed ... nodeclaims: 429`) is largely
  an EFFECT and not the cause: the door and the clusterprovider both authenticate with
  `/var/lib/socialweb/kcp/admin.kubeconfig`, so kcp's per-user rate limit is shared, and the
  provider's churn spent the budget the door's own list needed. After: `nextOrdinal` seeds its
  `present` set with the ordinals this `tenantNodes` already holds (`heldNodesLocked`, reading each
  name's ordinal the same way the tenant branch already does), so a pass at `observed=2`/`target=6`
  names `node3..node6` and the next pass counts 6 and asks for nothing. The door's own side was
  originally left as a separate change in the sibling Deno tree; it has since been made, and it was
  the same defect seen from the other end of the shared credential. The door's poll
  (`socialweb-computer-k8s/lib/socialweb-computer-k8s-node-market/mod.ts:330`,
  `setInterval(tick, opts.pollIntervalMs ?? 2_000)`) called `NodeClaimStore.list`, which re-walked
  every account and home and then LISTed nodeclaims per home on EVERY tick - two tenancy queries per
  account per 2 s - and its `request` had no `429` handling at all, so the rate limit it earned was
  paid again on the next tick and the one after that. Both halves are now in the store, where every
  caller gets them: the account-and-home walk is cached for 30 s (`homesTtlMs`, 0 to disable), and a
  `429` parks READS until the `Retry-After` it named has elapsed, capped at a minute, five seconds
  when the header is absent or unparseable. A read arriving inside the window fails immediately
  without reaching kcp; writes are deliberately not parked, because a claim's own status write is
  not the traffic that earned the limit and holding it back delays the node the door is bringing up.
  The poll's default interval moves 2 s to 5 s. The instruments are three tests in
  `socialweb-computer-k8s/test/nodeclaim_kcp_test.ts`: the walk happening once per ttl rather than
  once per poll, the walk happening AGAIN past its ttl, and a `429` holding reads back while writes
  still go through. The first and third were watched to fail with the cache and the park removed; the
  second cannot fail against the uncached code by construction, so it was watched against a cache
  that never expires, and the pair pins the cache from both sides.
  The instrument for the count itself is `TestOneScalePassClaimsEveryNodeTheTargetNeeds` in
  `cmd/socialweb-clusterprovider`:
  three running pods and one pass, watched to fail against the old `nextOrdinal` with `one pass that
  needs three nodes left 1 claims, want three`, and green with the change, where three distinct claims
  exist and a second pass writes nothing. `NEXT.yaml` MARKET-3.

- **A market cluster rented 15 machines for a target of 10, because a claim that had reached
  `Running` was counted in no list at all until its k3s agent registered. CLOSED, and the count is
  now the tenant cluster's own node list.** The count a pass observed was `tenant Nodes +
  inFlight`, and `inFlight` was keyed on the CLAIM's phase: `nodemarket.Host.InFlight` returned
  `claim.Status.Phase != Running`. That predicate has a hole with a duration in it. The door writes
  `Running` from `onForwarded`
  (`socialweb-computer-k8s/lib/socialweb-computer-k8s-node-market/mod.ts:214`, "the node's agent
  reached its apiserver"), which fires when the guest's k3s agent completes its first forwarded TCP
  connection through the door to the apiserver - and the Node object it is registering appears in
  the CLUSTER's node list only after that connection is served. Between those two moments the node
  exists and is paid for, is absent from the tenant node list because k3s has not registered it yet,
  and has stopped being counted as in flight because its claim says `Running`. It is in neither half
  of the sum, so the sum reads one node too few for as long as the gap lasts. One node too few would
  be harmless. What makes it a defect is that claims cross that gap TOGETHER: a batch of guests
  booted by one pass reaches `Running` within seconds of each other, so the whole batch leaves the
  count at once, the pass reads a count far below the target that is really out there, and it rents
  the difference again.
  MEASURED in production on `root@socialweb.computer` (ssh port 1997) on 2026-10-06, five GitLab
  jobs admitted at once with `SCALE_MAX_NODES=10` and `SCALE_PODS_PER_NODE=1`: the node count read
  `1 -> 5 -> 6 -> 7 -> 8 -> 15` and settled on `scaledriver: no change action=None nodes=15 ...` -
  15 Ready nodes named `node3` to `node17` for a ceiling of 10 - while all five jobs went `success`
  on five distinct nodes and both bidders carried the 18 VMs with no resource refusal. The claims
  that had already reached `Running` with none of their Nodes in the tenant list were the whole of
  the gap. The fix removes the phase from the count entirely: `tenantNodes.inFlight` lists the
  tenant cluster's Nodes once per pass and counts every node it HOLDS whose name is not in that
  list, so the sum is `tenant Nodes + held claims the tenant does not name`, which cannot
  double-count and cannot lose a moment. That second half is gated on `Substrate.deferredJoin()`,
  which is the market substrate alone, so a guest substrate's count is exactly what it was: a
  firecracker or qemu `Provision` does not return until the tenant cluster reports the node, so a
  host such a substrate still holds and the tenant does not name is a join that ran out its timeout
  rather than a machine on its way, and counting it would hold the decision off a node that is
  never coming. `internal/nodemarket.Host.InFlight` and the
  `inFlightReporter` seam are deleted rather than corrected, because the shop that could answer the
  question moved: a host knows its claim's phase, and only the cluster knows what it has registered.
  A read that fails counts every held node, the conservative direction, and says so in a warning;
  the previous behaviour on a failed read was the same. `tenantNodes.client`'s error text lost its
  teardown-specific wording, since the count now calls it too, and `Teardown` carries that wording
  itself.
  The instrument is `go test ./internal/providermain/ -count=1`. Two tests, both watched to
  fail against the old predicate by restoring it in place and running them:
  `TestAMarketNodeIsCountedUntilItsOwnClusterListsIt` records `a claim that reached Running while
  its cluster still did not list the node counted 0 nodes in flight, want 1`, and
  `TestASecondPassRentsNothingWhileTheFirstNodesAreStillJoining` - the regression itself - records
  `a second pass over three claims that had all reached Running and whose nodes the cluster did not
  list yet left 6 claims, want three`, the doubling, on three claims instead of the production five.
  Both are green with the change. `NEXT.yaml` MARKET-4.

- **A node claimed from the market was torn down two seconds later, because a node name hands its
  last claim's words to the next claim written under it. PARTLY CLOSED - the naming rule is fixed for
  every node but the first, and the first is MARKET-5.** `kcpclient.WriteNodeClaimTo` never touches
  status: it reads the claim, and when the SPEC already matches it returns without a write at all. The
  door writes the terminal status of a claim it can no longer serve (`adoptNever`, section 15's market
  entries), and nothing clears it, so a name whose claim ended carries that phase and that message
  into whatever is claimed under it next. The provider then reads its own fresh claim as a failure:
  `confirmHeld` sees `Failed`, `guestFatal` returns the door's words, and the node is released.
  MEASURED in production on 2026-10-06 during the five-job run: `nodemarket: node claimed from the
  market` for `-node7` at 06:18:59, `nodemarket: releasing the node` for it at 06:19:01, and the
  reason the provider reported was `the door restarted and released this node's VM; a new node will be
  requested` - a sentence the door had written for a claim of an earlier generation. Four nodes went
  that way in one run: `node1` (claimed 06:18:38, released 06:18:41), `node2`, `node7` and `node11`.
  No machine was rented for any of them - the door acts on a claim that is `Pending` or has a
  deletion timestamp, and a `Failed` one is neither - so the cost is a spent claim cycle and minutes
  of slower convergence, not a leaked VM.
  `tenantNodes.nextOrdinal` now seeds its `present` set from the claims already in the cluster's home
  workspace whose phase has ENDED, so such a name is retired. A claim that has NOT ended is
  deliberately left alone, and that is the half worth stating: re-claiming a live name is a no-op on
  the spec, which is how a provider that restarted adopts a node the door is still bringing up instead
  of abandoning it. `nodemarket.Claims` gained `ListNodeClaims` for this; `kcpclient` already had the
  call. The instrument is `TestANodeNameWhoseClaimHasEndedIsNotClaimedAgain` in
  `cmd/socialweb-clusterprovider`: a `Failed` claim seeded at `node2`, two provisions, and the second
  must name `node3`. Watched to fail with the seeding removed. `NEXT.yaml` MARKET-5 covers what is
  left - the FIRST node of a cluster, whose name is fixed at construction and never reconsidered.

- **A torn-down node kept being held, so the count passed the ceiling and the cluster could not rent
  another machine. CLOSED.** `tenantNodes.teardownHeld` deleted the node's claim, deleted the
  cluster's `Node` object and dropped the name from the live set, and left the host in `extra`. That
  was invisible while the count was keyed on a claim's PHASE, because a deleted claim reads as nil and
  a nil claim was not counted; the moment the count became "the tenant cluster's node list plus the
  names this process holds" (MARKET-4, above), a hold that outlives both its claim and its `Node`
  became a node counted for ever. It is the same shape as the defect it came from: the count and the
  thing counting it disagreed about what a held node is.
  MEASURED in production on 2026-10-06, at the end of the five-job run, by counting the log rather
  than reading the count: 11 `node claimed from the market` lines, 7 `releasing the node` lines, so 4
  live claims - and the tenant cluster reporting exactly those four Nodes, `node3` to `node6`, while
  the scale decision read `nodes=11` on 131 consecutive passes. That is not a wrong number in a log.
  11 is above the ceiling of 10, so `scale.Decide` returned the branch that reads "more nodes are
  counted than the target needs", which is the branch that does NOT provision: the cluster was holding
  four nodes and could not rent a fifth, and no pass would ever have corrected it on its own. The
  instrument is `TestANodeThatIsTornDownStopsBeingCounted` in `cmd/socialweb-clusterprovider`: a node
  is provisioned, its claim is failed, the cluster is confirmed, and the count must fall back to zero.
  Watched to fail with `forgetHeld` removed: "a node that was torn down is still counted as 1 nodes in
  flight". `forgetHeld` drops the primary host when the name is its own and removes the ordinal from
  `extra`, and `teardownHeld` calls it once the teardown has returned.
  AFTER, on the same cluster: the decision reads `intent, and 4 nodes are counted for a target of 6`
  against a `kubectl get nodes` that lists exactly four, and the pass provisions the two the target
  asks for. The wedge is the thing to watch for because it is silent - an inflated count and a correct
  count both stop at the same line, and only the cluster's own node list says which one this is.

- **A cluster's first node was claimed under a name whose claim had ended, and released two seconds
  later. CLOSED.** `nodeHosts.forCluster` built the primary host as `node1` and `Provision` took
  ordinal 1 straight to `hostFor(1)`, the host already in hand, never consulting the free-ordinal rule
  that the NEXT node is named by. `kcpclient.WriteNodeClaimTo` reads the claim and, when the spec
  already matches, returns without a write - so a name whose claim had ended carried its `Failed`
  phase and the door's old sentence into the claim written under it, and `confirmHeld` read it as a
  failure. MEASURED in production on 2026-10-06, after a deploy restarted the door: `-node1` claimed
  at 06:18:38 and released at 06:18:41 with `the door restarted and released this node's VM; a new
  node will be requested`, a sentence the door had written for an earlier generation's claim.
  `Provision` now asks whether the primary host's name is spent before it uses that host, reads the
  claim at that name (a by-name GET, not a list), and when its phase has ENDED it drops the host
  (`forgetHeldLocked`) and lets the ordinary free-ordinal rule name the first node instead. This is
  the same shape as the two counts above and it closed the same way: the thing that names a node is
  the claim that already exists at the name, not the host this process happened to build.
  The instrument is `TestTheFirstNodeOfAClusterIsNotClaimedUnderASpentName` in
  `cmd/socialweb-clusterprovider`: a `Failed` claim seeded at `node1`, one provision, and it must name
  `node2` and leave the spent claim alone. Watched to fail with the check removed. `forgetHeld` was
  split into a locked and an unlocked form in the process - the first draft called the locking one
  from `Provision`, which already held the mutex, and hung the test until the timeout named it.

- **The door's node-session test let ssh2 generate the key it then asked ssh2 to parse, and failed
  about one run in four. CLOSED, and BOTH of the causes proposed before the real one were wrong.**
  `test/node_session_test.ts`'s `fakeGuest` did `const hostKey = utils.generateKeyPairSync("ed25519")`
  and then `new Server({ hostKeys: [hostKey.private] })`, which fails with `error: Cannot parse
  privateKey: Malformed OpenSSH private key` from `ssh2@1.17.0/lib/server.js:249` whenever the
  generator's output is one its own parser rejects. MEASURED 2026-10-06/07, and the two refutations are
  the part worth keeping:
  the FIRST cause proposed was an ssh2 key-generator defect under load, which is right in substance but
  was written before any control; the SECOND was this host's `/tmp`, a 24 GiB tmpfs at 91% with 19 GiB
  of stale scratch, on the evidence of five green runs with `TMPDIR` moved off it - and that control
  changed two things at once. The refutation is direct: five more consecutive runs were green with
  `TMPDIR` LEFT ON the 91%-full tmpfs, and a 60-iteration probe of exactly that path
  (`generateKeyPairSync` + `Deno.makeTempFile` + write + read + `new Server`) never failed once. What
  the rate actually tracks is CONCURRENCY: 0 failures in 60 sequential generations, 1 in 240 across
  four concurrent probe processes, and 1 of 4 concurrent `deno task test` suites against 0 of 5
  sequential ones. Production does not use that path at all - the requester shells out to the real
  `ssh-keygen` into a per-VM temp dir (`ts/atproto-market/lib/requester-xrpc/mod.ts:749`) - so this was
  ever only the test's own key.
  The fix is a fixed test keypair for the fake guest, which removes the generator and the randomness
  together. Closing reading: SIX concurrent `deno task test` suites, 65 passed each; the same six-way
  run before the change failed one suite. `NEXT.yaml` FLAKE-2.

- **A Node whose guest the door reaped stayed Ready and took pods, and the door already had the word
  that says so. CLOSED.** The door's restart releases the claims it holds and destroys their machines,
  and it wrote `Failed` for that - the same phase it writes for a claim whose machine it could NOT
  confirm released. The provider acts on `Failed` only for the nodes IT holds, and a provider that has
  just started holds nothing, so the previous generation's `Node` objects stayed in the tenant cluster
  reading `Ready` and took pods. MEASURED on 2026-10-06, forty seconds after a deploy restarted the
  door: pipeline 74's four runner pods were scheduled onto `...-node6`, sat `Pending` for 58 s, and the
  runner gave up with `Job failed (system failure): prepare environment: waiting for pod running: pods
  "runner-w93ufg0qn-project-1-concurrent-3-nuixd3cp" not found` - the pod gone when the `Node` finally
  was. Four of five jobs failed that way, where a pipeline 25 minutes earlier with no restart in front
  of it had all five running at once on five distinct nodes. The deletion came from the cloud
  controller manager, not from anything that noticed the guest was gone: a corpse is a `Node` absent
  from the instance snapshot, and a snapshot is rewritten only when the provider next provisions or
  tears down.
  The obvious reconcile was recorded as UNSAFE first, and that entry is the reason the fix is shaped
  the way it is: `adoptNever` wrote `Failed` both for a claim it reaped and for one whose machine it
  could not confirm released, and the second may still have a running machine on its way to joining, so
  a reconciler built on the phase alone would delete `Node`s for machines still being paid for. The
  door's vocabulary already had the distinction - `Released` is what it writes in the normal teardown
  path, for a node whose VM it deleted - so `adoptNever` now writes `Released` when its own reap
  returned true and `Failed` when it did not, and no schema changed at all.
  Two halves follow from that. `nodemarket.Host.Fatal` becomes `Terminal` and returns the claim's own
  words for ANY ended phase, so a claim the door released is given up on rather than left counted (the
  guest hosts implement `Terminal` as the fatal-console marker they always had). `nodeHosts.reconcile`
  runs each pass before `confirm`: it lists the claims in the cluster's home workspace, and for every
  one reading `Released` it deletes the tenant `Node` of that name, because only `Released` says the
  machine is gone. It does not need the cluster's node host, which is what a just-restarted provider
  does not have.
  The instruments are three tests, each watched to fail with its half removed. In the door,
  "a live claim this process never started is reaped, never adopted" now asserts `Released` where it
  asserted `Failed`, and "a claim the door could not confirm released is Failed, not Released" holds
  the other branch. In `cmd/socialweb-clusterprovider`,
  `TestANodeWhoseClaimTheDoorConfirmedReleasedIsRemoved` seeds a `Released` claim and a `Running` claim
  with a tenant `Node` each, and asserts the first Node is gone and the second is not - watched to fail
  with the reconcile made a no-op: "the Node acct-liveweb-node1 is still in the tenant cluster after
  the door confirmed its machine gone".
  The reconcile's OWN first version was a defect, and in production within five minutes: it deleted by
  NAME, and a claim the door released stays in the workspace for ever as its own record, so the names
  to act on are not the Nodes still there. MEASURED on 2026-10-07: 325 `removed a Node the door has
  taken the machine away for` lines in five minutes - one write per pass per lingering released claim,
  on Nodes that had gone the first time - beside a `429` on the credential the door and the provider
  share. It now lists the tenant `Node`s once, intersects, acts only on names in both, and logs only a
  removal that happened. `TestAReleasedClaimWhoseNodeIsAlreadyGoneCostsNoDelete` runs it twice over a
  released claim with no `Node` and asserts the fake client saw no delete action; watched to fail with
  the intersection removed. `NEXT.yaml` MARKET-6.

- **A GitLab Runner off the kcp host reaches a tenant apiserver, exec and all. CLOSED, and it was
  closed by the runs rather than by a change.** The apiserver binds loopback and the only off-host
  route is the kcp mount, a reverse proxy that carries bearers; whether the exec and attach paths a CI
  runner needs survive that proxy was recorded as unmeasured (GL-2), with the note that making it work
  means publishing an authenticated front and widening a trust boundary the gateway and the entrypoint
  keep at loopback on purpose.
  MEASURED on 2026-10-06, from this workstation - a different host from the one serving kcp - with the
  kubeconfig the SPA downloads (`/etc/gitlab-runner/tenant-ci/kubeconfig.yaml`, server
  `https://kcp.socialweb.computer/clusters/root:2wjeqtgx:test-cluster-alice-0001:clusters`):
  `kubectl logs` returned a running container's own log lines, and `kubectl exec` returned the
  CONTAINER's answer - `error executing command in container: failed to exec in container: failed to
  start exec "82d36cf2...": OCI runtime exec failed: exec failed: unable to start container process:
  exec: "sh": executable file not found in $PATH` - which names the container runtime's own failure to
  find a shell in a distroless image, and therefore says the request reached containerd rather than
  stopping at a proxy. The front is named: Caddy on `socialweb.computer` terminates TLS for
  `kcp.socialweb.computer` and reverse-proxies `https://127.0.0.1:6443` with `versions 1.1` and
  `flush_interval -1`, which is what carries the upgraded exec connection; that front is reachable
  anonymously and answers `403 forbidden: User "system:anonymous" cannot get path "/version"`, so what
  it exposes to a caller without a credential is a refusal and nothing else.
  The runner itself is the second reading and the one the instrument asked for: the GitLab runner
  registered against project 1 uses the `kubernetes` executor, which runs a job's script with
  `pods/exec` in the build container, and it has completed FIVE jobs at once on five distinct market
  nodes with every job green (pipeline 75, `scale-check-1..5`, 198-202 s each), plus the same five
  green in pipeline 71 before it. A path that could not traverse the mount could not run a job.
  `NEXT.yaml` GL-2.

- **The minting CLI had never been run against a real cluster's data dir, and could not have been:
  it was not in the image. CLOSED.** `cmd/socialweb-tenantcredential` builds, is unit-tested, and the
  container image built eight other commands and not this one - so the tool the section-14 bullet was
  about existed and was undeployable, and its instrument ("a run of the minting command against a real
  cluster data dir") could not be carried out anywhere the state dir is. The section-14 bullet's own
  premise had also gone stale: it said no command writes a minted credential anywhere an operator can
  use, and the command had existed since the entry below.
  Built and copied into both final stages, beside `socialweb-vault`, and then run for real on
  `root@socialweb.computer` on 2026-10-07 as uid `kcp` inside the container - the uid that can read
  `<StateDir>/pdr-<key>/server/tls/client-ca.key`, mode 0600, which is why this cannot be done from
  outside:
  `SOCIALWEB_TENANTCREDENTIAL_STATE_DIR=/var/lib/socialweb/clusterprovider`,
  `..._CLUSTER=2wjeqtgx-test-cluster-alice-0001`, `..._SERVER=https://127.0.0.1:20003`,
  `..._COMMON_NAME=cli-close-gl-1`, `..._ORGANIZATIONS=system:masters`,
  `..._OUT_DIR=/tmp/tenantcred`. It printed
  `validUntil=2036-10-04T07:13:11Z ca=/tmp/tenantcred/ca.crt cert=/tmp/tenantcred/tls.crt
  key=/tmp/tenantcred/tls.key`, and the three files came out mode 0600 owned by `kcp`.
  `kubectl` with exactly those three files against the tenant apiserver listed the cluster's own Nodes
  (`node28` to `node32`, all Ready), and `kubectl auth whoami` reported `Username cli-close-gl-1` and
  `Groups [system:masters system:authenticated]` - the apiserver naming back the identity the CLI
  claimed to mint, which is the reading the instrument asked for. A second run refused:
  `these files already exist and this tool will not overwrite a credential ... that silent replacement
  is the failure invariant 20 exists to prevent`. The credential was removed from the container's
  `/tmp` afterwards. `NEXT.yaml` GL-1.

- **A host a caller adds to the vault's certificate is not served until the vault restarts. CLOSED as
  a STATED LIMIT, with a refusal where a URL would otherwise be handed out.**
  `vaultcert.EnsureVaultTLSCertificate` re-signs the leaf under the cluster's existing CA when a caller
  asks for one more host, so no client loses the anchor it trusts (the entry above). What it cannot do
  is make the RUNNING vault serve the new leaf, and the reason is where the handle lives: OpenBao reads
  `tls_cert_file` once at start; the vault step's own gate (`internal/clusterprovider/steps.go`) runs
  the vault a whole pass before the tenant add-on pass and the node disk stage exist; and those two are
  closures in `cmd/socialweb-clusterprovider` (`eso.go`, `nodes.go`) built without the materialiser's
  process group, whose `LocalHost` is the only thing holding the vault's `processrunner.Process`. On a
  market cluster the two host sets differ by construction: bring-up asks for
  `SOCIALWEB_MARKET_ADVERTISE_ADDRESS` and loopback (`internal/clusterprovider/stack.go`), the tenant
  add-on pass asks for the `10.100.0.0/16` gateway `internal/subnetallocator` leases and loopback
  (`internal/providermain/eso.go`). The reading, against a real OpenBao v2.6.2 on 2026-10-07:
  the vault started covering `[198.18.255.254 127.0.0.1]`, the tenant add-on pass re-signed the leaf to
  cover `[198.18.255.254 127.0.0.1 10.100.0.1]` on disk, the vault went on serving
  `[198.18.255.254 127.0.0.1]`, and a client dialing it as `10.100.0.1` failed with `tls: failed to
  verify certificate: x509: certificate is valid for 198.18.255.254, 127.0.0.1, not 10.100.0.1`. The
  window is from the re-sign until the vault process next starts; the leaf on disk already carries every
  host a caller has asked for, so a restart - or a cluster teardown and bring-up - closes it, and
  nothing asks for a NEW host after that. The next-best behaviour is taken: the tenant add-on pass
  REFUSES to hand the ExternalSecret a URL the running listener cannot present
  (`vaultcert.RequireServedVaultTLS`, called from `internal/providermain/eso.go`), failing that
  pass with the reason rather than writing a Secret whose client is guaranteed to fail with a name
  mismatch. A signal-based repair was refused with it - it would need a handle the re-sign site does
  not have, and a reload whose completion nothing observes would let a reload that did not happen and
  one that did look identical, the defect class `ErrNoGuardSeam` exists to prevent - and so was moving
  the gateway into the FIRST mint, whose host source is the allocator itself: `subnetallocator.Allocate`
  walks the pool by occupancy, so the gateway is not derivable without it, and leasing a cluster's range
  on the vault bring-up path would change what the zero-node run allocates (section 14). Pinned by
  `TestARunningVaultDoesNotServeAHostItsLeafGainedAfterItStarted` in `internal/clusterprovider`, which
  takes the reading and fails without the refusal. `NEXT.yaml` VAULT-TLS-1, then a limit; the
  signal-based repair refused here was built with the observation it lacked, and the limit closed, by
  plan 0005 M6-LIMITS (the entry at the end of this section).

- **The legacy-key migration ran in a different process from the one that reads what it renames.
  CLOSED.** The clusterprovider writes a mint credential as `<MINT_DIR>/<accountHash>-<name>` and the
  gateway reads that name, so the two agree from the clusterprovider's first pass onward. What did not
  agree was the ORDER: the entrypoint started the gateway first, and the rename of an existing
  `<MINT_DIR>/<name>` was the clusterprovider's own startup migration, so a mint request arriving in
  between named a file that was about to be renamed and was refused. It failed closed - an error naming
  the cluster and the key, never another cluster's credential - so the cost was a window of seconds and
  a refusal that reads to an operator as a broken deployment.
  The clusterprovider gained a `migrate` subcommand that opens the same inputs the gateway needs
  (`buildInputs`, extracted from `buildDeps`) and runs `migrateLegacyKeys`, and the entrypoint runs it
  BEFORE starting the gateway. A migration that fails does not stop the deployment: the gateway is
  started anyway and the clusterprovider runs the same migration itself, because a gateway that never
  starts is worse than a mint refused in the seconds before that one does. The ordering is what the fix
  IS, and it lives in a shell file, so it is pinned by a test that reads
  `deploy/container-entrypoint.sh` and asserts the `migrate` invocation appears before the gateway is
  started - `TestTheLegacyKeyMigrationRunsBeforeAnythingCanReadAMintCredential`, watched to fail with
  the step removed: "the container entrypoint never runs the legacy-key migration as its own step".
  `NEXT.yaml` MINT-1.

- **A node record did not say which substrate wrote it, so a sweep could only reap its own. CLOSED,
  and it closed with the reading the item asked for.** `noderecord.Record` held a pid, a start time
  and an identity, and the identity is the ARGV the process was started with - which only the substrate
  that started it knows how to read. `nodeIdentityFor` switched on the CONFIGURED substrate, and
  `sweepNodeRecords` returned early under `market`, so a state dir a firecracker or qemu gateway left
  behind was never walked and its guests stayed up.
  The record now carries `Substrate`, `noderecord.Write` refuses an empty one, and the four writers set
  it: `internal/nodefirecracker` and `internal/nodeqemu` from a name constant each, and
  `internal/nodevm` from `NamespaceParams.Kind`, which is the substrate the namespace child was started
  for and was already in the file it reads. The sweep resolves the identity pair PER RECORD from
  `rec.Substrate` rather than once from its own configuration, and a record naming a substrate with no
  identity function is a refusal naming that substrate, never a skip - guessing reads a live guest as
  gone and a gone one as live.
  Readings: `TestTheMarketSweepReapsWhatAnotherSubstrateLeftBehind` starts a stand-in process, records
  it under firecracker, sweeps it with a gateway configured for `market`, and asserts the process is
  gone and the record cleared - the item's own closing reading, watched to fail with the market
  early-return restored (`waitGone` timed out). `TestASweepStopsOnARecordWhoseSubstrateItCannotRead`
  holds the other side. `TestTheSweepRefusesASubstrateWithNoIdentity` asserted the up-front refusal
  that this replaces and was rewritten to the per-record one rather than deleted. `NEXT.yaml` MARKET-1.

- **The tenant store was written with a vault address nothing could reach, so the demo
  ExternalSecret had never synced on a market cluster. CLOSED.** The store's vault server is
  `https://<the cluster's subnet gateway>:<vault port>` (`internal/providermain/eso.go`). On
  a guest substrate that address is inside the cluster's own range -- the guests dial it and the host
  binds it -- and the demo was measured syncing there, which is where section 15's claim about
  `Secret/users/openbao-demo` comes from. On `market` it is neither bound nor routed.
  MEASURED on production on 2026-10-07, one cluster on `SUBSTRATE=market`: the store read
  `https://10.100.0.1:20001`; `grep -c 0A640001 /proc/net/tcp` inside the container was `0`, so
  nothing on the host binds that address; the only listener on 20001 was `0100007F:4E21` -- loopback
  -- and it is the cluster's own `bao server`; the store read `Ready=False unable to create client`
  and the ExternalSecret `Ready=False reason=SecretSyncedError`, `could not get secret data from
  provider`.
  The entry above about the served leaf does NOT catch this, and the distinction is the point: that
  check asks whether the running vault can PRESENT the host, and the leaf does carry 10.100.0.1 --
  what it cannot do is answer, because there is no listener there and no route to one.
  Closed by the third of the item's own instruments: the store is not written where nothing can reach
  it. `tenantAddOns` installs the operator on every substrate and returns before the store, the demo
  value and the ESO certificate when `tenantStoreIsReachableFrom` is false, with a logged decision
  naming this entry. `TestTheTenantStoreIsNotWrittenWhereNothingCanReachIt` pins the decision for all
  three substrates, watched to fail with it forced true. Not writing is not enough on its own, and
  that was measured too: an upgraded cluster kept the store an older version had written, because
  nothing removed it, so `eso.Remove` takes back what `eso.Apply` wrote -- by the names in the same
  manifest directory -- and the pass calls it on that path.
  `TestRemoveTakesBackTheObjectsThisInstallerWrote` seeds the objects the manifests name and asserts
  none survive; watched to fail with `Remove` made a no-op: "1 clustersecretstores objects survived
  the removal".
  One imperfection is left in it deliberately rather than fixed in a hurry: the removal is ATTEMPTED
  on every pass, because nothing remembers that the cluster's store is already gone. Each attempt is
  three deletes that come back NotFound, so it is cheap and it is bounded by the 60 s timeout -- but
  it is the same shape as the unguarded installer that stalled the provider the same night, and the
  fix (a per-cluster flag in the closure that owns the pass, so the removal is attempted once per
  process and a restart retries it) is a change to a path that runs inside every pass. It was not
  made at that hour because it would have shipped unverified, which is the mistake being recorded.
  (Closed by deletion: since M4-ESO-BESIDE the add-on pass no longer calls `eso.Remove` at all, and the
  same per-cluster memo was then built for the released-Node reconcile, plan 0005 M6-LIMITS.)
  Removing it took three passes to get right, and each correction came from production rather than
  from reading. FIRST: not writing is not enough, because an upgraded cluster keeps what an older
  version wrote. SECOND, and this is the one worth reading twice: the ExternalSecret went and then sat
  TERMINATING on ESO's own finalizer
  (`externalsecrets.external-secrets.io/externalsecret-cleanup`), which the controller removes after
  cleaning up the Secret it owns -- and that controller runs at ONE node, so on a cluster at rest,
  which is every market cluster that is not working, it is Pending and removes nothing. The object
  would have sat there indefinitely. The removal now deletes the ExternalSecret FIRST, so a running
  controller has something to work against, and then releases the finalizer itself, because the target
  Secret is already gone and waiting for a controller that cannot schedule is not a cleanup.
  The first version of that test passed against a no-op release, and it was the FAKE that was wrong:
  it deleted an object with finalizers outright, where an apiserver marks it Terminating and waits.
  With the fake made to behave like an apiserver,
  `TestRemoveReleasesAnExternalSecretTheControllerCannot` fails as it should when the release is
  removed.
  The first version of THAT change stalled production, and the way it read is the lesson. The
  store-less path called the installer function directly, which bypasses `eso.InstallOperator`'s "only
  if the group is not served" guard, so `bash install.sh` ran against the tenant apiserver on every
  pass; the pass is synchronous, so the node-scale pass behind it stopped with it. MEASURED minutes
  after it shipped: the last line the clusterprovider logged was `installing the External Secrets
  Operator into the tenant cluster`, nothing followed it, and the process showed ZERO cpu time 51
  seconds in -- blocked rather than working. It now calls `InstallOperator`, which is what the guest
  path has always reached through `applyTenantAddOns`, and the removal carries a 60 s timeout so the
  worst case is a pass that fails loudly rather than a provider that stops. NO instrument covers this
  one: the stall was found by reading the log and the process's cpu time, and what would have caught
  it is a test that counts the installer's invocations per pass, which does not exist. What is given up is stated rather than
  implied: a market cluster now has the operator and no store, and a tenant who wants one configures
  one they can reach. The other two instruments remain open as options -- reaching the vault through
  the door the node's SSH session uses, or publishing the vault on the provider host behind the
  client certificate that is already minted per cluster -- and both are trust decisions rather than
  bugs, which is why neither was taken here. They were scoped rather than left as words, and the
  scoping is the decision material:
  THROUGH THE DOOR. The door already opens exactly this bridge for the apiserver -- one ssh2
  `forwardIn` per session, pumped back to the provider host
  (`socialweb-computer-k8s/lib/socialweb-computer-k8s-node-market/guest.ts:198`, `:223-235`) -- so a
  second forward for the vault port carries the client certificate unchanged, because the forward is
  a raw TCP pipe and not a TLS terminator; the vault's own `auth/cert` stays the thing that decides,
  and a credential for one cluster cannot open another's vault because the role trusts only that
  cluster's imported CA. Two companions are required and neither is optional: the advertised address
  is a `/32` on the GUEST's own loopback, so a pod's CNI cannot reach where the forward would land,
  and the vault's leaf would have to carry the advertise address or the store's server-name check
  fails. And the forward lives and dies with the join session, which is reaped and renewed (MARKET-6),
  so vault reads would fail whenever a session drops -- the apiserver path lives with that; the vault
  never has.
  PUBLISHED ON THE PROVIDER HOST. This is the highest-blast-radius option: it puts a vault listener on
  the internet and makes the edge the first gate, where a miswritten site exposes every vault to
  anonymous TLS and one leaked ESO client certificate -- it lives in a tenant-namespace Secret --
  reads from anywhere. It also has an unanswered question that has to be settled before it can be
  called achievable at all: the vault authenticates by CLIENT CERTIFICATE IN THE HANDSHAKE, and an
  HTTP-terminating Caddy does not forward that certificate upstream, so as stated the vault would
  never see it. Settling that means L4/SNI passthrough to a per-cluster port, or replacing cert auth
  with a token.
  A THIRD OPTION, cheaper and honest, which the item did not name: under `market` keep the OpenBao
  store off and write instead a small, DIFFERENTLY-NAMED store using a provider that lives inside the
  tenant cluster (a `kubernetes` provider reading a ConfigMap). That proves operator installed ->
  store `Ready` -> `SecretSynced` end to end inside the cluster, with no provider-host transport and
  no new exposure -- it proves less, in that it does not exercise OpenBao on market, and it has to say
  so by name. The manifest set is already a directory chosen by config, so this is one YAML and one
  branch beside the log. Leaving the demo out entirely under market is the same shape and already
  half-built; it is the honest minimum that proves nothing.

- **The tenant grant admitted the External Secrets controller by the literal namespace `default`, and
  the installer could not have put it anywhere else. CLOSED, and the second half was found by taking
  the reading.** The grant's CEL policy admitted exactly
  `system:serviceaccount:default:external-secrets`, so an install anywhere else was denied and the
  denial read as a broken ExternalSecret rather than as a grant that does not name it.
  MEASURED BEFORE, on production: the policy named `default`;
  `kubectl create secret --as=system:serviceaccount:external-secrets:external-secrets -n users` was
  `forbidden`, while the same create as `system:serviceaccount:default:external-secrets` was
  `secret/... created (server dry run)`.
  The grant is a template, not a file applied verbatim, so the namespace became a placeholder
  `RenderTenantGrant` substitutes, from one source that also sets `ESO_NAMESPACE` for the installer, and
  the default is `default` on every path so an unchanged deployment renders byte-identically. MEASURED
  AFTER: the policy names `external-secrets`, and the same create as
  `system:serviceaccount:external-secrets:external-secrets` is `created (server dry run)`.
  Taking that reading is what showed the fix was only half of one. The upstream ESO bundle names
  `default` in SIXTEEN places -- the metadata of every namespaced object, the namespace of each
  ClusterRoleBinding's subject, and the webhook's service namespace -- and defines no Namespace object
  of its own, so `ESO_NAMESPACE` only ever decided which namespace the ROLLOUT WAIT looked in. With the
  grant naming `external-secrets` and the controller still installed in `default`, the grant admitted a
  controller that was not installed and denied the one that was. The installer now creates the
  namespace and rewrites those sixteen references; measured after: the controller's three pods are in
  `external-secrets`, its bindings' subjects name it, and `default` -- which the grant no longer names
  and which STILL has the RBAC -- is refused, which is the pair that isolates the policy from RBAC.
  What this does NOT close is the ExternalSecret's own sync: that needs a store a tenant can reach,
  which is ESO-3, and the two are separate. `NEXT.yaml` ESO-2.

- **The OpenBao live gate lent itself a port it had already given back. CLOSED.** `freePort` listens
  on `127.0.0.1:0`, reads the number, CLOSES the listener, and writes a vault config on that number
  with `tls_disable = true`. Between the close and the vault binding it, the number is free for anyone
  -- including a second `go test` of this repo running at the same time. MEASURED on 2026-10-07, with
  two other test runs in flight: `TestClientAgainstRealBao` failed with `the vault answered before it
  was ready, which this gate did not expect: openbao: GET /v1/sys/health -> HTTP 400: Client sent an
  HTTP request to an HTTPS server` -- the gate's plain HTTP probe reached somebody else's TLS listener
  on the port it had just given back, and the strict assertion that a vault which answers wrongly is a
  failure was reading a stranger. This is the class the TypeScript side already carries a rule
  against; it was written there and not here.
  The start now retries with a FRESH port when either shape of collision is seen, and there are two:
  something else ANSWERING (an HTTP-level error instead of a transport one, which is what was
  measured) or something else HOLDING the address so the vault cannot bind and exits (found by trying
  to force the first shape with a silent listener, which produced the second). The strict assertion
  stays: three attempts and the failure names the port race, the attempt count, and the address. The
  vault's exit is now observed through a channel rather than a bare `Wait`, so a vault that died at
  its first step is reported with its own stderr instead of waiting out the deadline.
  Closing reading, and it is the item's: TEN concurrent `go test ./internal/openbao/ -count=1` runs,
  all green. What is NOT shown is the retry firing under a real collision -- forcing one with a
  listener held on the chosen port produced the bind-failure shape rather than the answering shape,
  and the retry's firing is therefore still unobserved.

- **FLAKE-1: a gate test could fail for reasons unrelated to a change. CLOSED, and the reason was not
  the one the item named.** The item read: `TestAnImageWhoseNameItsRecordDoesNotHashToIsRefusedInBothDirections`
  in `cmd/socialweb-nodeimage` "resolves a live registry tag to a digest", so `--fast` could fail for
  reasons unrelated to a change. Taking that reading refuted it -- a full `go test ./... -count=1` with
  `HTTPS_PROXY` pointed at a dead port is green across all 50 packages, and the test that names a bare
  tag passes the digest in as an argument rather than fetching it -- and the failures were something
  else entirely.
  THE MECHANISM, established by an agent that reproduced it: 16 pass / 4 fail over 20 sequential runs,
  6/6 fail in one concurrent round and 6/6 pass in the next, and 20/20 pass with
  `GOFLAGS=-buildvcs=false`. An image's NAME carries the digest of the agent binary
  (`internal/nodefirecracker` folds `fileDigest(cfg.AgentBinary)` into the fingerprinted inputs), and
  that binary is built TWICE in one test process: once by the fixture's `cachedAgent`, and again by
  `Main`'s `buildAgent` before it fingerprints. Anything that changes those bytes between the two
  builds moves the name, so `completeImage`'s `Stat` misses, `Main` takes the build path, and
  `checkImagePins` refuses the fixture's unpinned image -- which is the failure text that was on
  record. What changed the bytes was `go build`'s default `-buildvcs=auto` stamping the state of this
  git working tree, so a concurrent process's edits moved it.
  Three changes closed it. `buildAgent` now always passes `-buildvcs=false`, which the deployed
  configurations already did, so an image's name follows its inputs and not the tree around them. The
  test computes the image's name BOTH ways -- from the configuration in memory and from the
  configuration on disk -- and fails naming both if they differ. And when `Main` fails AND the agent
  binary's digest changed under the run, the test SKIPS with both digests and this mechanism spelled
  out, because its premise (the image is already in the store) is void when an input changes
  underneath it; under the full gate it still fails, through `livegate.Require`, because a skip is not
  a pass. That skip was watched to fire: with the agent binary forced to change during the run, the test
  reported `the agent binary changed UNDER this run, from 9a6aae16... to 6e881ecb...` and skipped,
  where it used to report a red about an unpinned image.
  What is NOT claimed: my own attempts never reproduced the failure -- 20 sequential runs, then 8 with
  the working tree flipped dirty and clean every 0.3 s, then the same 8 with the flag removed again,
  all green -- so the reproduction behind this entry is the agent's. And the stronger fix was measured
  and REJECTED: building the fixture's agent where `Main` will rebuild it narrows the window from the
  whole test process to the milliseconds between the two builds, and took the package from about 4 s to
  about 24 s, because the agent is a real `go build` and seven tests then pay for one each.


- **An idle cluster of only our own add-ons held market nodes for seven hours. CLOSED by a measured
  baseline and by counting a pod once.** Reading on prod, 2026-10-07: the last GitLab job finished at
  07:58 UTC, and from 08:10 to 15:43 the cluster `2wjeqtgx-test-cluster-alice-0001` held seven market
  nodes, deciding `None` every pass with "scaling in is all-or-nothing" because it always had intent.
  The tenant held no workload of anyone's but ours: the External Secrets Operator's three Deployments
  in `default`, `replicas=1`, their pods Pending. When the bidder behind those nodes stopped, the
  cluster asked for SIX nodes, not three, because `Demand` counted each Pending pod AND its
  Deployment's replica (3 + 3) and prod runs one pod per node. Both halves are in section 10: the
  installer's own field manager marks what it applied, the provider hashes those workloads' live
  specs into `addon-baseline.json`, and `scale.Narrow` drops them and every pod whose controller is a
  counted workload. Tests watched to fail first: `TestOurInstalledOperatorAloneIsNothingDeployed`
  (demand 6 before), `TestAPodAndItsDeploymentAreOneUnitOfDemand` (6, want 3),
  `TestABaselineWorkloadWhoseSpecChangedIsIntent`, `TestATenantWorkloadBesideTheBaselineIsIntent`,
  `TestABaselineEntryNamesOneNamespace`, `TestPodsOfACountedJobStatefulSetOrDaemonSetAreNotCountedTwice`
  in `internal/scale`; `TestObserveLeavesAnIdleClusterOfOurAddOnsAtNoIntent` (6, want 3) and
  `TestTheBaselineIsWhatOurFieldManagerApplied` in `internal/clusterprovider`.

- **A market node lived only as long as the door's ssh session to it. CLOSED on the door's side by
  plan 0005 M3-SECRETS-TICKET and M3-UNITS (commits 0f3a771, b489218); the provider's side is open.** Before,
  the door generated an ssh keypair per claim, waited for the guest's sshd over dumbpipe, ran
  `socialweb-k3s-join` with the join token, node password and pinned CA on its stdin, and held that
  session (and a `forwardIn` of the advertised address) for the node's whole life; a door restart
  killed every agent, and startup reaped every live claim. Now the door delivers three public files
  through the secrets capability, writes `status.grant` before the accept, returns at
  `onSecretsFetched` with the claim `Joining`, and releases only on deletion; the guest's four units
  fetch the secrets from the cluster's OpenBao and dial the apiserver themselves (section 9, "A market
  node joins through its cluster's OpenBao, never through ssh"; invariants 13 and 29 rewritten).
  Tests watched to fail first (against the previous door and module with stub exports only, run
  `--no-check`: 29 failed, 3 passed), then pass: in `socialweb-computer-k8s/test/node_market_test.ts`
  "at accept the claim carries the workload grant for its node's audience before the guest can be
  authorized", "the secrets bundle carries exactly the OpenBao ticket, the OpenBao CA and claim.json",
  "the flow returns at onSecretsFetched, the claim is Joining, capabilities are disposed and nothing
  is released", "the door asks for no ssh key and builds no ssh session", "the user_data the door
  asks for renders without any secret value", "a door restarted with live claims adopts them and
  submits no vm.delete", "a door restarted mid-flight marks the orphaned claim Failed, releases
  nothing, and releases it once deleted", "a deleted Joining claim is released from the receipt on
  the claim, not the door's memory"; in `test/k3s_join_test.ts` "the node's user_data installs four
  units that join it through OpenBao and the apiserver link, in order", "the node's user_data carries
  no secret value and no ssh join path", and the node-secrets script run against a fake curl and the
  real jq ("exchanges the workload token for the claim's audience, logs in to OpenBao and writes the
  join files 0600"). Mutations, each on a scratch copy and reverted, each caught: the grant written
  after the capability is authorized, or not written; `onSecretsFetched` returning `continue`; a
  restart reaping live claims; a release at the end of a successful flow; the agent unit not ordered
  after the apiserver link; the token not pinned to the CA. NOT proven: a guest joining end to end
  (needs the provider's ClusterLinks, OpenBao JWT mount and KV entry, and the live instrument
  `test/live_market_node_test.ts` now also needs `SOCIALWEB_K8S_LIVE_OPENBAO_TICKET` and
  `SOCIALWEB_K8S_LIVE_OPENBAO_CA`); nothing deployed.

- **The External Secrets Operator ran as tenant pods, so it worked only while a node existed --
  closed by running it beside the control plane (`NEXT.yaml` ESO-4, plan 0005 M4-ESO-BESIDE).**
  Before: `deploy/eso/install.sh` applied the upstream bundle into each tenant (controller,
  cert-controller and webhook Deployments, webhooks `failurePolicy: Ignore`); at zero nodes the pods
  were Pending and nothing synced, and on the market substrate the add-on pass did not write the
  store at all, because its vault address was a subnet gateway a market guest cannot route to
  (ESO-3). After: the controller and its webhook are two members of each cluster's process group
  (section 5, "The External Secrets controller runs beside the control plane"), the store dials the
  cluster vault on `127.0.0.1`, and the tenant holds CRDs, ClusterRoles, one ClusterRoleBinding to
  the beside identity and the URL webhook registrations. Readings, 2026-10-07, on this host with
  the image's own binary (sha256 `9ea572f5...` for `/usr/bin/external-secrets` out of
  `ghcr.io/external-secrets/external-secrets:v2.11.0@sha256:66fb7108...`, the same bytes the
  Dockerfile `COPY --from` produced) and the release bundle (sha256 `c4271246...`):
  `TestAZeroNodeMarketClusterSyncsAnExternalSecretFromItsOwnVaultWithNoESOPodInTheTenant` PASS in
  25.4 s -- a `MarketDialled` cluster advertised on `198.18.0.7` reached Ready with the pair in its
  group in 22.68 s; with zero nodes, an ExternalSecret carrying neither `data` nor `dataFrom` was
  refused with `admission webhook "validate.externalsecret.external-secrets.io" denied the request:
  either data or dataFrom should be specified`, which only the webhook says, so the apiserver
  (`--egress-selector-mode cluster`) reached `https://127.0.0.1:<leased>` directly; the store went
  `Ready=True reason=Valid` against `https://127.0.0.1:<vault port>`; and `users/openbao-demo`
  carried `openbao:<cluster>` 500 ms after the apply, with 3 tenant pods (k3s's own Pending
  add-ons), none of them External Secrets, and no External Secrets Deployment. Watched to fail
  first: (1) the seven `eso` tests against a stub that split, registered and installed nothing;
  (2) the live test before the grant change, whose store read `Ready=True reason=Valid` while the
  ExternalSecret read `SecretSyncedError: ... ValidatingAdmissionPolicy 'socialweb-mint-identity'
  ... denied request` -- the policy still admitted only `system:serviceaccount:default:external-secrets`,
  which is now `__SOCIALWEB_ESO_USER__` rendered as `system:socialweb:external-secrets`; (3) the same
  test's first probe, a `ClusterSecretStore` with an empty provider, was refused by the CRD schema
  rather than the webhook, so it proved nothing and was replaced by the data-less ExternalSecret.
  What a deploy does to an existing cluster: the restart reap derives argvs without the pair (its
  lease-file entry holds none of the three purposes), the bring-up leases them, and the webhook
  step's `Prepare` deletes the in-tenant Deployments, ServiceAccounts, webhook Service and Secret,
  the cert-controller ClusterRole, both upstream ClusterRoleBindings and the leader-election
  Role/RoleBinding from the configured namespace (`SOCIALWEB_CLUSTERPROVIDER_ESO_NAMESPACE`,
  default `default`), deletes each webhook registration that still names a Service, and applies
  the URL ones; the next `Bootstrap` re-renders the grant, writes the store on loopback and
  re-measures an empty add-on baseline. Stated limits kept: neither member is restarted when it
  dies, as the cloud controller manager is not (plan 0005 M6-LIMITS); the webhook's TLS listener
  binds every interface of the container's namespace; the image grows by the 295 MB binary.

- **Four stated limits, fixed (plan 0005 M6-LIMITS). CLOSED 2026-10-07.** Each was a `NEXT.yaml`
  limit or a section 15 imperfection recorded as left on purpose; each now has a test watched to fail
  against the old code.
  ONE, a process-group member that binds no port and leaves no record was not swept. The cloud
  controller manager outlived a gateway that died without its reverse-order reap, and nothing on the
  host would ever stop it. Now `procgroup.Spawn` sets `processrunner.Spec.DieWithParent` on every
  step: `Pdeathsig: SIGKILL`, spawned from a goroutine that holds `runtime.LockOSThread` until the
  child is reaped, because Linux fires the signal when the FORKING THREAD exits. All members get it,
  not only the portless one, because a survivor of any member is never adopted -- the restart reap
  kills what it finds before the bring-up spawns -- so the signal only makes that death immediate.
  The port sweep is unchanged. `TestAGroupMemberDiesWithTheProviderAndNotWithTheThreadThatSpawnedIt`
  re-execs the test binary as a provider that spawns the group from a locked goroutine which then
  returns. Readings on hat-8: with no Pdeathsig the member outlived the SIGKILLed provider by the full
  5 s bound; with a bare `Pdeathsig` and no held thread the member was already a zombie when the test
  first read it, killed by its spawning thread's exit under a healthy provider; with the held thread
  it is alive 500 ms after that thread's goroutine returned and gone 0-20 ms after the provider is
  SIGKILLed.
  TWO, the instance snapshot was load-bearing and absent read as empty. `FileInstances.Get` now
  answers `ErrNoSnapshot` for a missing `nodes.json`, so the controller requeues the Node instead of
  deleting it; the `[]` the provider writes still means no guest. Absent is reachable only by a state
  dir removed under a running controller, and that is now a refusal rather than every Node deleted.
  `TestTheControllerDeletesNoNodeOverAnAbsentSnapshotAndDeletesOverAnEmptyOne` reads `deleted=[]`
  over an absent file and `deleted=[c1-node1]` over `[]`; before, the first Reconcile returned nil
  and deleted the Node. `TestAMissingInstanceFileIsAnErrorNotAnAbsence` and
  `TestAnAbsentSnapshotIsRefusedAndAnEmptyOneIsEmpty` replace the two tests that pinned the old
  reading.
  THREE, a running vault did not serve a host added to its leaf later (VAULT-TLS-1). The repair was
  refused before for two reasons and both are answered. The handle: the re-sign site has the data dir
  and the vault's port, and that is enough to find the process without the materialiser -- the one
  process holding the port (`portscan.ListenerPIDs`) whose command line is exactly
  `bao server -config=<OpenBaoConfigPath(dataDir)>`; any other holder is refused and never signalled.
  The observation: after the SIGHUP the SERVED leaf is read again, up to 10 s, so a reload that did not
  happen still reads as a refusal (`ErrVaultReloadDidNotTake`). `vaultcert.EnsureServedVaultTLS` is
  re-sign, read the served leaf, and only if it lacks a host, reload-then-verify; `esovault` and the
  node vault stage call it. OpenBao v2.6.2 reloads listener TLS on SIGHUP, MEASURED on hat-8:
  `TestARunningVaultServesAHostItsLeafGainedOnceItIsReloaded` started a real vault serving
  `[198.18.255.254 127.0.0.1]`, asked for `10.100.0.1`, and read `[198.18.255.254 127.0.0.1
  10.100.0.1]` served 114 ms after the call began, with a client verifying as `10.100.0.1` against
  the CA on disk; with the reload a no-op it failed with `ErrVaultReloadDidNotTake` after 10 s. The
  refusal is kept and pinned without a vault: `TestAReloadThatDoesNotTakeKeepsTheRefusal` and
  `TestAListenerThatIsNotTheVaultIsNeverSignalled` (the port's holder is the test binary, which a
  SIGHUP would end). The 21 vault tests of `internal/clusterprovider` pass with it under
  `SOCIALWEB_LOCAL=1`. As of this change no production caller grows the host set after start --
  bring-up already covers what the add-on pass and the node stage ask for -- so this closes the window
  for the next caller rather than one that is open now.
  FOUR, the per-pass deletes that come back NotFound. The ESO store removal that did them has had no
  caller since M4-ESO-BESIDE, and the released-Node reconcile had already stopped deleting a Node that
  was gone (`TestAReleasedClaimWhoseNodeIsAlreadyGoneCostsNoDelete`); what it still did was LIST the
  tenant's Nodes on every pass for the life of the process, because a Released claim stays in its
  workspace for ever. `nodeHosts` now keeps a per-cluster set of released claims (name and UID) whose
  Node it deleted or saw absent; a pass whose Released claims are all in it makes no tenant call, the
  set is pruned to the claims still Released on each pass, and a restart retries.
  `TestTheReleasedNodeReconcileCostsTheTenantNothingOnceItsNodesAreGone` counts calls over 5 passes:
  before, 1 DELETE and 6 calls (5 lists); after, 1 DELETE and 2 calls, and a claim released later
  still gets its one delete. The cost: a Node that re-registers under a name whose Released claim is
  already remembered is not deleted again until the provider restarts. That needs a kubelet on a
  machine the door took away, and a claim re-written under the same name is a new object with a new
  UID, so it is not remembered; the provider also never claims under a name whose claim has ended
  (`TestTheFirstNodeOfAClusterIsNotClaimedUnderASpentName`).

- **A market cluster's kube-scheduler was given a 1 s pod backoff, and not the flags the plan asked
  for, on 2026-10-10. CLOSED (plan 0005 M3-JOIN-FOLLOWUPS part 2).** The prod reading left the shape
  of the fix open -- `--kube-scheduler-arg=pod-initial-backoff-seconds=1` and its max sibling, or a
  `KubeSchedulerConfiguration` file -- and the real binary answered it: k3s v1.36.4+k3s1's
  kube-scheduler defines neither flag (its own `--help`, printed when it is handed a flag it does
  not know, lists `--config` and a deprecated `--pod-max-in-unschedulable-pods-duration`; a
  non-numeric value for `--pod-initial-backoff-seconds` is not rejected, so it is not a flag it
  parses), so the file form is taken, and it is read -- a config of garbage bytes makes
  kube-scheduler exit while the k3s server stays up, which the probes pin in both directions. The
  file is written into the cluster's data dir by `clusterprovider.WriteKubeSchedulerConfig` before
  the steps are built, and the market argv names it. Two decisions inside the file: `clientConnection`
  is set, because with a `--config` file kube-scheduler ignores the deprecated `--kubeconfig` flag
  k3s still passes and a config without it leaves the scheduler with no apiserver to talk to; and
  both backoffs are 1 s rather than the initial alone, because the initial is what a cluster that has
  been empty for one pass pays and the cap is what it pays after that. Market clusters only, since
  only they start from zero nodes and the plain argv names no file. What the change does not claim,
  and what the next reader should check before believing the root cause: in this host's reproduction
  a pod waiting on a node was requeued about 50 ms after the node became schedulable with the
  DEFAULT backoff, in every shape tried (node Ready, a taint removed, a label added), so the 16 s
  read on prod was not reproduced here and the backoff is a bound on the worst case rather than a
  measured cause.

- **The bootstrap layer is one table, and kine listens on a unix socket (plan 0006 M2-BOOTSTRAP-LAYER,
  2026-10-12).** `internal/bootstrap/table` lists the units that must run before kcp exists, per
  application instance, in the order store, kine, front, slot, core. The gateway is
  `socialweb-kcp-gw-pg`, `socialweb-kcp-gw-kine@a|b`, `socialweb-kcp-gw-front`, `socialweb-kcp-gw@a|b`
  and `socialweb-computer-gateway`; a bidder handle is `socialweb-kcp-<hid>-kine@a`,
  `socialweb-kcp-<hid>-front`, `socialweb-kcp-<hid>@a` and `socialweb-computer-bidder@<handle>`; a dev
  instance is the gateway shape keyed by its short id with one slot. The same table renders the
  devsup unit specs that `socialweb-computer dev up` places (proc) and the user unit files, the
  `<state>/kcp/<unit>.env` argument files and the core's `After=`/`Wants=` drop-in that install writes
  (systemd-user, started by name through `hostsvc.StartInstalled`, enabled for boot). Dev profiles:
  `quick` is kine on sqlite, `prodlike` is kine on a postgres unit (unix socket only, initdb one-shot
  before the unit starts); `market-network` waits for M2-APP-CONTROLLER. Kine binds `unix://<run>/kine-<slot>.sock`
  instead of TCP port 0 (decided by Claude, accepted by the lead): the address is stable across
  restarts, so a slot's `--etcd-servers` in its env file stays right when the user manager restarts kine
  at boot without the core, and no TCP port is opened at all. Readings on hat-8: `dev up --profile
  quick` 11.2 s cold to a Ready kcp for each of two instances in parallel; `dev up --profile prodlike`
  18.6 s cold, RSS pg 33 MB, kine 58 MB, front 33 MB, kcp 442 MB; the installed postgres layer started
  by name in a user manager (test prefix) in 6.4 s, and starting the core pulled pg, kine and the slot
  ahead of it.

- **Host processes are HostProcess objects in the application's own kcp (plan 0006
  M2-HOSTPROCESS-KIND, 2026-10-12).** Kind `HostProcess` (socialweb.computer/v1alpha1, export
  `hostprocesses` in `root:socialweb-provider`, bound in `root` by the `hostprocesses` set; the bidder
  WorkspaceType binds the same export). `status.runtime.backend` is a plain string, so the schema names
  no backend. `internal/hostprocess` holds a pure reconciler (`Reconcile(Observed) Result`) and a
  supervisor that runs inside the core (today the dev core; `serve gateway kcp` and `run bidder` start
  it when they are built). Rules: adopt by unit name and InvocationID, never re-create; a manager that
  does not answer is Unknown, never Lost; an object placed by another backend is
  Lost(IdentityMismatch) and is never adopted; an identity mismatch on an active unit stops the unit
  at once; a supervisor places only units under its own prefix `socialweb-hp-<key>-`, and stops a unit
  of that prefix whose object is gone. `spec.placement: bootstrap` objects are read-only mirrors of the
  bootstrap layer (`bootstrap-<role>`): the supervisor writes their status and never starts or stops
  their unit, and a deleted mirror is written again on the next pass. Readings on hat-8: on a quick dev
  instance `kubectl get hostprocesses` listed the five mirrors with pid, invocation and listener; a
  stand-in object was placed on proc and kept its InvocationID across a kill -9 of the core; deleting
  the front mirror left the front running; deleting the stand-in stopped its unit. On systemd-user
  (test prefix, a separate workspace) a second supervisor process adopted the stand-in with the same
  InvocationID and LimitsEnforced=True, and the delete stopped the unit.

- **An application's OpenBao is an OpenBaoServer object placed through a HostProcess (plan 0006
  M2-OPENBAO-SERVER, 2026-10-12).** Kind `OpenBaoServer` (socialweb.computer/v1alpha1, in the
  `hostprocesses` export). `internal/openbaoctl` runs in the core beside the HostProcess supervisor. It
  owns one HostProcess child `bao-<name>` (unit `socialweb-hp-<key>-bao-<name>.service`, restartPolicy
  OnFailure), renders the server config with `internal/rootvault` (mutual TLS from `internal/vaultcert`,
  listener `127.0.0.1:0`), and reads the bound address from the child's `status.listeners` into
  `status.address`. Key handling: no key, token or certificate is ever in a kcp object; `status.refs`
  names files only. The unseal keys live in `<state>/openbaoservers/<name>/openbao/unseal-keys.json`
  (0600, in a 0700 directory, written once with fsync before the first unseal, read only when no other
  user can read it). The init root token is used once to apply the layout (cert auth at `cert`, role and
  policy `socialweb-core` bound to the server's own client CA, kv-v2 at `secret`) and is revoked at once.
  The core then logs in with its client certificate for a 15-minute token. If that login is refused, the
  controller generates a root token from the unseal keys, applies the layout and revokes the token; the
  listener therefore enables the unauthenticated generate-root endpoints, which still need the client
  certificate and the unseal keys. A server whose storage is uninitialized while unseal keys exist is
  Failed, never re-initialized. The HostProcess supervisor adopts a unit that its manager restarted in
  place (restartPolicy OnFailure or Always) at the new invocation; before this it stopped such a unit as
  an identity mismatch. The controller unseals on every pass that finds the server sealed and records
  `status.unsealedInstance`. Readings on a quick dev instance (proc backend): created 07:17:49Z, Ready
  07:17:59Z on 127.0.0.1:46009; no unseal key or token-like string in any OpenBaoServer or HostProcess
  object; kill -9 of the bao main pid at 07:18:14.555Z, unsealed at the new invocation on 127.0.0.1:35737
  at 07:18:18.889Z, and a KV value written before the kill read back; a dropped cert role was restored
  through generate-root; `dev restart core` kept the bao invocation and did not unseal again; deleting
  the object stopped the unit and kept the storage.

- **Every OpenBao holds exactly its declaration (plan 0006 M3-BAO-SYNC, 2026-10-12).** `internal/baosync`
  reads a server (auth and secret mounts, the ACL policy list, the role collections of every declared
  auth and pki mount, every declared role and config, the owned KV listings), diffs it against the
  declaration and applies: missing added, drift corrected (rules and role fields are compared after
  normalising durations, lists and booleans), undeclared removed. Only the built-ins (`token/`, `sys/`,
  `identity/`, `cubbyhole/`, `root`, `default`) are never touched; a secret mount whose type differs is
  reported as a conflict, never replaced. An unread listing aborts the pass. The declaration is
  `spec.declared` of the OpenBaoServer plus the controller's own layout and `spec.clients`, so every
  pass of `internal/openbaoctl` syncs every Ready server. The core's `socialweb-core` policy is no longer
  `path "*"`: it covers only sys/auth, sys/mounts, sys/policies/acl, the role, cert and config paths of
  auth mounts, pki roles and KV metadata list/delete; it cannot read a KV value or issue a
  certificate. When the core's own login cannot read (its policy or role was removed) the pass falls
  back to a root token generated from the unseal keys and revoked after the pass. A change of the
  rendered config, the server certificate or a CA file changes the child's `spec.restartKey`; the
  HostProcess supervisor then stops and starts the unit in place and the next pass unseals it. Each
  tenant cluster's OpenBao is such a server (section 5): its declaration is the cluster layout of
  `internal/vaultlayout`, the ESO cert role (pending until the cluster's client CA is in `pki`), the
  pki roles, and per live node a policy `node-<n>`, a jwt mount `jwt-<sha12(issuer)>` with its role and
  a key in the `socialweb-nodes` KV scope. `NodeVault` edits that declaration in kcp and waits for the
  sync; the cluster provider's own token can only write the paths it writes (k3s KV, node and link KV,
  pki/config/ca, two pki issue roles). The root token of the cluster vault is no longer kept anywhere.
  The gateway's root vault followed with M3-SERVE-GATEWAY-KCP: it is the OpenBaoServer `root-vault`,
  `spec.declared` is vaultlayout's root layout, and apply-root is only the pki root generation and the
  KV writes, done as its client `gateway`. Readings on a quick dev instance (proc backend), UTC: a standalone
  server, hand drift at 08:26:30.583Z (policy `intruder` added, approle role `batch` deleted, auth
  `userpass` enabled) undone at 08:26:30.823Z; `socialweb-core` deleted by hand at 08:27:14.637Z,
  restored through generate-root at 08:27:16.547Z; `spec.listen` changed at 08:26:48.618Z, restarted in
  place and unsealed at 08:27:00.811Z. A tenant cluster (serve provider against the dev instance, all
  members Ready, ESO synced its ExternalSecret from the vault): drift at 08:35:57.014Z (policy added,
  approle role `cluster-node` deleted, auth `userpass` enabled) undone at 08:35:59.467Z; an undeclared
  `socialweb-nodes/ghost` written at 08:36:31.729Z deleted at 08:36:34.445Z, and the clusterprovider
  token got 403 reading it back and listing policies; kill -9 of the cluster bao at 08:36:10.230Z,
  unsealed at 08:36:14.421Z, the cluster stayed Ready.

- **`units roll kcp` rolls the installed kcp units (plan 0006 M3-KCP-ROLL, 2026-10-12).** One procedure,
  `kcpserver.Roll`, serves `dev roll kcp` (proc, specs started by devsup) and `units roll kcp`
  (systemd-user, the installed units of the bootstrap layer started by name). The installed placement
  (`bootstrap.RollInstalled`) starts `<prefix>kcp-<key>-kine@<new>` and waits for its socket, writes the
  new slot's env file with the front URL from kcp-slots.json, retargets `kcp/bin/kcp` and `kcp/bin/kine`
  to the target assets just before the new slot starts (and back if the roll aborts before the flip),
  starts `<prefix>kcp-<key>@<new>`, and after the probe stops the old kine and moves boot enablement to
  the new slot and kine. Version rule: the same asset or the same kcp release is blue/green; two
  releases are blue/green only when `versions.json` `kcpCompat` records the pair storage-compatible
  (`scripts/kcp-compat.sh <from> <to>` on the build host: CRD served/storage versions, the spec sha256 of
  every APIResourceSchema and the schemas of every APIExport in config/crds, system-crds, root-phase0,
  root and shard); any other pair is refused (exit 2) and takes `--planned-outage` (pg_dump, stop the
  old slot, start the new one, flip; an abort restarts the old slot on the old assets). A single-slot
  kcp (a bidder handle, `--handle`) always rolls as that short restart of slot a, after a copy of its
  sqlite store, with no lease step (a single slot runs no leader election). Records: v0.33.0 -> v0.33.1
  compatible (empty diff), v0.32.5 -> v0.33.0 not (the cache and migration exports were renamed).
  Readings on hat-8, a gateway-shaped instance placed as installed user units under the test prefix
  `socialweb-kcproll-` in the account's user manager, probed through the front every 100 ms with a new
  connection per request: same-asset roll a -> b 07:55:02.41Z-07:55:14.74Z (ready 5.84 s, old exit
  3.14 s, lease 3.03 s, probe 0.31 s), 1418 requests, 0 failures, 0 refused; v0.33.0 -> v0.33.1 b -> a
  07:56:28.89Z-07:56:41.28Z (ready 6.24 s, lease 2.42 s), 1144 requests, 0 failures, 0 refused, the new
  slot's /proc/<pid>/exe the v0.33.1 asset; a stub reporting v0.32.5 refused with nothing changed;
  `--planned-outage` back to v0.33.0 a -> b 07:57:40.03Z-07:57:50.96Z, kcp down 9.16 s (500/504
  through the front, 0 refused); same asset b -> a 07:59:16.20Z-07:59:26.79Z, lease 0.83 s, 860
  requests, 0 failures, 0 refused; a bidder-shaped instance restarted to v0.33.1 with kcp down 10.40 s.
  Not covered here: a crashed slot outside a roll is still seen by the dev core's 5 s unit check (the
  6.3 s outage of M2-KCP-SLOTS); the roll waits on the stop job of the old slot and never polls unit state.
- **One firecracker path, and no host binary beside `socialweb-computer` (plan 0006 M3-FOLD,
  2026-10-12).** `socialweb-nodeboot` (the resident VM wrapper), `socialweb-nodeimage` and
  `socialweb-nodeinit` are deleted. The bidder's firecracker provider runs every guest as a VM unit of
  `internal/vmruntime` through `--compute-provider-firecracker-vmruntime-command` (required); the docker
  runner container, the node image exec and their options, the guest range allocator and the
  `microvm-firecracker` and `node-image-firecracker` packages are gone, and the microvm interface lives
  in `microvm-abc`. The guest image's two Go build steps are `socialweb-computer nodeimage
  guest-rootfs|guest-manifest`, compiled only without the release tag. What check-host's build report
  still reads of the old node image store (`internal/nodefirecracker`, `internal/kernelmodules`) stays
  until 0006 M5-RETIRE. Reading: a market network on hat-8 (bidder VM units in test-0001's user
  manager, linger on for the run only) rented a firecracker guest through the RFP flow 08:13:40.94Z,
  onNetwork after 19.5 s, released 08:14:01.28Z (vm.delete 200), no VM unit, runtime work dir or
  guest work dir left 4 s after release; teardown clean at 08:14:12Z.

- **Each application is one root object in its own kcp; a market network is a MarketNetwork that
  composes child applications, each with its own kcp (plan 0006 M2-APP-CONTROLLER, 2026-10-12).**
  Kinds `MarketNetwork`, `MarketBidder`, `MarketRequester` and `Gateway` (socialweb.computer/v1alpha1,
  export `applications`, bound in `root` by the `applications` set every core applies). The network
  value `MarketNetworkValue` mirrors the TypeScript `MarketNetwork` of `market-network-common` field for
  field and is generated into the socialweb-computer-k8s API package. `internal/appctl` runs in every
  dev core. A `MarketBidder` renders into its own OpenBaoServer `bidder` (root) and, per profile, the
  bidder DenoPod in `root:app` from `deploy/apps/market-bidder/<profile>.values.yaml` through the service
  template; a phase named `<component>@<profile>` belongs to one profile and a DenoPod the profile omits
  is deleted. The bidder's core keeps the PDS account password in that vault, signs up on the PDS its
  root object names, and gives the DenoPod a token that can read only OpenBaoServer objects of its own
  kcp. `MarketRequester` is an accepted root object with no components yet (0008 M4-RUN-REQUEST adds
  them); a `Gateway` renders its root vault (0006 M3-SERVE-GATEWAY-KCP, below). A `MarketNetwork` runs
  its OpenBao as a HostProcess (in-memory storage, one unseal key in a mode 600 file; the core's deno
  runtime waits for it), the `market-network` set's services as DenoPods, and one child per
  `spec.apps[]`: the child's bootstrap layer (quick shape: kine, one kcp slot on port 0, the front, its
  core) is HostProcess objects in `root:dev:<child>` of the network's kcp, placed by the network's
  supervisor, and the child's core holds a ServiceAccount token valid only in that workspace. The
  network applies the child's root object with the network value copied onto it and reads back only the
  child's root status; that copy is the only discovery between applications. The value names loopback
  addresses read back from the services' status (`https://127.0.0.1:<port>`, `relay.localhost:<port>`)
  and a CA bundle (the network PKI root and the guest TLS CA), because a child's DNS shim resolves only
  its own application's names. For the same reason the bidder reads its own records through its own
  repo session rather than through the PDS name in its DID document. Every hono-bidder and
  request-vm-ssh option is mapped in `deploy/apps/option-map.yaml` to a root-object field, the network
  value, a profile, the state dir, a listener report, the `options` passthrough or a legacy-transport
  removal; `spec.options` accepts only options mapped to the passthrough. `dev up --profile
  market-network [--bidders N] [--with-gateway] --app-values <file>` brings it up; `dev down` deletes the
  MarketNetwork first (children, then services, then the OpenBao). Not built here: a QR DenoPod in the
  network (the network value's `qrOrigin` is empty; pdsAccount bidders need none), MarketIdentity
  objects as children, a bidder-owned PDS (bidders sign in to the network's bob PDS). Reading on hat-8
  (`accept.sh`, 2 bidders and a gateway, guests as VM units of test-0001's user manager): `dev up`
  09:04:30Z to Ready 09:06:01Z (91 s, the network, its services and three child kcps); one-bidder bring-up
  85 s; network RSS steady 4.45 GB with three children; the requester rented a guest through the RFP
  flow 09:06:17Z, 2 bids, onNetwork after 19.7 s, ssh host key verified, vm.delete 200 at 09:06:37Z;
  `dev down` 09:06:40Z to 09:07:00Z; teardown clean (no process, VM unit, port or work dir left).

- **`socialweb-computer serve gateway kcp` is the gateway application instance; it replaces
  container-entrypoint.sh (plan 0006 M3-SERVE-GATEWAY-KCP, 2026-10-12).** `internal/stack/gatewaykcp`
  runs the core of `internal/devcore` for the application `table.Gateway()` (its unit prefix may be a
  test prefix, `--unit-prefix`) on either backend. Order: lock `<state>/kcp/core.lock`; the kcp store,
  the active slot's kine, the front and the active slot, each adopted by its recorded InvocationID or
  started (a loaded installed unit is started by name); the provider, permission-inventory,
  hostprocesses and applications sets through the front; the `Gateway gateway` root object (profile
  prod or market-network); then the supervisors. The app controller renders the Gateway into the
  OpenBaoServer `root-vault` (the root layout of `internal/vaultlayout` as `spec.declared`, client
  `gateway` with policy `root-writer`); the core waits for it Ready and synced, generates the pki root
  and writes the root KV entries as that client (`vault apply-root`). No root token and no unseal file
  of the root vault exist; `vault prepare-root` is gone. Then, in process: the migration, the gateway
  (XRPC on 127.0.0.1:0), a Go crypto/tls front that proxies to it (replaces socat; a self-signed
  certificate under `<state>/tls` unless `--tls-cert-file`), the clusterprovider (its HostProcess
  objects under the core's prefix), and the socialweb-computer-k8s web app as a deno child with explicit
  permissions (port 0 read from its port file). Each listener is reported in the Gateway's
  `status.listeners`. The members reach kcp as kcp-admin through a kubeconfig rewritten from the shared
  material: a workspace created as shard-admin records no creator and the account WorkspaceType's
  apibinding initializer never finishes. The gateway resolves DIDs through `SOCIALWEB_GATEWAY_PLC_URL`
  (a loopback PLC through a plain dialer). Any in-process member ending ends the core with exit 1; the
  deno child dies with the core's unit; every other unit is adopted on the next start. Readings on
  hat-8, systemd-user, test prefix `socialweb-sgw-` in the account's user manager, a local PLC and PDS,
  UTC: empty state dir 09:30:30Z to kcp Ready 09:30:50Z, sets and root object 09:30:57Z, root vault
  initialized 09:31:17Z, root layout applied and the door listening 09:31:18Z (48 s); a graceful restart
  adopted every unit and served again in 1.3 s; a tenant cluster created through the web app's
  `/api/clusters/create` at 09:33:58Z was Ready at 09:35:58Z as member units (Postgres, kine, k3s slot a,
  apiserver front, OpenBao, CCM, ESO, two links) behind its apiserver front, `kubectl get ns` answered
  through the front and through the web app's kubeconfig (kcp mount, socialweb-credential); kill -9 of
  the core at 09:36:53.561Z, the next core served again at 09:36:57.855Z, all 18 other units kept
  their InvocationIDs and the cluster stayed Ready (no condition transition). Not here: the Gateway
  child of a MarketNetwork still runs a dev core (its root vault renders, its members do not), the
  door's TEST_* seams are still env variables, and the door moves into a DenoPod with 0008
  M3-DOOR-DENOPOD.

- **The socialweb-computer-k8s web app is a DenoPod of the Gateway application (plan 0008
  M3-DOOR-DENOPOD, 2026-10-12).** `serve gateway kcp` starts no deno child. Its core runs the deno
  runtime (`internal/stack/denoruntime`), and the app controller renders the `Gateway` into the set
  `gateway` (deploy/sets.yaml): the `root:app` workspace, its rbac, and the DenoPod
  `socialweb-computer-k8s` from `deploy/denopods/service.yaml` with
  `deploy/apps/gateway/<profile>.values.yaml`. The web app binds port 0; its listener is in the DenoPod
  status and in the Gateway's `status.listeners` (component `socialweb-computer-k8s`). Its permissions are
  explicit: net, env, sys, read of the member kubeconfig (and, in the market-network profile, of the
  test seam files), write of its state dir only, and `--allow-run=ssh-keygen` with its reason. With no
  `gateway-url` the web app reads the cluster gateway's XRPC listener from the Gateway object's status
  through kcp, so a core restart (a new XRPC port) changes no DenoPod spec. The Gateway's configuration
  is its spec: `--gateway-spec-file` names network, bidderDids, the web app's options (mapped in
  `deploy/apps/option-map.yaml`, app `gateway`) and `marketNetwork` (ownerSessionsFile,
  nodeGuestHostAliases, nodeUserDataBaseFile), the former TEST_* env seams; `marketNetwork` is refused
  under profile prod. `door-address` is gone. Readings on hat-8, systemd-user, test prefix
  `socialweb-wa-`, a local PLC and PDS, market-network profile, UTC: an empty state dir at 10:00:21.8Z
  to the Gateway Ready with the web app's listener at 10:01:19Z (57 s); a session deposited and the
  tenant cluster `wa1` created through `/api/clusters/create` at 10:01:28Z, Ready at 10:03:07Z; kill -9
  of the core at 10:03:10.566Z, the next core Ready at 10:03:13.8Z, and exactly one web app deno process
  afterwards (the old one went with the core unit's cgroup; the DenoPod started one new run), which
  listed `wa1` Ready.

- **The web app's sessions and requester key live in the Gateway's root vault; each account is a
  MarketIdentity; the Gateway's web app front holds the edge port (plan 0008 M3-DOOR-STATE-VAULT,
  2026-10-12).** The OpenBaoServer `root-vault` has a second client, `socialweb-computer-k8s`, with its own
  kvMount `socialweb-computer-k8s` and a policy on that mount only (a token of this client is refused on
  `secret/`). The app controller renders the web app only once that client's certificate is in the
  server's status, and grants it read of the CA and of its client directory. The web app finds the
  OpenBaoServer through kcp (`--openbao-server root-vault`), logs in with its client certificate and keeps
  each OAuth session at `<mount>/oauth-sessions/<did>` and its requester key at `<mount>/requester/key`,
  behind the `SessionStore` port (`socialweb-computer-abc`; sibling stores
  `socialweb-computer-oauth-session-fs` and `socialweb-computer-oauth-session-vault`) and a key port
  (`key-file-fs`, `key-vault`). On each stored session it writes one MarketIdentity (mode
  `oauthDelegated`, spec and status carry only the sessionRef) in `root:app`, where the gateway set binds
  the `marketidentities` export; a removed session sets the phase SessionExpired. Its state dir holds no
  secret. Edge port: the web app DenoPod binds port 0, and the Gateway's web app front (an L4 member of
  the core, `internal/tlsfront` without TLS) listens on `spec.webAppFront`, 127.0.0.1:8788 in profile prod
  (the address caddy reverse-proxies to, beside caddy 443 and the kcp front) and port 0 otherwise, and
  forwards to the address the DenoPod reports; a web app restart on a new port is invisible to caddy.
  Readings on hat-8, test prefix `socialweb-wa-`, market-network profile, UTC: fresh state 10:14:31Z to
  Ready 10:15:26Z; a session deposited through the front at 10:15:29Z was in the vault (version 1) with
  MarketIdentity `account-c6f141b2c8a0fbd5016d` Ready, and no session or key file existed under the state
  dir; `systemctl --user restart` of the core at 10:15:56.8Z, and the same signed-in session listed the
  account's clusters through the new web app process at 10:16:03.9Z; the cluster `wa2` it had created
  was Ready at 10:17:09Z.

- **The web app reaches kcp as its own ServiceAccount with a short-lived token, never as kcp-admin
  (lead decision, 2026-10-12).** The web app faces the internet behind caddy; it used to read the
  members' kubeconfig, which is kcp-admin, so one compromise of it controlled every workspace. Now the
  gateway set applies the ServiceAccount `socialweb-computer-k8s` in `root:app` with create and patch on
  MarketIdentities and patch on their status, and the web app DenoPod runs as that account. Seen from
  another workspace the account is `system:kcp:serviceaccount:<root:app cluster>:default:socialweb-computer-k8s`
  (kcp GlobalServiceAccount); the app controller binds that user to get on its Gateway and on the
  OpenBaoServer `root-vault` in `root`. NodeClaims are granted per home: with
  `SOCIALWEB_GATEWAY_NODECLAIM_AGENT` set (`serve gateway kcp` sets
  `root:app/default/socialweb-computer-k8s`), the cluster gateway, when it creates a home, applies there a
  grant to that user of get, list and patch on NodeClaims, patch on their status and get on ClusterLinks,
  and names the home in a ConfigMap `<account>-<cluster>` labelled `socialweb.computer/nodeclaim-home` in
  `root:app` namespace `default`, which the web app may list (`--kcp-workspace`). The web app finds the
  homes there and reads each home's NodeClaims at `/clusters/<home>`; it lists no root or account
  workspace and reads no Cluster or Node. Two narrower routes do not work in kcp 0.33: a grant on the
  `clusters` export's virtual workspace (`apiexports/content`) cannot be scoped by resource, so it gave
  every tenant's Clusters and Nodes too, and a foreign ServiceAccount cannot list workspaces in an
  account workspace, because the tenancy.kcp.io export's maximal permission policy refuses a token
  scoped to another logical cluster. The DenoPod names no ServiceAccount (the service template now
  writes `spec.serviceAccount` only for a service that names one), so its process carries no
  `KCP_TOKEN` or `KCP_TOKENS`. Account and cluster workspaces stay with the cluster gateway XRPC, which
  runs in the core with admin. The app controller issues the account a 30 minute token into
  `components/socialweb-computer-k8s.kcp/token` and a new one 10 minutes later; the web app's kubeconfig
  names it as `tokenFile` and the web app reads it on every request. The DenoPod may read that directory,
  the vault CA and its vault client directory, and nothing of the members' kubeconfig. listClusters for
  an account with no workspace yet answers `[]` instead of a 500. First readings (export grant) on hat-8, test prefix
  `socialweb-was-`, market-network profile, dev build, UTC: core started 10:34:47Z, Gateway Ready
  10:35:32Z, token issued 10:35:25Z; a fresh account listed `[]` at 10:35:52Z; a signed-in account
  created `demo` at 10:36:01Z and listed it at 10:36:02Z (listed Ready at 10:45:33Z). With the web app's token
  at 10:36:24Z: 200 on its Gateway, the root vault and NodeClaims and ClusterLinks through the export,
  404 on a dry-run status patch of a missing NodeClaim through the export; 403 on listing root
  workspaces, listing OpenBaoServers or secrets in `root`, DenoPods and a MarketIdentity list in
  `root:app`, APIExports in `root:socialweb-provider`, AtprotoIdentities and workspaces in the account
  workspace, Clusters and secrets in the home addressed directly, and a NodeClaim delete through the
  export. The token was re-issued at 10:45:25Z (expiry 11:15:25Z), and at 10:45:33Z the web app wrote a
  MarketIdentity and listed both accounts' clusters with it. Readings of the per-home grant, test prefix
  `socialweb-wb-`, UTC: core started 11:11:00Z, Gateway Ready 11:11:52Z; a fresh account listed `[]` at
  11:11:56Z; two accounts created `demo` and `cbox` at 11:12:03Z and 11:12:04Z and each listed only its
  own, `demo` listed Ready at 11:13:50Z; the web app process had no `KCP_TOKEN` or `KCP_TOKENS`. With
  the web app's token at 11:13:54Z: 200 on its Gateway, the root vault, the home index (2 ConfigMaps)
  and NodeClaims in both homes, 404 on a dry-run status patch of a missing NodeClaim and on a missing
  ClusterLink in both homes; 403 on Clusters (list, get, dry-run patch) and Nodes in both tenants'
  homes, on a NodeClaim delete and a ClusterLink list there, on Clusters, Nodes and NodeClaims through
  the export, on workspaces, AtprotoIdentities and secrets in both account workspaces, on
  workspaces, OpenBaoServers and secrets listed in `root`, on MarketIdentities, DenoPods and secrets in
  `root:app`, and on APIExports in
  `root:socialweb-provider`.

- **`socialweb-computer run request`: a fresh kcp per invocation whose state dir persists; the
  requester is a MarketRequester whose components carry out one ComputeContract (plan 0008
  M4-RUN-REQUEST, 2026-10-12).** Kind `ComputeContract` (socialweb.computer/v1alpha1, export
  `computecontracts`): spec {vmName, bidder allow and deny lists, ssh authorized keys, userData, policy
  and its args, vmReadyTimeoutSec, secretsRef, release}, status {phase Pending, Requesting, Held,
  Releasing, Released or Failed, message, secretsUrl, secretsFetchedAt, contract}; `status.contract` is
  the ContractState shape `NodeClaim.status.contract` carries. A `MarketRequester` renders (set
  `market-requester`) the ComputeContract and MarketIdentity bindings in `root`, its own OpenBaoServer
  `requester` (file storage in the invocation's state dir), a ServiceAccount that reads and writes only
  its MarketIdentity, ComputeContract and OpenBaoServer objects, the MarketIdentity `requester` from
  `spec.identity` (mode default localRepo, the ephemeral-PDS mode; association RequireApproval unless
  `--skip-qr`), and the requester DenoPod from `deploy/apps/market-requester/<profile>.values.yaml`
  (profiles `market-network` and `default`). The DenoPod is `request-vm-ssh`, which refuses to start
  outside a DenoPod: it signs in through its MarketIdentity, runs `requestCompute` for the
  ComputeContract (resuming from `status.contract`), writes the status at each step, holds the contract
  until `spec.release` and then sends `vm.delete`. Its repo key, sign-in session, the secrets bundle and
  each contract's secrets-server key live only in its OpenBao, so a restarted kcp re-serves the same
  secrets URL. The CLI never runs a requester itself: it starts the invocation's kcp, writes `--secrets`
  into that OpenBao, creates the ComputeContract, renders the MarketIdentity's QR, URL and association
  prompts (`internal/appcli`), waits for Held and the guest's secrets fetch, and runs ssh over the iroh
  ticket (`dumbpipe connect`) with StrictHostKeyChecking=yes against the host keys the guest reported.
  Without `--hold` it then sets `spec.release`, waits for Released and removes the invocation; with
  `--hold` it stops the kcp and keeps its state dir. `run request release|resume <name>` start that same
  kcp again from the state dir; `run request list` reads each `summary.json` and starts nothing. Every
  request-vm-ssh option is mapped in `deploy/apps/option-map.yaml` (new targets `contract.*` and `cli`)
  and passed after `--`. `--network <market network>` makes the invocation a child application of that
  MarketNetwork (`spec.apps[]`, kind MarketRequester, profile `market-network`, identity localRepo with
  association Skip, the network's bidder DIDs copied on); `spec.apps[].stopped` stops a child's kcp,
  kine and core and keeps its state dir. Two things a kcp restarted from a kept state dir needs: the root
  Shard object is pointed at the front the slot runs behind now (kcp keeps the first `--shard-base-url`
  it was started with, and every APIExport virtual workspace URL follows it), and a child keeps its front
  port across a stop. A Lost OpenBaoServer HostProcess is placed again on its storage, and stopping or
  tearing down an instance stops the processes its units left behind (the deno service processes outlive
  their core). The network value gained `hosts` (each network service name and the loopback address it
  answers on): a child's shim never resolves the network's names, and the PDS name in a bidder's DID
  document is one, so the one fetch adapter maps them. Reading on hat-8 (market network of one bidder,
  guests as VM units of test-0001): network up 10:25:12Z to 10:26:36Z; `run request --network rrnet
  --secrets` 10:26:36Z, the invocation's kcp cold in 18.1 s, RFP 10:27:17Z, receipt 10:27:18Z, host keys
  10:27:34Z, the guest fetched its secrets 10:27:35Z, ssh read the secret file 10:27:39Z, kcp stopped with
  its state kept 10:27:46Z; `release rr1` 10:27:50Z, kcp warm again in 13.1 s, vm.delete 200 10:28:19Z,
  no VM unit or work dir left; a second invocation held 10:28:40Z to 10:29:42Z, `list` with no kcp
  running, `resume rr2` 10:29:46Z (kcp again in 13 s, ssh to the same guest), vm.delete 200 10:30:15Z;
  `dev down` 10:30:35Z to 10:30:54Z with no process left.

- **The bidder's OpenBao holds exactly what its live ComputeVMs ask for (plan 0007 M4-BIDDER-BAO-SYNC,
  2026-10-12).** Each guest's material lives under `guests/<computevm>/` in the bidder client's kv mount:
  `user-data` (the whole cloud-init user_data, which carries the provisioning token) and `provisioning`
  (the OIDC nonce, the guest-reported ed25519 host key, `consumedAt` once `/v1/oidc/prove` used it). The
  ComputeVM names its user data as `spec.userData.vault {server, mount, path}`; no kcp Secret holds it,
  and the ComputeVM controller reads it as vault client `computevm` (policy on
  `bidder/data/guests/+/user-data`, read only). Every controller pass writes the names of the live
  ComputeVMs of its handle into the OpenBaoServer's `spec.declared.kv` scope `bidder/guests` (grace 2m);
  the openbaoctl sync removes an undeclared entry with everything under it once it is older than the
  grace, so a guest's material written before its ComputeVM is never lost to a race. The issuer's nonce
  store and reported host keys (`NonceStore`, now asynchronous) are that vault entry, with an in-memory
  nonce index rebuilt by listing on a miss. The bidder's kcp token is ServiceAccount `bidder`
  (OpenBaoServers get/list, ComputeVMs and their status), minted by appctl for 1h and renewed every 20m,
  used for both the vault resolver and compute-provider-kcp; the core no longer writes the shard-admin
  token. `MarketBidder.spec.signingKeyRotation` raised adds a new RSA signing key in the vault and keeps
  the old one published (`previous[].publishUntil` = rotation + 25h); the issuer reloads its key set every
  30 s (or at once for an unknown kid), signs with the newest, verifies by kid and publishes all keys in
  its JWKS, so the issuer URL and every running guest's tokens survive. Relay-keypair rotation (which
  moves the issuer URL) has no command. In a market network the issuer's RBAC lookup of the bidder's own
  records reaches the PDS its DID document names through the network value's host table. Reading on hat-8, a market network with a spec.kcp bidder (guests
  as VM units of test-0001): before the rent the vault listed no guest; onNetwork 13:28:53.6Z, the guest's
  `provisioning` and `user-data` entries listed at 13:28:54.4Z; the guest's token exchange at
  /v1/oidc/issue answered 200 at 13:29:05Z and 13:29:20Z (key slsC6N); rotation 1 written at 13:29:23Z,
  the issuer loaded key dxnyMU at 13:29:35Z with slsC6N still published, and the guest's pre-rotation
  token (kid slsC6N) was exchanged with 200 for tokens signed by dxnyMU at 13:29:35Z, 13:29:50Z and
  13:30:05Z; vm.delete 200 at 13:30:09.8Z, ComputeVM gone 13:30:13.9Z, the guest's vault entries gone
  13:30:59.4Z (the grace counted from the guest's last write at boot); teardown clean.

- **A restarted kcp bidder rebuilds its contracts from its ComputeVMs (plan 0007 M4-CONTRACT-RESTORE,
  2026-10-12).** The contract lives on the ComputeVM: `spec.contract` (receipt, accept, acceptedAt,
  submitEvent) under field manager `compute-provider-kcp-contract`, written by
  `market-bidder-contract-store-kcp` once the guest's ComputeVM exists (compute-provider-kcp reports the
  ComputeVM it applied for an accept) and before the receipt is acknowledged, and again at `provisioned`.
  `load()` returns one contract per owned ComputeVM that carries one (acceptAuthor = `spec.requester.did`,
  providerId = its name) and deletes an owned ComputeVM that no contract names (the bidder stopped between
  creating it and recording its receipt). Decision (the conflict with M4-TERMINATION-EVENT): the tombstone
  stays in the bidder's contract store files (`<state>/contracts/tombstones`, TTL 30 days) and the
  ComputeVM goes; compute-provider-kcp no longer deletes a Lost or Failed ComputeVM that carries a
  contract, the store deletes it on the `terminated` event, which market-bidder sends only after the
  tombstone is saved, so a crash between a guest's end and its tombstone leaves the ComputeVM for the next
  start to tombstone. Keeping a terminated ComputeVM until acknowledged was rejected: requesters skip
  vm.delete after vm.terminated, so it would hold the guest's work dir until a TTL. Reading on hat-8 (same
  market network): a guest rented, onNetwork 13:31:17.6Z; kill -9 of the bidder's core and its bidder
  process 13:31:17.8Z; the restarted bidder loaded 1 contract from its ComputeVMs and adopted the running
  guest at 13:31:18.9Z; vm.delete 200 at 13:31:26.7Z, ComputeVM gone 13:31:30.8Z. A second guest powered
  itself off, vm.terminated (vmm_exited) at 13:32:41.9Z, its ComputeVM deleted after the tombstone was
  written (gone 13:32:44.9Z); kill -9 of core and bidder 13:32:45.2Z; the restarted bidder loaded 0
  contracts and 3 tombstones at 13:32:46.2Z; the late vm.delete at 13:32:54.3Z answered 200 (already
  terminated). Seen and not fixed here: a SIGKILLed core leaves its bidder DenoPod process running while
  the new core starts another, and the old one's close drops the new one's dispatcher registration (a
  vm.delete then gets 404); the reading kills both.

- **The market network is driven by `socialweb-computer` alone; accept.sh is gone (plan 0008
  M4-MARKET-NETWORK-PORT, 2026-10-12).** `deploy/market-network/accept.sh`, the host-side
  `market-requester` and `internal/marketnetwork` (the network record and the slot lock) are deleted.
  The requester flow is `run request --network`. What accept.sh checked is what the applications
  report: a MarketNetwork carries conditions `ServicesReady` (every service DenoPod Running, ready,
  listening and carrying the `denopod.deno.computer/run` finalizer), `VerifierPassed` and `OwnKcp` (the
  network and every child on their own kcp front and kcp root) and is Ready only when all three hold;
  `status.apps[]` carries each child's components, and a MarketBidder on `spec.kcp` lists its ComputeVMs
  and their HostProcesses; a guest that ends on its own shows in the ComputeContract's
  `status.contract.termination`. `dev status` prints the MarketNetwork; `dev down` stops and reports any
  VM unit a bidder child left. The network's OpenBao is the OpenBaoServer `network` (first phase of the
  `market-network` set: mutual TLS, file storage, the `pki` mount declared, the initial root token
  revoked); no root token is kept. The deno runtime logs in as client `denoruntime`, whose token holds
  only the `network-pki` policy (namespaces, the `pki` mounts, the root CA, intermediates, roles and
  leaves, in the root namespace and one namespace level below); `deploy/denokcp/start-openbao.sh` is
  deleted. Reading on hat-8 (`dev up --profile market-network --bidders 2 --with-gateway`, both bidders
  on `spec.kcp`, guests as VM units of test-0001): `dev up` 14:47:27Z to Ready 14:49:09Z with all three
  conditions true; `run request --network mnp --secrets --hold` read the secret inside the guest of
  bidder-2 (2 bids, its ComputeVM and HostProcess listed under bidder-2 while held) 14:50:20Z;
  `release` vm.delete 200 at 14:51:02Z, no ComputeVM, HostProcess or VM unit of either bidder left 10 s
  later; `dev down` 14:51:12Z to 14:51:35Z with no process, VM unit or state left.
- **The gateway re-populates every VM it owns after an upgrade or restart (plan 0007
  M6-GATEWAY-REPOPULATE, 2026-10-12).** A claim is the index of a rented VM; a restart adopts it and
  rents nothing. Joined claims (Joining, Running) are adopted by both halves: the cluster provider holds
  them again (`adoptClaims`, every pass) and the web app watches each one for vm.terminated. In-flight
  claims (Bidding, Provisioning) resume, because each claim's secrets-ingress key now lives in the root
  vault (the web app's client `socialweb-computer-k8s`, KV `socialweb-door/claims/<claim>/secrets-ingress`,
  `socialweb-computer-k8s-claim-keys-vault`) instead of memory. A VM whose claim is gone is found from
  both sides, and only when they agree is it adopted: the cluster provider writes a claim again
  (annotation `socialweb.computer/adopt-joined-node`, the spec rebuilt as at provision) for a tenant Node
  that is Ready, named `<cluster>-node<N>`, has no claim, and still has a role in the cluster vault's
  declaration; the web app moves that claim to Adopting, reads the owner's own repo through its session
  (`findVmContract`: the newest `compute.vm` record whose role is the node name, its RFP, its accept, the
  winner's receipt, then the winner's events for vm.hostKeys and vm.terminated and the owner's for a
  vm.delete) and writes Running with the contract only when a receipt exists, no vm.delete was sent, the
  guest is not terminated and the receipt has a bidder-signed vm.hostKeys record; otherwise the claim is
  Failed with the reason, and the provider's teardown releases it at most once. The cluster vault keeps
  an adopted node's JWT grant while the claim carries none. A receipt with no claim and no joined Node
  (the ownership scan: every 5 minutes, 30 s while one waits, at most 20 record lookups a pass, over the
  owners of every claim this web app has seen and of its market-network owner sessions; an account whose claims
  were all gone before the web app started is not scanned, a stated limit) waits a grace of 10 minutes from when the web app started and is then
  released once (reason `unclaimed_after_join_grace`); the web app cannot see the provider's
  path-up time, so the grace is long rather than rebased. A market-network Gateway's web app takes the
  network value's hosts (`--network-hosts`) and the market-network fetch adapter, as the bidder and the
  requester do, because the network's DID documents name its PDSes by service name on port 443; every vault unit the
  core places carries the core's `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` and `SSL_CERT_FILE`, so a cluster
  vault fetches a bidder's issuer the way the host's own outbound traffic does.

- **socialweb-computer run bidder; hono-bidder runs only as the bidder DenoPod's entry, with no
  subprocess (plan 0007 M5-RUN-BIDDER, 2026-10-12).** `run bidder --profile prod-qr|market-network|dev-local`
  applies a MarketBidder root object into the handle's own kcp. With `--network <market network>`
  (host-backend proc) it adds a MarketBidder child to the network and returns when the network reports it
  Ready with its did; without, on proc it brings up the handle's own instance (its core runs the
  ComputeVM controller for the handle), renders the MarketIdentity sign-in and returns when Ready; on
  systemd-user it is the installed unit's process: it checks it runs as the account install bidder made
  for the handle and runs the bidder's core in the foreground. The bidder signs in only through its
  MarketIdentity `bidder` (oauthQr, pdsAccount or localRepo by profile); identity-prompt-tty is deleted
  and hono-bidder exits 2 naming `socialweb-computer run bidder` without its DenoPod's kcp environment.
  The QR guard is in Go: a core that sees a QR sign-in into another account removes the session from the
  bidder's vault, stops the bidder DenoPod and starts it again with a fresh QR. The bidder's provider is
  compute-provider-kcp; `spec.firecracker` is refused. The bidder DenoPod's deno argv has no -A,
  --allow-run or --allow-ffi: the guest issuer verifies the guest's SSHSIG in TypeScript (WebCrypto
  Ed25519, namespace prove-sshd) instead of ssh-keygen, and gha-lite policies run net-only (user decision
  2026-10-12: keep gha-lite, forced net-only): every `uses:` action runs in the permission-restricted
  Worker from the bundled-actions dir in the TypeScript tree, the built-in workflows have no `run:` step
  and the verdict is the action's `allow` output carried as a job output, and a policy with a `run:`
  step, a composite action or an action to download is refused (no bid, the reason logged). A core start
  stops the DenoPod processes a previous core of its instance left running before the deno runtime starts
  its own. Reading on hat-8 (market networks of test-0001, `run bidder --network`): the bidder Ready
  45 s after `run bidder`; `run request --network --secrets --hold` read the secret in the guest, the
  guest's host keys were accepted by the TypeScript SSHSIG check (14:49:14Z) and its token exchange at
  /v1/oidc/issue answered (`issue token`, 14:49:15Z, no rbac denial); kill -9 of the bidder's core at
  15:01:43Z, the restarted core reaped the old run and one bidder process ran 24 s later and 20 s after
  that; vm.delete 200 at 15:03:08Z, no VM unit left; `dev down` left no process. With two bidders, the
  only-me bidder's net-only gha-lite scope gate rejected an unassociated requester's RFP at 15:12:05Z, the
  open bidder bid and won, and 643 samples at 100 ms of both bidders' deno processes saw no child process.
- **A killed, restarted or logged-out installed bidder keeps its VM; a user-manager restart is a reboot
  (plan 0007 M6-LIVE-RECOVERY, 2026-10-12).** The installed path (`install bidder`, host-backend
  systemd-user) ran live for the first time and needed: the account reads its own install record
  (world-readable, no secret) instead of swc-bidder's; the handle directories belong to the account; the
  MarketBidder root object is applied once the application controller installed its API; an installed
  account joins a market network only over the atproto market protocol, with `"profile":
  "market-network"` and `"market-network-file"` (the MarketNetwork status, which now names
  `bidderPdsUrl`) in its `config.json`. A user-manager restart re-binds every port-0 listener, so: a
  port-0 front records the address it bound in its state file and binds it again when it starts with
  no inherited socket (the fd store is gone), falling back to port 0 if it is taken; a core start
  re-points every APIExportEndpointSlice at the front and the deno runtime re-reads the endpoints on
  each informer retry; an OpenBaoServer whose process is gone (IdentityMismatch, ManagerRestarted) is
  placed again on its storage. The VM tree audit pins every executable of a runtime set directory named
  by a VM asset's pinned tree digest, so pasta's own pasta.avx2 passes. Reading on hat-8 (stand-in
  account test-0002 with its own subuid range 1703936-1966079, release binary of 24a8a998, market
  network lrnet, each case renting through `run request --network --secrets --hold` and reading the
  secret in the guest): (a) `systemctl --user kill -s KILL` of the bidder's main process at
  16:58:52.6Z, bidder back 9.0 s later; (b) `systemctl --user restart` at 17:01:10.5Z, back 7.5 s
  later; (c) every login session of test-0002 ended with linger on at 17:03:46Z, the same core
  process kept running. In (a)-(c) the ComputeVM stayed Running with the same unit InvocationID, VMM
  pid and start ticks, the tree audit passed before and after, the issuer URLs and the did were the
  same, onNetwork was accepted, vm.delete answered 200 and no VM unit or work dir was left. (d)
  `systemctl restart user@946` at 17:06:24.8Z: the ComputeVM was Lost(UnitGone) 9.8 s later and no VM
  unit started, the front kept 127.0.0.1:36761, the bidder was back 19 s later, ended the contract with
  vm.terminated and released the Lost ComputeVM itself (M4-TERMINATION-EVENT), a new RFP rented a
  replacement (audit passed), and the late vm.delete of the old receipt and the replacement's both
  answered 200 with nothing left.

- **A bidder upgrade and a bidder kcp roll leave its VMs running; the VM stop grace is measured (plan
  0007 M6-BIDDER-UPGRADE, 2026-10-12).** The default VM stop grace is `vmStop.graceSeconds` in
  `internal/versions/versions.json` (100 s); `run bidder`, `dev up` and `internal vm` take it when
  `vm-stop-grace` is unset. The grace is a cap: a stop returns as soon as the unit is inactive. Readings
  on hat-8: an installed bidder (`install bidder` for the stand-in account test-0003) joined its own
  market network with `--market-network-file`; each guest was rented with `run request --network
  --secrets --hold --skip-ssh` and a `--user-data` that installs k3s (node Ready, 8 pods Running) and
  re-runs the guest's onNetwork report every minute. `upgrade --binary` between release builds with
  different closures (b9cd539d to 267f4965 with two guests, 267f4965 to 06925efe with four): the bidder
  DenoPod ran the new closure and was Ready 6 s after the upgrade; every VM unit kept its InvocationID,
  firecracker pid and start ticks; every ComputeVM stayed Running; the issuer URL was the same; `units
  audit` passed; each guest's next onNetwork was accepted within 60 s. `units roll kcp` from kcp v0.33.0
  to v0.33.1 is a short restart of the bidder's one slot (planned outage a to a, kcp down 38.6 s) with the
  same readings. Stop grace, SendCtrlAltDel to unit inactive, over six releases (each vm.delete 200):
  5.46, 5.57 and 5.65 s when every pod's PID 1 exits on SIGTERM; 90.78, 90.85 and 90.88 s when a pod's
  PID 1 ignores SIGTERM, which the guest's systemd ends at its DefaultTimeoutStopSec of 90 s. After each
  release no unit, cgroup, work dir or API socket of the VM was left. Defects found by these runs and
  closed here (the install, root object and pasta.avx2 defects these runs also hit were closed by
  M6-LIVE-RECOVERY): unpacking the closure fsynced every file (61 s on the first start, now one syncfs,
  3 s); the kcp slots record of an installed bidder said blue-green, so `units roll kcp` tried a slot-b
  unit install never writes; a requester's `--user-data` dropped the network's guest CA, so the
  guest's host-keys and onNetwork posts failed; a release of a contract whose guest never reported waited
  `vm-ready-timeout-sec` first. Decided by Claude, user may veto: the k3s in these guests is a one-node
  server whose own agent is the joined node (an agent joined to a tenant cluster through the gateway is
  0007 M6-NOSSH-JOIN); the repeated onNetwork report comes from the requester's user data, not the guest
  image.

- **A market node rented through compute-provider-kcp joins a tenant cluster with no ssh and stays Ready
  through a bidder upgrade (plan 0007 M6-NOSSH-JOIN, 2026-10-12).** Setup: a market network
  (`dev up --profile market-network --bidders 0`), an installed bidder (`install bidder`, stand-in account
  test-0004, `"market-network-file"`, so `upgrade --binary` applies), and `serve gateway kcp --host-backend
  proc --gateway-profile market-network` with the network value in its Gateway spec and a bootstrap
  cluster owned by an account on the network's alice PDS (`marketNetwork.ownerSessionsFile`). The
  network's `--with-gateway` child is not used: its core is a plain dev core, so its Gateway gets a root
  vault and nothing else (no web app, no clusterprovider, no tenant cluster). Defects found and closed:
  the tenant cluster's vault fetched the winning bidder's issuer at `https://<did-key>.relay.localhost`,
  which on the host is port 443 (another listener), so the node's JWT login failed forever; a
  market-network gateway now runs a loopback egress proxy (port recorded and bound again on restart) that
  sends a port-less `*.<guest TLS host>:443` to the network's guest TLS front, and every vault it places
  takes it as `HTTPS_PROXY`. A gateway cold start read the root vault's stale Synced status, failed
  apply-root against the dead address and exited before the core could place the vault again; apply-root
  now retries within the vault timeout. A market-network Gateway hands every node guest the network's CA
  bundle and `CURL_HOME=/root` (`node-guest-ca-file`, `node-guest-curl-home`), as the requester does, so
  no hand-written user_data base is needed. Readings on hat-8 (UTC), second, clean cycle (gateway on the
  fixed build, no user_data base file): a pod applied to the cluster at 0 nodes 18:35:18; claim and RFP
  18:35:53, accept 18:35:54, ComputeVM Running 18:35:57.9, Joining 18:36:09, node Ready 18:36:24, pod
  Running 18:36:32; `upgrade --binary` of the bidder at 18:36:45.8 to a dev build with another closure
  (4ffac85d from c314174f): bidder Ready 4.6 s later, the VM unit kept InvocationID f16c57e2, firecracker
  pid 1005182 and start ticks 7226369, both bidder DenoPods ran again from the new tree, the issuer URL was
  the same, 82 node Ready samples at 1 s all True with the Ready transition time unchanged, `units audit`
  passed; the pod deleted, the scale-in after the 2 min hold sent vm.delete 200 at 18:40:21.6, ComputeVM
  gone 18:40:26.6, no VM unit, work dir, cgroup or firecracker process left. The first cycle (the one that
  found the defects) gave the same upgrade readings (18:30:51, bidder Ready 6.8 s later, 84 samples all
  True) and vm.delete 200 at 18:34:28.2. No ssh from any side: a privileged pod on the node read the
  guest's PID 1 namespaces every 20 s through both cycles: sshd active (the image runs it), 0 accepted
  logins, 0 sessions, 0 established connections on port 22, 0 logins, 0 authorized keys; the web app,
  clusterprovider and bidder logs hold no ssh command or session; the web app has no debug ssh path.
  Not closed here: a gateway's scale pass took 35 s to claim a node for a pending pod.

- **Follow-ups to M6-BIDDER-UPGRADE and M6-NOSSH-JOIN: upgrade restarts an adopted account's bidder, a
  dev build is refused, appctl re-applies a missing component, `dev up --with-gateway` runs a tenant
  cluster, a node claim in about a second (2026-10-12).** `upgrade` looked for the enabled core units
  under `<state-dir>/.config`, which is the home only for an account `install` created; for an adopted
  account (swc-bidder's home is `/var/lib/socialweb-computer-bidder/host`) it switched `bin/current` and
  restarted nothing. It now reads the invoking account's home from passwd and fails when it finds no
  enabled core unit. Reading (stand-in account test-0005 adopted with home `/home/test-0005`, a guest
  held through the RFP flow): the old binary switched `bin/current` and kept the bidder's InvocationID and
  pid; the fixed one restarted it (new InvocationID, the new binary in `/proc/<pid>/exe`, the bidder
  DenoPod started again 1.6 s later), the VM unit kept InvocationID 534d243d, firecracker pid 1099669 and start
  ticks 7346380, and the guest's vm.delete was 200 afterwards. `install` and `upgrade` refuse a dev build
  (exit 2) unless `--allow-dev-binary`. appctl records the DenoPods and HostProcesses each apply placed
  and applies the components again on its normal pass when one is gone: the network's deleted plc DenoPod
  was back 0.6 s later and Ready at 2.6 s, with the same core pid. `dev up --with-gateway` places the
  gateway child as `serve gateway kcp --gateway-profile market-network` runs it (decided by the lead,
  for plan 0010's G2-IT-SCALE-TO-ZERO and G2-IT-FAILOVER): the network writes its Gateway spec and an
  owner account on alice's PDS, and the child brings up the tenant cluster `tenant` at zero nodes.
  Defects found and closed on the way: a HostProcess placed through a parent dropped the unit's env files,
  so the tenant cluster's dumbpipe links ran with a random key; a vault placed by a market-network
  gateway did not trust the network CA (the core now writes the system roots plus the network CA and sets
  `SSL_CERT_FILE` unless it is set); removing a child's state failed on the read-only TypeScript closure,
  so `dev down` waited out its 3 min timeout and left the child's state (now 34 s, nothing left). The
  35 s of M6-NOSSH-JOIN was mostly its proof script: the `kubectl apply` of the pod returned 32 s after
  the reading's start. What the gateway itself waited on were two fixed polls: Provision declared the
  node's policy in the cluster vault and waited for openbaoctl's 5 s ticker, and the web app listed
  NodeClaims every 5 s. openbaoctl now also passes on a watch event that changes an OpenBaoServer's
  generation, and the web app watches NodeClaims in each home workspace. Readings (`dev up --profile
  market-network --bidders 1 --with-gateway`, UTC): dev up 107 s, tenant cluster Ready 112 s later; pod
  applied at zero nodes 19:52:28.9, node policy in the vault 29.39, NodeClaim 29.41 (0.5 s), web app took
  it 30.41, RFP submitted 30.91, bid 31.2, VM Running by 36.7, node Ready 19:53:03, pod Running 19:53:09;
  before the watches the same span pod to NodeClaim read 6.8 s and 4 s. Pod deleted 19:53:10.9, the 2 min
  hold, vm.delete 200 at 19:55:18.6, ComputeVM gone by 19:55:26; `dev down` 34 s, no child state, process
  or VM unit left.

- **The web app offers socialweb-meshd and the credential helpers again, from the binary, and refuses
  to start without them (2026-10-12, found by 0006 M5-RETIRE).** Since prod moved from the container to
  the socialweb-computer binary nothing placed `socialweb-meshd-<arch>` or
  `socialweb-credential-<os>-<arch>` in the web app's downloads dir (the deleted control-plane
  Dockerfile was the only step), so the web app logged `meshd_not_offered` and market nodes had no
  cross-node pod mesh. Now the release builds the asset `socialweb-computer-k8s-downloads`
  (`scripts/build-web-app-downloads.sh`, pinned by tree sha256 in versions.json, the same tree on
  both release platforms), the gateway passes it as `--downloads-dir`, and both gateway profiles pass
  `--required-downloads` (section 9 item 6). Decided by Claude, user may veto: one asset for all six
  downloads; a market network's guests fetch from `http://192.0.2.1:<the web app's bound port>`
  (`test-node-download-host`, pasta's host loopback map), since it has no public origin. Readings
  (`dev up --profile market-network --bidders 1 --with-gateway`, hat-8, UTC, after the rebase on
  a9e8abb3): dev up 21:08:42 to 21:11:14, the web app ready 21:11:11.2 with `meshd.downloadBase
  http://192.0.2.1:41753`, both meshd pins, four credential helpers and no `meshd_not_offered`; tenant
  Ready 21:13:11. Deployments applied at zero nodes 21:13:11.1 (2 anti-affine httpd pods and 8 filler
  pods); two NodeClaims 21:13:12 and 21:13:13 through the RFP flow, both VMs Running by 21:13:24, nodes
  Ready 21:13:48 and 21:13:53, both pods Running by 21:13:58; both Nodes carry
  `socialweb.computer/mesh-endpoint` (pod CIDRs 10.42.0.0/24 and 10.42.1.0/24). Pod 10.42.1.2 on node 2
  fetched pod 10.42.0.4 on node 1 at 21:13:59.2, and a pod pinned to node 1 fetched 10.42.1.2 on node 2
  at 21:14:01. `dev down` 123 s, no process or VM unit left. Found on the way, not fixed here:
  (1) both market nodes report InternalIP 172.30.0.2 (every guest's pasta address), so the apiserver's
  egress-selector tunnel sends `kubectl exec` and `logs` for node 1 to node 2 ("unable to upgrade
  connection: pod does not exist"); (2) the scale target is pods per node only, so a Pending pod held off
  the one node by pod anti-affinity adds no node (the proof added 8 filler pods to reach a target of 2);
  (3) `SOCIALWEB_CLUSTERPROVIDER_SCALE_PODS_PER_NODE` in `dev up`'s environment does not reach the
  `--with-gateway` child's clusterprovider.

- **Each market node is dialled by its node name, the scaler follows unschedulable pods, and the
  scale settings reach the `--with-gateway` child (2026-10-12, the three defects above).**
  (1) Cause: k3s's tunnel server maps every Node InternalIP/ExternalIP to its node name, and the
  apiserver's default `kubelet-preferred-address-types` puts InternalIP first; every guest reports
  172.30.0.2, so the last node written owned that address and exec, logs and port-forward for any
  node went to it. Now a market cluster's apiserver prefers only the Hostname address; the tunnel
  server sends a host name to the agent session of that node name and dials the kubelet on its
  loopback, so no new path into a guest opens and N nodes work. The k3s join module sets the guest
  hostname to the node name before the agent starts (k3s takes the Hostname address from the OS
  hostname), and `nodeprovider.NodeName` refuses a name over 64 bytes. Rejected: a per-node
  `--node-ip` (kubelet needs it on a local interface, and no unique range is known before the Node
  registers; a pod-CIDR address would collide with socialweb-meshd's routes). InternalIP stays
  172.30.0.2 on every node: in-cluster scrapers that dial node IPs (metrics-server) still reach their
  own node. (2) Cause: the target was demand over pods per node only. Now the target is at least the
  largest group of live pods that repel each other by required pod anti-affinity on
  `kubernetes.io/hostname`, and a Pending pod with PodScheduled=False/Unschedulable that the scheduler
  refused on all current nodes ("0/N nodes are available", N = the node count) adds one node, but only
  when no node is joining, every node has been Ready for 5 s and every node holds workload (an
  unschedulable pod beside an empty node would not fit a fresh one either). A want above max-nodes is
  clamped and logged once per change as a warning naming the pods it keeps Pending. (3) Cause: the
  child's env is not `dev up`'s. Now `GatewaySpec.clusterProvider` and `NetworkApp.clusterProvider`
  (scaleHold, scaleMaxNodes, scalePodsPerNode; Gateway schema revision 7, MarketNetwork revision 8)
  carry them; `dev up` fills the gateway app from `SOCIALWEB_CLUSTERPROVIDER_SCALE_*` where the values
  leave a field unset, and the gateway sets the clusterprovider env from the spec. Readings (`dev up
  --profile market-network --bidders 1 --with-gateway`, VM slots of test-0001, env
  `SOCIALWEB_CLUSTERPROVIDER_SCALE_MAX_NODES=3 SCALE_HOLD=30s`, pods per node at its default 8, hat-8
  load about 110, UTC): the child's clusterprovider started with hold 30s, maxNodes 3, podsPerNode 8.
  A 2-replica Deployment with required anti-affinity, no filler pods, applied at zero nodes 22:41:50.5;
  one pass 22:41:56.3 provisioned 2 ("2 pods of default/app=nad-spread repel each other"), claims
  22:41:55 and 22:41:56, ComputeVMs Running 22:42:19, nodes Ready 22:43:17 and 22:43:28, both pods
  Running by 22:43:48, still 2 claims 15 s later. Both Nodes: InternalIP 172.30.0.2, Hostname = node
  name. kubectl exec and logs into the pod on each node at 22:44:09 and 22:44:14: 2 of 2 answered from
  the right node. Scaled to 4 replicas 22:44:22.8 with max-nodes 3: a third claim 22:44:35, Ready
  22:46:07, the fourth pod stays Pending, warning 22:44:32.6 "the cluster wants 4 nodes ... cap is 3".
  Deployment deleted 22:46:14.9: claims, Nodes and ComputeVMs gone 22:47:01.9 (47 s, hold 30 s);
  `dev down` 30 s, no unit, VM or process left. Not proven live: the one-node step for a pod refused by
  resources or selectors.

- **Defects of the 0006 M5-FRESH-DEPLOY run: a failed or released VM ends its contract at once, a claim
  never waits twice, short VM timeouts, one closure per DenoPod (2026-10-12).** On prod a ComputeVM went
  Preparing -> Failed after the accept, and the claim stayed Provisioning until vmReadyTimeoutSec (600 s);
  after a request had ended with `network_timeout`, the web app waited a second 600 s for the join
  secrets. A provision failure made the bidder keep the contract ("provisioning-failed") and send nothing.
  Now every end of a guest the requester did not ask for goes through the one path guest_poweroff used
  (tombstone, contract store, vm.terminated), with the failure message as the reason: a provision
  failure, a ComputeVM that turns Failed in any phase, and a ComputeVM released (Releasing) on the bidder
  without a vm.delete. The requester holds a vm.terminated or vm.hostKeys that arrives before it watches
  the receipt. The web app waits for the join secrets only after a request that completed with the guest
  on the network, from one budget counted from the accept, and a vm.terminated in that wait marks the
  claim Lost; any other outcome marks it Failed at once. Timeouts on the claim path (user: fail fast):
  web app vmReadyTimeoutSec 600 -> 120 s, requester onNetwork 300 -> 120 s (request-vm-ssh 900 -> 120 s),
  bidder provisioning bound 10 min -> 120 s, kcp provider wait for Running 300 -> 100 s. Second defect:
  during `upgrade` the web app DenoPod crashed once with NotCapable on its web dir. Cause: a DenoPod is
  not restarted when its spec changes, the core's processes die with the core, and the new core's deno
  runtime restarted the web app from the spec the previous core had applied (the web dir as an absolute
  path under the previous closure) with its own closure root in `--allow-read`; the crash restart 2 s
  later took the spec appctl had re-applied by then. A path under the TypeScript root is now written
  `${SOCIALWEB_TS_ROOT}/<relative path>` and the service template expands it with the root it starts the
  process from. `upgrade` and `rollback` take `--root` for a staged install (its account home is in the
  staged passwd). Readings (market network fu2, `dev up --profile market-network --bidders 1
  --with-gateway`, guests as VM units of test-0001, UTC): guest image source empty for the first claim:
  accept 20:44:12.71, ComputeVM Failed and bidder_provision_failed 20:44:14.46, vm.terminated submitted
  14.53, web app vm_terminated_by_bidder 14.49 and claim Lost 14.63, gateway released it 20:44:16.04, the
  replacement claim (image source filled) 20:44:18, node Ready 20:44:50. ComputeVM of a joined node deleted
  on the bidder's kcp 20:50:19.07: bidder_guest_gone 19.28, vm.terminated submitted 19.52, claim Lost
  19.67, gateway released 20.25, replacement node Ready 20:51:14. Web app vm-ready-timeout-sec 8:
  vm_fqdn_timeout 20:53:38.88, claim Failed 39.09 (no second wait), gateway released 41.01, vm.delete 41.55.
  Staged gateway (`install gateway --root`, systemd-user of test-0001), `upgrade` between two dev builds
  with different roots: before the fix 4 of 4 upgrades started the web app once with the previous
  closure's web dir (NotCapable, exit 1) and Ready 8.5-13.1 s after the upgrade; after it 3 of 3 started it
  once, Ready 5.2-6.0 s after the upgrade.

- **A DenoPod declares the credentials it receives; no token or CA file arguments (plan 0009
  M1-PROJECTED-CREDENTIALS, 2026-10-12).** Before, appctl minted each component's kcp token (1 h for the
  bidder and guest-kcp, 30 days for the requester and the ssh gateway, 30 min for the web app), wrote it
  and the kcp CA into the component's state dir and handed the paths to the service as
  `--*-kcp-token-file`, `--*-ca-cert-file`, `--*-ca-file` and (web app) `--kcp-kubeconfig`; the vault
  CA and client certificate were read from the OpenBaoServer's own directory, which the service's
  `--allow-read` had to name; the DenoPod also carried a static `KCP_TOKEN` in its environment. Now
  DenoPod `spec.projected` lists the kcp ServiceAccount token (the pod's own workspace or a parent's,
  1 h by default, rewritten at half its life), the kcp CA, an OpenBaoServer client (`ca.crt`, `tls.crt`,
  `tls.key`) and public PEM certificate files. The deno runtime writes them under
  `<state>/denokcp/projected/<cluster>/<namespace>/<pod>/` (each swapped by rename), adds exactly those
  paths to `--allow-read`, and hands the pod `KCP_PROJECTED`, a JSON map of `KCP_PROJECTED_<NAME>` to path,
  which the service template passes on to the service and allows; it no longer forwards `KCP_TOKEN`.
  `status.projected` shows each entry's path, env name, when it was written and when the token expires;
  a credential that cannot be written holds the start and shows its error there. The CLIs take the
  file options from `KCP_PROJECTED_KCP_TOKEN`, `KCP_PROJECTED_KCP_CA`, `KCP_PROJECTED_VAULT` (the client
  dir openbao-server-kcp reads) and `KCP_PROJECTED_NETWORK_CA`. A projected token is minted only for the
  pod's own workspace or a parent of it, and a projected file must hold only PEM certificates. Readings
  (market network pcr, `dev up --profile market-network --bidders 1 --with-gateway`, UTC): the bidder,
  guest-kcp and web app processes carry none of those arguments, and their `--allow-env` names only the
  projected env names; `run request` delivered the secret to a guest through the RFP flow (21:20:04,
  released 200). Token rotation with no restart: the bidder DenoPod's token set to 600 s at 21:23:39, the
  runtime rewrote it at 21:28:39 and 21:33:39 with the bidder process (pid 2506175, run started 21:23:09,
  0 restarts) unchanged; `run request` at 21:39:24, when every token minted before 21:33:39 had
  expired, delivered the secret and was released 200 at 21:41:51 (the bidder wrote its ComputeVM with
  the current file), and the bidder's log holds no 401.

- **OpenBao plugins on an OpenBaoServer; a guest logs in to its bidder's OpenBao by its ssh host key
  (plan 0009 M3-SSHSIG-AUTH-PLUGIN, route B, 2026-10-12).** OpenBaoServer `spec.plugins` names a plugin
  (name, type auth, asset, mountPath). The openbao controller resolves the asset, refuses it unless its
  sha256 equals the fetch pin in `internal/versions/versions.json`, copies it into the server's
  `plugin_directory` as `<asset>-<sha256 prefix>`, registers it in the catalog by that sha256, enables
  the mount and keeps both in its declaration (baosync registers a missing or changed entry, reloads a
  mount that runs another sha256, deregisters an external plugin no declaration names). A new sha256
  is a new file, so a running plugin keeps its binary until the reload; files no catalog entry runs are
  removed after a clean pass. The core's policy gains the catalog and reload paths only on a server
  with plugins. `status.plugins` shows the declared and the running sha256. Only the bidder handle's
  own OpenBaoServer (`deploy/apps/market-bidder/vault.yaml`) declares a plugin; the asset list
  (`openbaoctl.PluginAssets`) and `openbao-plugin-dir` are general for plan 0014's next plugin. The
  guest wiring: the bidder's kcp provider makes a one-time bind secret per guest, puts its sha256 on
  ComputeVM `spec.guestLogin` and the secret in the guest's user_data (`guestLoginModule` of
  cloud-init-common); the ComputeVM controller (Go, client `computevm`) writes
  `auth/sshsig/role/<computevm>` (alias `guest-<computevm>`, 5 min tokens, 10 uses) before it boots
  the guest, fails the ComputeVM when the role is refused, and deletes the role on release; guest-kcp
  forwards `POST /v1/sshsig/{bind,challenge,login}` to the bidder's OpenBao with only each step's
  fields. Readings (market network pc3, UTC; host load 120-200 from other agents' networks): the
  bidder's catalog holds `sshsig` with sha256 62b84df5...a60ea8 = the linux/amd64 pin, command
  `openbao-plugin-auth-sshsig-62b84df545733353`, mount `sshsig/` running the same sha256, Synced
  InSync; the market network's vault lists `cert/`, `token/` only. A guest booted through the RFP flow
  (ComputeVM fc-0a0ceea82866-49f294ef): role written pending, the guest's guest-login.service bound
  SHA256:hWU3P4PF...LGho at 22:24:27 (the ssh-ed25519 key the guest reported for vm.hostKeys) and logged
  in 3 s later (policies default, ttl 300 s). A second bind with another key: 403 "already bound"; a login
  signed by another key: 403 "the signature is not by the bound host key"; any other path under
  `/v1/sshsig/`: 404. The ComputeVM deleted at 22:33:47, gone at 22:33:55 with its role (404). Not read
  in this run: the gateway child's kcp never became ready under that load (kcp-start-informers), so its
  vaults were not listed live; no code path but the bidder's vault.yaml declares a plugin.

- **An OpenBao auth plugin `atproto`: OpenBao does the atproto OAuth sign-in as a confidential client
  (plan 0014 M1-ATPROTO-AUTH-PLUGIN, 2026-10-15).** `cmd/openbao-plugin-auth-atproto` is its own Go
  module on the OpenBao SDK, an asset like `openbao-plugin-auth-sshsig` (`openbaoctl.PluginAssets`,
  the `openbao-plugin-auth-atproto` entry of versions.json, built per platform by stage-assets.sh with
  the same flags, binary sha256 = the catalog sha256). One auth plugin, mounted by an OpenBaoServer
  `spec.plugins` entry. Paths: `config` (client_id, redirect_uris, scope, client_name,
  plc_directory_url, call_timeout 5 s, login_timeout 15 s, refresh_leeway 60 s, trusted_hosts,
  trusted_ca_pem; the ES256 client key is made at the first write and no path returns it),
  `config/rotate-client-key` (the old public key stays in the metadata 24 h), `client-metadata`
  (unauthenticated read: the metadata document with the public keys inline as jwks), `role/<name>`
  (bound_dids, bound_handles, the tokenutil token fields), `login/start` and `login` (unauthenticated).
  `login/start` resolves the handle (DNS TXT `_atproto` and the https well-known file, which must
  agree) or the DID (did:plc through plc_directory_url, did:web), reads the PDS from the DID document,
  its authorization server from the protected resource metadata, refuses a server that lacks PAR,
  PKCE S256, DPoP ES256, private_key_jwt ES256 or the atproto scope, pushes the authorization request
  (PKCE S256, a client assertion, a DPoP proof from a key made for this sign-in, the server nonce on a
  `use_dpop_nonce` answer) and keeps the pending state under a random `state`: single use, gone after
  10 minutes, at most 256 outstanding. `login` takes the state at once (a failed attempt burns it),
  checks `iss` against the issuer the sign-in started with (required when the server announces the iss
  parameter), exchanges the code, checks the token response (DPoP type, `sub` equal to the DID the
  sign-in started for, scope includes atproto), resolves `sub` again and checks that its PDS names the
  same authorization server, checks the role, stores the session in the mount (the DPoP key, the access
  and refresh tokens never leave it) and returns a bao token whose entity alias name is the DID
  (metadata did, handle, pds). A failure after the code exchange revokes the refresh token when the
  server publishes a revocation endpoint. Network rules for every fetch the plugin makes for a user-
  controlled name: https only; the resolved addresses must all be public (checked on the connected
  address, so a rebinding answer cannot slip through); no redirect is followed; one call has 5 s, a
  whole login 15 s; a response over 1 MiB is refused. `trusted_hosts` (host, host:port, or either with
  `=address` to say where to connect) and `trusted_ca_pem` lift the https and public-address rules for
  those hosts only; the openbao controller will copy a market network's PLC directory and PDS names
  there (plan 0014 M2-GATEWAY-SCOPE), and nothing else is exempt. hono-pds's test OAuth server now puts
  `sub` in the token response and `iss` in the authorize redirect, and `/oauth/authorize` accepts
  `identifier` and `password` for a scripted sign-in. Readings (market network atp1,
  `dev up --profile market-network --bidders 1`, the plugin declared on the bidder's OpenBaoServer for
  the run, UTC 17:49-18:05): `status.plugins` shows `atproto` with sha256 = runningSha256 =
  2674a685...499cc0, the linux/amd64 pin; `GET auth/atproto/client-metadata` with no token returns
  the client document with one jwk (kty, crv, x, y, kid, use, alg: no `d`); a login for the DID of an
  account on alice's PDS DenoPod, after the PAR, a scripted authorize and the callback, returns a token
  with policies `atproto-admin` and `default`, ttl 600 s, metadata did and pds, and the identity entity
  holds one alias, name = the DID, mount_type `atproto`; the same state again: 403 "unknown or already
  used state"; an unknown state: the same 403; iss `https://evil.example`: 403 naming both issuers;
  a state 627 s old: 403 "the state expired"; `did:web:127.0.0.1%3A1`, `did:web:169.254.169.254` and `did:web:localhost%3A8080`:
  400 "network rule: ... not a public address"; a role that does not admit the DID: 403; both handle
  and did, an unregistered redirect_uri: 400. Not read in this run: a handle sign-in (the network's
  handles resolve in no DNS), a gateway root vault (plan 0014 M2-GATEWAY-SCOPE declares the plugin
  there).

- **The atproto session stays in OpenBao and OpenBao signs the DPoP proofs (plan 0014
  M1-SESSION-CUSTODY, 2026-10-15).** The session paths belong to the same `atproto` auth plugin, so
  nothing crosses mounts and one asset is pinned. Paths that need a token (the gate is an ACL policy):
  `session/<did>` read (did, handle, pds, issuer, scope, expires_at, status, status_reason, created_at,
  updated_at; no secret) and delete (revokes the refresh token at the server's revocation endpoint when
  it publishes one, then removes the session, and says in the answer when the revocation did not happen);
  `session/<did>/proof` update ({htm, htu, nonce?} -> access_token, token_type, dpop_proof, expires_at).
  The proof is an ES256 DPoP JWT from the session's key, bound to the method, the URL without query, a
  time, a unique jti, the access token hash and, when known, the PDS nonce; htu must be under the PDS
  origin of the session, any other origin is refused. When the access token has less than
  `refresh_leeway` (default 60 s) left, the call refreshes it under one lock per DID and writes the new
  refresh token to storage before it returns, so callers that arrive together share one refresh. An
  answer from the authorization server that refuses the refresh marks the session `expired` with the
  reason and returns the error at once (HTTP 502; later proofs get 409); a transport failure leaves the
  session alone. The last DPoP nonce per origin is kept in memory and a newer one from the caller wins.
  TypeScript, in `ts/atproto-market/lib`: `abc/atproto-session-custody` (`SessionCustody`: start,
  complete, session, proof, signOut; the nonce book; `isUseDpopNonce`; the `CustodyAgent` interface) and
  `atproto-session-custody-openbao` (`createOpenBaoSessionCustody` over an endpoint resolver, the output
  of `createOpenBaoServerResolver` fits, with the cert login; `createCustodyFetchHandler`, which asks for
  a proof per request and retries once on `use_dpop_nonce` with the nonce of the `DPoP-Nonce` header;
  `createCustodySessionManager` for `@atproto/api`'s `Agent`; `createCustodyAgent`, the agent shape the
  requester library takes). A caller holds a bao token and proofs, never the refresh token or the DPoP
  key. Readings (market network atp1, `dev up --profile market-network --bidders 1`, the plugin declared
  on the bidder's OpenBaoServer for the run, UTC 18:06-18:16): `status.plugins` runs sha256
  011a1ae6...33f363a, the pin. Raw `session/<did>` read: ten keys, none a token or key; `config` and
  `client-metadata` carry no private key. With `refresh_leeway` 899 s, two callers at once got the same
  access token and 2 different proofs, the session `active`, expiry moved 18:22:58 to 18:23:19; twenty
  callers at once after the token went stale got 1 access token and 20 proofs, `active` (a second
  refresh would have used a consumed refresh token or minted a second access token). A proof for
  `https://evil.example/...`: 400 naming the PDS origin. With proofs only: `getServiceAuth` 200,
  `createRecord` 200, an `@atproto/api` `Agent` `createRecord` ok, and the requester library's own
  `applyOAuthAgentToRequesterPDS` + `createRepoRecord` wrote a record into the market.rfp collection of
  the account through `createCustodyAgent` (read back). Proof call latency from a TypeScript client with
  one warm mutual-TLS connection, 300 sequential calls: p50 2.87 ms, p95 8.44 ms, p99 11.70 ms, max 19.85
  ms (budget 20 ms at p95). `use_dpop_nonce`: against a local server that demands a nonce, 2 server calls
  and the second proof carried the nonce. A refusal: after the PDS DenoPod restarted (its token store
  is in memory), the next proof returned 502 "refresh failed ... 400 invalid_grant", the session read
  `expired` with that reason, the proof after it 409; sign out answered revoked false ("publishes no
  revocation endpoint") and the session read 404. Not read in this run: a gateway requester DenoPod that
  posts an RFP with these proofs (the gateway's root vault declares the plugin only under plan 0014
  M2-GATEWAY-SCOPE, and the requester takes the custody agent under M2-WEB-APP-SIGNIN); the market
  network's PDS runs hono-pds's test OAuth server, which has no revocation endpoint, so a real
  revocation was not read.

- **Tenant cluster ports are kernel-picked and held by the front; no lease range, no probe (task
  port0-cp, 2026-10-12).** Defect: each gateway leased tenant ports from its own lease file over
  20000-29999 and only probed that a port was free when it handed it out, so two gateways on one host
  (two market networks) handed out the same port before either member bound it (seen: 20036 for a k3s
  cloud-controller-manager, 20024 for an OpenBao, 20000 for an apiserver front); the failfast stopgap
  (one per-account lease file in 30000-32767) is removed with it. Now the front binds its listener on
  port 0 (fd store, sticky in front.json) and holds every other member port as a non-listening
  `[::]:0` socket in its fd store (section 8); k3s takes `--apiserver-port` from its own hold; dumbpipe
  binds UDP port 0. The node's join secret carries the front port, which stays the same for the life of
  the cluster because the front holds its socket across restarts (SIGKILL of a tenant front: back in 1 s,
  inherited, same address and all 16 holds) and a k3s restart binds its held ports again. Readings (two
  market networks p0a and p0b, `dev up --profile market-network --bidders 1 --with-gateway` at once, VMs
  of test-0001, UTC): round 1 23:32:26-23:39:16, round 2 23:39:16-23:45:38, round 3 23:45:38-23:51:47;
  in each round both tenant clusters came up (all members ready 120-131 s after dev up returned), a pod
  applied at zero nodes ran 38-76 s later on a market node, `kubectl exec` through the front and the
  apiserver link answered, and both clusters went back to zero nodes 32-44 s after the delete; no member
  log and no core log named a bind failure. Front ports per round: 33003/46341, 42883/45575,
  37749/37799. What still keeps a known address: only the edge (caddy 443, the kcp front, the web app
  front), and `leases.json` records which port each front holds; after a host reboot the front binds its
  recorded ports again and fails loudly (warning, new port) if one is taken.

- **Two bidders failing over and a bidder restart keep a rented VM are integration tests (plan 0010
  G2-IT-FAILOVER and G2-IT-BIDDER-RESTART, 2026-10-15).** `test/integration/failover_test.go`
  (`TestAStoppedBidderIsReplacedAndReleasesItsOrphan`): a market network with two bidders and a gateway;
  each bidder first wins a contract through `run request` that denies the other bidder's DID
  (`--deny-bidder-dids`), so both are proven to win; the tenant cluster then gets a pod, and the bidder that
  wins the node's RFP is stopped as soon as its accept shows on the NodeClaim, through
  `spec.apps[].stopped` of the MarketNetwork (its kcp, kine, core and DenoPods go down, its state dir stays;
  `Network.SetBidderStopped`). The gateway runs with `vm-ready-timeout-sec` 90 (`NetworkOptions.GatewayOptions`).
  The NodeClaim of the stopped bidder ends Failed, not Lost, with "a VM was accepted but it never reported
  that it was on the network before the VM ready timeout"; the node never joined, so the join grace of a
  Node object is not the clock. The scaler claims a replacement under the same name, the other bidder wins
  its RFP and the pod runs. Starting the stopped bidder again makes it release the orphaned ComputeVM of
  the old receipt through its restored contract store, with nothing left. Readings (each run on its own
  market network): bidder-1 stopped, claim Failed 82.4 s after the stop, bidder-2 won the new RFP and the pod
  ran 134.6 s after the stop, the orphan was Releasing 0.7 s after bidder-1 was Ready again and gone 3.0 s
  after; mirrored in a second run (bidder-2 stopped, bidder-1 won) with 83.0 s, 135.4 s and 2.3 s. The test
  takes 421.6 s from `dev up` to a clean `dev down`.
  `test/integration/bidder_restart_test.go` (`TestARentedVMSurvivesABidderKillAndRestart`): one bidder, a
  guest rented with `run request --hold`; then kill -9 of the bidder DenoPod, `dev restart` of it, and kill
  -9 of the bidder's `dev-core`. After each the VM unit's InvocationID, MainPID and start ticks, the
  ComputeVM's runtime pid and start time, its guest address, its onNetwork record and its receipt are the
  same, and the bidder's logs name one issuer URL. The held contract is then released with vm.delete 200 and
  no ComputeVM, VM unit or VM work dir is left. Readings: the bidder was back 3.6 s, 3.5 s and 10.5 s after
  the three cases (2.5 s for the core case in a second run); 446-459 s for the test. The `upgrade --binary`
  leg of 0007 M6-BIDDER-UPGRADE is not in the test: `upgrade --binary` works only on an installed host, so it
  stays a manual live reading. The integration job of `.gitlab-ci.yml` has 90 minutes instead of 75 for the
  two added networks.

- **The GitLab fixture serves a runner on a tenant cluster (plan 0010 G1-GITLAB-FIXTURE, 2026-10-15).**
  Live proof of `scripts/gitlab-fixture.sh up` and `down` on hat-8 (docker, the pinned gitlab-ce
  19.2.7 image, plain http on the host's LAN address and a docker-picked port): `up --name itac4924` was
  ready in 230.2 s with the project `root/fixture-probe`, its template commit (a pipeline already
  created) and a project runner tagged `socialweb-computer-fixture`. On a market network with a gateway
  and one bidder, a pod running `gitlab/gitlab-runner:alpine-v19.2.0` was applied to the tenant cluster at
  zero nodes (a ServiceAccount and Role for the kubernetes executor, the runner token in a Secret, `gitlab-runner
  register --executor kubernetes` then `run`). The cluster scaled out on a market RFP VM (claim Bidding
  +18.8 s, Provisioning +22.1 s, Joining +37.7 s, node Ready +47.0 s), the runner pod ran +60.0 s, and the
  pipeline of the template commit was `success` +73.0 s after the pod was created: job `clone-probe` ran as
  a pod of the executor on the market node, cloned the project from the fixture over the guest's own
  network and printed `FIXTURE_CLONE_OK`. `down --name itac4924` left no container, no volume and no state
  dir. The run was a manual reading through a scratch Go test (not kept: it needs docker and a free
  8 GiB for GitLab, which the CI runner of `test/integration` does not promise); the guest reaches the host
  address the fixture publishes on, so the default `--address` works for a runner in a tenant cluster.

- **One requester controller for the CLI and the gateway; the web app holds no market session (plan 0009
  M3-REQUESTER-CONTROLLER, 2026-10-15).** Before, the requester existed twice: `request-vm-ssh` carried
  exactly one ComputeContract per DenoPod, and the web app ran the same flow in its internet-facing
  process for every NodeClaim, with the owners' OAuth sessions, a requester key and each claim's secrets
  key in its own vault mount and memory. Now `request-vm-ssh` is a controller over every ComputeContract
  of its workspace (`lib/abc/requester-controller` for admission, disposition and the profile
  interface; `lib/requester-controller-kcp` for the watch, the per-contract runner and the orphan
  scan). A MarketRequester application (`run request`) keeps its one identity and its one contract
  `contract`. A Gateway application renders a second DenoPod, `requester`
  (`deploy/apps/gateway/requester.<profile>.values.yaml`, own ServiceAccount, no `env: true`, no
  `sys: true`), with its own client `requester` of the root vault: a KV mount of its own for the
  requester key and each contract's secrets-server key, read and update on
  `socialweb-computer-k8s/oauth-sessions/*` (the delegated sessions the web app deposits at sign-in; the
  controller reads each fresh from the vault on every use, so a refresh by either process is the next
  call's session) and read on `socialweb-computer-k8s/contracts/*` (the join bundles). The web app's
  node market (`lib/socialweb-computer-k8s-node-market`) is now a NodeClaim to ComputeContract mapper: it
  writes `nodeclaim-<hash of the claim uid>` with `createIfAbsent` in the Gateway's own workspace (a
  claim in a home workspace cannot own an object in another, so the claim's finalizer, not an owner
  reference, ties them: the web app sets `spec.release`, waits for `Released` or `Failed`, deletes the
  contract and the bundle, and only then removes the finalizer), writes the join bundle to its own vault
  mount, and maps `status.contract`, `status.grant` and the phases back. It holds no PDS session, no
  claim key and no secrets key; `socialweb-computer-k8s-claim-keys-openbao`, `-claim-keys-vault`, the
  in-process account requesters and the node fulfiller are deleted. What stays in the web app is the
  user's sign-in session (the vault session store, request proofs, and `getServiceAuth` for the
  cluster gateway until plan 0014 M2-WEB-APP-SIGNIN replaces it with `bao login`); two processes now
  refresh one session, so each re-reads the vault session before every use and a rotation by the other
  is seen on the next call. The unclaimed-receipt scan and the adoption of joined nodes moved with the
  flow (contracts with `spec.adopt`). The shared ComputeContract kind gained `spec.ownerDid`,
  `spec.adopt`, `spec.nodeJoin` and `status.grant`. The ssh gateway keeps its in-process session
  requester as `ts/socialweb-computer-ssh/lib/socialweb-computer-ssh-requester` over the shared
  `socialweb-computer-account-agents` and `socialweb-computer-account-requesters`; the package
  `socialweb-computer-requester-inproc` is gone. Readings (market network m3a, `dev up --profile market-network --bidders 2 --with-gateway`,
  guests as VM units of test-0001, UTC, 2026-10-15): Ready 1 min 40 s after the start; the gateway
  application listed DenoPods `requester` and `socialweb-computer-k8s`. A pod applied to the tenant
  cluster at zero nodes at 18:18:28: NodeClaim `tenant-node1`, ComputeContract `nodeclaim-de81bc9e...`
  created 18:18:30, RFP submitted 18:18:31.09, winner 31.35, accept 31.70, receipt 31.75, claim
  `Joining` 18:18:49, node Ready 18:19:02, pod Running 18:19:06 (38 s after the apply). Vault, read
  with each client's own certificate: the web app's `socialweb-computer-k8s` token gets 403 on every
  `requester/*` path (read and write) and 200 on its own mount; the `requester` token reads and writes
  `requester/*`, reads and updates `socialweb-computer-k8s/oauth-sessions/*`, reads
  `socialweb-computer-k8s/contracts/*` and gets 403 writing there. The web app process has no
  `--allow-run` and an `--allow-env` list of the projected names and `PORT_FILE`, `LISTENERS_FILE`,
  `STATUS_FILE`, `KCP_NAMESPACE`, `KCP_WORKSPACE`, `PATH`, `HOME`, `BUNDLED_ACTIONS_DIR`. A second pod
  (anti-affinity) at 18:19:21 made a second claim; the requester DenoPod was restarted at 18:19:33,
  while that contract was at `receipt` and the first was `Held`: the new run logged `rfp_resumed`,
  `accept_resumed` and `receipt_resumed` for the second contract (no second RFP, no second accept),
  `requester_contract_held_resume` for the first, the guest of the second fetched its bundle through
  the re-mounted secrets server at 18:19:44.6 and the contract was `Held` at 18:19:46.7; both nodes
  Ready. `run request --secrets` in the same network delivered the secret to its guest
  (`m3-secret-8f3a1c77d2` read at the guest's path), 2 bids, released 200 at 18:23:17. Both pods
  deleted at 18:23:22: the web app set `spec.release`, the controller sent `vm.delete` (200) at
  18:23:36.7 and 18:23:38.7, the contracts and bundles were deleted, the claims finalized and the cluster
  was at zero nodes at 18:23:39. A repeated status write of an unchanged claim (key order differing
  between the object and the mapped status) was found in the first run, fixed with a canonical
  comparison, and the second cycle on the same network logged 5 claim writes. Open edge: the web app and
  the requester refresh one session from two processes; both re-read the vault session before each
  use, so only two refreshes in the same instant can replay a refresh token.

- **The requester records the bids it saw and why it chose one (plan 0009 M3-REQUESTER-BIDS,
  2026-10-15).** ComputeContract `status.bids` holds at most 20 entries `{did, uri, offer (the bid
  payload NSID), cost, verdict, arrivedAt}` and `status.chosen` `{uri, did, reason, considered}`; no
  object per bid. `requestCompute` gained the handler `onWinner` (called after the policy re-check and
  before the accept record) and `arrivedAt` on `onBid`. `reason` is `firstFree` (the first
  policy-allowed free bid ended the window), `bestWithinWindow` (the lowest cost after the window),
  `policy` (best within the window, confirmed by the RFP's attached policy) or `resumed`; the verdict of
  each recorded bid is `considered`, then `chosen` or `notChosen`. Reading (the run above):
  `kubectl get computecontract -o yaml` showed two bids, both `offer:
  com.publicdomainrelay.temp.market.bids.free`, arrived at 18:18:31.347, the winner `chosen` and the
  other `notChosen`, and `chosen: {reason: firstFree, considered: 2}`.

- **Policies attached to an RFP run in a net-only policy engine DenoPod, not in the bidder or the
  requester (plan 0009 M3-POLICY-ENGINE-POD, 2026-10-15).** The MarketBidder profiles (`prod-qr`,
  `market-network`, `dev-local`) and the MarketRequester profiles (`market-network`, `default`) each
  render a DenoPod `policy-engine` (`deploy/apps/market-bidder/policy-engine.values.yaml`,
  `deploy/apps/market-requester/policy-engine.values.yaml`) of the entry `hono-policy-engine`
  (`ts/policy-engine/hono-policy-engine`: one `Command`, `createLogger`, `createServe` on port 0, the
  `createApp` of `policy-engine-server-gha-lite` forced `netOnly`). Its process has `--allow-net`,
  `--allow-read` of the TypeScript root, its run directory and its own state directory, `--allow-write`
  of the run directory and the state directory, `--allow-env` of `PORT`, `PORT_FILE`,
  `LISTENERS_FILE`, `STATUS_FILE` and the kcpdns names, and `--ignore-env`; no `-A`, no
  `--allow-all`, no `--allow-run`, no `--allow-ffi`, no `--allow-sys`. It has no ServiceAccount (so no
  kcp token), no projected credential, no vault and no PDS session. The bidder and the requester
  take `--policy-engine-url http://policy-engine.default.app.svc.kcp.local` and reach it by service name
  through kcpdns. `policy-engine-client-http` implements the gha-lite executor's `workflowRunner`
  over `POST /request/run` (the verdict in one hop, no poll) and `createRemoteEngineRegistry`; the
  evaluator takes `attachedRegistry`, which `evaluatePolicies` (the policies of an RFP, at the bidder
  and in the requester's re-check of a bid) uses while the hot-path `scope` lane keeps the in-process
  registry, so a built-in scope policy stays in process. A `typescript` policy record needs a worker
  in the caller and is refused when an engine URL is set. A policy expression that reaches for a file
  or the environment fails the job with `the policy expression was denied: ...` (before, the
  expression fell back to its literal text and the job went on); the executor maps it to the violation
  policyId `sandbox-denied`, the bidder answers `policy sandbox denied`, and the loop counts the
  decline as `policySandboxDenied`. The PolicyEngine kind's runner no longer starts the engine with
  `--allow-all`: it passes the same net-only argv. Two decisions differ from the plan text.
  The `policy-engine` DenoPod serves plain HTTP behind the kcpdns name (`tls: none`) because an
  application's kcp holds no `OpenBao` PKI namespace, so no pod there can be issued a certificate; a
  namespace object in the application workspace is the path to TLS. Net is `--allow-net` without a host
  list because a policy action reads any PDS and the kcpdns shim rewrites a name to a loopback address.
  The TaskManager of the engine keeps at most 1024 finished tasks. Not done: the Gateway application's
  `requester` DenoPod (`deploy/apps/gateway/requester.<profile>.values.yaml`) still evaluates in
  process. Readings (market network pe1, one bidder, guest as a VM unit of test-0001, UTC, 2026-10-15):
  the engine DenoPod argv, read from `/proc/<pid>/cmdline`, held `--allow-net`, `--allow-env=PORT,...`,
  `--ignore-env`, `--unstable-worker-options` and none of `-A`, `--allow-all`, `--allow-run`,
  `--allow-ffi`. `run request --policy open`: the RFP was submitted 18:45:50, the receipt was in at
  18:45:51, the guest was on the network at 18:46:11 (`bids: 1` in the MarketBidder status); the
  engine log showed `task created`, `executing workflow` and `completed: status=SUCCESS` for that
  RFP and the bidder logged `bidder_policy_engine_remote` at start. The bidder's own process logged one
  in-process `executing workflow` after that run, the hot-path scope lane of its own `open` policy,
  which stays in process; the attached policy started no Worker in the bidder. With the requester's `open` policy changed to read `/etc/hostname` in an
  expression: `policy rejected bid` with violation `the policy expression was denied: Requires read
  access to "/etc/hostname", run again with the --allow-read flag` (policyId `sandbox-denied`) at
  18:49:59.9, the engine task `FAILURE`, no bid, the contract `Failed: no_bids` at 18:50:30, and the
  MarketBidder status `declinedByReason: policySandboxDenied 1`. An expression that reads the
  environment is denied the same way (`Requires env access (get), denied by the policy sandbox`), read
  against the engine started alone with the same permissions. Latency of the local hop is read by plan
  0009 M4-LATENCY-READINGS.

- **Vault storage stays bounded: no_store leaf roles, pki auto-tidy, kv max_versions, token and lease
  tidy (plan 0013 M3-VAULT-TIDY, 2026-10-15).** The OpenBaoServer spec declares the bounds and the
  controller keeps them in sync like its mounts. `declared.pkiRoles[].noStore` writes `no_store`; every
  role of the cluster vault and of the root vault sets it (the DenoPod leaf role that
  `kcplibs/pkiprovisioner` writes in each namespace of the network vault too) except the two kubeconfig
  roles, whose certificates must stay stored: `pki/revoke` by serial and the `pki/certs/revoked` list
  that the client certificate front reads need them. `declared.pkiTidy[]` (mount, interval default 6 h,
  safetyBuffer default 72 h) writes `<mount>/config/auto-tidy` with `enabled`, `tidy_cert_store`,
  `tidy_revoked_certs` and `tidy_expired_issuers` false, so OpenBao runs the tidy itself; it removes a
  certificate only after its notAfter plus the buffer, which is why a revoked certificate that has not
  expired and a no-expiry certificate (notAfter at the CA's, 2036) stay, and the issuer settings that
  `ImportClientCA` rewrites on each harvest (`leaf_not_after_behavior`, `usage`) are never written by
  the tidy. `declared.secretMounts[].maxVersions` (default 3 for every kv-v2 mount the controller
  declares, the clients' mounts included) writes `<mount>/config`. `spec.tidy.interval` (default 6 h,
  at least 1 m) is the interval of the controller's own `auth/token/tidy` and `sys/leases/tidy` calls,
  recorded in `status.tidy` (lastRunAt, interval, message). The core's policy gained
  `+/config` write, `+/config/auto-tidy`, `auth/token/tidy` and `sys/leases/tidy`. Readings (market
  network vtidy1, `dev up --profile market-network --bidders 0`, UTC 18:31-19:00): the network vault
  declared 2 secret mounts, 1 auth mount, 2 policies and 3 roles and configs on the first pass with
  `status.tidy` set; `secret/config` read `max_versions` 3; with pkiTidy patched to interval 1 m and
  safetyBuffer 5 s, `pki/config/auto-tidy` read enabled, interval 60, safety_buffer 5. Five
  certificates were issued from throwaway roles with no_store false (ttl 3 s expired, a revoked one with
  ttl 1 h, a live one with ttl 24 h, a revoked one that then expired, one with ttl 700 h): after the
  automatic tidy (`tidy-status` Finished, cert_store_deleted_count 4, revoked_cert_deleted_count 2)
  the two expired certificates and their revocation entries were gone and the revoked unexpired
  certificate (still on `pki/certs/revoked`), the 24 h and the 700 h certificates stayed. Storage
  of the network vault (file backend, bytes under `openbao/storage`): with the namespace leaf roles
  still storing certificates, 60 restarts of one DenoPod (`dev restart vtidy1 relay:relay`) took it
  from 130698 bytes in 134 files to 176446 bytes in 194 files (+35.0 %, one certificate per
  restart); after the core ran this code (no_store on the namespace roles), 500 restarts of the same
  DenoPod left it at 180613 bytes in 197 files at restarts 0, 25, 50 and every 25 up to 500
  (+0.00 %). A tenant cluster vault of a gateway network shows `noStore: true` on its kcp-client,
  tenant-superuser and node-client roles, `pkiTidy` on `pki`, and `status.tidy`.

- **The atproto auth plugin is declared on the gateway root vault and the ssh-gateway vault, with its
  config, roles and policies from the spec (plan 0014 M2-GATEWAY-SCOPE, 2026-10-15).** `OpenBaoServer
  spec.plugins[].atproto` carries `config` (clientID, redirectURIs, clientName, scope, plcDirectoryURL,
  trustedHosts, trustedCAPEM) and `roles` (name, boundDIDs, boundHandles, policies, tokenTTL,
  tokenMaxTTL). The controller writes `auth/<mount>/config` and `auth/<mount>/role/<name>` like any
  declared object, and keeps the `role/` collection of that mount exactly to the declaration (the
  sshsig mount is not managed: its roles are written at run time by the ComputeVM controller). The CA
  and the client key are write-only: the plugin returns `trusted_ca_set` and the key id, so drift is
  read from those. `spec.declared.policies` may name `%accessor:<mount>%` in a path; the controller
  replaces it with the accessor of that auth mount and applies the policy in the same pass that
  enabled the mount (a second read of `sys/auth` and a second sync), so `atproto-self` reads
  `auth/atproto/session/{{identity.entity.aliases.<accessor>.name}}`, deletes it and updates its
  `/proof`, and nothing else. Decided here, user may veto: `client_id` is the application's
  `public-origin` option plus `/oauth-client-metadata.json` and the redirect URI is `/oauth/callback`
  (the path M2-WEB-APP-SIGNIN serves); without that option the mount is declared with roles and no
  config. In a market network `plc_directory_url` is `https://<the plc host name>` and `trusted_hosts`
  is `<name>=<address>` for the `plc.` and `pds.` hosts of the network value, with the network CA
  bundle as `trusted_ca_pem`; a prod gateway has no network value and uses plc.directory. Role `user`
  admits every DID (policy atproto-self, token ttl 1 h, max 30 d). The gateway requester client has
  `atproto-session-proxy` (read and proof on any session); the web app client has only its own kv
  mount. The root vault spec is now built from the Gateway spec (`appctl.RootVaultSpec`), the
  ssh-gateway vault from the SshGateway spec through `{{.VaultExtra}}` in
  `deploy/apps/ssh-gateway/vault.yaml`. Bidder, requester and tenant cluster vaults declare no
  plugin of this kind. Readings (market networks on hat-8, UTC 18:43-18:50, `dev up --profile
  market-network --bidders 0 --with-gateway`, gateway option public-origin
  `https://127.0.0.1:41873`, `run gateway ssh` with public-origin `https://127.0.0.1:41874`): the
  gateway's `root-vault` lists `approle`, `atproto`, `cert`, `token` and its `status.plugins` runs
  sha256 011a1ae6...33f363a, the pin; Synced InSync on the first pass with 13 policies and 14
  roles and configs; the rendered `atproto-self` names `auth_atproto_8716bd94`, the mount's
  accessor. The `ssh-gateway` vault lists `atproto`, `cert`, `token` with the same running sha256;
  the tenant cluster vault of the gateway declares no plugin. A login by DID through the network's
  alice PDS DenoPod (an account made on it, login/start, a scripted authorize, the callback to
  `auth/atproto/login`): 200, policies `atproto-self` and `default`, ttl 3600 s, metadata did and
  pds; with that token the own session read 200 (ten keys, none secret) and the own proof 200; the
  session and the proof of another DID 403; `auth/atproto/config` and a kv list 403. With the
  `requester` client's cert token: session read 200, proof 200, delete 403; with the web app
  client's: all three 403. The web app client's policy `client-socialweb-computer-k8s` lists only
  its own `data` and `metadata` paths. The web app's `oauth-sessions/<did>` path went with plan 0014
  M2-WEB-APP-SIGNIN (next entry).

- **The web apps sign users in through the OpenBao atproto plugin; the browser holds a cookie, not a
  session (plan 0014 M2-WEB-APP-SIGNIN, 2026-10-15).** `socialweb-computer-k8s/hono-socialweb-computer-k8s`
  and `ts/socialweb-computer-ssh/hono-socialweb-computer-ssh` hold no OAuth session, no DPoP key and no
  refresh token. The client_id is the public-origin option plus `/oauth-client-metadata.json`, which the
  web app serves from the plugin's `client-metadata` path (cached 60 s); the redirect URI is
  `/oauth/callback`. `POST /oauth/start` takes a handle or a DID, calls `login/start` (role `user`) and
  answers the authorization URL, and sets a short-lived HttpOnly SameSite=Lax state cookie that the
  callback must match (a sign-in started in another browser is refused); `GET /oauth/callback` forwards
  {state, code, iss} to `login`, writes the MarketIdentity (sessionRef = mount `auth/atproto`, path
  `session/<did>`) and sets the bao token as the cookie `swc_session` (HttpOnly, Secure when the origin is
  https, SameSite=Strict, Max-Age 30 d; the token itself has ttl 1 h and is renewed on use, up to the role's
  max). `GET /session` answers the DID, `DELETE /session` deletes the session in the plugin (which revokes
  the refresh token at the authorization server's revocation endpoint), revokes the cookie's token and sets
  the MarketIdentity condition SignedIn False. Every request that changes state must carry an Origin equal to
  the public origin. The web app calls bao with the user's own token only (`createOpenBaoCustodyConnector`
  `forToken`: lookup-self for the DID, renew-self, a custody agent for the PDS calls, `getServiceAuth` for
  the cluster gateway), and its own client policy reads no session path. The gateway `requester` DenoPod
  and the ssh gateway act for any DID through `atproto-session-proxy` (`socialweb-computer-account-agents`
  now builds `createCustodyAgent` agents from the plugin's session read; the requester takes
  `--account-custody-mount atproto`). The ssh gateway's page lists, adds and removes its key records
  through `/keys` (`hono-factory-socialweb-computer-keys`), and its co/core pairing identifies the user by
  the cookie. Packages: `socialweb-computer-web-signin-openbao` (the `WebSignIn` implementation, new),
  `hono-factory-socialweb-computer-oauth` (the sign-in routes and the cookie authenticator). Deleted:
  `socialweb-computer-oauth-session-fs`, `-session-vault`, `socialweb-computer-oauth-atproto` (the deposited
  session verifier), `socialweb-computer-k8s-request-proof`, `key-file-fs`, POST /session, the browser-side
  OAuth client and DPoP key (web/lib/atproto-oauth.js, pds.js), `OAuthSessionData`, the file fallback of
  accountState (`--openbao-server` is required in both web apps), the generated web scope, the
  `--oauth-client-id` option of both web apps, and the tests these broke. The sign-in scope now lives in
  Go (`appctl.GatewayAtprotoScope`, `SshGatewayAtprotoScope`), the one place that writes the plugin config.
  The web apps are probed at `/healthz`. A custody agent now speaks the spec to a real PDS: getServiceAuth
  is a GET with a query and applyWrites writes carry the `$type` of the lexicon union. hono-pds's test OAuth
  server gained `GET /oauth/authorize` (a sign-in form for a scripted browser), `POST /oauth/revoke`, and
  names itself by its publicHostname when it is reached by a loopback address (DPoP htu of a client that
  dialled the name). `scripts/oauth-signin.ts` signs a handle or DID in through the web app's form, takes
  Chrome arguments, and prints the cookie attributes, never the values. One defect of the base was found in
  the live run and fixed: `grantingCapability` of the node contract dropped `rbacRecord`, so a gateway
  requester never published the record the bidder turns into the guest's identity grant (the guest was
  refused its OIDC token, 403, and never fetched its join secrets).
  Readings (market network m2w1, `dev up --profile market-network --bidders 1 --with-gateway`, gateway
  option public-origin `https://127.0.0.1:43117` fronted by a TLS forwarder with a pod leaf of the network,
  `run gateway ssh` for the ssh gateway, headless Chrome with the PDS name mapped, UTC 19:52-20:16): the PDS
  fetched the client metadata from the web app (jwks with one public key, no `d`); `oauth-signin.ts` signed a
  DID in and the page showed the cluster list; the cookie attributes were name swc_session, httpOnly true,
  secure true, sameSite Strict. Vault, read with each client's certificate: the web app's kv mount lists
  only `contracts/` (no `oauth-sessions`), the web app reading `auth/atproto/session/<did>` is 403, the
  requester reading it is 200 with ten keys and none a token or key, and 403 on the web app's old session
  path. A second user (an account made on alice's PDS, not in the owner-sessions file) created the cluster
  `web1` through the web app (200, service auth from a bao proof), and a pod applied to it at 20:06:32 made
  the NodeClaim, ComputeContract `nodeclaim-d7de...`, an RFP record in the user's repo, a bid, an accept,
  the secrets record `capability_rbac_created`, and at 20:06:56 `requester_secrets_fetched` (the guest
  fetched its join secrets), `Held` at 20:06:58: all as proofs from bao, no refresh token or DPoP key in the
  requester. After `dev restart` of the web app DenoPod (pid changed, run during a claim) the same cookie
  answered `GET /session` 200 and listed clusters; the claim kept posting. Sign-out: `DELETE /session`
  answered `{"revoked":true}`, the same cookie then 401, the session read 404, the MarketIdentity read
  SessionExpired with SignedIn False, and the other user's session stayed `active`. The ssh gateway: sign-in
  by DID, `/keys` listed none, added one (a record in the user's repo), listed it, deleted it (applyWrites
  with the union `$type`), sign-out revoked. The web app's DenoPod arguments and run logs hold neither the
  cookie token nor `refresh_token`/`dpopPrivate`; the process memory was not scanned. Nodes join the tenant cluster through the bao-issued guest identity (see the node
  join profile above): in TestTenantClusterScalesFromZeroOnMarketVMsAndBack two pods made two NodeClaims at
  +29.2 s, both claims were Joining at +47.9 s, both nodes and pods were Ready at +64.7 s, and scale-in
  reached zero nodes 20.9 s after the last pod went.

- **Every kubeconfig certificate has a `ClusterClientCertificate`, and the front admits a serial only while
  its object exists and is not revoked (plan 0014 M3-KUBECTL-CLIENT-CERT, 2026-10-15).** The kind
  (`api/v1alpha1/types_clusterclientcertificate.go`, export `clusters`, schema `v1alpha1-1`) lives in the
  home workspace of the cluster, next to the `Cluster` it hangs from through `ownerReferences`; spec
  `{cluster, did, serial, notAfter, noExpiry, revoked}`, status `{revokedAt, lastError}`; no key and no
  certificate body. `clientcert.Issuer.Grant` (the `getKubeconfig` handler) writes it after it signs
  (from the client CA on disk, or after `pki/issue` when the vault is the source) and before it hands
  the kubeconfig back (`kcpclient.RecordCertificate`); when the write fails it revokes
  the serial and answers ClusterInitializing, so no certificate exists without its object. It replaces
  the `noexpiry-<serial>` ConfigMap, which only no-expiry certificates had. `Issuer.Revoke` (a cluster
  delete, `revokeIdentity`) revokes the serials in the PKI and sets `spec.revoked` and `status.revokedAt`
  (`MarkCertificateRevoked`); a Cluster delete also removes the objects with the Cluster. The front
  (`clientcertfront.Config.Records`, implemented by `kcpclient.CertificateState`) keeps one list and watch
  per home workspace, started by the first request for it and stopped after 10 minutes without one, and
  checks the serial after the chain, the PKI revoked list and before the deny record: no object is 401,
  a revoked object is 401, an unsynced watch is 503. The PKI list stays the second source. A user
  of the web app downloads the kubeconfig through the bao proof path of M2-WEB-APP-SIGNIN
  (service auth from the plugin, `getKubeconfig`); the web app holds no key. Not built: the "revoke my
  certificates" choice at sign-out (no method of the gateway revokes certificates without denying the
  DID). Readings (market network cc3, `dev up --profile market-network --bidders 0 --with-gateway`, gateway
  option public-origin `https://127.0.0.1:43871` fronted by a TLS forwarder with a pod leaf of the network,
  headless Chrome with the PDS name mapped, UTC 2026-10-15 14:35-14:42): `oauth-signin.ts` signed the
  owner DID in (cookie swc_session HttpOnly Secure SameSite Strict) and `POST /api/clusters/kubeconfig`
  answered 200 with `Cache-Control: no-store`; `kubectl --kubeconfig <download> get ns` listed five
  namespaces through the client certificate front, and `kubectl get clusterclientcertificates -o wide`
  in the home workspace listed the serial, `notAfter` and `revoked`, with an `ownerReferences` entry
  Cluster/tenant and no key. A script with only a PDS session (`createSession`, `getServiceAuth`,
  `getKubeconfig` over the gateway's xrpc listener) got `200`, `no-store`, and a kubeconfig that worked;
  the same call with another `lxm` or another `aud` was 401; a second account's call for the name
  `tenant` was answered for its own cluster (`ClusterNotRunning`), never the owner's. Patching
  `spec.revoked` to true refused the next `kubectl` call 50 ms later (front log reason
  `ClusterClientCertificate revoked`); deleting the object refused it 35 ms later (`no
  ClusterClientCertificate for this serial`); a certificate issued with `ttlSeconds` 60 was accepted at
  14:40:06 and refused at 14:41:14. A cluster created through the web app (`cc3b`) got a no-expiry
  kubeconfig that worked; `deleteCluster` through the web app logged `certificates revoked count 1`, the
  old kubeconfig was refused, and the objects were gone with the Cluster 30 s later. `revokeIdentity`
  marked the fresh certificate revoked (`REVOKED true`, the expired one stayed false). A scan of the
  gateway's logs and DenoPod run directories for `PRIVATE KEY` and `client-key-data` (tls.key files
  excluded) found none after five issues.

- **Every child object hangs from an owner that ends, and each root reports what does not (plan 0013
  M2-OWNERSHIP-TREE, 2026-10-15).** kcp's garbage collector cascades `ownerReferences` inside one
  workspace and never across workspaces, and it cascades bound resources (read: a Bid owned by a Bid went
  with its owner within 2 s). So the rule has two forms. Inside one workspace the child carries an
  `ownerReferences` entry: `appctl.adoptChildren` (every 2 s pass, per root object) hangs an
  `OpenBaoServer`, a `MarketIdentity`, a non-bootstrap `HostProcess` and the `app` `Workspace` of the root
  from it (they carry the `apps.socialweb.computer/root` label the profile apply stamps), a `Bid` from a
  `MarketBidder`, a `ComputeContract` of the CLI from a `MarketRequester` (one with the
  `socialweb.computer/nodeclaim` annotation is a gateway contract and is skipped), and the
  `socialweb-bounded` ResourceQuota of the root workspace from the root; the computevm controller hangs a
  `ComputeVM` from the Bid whose `status.computeVM` names it (`linkBids`, every 10 s while one is loose; the
  VM's own finalizer still stops its unit before the object goes); `nodemarket.clusterOwner` puts the
  `Cluster` on a `NodeClaim` at creation and `kcpclient.WriteNodeClaimTo` adds it to a claim that has none.
  Across workspaces the owner link is a label or annotation that a named controller acts on: a DenoPod and
  the other profile components in `root:app` carry the root label and the `apps.socialweb.computer/components`
  finalizer of the root removes them (`deletePods`, `deleteVaults`, `deleteComputeVMs`); a gateway's
  cluster members and Postgres carry `socialweb.computer/cluster` and the cluster provider removes them on
  teardown; a gateway contract carries `socialweb.computer/nodeclaim` and the claim mapper deletes it with
  the claim. A `PolicyWorkflowRun` has no creator in this tree (the provider only reconciles one), so it has
  no owner to hang from; one that appears with neither owner nor TTL is counted. The bootstrap mirrors
  (`bootstrap-*` HostProcess objects) belong to the instance, not to a root, and are not counted. Every
  root's `status.gc` lists, by kind with up to 5 examples, the objects in the root workspace and in each
  ready child workspace that have no `ownerReferences`, no TTL annotation and no owner label or annotation
  that resolves (the label counts only while its root exists), and the objects terminating for more than 10
  minutes by kind and finalizer with the controller that removes it (`appctl.FinalizerControllers`, the
  inventory: the root finalizer is appctl, `computevm.socialweb.computer/release` the computevm controller,
  `socialweb.computer/postgres` postgresctl, `socialweb.computer/cluster-member`,
  `cluster.socialweb.computer/teardown` and `node.socialweb.computer/teardown` the cluster provider,
  `nodeclaim.socialweb.computer/release` the node-market claim mapper of the web app,
  `atprotoidentity.socialweb.computer/revocation` the web app's revocation sweep, and the five
  `*.deno.computer` finalizers the deno provider). The sample runs every 30 s; a sample whose content did
  not change keeps its `sampledAt` for 5 minutes so the root is not rewritten for nothing. Readings (hat-8,
  test-0001, market network b13a, `dev up --profile market-network --bidders 1`, UTC 20:38-20:40): the
  MarketBidder `app` was deleted with its children in place (old uids: root 4dc89a0c, OpenBaoServer
  53cd9026, MarketIdentity 1e1b6102, Workspace app 6c65a9ee, HostProcess bao-bidder f60fc3ba, DenoPods
  bidder 323183ae, guest-kcp d3ecd239 and policy-engine 7c52cc7e); the network applied a new root 9 s later
  and, when the bidder was Ready again, every one of those objects had a new uid (root a8120f00,
  OpenBaoServer f9cd1d2d, MarketIdentity 05623793, Workspace app ed12e089, HostProcess 2f9719ab, DenoPods
  11180802, 8c9ba326, a84f48e6), the old Workspace being the last to go (NotFound 20 s after the delete).
  `status.gc` of that bidder read `objects 20, orphanTotal 0, stuckTotal 0` with 10 Bids, four finished
  ones included, and of the network `objects 13, orphanTotal 0`. The gc scan also covers the home workspaces under each `account`
  workspace (up to 200 workspaces), where NodeClaims and Clusters live. Cluster delete (market network b13g,
  `dev up --profile market-network --bidders 0 --with-gateway`, the bootstrap Cluster `tenant` Ready with its
  members, its Postgres and its OpenBao): a NodeClaim `probe-node1` made by hand with an
  `ownerReferences` entry naming the Cluster's uid (the door gave it its finalizer and marked it Failed
  for lack of an apiserver ticket) and `kubectl delete cluster tenant`: 1 s after the delete 10 cluster
  HostProcess objects, the Postgres, the NodeClaim and the Cluster were all still present, at 7 s six
  members were left, at 12 s none, and at 17 s the Cluster, the NodeClaim and the Postgres were gone, with
  no ComputeContract left in `root:app`; the gateway's `status.gc` read `objects 21, orphanTotal 0,
  stuckTotal 0` afterwards. A real claim: in a gateway network with two bidders (the integration test
  TestTenantClusterScalesFromZeroOnMarketVMsAndBack, run by hand) the two NodeClaims the scale-out wrote
  read `ownerReferences[0] Cluster tenant uid 1d7216f7-ae71-461d-93db-ff89615a4f59`, the uid of the Cluster
  in the same home workspace. That test fails at the claims' join (`a VM was provisioned but it never
  fetched its join secrets`; the guests' `identity/oidc/token/guest-...` reads answer 403) on this tree and
  on commit bc6ed7d5, before these changes, so the Cluster-delete reading above is the one that stands for
  claims with a VM.
- **One TTL controller in the core deletes every finished object (plan 0013 M2-TTL-CONTROLLER,
  2026-10-15).** `internal/ttlctl` runs in every core (`devcore.Run`) beside appctl. It watches `Bid`,
  `ComputeContract`, `DenoRun`, `DenoJob` and `PolicyWorkflowRun` in the root workspace of the core's kcp and
  in every child workspace that is Ready except `socialweb-provider` and `deno-provider` (a watch of the
  Workspace objects starts and stops the per-workspace watches; a resource that is not bound in a workspace is
  retried each minute). An object whose `socialweb.computer/ttl-after-finished` (Go duration or days, `8d`)
  has passed since the `lastTransitionTime` of its `Complete` or `Failed` condition with status True is
  deleted with a UID precondition; the watch schedules one timer per object, no poll, and a timer firing
  re-reads the object before it deletes. `status.ttl` of the root reports the tracked count and the
  deletions by kind. Defaults come from the creators: a Bid gets `24h` from the bidder (`--bid-ttl-sec`
  overrides, the bidder's record retention uses the same number) and the computevm controller marks it
  finished (Lost and Expired: `Complete` at `status.timings.finishedAt`, Failed: `Failed`, Won: `Complete`
  with reason `ComputeVMGone` once its ComputeVM no longer exists, or `NeverBound` after an hour with none,
  checked once a minute); this replaces the controller's old poll-and-delete of Bids. A ComputeContract gets
  `8d` from `run request` and from the node-market claim mapper (one day more than the 7 d record retention
  of M3-PDS-RECORD-RETENTION, so the requester's sweep always finds the contract) and its status carries
  `Complete` or `Failed` from the requester controller once the phase is Released or Failed. A DenoRun and a
  DenoJob get `1h` from the deno provider's creators (`newRunForJob`, `newJobForTrigger`); their own
  `ttlSecondsAfterFinished` stays and wins when shorter. Readings (market network b13a, a bidder kcp,
  TTL `20s` on objects made finished by hand): three Bids (phase Lost, `finishedAt` now), one DenoRun
  (`console.log(1)`) and one DenoJob were all gone 20 s after creation, the DenoJob's run with its owner;
  the core logged one `an object finished more than its TTL ago is deleted` line each. A real contract: a
  Bid with `--bid-ttl-sec 120` got `Complete ComputeVMGone` at 20:31:07, 25 s after its VM was released,
  and was deleted at 20:33:07. A `run request` ComputeContract read, while Held, `conditions: []`,
  owner MarketRequester `app`, annotation `8d`, and right after the release `conditions: [Complete,
  reason Released, lastTransitionTime 20:48:48]`. Under load (the bidder kcp, TTL `10s`): 300 Bids made in
  three batches of 100 and marked finished by hand were all gone 10 s after the last batch (`load bids`
  300, 200, 0 at 0, 5 and 10 s), a PolicyWorkflowRun marked Failed with TTL `20s` was gone at 15 s, and a
  ComputeContract marked Complete with TTL `15s` in a requester kcp was gone;
  no load Bid was left.
- **Object counts are capped per workspace, and a cap hit declines or waits with a reason (plan 0013
  M2-QUOTAS, 2026-10-15; the defaults were retuned on 2026-10-18, "Items closed by later work" above).** kcp enforces `ResourceQuota` for
  resources bound through an APIBinding, cluster-scoped ones included: a quota `count/bids.socialweb.computer:
  1` refused the second Bid with `exceeded quota`, and `count/denoruns.deno.computer: 1` refused the second
  DenoRun. The quota's `used` is not trustworthy, though: it is raised at admission and lowered only by a
  periodic recount, and the recount read lower than the real count (10 with 7 Bids present for 4 minutes,
  then 0 and 1 with 7 and 8 present). So each cap has two layers. The exact one is in the creator: the
  bidder takes `--bid-quota` and `--computevm-quota` from `spec.quota` (appctl passes them; `-1` leaves
  one out), counts its Bids and ComputeVMs with a list of `limit=1` and `remainingItemCount`, and its RFP
  gate answers 503 `{error: "capacity"}` and records `declinedByReason capacity` when either is at its
  limit; the Bid claim does the same count again and throws `BidCapacityError` for a push that raced it.
  The backstop is the kcp ResourceQuota `socialweb-bounded` in namespace `default` of the workspace
  (`appctl.ensureQuotas`, field manager appctl, owner reference to the root where the workspace is the
  root's): ComputeContracts (requester root, gateway `root:app`), DenoRuns and PolicyWorkflowRuns
  (`root:app`), and NodeClaims in each home workspace (`kcpclient.ensureNodeClaimQuota`, written when the
  home is made; `SOCIALWEB_GATEWAY_NODECLAIM_QUOTA` from `spec.quota.nodeClaims`). A creator that the
  backstop refuses reports it: the claim mapper leaves the NodeClaim `Pending` with an `AtCapacity` message
  and asks again, `kcpclient` returns an `AtCapacity` error for a NodeClaim, the deno provider logs the
  refused DenoRun and retries. The requester's side: when every bidder that answered the RFP answered
  `capacity`, the contract stays `Pending` with the `AtCapacity` condition and message `every bidder (N)
  declined the RFP with reason capacity; the contract asks again in 60s` and posts a new RFP after 60 s; the
  controller's own admission wait (64 contracts, 16 per account) sets the same condition. `status.quotas`
  of every root lists hard and the real count (a list count, not the ResourceQuota's `used`) per resource
  and workspace. Readings (market network b13a, bidder quota 10, ten Bids made by hand, kcp RSS from
  `ps`): a `run request` was declined (`status.market.declinedByReason [{capacity 1}]`), the Bid count stayed
  10 and `status.quotas` read `bids 10/10`; kcp RSS read 659520 KiB before and 606692 KiB after. The
  requester's contract went `Pending: every bidder (1) declined the RFP with reason capacity; the contract
  asks again in 60s` 30 s after the RFP and posted its second RFP 60 s later; in the earlier run, with a
  stale ResourceQuota and 9 Bids after a TTL deletion, that retry was declined again (the third `capacity`
  decline) because the stale `used` still read 10, which is why the exact count moved into the bidder.
- **A bidder and a requester remove their own market records when no object needs them (plan 0013
  M3-PDS-RECORD-RETENTION, 2026-10-15).** The bidder (`bid-retention.ts`, started by the bid loop after its
  restore) lists its own `market.bid` records page by page 30 s after it starts and then every 10 minutes
  (up to 50 pages and 200 removals a pass). A record is removed, with the `bids.free` payload and the bid
  config it points at, when the Bid object of its RFP is settled (`Complete` or `Failed` condition) and its
  TTL has passed, or when no Bid names the RFP and the record is older than the TTL; a record whose Bid is
  unsettled is kept, and only records of the bidder's own DID are touched. The requester
  (`requester-controller-kcp/retention.ts`, a timer of `--record-retention-scan-ms`, default 10 minutes)
  removes its own rfp, payload and accept records, those whose DID is the requester's, `--record-retention-sec`
  (default 604800, 7 d) after a contract is Released or Failed, then annotates the contract
  `socialweb.computer/records-removed`; it never touches the receipt or the vm.hostKeys and vm.delete
  records, so a later receipt check or vm.delete answer is served from the bidder's receipt and the
  ComputeVM tombstone. No application deletes a record of another DID. The deletes had never worked: the
  password, OAuth and custody agents sent `{action, collection, rkey}` to `com.atproto.repo.applyWrites`,
  which wants `$type: com.atproto.repo.applyWrites#delete`, and hono-pds answered 200 with an empty
  write list (a real PDS refuses it); all three now send `$type` (the same fix makes `update` writes land).
  Readings (market network b13a, bidder `--bid-ttl-sec 120`, five `run request` contracts released): after
  the first sweep with the fix the bidder's repo held `market.bid 0`, `bids.free 0` (before: 5 and 5) and
  `market.receipt 5`; the sweep logged `bid_records_removed scanned 5 deleted 5` (the first three sweeps,
  before the fix, logged `deleted 3`, `4` and `5` and left all five records). A requester run with
  `--record-retention-sec 1 --record-retention-scan-ms 500` logged `requester_records_removed` for its
  contract while the invocation still stood (the CLI saw the contract Released at 20:50:12 and the line is stamped 20:50:12.787). Not read: a 1000-contract run, which the
  soak (M4-SOAK-READING) reads.
- Two defects of the guest-kcp split and the Bid-held tombstones, closed together. (a) The accept bundle
  that the bidder injects into a guest named the bidder's own relay as `guest_onnetwork_url`, so the
  guest's on-network report (with its OpenBao bearer) reached the bidder's unauthenticated
  `/v1/on-network`, which wrote the PDS onNetwork record but never the ComputeVM `status.market`; the
  guest-kcp route, which verifies the bearer and records `address`, was never called. The kcp provider
  now sets `guest_onnetwork_url` to the guest endpoint it already uses for the login
  (`compute-provider-kcp` `injectAcceptBundle`). Before: ComputeVM `address` and `onNetworkUri` empty
  while the guest logged `http 200`. After: `address iroh://...` and `onNetworkUri` set, identical across
  kill -9 of the bidder, `dev restart` of the bidder DenoPod and kill -9 of the bidder core.
  (b) A restarted bidder loads tombstones from the Bid `status.terminated`; a replayed accept of an
  ended contract then matched `acceptToContract`, passed `checkAccept` as known, and the bidder answered
  `alreadyAccepted` 200 for a contract that had ended. `checkAccept` now answers 409 `ContractEnded`
  (`accept_contract_ended`) when the known accept's receipt is terminated. No VM was started in either case.
- The gateway `requester` DenoPod was OOM-killed at its 1 GiB limit because policy evaluations were not
  bounded. `compute-request-xrpc` ran the scope lane of the attached policy for every discovered bidder at
  once (`Promise.allSettled(filteredBidderDids.map(scopeGate))`, 448 relay offerings on prod), and each
  `GhaLiteExecutor` run starts net-only Worker isolates of about 25 to 30 MB each. Peak RSS grew with the
  number of candidates, not with the work: 8 concurrent scope runs 291 MB, 40 concurrent 1122 MB (the
  evaluator and the real `requester-tangled-vouch` workflow, stub trust resolvers). Each run also re-read
  the requester's own `badgeBlueKeys` and `vouch` collections over the network, so hundreds of
  `policy_public_records_failed` lines followed. The settlement: `createConcurrencyLimiter` and
  `mapWithConcurrency` in `policy-common` (4 at a time, `DEFAULT_POLICY_CONCURRENCY`); the policy evaluator
  limits `scope` runs and full evaluates, and `GhaLiteExecutor` limits workflow runs; the scope prewarm
  covers the vouched and the extra bidders only (a relay-discovered bidder is scoped when its bid arrives,
  and the scope cache dedupes that); `listRecordsForPolicy` shares one in-flight read per repo and
  collection for 15 s. The gateway requester service values (`requester.prod.values.yaml`) raise
  `memory` to 2Gi as headroom. Reading, in a market network with a gateway, a policy attached through
  `node-policy` and 448 synthetic extra bidders on top of the 2 real ones (450 candidates, the dev backend
  enforces no limit, so peak is VmHWM of the requester process): before 2479 MB with `tangled-vouch`;
  after 366 MB with `tangled-vouch` (the claim fails as before with `winner rejected by policy: not
  vouched`, since the network's bidders are not vouched) and 372 MB with `open`, where two NodeClaims won,
  two market RFP VMs joined and both pods ran (first NodeClaim +19.3 s, both pods Ready +67.8 s). The same
  network without a policy or extra bidders peaked at 197 MB. Not read: the prod gateway, which the lead
  deploys.

- **What kcp costs the market loop's hot path, read live (plan 0009 M4-LATENCY-READINGS, 2026-10-15).**
  The market loop runs in kcp; the bidder waits on kcp twice before it publishes a bid: one `get` in
  the RFP gate (`bidLoop.rfpGate`, is there a Bid for this RFP) and one `createIfAbsent` (`ledger.claim`,
  the Bid). The `published` patch is not awaited. Conditions of every number: a dev build of the tree at
  bc6ed7d5 (`scripts/build-socialweb-computer.sh --dev --publish`, TypeScript run live from the
  mirror), hat-8 (16 cores, 47 GiB), market network `dev up --profile market-network --bidders 1
  --with-gateway` as test-0001, fresh network per series, guest image at
  `/var/lib/socialweb-computer/guest-image`. The host is shared: other agents' builds and networks ran
  throughout (load average 1/5/15 min is given per series), and the live slot was held. Warm: every
  series ran after the network had served other RFPs; the first 8 RFPs after a bidder restart are
  dropped. p50/p95 in ms; max in brackets where it differs.
  1. kcp operation latency (`kcpbench.ts` against the instance's admin kubeconfig, the real
     `createKcpBidLedger` and `kcpResourceClient`, 300 sequential iterations after 10 warm-up, three
     runs each, load 4 to 5). Bidder kcp, kine on sqlite, the Bid ledger: `get` miss 0.54-0.57 /
     0.73-0.89, `createIfAbsent` 1.21-1.29 / 1.76-1.96, `patchStatus` 2.13-2.27 / 2.96-3.62, `get` hit
     0.70-0.72 / 0.92-1.10 (p99 of the create 2.5-8.4). The same bidder kcp with a ConfigMap, create
     0.95-1.00 / 1.33-1.48, merge patch 1.05-1.10 / 1.55-2.04. A kcp on kine over postgres
     (`dev up --profile prodlike`, the gateway's store; the Bid kind is not applied there, so a
     ConfigMap): `get` miss 0.59-0.63 / 0.74-1.07, create 1.60-1.77 / 1.96-2.90, merge patch
     1.67-1.82 / 2.00-2.87, `get` hit 0.57-0.60 / 0.69-1.05. In this network the gateway child's own kcp
     is on sqlite (`table.Dev(..., StoreSqlite)`); the postgres figures are the prodlike instance's, a
     reading of the engine and not of the prod gateway, which was not read.
  2. kcp in the RFP path, in the bidder process (log lines `bid_gate_find` and `bid_claim`, each with
     `ms`, written around the two awaits; temporary, in the mirror of the working tree only, not
     committed, with `rfpUri` added to `bidder created bid config` and `bidder created VM bid` and a
     `policy_eval` line around the attached-policy loop). RFPs came from 108 ComputeContracts per
     block created at 4 s spacing in the gateway's requester (policy `bid-payload` attached, so every
     RFP carries a policy record; the requester's `denyBidderDids` keeps the bid from being accepted,
     so no guest is born; the first 8 dropped, 100 RFPs per block).
     Block V1 (kcp on, policy evaluated in the PolicyEngine DenoPod; load 8.9 / 20.0 / 21.4): gate
     `get` 1.7 / 2.1 (2.4), claim 2.7 / 3.9 (12.2), kcp wait on the path 4.5 / 5.7 (14.1);
     `rfp watch discovered -> bidder created VM bid` 769 / 981 (1758); `dispatch: resolving rfp ->
     bid` 767 / 979. Block V3 (kcp on, policy evaluated in the bidder's own net-only Worker; load 3.4 /
     7.4 / 14.7): gate 1.7 / 2.2 (2.6), claim 2.7 / 4.0 (27.5), kcp wait 4.5 / 5.8 (29.1); seen to bid
     1197 / 1372 (2390). 200 RFPs in all, one block each. The requester's own `rfp_created ->
     firehose_bid_discovered` was 794 / 1032 in V1 and 1257 / 1448 in V3, and the bidder saw the RFP
     10 / 21 ms (V1) and 25 / 60 ms (V3) after the requester created it. Of the 769 ms p50 in V1,
     607 ms is `bidder received VM RFP -> bidder created bid config` (the policy evaluation, 502 / 549,
     the claim and the PDS write of the bid config) and 152 ms the two PDS writes of the payload and
     the signed bid; the kcp wait is 0.6 percent of the span.
  3. Before and after. The pre-plan commit was not rebuilt: its requester ran one contract per
     DenoPod, so 200 RFPs through it would be 200 cold kcp starts, and the comparison asked is what kcp
     adds. The bidder with its Bid ledger switched off (`bids: undefined` in the mirror of
     `market-bidder/mod.ts`, then `dev restart` of the bidder DenoPod; no gate lookup, no Bid) is the
     "before" of the hot path. Block V2 (ledger off, engine; the load rose from 7 to 35 during the
     block): seen to bid 762 / 3503 (4276). The p95 is the host's: the same block's policy evaluation,
     which has no kcp in it, went to 991 p95 and 2569 max. Subtracting the in-situ kcp wait from
     each RFP of V1 is the counterfactual without any host difference: 764 p50 and about 975 p95. kcp adds 4.5 ms p50 and 5.7 ms p95 to `rfp seen -> bid published` over
     the 200 RFPs of V1 and V3 (the larger max of V3 is one claim of 27.5 ms), against the 20 ms
     budget: within it.
  4. The budget does not hold on a starved host. Two further V1 blocks ran at load 35 (1/5/15 min
     35.8 / 18.2 / 16.9 and 35.4 / 22.6 / 18.8): the kcp wait was 5.6 / 24.1 (33.5) and 4.9 / 26.9
     (42.7), and `rfp seen -> bid published` queued past 5 minutes because the bidder could not keep
     up with one RFP per 4 s (policy evaluation 1.7 s and 3.4 s p50). Earlier series at 1 RFP per
     second and at 1 per 2.5 s, at load 30 to 57, queued to 27 s and 82 s the same way. Opened as a section 14 item;
     the fallback is not built because the two quiet blocks are within budget and the loaded ones
     show the process starved, not kcp slow (item 1 measured the same client at 0.7 ms p95 for a
     `get`). No block with the ledger off was usable as a second reading at load 35.
  5. A verdict miss, in process versus through the engine, on the RFP's attached policy (`policy_eval`,
     n 100 each): through the PolicyEngine DenoPod 502 / 549 (1455), in the bidder's net-only Worker
     542 / 579 (1719). The engine is not slower; both are dominated by resolving the policy record from
     the PDS and running the workflow, and the local hop does not show against that. The scope lane
     (`dispatch: running scope filter -> scope filter done`) was 0 / 1 ms on a cache hit and 97 ms
     on one RFP (the block's maximum, taken to be the one verdict miss after the restart; the bidder's
     own scope policy runs in the bidder, so a miss is in process). Not isolated: the engine's own task time
     separate from the PDS read, and the engine under concurrent load.
  6. The loop around it (3 concurrent ComputeContracts in the gateway's requester, no attached
     policy, guest image booted by the bidder's systemd-user VM backend; offsets from the start of
     the contract, ms, range of the 6 guests that booted; load 14.6 to 29.3): vm record 63-345, RFP
     record created 171-736, RFP delivered to the bidder 966-3563 (the requester's own PDS writes
     queue behind each other), bid seen 940-3613, winner 1254-3831, accept record 1406-4028, accept
     answered with the receipt 2074-5087 (accept to receipt 650-1490), receipt verified 2156-5175,
     guest on-network event 12322-20452 (receipt to on-network 7700-18100), host keys reported
     23780-33929, iroh ticket 28090-36734, contract Held 28208-36826 (receipt to Held 23600-34500).
     Of 9 contracts in 3 rounds, the 3 of one round ended at about 23 s: `the bidder ended the
     contract: ComputeVM ... was not Running` (the guest did not come up while the load was 29 and
     fell to 12). One `run request --network` (a kcp of its own, 24.2 s cold): Pending to the RFP
     submitted 1 s, receipt 1 s later, host keys 19 s after that, Held 23 s after Pending, the command
     ran, `vm.delete` accepted 200, the invocation's kcp stopped 12 s after the release.
  7. Guest identity, in that guest (`guest-login.sh` run through `--exec`; the guest booted at
     22:50:36, `guest-login.service` bound the ssh host key at 22:50:48 and logged in a second later):
     a login (challenge, signature, login) 73-88 ms over 10 calls; a token for the audience
     `api://guest-kcp?actx=<computevm>` through guest-kcp 71-168 ms, median 96, over 25 of 30 calls;
     5 of the 30 took 4.10 to 4.16 s, the 4 s the script sleeps after a reply that is not 200 (the
     reply was not captured; guest-kcp has a token bucket per role, which is the suspect). A call to
     a closed local port from the same shell is 5 ms.
  8. Not read: the prod bidder and gateway (lead only); the pre-plan commit; the engine hop apart
     from the policy; postgres with the Bid kind; more than one bidder. The ledger-off block ran after
     a bidder restart that replays the stream from its stored cursor; with the ledger off nothing
     dedups the replay, so each restart of that variant re-bid old RFPs for up to 112 s before the
     block began (the block waited for the log to go quiet).
  9. The lasting instrument (plan 0009 ACC9-LATENCY, 2026-10-17). `status.market` of a MarketBidder
     (and of the other kinds that carry `MarketLoopStatus`) holds `rfpToBid` and `kcpWait`: `count`,
     `p50Us`, `p95Us`, `maxUs` over the last 256 RFPs that reached a published bid, written with the
     counters (integer microseconds: the generated validator takes integers only). `rfpToBid` runs
     from the RFP's entry into the bidder's gate (`bidLoop.rfpGate`) to the call of
     `ledger.published`; `kcpWait` is the time the bidder waited on kcp in that span: the gate
     `find`, the quota `capacity` check, `claim`, and (until item 10) the `published` status patch (the
     patch is not awaited by the path, but its time was counted). Read it with `kubectl get marketbidder app -o
     jsonpath='{.status.market.kcpWait}'` on the bidder's kcp. The ring is not stored; a restart of
     the bidder empties it. Reading, dev-linux-1 (c-48), commit 56569f26, one bidder market network
     (`dev up --profile market-network --bidders 1`), 20 RFPs each from `run request --skip-ssh`
     (guests booted and released): 4 requests at a time, load 3.6: `kcpWait` 29.5 / 51.3 (84.2) ms
     p50 / p95 (max), `rfpToBid` 521 / 915 (1579); one at a time after a bidder restart, load 1.6 to
     8: `kcpWait` 31.0 / 40.5 (44.4), `rfpToBid` 786 / 1502 (1515). The bidder log span `dispatch:
     resolving rfp -> bidder created VM bid` over the same 20 RFPs was 778 / 1486 (1507), against
     `rfpToBid` 786 / 1502 (1515): the instrument and the log agree. A temporary per-call timer
     (mirror only, 8 more RFPs) split the `kcpWait` p50 of 29.2 ms into `find` 3.6, `capacity` 4.4,
     `claim` 12.7 (a `get`, a fresh quota count of Bids and ComputeVMs, the create) and `published`
     8.4. The calls the path awaits sum to about 21 ms p50; each kcp call on this droplet is 3 to 4
     ms, against 0.5 to 1 ms in the earlier reading. So ACC9-LATENCY is not met on this host and the
     check stays todo.
  10. Fewer awaited kcp round trips on the hot path (plan 0009 ACC9-LATENCY, 2026-10-17, commit
     e9630cc6; before = d1fc3531). Three changes in `bid-loop.ts` and `market-bidder-bids-kcp`. The
     gate reads the Bid (`find`) and the quota counts (`capacity`) at the same time, and the two counts
     (Bids, ComputeVMs) are read at the same time; the sum of the awaited gate time is counted once,
     not once per call. The counts are kept for the quota cache window (2 s) and a created Bid raises
     the kept Bid count; `claim` reads the kept counts (a recount only when the window ended while the
     policy ran) and goes straight to the `createIfAbsent`, whose 409 already answers an existing Bid
     with the one `get` it needs. `claim` calls `get` first only when a quota is full, to tell an
     existing Bid (returned, not refused) from a new one (refused). The create is still the atomic
     claim: one POST with the Bid name, a conflict means another process holds it. The `published`
     patch is not awaited and not in `kcpWait` any more (item 9 counted it; `rfpToBid` and `kcpWait`
     now both stop at the call of `ledger.published`). The exact-count rule of M2-QUOTAS changes in one
     way: the count `claim` checks is at most 2 s old (and includes this bidder's own new Bids), where
     it was read fresh; the kcp ResourceQuota backstop is unchanged.
     Conditions: dev-linux-1 (c-48), host load 1 to 4 throughout, one bidder market network per series
     (a fresh network each, the fake PDS runs out of heap after about 30 `run request`), 20 RFPs each
     from `run request --skip-ssh`, bidder restarted before the series, all RFPs counted (no
     warm-up dropped); the per-call timer is a temporary log line in the mirror's `kcp-client-rest`
     (not committed), the same one for before and after. `kcpWait` p50 / p95 (max) ms:
     before, 4 at a time 30.3 / 42.3 (44.0), serial 29.8 / 93.0 (93.0, 19 RFPs: the first request
     hung before its RFP, see below); after, serial 12.3 / 15.8 (17.4), 4 at a time 11.9 / 25.7 (40.1)
     and, on a second fresh network, 10.9 / 13.5 (13.9). The 25.7 series has a 34 ms `list` and a 20 ms
     create in it; the same code with the kine file on tmpfs, 4 at a time: 11.4 / 13.2 (15.8). `rfpToBid`
     p50 stayed 500 to 550 ms (the policy evaluation and the PDS writes). Per-call p50 / p95 ms, disk,
     before (serial): `get` of the Bid 2.7 / 3.7, `list` limit=1 2.3 / 2.7 (Bids) and 1.9 / 2.3
     (ComputeVMs), create 5.2 / 10.4, `published` = `get` 2.7 + patch 5.1 / 45.8; after (serial): `get`
     2.6 / 4.9, `list` 4.1 / 4.8 and 3.8 / 5.0 (slower when two run at once; the pair costs the
     slower one), create 6.8 / 10.4. Awaited path before: 2.7 + 2.3 + 1.9 + 2.7 + 2.3 + 1.9 + 5.2 = 19
     ms of calls in 7 round trips plus the counted patch; after: max(2.6, 4.1) + 6.8 in 2 steps. Disk
     effect (kine sqlite on tmpfs against the default disk, same code, 4 at a time): the median of each
     call is the same on both (`get` 2.8 and 2.8, create 5.7 and 5.9), so the 3 to 4 ms per call is
     not storage; the tail is: single calls of 20 to 70 ms (a `list` 34, a `get` 30, a status patch 47)
     appear on disk, and no awaited call of the tmpfs series was over 13.5 ms. The status patch of 46 to 48 ms at p95
     is on tmpfs as well (n 80 each), so that one is not the disk; it is off the awaited path. The
     production durability settings are unchanged. Not isolated: whether the 25.7 series was the
     droplet's disk (guests of earlier requests booting at the same time) or chance; the second disk
     series at 13.5 says the tail is rare, and the prod reading (256 RFPs) decides. A load driver
     lesson: a `run request` that hangs before its RFP printed nothing for 28 minutes (ssh to the
     droplet stalled once, also seen on a `dev restart` right after `dev up`); the driver now kills a
     request at 240 s and prints a line per request. Decision: ACC9-LATENCY is done on this reading
     (p95 15.8, 13.5, 13.2 against 20, three of four series). The lead reads the prod bidder after
     the deploy; a prod p95 over 20 ms with host load under 16 reopens it, and the next step is
     the Bid create beside the bid config and payload PDS writes (they take about 150 ms), not a fallback
     to the market.bid record key.
  The "3 live slots" were `SWC_LIVE_SLOTS` in the lead's
  `~/.tmp/swc-lead/live-slot.ts`, not hardware. Readings on hat-8 (Ryzen 7 7700X, 16 threads, 46 GB, load
  average 20 to 29 from other work) and on a DigitalOcean `c-16-intel` droplet (16 vCPU Xeon 8358, 32 GB,
  /dev/kvm present, `kvm_intel nested=Y`; a `c-16` Xeon 8168 droplet has it too), one `1 bidder` market
  network each, the same binary and guest image: `dev up` 80 s on hat-8 and 85 s on the droplet (the droplet
  was idle: the bring-up is wall-clock bound, about 1 core on average, kcp start 10 s, root object applied
  at 17 s, services ready at 60 s, the bidder Ready at 80 s); `run request` 80 s on hat-8 and 86 s on the
  droplet (requester kcp and RFP 35 s, guest boot to on-network 26 to 31 s, release 10 s); `dev down` 20 s;
  `TestHarnessEmptyNetworkUpAndDown` 121 s and `TestOneAcceptMakesOneComputeVM` 203 s on the droplet,
  through the compiled test binary (`scripts/dev-droplet.sh it`). A new worktree: `go build
  ./cmd/socialweb-computer` 3.9 s, `deno task check` 3.4 s, `build-socialweb-computer.sh --dev --publish`
  21.4 s (closure 18 s, the one per-worktree cold step) and 6.5 s after. Footprint of one network with one
  bidder: 28 processes, 2.9 GB summed RSS (about 2.2 GB used), 0.2 core idle, 50 to 85 core-seconds to bring
  up; with `--with-gateway` 41 processes and 4.5 GB RSS. A `run request` adds a kcp for the invocation
  (1.3 GB, 45 core-seconds). A guest: firecracker RSS 400 MB, cgroup memory.current 1.7 GB of which 1.2 GB
  is the rootfs page cache, memory.max 2.35 GB, and one core at 100 % while the guest is idle (hat-8: 738 s
  of CPU in 733 s; droplet: 1001 ticks in 10 s; KVM `halt_exits` 70000 per second with `halt_wakeup` 1, the
  guest top reads 90 % idle). Concurrency on the 16 vCPU droplet: four networks brought up at once took 81
  to 90 s each (one alone: 85 s); five networks with five concurrent `run request` flows peaked at 21 GB
  RSS, 17 GB MemAvailable left, CPU 73 % mean and 95 % peak, and each flow took 99 to 118 s (one alone: 86
  s). Seven networks fit in 32 GB; CPU, not RAM, is the limit, at about five full flows per 16 threads
  before the flows stretch. A modified `image.json` (boot args) is refused by the bidder ("the image is
  refused"), so the idle-vCPU defect needed a rebuilt, re-pinned guest image (closed in the next entry). Decision: the
  default of `SWC_LIVE_SLOTS` is 5, and `acquire` also waits while MemAvailable is under 12000 MB
  (`SWC_LIVE_MIN_AVAIL_MB`) or cpu pressure avg10 is over 90 (`SWC_LIVE_MAX_CPU_PSI`); a droplet is a
  separate pool with no slot. The VM account test-0001 has 16 VM slots (a 1048576-id range in /etc/subuid, 16 x
  65536), shared by every concurrent network, so five slots fit while each uses at most three guests; test-0002
  has 4 slots. More than 16 concurrent guests on this host need another account with its own range.
  Release runner on a second host, the one asset that follows the compiler: `rust/build.sh x86_64` (from
  commit 917f06bb, clang 22.1.8) read sha256 a6f31ac8...5c92 on hat-8 (Fedora clang 22.1.8-4.fc44, binutils
  2.46.1-1.fc44), a6f31ac8...5c92 on a DigitalOcean Fedora 44 droplet with the same two package versions,
  and 143e6e8e...7a03 on an Ubuntu 24.04 droplet with apt.llvm.org clang 22.1.8. The same clang version number
  from another packager is not enough; only the same distribution packages reproduce the bytes. The other
  release assets and the Go closure were not built on the droplet, so no second release runner is proposed
  or registered; a Fedora 44 host would be the candidate and needs `make check-pins` to reproduce the
  pinned assets first.
- **The shared Linux dev VM dev-linux-1 (2026-10-17).** `scripts/dev-droplet.sh create/setup/sync/build/swc` from
  a Mac made one Ubuntu 24.04 droplet that builds and runs live (Go and Deno at the pinned versions, rustup, clang,
  podman, passt; user `dev` builds, test-0001 runs). Readings on `c-16` (nyc1, $0.50/h, the largest size that
  region had) and `c-48` (sfo2, $1.50/h, 48 vCPU, 96 GB; `c-60-intel` and every `*-48-intel`/`c2-` size
  were "not available" in every region tried), same binary and the pinned guest image from
  `main/latest/guest-image`: cold `go build ./cmd/socialweb-computer` 43.5 s on c-16 and 28.9 s on c-48;
  `deno task check` 8.6 s and 8.2 s; first `--dev --publish` build with empty build root 205 s (c-16) and 354 s
  (c-48, rustup, podman builder image and downloads also cold), a new worktree 28 s (c-48), a rebuild 7.4 s and
  7.0 s; `dev up` (1 bidder) 91 s and 85 s; `run request` 130 s (c-16, includes the one-time guest image fetch)
  and 82 s; `dev down` 20 s. Six networks brought up 4 s apart, six `run request` flows at once and three cold
  `go build` runs beside them on c-48: `dev up` 78 to 92 s, `run request` 88 to 107 s, `go build` 51 s, `dev down`
  19 to 22 s, load average 46 at the peak and 94 GB RAM untouched. Decision: the one droplet is c-48. The size is
  3x the price of c-16 for 1.5x the compile speed alone, and about 6x the concurrent live flows; the per-proof time
  does not shrink (it is wall-clock bound). The meshd bytes of the `socialweb-computer-k8s-downloads` asset follow
  the compiler packages, so on Ubuntu the pin check fails; the dev build stages that asset unpinned off Fedora.
  Launching six `dev-droplet.sh swc` ssh sessions in the same second hung every client; start them 4 s apart.
- **A firecracker guest burned one host core while idle (2026-10-15).** Cause: the guest kernel config had
  `# CONFIG_X86_MPPARSE is not set` and the boot args carry `acpi=off`. With ACPI off and no MP table parser
  the kernel found no APIC: the guest console read `ACPI: MPS support code is not built-in`, `No local APIC
  present`, `APIC: Keep in PIC mode(8259)`, `smpboot: SMP disabled`; `/proc/cpuinfo` had no `apic` flag, the
  guest saw 1 CPU although the manifest gives it 2 vCPUs, every irq in `/proc/interrupts` was `XT-PIC`, `LOC`
  stayed 0 and the tick device was the PIT (`pit_next_event`). The guest was truly idle (about 100 timer
  interrupts and 50 interrupts per second in total, `vmstat` 99 % idle), yet the host vCPU thread `fc_vcpu 0`
  had utime 0 and stime at 100 %, 4 voluntary context switches per second, so it never blocked in the host. The
  KVM exit path was not read (kvm debugfs needs root, which hat-8 does not give); the A/B below
  demonstrates the cause. Reading on hat-8 (Ryzen 7 7700X, host
  kernel 7.2.9, firecracker v1.13.1, one held guest of a 1 bidder market network, a 60 s window of
  `/proc/<pid>/task/*/stat` after the guest booted): 59.86 CPU seconds in 60 s and 59.85 for a second guest;
  with `lapic` added to the boot args 58.56 (the kernel ignores it: the CPUID APIC bit is already gone).
  Fix: `CONFIG_X86_MPPARSE=y` in `deploy/guest-kernel/socialweb.config`, resolved into
  `firecracker-x86_64.config` (one line), with the boot args unchanged. The kernel now logs `found SMP
  MP-table`, `IOAPIC[0]`, `x2apic enabled`, `Brought up 1 node, 2 CPUs`, irqs are `IO-APIC`, and a held idle
  guest read 0.12 CPU seconds in 60 s (`fc_vcpu 0` 0.06, `fc_vcpu 1` 0.05, 15 voluntary switches per second per
  vCPU thread). The guest joined through `run request --network` (the cloud-init `user_data` ran the probe over
  ssh through the relay) and `run request release` ended it. Only `vmlinux` and `image.json` changed; the
  rootfs pin is the same. The aarch64 config is not touched (MP tables are x86 only). A guest now sees its 2 vCPUs, where it saw 1 before. The bidder refuses a modified image, so the
  published `main/<commit>/guest-image` of the first CI build with this commit carries the fix; a bidder that
  pins an older `versions.json` keeps the old image and the old cost.
- A 429 from the PDS front proxy ended a requester contract Failed, and the retry storm that followed kept
  the rate limit from clearing. Prod (build 66d9e9ff) logged `the contract flow threw:
  com.atproto.repo.createRecord failed: 429 <html>...` one second after `postingRfp`
  (`atproto-session-custody-openbao` `expectOk`); bsky.social allows 3000 requests per 300 s per IP
  (`ratelimit-policy: 3000;w=300`). Three settlements. (a) Every PDS and relay request now goes through
  `fetchWithRetry` (`ts/typescript-helpers/lib/http-retry-fetch`, with the pure policy in
  `lib/common/http-retry-common`): 429, 408, 425, 500, 502, 503, 504 and network errors are retried;
  `Retry-After` (seconds or date) and `ratelimit-reset` (seconds, or an epoch) are honored plus 250 ms;
  without a header the wait is 1 s doubling to 30 s with 25 percent jitter; the total wait is 90 s and
  the attempts are 8; a header that asks for more than the remaining wait ends at once. A DPoP proof is
  made again for each attempt. It wraps the custody fetch handler, the OAuth DPoP fetch, the password
  session agent, the `@atproto/api` credential sessions and the public reads of `market-atproto`
  (`publicGet`); the error text names the attempts and the wait. (b) The back-off of the scale pass
  already counts every `Failed` or `Lost` claim end (`scaledriver.Driver.Declined`); its cap is now 5 m
  (15 s, 30 s, 1 m, 2 m, 4 m, 5 m) and the provider writes it into the Cluster status as condition
  `NodeProvisionBackoff` (reason `MarketProvisionsFailing`, message with the count, the time before which
  no attempt is made and the last end), set by `clusterprovider.Provider.SetNodeBackoff` from the scale
  pass and carried by the next reconcile; it is removed when a node joins or the intent drops. (c)
  `compute-request-xrpc` keeps the relay discovery (180 s, only a non-empty answer), each bidder's offering
  list (120 s, only a non-empty list) and DID documents (stale after 60 s, gone after 300 s) in
  process-wide `TtlMemo` caches (`lib/common/ttl-memo-common`), shared by concurrent contracts. A service
  auth token is not reused: it is the one request per offering that stays.
  Readings (market network with a gateway and one bidder, hono-pds answers 429 to `market.rfp` and
  `market.accept` writes): six 429 with `Retry-After: 2` -> the requester retried at +0, 2.25, 4.5, 6.75,
  9.0 and 11.25 s, the seventh write landed, the claim went Bidding, Provisioning, Joining, Running and the
  pod was Ready 80 s after it was created (66 s in the first run; before: the claim was Failed at the first
  429). Every write 429 with no header -> `gave up: 8 attempts used, the limit is 8` after 90 s of waiting,
  claim Failed; the next contracts started 22 s, 36 s and 63 s after each end (15 s, 30 s, 60 s plus a
  pass), and the Cluster showed `NodeProvisionBackoff=True ... the next attempt is not made before
  23:43:07Z` and, after the second end, `2 market provision(s) in a row ... not before 23:45:11Z`. Request
  count of one contract's discovery for 443 relay offerings (a stub relay, PLC and PDS counting
  requests): before 887 reads (1 relay, 443 DID documents, 443 offering lists), after 887 on the first
  contract and 0 on the next ones inside the cache window. Not changed: the 443 service-auth requests to
  the requester's PDS and the 443 `submitRfp` posts that one contract makes to the bidders it found; an
  RFP to every relay-discovered bidder stays 443 requests of bsky.social per contract, so a fan-out cap is
  the next lever and a market decision. Not read: the prod gateway, which the lead deploys.

- **A bidder is online while its heartbeat is fresh; a requester drops every other discovered DID (plan
  0017 M1-STALE-BIDDER-RECORDS, 2026-10-18).** Before: an offering record stayed in the relay index after
  its bidder was gone, and the requester sent a submitRfp to every DID it found (prod: 448 per contract).
  Now: the bidder (`market-bidder`) rewrites one record `com.publicdomainrelay.temp.market.heartbeat`
  (key `literal:self`, `{offering: at-uri of its offering, createdAt}`) in its own repo PDS (plan 0018)
  right after its offering is ensured and then every 60 s (write class `sweep`). `compute-request-xrpc`
  keeps, per DID, the heartbeat time and the time it was read, process-wide: a DID is fresh while
  `createdAt` is at most 5 min old (a `createdAt` more than 60 s in the future counts as none); a DID that
  is not fresh is read again by `getRecord` from its PDS (5 s timeout, all DIDs at once) at most once per
  60 s; a heartbeat event on the event streams triggers that `getRecord` at once (15 s debounce), the way
  an AtprotoRecordWatch verifies. The filter applies to the union of every source (relay
  `listReposByCollection`, the offering watch, vouched and operated DIDs, `bidderDids`) after the deny list;
  the scope prewarm takes only fresh DIDs. `bidder_discovery` logs `found`, `fresh`, `stale` and
  `heartbeatTtlMs`; `total` is `fresh`, the DIDs that get a direct submitRfp. The market.rfp record is
  posted as before, so a bidder that watches the firehose still sees it. The market network relay
  (`hono-atproto-relay`) is a plain atproto relay and does not filter its `listReposByCollection`; the prod
  relay relay.mini-cloud-0002 is not in this repository. Readings (droplet dev-hb-1, market network with 2
  bidders and the gateway): run request `found 2, fresh 2, stale 0`, RFP to both, exec ok; bidder-2
  stopped (MarketNetwork `spec.apps[].stopped`) at 22:55:32: a new run request 43 s later `found 2, fresh
  1, stale 1` and one submitting_rfp (bidder-1), exec ok; the gateway's requester at +33 s still `fresh 2`
  (its cached heartbeat was inside the TTL), at +5 min 39 s `fresh 1, stale 3` (the 2 vouched operator
  accounts have no heartbeat) and one submitting_rfp, node 3 Ready. Restart: the gateway read bidder-2
  stale at 23:19:54, bidder-2 started at 23:19:59 and wrote its first heartbeat at 23:20:20, and the
  gateway's next contract at 23:20:26 had `fresh 2` (the event path; the 60 s re-read was not due) and
  posted to both. Bidder-1's repo over 33 idle minutes with the heartbeat: repo_block_gc live blocks 148
  and 62389 bytes from the third pass on, about 37 blocks removed per 5 min pass, `repo.sqlite3` 344064
  bytes and its WAL 4165352 bytes unchanged for the last 23 minutes.

- **kubectl uses a client certificate and nothing else; the exec-plugin path is deleted (plans 0015 M3-DOCS,
  M4-INTEGRATION-TEST, M5-RETIRE-EXEC-PLUGIN and 0014 M3-RETIRE-EXEC-PLUGIN, 2026-10-16).** Caddy and
  kcp's mount proxy both end TLS, so a client certificate never reaches k3s, and k3s checks no CRL; the
  gateway therefore runs a client certificate front (`internal/clientcertfront`) that requires a TLS
  client certificate, verifies it against the cluster's client CA, the PKI CRL and the deny record, and
  reverse-proxies to the cluster's apiserver front with the proxy client pair and X-Remote-User and
  X-Remote-Group headers that it alone writes. Certificates come from the role `kubeconfig-user` of the
  cluster's own OpenBao PKI, issued with a gateway-only vault client that may issue and revoke nothing
  else; a certificate with the organizational unit `socialweb.computer:no-expiry` may outlive 24 h and is
  ended only by revocation, cluster delete or CA expiry. Prod reading (the lead, build 61af89f9): plain
  `kubectl` through `kcp.socialweb.computer` with the kubeconfig of `getKubeconfig` works, revoke refuses
  the old certificate in 0.7 s, and a Job ran on a bobvmbuilder market node. Deleted: `cmd/socialweb-credential`,
  `internal/execplugin`, `internal/devicekey`, `internal/mint`, `internal/tenantclient`, the `/v1/credential`
  route, the device-key registry, the `gwmint` ServiceAccount, Role, RoleBinding and the `serviceaccounts/token`
  arm of the `socialweb-mint-identity` policy, the mint kubeconfig directory and its `Secret`, the device-key
  secret and policy of the root vault layout, the four `socialweb-credential-<os>-<arch>` downloads (the
  `socialweb-computer-k8s-downloads` tree is now 17227819 bytes, tree sha256 01030c9a...), the helper list of
  the web app and its help text. The gateway environment keys `SOCIALWEB_GATEWAY_MINT_TTL`,
  `_MINT_FAILURE_HOLD`, `_MINT_KUBECONFIGS`, `_DEVKEY_REGISTRY`, `_DEVICE_KEY_AUDIENCE`, `_DEVICE_KEY_TTL`,
  `_DEVICE_KEY_MAX_TTL`, `_DEVICE_KEY_EXEC_COMMAND`, `_DEVICE_KEY_EXEC_ENV_NAME`, `_PUBLIC_MINT_URL` and
  `SOCIALWEB_CLUSTERPROVIDER_TOKEN_TIMEOUT` are no longer read; an unknown environment key is ignored, so an
  env file that still sets them starts the gateway. One consumer was outside the exec path: `tenantapply`
  (`kubectlApplyServerSide`, `getJob`) took its owner credential from `tenantclient.OwnerConfig`, a
  `TokenRequest` for the owner ServiceAccount through the `gwmint` token. It now signs a 10 minute client
  certificate (CN the owner's DID, O `socialweb.computer:cluster-owners`) per call from the cluster's client CA
  in the cluster state dir (`gatewaymain/ownerconfig.go`) and dials the apiserver front that `front.json`
  names, so the `socialweb-owner` ClusterRoleBinding of the group is the only authorization and no token is
  minted. Readings (hat-8, `TestKubeconfigClientCertificateWithoutCredentialHelper`, one market network with a
  gateway and no bidders, the dev build of the working tree): before the deletion 159.7 s (dev up 84.7 s, dev
  down 24.2 s), after it 144.2 s (dev up 81.1 s, dev down 20.1 s). The test reads `owner-sessions.json`, does
  `createSession` and `getServiceAuth` at the owner's PDS, calls `getKubeconfig` with a service JWT (200,
  `Cache-Control: no-store`, a plain client certificate and key, CN the DID, O the owners group, 3600 s
  lifetime; `ttlSeconds` 59 and 86401 are 400, a JWT for another lxm is 401), runs `kubectl get nodes` with a
  PATH that holds no `socialweb-credential` and `Nodes().List` through client-go, applies a ConfigMap through
  `kubectlApplyServerSide` (the new owner credential), calls `revokeIdentity` (the certificate was refused
  0.43 s after it before the deletion, 0.18 s after it), and finds no `PRIVATE KEY` or `client-key-data` in the
  gateway logs and DenoPod run directories. A first run failed because it asked kubectl before the tenant
  grant (`ClusterRoleBinding socialweb-owner`) was applied (Forbidden for the user with no group): the cluster
  answers `/readyz` before the grant, so the test waits for the binding.

- **The web app sign-in is an integration test: the plugin sign-in, nodes by RFP, a web app restart that keeps the
  cookie, and a downloaded certificate that revocation ends (plan 0014 M4-INTEGRATION-TESTS, first part, 2026-10-16).**
  `TestWebAppSignInThroughThePluginKeepsTheCookieAcrossARestartAndIssuesACertificate` opens a loopback listener
  first and passes `https://127.0.0.1:<its port>` as the gateway option `public-origin`, because the plugin
  config and the client_id exist only when that option is set. After `dev up` (one gateway, one bidder) it
  terminates TLS on that listener with the leaf (`tls.crt`, `tls.key`, SAN 127.0.0.1) of the alice PDS DenoPod,
  found by the port its `listeners.json` reports, and forwards to the gateway's `web-app-front` listener, which
  is a plain TCP front in front of the web app pod and keeps its address across a restart. The PDS fetches
  the client metadata over that origin, so the leaf must chain to the CA that the PDS DenoPod trusts (its run
  directory `ca.pem`); a leaf signed with the network's guest CA was refused (`invalid_client_metadata: could
  not resolve client metadata`) because `ca.pem` holds the kcp-mesh-root PKI and not the guest CA, while
  `network/ca-bundle.pem` holds both. The test then runs `scripts/oauth-signin.ts` as the test's own user with
  `--sign-in-as <did>` (the network's handles have no DNS, so a handle sign-in fails at `login/start` with
  "resolves to no DID"), `--chrome-arg=--ignore-certificate-errors` and
  `--chrome-arg=--host-resolver-rules=MAP <PDS host name>:443 <PDS address>` (the authorization URL names the PDS
  by `pds.default.alice.svc.kcp.local` on port 443). Readings (hat-8, three runs that passed, one gateway and
  one bidder): sign-in 13.8 s, 14.0 s and 14.1 s; `GET /session` 200 with the DID and the cookie attributes
  HttpOnly, Secure, SameSite Strict; the first pod (anti-affinity) Ready on a market node 36.7 to 39.4 s after it
  was created with one NodeClaim Running and a receipt; `dev restart` of the web app DenoPod 1.9 to 2.9 s (pid
  changed), the first request after it got EOF (the front had no backend yet) and the same cookie answered
  `GET /session` 0.5 to 1.0 s after the restart returned and the cluster list worked; the second pod (anti-affinity)
  Ready on a second market node 39.7 to 45.6 s after it was created with two NodeClaims Running; `POST
  /api/clusters/kubeconfig` answered 200 with `Cache-Control: no-store` and a plain client certificate;
  `kubectl get nodes` listed both nodes and `Nodes().List` worked; exactly one `ClusterClientCertificate` for the
  DID existed and was not revoked; `DELETE /session` answered `{"revoked":true}` and the cookie then got 401;
  `revokeIdentity` refused the certificate in 0.23 to 0.29 s and set the object `revoked`; no cookie token,
  `"refresh_token"`, `dpopPrivate`, `PRIVATE KEY` or `client-key-data` was found in the gateway's logs, in its
  DenoPod run directories, or in the DenoPod, MarketIdentity and NodeClaim objects of the gateway workspace and
  the home workspace (24836, 1244 and 9733 bytes read). The run took 349.2 to 358.7 s (`dev up` 80 to 94 s,
  `dev down` 116 to 118 s with two VMs). Not read: a restart of the core and `socialweb-computer upgrade`
  with the same cookie, the scan of the unit journals, the DenoPod and HostProcess arguments and the records of the
  PDS repositories. The host image store `/var/lib/socialweb-computer/guest-image` holds `image.json` sha256
  f2f39859... and versions.json has pinned bddc484c... since the guest kernel commit of 2026-10-15, so the first
  run of this test (and any test that rents a VM) failed with the bidder's `ComputeVM ... Failed: guestimage:
  image.json ... the image is refused`; the runs above used `SOCIALWEB_IT_GUEST_IMAGE` pointing at a copy of the
  pinned image.

### Bidder PDS write budget (2026-10-17)

A hosted PDS gives an account 5000 write points per hour and 35000 per 24 hours (create 3, update 2,
delete 1) and answers 429 `RateLimitExceeded` with `retry-after` when they are used up. The prod bidder
used them up and every Bid then failed. Decisions, all local to the bidder (no change to the public
protocol):

- A Bid names one shared `compute.config.wif.simple` and one shared `market.bids.free`. `createATProto`
  creates each once per content (the key is the record without `createdAt`, so a new issuer URL makes a new
  record) and gives the same strongRef to every Bid. Bid retention deletes the Bid first, then a record the
  Bid named only when no live Bid names it and it is not the current shared record.
- `PdsWriteBudget` (`pds-write-budget-abc`) keeps the latest `ratelimit-*` headers of every repo write per PDS
  and counts writes and points by collection and by hour. `pds-write-budget-fetch` observes the fetch of the
  process, so every agent is covered. Writes have three classes: `contract` (receipt, events, payloads, never
  held back), `bid` (Bid, bids.free, wif.simple) and `sweep` (every delete, the offering refresh). Below
  `bid-write-reserve` points plus 21 points for each live contract (what finishing a contract writes), the
  bidder declines an RFP with reason `writeBudget` and `sweep` writes stop. A 429 pauses `bid` and `sweep`
  writes until its `retry-after`; after the reset time the remaining points count as unknown, not as the old
  limit. The counters are in the status of the MarketBidder object, `status.market.pdsWrites`.
- `vm.onNetwork` and its event have one writer, the kcp provider factory, which keeps the payload and the
  event it wrote and retries only what is missing, with a delay that doubles from 5 s up to 300 s. The
  `vm.hostKeys` payload is kept across retries the same way. The writer in `onAccept` and the bidder's
  `/v1/on-network` route are gone; the kcp provider points its guests at its own route.
- Bid retention deletes every due Bid each pass (every 10 minutes), in `applyWrites` batches of 100 deletes, and a
  pass ends after 5 minutes (`DEFAULT_BID_RETENTION_PASS_MS`) rather than after a count, so deletes keep up with
  bids at any rate (changed 2026-10-18: the cap of 100 deletes a pass was set for the bsky.social write budget,
  and since plan 0018 the bidder's records are in its own repo PDS DenoPod). It scans every page (oldest
  first) before it deletes so that it knows which records live Bids name, and stops at the first 429. `bidder_associate` has the fixed rkey `bidder_associate`, and the
  `badge.blue.keys` record is read before it is created.
- The market network's bob PDS limits repo writes while the file named by `PDS_WRITE_LIMIT_FILE`
  (`<state>/bob-pds/write-limit.json`, `{"points":N,"windowSec":S}`) exists; delete the file to switch it off.

Readings (dev-linux-1, one market network on the old commit and one on the new, 20 contracts each with
`run request --exec true`, five at a time; records counted from the bidder's repo on the PDS): per Bid 9
points before, 3 after (wif.simple and bids.free: +20 records before, +0 after). Per contract 35.4 points
before (236 records for 20 contracts, of them 39 onNetwork and 78 events) and 23.4 after (156 records at the
time of the count; 24 once the late hostKeys and onNetwork records of the last contract are in). The counter in
the status read 513 points for 21 contracts and the startup writes. With a limit of 700 points per 15 minutes
and the default reserve, 9 of 12 contracts finished and the RFPs of the other three were declined 9 times with
reason `writeBudget` while 496 points were left; after the reset the next contract ran. With a limit of 2 points, one Bid
failed on the 429 and the next 12 RFPs were declined with `rate limits writes until ...`. With a 429 on every
write for 45 s after the receipt, `vm.hostKeys` and `vm.onNetwork` failed three times each at 5, 10 and
20 s, were written once the limit was off, and the contract finished.

### Bid claim across bidder processes (plan 0009 ACC9-EXACTLY-ONCE, 2026-10-17)

Several bidder processes of one handle share the bidder's kcp, so the Bid object is the one claim:
`createIfAbsent` of `bid-<sha256 of the RFP uri>` gives one winner, and the others get the existing Bid.
The claim itself was right. Two things around it were not, and both are fixed in `bid-loop.ts`,
`market-bidder-bids-kcp` and `market-bidder`:

- A process that lost the claim never looked at the Bid again. A process that died after the claim left the
  Bid in `Deciding` with no `market.bid` record, and the living process dropped the RFP as a duplicate: the
  bid was lost until the dead process restarted. Now a process that meets a `Deciding` Bid it does not own
  (in the gate, or when its own `createIfAbsent` lost) waits until the Bid has been silent for 10 s
  (`DEFAULT_BID_CLAIM_STALE_MS`), looks for the `market.bid` record (found: marks the Bid Published), and
  otherwise deletes the Bid (kcp delete with uid and resourceVersion, so one process wins and a
  later claim is never deleted) and runs the RFP again through its own gate and claim. A claimer that is
  slow but alive writes `socialweb.computer/claim-heartbeat` on its Bid every 5 s while its RFP is in the gate;
  nothing is written for a claim that finishes inside 5 s. The bidder ClusterRole gets `patch` on `bids`
  (`deploy/apps/market-bidder/vault.yaml`).
- `restore` deleted every `Deciding` Bid at the start of a process, which would delete the live claim of a
  process that is running. It now treats them like any other `Deciding` Bid. `listOwnBids` read the first 100
  `market.bid` records; hono-pds lists oldest first, so the record of a stuck Bid was not found once a bidder
  had more than 100 bids. It pages through all of them now.

Readings, dev-linux-1 (c-48), one market network (`dev up --profile market-network --bidders 1`), the extra
bidder processes are copies of the `bidder` DenoPod with their own state dir (same handle, account and kcp).
The RFPs are `compute.vm` and signed `market.rfp` records written to alice's PDS by a script, because the relay
carries them to every process: the requester of `run request` keeps its repository in its own process, no relay
crawls it, and its RFP reaches one bidder, by the push. Counts are read by RFP uri from the Bid objects and from
the `market.bid` records on bob's PDS. Before = d84f30ff, after = this commit.

| Case (RFPs) | Before: Bids / records | After: Bids / records |
|---|---|---|
| 2 processes, burst of 100 | 100 / 100, 33 and 67 published | 100 / 100, 42 and 58 published, 0 twice |
| 2 processes, kill -9 of one mid-claim, it stays dead (40) | 40 / 33, 7 stuck in `Deciding` for good (an earlier run of 20: 12 records, 8 stuck) | 40 / 40, 6 stuck ones answered 9.6 s after the kill |
| 1 process, kill -9 mid-claim, 30 more RFPs posted while it restarts (60) | 60 / 60 | 60 / 60, the 9 stuck ones answered 10.1 s after the kill |
| 2 processes, bob's PDS stopped for 26 s while 20 claims are open (20) | not run | 20 / 20; all 20 Bids carried a heartbeat, none taken over |
| second relay (`--relay-url a,b`, relay b replays about 900 RFPs from seq 0), then 100 new | not run | 0 new Bids or records from the replay; 100 / 100 |

`bids.free` and `wif.simple` records are made once per process, not per Bid: 100 RFPs over two new processes
added 2 of each. The claim does not touch them (it comes before they are made). Not read, and open:
a process that is frozen for more than 10 s and then continues publishes a second `market.bid` record for the
Bid that another process answered; the ComputeVM controllers of two processes of one handle (a
contract with two processes was not run); and the retention of one process deleting a shared record that
another process still caches, once every Bid that names it is older than the Bid TTL (24 h by default).

### The market network is one kcp (plan 0019, 2026-10-18)

User decision 2026-10-17: Q1 is amended by profile (section 0). Each child application of a MarketNetwork is the
workspace `root:<app>` of the network's kcp (type universal, label `apps.socialweb.computer/application`),
its components in `root:<app>:app`; the network's own services stay in root:global, root:relay,
root:alice and root:bob. One manifest set serves both shapes: `kcpapply.Options.Base` rebases every phase
and wait workspace of a set under the application's workspace (`root` -> base, `root:x` -> `<base>:x`),
except the API provider workspaces `root:*-provider` and the sets with `scope: kcp` (provider,
permission-inventory, denoruntime, dev-host, market-network); file contents are never rewritten, so a
manifest names in-app services as `<name>.default.{{.ServiceSuffix}}` (the suffix of the phase workspace)
and the logical cluster of its base as `{{.BaseCluster}}`. The child's record carries `sharedKcp` (the
network's state dir), the network's admin kubeconfig and kcp URL and `baseWorkspace root:<app>`; the
bootstrap layer places no kcp, kine or front for it, and its core (still a HostProcess in
`root:dev:<instance>`) runs no deno runtime and no store maintenance: the network's one deno runtime runs
every DenoPod of the kcp and writes every run directory under `<network state>/denokcp/runs/` (each names
its pod in `service.txt`). Condition OneKcp replaces OwnKcp. Workspace RBAC (kcp: a service account of
another workspace is the user `system:kcp:serviceaccount:<logical cluster>:<namespace>:<name>`, and it
also needs the non-resource verb `access` on `/` in the target workspace): the app's own pds account
reads its `root:app` DenoPods through `deploy/apps/repo-pds/components-rbac.yaml`, and the composer
grants every child's pds account get/list on the MarketIdentity objects of the other children
(ClusterRole `market-network-peers` in both workspaces of each child), so an AtprotoRecord `valueFrom`
with `workspace: root:<other>` resolves (403 elsewhere, the record stays Pending). Prod keeps a kcp per
application and runs the same sets with base root.

A DenoPod restart (a crash, a kill, a spec change) gives the pod a new run, so the network's fronts (plc, relay, dispatcher, dispatcher-tls, alice and bob pds) are edges
(`deno.computer/edge`, the one case a service may name its port): each binds port 0 once, the network
records the ports in `<network state>/network/ports.json`, renders them as `{{.Ports.*}}`, restarts a
front that came back elsewhere (`deno.computer/restart`) and holds ServicesReady until every front is
back on its port; the network value copied onto the children therefore stays valid. The PLC directory
keeps its operation log in its state dir (`hono-plc --state-path`), so no DID is lost to a restart. In
an application, peers inside it are reached by service name per connection (`<name>.default.{{.ServiceSuffix}}`).
The gateway child's teardown deletes the account workspaces (they sit at the root of the kcp) before
its own workspace, so the clusterprovider releases its tenant members (dev down 130 s -> 60 s).

A core restart adopts the DenoPods (plan 0019 M3C, 2026-10-18). A run (DenoPod, DenoRun or PolicyEngine
process) belongs to no unit of the core: the execrunner drops SWC_UNIT, SWC_DEVSUP and INVOCATION_ID
from its env, and devsup counts a child outside a unit's process group as a member only while it
carries that unit's attribution, so devsup's stop of the core spares the runs (dev down still kills
them by SWC_INSTANCE). Under systemd-user (the prod gateway and bidder units, KillMode=control-group)
the deno runtime starts or adopts the delegated unit `<prefix>denopods-<key>.service` (a `sleep
infinity` holder in socialweb.slice) and clones every run into its `pod-<run>` cgroup (memory.max there
when the pod sets a limit), so the core unit's cgroup kill spares the runs and uninstall stops them
with the other units; measured on a droplet, a child placed there from another user unit outlives
that unit's stop. The deno runtime no longer reaps at start: the provider observes each named run
from `runs/<id>/state.json` (pid and start ticks against pid reuse; a probe reads the run's env from
/proc), the 2 s requeue of a running pod is the exit poll, and the runs sweep stops a live run that no
object names once it is a runs-keep period (1 min) old. `dev restart core` snapshots the live runs
before the stop and 10 s after ready: unitsUnchanged holds only when every run kept its pid
(runsChanged lists the rest). Readings (dev-adopt-1, 1 bidder and the gateway, 13 DenoPods): before,
every core restart replaced every DenoPod (the start reap; twice in 4 s on the runner worker's swap
leg on main). After, `dev restart core` 2.06 s and `--binary` 2.03 s in the middle of a
`run request`: 13 and 15 runs, every pid and runID unchanged, bidder-1 DenoPods restarts 0 from dev up;
9/9 `run request --network` rc 0 across both restarts; the web app answered 921 of 921 polls of
/api/clusters/list (401 without a cookie, never a connection error); a `kill -9` DenoPod ran again in
3 s under the new core; a DenoPod deleted (finalizer stripped) while the core was stopped then killed
had its orphan run stopped 63 s later by the sweep; dev down left no process. Stated limits: an adopted
run that exits has no exit code (the old core held the wait), so it fails and restarts under its
restart policy, and an OOM kill of an adopted run reads as a plain failure.

A PDS pod restart keeps every account and session (plan 0019 M3D, 2026-10-18). Before: hono-pds kept
only repo blocks in its KV; the account store, the per-user signing keys and the OAuth tokens were
in-memory maps (and a PDS without --private-key-hex made a new service key per start), so a restart of
alice's PDS logged the web app's owner out (getServiceAuth 401 invalid_token for 3 min on the runner
worker's swap leg). Now, with --kv-path, the PDS keeps its service key, accounts (password hash and
salt), per-user signing keys and OAuth tokens (with their expiry) in that KV (a StateStore contract in
atproto-repo-abc; DenoKvStorage keys under ["state"]) and restores accounts and signers before it
answers; session JWTs are stateless and verify again under the kept key. A restarted front also comes
back on its port now: the first apply of the network's services rendered `{{.Ports.*}}` as 0, so a
`dev restart alice:pds` bound a new port, was restarted onto its kept port and bound port 0 again; the
network applies the set again once ports.json is recorded, holding ServicesReady until then. Reading
(dev-adopt-1, 1 bidder and the gateway, dev up 78.6 s): `dev restart <net> alice:pds` (pid 220017 ->
223847, new run, same port); the same web app cookie answered GET /session 200 and
/api/clusters/list 200 (tenant Ready) after it, the owner's and a fresh did:plc account's old
sessions got getServiceAuth tokens, and createSession with the owner's password answered 200; the new
PDS logged "accounts restored from the state store { accounts: 3 }".

The teardown of a gateway child deletes its account workspaces first, while its root object and the web
app still run (2026-10-18, 499b454d). Before, the network deleted the Gateway root object first: its
components (the web app and the requester) stopped, and then the account workspace was deleted, so the
tenant's NodeClaim kept its finalizer `nodeclaim.socialweb.computer/release` (the web app owns it) and
the account workspace stayed Deleting until devsup stopped the kcp ("the MarketNetwork was not gone
within 2m0s"). Now the web app releases the NodeClaim through the RFP flow and the clusterprovider
tears the cluster down before the root object goes.

Readings, before (each child application has its own kcp) and after (one kcp), 1 bidder and the gateway:

| Reading | Before: own kcp per child | After: one kcp |
|---|---|---|
| dev up to Ready (runner worker) | 89 to 98 s | 76 to 92 s |
| kcp and kine memory of the network | 1895 MiB | 797 MiB |
| requester kcp start in `run request --network` | 17.1 s | 3 to 6 s |
| dev down | 130 s (account workspaces deleted after the child's own) | 56 to 80 s |
| `dev restart core` | every DenoPod replaced (twice in 4 s on the runner worker's swap leg) | 2.06 s, `--binary` 2.03 s, every run kept its pid (M3C) |
| restart of alice's PDS | the web app's owner logged out (getServiceAuth 401 for 3 min) | accounts and sessions kept (M3D) |

Confirmed on rc13 and main (dev-final-1, DigitalOcean c-48, sfo2, 2026-10-18, `dev up --profile
market-network --bidders 1 --with-gateway`, firecracker guests under systemd-user): dev up 81.1, 81.9,
81.6, 81.8 and 80.1 s to Ready, each with "7 service(s) and 2 child application(s), each in its own
workspace of one kcp, are ready"; kcp 700 to 730 MiB and the network's kine 65 MiB resident after dev up
(765 and 797 MiB); `run request --network --exec 'uname -r'` 63 s, exit 0, requester kcp start 4.04 s,
released with vm.delete 200. dev down with no node rented: 62.2 s (rc13) and 56.1 s (main), the
MarketNetwork deleted, `remaining` empty. dev down with a tenant node rented from bidder-1 (a pod whose
PID 1 ignores SIGTERM, so the guest stop takes its 90 s bound of `vmStop.graceSeconds`): before 499b454d
130.0 and 124.3 s, the account workspace Deleting for 2 minutes with the NodeClaim's release finalizer
held, the MarketNetwork not gone; after 122.7 s, the account workspace gone 25 s after its delete, the
MarketNetwork deleted, `remaining` empty.

### The pds DenoPod, the gateway's association and the teardown of records (plan 0018, 2026-10-18)

Readings on dev-final-1 (DigitalOcean c-48, rc13 and main), one market network with 1 bidder under
`bidder-tangled-vouch` (`dev up --app-values` with `bidder: {policy: bidder-tangled-vouch}`) and the gateway.

- M0-SHARED-PDS-DENOPOD. The bidder's DenoPod `pds` in `root:bidder-1:app` is Running with
  `status.listeners` tcp 127.0.0.1:<port> and relay https://did-key-<key>.relay.localhost. An
  AtprotoRecord `probe-delete` (repo pds, collection computer.socialweb.temp.probe, rkey probe1) in
  `root:bidder-1`: getRecord on the pds listener answered RecordNotFound before, the record with the
  object's status uri and cid once Applied, and RecordNotFound again after `kubectl delete`; the object
  was gone 4.6 s after the delete, and the pds logged `atproto_record_applied` and `atproto_record_deleted`.
- ACC18-GATEWAY-ASSOCIATION-ONCE. Alice's operator account repo holds one badgeBlueKeys record per
  gateway requester DID (rkey `requester_associate-<requester DID>`), and the gateway core logged its claim
  write once. After a restart of the gateway core (its HostProcess killed, generation 1 -> 2, a new pid)
  the record, its cid, the repo rev and the count of claim writes are unchanged, and the Gateway status
  did is the same. Before 36f3d0cb the bidder under bidder-tangled-vouch declined every RFP of the gateway
  ("rejected by scope filter"): bob, its operator, vouched nobody in the network. Now the network
  declares both directions: in the gateway's `root:gateway:app` the gateway operator's vouch for each
  bidder operator (`operator-vouch-<bidder app>`), and in each bidder's workspace the bidder operator's
  vouch for each gateway operator (`operator-vouch-<gateway app>`), which the bidder core writes with the
  operator account it signs in as. The bidder logs the operator it reads from the association record:
  `operator resolved from the association record {did: <requester DID>, operatorDid: <alice's operator
  account>, uri: at://<requester DID>/com.publicdomainrelay.temp.badgeBlueKeys/association}`, the scope
  filter accepted, the bid won, and the tenant node was Ready 42 s after the pod was applied.
- M3-RECORDS-BEFORE-PDS-STOPS. Read every 0.5 s with kubectl while dev down ran: the MarketBidder status
  "2 AtprotoRecord(s) leaving the repo before the pds DenoPod stops", the records gone in 1 to 6 s, then
  "4 component(s) stopping"; the same order on the Gateway in `root:gateway:app`; then each child's
  workspace Deleting and gone in 7 to 12 s. No AtprotoRecord object was left. With a rented tenant node the
  account workspace stayed Deleting until devsup stopped the kcp; that was the teardown order of the
  gateway child, fixed in 499b454d (section 15, "The market network is one kcp").

### A deleted cluster's home workspace goes (2026-10-19)

Before: deleteCluster deleted only the Cluster object. The teardown (reap, which deletes the NodeClaims and
waits for the web app to release them; links; store; finishDelete) removed the Cluster finalizer and the
node claim index entry, and the home workspace `root:<account>:<cluster>` with its APIBindings, RBAC,
quota and mount child stayed, empty, for ever. A market network lost them at dev down; prod never did.

Now finishDelete calls `kcpclient.RemoveHome` after the finalizer: it deletes the home only when its
Cluster is NotFound and it holds no NodeClaim, drops the index entry, then deletes the workspace. The
clusterprovider sweep (every minute, beside the index sweep) removes any home of type cluster under an
account that is older than 10 minutes (`EmptyHomeGrace`) and holds no Cluster and no NodeClaim. The
account workspace stays: it is the user's account. createCluster for a name whose Cluster or home is
being deleted answers 503 ClusterUnavailable ("being deleted; ask again once it is gone") instead of 200
for the cluster that is going. kcp answers 504 to a read of a home right after its delete, so the cluster
list re-reads that home and skips it when it is gone or Deleting.

Readings on dev-home-1 (DigitalOcean c-16), one market network with 1 bidder and the gateway, 35 min of two
lanes through the gateway XRPC: one creates a cluster, schedules a pod (one NodeClaim, Running, with the
web app's release finalizer) and calls deleteCluster; the other creates and deletes empty clusters and,
every third cycle, calls createCluster again right after deleteCluster.
- 59 deleteCluster cycles, 23 with a rented node. In every cycle the NodeClaims were gone (2.7 s median,
  3.9 s max) and the Cluster was gone before the home was Deleting; the home was gone 17.5 s median, 22.4 s
  max after deleteCluster. 0 NodeClaims at the moment the home turned Deleting; 0 firecracker processes after.
- 139 census samples of `kubectl get workspaces` under the account: home workspaces with a Cluster equal
  listClusters in 138 (one sample raced a delete), at most 1 home Deleting at a time, none Deleting longer
  than 3 min; at the end 1 home (the bootstrap cluster tenant) for 1 live cluster.
- A home workspace written by hand without a Cluster was removed by the sweep 11 min 11 s later.
- 12 createCluster-after-deleteCluster cycles: 503 for 15 to 21 s (8 to 11 tries) until the old home was
  gone, then a new cluster Ready in about 26 s.
- 71 finishDelete lines with homeRemoved true, 0 RemoveHome warnings, 0 cluster-list errors (before the
  re-read, with only the Deleting flag of the list: 12 "list clusters in root:<account>:<home>: ... 504"
  warnings in 25 cycles).

### The guest node agent is one Go binary (2026-10-21, 1db5f084)

The cloud-init module (`internal/atproto/cloudinit`, `K3sJoinModule`) now ships one Go binary,
`socialweb-node` (`cmd/socialweb-node`, `internal/nodeagent`), on the shared `internal/systemdunit`
library that HostProcess/hostsvc also use. The module writes the resolver, the sysctl file, the join
config, the unit files and one pinned-download bootstrap shell; the bootstrap fetches
`/downloads/<sha256>/socialweb-node-<arch>` from the door, checks the sha256, installs it 0755 and
execs `socialweb-node units start`. The join's own work after that is Go: the workload-token exchange
and OpenBao JWT login, the KV read and the `0600` files, the OpenBao and apiserver dumbpipe links,
the k3s agent ordering, the bridge CNI conflist (`encoding/json`, written through a rename), diagnose,
hostname, the DNS link, masquerade and the meshd install. The pin is
`ComputeContractNodeJoin.NodeAgent`, filled from the `socialweb-node-x86_64` and
`socialweb-node-aarch64` downloads the web app serves. The old `lib/cloud-init-plugin-k3s-join`
TypeScript module and its scripts and units are gone; only the pinned-download bootstrap and the
separate `setup-secrets`, k3s-install and iroh modules remain shell.

### Ingress is one IngressProxy with three providers (2026-10-21, 1db5f084)

An `IngressProxy` component (`internal/services/ingressproxy`) serves exposures through one of three
providers: `xrpc-subscription`, `iroh` or `opentunnel` (`spec.provider`). The provider is selected
explicitly: `ingress.xrpc.ingressProxyHost` selects `xrpc-subscription`, `ingress.opentunnel` selects
`opentunnel`, and neither fails closed; `iroh` is recognized but rejected until a later plan. Each
provider is its own package under
`internal/services/ingressproxy/`; `Exposure.spec.ingressRef` names the component, and the exposure
dispatcher resolves it through the component registry and uses the optional `IngressRegistrar` the
component publishes. `xrpc-subscription` is the former did:key subscriber, `iroh` supervises
`dumbpipe listen-tcp`, and `opentunnel` supervises `opentunnel route add`. The component fails closed
on a missing or non-executable dumbpipe, a missing opentunnel command, a missing `spec.iroh` or
`spec.opentunnel` block for the selected provider, or an unknown provider; it does not silently start
a different provider. An `ingressRef` that resolves to a component with no registrar is an error
rather than a different provider.

### Certificates share one pair path, and kubeconfig users sign from the tenant CA (2026-10-21, f4c30b42)

`tlsfront.EnsurePair` and `clientcertfront.EnsureEdgeClient` now share one implementation: a lock file
in the directory, a pair validated by `tls.LoadX509KeyPair`, generation under the lock, and
temp-file-and-rename writes. A mismatched or truncated pair is regenerated, so a pair left half-written
heals on the next start. Gateway kubeconfig user certificates are signed from the tenant's
`server/tls/client-ca.crt` and `client-ca.key` when both exist (`internal/clientcert`,
`signFromClientCA`); the vault PKI issue path is the fallback, and the gateway kubeconfig client gained
the `pki/config/ca` and `pki/issuer/*` capabilities that fallback needs. The tenant k3s step now depends
on the OpenBao step in the process-group graph, so k3s's post-readiness harvest cannot read
`openbao/access.json` before the vault step writes it (25be7cb3).

### The requester applies the contract's rules (2026-10-21)

- `run request --deny-bidder-dids` writes `ComputeContract.spec.denyBidderDids`; the requester now
  filters discovered bidders through the running contract's list (`withoutDeniedBidders`), not only its
  own component config, so a denied bidder no longer answers the RFP (ba0e85a5).
- An ssh gateway that acts for one signed-in owner defaults to the open policy workflow. The owner
  session is the trust boundary, and a fresh network has no vouches for the tangled-vouch workflow to
  read (637a5a21).
- The NodeClaim mirrors the contract receipt's strongRef into `receiptUri` and `receiptCid`, which is
  what the scale-to-zero assertions read (ff7969f3).
- The in-process GatewayAPI builds its tenant API client from the gateway's clusterprovider state dir
  and the front's address, so the cluster k8s routes no longer answer 503 ClusterUnavailable
  (a8f341b7).

### The rewrite's readings, and the host contract's limits (2026-10-21)

On the rewrite's integration network in `test/integration` (hat-7): the market network reached Ready in
52 to 56 s; a tenant cluster idled at zero nodes, scaled to two market RFP VMs and back to zero with
cross-node pod connectivity, CoreDNS resolution and the NetworkPolicy matrix as expected (the matrix
logged 404s on hat-7); a NodeClaim's Node was Ready with its podCIDR about 28 s after its pod
was created; `TestAcceptSurvivesABidderRestart` completed the contract across a bidder kill and restart
in about 250 s; `TestWebAppSignInOverLoopbackOriginIssuesAClientCertificate` issued a client certificate
the tenant cluster accepted; and `TestKubeconfigClientCertificateWithoutCredentialHelper` issued a
kubeconfig and had the revoked certificate refused. A market guest fetched its pinned meshd from
`192.0.2.1`, the host loopback a guest reaches through pasta. The tests are the instruments; the
wall-clock values are this run's readings.

The host contract has two limits. A tenant host needs `/etc/rancher` to exist: the k3s server runs under
`nsexec`, which binds `<slot data dir>/rancher` over `/etc/rancher`, and when the target is absent the
bind is skipped, so k3s writes on the host instead of in the cluster's state; `check-host` does not test
it. A dev build also needs Deno 2.9.3 on `PATH` for the policy engine: a release build extracts the
embedded `deno` asset, while a dev build has no embedded zip and `internal/policyengine/deno` falls back
to `exec.LookPath("deno")`. The integration hosts hat-7 and hat-9 were prepared by hand with linger,
`kubectl`, `/etc/rancher`, and a subuid/subgid range for three VM slots.

### Links, syncs and remote vaults are objects (2026-10-21)

- **ComponentLink** (`73c8798a`, `800eedc5`, `b6e17b50`, `0a687a68`). Before: the handler registry had no
  cross-workspace rule and appctl wrote no links. Now a cross-workspace call is admitted only by a matching
  cluster-scoped link in the target workspace; System and same-workspace calls are allowed by default.
  appctl derives one link per concrete component ref, skips wildcard refs, and writes none to
  `OpenBaoServer` or `PolicyEngine`, which have no handler registry entry.
- **The first link generation wrote zero links** (`800eedc5`). Before: appctl passed each desired
  component's spec value to `runtime.DefaultUnstructuredConverter.ToUnstructured`, which needs a pointer,
  so `specRefs` walked an empty map and every application held zero ComponentLinks despite concrete refs.
  After: `componentSpecMap` pointerizes the spec with `desiredSpecPtr`, and the same application holds one
  concrete link per non-wildcard ref (for example, the bidder's link to its `Pds`). The converter accepted
  the value silently, so the failure was a zero, not an error.
- **StateSync** (`667819b2`, `412c7b48`, `072188f4`). Before: the ComputeVM finalizer waited on a status
  marker written by the bidder's best-effort publication, and the report loop wrote `onNetwork` and
  `hostKeys` directly. Now a sync is a kcp object with an idempotency key and an observation-based ack;
  the finalizer waits for Acked, then drops after its timeout so the sync outlives the VM, and a retry
  observes before it writes. The bidder's `atproto.market.event` adapter writes the deterministic payload
  and `market.event` rkeys for `vm.terminated`, and the bidder also writes the `onNetwork` and `hostKeys`
  report syncs.
- **RemoteOpenBao** (`a905e2b2`). Before: vault access existed only through an `OpenBaoServer` the same
  process reconciled. Now a cluster-scoped object names an external OpenBao and the controller reports its
  health, seal, mounts and capabilities as conditions, without touching the remote vault. Remote access is
  off unless `Resolver.RemoteEnabled` is set, http needs the explicit insecure flag, refs are files, and
  `Component.spec.vaultRef` still resolves locally.

### Submit handlers enqueue and return (2026-10-21, 937d287a)

- **The anti-pattern, found live.** Three submit paths did remote work inside the request. The
  bidder's `handleSubmitAccept` (`internal/services/bidder/market.go`) provisioned the guest inline on
  a `context.WithoutCancel` with a 3-minute budget, so the accept answered only after a VM existed,
  and a failed provisioning destroyed the guest on the request's own error path. The bidder's
  `handleSubmitEvent` waited up to 30 s for a termination `StateSync` to be Acked and then called
  `Destroy` inline, so a release carried the PDS/firehose write and the teardown. The requester's
  `submitEvent` (`internal/services/requester/service.go`) resolved the event's payload from the PDS,
  verified the envelope signature with a directory key lookup, and dispatched inline, so a callback
  paid the requester's own PDS latency and an unreadable payload ended the call as an error. Resolving
  an Accept's DID service `submitEvent` ref was then tried behind a 3 s `LookupDID` bound; that
  experiment was rejected, because a bound shortens a wait but does not take the lookup out of the
  request path.
- **The correct flow.** The handler records durable intent in kcp and returns. `submitAccept` records
  the Receipt intent and one `compute.provision` `StateSync` keyed `accept:<accept uri>`
  (`internal/services/bidder/statesync.go`, `internal/statesync`); the adapter writes the Receipt to
  the bidder's PDS, provisions the guest, binds the contract and publishes the events. The release
  path annotates the `ComputeVM` and records the termination `StateSync` keyed
  `terminated:<receipt uri>` (`internal/computevm/termination.go`); its adapter writes the PDS and
  firehose record and calls the Accept record's `submitEvent` only when it names one. The requester's
  `submitEvent` appends the event to the contract's pending events and to the inbox `StateSync` keyed
  `inbox:<event uri>#<cid>` (`internal/services/requester/inbox.go`,
  `internal/services/requester/contracts.go`); its worker resolves the payload, verifies the
  signature, dispatches, and later publishes the vm.delete and calls the bidder Receipt record's
  `submitEvent` only when it names one. With no ref the firehose carries the event and no callback is
  sent. Every write carries an idempotency key, so a retry observes before it writes and a duplicate
  callback is a no-op.

### Tenant requests go around the kcp mount proxy (2026-10-22, D25, W0-MOUNTS)

- **The defect.** On kcp main 74e9fb9c the mount proxy verifies the target CA, authenticates the
  caller and strips `Authorization`, so every tenant request through
  `/clusters/<home>:clusters/...` answered 502 or 401, `Registry.MountStatus` read 502 for a serving
  cluster, `materialiser.shut` counted that 502 as shut, and the revoker's ServiceAccount calls never
  reached k3s.
- **The decision.** The client certificate front serves the whole tenant path (certificate and
  bearer), gated on the `Cluster`'s phase and deletion; the shut decision comes from that gate state
  and `PathGateGrace`, never from a status code; the liveness probe and the revoker dial the
  cluster's own front directly. The rules are in section 6 ("No tenant request crosses the kcp
  mount", "The tenant path is gated") and section 7. The live proof at the end of plan 0026 is the
  instrument.

### A browser shell on a guest is an opt-in tunnel (2026-10-24, plan 0032)

- **Before.** iroh was the only guest transport; the old browser-terminal module was removed and
  its string module id failed with LegacyTransportError. A requester could reach its guest only over
  ssh through the iroh ticket in `status.contract.vmAddress`; vm.onNetwork carried one address.
- **The decision (user, 2026-10-24).** iroh stays the default and required transport. A
  ComputeContract may list `spec.tunnels` (`{tag, port, server, shell: {envFile}}`); the requester's
  user_data then installs the module of `internal/atproto/cloudinit/tunnels.go`: opentunnel from the
  `assets.opentunnel` fetch pin, and a browser-shell binary with its web UI from a pinned image
  layer in `guestDownloads` of `internal/versions/versions.json`, both checked by sha256 on the
  guest.
  One opentunnel profile per tag gives `https://<16 hex route>.<12 char id>.opentunnel.xyz`. The
  guest reports it through the bidder (`send-onnetwork.sh <tag> <url>`, `POST /v1/on-network` with
  `tag`, ComputeVM `status.market.guest.networks`), the bidder signs one vm.onNetwork with `tag` per
  tunnel, and the requester records `status.contract.networks[] {tag, address, at, event}` without
  touching vmAddress or the Held transition. The shell token came only in the secrets bundle (a
  root:root 0600 env file, 32 base64url characters); the shell's run script refused to start with
  fewer, because the shell asks for no auth on a loopback bind without a token. A bidder older than
  this records no tagged address.
- **After (live, market network sh32, --bidders 1).** Held at 07:41:58Z on the iroh address with the
  secrets fetched at 07:41:57Z; the opentunnel certificate came at 07:43:59Z and the tagged
  vm.onNetwork (bidder repo, signed market.event bound to the receipt) and
  `status.contract.networks` followed the same second; over HTTPS `/api/sessions` answered 401
  without the cookie, the bootstrap 401 with a wrong bearer and 204 with the token, `/api/sessions`
  200 with the cookie, `/` 200 with the shell page; the token was in no log, kcp object or
  cidata. A shell without spec.secretsRef was Failed before the RFP; a contract without tunnels was
  unchanged; release removed the guest and the URL stopped answering.
- **Removed (plan 0034 W0, 2026-10-24).** The browser shell, `spec.tunnels[].shell`,
  its pin, units and scripts are gone; the guest shell moves to the guest's sshd over a WebRTC
  datachannel (plan 0034). opentunnel https tunnels stay as they were.

### The tunnel's bring-up time is the opentunnel certificate (2026-10-24)

- **Before (live, market network tn13, guest monotonic seconds).** cloud-init 1.0 to 11.9 (network
  stage 3.6 to 5.8, final stage 8.5 to 11.9, of which runcmd 2.8); the package guard installs nothing
  (jq, curl and ca-certificates are in the image from `deploy/bidder/preinstall-market.json`); the
  iroh vm.onNetwork goes out at 12.1. One setup-guest-tunnels.service started at 11.8, after every
  other module's `systemctl enable`, and fetched opentunnel (0.4) then the shell binary (0.8); the tunnel unit
  waited for both and started at 13.0, and opentunnel asked for its certificate at 13.75. The
  certificate came at 177.3 (163.5 s); the tagged vm.onNetwork went out at 177.5. Requester:
  iroh network 10:00:12.8Z, tagged network 10:02:58Z, 165 s apart.
- **The decision.** One setup unit per binary (`setup-guest-opentunnel.service` and one for the
  shell binary, stamp `/var/lib/setup-guest-<name>.done`), started in parallel; the tunnel unit
  requires only the opentunnel one, the shell unit only its own; the tunnels
  module starts its setup and tunnel units from the front of runcmd (`RuncmdPrepend`) and enables
  them after; a failed fetch retries after 1, 2, 3 and 4 s instead of 5 s each. Starting them from
  `bootcmd` through a `systemd-run` path unit hung boot twice: cloud-init.service is
  `DefaultDependencies=no` and `Before=sysinit.target`. The binaries stay downloads: both take 1.2 s
  and now finish before the iroh report, so baking them into the guest image would gain nothing.
- **After (live, tn13).** Both setups 10.5 to 11.2 in parallel, the tunnel unit at 11.0, the
  certificate request at 11.9, before the iroh report at 12.1; the certificate came at 119.5
  (107.6 s); the tagged vm.onNetwork at 119.8. Requester: iroh network 10:21:08.2Z, tagged network
  10:22:55Z, 107 s apart. The guest's own share of the gap went from 1.9 s to none; the rest is
  opentunnel.xyz's ACME DNS-01 issuance (server side, 108 to 164 s here, 72 s on prod), which only
  an identity made before the guest boots could take off the critical path. The URL answered as in
  plan 0032 (health 200, bootstrap 204 with the token, sessions 200 with the cookie).
- **Changed (plan 0034).** The guest no longer fetches opentunnel from its GitHub release: the
  bidder's `setup-guest-binaries.service` installs it from the bidder's own `/v1/downloads`; the
  certificate wait is unchanged and is why a webrtc transport exists.

### A guest is reached over WebRTC, with sealed signaling through the bidder (2026-10-24, plan 0034)

- **Before.** A requester reached its guest over ssh on iroh, or over an opentunnel https URL that
  cost 70 to 165 s of opentunnel.xyz ACME DNS-01 per tunnel before its first byte (the entry
  above; 72 s on prod). The guest fetched opentunnel from GitHub and the browser shell from GHCR, and
  reported its address to `POST /v1/on-network`. The bidder acted on `vm.delete` only and dropped
  every other market.event payload; there was no way to say anything to a guest after boot. Every
  guest reached the whole bidder host loopback at 192.0.2.1 through pasta `--map-host-loopback`: the
  kcp front, OpenBao and every local Exposure.
- **The decisions, as built.**
  - Three transports: iroh (default, required), `spec.tunnels[].transport` `opentunnel` (default for a
    tunnel) and `webrtc`. `vm.onNetwork` gains `kind` (`iroh`, `https`, `webrtc`; absent reads iroh
    without a tag and https with one); a webrtc address is the guest agent's DTLS fingerprint
    `sha-256 <hex>`. `run request --tunnel <tag>:<port>[:<transport>]`.
  - A guest namespace `com.publicdomainrelay.temp.compute.events.guest.*` in the unchanged market.event
    envelope: `guest.publicKey` {kind `age-x25519`, tag, key} and `guest.signalWebRTC` {kind
    `offer`|`answer`, tag, ciphertext}. A signal names no recipient (the user's decision): the offer's
    sealed plaintext is `{sdp, replyTo}`, the answer is sealed to `replyTo`, and each reader trial-opens
    every signal for its tag, so no public record links a signal to a session key. Non-trickle ICE,
    `stun:stun.cloudflare.com:3478` only, no TURN; a symmetric NAT fails and is reported.
  - Guest to requester: GuestControlPlane `POST /v1/events {acceptUri, acceptCid, record}` replaces
    `/v1/on-network`. vm.onNetwork keeps the ComputeVM guest report path; guest.publicKey and
    guest.signalWebRTC are checked for shape and size only and published by the bidder as signed
    market.events through a StateSync per (collection, tag, kind, sha256 of the record).
  - Requester to guest: `applyMarketEvent` takes a guest.* payload bound to this bidder's receipt from
    the accept author, verifies it and appends it, fsynced, to the ComputeVM's NDJSON queue in the
    GuestControlPlane state dir before the 200 (duplicates by uri#cid dropped; the queue goes with the
    ComputeVM). The guest reads it on the XRPC subscription `compute.guest.subscribeEvents` (WebSocket,
    `cursor`; JSON text frames `{$type: ...#event, seq, uri, cid, record}`; `FutureCursor`; a cursor
    prunes entries at or below it; an empty binary keepalive every 25 s, in `deploy/obligations.txt`),
    on the local handler and as a DirectSubscriptionHandler of the guest relay. The bearer is the
    `Authorization` header only: the Go ingress and the deployed relay forward `authorization` and
    `atproto-proxy` on subscriptions, so the earlier `token` query fallback is gone.
  - Retries: a market event StateSync and the requester's ContractEvent submit classify a reply with
    `participant.Refused`: a handler 4xx (400, 401, 403, a 404 with an XRPC error name) is terminal; the
    relay's 404 "no active subscriber", a bare 404, 408, 429, 5xx, timeouts and connection errors retry
    (2 s doubling to 1 m, no attempt limit while the contract is Held or Releasing, or the ComputeVM
    lives).
  - ContractEvent (`socialweb.computer/v1alpha1`, cluster-scoped, ownerRef to its ComputeContract): an
    outbound one (made with kubectl by the CLI or the browser page) is written as the payload and its
    market.event and submitted to the bidder; an inbound one (`in-<sha256(uri)[:10]>`, phase Received)
    is made by the requester from a verified guest.* event of the contract's bidder.
  - socialweb-guest-agent (`internal/guestagent`, pion/webrtc v4, filippo.io/age; 11 MB for
    linux/amd64): an in-memory age identity and DTLS certificate per run; per webrtc tunnel it posts
    guest.publicKey and vm.onNetwork kind webrtc, streams the subscription (cursor in
    `/run/socialweb-guest-agent/cursor`, 2 to 30 s reconnect backoff), answers each offer it can open
    and bridges each datachannel labelled with the tag to `127.0.0.1:<port>` with buffered-amount
    backpressure; a session that does not connect in 2 minutes is dropped. The CLI side is
    `internal/webrtcproxy`: `run request proxy <name> --tag <tag>` checks every `a=fingerprint` of the
    answer against the onNetwork address and pipes stdin and stdout; `run request ssh --transport
    webrtc` uses it as the ProxyCommand against the reported host keys (logs at warn, plain text).
  - Binaries come from the bidder. The did:key ingress cannot carry them (a response body crosses as a
    JSON string, so non-UTF-8 bytes become U+FFFD, and frames were capped at 8 MB), so
    GuestControlPlane serves `GET /v1/downloads/{sha256}/{name}` (`internal/kit/pinneddownloads`, moved
    out of the gateway) from the release asset `socialweb-guest-downloads` on a local Exposure, and the
    guest fetches from `http://192.0.2.1:<port>`. The bidder adds `setup-guest-binaries.service` with
    pins from its own sidecars whenever user_data has an opentunnel unit or the guest agent's
    `tunnels.json`; the requester's tunnel module carries config only. wootty and
    `spec.tunnels[].shell` are gone (W0).
  - The host-loopback fence (invariant 37): socialweb-vmexec loads an nf_tables bridge table in the
    guest's namespace before it creates the tap; TCP to 192.0.2.1 is accepted only on the ComputeVM's
    `spec.hostPorts` and reset otherwise, everything else to 192.0.2.1, to 127.0.0.0/8 and to the
    namespace itself is dropped, and pasta runs with `--no-map-gw` and no port forwards. The bidder
    lists the local GuestControlPlane port, and in a market network the guest TLS port and the ports
    of the user_data's 192.0.2.1 URLs.
  - The xrpc.fedproxy.com relay is the Go relay `socialweb-computer internal ingress-relay`, deployed by
    `scripts/deploy-xrpc-relay.sh` (`--rollback` returns to the deno relay). It took parity fixes with
    the TypeScript relay: CORS preflight reflecting Access-Control-Request-Headers, a send queue per
    subscription (a caller 4096 messages behind is closed 1008), 64 MiB frames, escaped paths, 413
    for an oversized body, binary frames from subscribers.
  - `tlsreload.Pool.Current` waits (up to 10 s) for the first CA pool instead of failing, which took a
    5 s submit backoff off the first signal after a restart.
- **After (live, market networks p34cli and p34tls, CLI requester, local bidder).** `run request
  --hold --tunnel ssh:22:webrtc --transport webrtc --exec "uname -a; id"` reached Held and ran the
  command over WebRTC with host key checking on. Held to shell: 2.16 s on a fresh invocation; after
  the invocation's kcp had stopped and started again, 10.09 s before the tlsreload change and 6.99 s
  after it (offer to answer 8.49 s to 5.56 s). The guest's own part of an answer is 0.2 s; the rest
  is the two market.event round trips through the bidder. An opentunnel https tunnel still needs 70
  to 165 s for its certificate, so webrtc is the interactive path. iroh ssh is unchanged (0.73 s on a
  held invocation).

## 16. Vocabulary

| | |
|---|---|
| **gateway** | the host-side process set: for us, kcp plus the per-cluster process groups |
| **mount** | the kcp workspace mount by which a tenant reaches its cluster's apiserver |
| **guest** | the VM a node IS: its own kernel, containerd and network stack. A node rented from the market is one too, but it is not THIS host's guest: no tap, no pasta and no address out of this host's subnet leases |
| **node** | a worker. One long-lived command. Never the apiserver |
| **instance** | one cluster's incarnation. The token is per instance |
| **ON / OFF** | whether a cluster's process group is up |
| **zero state** | control plane up, zero nodes, workloads Pending |
| **key** | the name a cluster has INSIDE a provider: `kcpclient.ClusterKey(home)` = `<accountHash>-<name>`, derived from its home workspace path. Unique across accounts; the bare name is not |
| **seam** | the interface where a substrate plugs in: `ComputeProvider`, `scale.Nodes`, `processrunner` |
| **the door** | the socialweb-computer-k8s web app, `socialweb-computer-k8s/hono-socialweb-computer-k8s`: it signs users in through the OpenBao atproto plugin and holds no session, maps the `NodeClaim`s this repo writes to ComputeContracts, and is the only writer of a claim's status. The short name comes from the plans (AGENTS.md maps it) |
| **the core** | the `socialweb-computer` process of an instance that runs the controllers (`internal/stack/componentcore`) and supervises the components and HostProcesses. The short name comes from the plans (AGENTS.md maps it) |
