# Acceptance 0003: CLI requester, ssh over WebRTC, per bidder host (ACC34)

Proves the CLI requester (`socialweb-computer run request`) rents a VM on each prod
bidder and opens ssh to the guest over a WebRTC datachannel, both from a fresh
invocation kcp and after that kcp restarts from its state dir.

- Plan 0034 ACC34-CLI-SSH; the long form with the scope note is
  [0001 section 9](0001-auth-modes.md#9-acc34-ssh-over-webrtc-from-the-cli-on-each-bidder)
- One run per host: only `$HOST` bids: `-- --allow-bidder-dids <$HOST bidder DID>` (or the other two bidders suspended)
- Last pass: bidders rollout 6 (2d367fe8; hat-7 iroh + webrtc only, market-https off), CLI 41319a3a on hat-8, 2026-11-10
  16:39Z hat-9 and 16:44Z hat-7, alice pdsAccount: Held in 63 / 66 s, ssh over WebRTC exit 0 fresh (5.5 / 2.8 s) and from a
  restarted invocation (4.8 / 2.3 s), plaintext 0 / 0, Released; every record of alice's the run logged (rfp, accept, 4
  market.event incl. the restarted invocation's signal envelope, 2 guest.publicKey, 2 guest.signalWebRTC) answers getRecord
  400 after release (22479e17); the bidder's own event records stay in the bidder's repo. Before: kcp 438ca0fa bidders (p2p
  rollout 5), CLI 69aa8b0f, 15:13-15:17Z (also rollout 2, 13:49-13:56Z)

```mermaid
sequenceDiagram
  participant C as CLI requester (alice, oauthQr)
  participant Q as qr.fedfork.com
  participant B as bidder $HOST
  participant G as guest VM
  C->>C: pendingAction scanQr url (stderr)
  Q-->>C: session transfer (signed in as alice)
  C->>B: RFP -> bid -> accept, over the bidder's offering endpoints (iroh; https only if advertised)
  B->>G: boot, hostKeys, publicKey, onNetwork
  C->>G: WebRTC offer/answer (signalWebRTC events), STUN only
  C->>G: ssh (StrictHostKeyChecking=yes) uname -a; id
  C->>B: release -> vm.terminated
```

## Preconditions

| Item | Need |
| --- | --- |
| Hosts | ssh to `hat-7`, `hat-9`; local bidder on this host; kubeconfigs as in 0001 Variables |
| Bidders | all three `bidder-tangled-vouch`, Ready, same rc as the CLI; each advertises its ingress in `market.offering/self` `endpoints` (iroh by default; webrtc once its `computer.socialweb.ingress.listen/market` record is written, at once for a localRepo bidder, for an OAuth one after a sign-in grants the optional ingress listen/answer scopes; https only with `market-https=true`, the hat-7 transition): the requester calls the first it reaches, service auth aud `<bidder DID>#pdr_temp_market` (no `endpointUrl`, no fedproxy hostname) |
| Requester | alice signed in by QR (`-- --atproto-oauth-qr`), or with an app password (`-- --atproto-identity-mode pdsAccount --atproto-handle aliceoa.bsky.social`, the password in `ATPROTO_PASSWORD` of that one command); default localRepo makes an unassociated DID and gets no bids |
| Vouches | bob + john vouch alice; alice vouches bob + john |
| Signer | a second shell (or the drives-oauth agent) opens the newest `pendingAction` url and signs alice in; creds file never printed |
| hat-7 | oauthQr bidder: after a role-scope change re-sign bob first (0001 section 9) |
| ICE | guest reaches `stun:stun.cloudflare.com:3478`, nothing else |

## Steps

Announce first: `linux-kcp -> all: ACC34 on <host> now; hold VM tests.`

```bash
SWC=socialweb-computer
R=~/.tmp/acc34; mkdir -p "$R"; chmod 700 "$R"
HOST=<local|hat-9|hat-7>
# 1. suspend the other two MarketBidders (spec.suspend=true), wait both Suspended
<kubectl for other host> patch marketbidders.socialweb.computer app --type merge -p '{"spec":{"suspend":true}}'
# 2. fresh invocation: request + hold + first ssh
NAME=acc34-$HOST-$(date +%s)
$SWC run request --state-dir "$R" --name "$NAME" --hold \
  --tunnel ssh:22:webrtc --transport webrtc --exec "uname -a; id" \
  -- --atproto-oauth-qr > "$R/$NAME-request.log" 2>&1 &
# 3. sign alice in with the newest url from the log (second shell / drives-oauth)
command grep -oE 'pendingAction (scanQr|signInAgain) url [^ ]*' "$R/$NAME-request.log" | tail -1
# 4. wait for uid= in the request log and the --hold process to exit
# 5. restarted invocation kcp: second ssh from the same state dir
$SWC run request ssh "$NAME" --state-dir "$R" --transport webrtc --exec "uname -a; id" > "$R/$NAME-ssh.log" 2>&1
# 6. winner check (host lists a Running ComputeVM for alice), plaintext check
command grep -c -E 'a=candidate|AGE-SECRET-KEY' "$R/$NAME-request.log" "$R/$NAME-ssh.log"
# 7. release, then resume the two paused bidders
$SWC run request release "$NAME" --state-dir "$R"
```

The scripted form (pause, sign-in watch, both ssh runs, winner, release, resume) lives
on the lead host as `acc34.sh <host>`; it is a wrapper of the commands above.

## Expected observations

Request log phases (`"phase":` lines), rc142 runs:

| Step | local | hat-9 | hat-7 |
| --- | --- | --- | --- |
| start -> `AwaitingScan` | 20 s | 21 s | 20 s |
| scan -> identity `Ready` | 17 s | 19 s | 18 s |
| `Requesting` -> `Held` | 31 s | 40 s | 29 s |
| ssh returned, fresh kcp (`seconds`) | 2.5 s | 5.2 s | 8.9 s |
| ssh returned, restarted kcp | 6.1 s | 4.9 s | 3.6 s |

- `signed in as aliceoa.bsky.social`, then `Held`
- `{"msg":"ssh returned","transport":"webrtc","exit":0,"seconds":N}` in both logs
- output contains `Linux` and `uid=`
- whole host run (pause -> resume) ~3 min; all three ~10 min
- 2026-11-10 (p2p, offering endpoints, alice pdsAccount, `--ready-timeout 10m`): hat-9 start -> Held + first ssh 63 s, ssh 3.8 s fresh / 10.5 s restarted; hat-7 62 s, 5.2 s / 3.5 s; plaintext 0/0, both Released
- 2026-11-10 rollout 5: hat-9 64 s, ssh 3.4 s / 2.1 s; hat-7 60 s, 3.1 s / 2.1 s; plaintext 0/0; each run left one market.event (the restarted invocation's signal) in the repo: delete it
- earlier baselines: 2026-10-24 local 2.16 s fresh / ~7 s restarted; rc118 CLI run RFP -> Held 29 s, ssh 4.4 s

## Pass criteria

- Each host: `Held`, `uid=` from both invocations, `exit 0`
- ssh ran with `StrictHostKeyChecking=yes` against guest-reported host keys
- Winner = `$HOST` (Running ComputeVM for alice DID there while held)
- Plaintext check counts 0 / 0 (no ICE candidates, no age keys in logs)
- `run request release` ends `Released`; `$HOST` has no ComputeVM / firecracker left
- All 3 hosts pass, paused bidders resumed (`Ready`)

## Cleanup

- Resume both paused bidders (`spec.suspend=false`), check all three `Ready`
- Leftover VM: [0007](0007-bidder-termination.md)
- `chmod 700` state dir stays; delete old `$R/acc34-*` state dirs when done

## Known pitfalls

| Symptom | Cause / fix |
| --- | --- |
| second ssh `exit 255` after 120 s (seen once on local, 18:15Z) | first invocation still held the shared state-dir kcp; wait for the `--hold` process to exit, rerun |
| `invocation kcp did not come up` on release | same: release before the hold process exited |
| timeout waiting for guest publicKey on hat-7 | bob's oauthQr session predates a scope change; re-sign bob (0001 section 9) |
| no bids | alice not signed in by QR (localRepo DID waits in `AwaitingApproval`), or a vouch missing |
| every canary lands on the old rc | other bidders not paused; suspend them as above |
| `this host reaches none of <did>'s endpoints` / no submitRfp | the bidder's offering has no endpoint the CLI dials (iroh needs dumbpipe on the CLI host; https only when advertised) |
| guest publicKey / answer timeout | read the winning bidder journal for `the requester refused the market event` / `submitting the market event to the requester failed` |
