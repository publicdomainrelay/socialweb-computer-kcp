# Acceptance 0008: host observer verification (per bidder host)

Read-only verification a host watcher agent runs on a bidder host for every rollout
and every VM: the bidder is healthy, each VM's lifetime is captured, the
`vm.terminated` record is pinned, and the host is empty afterwards. Every other
acceptance (0001-0006, 0009, 0010) uses these posts as its host-side evidence.

- Bidders (2026-11-10): hat-7 `johnandersen777.bsky.social`, bidder `did:plc:7q5lnitxv4meizdon5fmhzrq` (localRepo, handle dir
  `855d5a9904`); hat-9 `bobvmbuilder.bsky.social`, bidder `did:plc:4vcig7s43z3nyqrnepmcfzqt` (localRepo, handle dir `53eff7074d`).
  hat-7's old bob handle `53eff7074d` is archived in `/var/lib/socialweb-computer/handle-53eff7074d-*.tgz`
- Watchers: the host watcher agent per host, lead (local), gateway agent (gateway kcps)
- Adapted from the hat-7 watcher's observer protocol
- Last pass: 2026-11-12 21:35-22:23Z, rollout 19 (kcp e190a3b1, release 66f3bd4d), read by the lead (not posted on atirc):
  hat-7, hat-9, hat-8 `swc-bidder` CONFIRMED (version, `bidder.json` ready=true, MarketIdentity Ready, no `pendingAction`,
  0 unsupported / 0 ERROR since the 21:10Z restarts) + host empty. hat-8 VM timelines (all three rounds won by hat-8):
  0006 compat CLI (rollout 18 CLI) bid 21:47:26.46 -> accept 21:47:33.58 -> prepared 1.07 s -> Running 21:47:36.41 ->
  hostKeys 21:48:22.64 (46 s after Running) -> onNetwork 21:48:25.18; term 21:50:15.75 -> released 21:50:17.33 ->
  finalizer 21:50:17.53, pinned `termination-payload-be1703ace14c23b5ccf7659606906ba6` (cid
  `bafyreiep26viujvvyiwv7xx7iww3xls2pn3nnret3f2kc4v55bfv5t7xae`, `deleted_on_request`, `vm.delete` ref); page round bid
  21:55:27.71 -> accept 21:55:31.46 -> prepared 0.88 s -> Running 21:55:33.60 -> hostKeys 21:55:46.29 -> onNetwork x2
  21:55:46.98; term 21:56:00.26 -> released 21:56:01.91 -> finalizer 21:56:01.99, pinned
  `termination-payload-7c26af7fa0551c8a7c0410a50178d673` (cid `bafyreiclmsz66antgymtrkjfa35mpn6cku7rck7ygzmrnmrtdp2robks7a`,
  `deleted_on_request`, `vm.delete` ref); 0007 rounds below. Orphan check 0/0/0 on all three after each release.
  Anomalies: hat-8 one ERROR at 21:50:17.01Z in the compat release (`firecrackervm` update of HostProcess
  `vm-fc-lpfuqerea3de-be063eb5`: `the object has been modified`, retried in 500 ms, released 0.3 s later; 3 such on hat-8
  since 2026-11-11, 0 on hat-7 / hat-9); hat-8 WARN `refreshing the guest OIDC document failed ... keys document is empty`
  12 times since its restart
- Previous pass: 2026-11-10 15:14Z, rollout 5 438ca0fa: hat-7 and hat-9 CONFIRMED + host empty; hat-9 VM timeline + pin, orphan check 0/0/0

```mermaid
stateDiagram-v2
  [*] --> Rollout: bidder restart
  Rollout --> Idle: CONFIRMED + host empty
  Idle --> Bid: RFP seen, bid published
  Bid --> Idle: not accepted (post only if anomaly)
  Bid --> VM: accept
  VM --> Released: termination StateSync, finalizer dropped
  Released --> Pinned: vm.terminated getRecord
  Pinned --> Idle: orphan check clean
```

## Rules

- Watch only: never release, delete or restart anything without explicit owner (or lead, on owner authority) direction
- One CONFIRMED line per rollout, one timeline per VM, one host line after release
- Post only VMs that land here and anomalies (bid failures, bidder errors); lost bids are noise (owner/lead rule)
- Each atirc line < 300 chars; split timeline, pinned record, cid line, host line
- No tokens, cookies, URLs with secrets, file contents of creds

## Preconditions (after any bidder rollout)

| Check | Pass value |
| --- | --- |
| `socialweb-computer version` | expected `v0.0.0-rcN <commit>` |
| `bidder.json` under the handle dir | `ready: true` |
| MarketIdentity | `phase: Ready`, `SignedIn: True`, `pendingAction` absent |
| startup since restart (`--since` = local restart time) | 0 `unsupported type: source.TypedFunc`, 0 `"level":"ERROR"` |
| host (unless a VM is expected) | 0 computevms, 0 firecrackervms, 0 `firecracker` procs, `vms/` empty |

`vms/` is mode 0700: always count it with sudo; a non-sudo count reads 0 while a VM runs.

