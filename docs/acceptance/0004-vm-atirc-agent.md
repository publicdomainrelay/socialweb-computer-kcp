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
- Last pass: 2026-11-12 21:13-21:18Z, kcp e190a3b1 (release 66f3bd4d, rollout 19), alice by QR, winner hat-8: association
  +103 s, online +180 s, answer 3.0 s, `r2Released` 8.8 s after release, the CLI stops 5.3 s after Released, 0 ERROR.
  Before: 2026-11-12 18:38-18:44Z, kcp cdd93727 (release 6380eee4, rollout 16), alice by QR, winner hat-8: the release
  answers at once: `r2Released` posted by the guest 8 s after release, `requester_guest_requester_released` 18:44:05Z,
  Released 0.7 s later, the CLI stops 5.5 s after Released; records 14/14, RecordNotFound x6, both DIDs `plc_tombstone` by
  the guest; requester `core.log` 0 ERROR (no startup noise). The full flow before it: 2026-11-12 17:16-17:29Z, kcp
  60d0d3b7 (release 0b68ceeb, rollout 15), atirc 9e83813, alice by QR
  (`cli-agent-1794503795`), no bidders list (winner hat-8), `--agent-config` (claude on the deepseek model), no workaround.
  Held 17:18:24Z; before agentStart both units inactive, `/etc/atirc/*` root 0600, r2 RSS 12.8 MiB. agentStart 17:18:36Z;
  R2 reported 17:19:13Z, `requester_association_written` 17:19:14Z (grant contract-scoped, `expiresAt` accept + 24 h,
  endorsement `{agent}`), `requester_agent_identity` 17:19:14Z; the ref reached `/run/socialweb-r2` (no hand copy). Walk of
  the agent: R2 depth 1, alice depth 2, roots [alice]; `atirc peer show` owner alice, `expiresAt` = the grant's; peer
  record in the agent repo, no `lease`; rotationKeys [renter key, VM localPDS key] on both DIDs. The owner node (`--peer`)
  lists the agent; `<nick> online` 17:22:47Z; messages both ways; `<nick>: uname -m` answered `x86_64` in 3.2 s. Release:
  grant + endorsement RecordNotFound x3, both DIDs `plc_tombstone` by the guest (4 s after release), records 14/14, hosts
  empty; the CLI stops 5 s after Released (`the guest's DIDs are tombstoned`). Open: the renter waits the full 3 min
  (`requester_guest_requester_release_timeout`) because the bidder refused the guest's `r2Released` (fixed in 36b05233).
  On rollout 14 (kcp f482083c, 2026-11-11 16:07-16:21Z) the same flow passed with the renter tombstoning both DIDs after
  2 min. Before: 2026-11-11 12:49-13:50Z, kcp e496cbff, **FAIL** (OAuth renters could not endorse R2, the association ref
  was never mirrored, release waited 24 min; fixed in 94c4b74b, 982ba545, 51a9bbb8, 66339480)
- Earlier pass: 2026-11-11 04:00-04:05Z CLI, alice (pdsAccount) on a prod bidder, rollout 11 (kcp 06d04283, atirc 2de8b6c, guest atirc fecce97e), `--agent-config` (claude on the deepseek model), owner node `atirc mesh run` as alice on hat-8 (`--peer <agent>`, both channels): no mint, no token in the bundle, files root 0600, unit inactive until agentStart; the agent carries `#johnandersen777_bsky_social` and its own `#<label>-<rkey>_aliceoa_bsky_social` and monitors with `atirc mesh sub --others`; online on both 26 s after agentStart; `uname -m` on the owner channel answered `x86_64` and `hostname` on its own channel (1:1) answered there, both 6 s after posting; a browser (atlibs-atirc, headless Chrome over WebRTC to the owner node) asked `nproc`, answered in 3 s; release: peer record and lease RecordNotFound x3, hosts empty. That pass predates the ident.link cut-over (renter-repo peer record + lease): not yet re-run on the flow below

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
# sign in from the pendingAction url (as 0003), wait for Held and requester_agent_mesh_key_written
$SWC run request agent "$NAME" --state-dir "$R"  # publishes guest.agentStart, waits Published; the guest starts
                                                 # atirc-agent.service and only then R2 identity-only
