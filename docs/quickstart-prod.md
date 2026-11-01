# Quickstart (prod): Infra and compute against prod bidders

- the same Infra / EphemeralInfra objects as [quickstart-local.md](quickstart-local.md), rented from prod bidders with a real
  atproto account
- prod means: prod bidders (each runs `bidder-tangled-vouch`), plc.directory, the did:key ingress relay at
  xrpc.fedproxy.com, the relay at relay.mini-cloud-0002.chadig.com, irc.fedfork.com for atirc, a real atproto account
- what works today: the browser page (section 3), the CLI per contract (section 4), the agent join (section 5), tenant clusters
  through the prod gateway (section 6), a long-lived requester from the CLI that `kubectl apply` works against (section 7,
  `run requester`)
- what is planned: installing that requester as a systemd unit and the gateway in it (section 7, plan
  [0039](plans/0039-long-lived-requester.yaml)); the atirc agent from a local PDS is built (section 5) and not yet proven on prod
- acceptance: [acceptance/0012-infra-quickstart.md](acceptance/0012-infra-quickstart.md) "Prod Step For The Agent Join" and
  "Prod Readings"; the runbook rules of [acceptance/README.md](acceptance/README.md) apply (announce on atirc first, release
  what you rent, no secret on any terminal)

```mermaid
flowchart TD
  ACC["your atproto account<br/>(OAuth QR / browser, or app password)"] --> V["vouches: you -> bidder operator,<br/>bidder operator -> you"]
  V --> PAGE["browser page shell.socialweb.computer<br/>kubectl terminal, page's own kcp"]
  V --> CLI["CLI per contract<br/>run request --hold [--agent]"]
  V --> GW["prod gateway XRPC<br/>createCluster / getKubeconfig"]
  V --> LL["run requester<br/>long-lived kcp, ready file, kubectl apply"]
  PAGE --> HELD["ComputeContract Held on a prod bidder"]
  CLI --> HELD
  LL --> HELD
  GW --> K8S["tenant k3s, nodes rented through the gateway Infra"]
  HELD --> AG["agent VM: requester mints at irc.fedfork.com,<br/>guest.agentStart, nick joins"]
```

## 1. Who Runs This

- you, with your own account, from your own host or browser; prod bidders and prod services are shared, so follow the
  acceptance rules: one driver per account, announce before a run, release everything you rent
- never set `policy: open`: the default `requester-tangled-vouch` (requester) and `bidder-tangled-vouch` (every prod bidder)
  always run
- credentials stay in owner-only files, sourced into the one command that needs them; never print a token, a password or a
  sign-in URL

## 2. Vouches On Prod

Both policies read `sh.tangled.graph.vouch` records (rkey = the subject DID, `record.subject` = that DID, any `kind` but
`denounce`). On prod they are records in people's own repos, written with any atproto client (`goat record create`):

| direction | who writes | record |
|---|---|---|
| bidder operator -> you | the bidder operator, in the operator's repo | rkey = your DID |
| you -> bidder operator (or the bidder DID) | you, in your repo | rkey = the operator's DID |

- read a bidder's operator from its MarketIdentity `status.approvedAssociations`, or ask the operator
- the bidder runs the RFP's attached policy as itself before it bids, so without the operator's record you see
  `no bids received within 30s` and, on the bidder, `policy rejected ... not vouched`
- verdicts are cached 5 minutes, denials too: write the records first
- check a record: `goat get "at://<operator did>/sh.tangled.graph.vouch/<your did>" | jq -r .kind` prints `vouch`
- when you operate the bidder yourself (`install bidder`, [bidder.md](bidder.md)), you write the operator record from the
  operator account; [../examples/infra/vouch-bidder-operator.yaml](../examples/infra/vouch-bidder-operator.yaml) applies in
  the bidder kcp of a bidder installed with `--bidder-operator-signin <operator handle>`: the operator signs in once
  by QR (scope: `sh.tangled.graph.vouch` writes only) and the record is written to the operator's repo
  ([bidder.md](bidder.md), Operator sign-in)

## 3. Browser Page: kubectl Terminal

