# Features

What socialweb-computer supports today, how far each item is proven, and where it is documented. Releases at the
time of writing: socialweb-computer rc159 (`ec0f4849`) on the gateway, the ssh gateway and the bidders local, hat-9,
hat-7; page-rc143 on `shell.socialweb.computer`; atirc v0.1.0-5a8dfa2 on irc.fedfork.com.

Proof levels: **prod** = proven on the production gateway and bidders with real accounts; **local** = proven in a
market network of one host (`dev up`); **built** = in the code, not yet proven live.

## Infrastructure as code

| Feature | How | Proven | Docs |
| --- | --- | --- | --- |
| `Infra` / `EphemeralInfra` objects: VMs, Deno sandboxes, agent VMs in one file | `kubectl apply -f examples/infra/*.yaml` | prod (page, CLI), local | [quickstart-local.md](quickstart-local.md), [quickstart-prod.md](quickstart-prod.md) |
| `EphemeralInfra` lifetime: `ttlSeconds`, `endWhen: Deadline` or `Complete` | Complete ends in the pass that sees every contract Released or Failed | local | [acceptance/0012](acceptance/0012-infra-quickstart.md) |
| Infra records in the user's repo, never deleted (`endedAt`, successor rkeys) | `computer.socialweb.infra` / `ephemeralInfra` | prod | [protocol/infra.md](protocol/infra.md) |
| Long-lived requester in the foreground | `run requester --name <n>`: own kcp, port 0, `ready.json` with kcpUrl + kubeconfig | prod (alice, OAuth QR, acceptance 0013 A) | [quickstart-prod.md](quickstart-prod.md) section 7 |
| Requester as a systemd unit of its own account | `install requester` (account created or adopted, `socialweb.slice`, upgrade, uninstall) | prod: install, sign-in, contract, restart, uninstall (0013 B); ssh as the account not yet working | quickstart-prod.md 7a |
| Requester under an account that already runs a core | `kubectl apply -f examples/infra/requester-hostprocess.yaml` | local | quickstart-prod.md 7b |
| Restart keeps the DID, the vault and every Held contract | resume waits for its policy engine, then runs the policy | local + prod (rc158) | [acceptance/0013](acceptance/0013-mixed-prod.md) |
| Tenant k8s cluster from the requester | `run requester --with-gateway`, `run requester cluster`, `kubeconfig`; nodes rented by the same requester | prod: node on a remote bidder (hat-7) fetches meshd from the public bucket, pod runs, scale to zero (0013 C) | quickstart-prod.md section 8 |
| Tenant k8s cluster from the hosted gateway | `k8s.socialweb.computer`: createCluster, getKubeconfig, NodeClaim to rented node, scale to zero | prod | [acceptance/0001](acceptance/0001-auth-modes.md) |

## Compute

| Feature | Proven | Notes |
| --- | --- | --- |
| Firecracker VM per contract, cloud-init `user_data` from the RFP | prod | one VM per host on demand, released on request or ttl |
| Deno sandbox contracts (`computeKind: deno`) | prod (local bidder opted in: CLI 0013 and the page) | `dev up --compute-kinds vm,deno`, `install bidder --compute-kinds vm,deno`, `run bidder --compute-kinds` |
| Guest transports: iroh (default), ssh over WebRTC, opentunnel https, wootty web shell | prod | STUN `stun.cloudflare.com` only, no TURN |
| Guest fence: no TCP to the LAN or the host, only declared host ports | prod | `lanUDP` / `hostUDP` per bidder |
| Secrets: getSecrets by workload identity, sealed fallback on demand | prod | bundle in the requester's OpenBao |
| Bidder-side termination of a VM (ops) | prod | [acceptance/0007](acceptance/0007-bidder-termination.md) |

## Agents (atirc)

| Feature | Proven | Notes |
| --- | --- | --- |
| atirc agent in a rented VM (`spec.agent`, `guest.agentStart`) | prod (CLI, page, ssh gateway) | leased mint after the receipt; revoked before `vm.terminated` |
| Agent placeholder bundle for kubectl-applied contracts | local, prod (page) | no `bao` step needed |
| Agent identity: OAuth (QR / browser) | prod | |
| Agent identity: app password (`pdsAccount`) | built | signs the mint through the owner MarketIdentity |
| Agent identity: local PDS (`localRepo`) | built | needs did:plc on plc.directory, a public PDS ingress and a handle you control; `.local` is refused before the RFP |
| `ssh <handle>@socialweb.computer atirc agent run <name>` | prod | per-user OAuth; the VM is rented as the ssh user |
| Guest requester R2 with a contract-scoped association, nested VM, grandchild agent, cascading release | prod | [acceptance/0005](acceptance/0005-guest-requester-association.md) |

## Market, identity and policy

