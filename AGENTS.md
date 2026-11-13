# AGENTS.md - working rules for this repository

Read this file first. It is the one guide for agents in this repository. `CLAUDE.md` imports it.

This repository (socialweb-computer-kcp) is the monorepo of the socialweb-computer project. It is the
single source of truth for its Go code. The only TypeScript left is `scripts/*.ts` and the policy
actions under `internal/policyengine/bundled-actions/tangy/`.

`docs/ARCHITECTURE.md` holds the decisions, the measurements and the invariants. This file points at it
and states the working rules. Open a section of `docs/ARCHITECTURE.md` by its heading when a change
needs the argument behind it. Do not read it end to end.

## Orientation: which file answers which question

| Question | File |
| --- | --- |
| What is this, in one paragraph? | `README.md` |
| The objective, stated once | `docs/ARCHITECTURE.md` section 1 |
| Where a concept lives; the package layout | `docs/ARCHITECTURE.md` section 12, and Layout below |
| Rules a change must not break | `docs/ARCHITECTURE.md` section 13 |
| Why a decision was made, and what it closed | `docs/ARCHITECTURE.md` section 15 |
| The plans and the status of each item | `docs/plans/*.yaml` |
| The live backlog (what is open now) | `NEXT.yaml` |
| The TypeScript that is left | `scripts/*.ts`, and the policy actions in `internal/policyengine/bundled-actions/tangy/` |
| Env the clusterprovider reads | `internal/providermain/config.go`; the gateway's defaults for it in `internal/stack/gatewaykcp/env.go` |
| Env the gateway reads | `internal/gatewaymain/config.go`, `deploy/socialweb-gateway.env.example` |
| How prod is installed and upgraded | `scripts/deploy.sh` (gateway: `install gateway`, then `upgrade`), `socialweb-computer install bidder`, `socialweb-computer install requester` |
| What a host must provide | `socialweb-computer check-host -role gateway`, `-role bidder` or `-role requester`, `docs/host-contract.md` |
| The market network | `deploy/market-network/README.md` |
| The operator handoff | `handoff.yaml` |
| What CI runs on a push, and its runners | `.github/workflows/ci.yml`: the gate, the guest image (fetched from `guest-images/<target>/<pinned image.json sha256>` in the bucket when it is there, built otherwise) and the Spaces publish on GitHub-hosted runners; the pin check (`pins`) and the release builds (`binaries`) on the self-hosted runner of `scripts/gh-runner-install.sh` (account socialweb-ci) |
| What the system does, end to end, and its tests | `docs/acceptance/` (the runbooks), `tests/system` (one test file per runbook), `tests/testkit/README.md` |

## Layout

Go:

| Directory | What it holds |
| --- | --- |
| `cmd/` | one directory per binary (`socialweb-computer`, `socialweb-node`, `socialweb-vmexec`, `socialweb-guest-agent`, `socialweb-apigen`, the two OpenBao auth plugins) |
| `internal/` | the Go packages, in the plan 0026 layers (`docs/ARCHITECTURE.md` section 12) |
| `pkg/` | the packages another module imports: `pkg/embedded`, the requester an app runs in its own in-process kcp (plan 0038); its `Config.OnSnapshot` bytes hold every vault secret in clear JSON: store them encrypted (Keychain, Data Protection Complete, no backup), never log them; `Config.StateDir` holds the renter PLC rotation key `components/Requester-requester/plc-rotation.key` (k256, 0600, made on first use; first rotation key of every guest DID it rents, so it tombstones them when the guest is gone): same protection, never in a backup |
| `api/` | the kcp API types; `go generate ./api/...` writes the schemas, the APIExports and the generated Go |
| `deploy/` | manifests, sets, the generated schemas and exports, `check.sh` |
| `docs/` | `ARCHITECTURE.md`, `plans/`, inventories, research notes |
| `rust/` | socialweb-meshd |
| `tests/` | the Rust system integration tests: `testkit` (harness), `system` (runbook ports), `targets/`; `vmm` (the VMM wrapper on a KVM host, `-p swc-vmm-tests`) |

TypeScript:

| Directory | What it holds |
| --- | --- |
| `scripts/*.ts` | one-off operator scripts (`oauth-signin.ts`, `associate-bidder.ts`, `publish-spaces.ts`), run with `deno run` |
| `internal/policyengine/bundled-actions/tangy/<action>/` | one policy action: `index.ts` and its built `dist/index.js`, embedded by the policy engine |

There is no deno workspace, no lockfile and no vendored TypeScript repository here any more.

## The TypeScript that is left

- `scripts/*.ts` are one-off operator scripts, run with `deno run -A`. No Go code runs them; the
  `publish` job of `.github/workflows/ci.yml` runs `scripts/publish-spaces.ts`.
- The policy actions under `internal/policyengine/bundled-actions/tangy/<action>/` are gha-lite assets
  that the policy engine embeds: `action.yml`, `index.ts` and its built `dist/index.js`. Edit them as
  such assets, with the workflows beside them in `internal/policyengine/bundled-actions/workflows/`.
- No comments in code. ASCII only.

## The kcp paradigm, port 0, and the RFP flow

