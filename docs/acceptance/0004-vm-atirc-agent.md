# Acceptance 0004: atirc agent in a market VM (plan 0035)

Proves a requester can ask for an atirc agent on its guest that joins the owner's atirc mesh peer to peer,
with no atirc token: after the verified receipt the requester writes the agent's iroh key into the secrets
bundle and `computer.socialweb.peer/<label>-<rfp rkey>.<handle>` (leased on a contract-scoped
market.association) into its own repo, `atirc-agent.service` starts only on a requester-originated
`guest.agentStart` and runs `atirc mesh run --no-publish` beside `atirc agent run`, another mesh node of
the owner reaches the agent, and release deletes the peer record and its lease.

- Plan [0035](../plans/0035-atirc-agent.yaml) item ACC35-AGENT; the p2p cut-over (P6)
- Paths: CLI (`run request --agent`), page (Agent checkbox + row Agent button), iOS app ([0010](0010-ios-app.md))
- Last pass: 2026-11-10 CLI, alice (pdsAccount) on a prod bidder from hat-8, owner node an AtircNode of an own market network; two workarounds until the bidders roll out: the pinned atirc build copied onto the guest, `--jetstream off` on both nodes (see Known pitfalls)

```mermaid
sequenceDiagram
  participant R as requester (CLI / page / iOS)
  participant P as requester's PDS
  participant B as bidder
  participant G as guest
  participant N as owner mesh node (AtircNode)
  R->>B: RFP (spec.agent + spec.secretsRef)
  B->>R: receipt
  R->>R: iroh key + ATP_USERNAME/ATIRC_NICK into the secrets bundle (vault, 0600)
  R->>P: market.association (subject = own DID, contract, expiresAt) + computer.socialweb.peer/<nick>.<handle> {iroh, lease}
  B->>G: boot; atirc-agent.service installed, inactive
  R->>B: ContractEvent outbound guest.agentStart
  B->>G: systemctl start --no-block atirc-agent.service
  G->>G: atirc mesh run --no-publish as the nick user
  N->>G: iroh dial (ALPN atirc/mesh/1), both sides verify the peer records
  R->>P: release: delete the peer record and its lease
  R->>B: release -> vm.terminated
```

## Preconditions

| Item | Need |
| --- | --- |
| Requester | an account whose handle resolves to its DID (alice `aliceoa.bsky.social` with `--atproto-identity-mode pdsAccount`, or an OAuth session); no atirc scope is needed any more |
| Vouches | as 0003 (bidder-tangled-vouch + requester-tangled-vouch); an own market network may run `--bidder-policy open` |
| Bidders | serve `atirc-x86_64` from `socialweb-guest-downloads` built from `assets.atirc` (a `cargosrc` pin with `atirc mesh`) |
| Mesh node | an owner mesh node: an `AtircNode` (examples/infra/atircnode.yaml) or `atirc mesh run`, with its own `computer.socialweb.peer` record in the owner's repo |

## Steps

CLI:

```bash
SWC=socialweb-computer; R=~/.tmp/acc35; mkdir -p "$R"; chmod 700 "$R"
NAME=cli-agent-$(date +%s)
# --agent last (or --agent=<envFile>): a bare --agent before another flag swallowed it (pre rc126)
$SWC run request --state-dir "$R" --name "$NAME" --hold --agent=/etc/atirc/atirc.env -- --atproto-oauth-qr \
  > "$R/$NAME-request.log" 2>&1 &
# sign in from the pendingAction url (as 0003), wait for Held + requester_agent_peer_published
atirc peer show <label>-<rfp rkey>.<handle>      # the binding verifies: node, owner (the lease chain's root)
$SWC run request agent "$NAME" --state-dir "$R"  # publishes guest.agentStart, waits Published
# an owner node (AtircNode with spec.peers: [<label>-<rfp rkey>.<handle>]) lists the agent in `atirc mesh peers`;
# `atirc mesh pub hello` there, then on the guest as the nick user `atirc mesh sub --cursor 0` shows it
$SWC run request release "$NAME" --state-dir "$R"
# both records answer RecordNotFound
```