The page `shell.socialweb.computer` runs a kcp and the requester in the browser (comms Firehose, sealed secrets fallback).
Its kubectl terminal applies the same files into the page's own kcp ([acceptance/0009-browser-page.md](acceptance/0009-browser-page.md)).

1. sign in with your account (OAuth)
2. in the terminal, paste the file with a heredoc (the page terminal has no repo checkout, so `-f agent-vm.yaml` finds
   no file): `kubectl apply -f - <<EOF` + the contents of `examples/infra/agent-vm.yaml` (or `vm.yaml`) + `EOF`
3. read `kubectl get ephemeralinfras.socialweb.computer -o yaml` until the contract is Deployed
4. the Agent button sends `guest.agentStart`; `ssh <contract>` opens a shell from the page terminal
5. `kubectl patch ... state Released` releases it

- proven on the page: `agent-vm.yaml` and `vm.yaml`, released by `kubectl patch ... state Released`; `mixed.yaml` (with
  the DenoSandbox `sbx1`) and release by deleting the object are not yet proven on the page

- page-rc142 and later write the ssh key and the agent secrets bundle for a kubectl-applied contract; on page-rc141 an
  `agent` contract failed at once and `ssh <contract>` had no key
- last pass: page-rc142, account hat, EphemeralInfra `qsprod`, Held in 41 s, `hat-<rkey>` joined, revoked, then Released
  (acceptance 0012 "Prod Readings")

## 4. CLI Per Contract: run request

`socialweb-computer run request` brings up a kcp of its own for one invocation, composes a MarketRequester root, rents one
contract and keeps the invocation under `--state-dir` (runbooks [0003](acceptance/0003-cli-webrtc-ssh.md),
[0004](acceptance/0004-vm-atirc-agent.md)).

```bash
SWC=socialweb-computer
R=~/.tmp/req; mkdir -p "$R"; chmod 700 "$R"
NAME=qs-vm-$(date +%s)
$SWC run request --state-dir "$R" --name "$NAME" --hold \
  --tunnel ssh:22:webrtc --transport webrtc --exec "uname -a; id" \
  -- --atproto-oauth-qr > "$R/$NAME.log" 2>&1 &
# sign in from the pendingAction url in the log (QR or a scripted sign-in); never paste it anywhere
$SWC run request ssh "$NAME" --state-dir "$R" --transport webrtc --exec "uname -m"
$SWC run request release "$NAME" --state-dir "$R"
```

- identity: `-- --atproto-oauth-qr` (QR), `-- --atproto-oauth` (browser), or an existing account with an app password
  (`-- --atproto-identity-mode pdsAccount --atproto-handle <handle> --atproto-pds-url <pds>`, the password from
  `--atproto-password` through the environment)
- `run request list`, `run request resume <name>`, `run request release <name>` manage the invocations it recorded
- one invocation is one contract; there is no `kubectl apply` of an Infra into it (section 7)

## 5. Agent VM: The atirc Join

The mint needs a requester identity irc.fedfork.com resolves and that signs service auth (lxm `chat.atirc.mint`, aud
`did:web:irc.fedfork.com#atirc`). irc.fedfork.com checks the JWT against the DID document from plc.directory, then the
handle: the first of up to 3 `alsoKnownAs` aliases whose DNS TXT `_atproto.<handle>` (else
`https://<handle>/.well-known/atproto-did`) names the DID. An agent token is leased, so irc.fedfork.com also reads the
receipt and the bidder's `vm.terminated` from the requester's PDS: that PDS must be reachable from the internet.

