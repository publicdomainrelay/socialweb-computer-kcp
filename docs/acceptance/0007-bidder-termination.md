# Acceptance 0007: bidder-side VM termination (ops)

Proves a bidder operator can end a VM whose requester cannot release it (tab or app
gone, cascade gap, stray debug VM) and that the delete leaves nothing behind: the
contract is released, `vm.terminated` is pinned in the bidder repo, and every atirc
token leased to the contract dies.

- Procedure: [bidder.md, Terminate a VM from the bidder](../bidder.md#terminate-a-vm-from-the-bidder)
- Used on prod: R2 orphan (hat-7, 2026-10-30), iOS cross-restart retry VM (hat-7, 2026-10-31 02:07Z), stray debug VM (hat-7, 2026-10-31 18:11Z)
- Last pass: 2026-11-10 15:14Z (hat-7, rollout 5 438ca0fa): bob's held contract, delete -> gone in ~1 s, `vm.terminated` pinned (`termination-payload-19e3b1b5db974dc15cff4d61429bbeb2`, `deleted_on_request`, no `request` ref), requester `release` -> Released "the bidder ended the contract: deleted_on_request"
- Last expiry pass: 2026-11-11 01:17Z (hat-9, rollout 10 253031e2, binary 233343a9): bob `run request --hold --expires-in 3m`,
  `bidder_lifetime_capped` (compute.vm expiresAt 01:07:32Z), `bidder_association_expired_release` 01:17:32Z, released in 1 s,
  `vm.terminated` pinned (`termination-payload-8719af6146077f7514b9d08be02b4c4e`, cid
  `bafyreiht6bbyzcvfy4jfakpzqrkdp6tn7os6mi6cetmlq44eefmqwidjoi`, `expired`, no `request` ref), requester `release` ->
  Released "the bidder ended the contract: expired"; the contract's records gone from bob's repo; hosts 0 firecracker

```mermaid
sequenceDiagram
  participant Op as operator (bidder host)
  participant K as bidder kcp
  participant P as bidder repo
  participant Rq as requester (maybe gone)
  participant A as atirc
  Op->>K: delete ComputeVM fc-<did12>-<hash> --wait=false
  K->>P: termination StateSync + vm.terminated (reason deleted_on_request)
  K->>K: stop VM process, unit, work dir; release contract
  K-->>Rq: vm.terminated event (404 = requester gone, fine)
  A->>P: lease check sees vm.terminated -> token dead (latched)
  K->>K: drop finalizer, ComputeVM gone
```

## Preconditions

- Authority: the owner asked for it, or the VM is matched to a contract you are ending (stray test / orphan). Never an owner-held VM unless the owner says so
- Shell on the bidder host with sudo; announce on atirc: `<nick>: deleting <vm> on <host> out of band (contract <name>)`
- Know the requester DID and the contract (`kb get computevms ... -o wide`, watcher timeline)

## Steps

```bash
K=$(sudo -n -u swc-bidder sh -c 'ls -t /var/lib/socialweb-computer/swc-bidder/handles/*/kcp/front.json | head -1 | sed s,front.json,admin.kubeconfig,')
kb() { sudo -n -u swc-bidder env HOME=/var/lib/socialweb-computer/swc-bidder KUBECONFIG="$K" kubectl "$@"; }
kb get computevms.socialweb.computer -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,CREATED:.metadata.creationTimestamp
# name = fc-<first 12 chars of requester DID after did:plc:>-<hash>; match DID prefix AND creation time
kb delete computevms.socialweb.computer <name> --wait=false
kb get computevms.socialweb.computer -w      # until it is gone
```

Then pin `vm.terminated` (getRecord on the bidder repo, see [0008](0008-host-observer.md)) and ask atirc-dev
to confirm the leased token is dead.

## Expected observations

Bidder journal, in order (hat-7 example, 2026-10-31 22:51Z):

| Line | Time |
| --- | --- |
| termination StateSync `termination-<hash>` | 22:51:38.49 |
| `Running -> Releasing` | |
| `released` | 22:51:39.75 (+1.3 s) |
| `released: the backend is gone and the finalizer is dropped` | 22:51:39.81 |
| `a released bid is marked terminated` | |
| `vm.terminated` record pinned, reason `deleted_on_request` | |

- Requester side: `vm.terminated` delivery may 404 (requester gone): record in the bidder repo still stands; the
  termination StateSync then ends `Failed` with `the ComputeVM is released; the market event is no longer sent: HTTP 404`
  (expected; pin the record, not the StateSync phase)
- An operator delete always pins `vm.terminated`, also when an earlier inbound requester delete was refused or its
  StateSync stalled (648384c1); an unsigned inbound `vm.delete` is refused at submitEvent with 400 and creates no StateSync
- atirc: next use of any token leased on the contract -> 401 `lease is not alive`, latched (no revival if the record later disappears)
- Without an operator delete: a VM whose requester never returns is released by the bidder at its
  `spec.associationExpiresAt` + 10 min: the compute.vm `expiresAt` (CLI `--expires-in`), else accept + 24 h, or the
  association proof's `expiresAt` when earlier (`bidder_lifetime_capped` at provisioning, `bidder_association_expired_release`
  at the end), and its `vm.terminated` has reason `expired` and no `request` ref. Before this cap
  (2026-11-10), 8 VMs that the iOS app leaked from killed launches ran on hat-9 until an operator delete.

## Pass criteria

- ComputeVM gone within seconds; contract released
- `vm.terminated` exists in the bidder repo with reason `deleted_on_request`
- Without an operator delete (expiry): `vm.terminated` with reason `expired` and no `request` ref
- Host clean: `kb get computevms,firecrackervms` "No resources found", `pgrep -c -x firecracker` 0, `sudo ls -A <handle>/vms | wc -l` 0
- Leased tokens dead (atirc-dev check, or a probe with the token gets 401)
- Unleased (plain) tokens of the contract revoked manually (atirc-dev, by subhandle)

## Cleanup

- Post the timeline and the pinned record on atirc (split lines < 300 chars)
- Note the root cause of the orphan (why the requester did not release) as an open item; out-of-band delete is a mitigation, not the fix

## Known pitfalls

| Symptom | Cause / fix |
| --- | --- |
| deleted the wrong VM | matched by DID prefix only; always match DID prefix + creation time + contract |
| `vms/` count 0 while a VM runs | dir is 0700; read with sudo |
| token still answers | token was plain (pre-lease or test mint); revoke it by subhandle |
| ComputeVM `Lost` after `uninstall --force` | expected: forced uninstall does not run the finalizer path; do not use it to end one VM |
| requester resumes later and re-releases | harmless: release is idempotent; the contract is already Released |