Page: Compute panel, Agent checkbox on (default), Request VM, wait Held, row **Agent**,
wait the event `Published`, see the agent in the owner node's peers, **Release**, keep the tab open 30 s.

## Expected observations

| Event | Where | Typical |
| --- | --- | --- |
| `requester_agent_mesh_key_written envFile=/etc/atirc/atirc.env keyFile=/etc/atirc/iroh_key.hex mode=0600` | requester log | right after the receipt |
| `requester_agent_lease_written uri=at://<did>/...market.association/requester_associate-...` | requester log | same second |
| `requester_agent_peer_published identity=<label>-<rkey>.<handle>` | requester log | same second |
| `Held`, condition `AtircTokenMinted=True reason=PeerPublished` | contract | 29-45 s after request |
| `guest_agent_start unit=atirc-agent.service` | guest agent journal | ~1 s after agentStart |
| `atirc mesh run` running as the nick user, `~/.config/atirc/iroh_key` 0600 | guest | after agentStart |
| the owner node verifies and reaches `<label>-<rkey>.<handle>` | mesh node log | seconds |
| `requester_agent_peer_deleted`, `requester_association_deleted` | requester log | at release |

## Pass criteria

- No `chat.atirc.mint` for the agent; no token in the bundle, a kcp object, a record, a ContractEvent or a log line
- The key never in a kcp object, a record or a log line; `/etc/atirc/iroh_key.hex` and `/etc/atirc/atirc.env` root, 0600
- The peer record's `iroh` is the short ticket of the bundle key; its `lease` names the association at its CID
- After `Held`, before agentStart: `atirc-agent.service` inactive
- After agentStart: unit active, the owner node reaches the agent
- Release: the peer record and the lease are gone (a getRecord answers RecordNotFound); a skipped delete is harmless after the lease's `expiresAt`
- Without `--agent`: no key, no record, no unit, no atirc binary on the guest
- Winner host empty after release

## Cleanup

- Release every test VM; never touch another driver's VM (owner VMs are held, do not release)
- A leftover peer record or lease: delete it by its key (one deleteRecord each); its `expiresAt` ends it anyway
- Orphan VM: [0007](0007-bidder-termination.md)

## Known pitfalls

| Symptom | Cause / fix |
| --- | --- |
| `requester_atirc_revoked ... already=true`, no ndjson entry | revoke used the dead child token after termination (401 mapped to already); fixed rc132: revoke before destroy, parent / account credentials |
| CLI release, token alive | requester exited before the termination POST (pre rc132); now revoke first |
| `spec.agent` contract fails before RFP | `an agent needs spec.secretsRef`: `--agent` adds a one-entry bundle; page must send one |
| the owner node never dials the agent | the peer record's `channels` must hold the daemon's channel (the owner-graph root's, e.g. `#johnandersen777_bsky_social` for alice); the requester walks `com.fedfork.irc.agentOperator` / `com.fedfork.irc.agents` to find it |
| `atirc mesh run` panics: `Could not automatically determine the process-level CryptoProvider` | an atirc older than a6a3f0f (both rustls providers, no default); the pinned `assets.atirc` installs ring |
| `atirc agent run`: `no harness configured` | without a token the harness comes from the agent user's local config (`[agent] harness` in `<config dir>/config.toml`); kcp ships none, so the agent answers only once one is configured; the mesh daemon is the unit's main process and keeps running |
| guest has no `atirc mesh` | the bidder serves an older socialweb-guest-downloads; roll the bidder to a build with the `assets.atirc` cargosrc pin |
| oauthQr bidder SessionExpired after rollout | scope added to Bidder/RequesterOAuthScope: re-sign every OAuth identity (announce before tagging) |
| harness never joins | no agent config for the owner chain (`atirc config agent`), or claude still installing (give it ~1 min) |
| release after page reload slow (20-38 s) | requester start after boot + release not first; fixed rc134 (release-first on resume, ~17 s click -> Released) |