- kcp is part of each application, not infrastructure. An application instance is a kcp of its own (or,
  in a market network, a workspace of the network's one kcp), a manifest set and a profile. State is kcp
  objects. Components (Pds, Plc, Relay, Bidder, Requester and the other component kinds) run in
  process in the core, in the component runtime of `internal/component`, and host processes are
  HostProcess units; both are reconciled from those objects. Only the virtual Kubernetes clusters
  project uses kcp in the traditional way.
- Which kcp, by profile: a prod profile (the gateway, `run bidder`, `run request` and `run requester`
  without `--network`) runs a strict kcp per application. The market network profile (dev, CI, soak) runs every application
  in one kcp, each in its own workspace `root:<app>` (components in `root:<app>:app`), from the same
  manifest set: the applier rebases the set's workspaces under the application's workspace. Either way
  applications talk only over the atproto market protocol; the shared kcp is composition and isolation,
  never a data channel. Write service names in manifests as `<name>.default.{{.ServiceSuffix}}`.
- Port 0 behind the edge: every listener binds port 0 and reports its bound address into the status of
  its kcp object. Peers find it by service name through kcp. Only the stable public entry points
  (caddy on 443, an application's front) keep a known address.
- The RFP flow is the only way a guest comes up: the requester posts a `compute.vm` record and a signed
  `market.rfp`, bidders bid, the requester accepts, and the winner boots the guest from cloud-init
  `user_data` (`BuildUserData` and the module registry in `internal/atproto/cloudinit`).
  The requester reaches the guest only through the guest transports below. Never start a container or VM by hand in
  code or in a test, and never install a guest agent outside cloud-init. A live run that needs a
  guest brings up its own market network,
  and `socialweb-computer run request` or a tenant cluster of the network's gateway rents the guest.
  `run request` composes a MarketRequester root (`internal/app/marketrequester`), and its Requester
  component (`internal/services/requester` over `internal/market/requester`) runs the RFP, the
  accept and the release; `run request list`, `run request release` and `run request resume` manage
  the invocations it recorded.
- `run requester --name <name>` (plan 0039 A-RUN-REQUESTER, `cmd/socialweb-computer/requester.go`) is the
  long-lived form: the same MarketRequester root and the same bring-up (standalone: bringUp profile quick,
  role requester; `--network <market network>`: a child application), state under
  `<state-dir>/requesters/<name>` (never `requests/`) with the owner flock of `internal/stack/request/owner.go`,
  no contract of its own. Its options after `--` are the MarketRequesterSpec part of `run request`'s, parsed
  by one shared function (`parseRequesterSpec`); a contract or CLI option there is refused. Once the root is
  Ready it writes a workspace kubeconfig (`<dir>/kubeconfig`, 0600) and the ready file (`<dir>/ready.json`,
  plus `--ready-file` when given; tmp then rename, 0600; `internal/stack/request/ready.go`) {schema 1, name,
  pid, startedAt, kcpUrl, kubeconfig, workspace, did, handle, identityMode, pdsUrl, ingress {pds, requester},
  sshPublicKey}: paths and public values only. It prints that as a JSON summary, rewrites the file when a
  field changes (a watch on the MarketRequester), stays in the foreground (`--fg` is a no-op for the unit
  form), and on Ctrl-C or SIGTERM removes the ready file and stops its kcp keeping its state (a network
  child is stopped, not removed). `<dir>/requester.json` records the first start's network, `--identity` and
  spec; a restart with no options after `--`, or the same ones, reuses it (other options are refused),
  adopts the units and keeps the DID, the vault and every contract (Held ones stay Held). `kubectl apply`
  Infra, EphemeralInfra or ComputeContract against the ready file's kubeconfig. Helpers on a running one
  read its ready file: `run requester env <name>` (KUBECONFIG, the kcp URL, the workspace, the DID, the ssh
  public key; `--json`) and `run requester ssh <name> --contract <cc> [--transport webrtc] [--exec ...]`
  (the `run request ssh` code with the requester's own key `<dir>/ssh/id_ed25519`; put its `.pub` in the
  contract's `sshAuthorizedKeys`); `run requester proxy` is its webrtc ProxyCommand. Every ssh the CLI runs gets `SHELL=/bin/sh`, because ssh runs its
ProxyCommand with `$SHELL -c` and an installed account's shell is nologin (sudo and the user manager set SHELL from passwd). With `--host-backend
  systemd-user` (plan 0039 B-INSTALL-REQUESTER, the installed form; refused with `--network`) the process is the core
  itself (`request.RunCore`, the `bidder.RunCore` pattern over `devcore.Run` with the bootstrap app
  `table.Requester`, key `req-<sha256(name)[:10]>`, layout `install.RequesterLayout` under `<dir>/dev/<name>`): it places
  kine, front and slot as user units, applies the root, signs in and sends READY=1 only after the ready file; a stop
  leaves those units and the HostProcesses running for the next start to adopt. `install requester --name <n>`
  (default account `swc-requester`) writes the unit template `socialweb-computer-requester@.service` (Type=notify,
  TimeoutStartSec the ready budget plus `install.RequesterStartGrace`, 1 h for a sign-in) and, once per name,
  `<dir>/requester.json` from the options after `--` (default `--atproto-oauth-qr`). The account is a system account, so
journald puts its user units' lines in the system journal (only root reads them: `journalctl _UID=<uid>
_SYSTEMD_USER_UNIT=<unit>`); `run requester signin <name>` prints the MarketIdentity's `pendingAction.url` from the
requester's own kcp, and `install` prints both. A password never goes into requester.json:
  `run requester password <name>` reads one from stdin into `identity/requester/keys` of the requester's vault.
  With `--atproto-identity-mode pdsAccount`, `run request` and `run requester` (standalone or `--network`, where the
  network child takes only that mode, `--atproto-handle` and `--atproto-pds-url`, default the network's bob PDS) put
  `ATPROTO_PASSWORD` from their own environment into that path before the sign-in; a missing or unreadable keysRef keeps
  the MarketIdentity Pending (`WaitingForCredential`, `VaultUnavailable`), never Failed. A pdsAccount requester signs its
  market callbacks with its PDS service auth (`signing: owner-identity`) and publishes its badge.blue signer key.
  `upgrade` takes the role from the install record. The same-account form is a kubectl-applied HostProcess
  (`examples/infra/requester-hostprocess.yaml`); no root prunes a HostProcess outside its set.
- `run requester --with-gateway` (plan 0039 C-GATEWAY-COMPOSE, `cmd/socialweb-computer/requestergateway.go`,
  `internal/stack/gatewaykcp/requester.go`, `internal/app/gateway/requester.go`) applies a Gateway root `gateway` of
  profile `requester` with `spec.requesterRef` (the MarketRequester `app` of the same workspace) and runs its
  clusterprovider in the `run requester` process. With requesterRef the Gateway composes no PlanAccount, Pds,
  Requester, PolicyEngine, root vault or web app: only the rbac phase of the gateway set (the NodeClaim agent) and a
  nodes-only GatewayAPI (`spec.nodesOnly`, the NodeClaim binding and the downloads route) whose node bundles go to
  the requester's vault (`nodeMarket.vaultMount` requester); the nodes' Infra is in the requester workspace and the
  MarketRequester's Requester rents them as the user's DID. A Gateway without requesterRef composes what it did
  before. Its clusterprovider serves only the clusters of the requester's DID (`SOCIALWEB_CLUSTERPROVIDER_ACCOUNT`:
  the registry and the Cluster controller skip every other account), and `--with-gateway` is refused in a market
  network that has a gateway child (that provider serves every Cluster of the network's kcp). The cluster's k3s,
  kine and OpenBao are HostProcesses of the requester's core; in a market network the
  child core runs the gateway's outbound trust and egress (`gatewaykcp.NetworkOutbound`) when
  `<instance state>/gateway` exists, which `run requester --with-gateway` makes before the network places it.
  `requester.json` records `withGateway`; the ready file is first written only once the Gateway is Ready too, with
`gateway {workspace, name, clusters}`;
  `run requester cluster <name> --cluster <c>` makes the home workspace `root:<account>:<c>` and its Cluster, and
  `run requester kubeconfig <name> --cluster <c>` prints its admin kubeconfig path once the Cluster is Ready (exit 1 before). A node fetches socialweb-meshd and
  socialweb-node from `<base>/downloads/<sha256>/<name>` and checks the pinned sha256: the base is the nodes-only
  GatewayAPI's `spec.publicOrigin`, set from `--node-download-base` (default the public bucket, where the CI
  `publish` job puts the pinned socialweb-computer-k8s-downloads tree content-addressed under `downloads/`); with
  `--network` and the default it is the local Exposure.
- Comms modes (`spec.comms.mode` of the MarketRequester and its Requester; `run request -- --comms-mode
  XRPC|Firehose|Both`). XRPC takes bids and events on the requester ingress only. Firehose has no
  inbound endpoint. Both (the requester default) is the ingress plus the same streams as Firehose. In
  Firehose and Both the requester composer (`internal/app/compose/streams.go`, the same code in the
  browser build) composes EventStreams from the network relay, `spec.comms.eventSourceRefs` and the
  default public firehoses; it composes no per-bidder stream (no subscribeRepos of a bidder PDS: a
  did:key bidder requestCrawls our relay and a bsky-hosted one is on the Jetstreams, and a shared
  host replays every repo it holds), and it prunes the old `bidder-pds-*` sources by their label
  `socialweb.computer/bidder-source`. The defaults (`api/v1alpha1` `DefaultFirehoseRelayURLs`,
  `DefaultFirehoseJetstreamURLs`): our relay
  `https://relay.mini-cloud-0002.chadig.com` (subscribeRepos; on a network with a relay host the network
  relay replaces it) and the two Jetstream v2 instances Bluesky documents,
  `wss://jetstream.us-east.bsky.network` and `wss://jetstream.us-west.bsky.network` at
  `/xrpc/network.bsky.jetstream.subscribeEvents` (`collections`, `dids`, `kinds=commit`). bsky.network
  is not a default. A Jetstream URL whose path ends in `/subscribe` is spoken as v1 (`wantedCollections`,
  `wantedDids`); a bare host is v2. Every relay subscribe sends the stream's filter as repeated
  `wantedCollections` and `wantedDids` (at most 100 and 10000, else that param is left out); our Go relay
  (`internal/atproto/relay`, the network relay and prod) filters on them, other relays ignore them, and
  the client filter (`events.Filter.Narrow`) always runs too. Each subscribe logs `relay_subscribe_dial`
  or `jetstream_subscribe_dial` (endpoint without query, the filter params, the cursor), and the
  EventStream's `status.serverFilter {collections, dids, supported: true|false|unknown}` is written on
  connect only. Each is a RelayEventSource or JetstreamEventSource with its
  EventSource (`firehose-relay-<host>`, `firehose-jetstream-<host>`, label
  `socialweb.computer/market-source=public-requester`) and an EventStream with `spec.resume: Live`
  (no stored cursor; a reconnect resumes from the cursor in memory). `spec.comms.relayUrls` and
  `spec.comms.jetstreamUrls` replace a default list; an empty list turns it off. A requester stream
  carries only `market.bid`, `market.receipt` and `market.event`; a bidder stream only `market.rfp`,
  `market.accept` and `market.event` (`compose.MarketStreamCollections`); payload records
  (`bids.free`, `compute.events.*`, ...) are read through the strongRef, never taken as stream input.
  Every market EventStream of a role delivers to one handler sink (`internal/eventstream/market.go`),
  which drops a `uri|cid` it took in the last 15 s (at most 10000 keys) and logs
  `market_event_duplicate_dropped` once per key with the running count. The requester tracks only the
  records of its own ComputeContracts (`internal/atproto/market/addressee.go`; the sink's index
  `internal/eventstream/addressees.go` is fed by a ComputeContract watch, no kcp read per record, and
  no DID allowlist): a bid whose `rfp` is the RFP of one of its contracts that has no accept yet, a
  receipt whose `accept` is one of its accepts, an event whose `receipt` is one of its receipts or,
  before that receipt is in the contract status, whose `receipt` resolves to a receipt of one of its
  accepts. Before the fetch it drops a bid while no contract takes bids, and a receipt or event whose
  repo is not the repo of an accepted bid of a contract that is not settled. Every StateSync and
  inbound ContractEvent it creates has an ownerReference to that ComputeContract, so deleting the
  contract (or the Infra or EphemeralInfra that owns it) deletes them. The bidder takes every RFP,
  an accept whose `bid` is in its repo and an event whose `receipt` is in its repo (its MarketIdentity
  DIDs). The rest is dropped before any object and counted in `market_foreign_dropped` (every 1000).
  The roles prune their market inbox StateSyncs (Outbound, Retain: the role adapter is the target,
  and a delete has no remote side): one that matches no own contract at once; the requester's of a
  Released or Failed contract once Acked and 10 min old (`MarketInboxRetention`); the bidder's
  every Acked one at 10 min (`requester_inbox_pruned`, `bidder_inbox_pruned`, every 100). No
  inbox StateSync is a dedup fact. A bidder bids only inside an RFP's bid window
  (`market.RFPBidWindow`, 10 min from its `createdAt`, else from its TID rkey; the lexicon has no
  expiry), which the bidder's inbox retention equals: its sink drops a closed RFP before the record
  fetch and again after it (`market_rfp_closed_dropped`, with the source, every 100), submitRfp
  answers 409 `RFPClosed`, and a Deciding Bid of a closed RFP fails `BidWindowOver`
  (`bidder_rfp_closed`). It never bids twice on one RFP: the handled fact is the Bid object (name
  `market.BidRKey`) and, after its TTL, the bidder's own `market.bid` record at that rkey
  (`bidder_rfp_already_bid`). A relay stream reconnects 1000 events back, a Jetstream 30 s back,
  but never before the cursor its last connection started at, after a 1 s backoff that doubles to
  30 s and resets only after a connection that stayed up 30 s; each close logs `event_stream_closed`
  (the cursor, `openSeconds`, `retryIn`, `restored`, the error) and a failed dial `event_stream_subscribe_failed`.
  A close the browser page fires after a memory snapshot restore (code 1006, reason
  `wsconn.RestoredCloseReason` "restored from a memory snapshot", the page's `RESTORE_CLOSE_REASON`;
  `wsconn.Restored`) redials at once from the cursor in memory and resets the backoff to 1 s; the
  pdsclient subscribeRepos stream does the same. Any other close keeps the backoff. Our relay answers a subscribeRepos
  without a cursor with live events only (the atproto sync spec); a cursor replays its backlog. The
  js/wasm WebSocket (`internal/kit/wsconn/conn_js.go`) has no receive backpressure, so it queues
  frames up to 16 MiB (`MaxBufferedBytes`), never by a frame count.
- The bidder's requester streams (any comms mode, `internal/app/compose/requesterstreams.go`): requester
  events reach the bidder through submitEvent first; as a record-only fallback, while the bidder has
  live ComputeVMs (not deleted, not Lost or Failed), it composes EventStreams with the bidder's market NSIDs
  and `dids` = the requester DIDs of those ComputeVMs (at most 100, read at runtime, never configured):
  sources `requesters-relay-<host>` (the network relay, else `spec.comms.relayUrls` or our relay; the
  relay filters on the server) and `requesters-jetstream-<host>` (`spec.comms.jetstreamUrls`, else
  `wss://jetstream.us-east.bsky.network` when the network has no relay). They deliver to the bidder's
  market sink (the same intake as submitEvent, deduplicated by `uri|cid`). A ComputeVM watch
  reconciles the MarketBidder only when that DID set changes; a new set is a spec change, so the stream
  reconnects with the new filter. Their `spec.resume` is `Recent`: after a restart or a spec change the
  stream resumes from its stored cursor only when it was saved in the last
  `delivery.RecentResumeWindow` (10 min), else it starts live. No live ComputeVM, no stream.

## Guest transports, guest events and the host-loopback fence

- Three guest transports, and no other. Never add one.
  - iroh is the default and required transport: every guest gets the iroh module, the requester's
    ssh and `status.contract.vmAddress` are iroh, and a contract is Held on the iroh address and the
    host keys alone.
  - opentunnel https, opt-in per entry of the ComputeContract's `spec.tunnels` with transport
    `opentunnel` (the default): one https URL per tag for the guest's `127.0.0.1:<port>`, back as a
    `vm.onNetwork` kind `https` in `status.contract.networks` (plans 0032, 0034).
  - webrtc, opt-in per entry with transport `webrtc`: socialweb-guest-agent (`cmd/socialweb-guest-agent`,
    `internal/guestagent`, pion and age) bridges each datachannel labelled with the tag to
    `127.0.0.1:<port>`; its DTLS fingerprint comes back as a `vm.onNetwork` kind `webrtc`. The CLI
    side is `run request proxy <name> --tag <tag>` (`internal/webrtcproxy`), and `run request ssh
    --transport webrtc` uses it as the ssh ProxyCommand; `run request --tunnel <tag>:<port>[:<transport>]`
    asks for tunnels. STUN is `stun:stun.cloudflare.com:3478` only; no TURN (plan 0034).
    A tunnels.json entry maps its tag to `"port": <n>` (127.0.0.1) or `"unix": "<socket path>"`; each
    datachannel is one connection to it. The guest agent's shell service (an MCP server:
    `internal/guestagent/mcp.go`, `jobs.go`; tools exec, start, read, write, kill, list, as root) is
    `/etc/socialweb-guest-agent/services.json` `{"shell": {"unix": "/run/socialweb-guest-agent/shell.sock"}}`
    (the socket directly in the state dir): HTTP (POST /mcp, JSON, stateless) on that unix socket only,
    root:root 0600 in a 0700 dir, never TCP; a services.json makes the bidder install the agent too. Reach
    it over webrtc with `{"tag": "shell", "unix": "/run/socialweb-guest-agent/shell.sock"}`, over iroh with
    `ssh root@guest socialweb-guest-agent relay /run/socialweb-guest-agent/shell.sock` (stdin and stdout
    to any unix socket, raw bytes) or ssh streamlocal forwarding. Jobs outlive a connection.
  - The browser shell: ssh over webrtc comes first; wootty is the fallback. An opentunnel entry with
    `shell {envFile}` (default `/etc/wootty/wootty.env`) runs `wootty-<tag>.service` on
    `127.0.0.1:<port>` behind that tag's https URL; a shell on transport webrtc is invalid. The shell
    needs `spec.secretsRef`: its `WOOTTY_AUTH_TOKEN` (32 characters or more, else it does not start)
    comes only in the secrets bundle, and the unit waits for `setup-guest-binaries.service` and the
    secrets ready file. `run request --tunnel <tag>:<port>:opentunnel:shell[=<envFile>]` (or the
    older `<tag>:<port>:<envFile>`) asks for one.
  - The atirc agent (plan 0035; peer to peer, no token): `spec.agent {envFile}` (default `/etc/atirc/atirc.env`)
    needs `spec.secretsRef`; with no `spec.association` the requester treats it as `{fromGuest: true}` plus
    `guestTools: [socialweb-computer]` (`requester.DefaultAssociation`, every path: CLI, infra, kubectl, page, iOS). After the receipt is verified the requester (`internal/market/requester/agentpeer.go`)
    generates the agent's iroh key (a 32-byte ed25519 seed) into the bundle at `/etc/atirc/iroh_key.hex` (hex, 0600),
    writes envFile `ATP_USERNAME=<agent handle>`, `ATIRC_DID=<agent DID>` (0600) and `/etc/socialweb-computer/r2-agent.env`
    `R2_COLOCATED=<label>-<rfp rkey>=<the key's ticket>`. The agent is a co-located account of the guest requester R2
    (identity-only when nothing else asked for one; no atirc token is minted for R2 or the agent): R2 makes its did:plc
    (renter rotation key first, `agent-of/<R2 DID>`), endorses and grants it in R2's repo and writes
    `computer.socialweb.peer/<agent handle>` {iroh: that ticket} in the agent's repo. R2's handle is
    `<label>-<rfp rkey>.<renter handle>` (the association request entry's `handle`, when the renter's handle verifies), the agent's
    `<label>-<rfp rkey>.<R2 handle>`; the requester reads both from the DID documents after the nonce check and logs
    `requester_agent_identity peer=<agent handle>@<agent DID>`, the owner node's `atirc mesh run --peer`. getSecrets
    and the sealed fallback serve the bundle without the envFile and the key until the envFile names `ATP_USERNAME`,
    and that write publishes guest.secretsUpdated so the guest fetches
again; user_data installs `atirc-agent.service`
    and `/etc/socialweb-guest-agent/agent.json` but never starts the unit; it enables `atirc-agent-env.path`
    on the envFile, which runs `systemctl try-restart --no-block atirc-agent.service` (never a start)
    when the envFile's sha256 differs from the one the agent's run script read. A requester-originated
    `guest.agentStart` makes socialweb-guest-agent run `systemctl start --no-block atirc-agent.service`; its run script
    copies the key to the agent user's `~/.config/atirc/iroh_key` (0600) and runs `atirc mesh run --no-publish` beside
    `atirc agent run`; with `/etc/atirc/config.toml` in the bundle (`run request --agent-config <file>`: an atirc client config, 0600,
    its `[agent] harness` and `[agent.anthropic]` model, baseUrl, apiKey) it installs that as the agent user's `config.toml` (0600),
    where `atirc agent run` takes its harness. `run request --agent[=<envFile>]` asks for one; `run request agent <name>` starts it. atirc is
    linux/amd64 only on guests; it is the pinned source build `assets.atirc` (fetch kind cargosrc,
    `scripts/build-cargosrc.sh`), also embedded for the host, where an `AtircNode` (export hostprocesses,
    `internal/controllers/atircnode`, `examples/infra/atircnode.yaml`) runs `atirc mesh run` as a HostProcess (secrets
    only in `spec.passwordEnvFile`, never argv; the key at `<configDir>/iroh_key`, 0600). The ssh session mints below sign with `atircSigning` of the `requester_service_auth` log (atirc only; the market
    callbacks keep `signing`): the OAuth session, the owner MarketIdentity for pdsAccount (`owner-identity`), or for
    localRepo its own Pds component (`local-pds`, a `pds.serviceauth` ComponentLink Requester -> Pds the MarketRequester
    composes only when the Pds is public through its ingress (the DID may be on any PLC: atirc resolves it with its own `--plc-directory-url`),
    `role.LocalPdsSignsAtirc`); else `none` with `atircReason`. Before the RFP of a spec.agent contract (no
    `spec.atircParentRef`) the requester writes the placeholder bundle (one empty entry at envFile, 0600) when
    the secretsRef path holds none (`requester_agent_bundle_placeholder`), then refuses the contract (Failed `refused:
    ... --atproto-handle ...`, no record) when its handle has a disallowed TLD (`.local`) or fails `identlink.HandleOK`
    (it verifies both ways, or the DID document declares `at://<handle>` and a strict parent of it verifies both ways
    to an ancestor in the DID's ident.link walk, so a guest requester's namespaced handle passes). An ssh gateway session (`internal/market/requester/sessionatirc.go`) whose
    command starts with `atirc`, or whose client sends `LC_ATIRC=1` (`LC_ATIRC=0` turns it off), gets,
    when it is not an `LC_SESSION` held one, the agent module in its user_data (so the bidder seeds
    `/usr/local/bin/atirc`), and after the receipt the requester mints, with its own service auth
    (lxm chat.atirc.mint, aud `did:web:irc.fedfork.com#atirc`; the ssh gateway's sign-in scope carries
    `rpc:chat.atirc.mint` and `rpc:chat.atirc.revoke`), the child `ssh-<rfp rkey>.<its handle>` leased
    on the session's receipt and vm.terminated, exp at most the session cap; the command runs with
    `ATIRC_TOKEN=<child>` (sent as the ssh session variable `LC_ATIRC_TOKEN` and exported by the
    command, never in the command line) and the child is revoked when the command ends; a held
    `LC_SESSION` session gets none. A failed mint runs the
    command without it and writes one stderr line. A command of exactly the argv `atirc agent run <sub>`
    instead gets a token the requester mints itself the same way for the agent: `<sub>` when it is
    the user's handle or ends in `.<handle>`, else `<sub>.<handle>` (DNS labels of letters, digits and
    `-`, at most 253 chars; anything else is refused with one stderr line before provisioning), and
    the guest runs plain `atirc agent run` with that token, so the agent joins atirc as
    `<sub>.<handle>`; its mint failing fails the command, and the token is revoked at command end.
    A session command with a minted token runs as the atirc agent user, not root (the guest's claude
    refuses `--dangerously-skip-permissions` as root): the agent module writes
    `/usr/local/bin/atirc-agent-user.sh` (creates the user named by `atirc nick` for the token, in
    wheel with NOPASSWD sudo, lingering, and prints its name; `atirc-agent-run.sh` uses it too) and
    `/usr/local/bin/atirc-agent-exec.sh '<command>'`, which runs the command with `runuser -u <nick>`
    and `bash -c` from that user's HOME; the session waits for both scripts like the binary, and
    ATIRC_TOKEN reaches only that process environment (never argv or a file).
    A requester with no `vaultRef` (the per-user ssh requesters) mints into no bundle, so it skips the
    atirc orphan sweep (`requester_atirc_orphan_sweep_skipped`, logged once).
    The SshGateway's `spec.ssh.publicListen` (`run gateway ssh -- --ssh-public-listen host:port`,
    standalone only) makes the SshServer bind that address itself (the edge; a fixed bind, reported as
    listener `ssh-public`, next to the loopback Exposure); `spec.ssh.webListen` (`--ssh-web-listen
    host:port`) binds the SshServer's web page and API the same way (listener `http-public`, the
    address caddy proxies the public origin to). With the option `public-origin` (and no
    owner sessions file) the SshServer serves the web sign-in at `/` over the same OpenBao atproto
    plugin as GatewayAPI (`gatewayhttp.NewOAuth` over `internal/gateway/custody`, the session at
    `auth/atproto session/<did>` of the ssh-gateway vault) and `/keys` (badgeBlueKeys at keyRkey in
    the user's own repo, through the custody agent). A sign-in first runs the policy
    `bidder-tangled-vouch` (scope lane, self `spec.ssh.operatorDid`, `--ssh-operator-did`; no operator
    admits nobody); a refused or failed sign-in is signed out again and the page reads "this account
    was not admitted". An admitted sign-in asks the SshServer-only handle `market.sessionowners`
    (EnsureSessionOwner(did), built server side from the SshServer's vault and root labels, refused
    past `spec.ssh.maxAccounts`, default 64) for the session owner MarketIdentity `owner-<stamp(did)>`
    (oauthSessionProxy, `auth/atproto session/<did>`); sign-out deletes it. The SshGateway composer
    applies each session owner (compose.ApplySessionOwner), prunes the kcp objects of the ones whose
    session expired or past the cap (compose.PruneSessionOwners; never a repo record), and, once one
    is Ready, composes a Requester `requester-<stamp(did)>` on it with its own ingress, policy, service
    auth, repo links and the SshServer's stream link to it. An ssh session of DID X runs on X's
    requester (which refuses any other DID), so its RFP, accept and records are X's; with no link to
    it the client gets "sign in at <public-origin> first", any other failure "ssh gateway error", exit
    1 either way. The owner sessions file (market network seam) composes the same per-DID requesters,
    one per account in the file, signed in with its password. In both modes the shared Requester
    `requester` and its identity are not composed, the SshServer has `spec.accountRequesters`, and the
    SshGateway is Ready without one. Every ssh gateway requester names its ingress Exposure in
    `spec.marketExposureRef`, so its RFPs and accepts carry the submitBid and submitEvent callbacks,
    and a per-DID requester has `spec.contractOwnerDid`: it runs only the ComputeContracts whose
    `spec.ownerDid` is that DID and their StateSyncs and ContractEvents, so many requesters share one
    workspace. Every ssh session, transient (`ssh-run-<hash>`, TTL 1 h, released when the command
    ends unless `LC_KEEP_VM`) or held, is a ComputeContract, so bids and events reach it through the
    addressee index and the ingress like any contract. A session ends with its ssh channel: when the
    client closes it or the connection drops (the server sends keepalive@openssh.com every 15 s and
    closes a connection after 3 missed replies), the command's context is cancelled, the guest
    session closes, the session's atirc token is revoked and then the contract released (a release
    is tried 6 times, 1 s doubling to 30 s). A requester's Shutdown ends its live transient sessions
    the same way, and its first sync releases every `ssh-run-` contract it owns that is not Released,
    Failed, already releasing, labelled `socialweb.computer/ssh-keep-vm` (`LC_KEEP_VM`), created
    after its start or held by a live session (`requester_stale_session_release`); a VM whose token
    could not be revoked then ends it through the token's vm.terminated lease.
    A held `LC_SESSION` contract (`ssh-<hash>`) records `status.heldSession` {name, guestKeyPath
    (in the requester state dir), until}: until is the session cap plus idle when a command starts
    and the idle when it ends. Shutdown keeps held sessions (`requester_held_session_kept`); the first sync adopts
    one whose recorded expiry is ahead, whose guest address and key are still there, with its idle
    timer at the time left (`requester_held_session_adopted`), and releases the rest
    (`requester_stale_session_release`), sparing `socialweb.computer/ssh-keep-vm`.
    The iroh dialer socket lives in the SshServer
    state dir, or in the instance runtime dir when that path is over 107 bytes; `run gateway ssh`
    refuses a layout where both are. The set `ssh-gateway` binds marketidentities in root. A restart
    keeps the path whole: every app root (gateway, bidder, requester, network, ssh gateway), when it
    is not suspended, renders its set's HostProcesses and OpenBaoServers each pass
    (compose.SetWorkloads, CheckWorkloads). A missing one, or a HostProcess Lost because its unit is
    gone, the host rebooted, its manager or devsup restarted or its identity mismatched (the
    supervisor already stopped a mismatched unit, and a unit name runs once), which the root deletes,
    voids the applied-set shortcut and the set is applied again; one that is being deleted holds the
    pass until it is gone. A per-DID account requester stays composed, with the repo and RemotePDS
    links of its last applied PDS URL, while its session owner is Pending or unresolved; it is
    dropped only when the session expired for a reason that is not one the plugin keeps active, the
    owner signed out or the cap is reached. In per-user mode the SshServer has no requesterRef and
    the shared requester no links. The SshServer's Start waits, without polling, until its vault's
    OpenBaoServer is Ready (`sshserver_waits_for_vault`; woken by the vault provider's bus NotifyBound,
    whose binding fingerprint carries the server's readiness, so a Starting to Ready change notifies)
    instead of exiting. A requester that publishes its signer key binding (owner-session and OAuth
    identities) retries that write on every Vault, MarketIdentity or AtprotoRepo bind
    (`requester_signer_key_waits_for_vault`) instead of exiting while its identity's vault is
    Starting, and fails the start only when no bind comes within 2 min; the MarketIdentity controller
    sends a bus NotifyBound when an identity turns Ready, because the session it serves comes up
    after the Vault bind that woke the requester. The SshServer's web sign-in follows its vault: a rebound Vault
    (bus NotifyBound from the vault provider) rebuilds the custody sign-in on the new address, with
    at most 6 attempts at 1 s doubling to 30 s when the open fails; the MarketIdentity controller
    reconciles on the same signal and never drops it. The atproto plugin keeps a session active when
    a refresh is refused with invalid_client, invalid_client_metadata, unauthorized_client,
    use_dpop_nonce, server_error, temporarily_unavailable, or HTTP 429 or 5xx without a code, and
    when the refresh never reaches the server; every other answer (invalid_grant after the rotated
    token check, invalid_dpop_proof, invalid_request, any other code) expires it. A session an older
    plugin expired for one of the kept codes is refreshed again on a read or a proof, at most once a
    minute per DID. A release whose destroy record or submitEvent keeps failing settles as Released
    once the bidder's vm.terminated for that receipt appears, whatever its reason.
  - The requester association of a guest (plan 0036 A5): `spec.association {subject | fromGuest, refFile}`
    (default refFile `/etc/socialweb-computer/association-ref`, under `/etc/socialweb-computer/` only)
    needs `spec.secretsRef`. With a subject the requester writes a contract-scoped market.association
    after the accept; with fromGuest, user_data writes `/etc/socialweb-guest-agent/association.json`,
    the bundle carries `/etc/socialweb-computer/association-request.json` {authority, contract (the
    accept uri), nonce}. The bundle files are root 0600 and a dev requester refuses root, so
    socialweb-guest-agent makes the group `socialweb-r2` (user_data does too; the atirc agent user joins
    it) and `/run/socialweb-r2` (root:socialweb-r2 2770) and mirrors `association-request.json`,
    `r2-atirc.env` (when the bundle holds a non-empty one; an `--agent` contract has no token) and then
    `association-ref` (empty until the ref is written) into it 0640 on start, on a bundle change and
    after a refetch. The requester inside the guest (`run request --association-ref`, bare, as the
    agent user; it reads `/run/socialweb-r2` when that dir exists, `/etc/socialweb-computer` otherwise),
    finding the ref missing or empty, writes the request as a market.associationRequest record in its
    own repo at rkey `requester_associate-<sha256(authority)[:32]>-<accept rkey>`, then its DID to
    `requester-did` in that dir (`/run/socialweb-guest-agent/requester-did` in the root case);
    socialweb-guest-agent posts guest.requesterDid (did:plc
    only, never the requester's own DID; a did:web is refused because its resolution fetches a host the
    guest names); the requester records the first DID as Pending in `status.contract.guestRequester`
    before it proves it (a second DID is refused, a give-up turns it Refused); the requester reads that request record by one getRecord in the reported
    repo and writes nothing unless authority, contract and nonce match exactly; then it writes the
    record at rkey `requester_associate-<sha256(subject)[:32]>-<accept rkey>` (never over a record of another
    contract, and release deletes only its own), the ref into the bundle, publishes
    guest.secretsUpdated, and the guest agent fetches the bundle again (`SECRETS_REFETCH=1
    setup-secrets.sh`, or the sealed fallback). A failed step retries with a bounded backoff and shows in
    `status.associationMessage`. chat.atirc.mint never gets the association as `assoc` (atirc wants the
    standing rkey and the minting account as subject); every token is minted with an explicit exp
    (at most 24h, the parent token's exp and, for the guest requester, the association expiresAt). The
    agent token is one token per subhandle (named after the RFP rkey) minted after the receipt. Once a fromGuest association is
    written, the requester also mints the guest requester its own child token, subhandle
    `r2-<accept rkey>.<the requester's handle or subhandle>`, into the bundle at
    `/etc/socialweb-computer/r2-atirc.env` (`ATIRC_TOKEN`, `ATIRC_SUBHANDLE`, `ATIRC_EXP`, mode 0600);
    `source` it to run `atirc sub` / `atirc pub` as that subhandle in the owner's channel. A requester
    whose root names `spec.atircParentRef` (a vault secret {token, subhandle, exp}; the guest requester's
    `run request` writes it from that file) mints its own agents' tokens below that subhandle with the
    parent token as bearer instead of service auth. Release revokes the agent and the guest requester
    tokens in the background with a backoff capped at 5 min until each is revoked or expired (24h at
    most after release, from the release time kept in a marker in the bundle secret), starts only
    once that marker is written, stops with the requester and resumes after a restart from the marker;
    every mint is also recorded (path, subhandle, token, exp) under `atircMinted` in the bundle secret,
    and a minted subhandle counts as revoked only on a 200, a 401 for its own token, or its exp; a 401
    counts for an unrecorded (rc126) token only for a subhandle read from the env file. Every revoke
    of an `atircMinted` entry (release, orphan sweep, a refused mint) and of an env-file token whose
    subhandle the env file names is sent, when the subhandle equals the token's own `subhandle` claim
    exactly (the server answers 200 for any name in the caller's scope), first with a service auth
    for chat.atirc.revoke (lxm `chat.atirc.revoke`) when the requester identity is OAuth, the
    subhandle is below the requester's handle and the session's granted scopes (the token response,
    never the access token's scope claim) include `rpc:chat.atirc.revoke` (RequesterOAuthScope asks
    for it, but it is in `oauth.OptionalScopes`, so the identity's scope check does not require it
    and a session granted without it is not asked to sign in again; such a session never asks for
    that service auth; the MarketIdentity stays Ready with `status.pendingAction {kind: signInAgain,
    reason: ScopeUpgrade, message, missingScopes, handle}`, plus `url` and `expiresAt` for mode
    oauthBrowser (an authorize URL for the full requested scope, reissued when it expires), cleared
    once a session grants the full requested set), then, for an `atircMinted`
    entry, with the minting parent's
    bearer, the `spec.atircParentRef` long token, when the subhandle is below it; an entry revoked
    by a 200 is stamped `revoked` there and not revoked again, while a 401 counts as Done but leaves
    it unstamped: an unstamped entry stays in the bundle and its bundle in the orphan-sweep index
    until its exp, the sweep revokes it again on each requester start and sweeps once more at the
    latest such exp, and a bundle is finished for the sweep only when every entry of its contract is
    stamped or expired (the release marker stays until then). A derived subhandle, a claim mismatch, no grant and no parentRef, or each of them
    answered 401/403 uses the token itself; a leased token is revoked before
    vm.delete, while its lease is alive, and a 401 saying the lease is not alive counts as revoked
    (`requester_atirc_revoked` byLease). The first
    attempt runs before vm.delete is recorded when the bundle holds a leased token (after the guest
    requester's release), else before Released is written, or Failed when no guest can still run (no
    receipt, an acknowledged release, or the release bound), and its outcome is `status.atircRevokes` (Done only
    when every token path is revoked that way or never held a token; Pending while retries run;
    Abandoned after 24 h); only that first write and the retries set it, a later status write never
    does, and no token is minted once it is set. `run request` and `run request release` share one
    settle step before they remove an invocation: they wait up to `cloudinit.RequestRevokeWait`
    (15 min) while it is Pending or unset on a contract that names token paths; on that timeout they
    exit non-zero and keep the invocation's state, and a rerun of `run request release` starts the kcp
    so the requester resumes the revokes, from the marker or, with no marker, over every token path
    again. The process that starts an invocation's kcp (`run request`, `resume`, or a `release` with no
    other process) owns it through `requests/<name>/owner.lock` (flock) and is the only one that
    stops it. A `run request release` that finds the lock held asks for the release through the
    running kcp, waits for Released (exit non-zero otherwise), then for the owner to exit, and stops
    nothing; a `--hold` owner whose contract leaves Held with `spec.release` set keeps its kcp until
    the release settles, then stops (and removes) it as its own release would. A refFile, agent envFile or `--secrets` path equal to a token path is refused (CLI and
    requester admission). Every release request stamps `spec.releaseRequestedAt` (SSA, the release
    field manager) and the outcome it answers is `status.release.requestedAt`; a denied accept whose
    receipt never arrives leaves the contract Failed with ReleaseFailed, and a new release request
    restarts its runner, looks for the receipt and releases again; the CLI waits for an outcome of its
    own request (with the receipt waits added to its release wait). Every read-modify-write of a
    contract's bundle holds one lock per contract on the requester. Revoking a subhandle revokes every
    token below it. `spec.atircParentRef` must name `identity/atirc-parent` in the requester's own
    vault and mount.
    `run request --associate <did>|guest` asks for one. Inside such a guest, the agent user starts the
    guest requester with one command, `socialweb-computer run request --name <name> --guest-requester
    [--agent --hold]`: it implies `--association-ref` on `/run/socialweb-r2`, `--skip-ssh` and the
    parent atirc token, which it uses only to mint its own agents' tokens (the guest requester runs no
    `atirc sub` of its own). The atirc agent run script enables linger for the agent user and runs the agent
    with its `XDG_RUNTIME_DIR`, so a dev requester can run as that user. With no command in the guest,
    `run request r2 <name> [--r2-name r2] [--agent] [--hold]` publishes guest.r2Start and the guest agent
    starts `socialweb-r2.service` as the user `r2`; releasing the parent publishes guest.r2Release first (also when no guest requester DID was reported, once the guest is on the network: a guest requester that failed before its hand-off still retires, so its did:plc is tombstoned with its own PDS rotation key and an OAuth renter with no renter key leaves no orphan)
    and waits for guest.r2Released after `socialweb-r2-release.service` released the guest requester's
    contracts. The bounds nest, from the constants in `internal/atproto/cloudinit`: each guest
    `run request release` gets RequestReleaseWait + RequestRevokeWait + 2 min, the release unit 1 min
    more, the cascade (GuestRequesterReleaseTimeout) the unit stop plus 1 min more, and the parent's
    `run request release` waits the cascade plus `--contract-timeout`. An identity-only guest requester (no r2Start
    for the contract) rents nothing: the guest agent sends SIGTERM to the unit's main process only (the run
    script execs `socialweb-computer`; a plain `systemctl stop` signals the whole cgroup, its kcp too, and the
    kcp runs the retire), waits for it to exit (it retires its accounts on stop, bounded to 1 min, and its
    summary says Released), then runs the release unit as for a renting one (it releases, and so retires, any invocation a crashed guest requester left) and answers guest.r2Released, all within
    GuestRequesterStopTimeout (2 min), and the parent waits for that answer GuestRequesterIdentityReleaseTimeout
    (the stop plus 1 min) before it goes on; the outcome is
    `status.contract.r2Release` and the GuestRequesterReleased condition. A bidder releases a VM whose
    requester's contract-scoped association expired 10 min ago (ComputeVM `spec.associationExpiresAt`,
    at most 24 h after the accept, the earliest `expiresAt` of the proof's `parent` chain, only from a proof
    in another repo than the requester's that names the requester as subject and an accept of a repo of the
    chain as contract; each skip is logged). The guest requester's PDS requestCrawls our relay only
    (`DefaultMarketRelayURL`, when its spec names no network relay). `run request --guest-requester
    --colocated a,b` (the r2 unit passes `R2_COLOCATED` from `/run/socialweb-r2/r2-start.env`) makes
    each `<label>.<its handle>` an account on its own PDS (`spec.peer.colocated` of the MarketRequester),
    associated by a market.association in the guest requester's repo whose `parent` is the owner's
    association of the guest requester (same contract and expiresAt), plus the account's keys mirror;
    every verifier follows `parent` (maxDepth 1 to 100, default 100) with the expiry and contract checks at
    each link, and `socialweb-computer internal association <did>` prints that verdict. It also writes
    `computer.socialweb.peer/<subhandle>` {lease, iroh, webrtc, channels} in its own repo and each
    co-located one (`docs/protocol/lexicons-market.md`). Its `run request release` sets `spec.peer.retire`
    before its kcp goes: those records are deleted, every local account deactivated and its did:plc
    tombstoned, best effort; trust never depends on that cleanup (vm.delete, vm.terminated and expiresAt
    end every link).
