# Acceptance 0004: atirc agent in a market VM (plan 0035)

Proves a requester can ask for an atirc agent on its guest that joins the owner's atirc mesh peer to peer,
with no atirc token: after the verified receipt the requester writes the agent's iroh key and agent config into the
secrets bundle. In the VM the guest requester R2 holds the agent as a co-located account: a did:plc on the VM localPDS
whose DID doc says `https://ident.link/agent-of/<R2 DID>`. The renter endorses and grants R2 (rkey = R2 DID, after the
nonce check), R2 endorses and grants the agent in its own repo and writes the agent's peer record
`computer.socialweb.peer/<name>.<R2 handle>` in the agent's repo (no `lease`). `atirc-agent.service` starts only on
a requester-originated `guest.agentStart` and runs `atirc mesh run --no-publish` beside `atirc agent run`, another mesh
node of the owner reaches the agent (walk: agent -> R2 -> renter) and the agent answers on the mesh. Release deletes the
endorsements and grants and tombstones both DIDs.

- An invocation that asks for no R2 starts R2 identity-only (no RFP, no nested VM) with the agent as its co-located
  account (`run request --agent` implies `--associate guest`; the guest agent starts socialweb-r2.service with
  `R2_IDENTITY=1` and `R2_COLOCATED=<nick>=<agent iroh ticket>` from `/etc/socialweb-computer/r2-agent.env`)
- Design: [../design/ident-link-associations.md](../design/ident-link-associations.md) (Q1)
- Plan [0035](../plans/0035-atirc-agent.yaml) item ACC35-AGENT; the p2p cut-over (P6)
- Paths: CLI (`run request --agent`), page (Agent checkbox + row Agent button), iOS app ([0010](0010-ios-app.md))
- Last pass: 2026-11-11 04:00-04:05Z CLI, alice (pdsAccount) on a prod bidder, rollout 11 (kcp 06d04283, atirc 2de8b6c, guest atirc fecce97e), `--agent-config` (claude on the deepseek model), owner node `atirc mesh run` as alice on hat-8 (`--peer <agent>`, both channels): no mint, no token in the bundle, files root 0600, unit inactive until agentStart; the agent carries `#johnandersen777_bsky_social` and its own `#<label>-<rkey>_aliceoa_bsky_social` and monitors with `atirc mesh sub --others`; online on both 26 s after agentStart; `uname -m` on the owner channel answered `x86_64` and `hostname` on its own channel (1:1) answered there, both 6 s after posting; a browser (atlibs-atirc, headless Chrome over WebRTC to the owner node) asked `nproc`, answered in 3 s; release: peer record and lease RecordNotFound x3, hosts empty. That pass predates the ident.link cut-over (renter-repo peer record + lease): not yet re-run on the flow below

```mermaid
sequenceDiagram
  participant R as requester (CLI / page / iOS)
  participant P as requester's PDS
  participant B as bidder
  participant G as guest (R2 + VM localPDS)
  participant N as owner mesh node (AtircNode)
  R->>B: RFP (spec.agent + spec.secretsRef)
  B->>R: receipt
  R->>R: iroh key, agent config, association request {principal, contract, nonce, rotationKey} into the secrets bundle (vault, 0600)
  B->>G: boot; atirc-agent.service installed, inactive
  G->>G: R2 identity-only: did:plc for R2 and the agent, alsoKnownAs agent-of
  G->>R: guest.requesterDid; R2 repo market.associationRequest/<renter DID>
  R->>P: market.association/<R2 DID> {agent, contract, expiresAt}, then link.ident.endorsement/<R2 DID>
  G->>G: R2 repo: market.association/<agent DID>, link.ident.endorsement/<agent DID>; agent repo: computer.socialweb.peer/<name>.<R2 handle>
  R->>B: ContractEvent outbound guest.agentStart
  B->>G: systemctl start --no-block atirc-agent.service
  G->>G: atirc mesh run --no-publish as the nick user, ATP_USERNAME=<name>.<R2 handle>, ATIRC_DID=<agent DID>, no ATIRC_NICK
  N->>G: iroh dial (ALPN atirc/mesh/1), both sides walk the peer record's repo DID
  R->>P: release: delete the endorsement, then the grant
  G->>G: R2 retire: tombstone the R2 and agent DIDs
  R->>B: release -> vm.terminated
```

