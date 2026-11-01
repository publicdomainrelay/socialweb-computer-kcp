# Acceptance 0004: atirc agent in a market VM (plan 0035)

Proves a requester can ask for an atirc agent on its guest: a leased atirc token is
minted at the RFP into the secrets bundle, `atirc-agent.service` starts only on a
requester-originated `guest.agentStart`, the agent joins the owner channel, and
release revokes the token before the bidder writes `vm.terminated`.

- Plan [0035](../plans/0035-atirc-agent.yaml) item ACC35-AGENT
- Paths: CLI (`run request --agent`), page (Agent checkbox + row Agent button), iOS app ([0010](0010-ios-app.md))
- Last pass: CLI rc132 smoke 2026-10-31 (revoke before destroy, 200); page prod rc135 2026-10-31 02:55Z (revoke via=account 1.4 s before termination)

```mermaid
sequenceDiagram
  participant R as requester (CLI / page / iOS)
  participant A as atirc
  participant B as bidder
  participant G as guest
  R->>B: RFP (spec.agent + spec.secretsRef)
  R->>A: chat.atirc.mint <label>-<rfp rkey>.<handle> lease{receipt, termination}
  R->>R: ATIRC_TOKEN into secrets bundle (vault, 0600)
  B->>G: boot; atirc-agent.service installed, inactive
  R->>B: ContractEvent outbound guest.agentStart
  B->>G: systemctl start --no-block atirc-agent.service
  G->>A: atirc agent run as nick user -> JOIN channel
  R->>A: chat.atirc.revoke (before destroy)
  R->>B: release -> vm.terminated (lease dies too)
```

## Preconditions

| Item | Need |
| --- | --- |
| Requester | alice (`aliceoa.bsky.social`) or hat (`hat.dplc.rip`), signed in with a session that holds `rpc:chat.atirc.mint` (and `chat.atirc.revoke` for via=account) |
| Scope change | any added scope = re-sign-in; wait ~5 min after qr / client metadata deploy (cache) before signing in |
| Vouches | as 0003 (bidder-tangled-vouch + requester-tangled-vouch) |
| Bidders | serve atirc in `socialweb-guest-downloads` (pinned `https://irc.fedfork.com/dl/v0.1.0-<sha>`) |
| atirc | lease server live (mint with lease, lease latch); agent config for the owner chain (harness) synced server-side |
| Watchers | host watcher on the winner; atirc-dev for `revocations.ndjson` |

## Steps

CLI:

```bash
SWC=socialweb-computer; R=~/.tmp/acc35; mkdir -p "$R"; chmod 700 "$R"
NAME=cli-agent-$(date +%s)
# --agent last (or --agent=<envFile>): a bare --agent before another flag swallowed it (pre rc126)
$SWC run request --state-dir "$R" --name "$NAME" --hold --agent -- --atproto-oauth-qr \
  > "$R/$NAME-request.log" 2>&1 &
# sign in from the pendingAction url (as 0003), wait for Held + requester_atirc_minted
$SWC run request agent "$NAME" --state-dir "$R"          # publishes guest.agentStart, waits Published
# wait for nick aliceoa-<rkey> JOIN on the channel; prompt it once: "aliceoa-<rkey>: uname -m"
$SWC run request release "$NAME" --state-dir "$R"
```

Page: Compute panel, Agent checkbox on (default), Request VM, wait Held, row **Agent**,
wait the event `Published`, see the nick join, **Release**, keep the tab open 30 s.

Drivers must match the atirc **join event** of `aliceoa-*`, never page text (a text match on "agent" released 3 s after the click once).

## Expected observations

| Event | Where | Typical |
| --- | --- | --- |
| `requester_atirc_minted contract=... subhandle=aliceoa-<rkey>.aliceoa.bsky.social path=/etc/atirc/atirc.env lease=Leased exp=...` | requester log | right after RFP / accept |
| `Held` | contract | 29-45 s after request (page 40-55 s) |
| `requester_contract_event_published` / event `Published` (guest.agentStart) | requester | < 1 s after click |
| `guest_agent_start unit=atirc-agent.service` | guest agent journal | ~1 s after event |
| `JOIN aliceoa-<rkey>` | atirc channel | 10-25 s after agentStart (harness may install first) |
| prompt answered | channel | ~5 s |
| `requester_atirc_revoke_before_destroy atircRevokes=Done` | requester | at release |
| `requester_atirc_revoked subhandle=... via=account revokedAt=...` (no `already=true`) | requester | 1-2 s before `vm.terminated` |
| `lease_revoke attempt=0 left=0` | requester | after revoke |
| `vm.terminated` pinned | bidder repo | ~1 s after revoke |
| `revocations.ndjson` entry for the subhandle | atirc server | matches revokedAt |

## Pass criteria

- Mint logged with `lease=Leased`; token never in a kcp object, spec, status, record, ContractEvent or log line
- After `Held`, before agentStart: `atirc-agent.service` inactive
- After agentStart: unit active, nick joined, one reply to a prompt
- `/etc/atirc/atirc.env` root, mode 0600
- Release: real revoke (via=account or parent token, HTTP 200) **before** `vm.terminated`; then `Released`
- Lease death: a token never revoked answers 401 `lease is not alive` after `vm.terminated` (probe once per atirc lease change)
- Without `--agent`: no mint, no unit, no atirc binary on the guest
- Winner host empty after release

## Cleanup

- Release every test VM; never touch another driver's VM (owner VMs are held, do not release)
- Missing revoke: ask atirc-dev for a server-side revoke by subhandle (suffix covers children); the lease kills it anyway once `vm.terminated` exists
- Orphan VM: [0007](0007-bidder-termination.md)

## Known pitfalls

| Symptom | Cause / fix |
| --- | --- |
| `requester_atirc_revoked ... already=true`, no ndjson entry | revoke used the dead child token after termination (401 mapped to already); fixed rc132: revoke before destroy, parent / account credentials |
| CLI release, token alive | requester exited before the termination POST (pre rc132); now revoke first |
| `spec.agent` contract fails before RFP | `an agent needs its token in spec.secretsRef`: `--agent` adds a one-entry bundle; page must send one |
| oauthQr bidder SessionExpired after rollout | scope added to Bidder/RequesterOAuthScope: re-sign every OAuth identity (announce before tagging) |
| harness never joins | no agent config for the owner chain (`atirc config agent`), or claude still installing (give it ~1 min) |
| release after page reload slow (20-38 s) | requester start after boot + release not first; fixed rc134 (release-first on resume, ~17 s click -> Released) |