- The bidder installs the guest binaries, never the requester and never the internet. The requester's
  tunnel module carries config only; the bidder adds `setup-guest-binaries.service`
  (`internal/guestseed`), which fetches opentunnel, wootty, atirc or socialweb-guest-agent from its own
  GuestControlPlane `GET /v1/downloads/<sha256>/<name>` at `http://192.0.2.1:<port>` (a local
  Exposure), checks the sha256 and installs to `/usr/local/bin`. The binaries are the release asset
  `socialweb-guest-downloads` (`scripts/build-guest-downloads.sh`, which takes wootty from its pinned
  ghcr.io image layer at build time). A guest never fetches from GitHub or GHCR. The did:key ingress cannot carry binaries (JSON string bodies, a frame cap).
  The one opt-in exception to that asset: a contract whose `spec.guestTools` names `socialweb-computer`
  (`run request --guest-tools socialweb-computer`) gets the bidder's own running binary
  (`/proc/self/exe`, hashed once per process, linux/amd64 bidders only; any other bidder refuses at
  provisioning), served streamed at `/v1/downloads/<sha256>/socialweb-computer-x86_64` and installed by
  the same unit as `/usr/local/bin/socialweb-computer`. The requester marks it in user_data as
  `/etc/socialweb-computer/guest-tools/<tool>`.