## Preconditions

| Item | Need |
| --- | --- |
| Requester | an account whose handle resolves to its DID (alice `aliceoa.bsky.social` with `--atproto-identity-mode pdsAccount`, or an OAuth session); no atirc scope is needed any more |
| Vouches | as 0003 (bidder-tangled-vouch + requester-tangled-vouch); an own market network may run `--bidder-policy open` |
| Bidders | serve `atirc-x86_64` from `socialweb-guest-downloads` built from `assets.atirc` (a `cargosrc` pin with `atirc mesh`) |
| Mesh node | an owner mesh node: an `AtircNode` (examples/infra/atircnode.yaml) or `atirc mesh run`, with its own `computer.socialweb.peer` record in the owner's repo |
| Scopes | an OAuth renter: the optional `repo:link.ident.endorsement?action=create&action=delete` (without it `requester_endorsement_skipped` and R2 stays unassociated: sign in again) |
| Agent config | an atirc client config file on the requester host, mode 0600: `[agent] harness` (a `#!` script that starts the agent, e.g. claude, and reads its settings with `atirc agent anthropic model|base-url|api-key`), optional `addPath`, `[agent.anthropic] model, baseUrl, apiKey`; the owner's `~/.config/atirc/config.toml` is one (its harness installs claude from claude.ai when missing). Never print it |

## Steps

CLI:

```bash
SWC=socialweb-computer; R=~/.tmp/acc35; mkdir -p "$R"; chmod 700 "$R"
NAME=cli-agent-$(date +%s)
# --agent last (or --agent=<envFile>): a bare --agent before another flag swallowed it (pre rc126)
$SWC run request --state-dir "$R" --name "$NAME" --hold --agent-config ~/.config/atirc/config.toml --agent=/etc/atirc/atirc.env -- --atproto-oauth-qr \
  > "$R/$NAME-request.log" 2>&1 &
# sign in from the pendingAction url (as 0003), wait for Held, requester_agent_mesh_key_written, requester_association_written
PEER=$(grep -m1 requester_agent_identity "$R/$NAME-request.log" "$R/requests/$NAME/logs/core.log" | sed 's/.*"peer":"\([^"]*\)".*/\1/')
echo "$PEER"                                     # <name>.<R2 handle>@<agent DID>
atirc peer show "$PEER"                          # the binding verifies: node, owner (a root of the walk)
$SWC run request agent "$NAME" --state-dir "$R"  # publishes guest.agentStart, waits Published
# an owner node (`atirc mesh run --peer "$PEER"`, or AtircNode spec.peers: ["$PEER"]) lists the agent in `atirc mesh peers`;
# `atirc mesh pub hello` there, then on the guest as the nick user `atirc mesh sub --cursor 0` shows it;
# a guest-side `atirc mesh pub` shows up in the owner node's `atirc mesh sub --cursor 0` (both directions);
# prompt the agent once on the mesh (`atirc mesh pub '<nick>: uname -m'` on the owner node) and see its answer
```

Check the pair + grant chain while Held (read only):