| Feature | Proven | Notes |
| --- | --- | --- |
| RFP, bid, accept, receipt, events over XRPC submit* and relay / Jetstream | prod | [protocol/market.md](protocol/market.md) |
| Default policies always run: `requester-tangled-vouch`, `bidder-tangled-vouch` | prod, local | `open` only when the user names it; dev networks compose both vouch directions |
| Vouch records through an OAuth requester (optional scope) | built | old sessions stay Ready with a scope-upgrade prompt |
| Operator vouch from an installed bidder (`--bidder-operator-signin`) | local | QR page carries the vouch scope since 2026-11-01 |
| Stable rkeys, single getRecord reads | prod | no listRecords on hot paths |
| Per-user OAuth ssh gateway on port 22 | prod | [acceptance/0002](acceptance/0002-ssh-gateway.md) |

## Browser page (shell.socialweb.computer)

Real kcp and the socialweb control plane compiled to js/wasm, running in the tab as a requester. Release page-rc143.

| Feature | Proven |
| --- | --- |
| Sign in with any atproto account (OAuth, PAR, PKCE, DPoP) | prod |
| Snapshot hibernation: reload restores in about 3 s, also while a VM is Held | prod |
| Request VM button, ssh over WebRTC from the page terminal, Agent button, Release | prod |
| `kubectl apply` of Infra / EphemeralInfra in the page terminal (heredoc); the page adds the ssh key, the ssh tunnel and the agent bundle | prod |
| Policy in a fenced sandbox on every bid | prod |
| `/chat` web chat | prod |
| Deno sandbox contracts from the page terminal, on a bidder that opts in (`install bidder --compute-kinds vm,deno`, pinned with `bidderDids`) | prod (mixed.yaml, stdout `quickstart-deno-ok on linux/x86_64`) |

Not yet: release by deleting the EphemeralInfra (use `kubectl patch ... state Released`);
reading repo files from the page terminal (paste them). Acceptance: [acceptance/0009](acceptance/0009-browser-page.md).

## atirc (irc.fedfork.com)

- Transports: IRC (SASL PLAIN), XRPC `chat.atirc.subscribe` over websocket (CBOR or `?format=json`) and over iroh
- Tokens: service-auth JWTs and long tokens; mint equal-or-below your own handle; 11 scopes per NSID; revoke by the
  parent; contract leases (receipt + termination) that latch dead
- Presence: join and leave events carry an RFC3339 `ts` and `by: client|server`
- Web chat `/chat`, owner questions (`ask`, `answer`, `decide`, owner mode)
- `atirc agent run`, and `atirc send` / `atirc recv` byte pipes (the receiver's child token is revoked when it ends)
- Protocol and acceptance live in the atirc repo (`docs/protocol/atirc.md`, `docs/acceptance/0001-atirc.md`)

## iOS app

The embedded requester (`pkg/embedded`) runs in the app on its own in-process kcp. Last proven on the simulator on
2026-10-31 ([acceptance/0010](acceptance/0010-ios-app.md)); not re-run on rc158/rc159.

## Acceptance status (2026-11-01, rc158 / rc159)

| Runbook | Result | Release |
| --- | --- | --- |
| [0001](acceptance/0001-auth-modes.md) account modes, pause, one VM per host, tenant k3s | pass | rc159 |
| [0002](acceptance/0002-ssh-gateway.md) ssh gateway, per-user OAuth | pass | rc158 |
| [0003](acceptance/0003-cli-webrtc-ssh.md) CLI ssh over WebRTC, every host | pass | rc158 |
| [0004](acceptance/0004-vm-atirc-agent.md) atirc agent in a VM | pass | rc158 |
| [0005](acceptance/0005-guest-requester-association.md) guest requester R2, nested VM, cascade | pass | rc159 |
| [0006](acceptance/0006-rollout-compat.md) rollout checks, compat smoke | pass | rc158 |
| [0007](acceptance/0007-bidder-termination.md) bidder-side termination | pass | rc158 |
| [0008](acceptance/0008-host-observer.md) host observers | pass | rc158 |
| [0009](acceptance/0009-browser-page.md) browser page A1-A7, deno from the page | pass | page-rc143 |
| [0010](acceptance/0010-ios-app.md) iOS app | not run (no iOS client session); last pass simulator 2026-10-31 | - |
| [0011](acceptance/0011-atirc.md) atirc | pass | v0.1.0-5a8dfa2 |
| [0012](acceptance/0012-infra-quickstart.md) Infra quickstart, every example | pass (own network) | main ec97f78e |
| [0013](acceptance/0013-mixed-prod.md) mixed prod: A run requester, B install requester, C gateway in the requester | A pass, C pass, B partial | rc159 |

## Known limits

- `run request release` on an invocation that already settled waits 3 min and exits 1 instead of reporting it is done
- a tenant-cluster node's cloud-init fetches `get.k3s.io` from the internet (not pinned)
- `dev down` can warn that the MarketNetwork was not gone within 2 min; it still finishes clean
- `units roll kcp` cannot roll an installed requester's kcp yet (a drifted kcp rolls when the core starts)
- an atirc agent from a local PDS or an app-password identity is built but not yet proven against irc.fedfork.com