| identity | signs the mint with | log `requester_service_auth` `atircSigning` |
|---|---|---|
| OAuth (`--atproto-oauth-qr`, `--atproto-oauth`, the page, the iOS app) | the OAuth session; its granted scope holds `rpc:chat.atirc.mint` | `oauth` |
| existing account with an app password (`--atproto-identity-mode pdsAccount`) | the owner MarketIdentity (getServiceAuth on that account's PDS) | `owner-identity` |
| local PDS (`localRepo`) | its own Pds component, only when the DID is on `https://plc.directory` and the Pds is public through its ingress (a ComponentLink Requester -> Pds `pds.serviceauth`); otherwise none, with the reason in `atircReason` | `local-pds` or `none` |

- the market callbacks (submitBid, submitEvent, the RFP and accept signatures) are unchanged: this source signs only the
  atirc mint
- a `spec.agent` contract whose requester handle is `.local` (the default `localRepo` handle
  `<workspace>.requester.local`) or does not resolve to the requester DID is Failed before its RFP, with
  `refused: the atirc agent needs a requester handle that resolves to <did>: ...; start the requester with
  --atproto-handle <a domain you control> ...`; a plain VM contract of the same requester still runs
- a `spec.agent` contract whose `spec.secretsRef` path holds no bundle gets one from the requester: one entry at
  `agent.envFile` (`ATIRC_TOKEN=`, mode 0600), logged `requester_agent_bundle_placeholder`; no bao step is needed (the
  bao commands of [quickstart-local.md](quickstart-local.md) still work and win, a bundle that is there is kept). The path
  must be under `contracts/` of the requester mount

```bash
NAME=qs-agent-$(date +%s)
$SWC run request --state-dir "$R" --name "$NAME" --hold --agent -- --atproto-oauth-qr > "$R/$NAME.log" 2>&1 &
# sign in, wait for Held and requester_atirc_minted in the log
$SWC run request agent "$NAME" --state-dir "$R"     # guest.agentStart
# watch your channel with your own atirc token for the JOIN of <label>-<rkey>; prompt "<label>-<rkey>: uname -m"
$SWC run request release "$NAME" --state-dir "$R"
```

- the requester mints a leased token `<label>-<rfp rkey>.<handle>` after the verified receipt and writes it into the
  bundle (`/etc/atirc/atirc.env`, 0600); release revokes it before the bidder writes `vm.terminated`
- last pass: CLI, account alice, `aliceoa-<rkey>` joined and answered `x86_64` over atirc; revoked before vm.delete

## 6. Tenant Kubernetes Clusters: The Prod Gateway

The prod gateway at `k8s.socialweb.computer` owns tenant clusters. With service auth from your account (runbook
[0001](acceptance/0001-auth-modes.md) section 3):

```bash
xrpc procedure createCluster name="$CL"
xrpc query listClusters | jq -r --arg n "$CL" '.clusters[] | select(.name==$n) | .phase'   # until Ready
xrpc procedure getKubeconfig name="$CL" ttlSeconds:=28800 | jq -r .kubeconfig > "$W/kubeconfig"
chmod 600 "$W/kubeconfig"
kubectl --kubeconfig "$W/kubeconfig" apply -f examples/infra/k8s-cluster.yaml
```

- a pod that does not fit makes a NodeClaim; the gateway's Infra `nodes-<hex>` rents the node from a prod bidder
- running the gateway (cluster control plane and node market) inside your own requester is plan 0039 item C-GATEWAY-COMPOSE

## 7. A Long-Lived Requester From The CLI: run requester

`socialweb-computer run requester` (plan [0039](plans/0039-long-lived-requester.yaml) item A-RUN-REQUESTER) brings up the
same MarketRequester root as `run request` in a kcp of its own and keeps it up: no contract of its own, every listener on
port 0, state under `<state-dir>/requesters/<name>`. You `kubectl apply` Infra, EphemeralInfra or ComputeContract objects
against it, and its requester rents them from prod bidders under the default policy.

```bash
ST=~/.tmp/lr; mkdir -p "$ST"; chmod 700 "$ST"
$SWC run requester --name me --state-dir "$ST" -- --atproto-oauth-qr > "$ST/me.log" 2>&1 &
# sign in from the pendingAction url in the log (QR or scripts/oauth-signin.ts); never paste it anywhere
R=$ST/requesters/me/ready.json
until [ -s "$R" ]; do sleep 1; done                       # written atomically once the root is Ready
eval "$($SWC run requester env me --state-dir "$ST")"     # KUBECONFIG of the requester workspace, the DID
kubectl get marketrequesters.socialweb.computer app -o jsonpath='{.status.phase} {.status.did}{"\n"}'
sed "s|ssh-ed25519 AAAAREPLACE-WITH-YOUR-PUBLIC-KEY you@laptop|$(cat "$(jq -r .sshPublicKey "$R")")|" \
  examples/infra/vm.yaml | kubectl apply -f -             # pin a bidder with bidderDids when you need one
kubectl wait infras.socialweb.computer/quickstart-vm --for=jsonpath='{.status.phase}'=Deployed --timeout=3m
$SWC run requester ssh me --state-dir "$ST" --contract infra-quickstart-vm-vm1 --exec 'cat /root/quickstart'
$SWC run requester ssh me --state-dir "$ST" --contract infra-quickstart-vm-vm1 --transport webrtc --exec 'uname -m'
kubectl delete infras.socialweb.computer quickstart-vm    # releases the contract first
kill -INT %1                                              # Ctrl-C: stops the kcp, removes the ready file, keeps the state
```

- identity, after `--` (the MarketRequesterSpec options of `run request`, parsed by the same code): `--atproto-oauth-qr`,
  `--atproto-oauth`, or an existing account with an app password (`--atproto-identity-mode pdsAccount --atproto-handle
  <handle> --atproto-pds-url <pds>`, the password through the environment); without one it is a local PDS (`localRepo`,
  `--identity <name>` before `--` reuses a stored one); comms, ingress, requester ingress and `--secrets-fallback` too. A
  contract option (`--vm-name`, `--tunnel`, `--hold`, ...) is refused: contracts are kubectl objects here
- the ready file (`<dir>/ready.json`, and a second copy at `--ready-file <path>`): written to `<path>.tmp` then renamed,
  mode 0600, once the root is Ready; rewritten on every restart and when a field changes; removed on a clean stop.
  `{schema: 1, name, pid, startedAt, kcpUrl, kubeconfig, workspace, did, handle, identityMode, pdsUrl, ingress {pds,
  requester}, sshPublicKey}`: the bound kcp front, the path of a kubeconfig for the requester's workspace, the public
  ingress URLs from the Exposure status, the path of the requester's ssh public key; never a token, key or password. The
  same JSON (plus mode, network, instance, readyFile, restarted, seconds) is the summary on stdout
- foreground only: it blocks until Ctrl-C (or SIGTERM), then stops its kcp and keeps its state; `--fg` is accepted (the
  installed unit form). A second `run requester` on the same name is refused while the first holds `<dir>/owner.lock`
- restart: the same command (or the same command with nothing after `--`) adopts the units and keeps the DID, the vault
  and every contract; a Held contract stays Held across the restart and ssh works again. `<dir>/requester.json` records
  the first start's network, `--identity` and options; a restart with other options is refused (start a new name)
- `run requester env <name>` prints `KUBECONFIG`, `SOCIALWEB_KCP_HOST`, `SOCIALWEB_KCP_WORKSPACE`, the DID and the ssh public
  key path from the ready file (`--json`); `run requester ssh <name> --contract <cc>` is the `run request ssh` code over
  any Held contract, with the requester's own key and `StrictHostKeyChecking=yes` against the guest's reported host keys
  (`--transport webrtc` needs a webrtc tunnel on the contract, as in `vm.yaml`)
- vouches: as section 2; the requester's own record for the bidder can be the `vouch.yaml` AtprotoRecord applied into
  this kcp (its repo is `repo-<did with - for :>`)
- proven (2026-11-01, own market network, `--network`, bidder policy `bidder-tangled-vouch`): ready 11 s, `vm.yaml`
  Deployed 20 s after the apply, ssh over iroh and webrtc printed `quickstart-vm-ok`; Ctrl-C and restart: same DID, ready
  file rewritten in 1.4 s, contract still Held, ssh again. The standalone path against prod bidders with an OAuth QR
  sign-in is the lead's run (acceptance rules)

### Ingress options per identity

Two ingresses, two jobs. Both go after `--` (rows of `deploy/apps/option-map.yaml`, app `market-requester`); the ready
file reports both public URLs (`ingress.pds`, `ingress.requester`).

- PDS ingress (`spec.ingress`, a local PDS only): makes the DID document's `atproto_pds` endpoint, the relay crawl, the
  bidders' record reads and irc.fedfork.com's lease reads reach the local PDS
- requester ingress (`spec.requesterIngress`, every identity): submitBid, submitEvent and getSecrets; the agent's
  `ATIRC_TOKEN` reaches the guest through getSecrets

| identity | PDS ingress | requester ingress | agent mint |
|---|---|---|---|
| OAuth QR / browser | none (the account's PDS is public) | as below | mints |
| pdsAccount (app password) | none (the account's PDS is public) | as below | mints through the owner MarketIdentity |
| localRepo (own Pds) | `--ingress-xrpc-host xrpc.fedproxy.com` (the did:key ingress relay; the standalone default, public URL `https://<did label>.xrpc.fedproxy.com`); `--ingress-provider opentunnel --ingress-opentunnel-command ... --ingress-opentunnel-route-name ...`; `--ingress-public-host <host>` (with `--ingress-relay-url <your relay>`) for `https://<did label>.<host>` behind your own edge | as below | mints only with the DID on plc.directory (standalone default `--plc-directory-url https://plc.directory`), that PDS ingress Ready, and `--atproto-handle <a domain you control>` whose DNS TXT `_atproto.<handle>` or `https://<handle>/.well-known/atproto-did` names the DID |

Requester ingress, every identity:

| option | effect |
|---|---|
| (none, comms `XRPC` or `Both`) | the app ingress (standalone: xrpc-subscription through xrpc.fedproxy.com) |
| `--requester-ingress-provider webrtc` (`--requester-ingress-webrtc-stun stun:...`) | bids, events and getSecrets over a webrtc datachannel |
| `--requester-ingress-provider xrpc-subscription`, `opentunnel` or `iroh` | the same endpoints through that provider |
| `--comms-mode Firehose` | no inbound endpoint at all: add `--secrets-fallback Sealed`, or a contract with `spec.secretsRef` (every agent contract) fails with "secrets need the requester ingress (getSecrets) or secretsFallback: Sealed" |

- a localRepo requester with a public handle, its PDS behind xrpc.fedproxy.com and the agent:
  `$SWC run requester --name me --state-dir "$ST" -- --atproto-handle me.example.org` (the defaults give
  plc.directory and xrpc.fedproxy.com); publish `did=<the ready file's did>` as DNS TXT `_atproto.me.example.org`
  before the first agent contract; the core log shows `requester_service_auth ... "atircSigning":"local-pds"`
- proven (2026-11-01, own market network, localRepo with the default `.local` handle, bidder policy
  `bidder-tangled-vouch`): `atircSigning` `none` ("the DID is on the network PLC"), no `pds.serviceauth` link;
  `agent-vm.yaml` with no bao step: placeholder bundle written (`/etc/atirc/atirc.env`, 0600), contract Failed
  `refused: ... --atproto-handle ...` with no RFP; `vm.yaml` of the same requester Deployed in 22 s, ssh printed
  `quickstart-vm-ok`. A real mint from a local PDS is the lead's prod run

Still planned in plan 0039:

- `install requester` (item B-INSTALL-REQUESTER): the same process as a systemd user unit in `socialweb.slice` of an
  account that install creates when asked, with `config.json` and `upgrade`, as `install bidder` does
- the gateway in the same kcp (item C-GATEWAY-COMPOSE): `kubectl apply` a Cluster, its k3s control plane runs as
  HostProcesses of the requester's core, and its nodes are rented by the same requester
- `examples/infra/mixed-prod.yaml` and acceptance `docs/acceptance/0013-mixed-prod.md` (item F-MIXED-PROD, not written yet)

## Not Supported On Prod Today

| gap | where it is planned |
|---|---|
| `run requester` as an installed systemd unit (today it runs in your terminal) | plan 0039 B-INSTALL-REQUESTER |
| a tenant cluster without the prod gateway | plan 0039 C-GATEWAY-COMPOSE |
| an atirc agent from a local PDS identity or a pdsAccount identity: built, not yet proven against irc.fedfork.com | plan 0039 D-AGENT-IDENTITY (the lead's prod run) |
| `vouch-bidder-operator.yaml` on an installed bidder whose operator did not set `--bidder-operator-signin`, or before the QR page (qr.fedfork.com) asks for `repo:sh.tangled.graph.vouch` | operator setting; the QR page's scope |
| deno on a prod bidder unless its operator ran `run bidder --compute-kinds vm,deno`; pin such a bidder with `bidderDids` | operator setting |