- Guest events live under `com.publicdomainrelay.temp.compute.events.guest.*` in the market.event
  envelope. The guest holds no repo key: it POSTs `{acceptUri, acceptCid, record}` to GuestControlPlane
  `POST /v1/events` (vm.onNetwork, guest.publicKey, guest.signalWebRTC, guest.secretsRequested,
  guest.secretsApplied, guest.requesterDid; never guest.secrets, guest.agentStart or
  guest.secretsUpdated) and the bidder signs and publishes them; requester events to the guest
  (guest.publicKey, guest.signalWebRTC, guest.secrets, guest.agentStart, guest.secretsUpdated) reach it through the bidder's per-ComputeVM queue
  and the XRPC subscription `com.publicdomainrelay.temp.compute.guest.subscribeEvents` (WebSocket,
  `cursor`, JSON text frames, an empty binary keepalive every 25 s). The guest bearer is the
  `Authorization` header only. The bidder routes by NSID prefix and never opens a sealed body.
- Signaling is sealed. Each side publishes a `guest.publicKey` (kind `age-x25519`) per tag; an offer
  is age ciphertext sealed to the guest's latest key, with plaintext `{sdp, replyTo}`; the answer is
  sealed to `replyTo`. A signal names no recipient: a reader trial-opens every signal for its tag.
  No private key, SDP, ICE candidate or IP address is ever in a record, a ContractEvent, a StateSync,
  kcp or a log. The requester side of each event is a cluster-scoped `ContractEvent`
  (`socialweb.computer/v1alpha1`, outbound ones made by the CLI or the browser, inbound ones by the
  requester), owned by its ComputeContract.