```bash
RENTER=<renter DID>
R2=<R2 DID>       # `subject` of requester_association_written in "$R/$NAME-request.log"
pds() { curl -s "https://plc.directory/$1" | jq -r '.service[] | select(.id=="#atproto_pds") | .serviceEndpoint'; }
rec() { curl -s "$(pds "$1")/xrpc/com.atproto.repo.getRecord?repo=$1&collection=$2&rkey=$3" | jq -c '.value // .error'; }
AGENT=$(curl -s "$(pds "$R2")/xrpc/com.atproto.repo.listRecords?repo=$R2&collection=link.ident.endorsement" | jq -r '.records[].value.agent')
rec "$RENTER" com.publicdomainrelay.temp.market.association "$R2"  # {agent: R2, contract: the accept, expiresAt, createdAt}
rec "$RENTER" link.ident.endorsement "$R2"                         # {agent: R2}
rec "$R2" com.publicdomainrelay.temp.market.association "$AGENT"   # {agent, same contract + expiresAt}
rec "$R2" link.ident.endorsement "$AGENT"                          # {agent}
rec "$AGENT" computer.socialweb.peer "<name>.<R2 handle>"       # {iroh, createdAt}, no lease
curl -s "https://plc.directory/$R2" | jq -c .alsoKnownAs           # at://<R2 handle>, https://ident.link/agent-of/$RENTER
curl -s "https://plc.directory/$AGENT" | jq -c .alsoKnownAs        # at://<name>.<R2 handle>, https://ident.link/agent-of/$R2
for d in "$R2" "$AGENT"; do curl -s "https://plc.directory/$d/data" | jq -c .rotationKeys; done
# [renter key, VM localPDS key]; renter key = `find "$R" -name plc-rotation.pub` (a localRepo renter);
# an OAuth or pdsAccount renter has no local PDS: [VM localPDS key] only
$SWC internal association "$AGENT" | jq -c '{ancestors, roots, expiresAt, errors}'  # roots [RENTER], R2 depth 1
```

Release and its checks:

```bash
$SWC run request release "$NAME" --state-dir "$R"
for i in 1 2 3; do
  rec "$RENTER" link.ident.endorsement "$R2"; rec "$RENTER" com.publicdomainrelay.temp.market.association "$R2"
done                                                                # RecordNotFound x6
for d in "$R2" "$AGENT"; do
  curl -s -o /dev/null -w '%{http_code}\n' "https://plc.directory/$d"  # 410
  curl -s "https://plc.directory/$d/log/last" | jq -r .type          # plc_tombstone
done
```

Page: Compute panel, Agent checkbox on (default), Request VM, wait Held, row **Agent**,
wait the event `Published`, see the agent in the owner node's peers, **Release**, keep the tab open 30 s.

## Expected observations

| Event | Where | Typical |
| --- | --- | --- |
| `requester_agent_mesh_key_written envFile=/etc/atirc/atirc.env keyFile=/etc/atirc/iroh_key.hex mode=0600` | requester log | right after the receipt |
| `requester_association_request_entry path=/etc/socialweb-computer/association-request.json` | requester log | same second |
| `requester_association_written subject=<R2 DID> uri=at://<renter>/com.publicdomainrelay.temp.market.association/<R2 DID>` | requester log | after R2 reports its DID and its associationRequest checks (principal, contract, nonce) |
| AtprotoRecords `peer`, `colocated-<name>-grant`, `colocated-<name>-endorsement`, `colocated-<name>-peer` Applied | R2 core log (guest journal, `socialweb-r2.service`) | after the association ref arrived |
| `Held`, condition `AtircTokenMinted=True reason=PeerPublished` | contract | 29-45 s after request |
| `guest_agent_start unit=atirc-agent.service` | guest agent journal | ~1 s after agentStart |
| `atirc mesh run` running as the nick user, `~/.config/atirc/iroh_key` and `config.toml` 0600, owned by it | guest | after agentStart |
| the harness starts (claude installs first when missing, ~1 min), posts `<nick> online` and answers a mesh prompt | owner node `atirc mesh sub` | online ~30 s after agentStart; an answer ~25 s after the prompt |
| the owner node verifies and reaches `--peer <name>.<R2 handle>@<agent DID>` (renter log `requester_agent_identity`) | mesh node log | seconds |
| `requester_association_deleted rkey=<R2 DID>` | requester log | at release |
| `local_account_retired did=<DID> plc=tombstoned` for the agent and R2 | R2 core log | at release |

## Pass criteria