CL="$R/requests/$NAME/dev/$NAME/logs/core.log"   # the renter's core log while it runs ($R/requests/$NAME/logs/ after release)
# wait for requester_association_written and requester_agent_identity (R2 reports its DID ~30 s after agentStart)
PEER=$(grep -h -m1 requester_agent_identity "$CL" | sed 's/.*"peer":"\([^"]*\)".*/\1/')
echo "$PEER"                                     # <name>.<R2 handle>@<agent DID>
atirc peer show "$PEER"                          # the binding verifies: node, owner (a root of the walk)
# an owner node started (or restarted) with `atirc mesh run --peer "$PEER"` (or AtircNode spec.peers: ["$PEER"]; a node
# without it never reached the agent on 2026-11-11) lists the agent in `atirc mesh peers`;
# `atirc mesh pub hello` there, then on the guest as the nick user `atirc mesh sub --cursor 0` shows it;
# a guest-side `atirc mesh pub` shows up in the owner node's `atirc mesh sub --cursor 0` (both directions);
# prompt the agent once on the mesh (`atirc mesh pub '<nick>: uname -m'` on the owner node) and see its answer
```

Check the pair + grant chain while Held (read only):

```bash
RENTER=<renter DID>
R2=<R2 DID>       # `subject` of requester_association_written in "$CL"
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
# [renter key, VM localPDS key]; renter key = the requester component's plc-rotation.key (made on first use, also
# for an OAuth or pdsAccount renter: 2 keys on 2026-11-11), `requester_tombstoned ... key=` names it
$SWC internal association "$AGENT" | jq -c '{ancestors, roots, expiresAt, errors}'  # roots [RENTER], R2 depth 1
```

Release and its checks:

```bash
$SWC run request release "$NAME" --state-dir "$R"
for i in 1 2 3; do
  rec "$RENTER" link.ident.endorsement "$R2"; rec "$RENTER" com.publicdomainrelay.temp.market.association "$R2"
done                                                                # RecordNotFound x6
for d in "$R2" "$AGENT"; do
  curl -s -o /dev/null -w '%{http_code}\n' "https://plc.directory/$d"  # 404 (plc.directory answers 404 for a tombstone)
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
| `requester_association_written subject=<R2 DID> uri=at://<renter>/com.publicdomainrelay.temp.market.association/<R2 DID>` | renter core log (`$CL`) | after agentStart: R2 identity-only starts then; after R2 reports its DID and its associationRequest checks (principal, contract, nonce) |
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
- Release: the renter's endorsement and grant at rkey = R2 DID answer RecordNotFound (3 reads); R2 and agent DIDs 404 /
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
| `requester_association_failed` `Missing required scope "repo:link.ident.endorsement?action=update"` | kcp e496cbff wrote the endorsement with putRecord; fixed in 94c4b74b (createRecord), passed 2026-11-11 |
| R2 `waiting for the association ref` forever, `/run/socialweb-r2/association-ref` empty | kcp e496cbff mirrored the ref only with a non-empty `r2-atirc.env`; fixed in 982ba545, passed 2026-11-11 |
| release waits 3 min, `requester_guest_requester_release_timeout`; guest journal `the bidder refused a guest event` `r2Released` 400 `message is printable ASCII of at most 512 bytes` | the identity-only R2 retires in ~7 s, but the bidder's report check (`internal/guestidentity` `printableASCII`) rejected the space in the guest's `r2Released` message (kcp 60d0d3b7 and before); fixed in 36b05233, passed 2026-11-12 (8.5 s) |
| the CLI waits 3 min after Released and logs `the guest's DIDs are not tombstoned` though they are | kcp before 978ef53a checked for HTTP 410; plc.directory answers 404 for a tombstone; fixed (reads `log/last`) |
| guest journal during release | `run request ssh` ends when the contract leaves Held; stream it with plain `ssh` through the same `dumbpipe connect <ticket>` ProxyCommand the CLI's own ssh process uses |
| release after page reload slow (20-38 s) | requester start after boot + release not first; fixed rc134 (release-first on resume, ~17 s click -> Released) |