- Secrets. `spec.secretsRef` names a bundle in the requester's OpenBao (`[{path, value, mode?}]`,
  mode octal and at most 0600). The primary path is getSecrets: the guest's `setup-secrets.service`
  exchanges its workload identity token and GETs `/xrpc/com.publicdomainrelay.temp.compute.secrets.getSecrets/<contract>`
  from the requester's ingress. A sealed fallback exists for a requester whose ingress may not work
  (the browser page): it runs only when the requester's `spec.secretsFallback` is `Sealed`
  (MarketRequester, Requester; default `None`; `run request -- --secrets-fallback Sealed`), and only
  on demand. With `Sealed`, user_data also writes `/etc/socialweb-guest-agent/secrets.json`
  `{"fallback": "sealed", "getSecrets": <bool>}`, so the bidder installs socialweb-guest-agent and
  `socialweb-guest-secrets.service`. setup-secrets gives getSecrets 90 s
  (`cloudinit.SecretsFetchFallbackDeadlineSec`), then touches `/run/socialweb-guest-agent/secrets.request`
  and exits 0; without an inbound endpoint (comms mode Firehose) setup-secrets is not in user_data and
  the agent asks at once. The agent publishes a `guest.publicKey` with tag `secrets` and posts
  `guest.secretsRequested`; the requester then, and only then, seals the bundle to that key as a
  controller-owned outbound ContractEvent `secrets-<contract>` (`guest.secrets`, reseals for a newer
  key while the request is unanswered). The agent writes each file atomically (root:root, 0600 or
  the bundle's mode, missing parents 0700) and posts `guest.secretsApplied {via}`. Either path ends in
  `/run/socialweb-guest-agent/secrets.ready`; a unit that consumes secrets orders itself
  `After=setup-secrets.service socialweb-guest-secrets.service` with `Wants=socialweb-guest-secrets.service`
  (`cloudinit.SecretsReadyUnits`). The ComputeContract shows `status.contract.secretsRequested`,
  `status.contract.secretsApplied` and the condition `SecretsApplied` (reason `GetSecrets` or
  `SealedFallback`). Never send a sealed bundle unasked: published ciphertext stays on the protocol
  and can be broken later. A Firehose requester with `None` and a secretsRef fails the contract
  ("secrets need the requester ingress (getSecrets) or secretsFallback: Sealed").
- The guest fence. socialweb-vmexec loads the nf_tables bridge table `swcguest` in the guest's
  namespace before the tap exists, and the VM does not start without it (`internal/guestnet`,
  `fence_linux.go`). On frames from the guest's tap it keeps, in order: TCP to the host's loopback at
  `192.0.2.1` only on the ports of the ComputeVM's `spec.hostPorts` (every other TCP connection to it
  is reset, the rest dropped); DNS (udp and tcp 53) to the pasta gateway 172.30.0.1; ICMPv6 router and
  neighbor discovery. It then drops everything to the bidder host's own addresses (`vmruntime`
  lists them in the host namespace at Prepare and writes them into the vmexec params) and to the
  guest link 172.30.0.0/30, and to 0/8, 10/8, 100.64/10, 127/8, 169.254/16, 172.16/12, 192.0.0/24,
  192.168/16, 198.18/15, 224/4, 255.255.255.255, ::1, fc00::/7, fe80::/10 and ff00::/8, and every
  ethertype but IPv4, IPv6 and ARP. TCP to the LAN is always closed. One allowance, on by default:
  UDP to 10/8, 172.16/12, 192.168/16 and fc00::/7 on destination ports 1024 and up, except 1900,
  5353 and 5355, so same-LAN WebRTC can use a LAN candidate; it never reaches the host's own
  addresses, 100.64/10, 169.254/16 or 127/8. It is the ComputeVM's and FirecrackerVM's
  `spec.guestNetwork.lanUDP` (unset: true), which the bidder writes from its own
  `spec.guestNetwork.lanUDP` (MarketBidder, Bidder, NetworkApp), the bidder operator's choice and
  never the requester's: `run bidder`, `install bidder` and `dev up` take `--no-guest-lan-udp`, the
  hono-bidder passthrough takes `--guest-lan-udp=false`. A second allowance, `hostUDP`, comes before
  the drop of the host's own addresses: UDP to those addresses (never loopback or link-local, never
  TCP) on the same ports, so WebRTC works when the requester runs on the bidder's own host (the
  router does not hairpin). It is the ComputeVM's and FirecrackerVM's `spec.guestNetwork.hostUDP`
  (unset: false), which the bidder always writes as its effective value: its own
  `spec.guestNetwork.hostUDP` (MarketBidder, Bidder, NetworkApp) when set, else on when its
  `spec.policy` is `bidder-only-me` and off otherwise; the Bidder's `status.guestNetwork` reports
  `{lanUDP, hostUDP, hostUDPReason}` (`spec`, `policy only-me`, `policy not only-me`). `run bidder`,
  `install bidder` and `dev up` take `--guest-host-udp true|false|auto`, the hono-bidder passthrough
  `--guest-host-udp`. The one inbound path is the forwarded webrtc UDP port: for a guest with a
  webrtc tunnel the bidder leases one UDP port per ComputeVM from its `spec.guestNetwork.webrtcUDPPorts
  {min, max}` (MarketBidder, Bidder, NetworkApp; default 31000-31999; a udp port registry under its
  state dir, `webrtc-udp-ports`), writes it as the ComputeVM's and FirecrackerVM's
  `spec.guestNetwork.webrtcUDPPort`, and writes `{"webrtc": {"udpPort", "hostAddresses"}}` (the
  host's RFC1918 and ULA addresses, `guestnet.HostLANAddresses`) into
  `/etc/socialweb-guest-agent/bidder.json`. pasta forwards `-u <lan addr>/<port>` to the same guest
  port, where socialweb-guest-agent listens with one pion UDPMux and rewrites its host candidates to
  those addresses, so a same-LAN peer (mDNS-only host candidates, no hairpin) reaches the guest. The
  fence accepts UDP from that source port to the host's own non-loopback addresses and to the LAN
  prefixes on ports 1024 and up except 1900, 5353 and 5355, with `lanUDP` and `hostUDP` off too; no
  other UDP port is forwarded, and the addresses never go into kcp. A guest-facing local listener the bidder hands to a guest goes into
  `spec.hostPorts` (`GuestHostPorts`, `guestseed.HostPortsOf`); never widen the fence to all ports,
  and never open TCP to the LAN or to the host's own addresses.
- After a change to `cmd/socialweb-vmexec` or `internal/guestnet`, run `make pins` and commit the
  `socialweb-vmexec` pin with the change, then `dev down` and `dev up` the network instead of a
  `dev restart ... core`.

## Tests: Rust system integration tests

- Tests are Rust system integration tests (`tests/system`, harness `tests/testkit`), plus the VMM wrapper and meshd
  integration tests. Never unit tests: no mocks, fakes, golden files, snapshots, ratchets, inventories or source greps.
- The runbooks (`docs/acceptance`) are the spec. Each test in `tests/system/tests/acc_NNNN_*.rs` names the runbook
  section it implements; a runbook change and its test change land together.
- New behavior gets a test in `tests/system`.
- Tests are atlibs compositions: step-loop modules (`tests/modules`, `tests/compositions/<runbook>`), host handlers
  in Rust (atlibs-kube, atlibs-ssh, `tests/host`); `tests/testkit/README.md`. A runbook's Rust test stays until its
  composition passes on staging and prod.
- A failed composition run freezes (the default outside CI; knotless CI's test.yml freezes too): attach with
  `swc-test-host attach <run dir>` (MCP), read `failure`, `state`, `resources`, inspect with `kube_get` / `ssh_exec`, then
  `cleanup`; `swc-test-host replay <run dir>` reruns the module's decisions offline. Never leave a frozen run past its TTL by hand.
- atlibs pins: `tests/Cargo.toml`'s atlibs-host, atlibs-iroh, atlibs-kube, atlibs-ssh and identlink follow one tested
  set, atlibs `pins.json`, as the knotless runner does (fork branch `runner-atlibs-composition`, deployed by
  knotless-fork-infra `versions.env`). CI's `atlibs-pins` job fails when either is behind; catch up (rewrites the revs,
  `cargo update --workspace`, `cargo check`), then commit:
  `curl -sSf https://knotless.dev/johnandersen777.bsky.social/atlibs/raw/main/pins.sh | sh -s -- sync`.
  A host older than its modules refuses them before they run: `this host lacks <nsid>; update atlibs-host (pins
  sync)` (2026-11-13: a runner without `identity.resolveDid` made the requester's vouch policy deny every bid).
- A change is done when the system tests covering it pass on staging.
- No Go tests, except the policy engine specification tests (owner, 2026-10-30): `internal/policyengine/spec`, the
  policy decision tests in `internal/market/bidder` and `internal/market/requester`, their harness
  `internal/policyengine/policytest`. They must pass; no skip.
- Tests drive a real Go requester (`run requester` / `run request`) as an account with vouches; requests use discovery
  (no bidders list); cleanups release VMs and delete records even on failure; one driver per account.

Run (from `tests/`; the target is a target.json, `tests/targets/prod.json` for prod):

```sh
cd tests
SWC_TARGET=$PWD/targets/prod.json SWC_DRY_RUN=1 cargo test                   # preflight; touches no account
SWC_TARGET=<staging target.json> cargo test                                  # the full suite on staging
SWC_TARGET=$PWD/targets/prod.json cargo test --test acc_0004_vm_atirc_agent  # prod: only in a lead-given window
cargo run -p testkit --bin last-pass -- readings/*.json                       # passing runs onto docs/acceptance/README.md
cargo test -p swc-vmm-tests -- --nocapture                                   # VMM wrapper, KVM host: tests/vmm/README.md
```

Before pushing a `tests/` change (commit it first): `tests/publish-test-host.sh`. It builds the static swc-test-host and
the module wasm of HEAD's `tests/` tree on hat-8 and publishes them to the bucket at `tests/<git rev-parse HEAD:tests>/`
(SHA256SUMS last); knotless CI's test.yml downloads and checks them, and builds on its VM only when they are not there
(its log says `fallback: building test host`).

Debug a failed CI run (a composition run dir holds `ticks.ndjson.zst`, `state.json`, `failure.json`,
`resources.json`, `run.json`; no secret in any of them). On a failure, before the freeze or the cleanup, `logs/` too: per
invocation `<name>/local/` (the request log and its requester's dev logs) and, for a VM that was Held, `<name>/vm/` (over
request.ssh: the VM's journal and every `*.log` under /home and /root: core.log, dev logs, step logs; a failed staging
run's `dev down` keeps its state for this). Each its last 512 KiB, 6 MiB in all, smallest first, the rest named in
`logs/skipped.txt` (an artifact reaches the runner through the guest agent's 16 MiB job output ring). Every run also
writes `logs/<name>/release.log`: its cleanup's `run request release` of each invocation, with its time. Logs are
redacted: secret-named JSON keys as in the dump, other text cut after a secret word's `=` or `:`, JWT-like words and PEM
blocks dropped.

```sh
# 1. the failed check's artifacts (a workflow declares `artifacts: [runs/]` and runs with SWC_RUNS=runs; private to
#    the owner and collaborators, kept 14 days); a knotless CLI built from the fork (runner-atlibs-composition),
#    logged in again once it asks for dev.knotless.getArtifact (`knotless login <you>`). The check's log shows the
#    run as it goes: one swc-test-host line per tick (event, calls, timers, `done pass=`)
export KNOTLESS_APPVIEW=https://knotless.socialweb.computer
knotless run artifacts knotless://johnandersen777.bsky.social/socialweb-computer-kcp --workflow test --commit <sha>    # list
knotless run artifacts knotless://johnandersen777.bsky.social/socialweb-computer-kcp --workflow test --commit <sha> --out ./artifacts
R=./artifacts/runs/<runbook>-composition-<ms>       # locally: ~/.tmp/swc-system-tests/runs/<...>
jq . "$R/failure.json"
# 1b. the market as the run saw it (test.yml captures the relays and Jetstreams into runs/watch.ndjson): load
#    ./artifacts/runs/watch.ndjson in https://watch.socialweb.computer/ with its file picker
# 2. replay it offline (from tests/, after `cargo wasm` in tests/modules; a CI run's composition is this checkout's of
#    the same name): the first diverging tick, then the states there
cd tests && SWC_TARGET=$PWD/targets/prod.json cargo run -q -p swc-test-host -- replay "$R"
cargo run -q -p swc-test-host -- replay "$R" --to <seq> | jq .statesAt
# 3. a frozen run: attach (MCP on stdio), then the tools state, ticks, state_at, failure, resources, kube_get, ssh_exec,
#    run_assert, and continue or cleanup
cargo run -q -p swc-test-host -- attach "$R"
#    a frozen CI run (test.yml freezes a failure for its --freeze-ttl, then cleans up; its `attach` is what a given key
#    runs on the CI VM): the owner or a collaborator gets a handle from the runner (dev.knotless.getAttach, service auth;
#    the CI VM's iroh ticket and host keys, never in the check record; the key goes with the VM), then attach or end it
ssh-keygen -q -t ed25519 -N '' -f ~/.tmp/ci-attach/attach_key
knotless run attach knotless://<owner>/<repo> --workflow <wf> --commit <sha> --key ~/.tmp/ci-attach/attach_key.pub > attach.json
cargo run -q -p swc-test-host -- attach --ci attach.json --key ~/.tmp/ci-attach/attach_key    # needs dumbpipe on PATH
cargo run -q -p swc-test-host -- cleanup --ci attach.json --key ~/.tmp/ci-attach/attach_key   # cleanup: the step ends, VMs released
# 4. a local repro: freeze at the step before the failure
SWC_TARGET=$PWD/targets/prod.json cargo run -q -p swc-test-host -- compositions/<runbook> --on-fail freeze --freeze-at <step>
# 5. leave nothing behind
SWC_TARGET=$PWD/targets/prod.json cargo run -q -p swc-test-host -- cleanup "$R"
```

A later workflow job (`after: [build]`) reads an artifact with
`curl -sfo <file> -H "Authorization: Bearer $KNOTLESS_ARTIFACTS_TOKEN" "$KNOTLESS_ARTIFACTS_URL?workflow=build&name=<path>"`.
Never paste creds anywhere. A run's dump dir (`$SWC_RUNS/<run>`) holds none: secret-named keys are redacted,
kubeconfigs, keys, tokens and the requester state stay in its work dir (`~/.tmp/swc-system-tests/work/<run>`), never
an artifact.

Staging knotless (`dev up --with-knotless <dir>`, with `--with-gateway`): the gateway runs knotless-serve and
knotless-run (the fork's image, binaries copied out) on the network's PLC, PDSes and relay; the runner is the gateway's
owner account, trusts only its repo's compositions and rents from the network's bidders. Guests reach both by the
network's `*.localhost` hosts. Tests: `tests/compositions/knotless/README.md`.

```sh
K=/var/tmp/<work>-knbin; mkdir -p $K; c=$(docker create knotless-fork:<commit12>)  # knotless-fork-infra scripts/build.sh
for b in knotless knotless-serve knotless-run git-remote-knotless dumbpipe; do docker cp -q $c:/usr/local/bin/$b $K/; done; docker rm $c
socialweb-computer dev up --instance <i> --profile market-network --with-gateway --with-knotless $K ...   # as test-0001
socialweb-computer dev status --instance <i> | jq '.network.status.apps[]|select(.name=="gateway").listeners[]|select(.component=="knotless")'
G=<state>/dev/<i>/apps/dev/<i>-gateway/knotless           # client.env (no secret), owner.env (0600), serve.log, run.log
set -a; . $G/client.env; . $G/owner.env; set +a; PATH=$KNOTLESS_BIN:$PATH knotless repo list $KNOTLESS_IDENTIFIER
grep '"run ended"' $G/run.log                             # a failed run: knotless run list <url> --commit <sha> --log
```

A new repo's refs run only once the runner watches it (it acts on nothing already there): wait for the appview to
list it (`$KNOTLESS_APPVIEW/xrpc/dev.knotless.listRepos?runner=$KNOTLESS_IDENTIFIER`) and a `--sweep` (10 s) before
pushing.

Pipeline, in order; cut the rc only when both suites are green:

1. Build the release of the commit (`scripts/build-socialweb-computer.sh`), `deploy/check.sh --fast`.
2. VMM wrapper tests (QEMU default, Firecracker).
3. Stand up staging on VMs rented from the market: own PLC, PDS with throwaway accounts, relay, Jetstream, gateway, ssh
   gateway, bidders; it writes the staging target.json. `tests/compositions/staging`: one VM, or `--input '{"layout":
   "multi"}'` (four small VMs on one meshd overlay, the bidders joined by `--market-network-file`; README there).
4. Full suite against staging.
5. Tear staging down.
6. Redeploy prod.
7. Full suite against prod (from rc166 the release gate; no hand runs), on hat-8, as bob, in one command:

```sh
cd tests && SWC_COMPAT_REQUESTER=release:<previous rc tag> ./gate.sh $PWD/targets/prod.json --account bob
```

It runs every composition in order, one at a time (`--on-fail freeze`, `GATE_FREEZE_TTL` 900 s), prints one
`RESULT <runbook>:<entry> PASS|FAIL|SKIP <seconds> <note>` line each and a summary table, and exits 1 on a FAIL. A SKIP
names its reason (0010 iOS paused; staging-only entries; `GATE_SKIP="<runbook>:<entry>=<reason>;..."`). Logs, the
readings and each run's dump path: `~/.tmp/swc-system-tests/gate/<time>/`.

## Prod deployments

A prod deployment guarantees nothing; the routine path preserves most things.

- The routine deploy (`scripts/deploy.sh` on an installed gateway) upgrades in place: state dir, kcp
  objects, OpenBao contents, the requester's and bidder's DIDs, association records, web sessions and
  running HostProcess units, which the new core adopts by unit name and InvocationID, all survive
  (read on prod: web session across rc8 to rc9, DIDs and association
  across rc11 to rc21). A fresh install, or a lost or purged state dir, preserves nothing and needs
  one association per identity again.
- Losing sessions, VMs, clusters, claims, receipts or any other prod state during a deploy is
  acceptable. Do not plan migration, carry-over or rollback for a change of shape. Deploy fresh when
  the shape changed.
- The atproto credentials of the two prod accounts are on the deploy host, owner-only (mode 0600):
  `~/.aliceoa_bsky_social_source` (requester account alice; exports `ALICE_ATPROTO_HANDLE`,
  `ALICE_ATPROTO_PASSWORD`) and `~/.bobvmbuilder_bsky_social_source` (bidder account bobvmbuilder;
  exports `BOB_ATPROTO_HANDLE`, `BOB_ATPROTO_PASSWORD`). When a deploy loses a session, sign in again
  from these files. Source them into the one command that needs them; never print, log, commit or
  copy their values, and never put them in a kcp object, a HostProcess argument or a plan.
- OAuth sign-ins are scripted; never hand a sign-in link to the user. `scripts/oauth-signin.ts` drives
  a headless Chrome (`/usr/bin/google-chrome`, puppeteer-core) through the atproto OAuth flow with the
  handle and password from a credential file: the QR session-transfer page (qr.fedfork.com, a bidder's
  `prod-qr` MarketIdentity `pendingAction.url`) or the web app's sign-in page, the PDS's authorize
  page, then "Authorize". Example, for the hat-8 bidder:
  `deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.bobvmbuilder_bsky_social_source --account BOB --url "<pendingAction.url>"`.
  It reads the file itself (values never on the command line or in logs) and writes step screenshots
  to `~/.tmp/oauth-signin/<ts>/` (mode 0700) for debugging. A QR link expires after 5 minutes; read the
  current one from the MarketIdentity status right before running it.
- After a fresh gateway deploy, the requester alice has no web app session: the cluster provider logs
  "no oauth session stored for did:plc:..." on every claim. Sign alice in with the same script and
  `--url https://k8s.socialweb.computer/ --start-api` (the gateway is API-only: `--start-api` POSTs
  `/oauth/start` with the handle and drives the returned authorization URL).
- A bidder's guest image comes from `--guest-image-source` (a `file://` or `https://` base that
  `install bidder` writes to config.json), or, when that is unset, from the published artifacts:
  `run bidder` reads `guest-images/<target>/<pinned image.json sha256>`, then `releases/<version>` and
  `main/<commit>` under `--guest-image-release-base`, the public Spaces bucket by default. CI uploads a
  guest image once, to that content-addressed key, and only when the key does not hold it yet. On hat-8 the pinned image is in
  `/var/lib/socialweb-computer/guest-image` (root-owned; the bidder verifies every file by the sha256
  pinned in internal/versions/versions.json).
- The did:key ingress relay at xrpc.fedproxy.com is the Go relay `socialweb-computer internal
  ingress-relay` (`internal/atproto/ingress`), behind caddy on a unix socket as the
  `30-go.conf` drop-in of `xrpc-relay-server.service`. Deploy it only with
  `scripts/deploy-xrpc-relay.sh` (build, ship, restart, health check, rollback on a failed check;
  once healthy it points Caddy's on_demand ask at the relay's 127.0.0.1:5556, live and in
  /etc/caddy/Caddyfile; `infra/ingress-relay/deploy.sh` wraps it); `--rollback` goes back to the deno
  relay, which has no ask listener: it starts `caddy-check-dns-from-config` (127.0.0.1:5555, installed
  but disabled otherwise), points the ask (live and Caddyfile, backup kept) at 5555, removes the
  drop-in (which stops the Go relay), then runs `xrpc-custom-domains.sh remove`; `--status` only
  reads. What runs on fedproxy.com and what depends on it: `docs/infra/FEDPROXY.md`. Never run
  `atproto-reverse-proxy/scripts/deploy.sh`: it prints `CF_API_TOKEN`. A change to the ingress
  protocol (`internal/atproto/ingress`) ships to both ends: the core and this relay.
  Custom domains: a host whose `_xrpc.<host>` TXT holds a live subscriber's did:key (or its
  did-key host) is served as that subscriber; a subscriber may also send
  `com.fedproxy.temp.xrpc.alias {hosts}` (answered `#result {accepted, rejected}`, 16 per subscriber,
  gone on disconnect). Caddy reaches it through the relay itself: with
  `--ingress-relay-caddy-custom-domains` (the deploy's `CUSTOM_DOMAINS=1`) it adds, on start and every
  30 s over the admin socket, what is missing of the route of every host outside fedproxy.com and of
  the HTTP-01 fallback issuer on atprp-ssh-relay's policy (a Caddy restart drops both; admin API only:
  a hostless Caddyfile site block would shadow atprp's appended tunnel routes), plus its ask listener
  127.0.0.1:5556. `infra/fedproxy-legacy/xrpc-custom-domains.sh status|remove` reads or drops the
  route and the issuer; the ask stays on 5556 (it answers every fedproxy.com host as the old checker
  did). A response
  frame's optional `headers` (name -> string, or -> [string] for Set-Cookie) reach the caller; the relay
  drops hop-by-hop, Content-Length, Strict-Transport-Security, Alt-Svc, Proxy-*, Set-Cookie with a
  Domain, invalid names or values (CR, LF, NUL), and past 64 values or 32 KiB; `contentType` wins over
  a Content-Type header.
- The atproto relay at relay.mini-cloud-0002.chadig.com (bidders' requestCrawl, a firehose source) is
  the Go relay `socialweb-computer internal atproto-relay` (`internal/atprotorelaymain` on
  `internal/relay/local`), on 127.0.0.1:2584 behind caddy as the `30-go.conf` drop-in of
  `atproto-relay.service`, state in `/var/lib/atproto-relay-go`. Deploy it only with
  `scripts/deploy-atproto-relay.sh` (`--dry-run` prints the plan, preflight on 127.0.0.1:2585, rollback
  on a failed check); `--rollback` removes the drop-in and goes back to the deno relay, `--status` only
  reads. Caddy is not touched.
- Every prod bidder runs the policy `bidder-tangled-vouch`, the default when MarketBidder
  `spec.policy` is unset (`install bidder` writes `bidder-policy` to config.json); `open` is insecure
  and runs only when set explicitly, never on prod. The acceptance requester alice must be vouched by
  the bidder operators bob and john (`sh.tangled.graph.vouch`, docs/acceptance/0001-auth-modes.md).
- A market policy always runs, on both sides and in both lanes. No policy named means the default
  (`requester-tangled-vouch` on a requester, every contract and RFP carries it, owner-session mode
  included; `bidder-tangled-vouch` on a bidder). The bidder's full lane runs its own workflow and then
  every policy the RFP attaches, and allows only if all allow; no verdict is cached across decisions,
  and a stored bid verdict never skips it. The requester gate (scope and full, contract and session)
  runs the body of the attached policy record, which must be in the requester's own repo and, for a
  bundled name, equal the bundled workflow; it refuses a contract or session whose policy names
  another. On the bidder the requester is the subject. A run whose workflow declares `cache-key` gets
  `runs.CacheKey` (workflow, mode, perspective, every DID, the records, the args), so a memoized
  verdict never crosses parties. Free winners get the full run too. A missing evaluator, run client
  or engine, or an `allow` output that is not a strict boolean, denies. `open` runs the open workflow
  only when named; it never skips. A run whose PolicyEngine is not bound yet (a core start) waits
  for the binding (`policyengine_run_waits_for_engine`, then `policyengine_run_engine_bound`) within
  the caller's run timeout instead of failing at once. A requester restart never fails a contract
  for a transient start-up failure: when a resumed contract's recorded-winner policy run does not
  complete, or its attached policy record, its secrets bundle or its ingress url cannot be read, it
  keeps its phase and tries again (`requester_contract_waits_transient`, a bind or the backoff 2 s
  doubling to 30 s) and fails only on a deny or after `TransientResumeGiveUp` (2 min).
- The PolicyEngine run cache (`internal/policyengine/cache`) hands a run only its own entries (its cache-key verdict, the policy/ keys of its rfp, bid and accept, their rec/ entries and the strongRefs in them, its trust snapshot), drops expired verdicts, and its `spec.limits.maxCacheBytes` unset means `sandbox.OutputLimit`/2 (32 KiB; the scope verdict store keeps the set value, unset is no byte cap). A policy action whose stdout passes `sandbox.OutputLimit` (64 KiB) fails with `policy action output exceeded 65536 bytes`.
- Only the lead runs a prod deployment (plan 0006 M5-FRESH-DEPLOY). Task agents never touch prod.

## How to verify

- While iterating, build only what you touched: `go build ./internal/<pkg>/... ./cmd/socialweb-computer`.
  Do not run the full gate per change.
- The full gate runs ONCE, right before "ready": `go build ./...`, `go vet ./...` and
  `deploy/check.sh --fast`. GitHub CI runs the same three on every push to `main` and `task/**`
  (`.github/workflows/ci.yml`); read the branch's run on GitHub.
  - `deploy/check.sh --fast`, in order: `gofmt -l` of `api`, `internal`, `cmd`, `hack` and `lexicons`;
    `go build ./...`; apigen into a scratch directory and a diff against every file
    `go run ./cmd/socialweb-apigen -root . -list` names (generated code must match its source);
    `go generate ./...` must leave `api`, `internal/atproto/market` and `deploy` unchanged;
    `go vet ./...`; `scripts/check-architecture.sh`; the identlink vectors (`go run ./hack/identlinkvectors internal/identlink/vectors.json`); the policy engine specification tests
    (`go test ./internal/policyengine/... ./internal/market/bidder/ ./internal/market/requester/`; they
    find deno through `SOCIALWEB_POLICYTEST_DENO`, the embedded deno, the build-staged pinned zip in
    `~/.tmp/socialweb-computer-build/cache/deno-embed`, or PATH, and fail without one); `go build` and `go vet` of each Go asset module
    in `internal/versions/versions.json`; `go build` and `go vet` for darwin/arm64, darwin/amd64,
    windows/amd64, windows/arm64 and linux/arm64, each with a `-tags release` build of
    `cmd/socialweb-computer`; and the `-tags release` build of every release platform.
  - `deploy/check.sh --local` adds `cargo fmt --check` and `cargo test` of `rust/`, and
    `socialweb-computer dev verify-assets` on a release build when the host's blobs are staged; it
    lists what it skipped. `deploy/check.sh --full` makes a skip a failure.
- The binary for live runs: `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/<work>`
  (`make dev` is the same with `/var/tmp/swc`). It builds one socialweb-computer of the working tree
  (uncommitted changes included) for the host platform, or for each `--platform`. It embeds every
  pinned asset, staged once per `versions.json` entry; the first `--dev` build on a host stages every
  asset (about 3.5 minutes), later builds only an asset whose entry changed. `socialweb-computer
  version` says "dev build, not reproducible" with the commit and the dirty state. `--publish`
  copies the host binary to `<publish>/bin/socialweb-computer` and mirrors the working tree to
  `<publish>/src`, both readable by test-0001. Output: `~/.tmp/socialweb-computer-build/dev/<worktree>/`.
- A Go change in a running dev instance or market network: rebuild with `--dev --publish`, then
  `socialweb-computer dev restart <instance> core --binary /var/tmp/<work>/bin/socialweb-computer`
  (as the account that runs the instance). It stops the core unit, places it again with the new
  binary, waits until it is ready, prints a JSON report and fails unless every unit was adopted
  unchanged. A child application of a market network takes `dev restart <network> <app>/core` (or
  `dev restart <network>-<app> core`); the network's supervisor places that core through its
  HostProcess object. Then rerun `run request`. Do not `dev down`/`dev up` for a code change.
- One component: `socialweb-computer dev restart <instance> [<app>/][<workspace>:][<kind>/]<component>`.
  It writes `spec.restartedAt` on that component's kcp object and waits until the component reports
  the restart. The PolicyEngine restarts only with `core`. A kcp roll is `dev roll kcp`; on an installed host,
  `units roll kcp`, `units roll cluster` and `units roll link`.
- The release binary (prod only; the lead builds it): `scripts/build-socialweb-computer.sh` without
  `--dev` builds one commit once per release platform of `versions.json` from a clean checkout:
  every pinned asset staged by `scripts/stage-assets.sh` and checked against its pin (a signed upstream is
  verified with its key vendored at `internal/versions/keys/<fingerprint>.asc`, never fetched at build time;
  only `make pins` fetches a key that is not vendored, from its pinned URL, keyserver.ubuntu.com or
  keys.openpgp.org, and keeps the first that imports with the pinned primary fingerprint), the embedded
  Deno of the policy engine, then `go build -tags "release embed_deno"` with `CGO_ENABLED=0`.
  Each step is keyed by its inputs and does not run again when they did not change: asset blobs by
  their pinned inputs (assets built here by their tar bytes), socialweb-meshd by rust/ and the
  toolchain, plus the go build cache and the cargo target dir. Tens of seconds warm; minutes with
  empty caches. Every phase prints a `timing` line. `make` builds with `--update-pins`: assets built
  from this repository or from pinned upstream source are rebuilt and their new pins written to
  `internal/versions/versions.json`; downloaded assets still verify their pins and never move.
  `make pins` re-stages and re-pins only the owned assets. Commit the pin change with the source
  change. The `pins` CI job runs `make check-pins`, which runs `make pins` and fails when
  `versions.json` differs from HEAD. Checkouts, caches and outputs stay under
  `~/.tmp/socialweb-computer-build`; `release/<commit>/` holds the binaries and `SHA256SUMS` that
  `install` and `upgrade --binary --sha256` take. Never build it in the inner loop.

## Where and how to work

- Work in a plain git worktree or branch of this repository. Do not put a worktree inside another
  worktree.
- Commit small logical commits and push as you go.
- Before you mark a goal complete cut a release candidate
  - Babysit CI every time you tag a release candidate. You are only allowed to
    tag rc's, never full releases.
  - Cut more rc's until CI is green and you have validated that they install and
    work on all bidder hosts and prod gateway.
- Never use `/tmp`; it is tmpfs and spills to swap. Use `~/.tmp` (set `TMPDIR` under it).
- A one-off script is a `.ts` file under `$TMPDIR`, run with `deno run -A`, using `Deno.Command` for git
  and go. Keep each shell call to one plain command.
- Plans live in `docs/plans`. Flip an item's status only. Do not add evidence lists; the git history is
  the evidence.
- Rebase on `origin/main` often (`git fetch && git rebase origin/main`). When a rebase stops on
  conflicts, run `scripts/regen-after-rebase.sh` (no arguments). It stops first when a generator
  input is conflicted (`api/*.go`, `internal/apigen`, `cmd/socialweb-apigen`, `hack/codegen.sh`):
  resolve those by hand, `git add` them and run it again. Otherwise it drops the conflicted
  `api/**/zz_generated.*.go` files, reruns `hack/codegen.sh`, takes one side of every conflicted file
  that `go run ./cmd/socialweb-apigen -root . -list` names (the schemas and exports under `deploy/`,
  the zz_generated register and conditions files), reruns `go generate ./api/...`, stages the
  outputs and prints the hand-written conflicts left, or the command to continue. Never hand-merge
  generated output. `deploy/apiresourceschema-revisions.txt` merges itself (`merge=union`); when it
  names one schema twice, the script keeps the HEAD line, and the generator may ask for a Revision
  bump in `api/v1alpha1/resources.go`.
- Live runs: your own market network only, run as the VM account (test-0001) from the binary and the
  mirror that `--dev --publish /var/tmp/<work>` wrote, with an instance name and bidder handle unique to
  your work. Keep the network up for the whole item; clean up only your own units and VMs.

## The fast loop for one item

- Compile: `go build ./cmd/socialweb-computer` (about 4 s in a new worktree, the Go caches are
  shared).
- Binary and mirror: `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/<work>` (about 21 s the
  first time in a worktree, about 6.5 s after). Later changes: build again, then
  `dev restart <instance> core --binary /var/tmp/<work>/bin/socialweb-computer`.
- One network for the whole item: `dev up` once (80 to 95 s, mostly waiting, about one core), then every
  proof is a `run request --network <instance>` (80 to 90 s each, 35 s of it the requester's own kcp).
  `dev down` costs 70 to 95 s (one-kcp network with a gateway). Do not `dev up` per proof.
- A held guest burns one host core for as long as it lives (an idle firecracker vCPU does not halt), so
  release a `--hold` invocation as soon as you have read it.
- Take a live slot before `dev up` and release it after `dev down`:
  `deno run -A ~/.tmp/swc-lead/live-slot.ts acquire|beat|release <name>`. The default is 5 slots
  (`SWC_LIVE_SLOTS`), and `acquire` also waits while MemAvailable is under 12000 MB
  (`SWC_LIVE_MIN_AVAIL_MB`) or cpu pressure avg10 is over 90 (`SWC_LIVE_MAX_CPU_PSI`).

## The loop

- There is no container. Read the state of an instance with `socialweb-computer units list` and its
  units' journals (`journalctl --user -u <unit>`); the processes already log a status line.
- NEVER write a sleep-poll loop such as `for i in $(seq ...); do sleep ...; journalctl ...; done`.
  It blocks a turn for minutes and cannot fail. Watch the log with the Monitor tool instead (below).

## Turns and background work

- Never end a turn with a build, test or live run in flight. Background a long command and
  keep working, or wait for it, but do not hand back a turn that says "still running".
- Never block the foreground on a long run. Start a live run (`dev up`, `run bidder`, `run request`)
  or a build with run_in_background and wait for its completion notification, or watch its log with
  the Monitor tool (filter for the success line AND the failure lines). Foreground `until` loops
  only for waits under about a minute; no `sleep N; tail` chains. Do other work (docs, the next
  commit) while it runs.
- Parallelize reads, greps and edits. Live runs go in parallel too: every listener binds port 0, and
  the VM account test-0001 has 16 VM slots that vmruntime hands out under a per-account lock (no
  whole-run lock; test-0001 linger stays on). Keep heavy builds few: 16 cores.
- Launch subagents for parallel work, then verify the artefact (the file, the exit code, the log
  line) rather than the summary.
- Nested subagents hit a depth limit. Do not spawn a subagent from inside a subagent; do that
  reading inline.

## Edit discipline

- Read the exact bytes before editing. The edit tool matches text, and a guessed string is a
  failed edit.
- A block that repeats: use `replaceAll`, not one edit per copy.
- One failed match is fixed by re-reading and retrying. Do not guess the same string again.
- Do not revert another agent's work. Run `git status` first; the tree is shared.

## Architecture rules (plan 0026)

- Layout: the TypeScript ABC layers in Go. Imports point down this list only;
  `scripts/check-architecture.sh` enforces it:
  - `api/<group>/v1alpha1` (common): types, contract status, refs.
  - `internal/contract/<concept>` (abc): small Go interfaces and pure types; api and stdlib only.
  - `internal/kit/<name>` (common helpers): no concept; api, contract, kit and third-party only.
  - `internal/controllers/runtime/<pkg>` (transport): the kcp transport (`kube` typed client and
    awaits, `kcpapply`, `manifests`); the only home of the dynamic client and unstructured objects.
  - `internal/<concept>/<impl>` (impl): one sibling package per backend, the real work, no kcp.
  - Adapters: `internal/controllers/<kind>` turns kcp objects into contract calls (one reconciler per
    kind on `internal/controllers/runtime`); `internal/services/<svc>` turns XRPC/HTTP routes into
    contract calls and holds no role logic.
  - `internal/app/<root>` (factory): one composition controller per app root; wires impls into the
    bus and picks a backend by the `backendRef` kind, never by a flag. Composition code
    (`internal/stack/**`, `internal/devcore/**`, `internal/<x>main`) is this layer too.
  - `cmd/` (CLI); tools (`hack/`, `internal/apigen`, `internal/lexgen`) rank with it.
- Gate. `scripts/check-architecture.sh` (`go run ./hack/archcheck`) fails on any violation of: the
  layer order; backend deps; a ticker or periodic requeue site missing from `deploy/obligations.txt`;
  a System caller missing from `deploy/bus-bootstrap-callers.txt`; a dynamic client or unstructured
  import outside the transport and apigen; a GroupVersionResource literal outside `api/` (foreign
  groups are vars in `api/kcpgvr`); an annotation read as a signal (kcp and kubelet metadata
  conventions are listed in the checker); an endpoint field in spec without `apigen:"userEndpoint"`;
  an impl package without a compile-time assertion against an `internal/contract` interface.
- Go idiom. No factory types: `New(cfg, deps)` takes explicit dependencies and returns a concrete
  struct; callers accept the contract interface. Every impl states its contracts at compile time
  (`var _ compute.Provider = (*Provider)(nil)`). One package, one concept; no util package outside
  `internal/kit`. kcp is a transport like XRPC or the CLI: an adapter translates it.
- No polling. Controllers are driven by watches on their own kind and every kind they ref. Deadlines
  use delayed requeue; errors use the runtime's rate-limited backoff. A ticker is allowed only for an
  outbound obligation and must be listed in `deploy/obligations.txt` with its owner and period.
  systemd units are followed through D-Bus signals, processes through wait.
- Bus. Components and controllers get typed contract handles with `bus.Resolve[T]`. The caller is the
  reconciled object or the component instance, never a controller and never System; System is allowed
  only for the callers in `deploy/bus-bootstrap-callers.txt`. The callee holds credentials; a handle
  never exposes a token. A call with no accepted ComponentLink fails with `ErrLinkDenied`.
- Testable backends. A backend package (`internal/<concept>/<impl>` and the host libraries it drives)
  runs without kcp, the bus or a controller runtime: it takes and returns `internal/contract` and api
  value types, and host effects (systemd, processes, filesystem root, clock) come in through the small
  interfaces of `internal/contract/host`. The controller translates objects to contract values.
  `scripts/check-architecture.sh` reports a backend whose deps reach kcp.
- Writers. Every write is server-side apply with field manager `socialweb.computer/<controller>`. A
  spec is written only by its composer; a status only by the controller of its kind; a field a second
  controller reports is its own status subfield with its own field manager. Every child and intent
  object has an ownerReference to its composer or creator. No annotation carries a control signal.

## Standing constraints

- No explanatory comments in code. The code explains itself through its structure: names, layers,
  contracts and types. Two kinds of comment are allowed: the `ponytail:` form naming a ceiling and its
  upgrade path, and functional comments that a tool reads, such as `//go:` directives, generated-code
  headers and code generator markers (`// +k8s:deepcopy-gen=package`, `// +groupName=`,
  `// +k8s:deepcopy-gen:interfaces=...`). A functional comment carries no prose.
- ASCII only, everywhere, including this file and `docs/ARCHITECTURE.md`.
- Decisions, measurements and invariants live in `docs/ARCHITECTURE.md`, never in code comments.
  An item opens in section 14 and moves to section 15, with its before/after readings, when it
  closes.
- The invariants in section 13 are load-bearing; a change must not break one.
- Runtime runs as USER 1000:1000. Nothing on the L0 host needs root.

## Names: use the real name

Write the real name of a thing: the repository, directory, package, binary, command or plan item id.
Do not make up a short name. Write plain, short sentences, with one meaning for each word. The plans
and older notes use the short names below; read them as the real names.

| Short name in the plans | Real name |
| --- | --- |
| the door | the gateway's web app and tenant API: the GatewayAPI component (`internal/services/gatewayapi`) and `internal/gatewayhttp`, served by `socialweb-computer serve gateway xrpc` |
| the ssh door | the SshServer component (`internal/services/sshserver`) of the SshGateway root (`internal/app/sshgateway`), run by `socialweb-computer run gateway ssh` |
| the core | the `socialweb-computer` process of an instance that runs the controllers (`internal/stack/componentcore`) and supervises the components and HostProcesses; `internal dev-core` in a dev instance |
| a fold, folded | one pass that merged a set of decisions into the plans |
| the spine | the RFP flow: requester, RFP, bid, accept, cloud-init `user_data` |
| the front | the stable entry point in front of a service: the kcp front before the kcp slots (0006 M2-KCP-SLOTS), the TLS front of the market network |
| the edge | caddy and the other stable public entry points that keep a known port |
| a stand-in | a small test executable used in place of k3s, kine or postgres |
| the soak | the last run against the real accounts (the bobvmbuilder bidder and alice), after the market network passes |
| a gate | a check that must pass before an item or a merge goes on (`deploy/check.sh`, a plan's done_when) |
| the seed | the first example that a model grows from |