- No `chat.atirc.mint` for the agent; no token in the bundle, a kcp object, a record, a ContractEvent or a log line
- The key and the agent config (its apiKey) never in a kcp object, a record, an argument or a log line; `/etc/atirc/iroh_key.hex`, `/etc/atirc/atirc.env` and `/etc/atirc/config.toml` root, 0600
- The peer record is in the agent's own repo, its `iroh` is the short ticket of the bundle key, no `lease`
- Owner = the walk root (the renter): alice and bob are no longer john's atirc agents (retired 2026-11-11), so an alice agent carries `#aliceoa_bsky_social`, not `#johnandersen777_bsky_social`; the owner node runs as the renter
- The agent DID doc: `agent-of/<R2 DID>`; R2's: `agent-of/<renter DID>`; rotationKeys as in the check above
- `internal association <agent DID>` exits 0, roots = [renter DID], `expiresAt` = the grants' `expiresAt`
- The renter repo holds no peer record and no self-association for the agent: only the grant + endorsement at rkey = R2 DID
- After `Held`, before agentStart: `atirc-agent.service` inactive
- After agentStart: unit active, the owner node reaches the agent, a message crosses the mesh in both directions, the agent answers one prompt
- Release: the renter's endorsement and grant at rkey = R2 DID answer RecordNotFound (3 reads); R2 and agent DIDs 410 /
  `plc_tombstone`; a skipped delete is harmless after the grant's `expiresAt` or once the contract is not Held
- Without `--agent`: no key, no record, no unit, no atirc binary on the guest
- Winner host empty after release

## Cleanup

- Release every test VM; never touch another driver's VM (owner VMs are held, do not release)
- A leftover renter grant or endorsement: delete endorsement, then grant, at rkey = R2 DID (one deleteRecord each); the
  grant's `expiresAt` and the contract's end stop trust anyway
- Superseded: a renter-repo `computer.socialweb.peer/<label>-<rfp rkey>.<handle>` or `market.association/requester_associate-...`
  of a run before the cut-over is an old record: delete it by its key
- Orphan VM: [0007](0007-bidder-termination.md)

## Known pitfalls

| Symptom | Cause / fix |
| --- | --- |
| `requester_atirc_revoked ... already=true`, no ndjson entry | revoke used the dead child token after termination (401 mapped to already); fixed rc132: revoke before destroy, parent / account credentials |
| CLI release, token alive | requester exited before the termination POST (pre rc132); now revoke first |
| `spec.agent` contract fails before RFP | `an agent needs spec.secretsRef`: `--agent` adds a one-entry bundle; page must send one |
| the owner node never dials the agent | the walk of the peer record's repo DID must reach the owner node's owner: `socialweb-computer internal association <agent DID>` (roots, errors); a missing renter endorsement (`requester_endorsement_skipped`: the optional scope) leaves the chain at R2 |
| an agent ignores a prompt that starts with its nick | atirc before 2de8b6c: the agent filtered its "own echoes" by text; 2de8b6c's prompt monitors with `atirc mesh sub --others` (by identity) |
| the owner node reaches the agent's own channel only with it in `-c` | the agent carries `#<identity>` beyond the record's `channels` (hellos list both); the owner node opens it when it carries it too |
| `atirc mesh run` panics: `Could not automatically determine the process-level CryptoProvider` | an atirc older than a6a3f0f (both rustls providers, no default); the pinned `assets.atirc` installs ring |
| `atirc agent run`: `no harness configured` | no `--agent-config` (or no `/etc/atirc/config.toml` in a kubectl bundle): without a token the harness comes only from the agent user's `config.toml`; the mesh daemon is the unit's main process and keeps running |
| `run request: --agent-config: ... readable by group or others` | chmod 600 the config file |
| guest has no `atirc mesh` | the bidder serves an older socialweb-guest-downloads; roll the bidder to a build with the `assets.atirc` cargosrc pin |
| oauthQr bidder SessionExpired after rollout | scope added to Bidder/RequesterOAuthScope: re-sign every OAuth identity (announce before tagging) |
| harness never answers | the harness exited (journalctl -u atirc-agent), claude still installing (give it ~1 min), or a wrong model / baseUrl / apiKey in the agent config |
| release after page reload slow (20-38 s) | requester start after boot + release not first; fixed rc134 (release-first on resume, ~17 s click -> Released) |