A localRepo MarketIdentity (both prod bidders now) has no `SignedIn` condition: `Ready=True` and no `pendingAction` pass.
Expected WARN at start: one `no ComputeVM PolicyTrigger action handler is registered`. Not anomalies: a Bid `Failed
InvalidPayload` (an RFP whose compute.vm does not parse is refused before any bid, fc56b5fb), WARN `the provision of an
accept failed` ending `Obsolete` when the requester deleted its accept, WARN `an inbound vm delete event is refused`.

Post: `hat-7 rcN CONFIRMED: v0.0.0-rcN <commit> live; bidder.json ready=true; MarketIdentity Ready, SignedIn=True, pendingAction absent; startup clean (0 unsupported-type, 0 ERROR since restart).` then `hat-7 host empty: 0 computevms / 0 firecrackervms / 0 firecracker procs, vms dir empty.`

## During a VM lifetime (per ComputeVM)

Sources: bidder journal (`journalctl -f -o cat _SYSTEMD_USER_UNIT=socialweb-computer-bidder@<handle>.service`)
and/or the kcp watch (`kubectl get computevms.socialweb.computer -w -o json`). Capture:

- bid published (RFP uri, bid uri), accept uri, provision StateSync name
- phases `Pending -> Preparing -> Booting -> Running` with timestamps, `prepared` seconds, pid, slot
- `a guest ComputeVM is applied`, backend creation, hostKeys report (payload rkey), onNetwork report(s) (payload rkey, tag)
- release: termination StateSync name/time, `Running -> Releasing`, `released`, `released: the backend is gone and the finalizer is dropped`, `a released bid is marked terminated` (bid id)

Typical (hat-7, ssh gateway VM, 2026-10-31): bid 22:50:56.18 -> accept 22:51:16.49 -> prepared 1.32 s -> Running 22:51:21.38 -> hostKeys 22:51:30.79 -> onNetwork 22:51:33.43; term 22:51:38.49 -> released 22:51:39.75 -> finalizer 22:51:39.81.
Tenant node (acceptance 0001, rc154): accept -> Running 3 s, Running -> onNetwork ~10 s.
hat-9, bob CLI VM, rollout 5 (2026-11-10): accept 15:14:10.38 -> prepared 1.27 s -> Running 15:14:13.58 -> hostKeys 15:14:23.51 -> onNetwork 15:14:26.19; term 15:14:33.33 -> released 15:14:34.34 -> finalizer 15:14:34.41; pinned `termination-payload-812eba60d84956819e88b4d0639f5fcf` (cid `bafyreicqol5x2sfqpv76cshnd4xhlgksbdzikglkncqm5uko3llhamso6q`, `deleted_on_request`, with a `vm.delete` request ref).

## Pinning the vm.terminated record

The record URI is what matters; the cid is what a counterparty must cite.

1. Take the rkey from the journal (`termination-payload-<hash>`) and `com.atproto.repo.getRecord` it on the bidder PDS
   (repo = bidder DID, collection `com.publicdomainrelay.temp.compute.events.vm.terminated`). One getRecord, no scan
2. If the journal line is gone, compute the rkey. It is `market.EventRKeys(market.TerminationKey(<receipt uri>))`:
   `termination-payload-` and the first 32 hex chars of sha256 of `terminated:<receipt uri>`. Take the receipt uri
   from the bidder journal line of the termination StateSync. Then do the one getRecord of step 1. Never list records
3. Pin: uri, cid, `createdAt`, `reason`, `request.uri` or its absence

```bash
RK=termination-payload-$(printf 'terminated:%s' "$RECEIPT_URI" | sha256sum | cut -c1-32)
```

Caveat: releases over the ssh-gateway session path produced records with **no `request` field**
(keys `$type`, `createdAt`, `observedAt`, `reason`, `signatures`); page-path releases carry a `vm.delete`
strong ref. Report an absent request ref explicitly.

## Orphan check after every release

```bash
kb get computevms.socialweb.computer,firecrackervms.socialweb.computer   # No resources found
pgrep -c -x firecracker                                                  # 0
sudo ls -A /var/lib/socialweb-computer/swc-bidder/handles/<hash>/vms | wc -l   # 0
```

## Pass criteria

- Rollout: CONFIRMED line with every precondition true, then host empty
- Per VM: timeline complete (bid to finalizer), vm.terminated pinned with uri + cid + reason
- After release: orphan check 0 / 0 / 0
- Anything else (bidder ERROR, VM left, record missing) posted as an anomaly to the lead

## Known pitfalls

| Symptom | Cause / fix |
| --- | --- |
| watcher silent for hours | its atirc sub / Monitor expired and was never re-armed; restart loop `while true; do atirc sub; sleep 2; done` |
| "all bids denied by policy" blamed on the bidder | check the bidder policy runs (Succeeded + bid published): the deny may be requester-side |
| stale pendingAction after rollout | session predates a scope change; lead / drives-oauth re-signs |
| bidder clock off (hat-9 -9.94 s) | service-auth 401 at the counterparty; enable NTP (chronyd) |
| CONFIRMED with the old version | `install bidder` not re-run after `upgrade` |
