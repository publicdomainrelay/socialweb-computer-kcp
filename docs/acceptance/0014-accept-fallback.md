# Acceptance 0014: accept fallback and committed capacity

Proves a bidder never takes more VMs than it has room for, and that a requester whose accept is refused for capacity
gets its VM elsewhere on the same RFP: the bidder answers the accept's `submitAccept` with HTTP 409
`CapacityExceeded` (no receipt), the requester deletes that accept, denies the bidder for the contract and takes the
next bid; with no bid left it deletes the RFP and issues a new one without the refusers.

- Code: `internal/market/bidder/vmsize.go` (bid-time check, accept-time reservation keyed by RFP URI, 10 min hold,
  dropped once the ComputeVM exists), `internal/market/requester/runner.go` (`replaceDeclined`, `reRFPAfterRefusals`,
  up to 3 replacements)
- Test: `tests/compositions/0014` (module `tests/modules/acc-0014`), entries `run`, `concurrent`, `exhausted`, `giveup`, `wait` and `backtoback`, through the kcp
  CLI requester (`run request`); staging only. The same fallback in the atlibs requester module (the cicd
  composition's) is `tests/compositions/knotless` entry `fallback` (concurrent workflows, one accept refused 409, the
  checks still succeed, the refused accept gone)
- Last pass: see Evidence

```mermaid
sequenceDiagram
  participant R1 as rental 1
  participant R2 as rental 2
  participant X as bidder X (room for 1)
  participant Y as bidder Y
  R1->>X: RFP 1; X bids (room)
  R2->>X: RFP 2; X bids (room)
  R2->>Y: RFP 2; Y bids
  R1->>X: accept 1 -> reserved, receipt
  R2->>X: accept 2 -> 409 CapacityExceeded, no receipt
  R2->>R2: requester_submit_accept_declined, requester_accept_declined_replace, delete accept 2, deny X
  R2->>Y: accept (Y's bid on RFP 2) -> receipt, Held
```

## Why the rentals are concurrent

A bidder checks capacity twice: when it bids (its live guests plus reservations of other RFPs) and when it gets the
accept (the same, then it reserves). A bidder that is already full when an RFP arrives does not bid at all, so filler
VMs rented first make the requester skip that bidder, never a 409. A 409 needs room taken between a bidder's bid and
the accept: rentals whose RFPs land together, each first picking the bid that arrived first. The test sizes every VM
from the bidders' offerings (`maxCpus`) less the guests already on their kcp, so each bidder has room for exactly
`fit` of them.

| entry | bidders | rentals | VM vCPUs | refusal |
| --- | --- | --- | --- | --- |
| `run` | first 3 of the target | 3 | `min(free)/2 + 1` (one per bidder) | required; up to 3 rounds (a round where every first pick had room has none) |
| `concurrent` | first 2 of the target | 4 | `min(free)/2` (two per bidder) | a reading |
| `backtoback` | the target's first | 6 (input `count`), one after another | 1 (input `cpus`), mem 1G | none: every VM Held |
| `giveup` | first 2 of the target | 1 | both bidders full (one pinned 1 h filler each) | one rental with `--expires-in 150s` has no room: it waits in rounds (at least one `requester_waiting_for_capacity`) and fails at its deadline with "gave up waiting for capacity after N rounds" |
| `wait` | first 2 of the target | 1 | both bidders full | one rental with `--expires-in 15m` waits; after two rounds a filler is released: the rental is Held on the freed bidder within 300 s of the release (about one backoff and a boot); readings `waitRounds`, `heldAfterFreeS` |
| `exhausted` | first 2 of the target | 3 | `min(free)/2 + 1` (one per bidder) | required: one rental is refused by both, its bids run out, its new RFP (the refusers excluded, as specified) finds no bid: the rental WAITS for capacity (`requester_waiting_for_capacity`, rounds with a backoff of 30 s, 1 min, 2 min, a fresh RFP asking every bidder each time) and is released by the test |

Every rental is pinned to the entry's bidders (`--allow-bidder-dids`); mem 1G (input `mem`).

Big bidders (more than 8 free vCPUs, e.g. a dev network's bidders, which offer the whole host) first get one filler
VM each, pinned to that bidder (`--allow-bidder-dids <it>`), sized to leave room for exactly `fit` guests of 2 vCPUs
(input `cpus`); the rentals are then 2-vCPU guests. Without fillers a 16-CPU host gets four 8-vCPU guests booting at
once, and on a loaded host they miss the requester's 2 min on-network window. The fillers are released with the
rentals and their records checked too.

## backtoback

VMs booted one after another on one bidder reuse its VM slots: each unit was just reset when the next is placed. Before
f15dda15 the placer took a unit that was merely loaded (inactive) for placed, awaitBoot read "went inactive (result
success, status 0) before its VMM answered" and killed the VM some 100 ms after the hand-off; the requester saw "the
bidder reported vm.terminated before the guest came up". The entry rents, waits for Held, releases, `count` times,
each pinned to the bidder; a rental that ends before Held is recorded and the run goes on. It needs the bidder's VMs
placed by a service manager (`--vm-host-backend systemd-user`, as prod): with the proc backend the race does not show.

Inputs: `count` (6), `cpus` (1), `mem` ("1G"), `overlap` (false: with true the next VM is requested the moment the
previous one is Held, while its release runs, so the next unit is placed as the last is reset). The race is rare (about one
boot in a hundred on prod), so a pass proves little: run with `--input '{"count":12,"overlap":true}'`.

Pass: every VM Held (none Released or Failed before it, `onNetwork` in the readings for the ones that ended), the
bidder's log gains no "before its VMM answered" line over the run (`bidder.logCount`: the network bidder's core.log, an
installed bidder's journal), every record gone.

## Preconditions

- A market network the run owns: staging (`tests/compositions/staging`, `bidders` >= 3 for `run`) or a dev market
  network (`dev up --profile market-network --bidders 3`), its target file from `tests/compositions/staging/target.sh`
- Nothing else renting on those bidders during the run (the sizing reads their live guests once, at the start)
- On the prod target both entries SKIP: they fill bidders to capacity, and prod bidders are shared

## Steps

```sh
cd tests/modules && cargo wasm && cd ..
cd compositions/0014 && deno run -A --no-lock compose.ts > composition.json && cd ../..
SWC_TARGET=<staging target.json> cargo run -p swc-test-host -- compositions/0014 --account alice --entry run
SWC_TARGET=<staging target.json> cargo run -p swc-test-host -- compositions/0014 --account alice --entry concurrent
SWC_TARGET=<staging target.json> cargo run -p swc-test-host -- compositions/0014 --account alice --entry exhausted
```

A dev market network on a host (hat-8: `--asset-cache` under /var/tmp, the default cache is not traversable by the
VM user):

```sh
B=<socialweb-computer>; N="--instance capt --state-dir /var/tmp/capt/net"
$B dev up $N --profile market-network --bidders 3 --network-timeout 15m --asset-cache /var/tmp/capt/assets
SWC=$B SWC_STATE=/var/tmp/capt/net SWC_INSTANCE=capt SWC_CREDS=/var/tmp/capt/creds \
  tests/compositions/staging/target.sh capt0001 > /var/tmp/capt/target.json
$B dev down $N
```

## Expected observations

- Each bidder's offering record `com.publicdomainrelay.temp.market.offering/self` carries `maxCpus` (the host's CPUs)
- The refused rental's request log, in order: `requester_submit_accept_declined` (`status` 409, `body.error`
  `CapacityExceeded`, `body.message` "... running and reserved guests hold N of its M"), `requester_accept_declined_replace`
  (`bidder`), then `contract` Held with a receipt from another bidder; with no bid left, `requester_bids_exhausted_new_rfp`
  (`declined`) and a new RFP
- The refusing bidder's log: `bidder_accept_refused_capacity` (the async provision path, from the accept record on the
  firehose, and the `submitAccept` path)

## Pass criteria

- Every rental Held (`exhausted`: all but one), each on one of the entry's bidders and never on a bidder that refused it
- `exhausted`: the one rental left without a VM was refused (409) by every bidder of the entry, logged
  `requester_bids_exhausted_new_rfp`, and then waits for capacity (at least one `requester_waiting_for_capacity` line) instead of failing; the test releases it
- No bidder over its vCPUs: this run's VMs on a bidder fit its free vCPUs; its kcp's live ComputeVMs' `spec.vm.cpus`
  sum to at most its `maxCpus`
- Every refusal is a 409 `CapacityExceeded`; every refused accept record RecordNotFound (x3)
- `run`: at least one refusal (readings: `rounds[].refusals`, `maxRefusalsOneRental`: 2 is a rental refused by its first
  two picks and Held on the third)
- After the release: every record of every rental RecordNotFound, no ComputeVM of the run's receipts on any bidder kcp
  within 3 min

## Cleanup

The run releases every rental (`run request release`) and checks; `--on-fail freeze` keeps them for `attach`, then
`cleanup` releases them.

## Known pitfalls

| Symptom | Cause / fix |
| --- | --- |
| `run` fails "no accept refused in 3 rounds" | every round's first picks were distinct (a race); run again. A requester picks the first free bid that arrives |
| "the bidders have room for N VMs" | guests of another run on the bidders; wait for them to go or use another network |
| a rental fails `requester_no_bidders` after `requester_bids_exhausted_new_rfp` in `run` or `concurrent` | every bidder refused it: a bidder had less room than its live guests show. Before 91103758 a reservation whose ComputeVM was released before the bidder's next capacity check counted for its whole 10 min hold (phantom "hold 16 of its 16" with one VM); run the entries back to back only on bidders with that fix |
| `the guest did not come on network within 2m0s` | a loaded host booting several large guests at once (hat-8, four 8-vCPU guests, 2m10s); rerun when the host is quieter |

## Evidence

Dev market network `capt` on hat-8 (3 bidders, `--vmm qemu`, 16 CPUs each in their offering), binary bcf42c0d
(capacity 8b04fd2c + the reservation fix 91103758), test host and modules of this branch, account alice (generated):

- `concurrent` PASS 2026-11-13 21:38:47-21:40:58Z: fillers 12 vCPUs on each bidder (Held 35 s), four 2-vCPU rentals
  Held in 56 s; one refused 409 by bidder zdt45... (`requester_submit_accept_declined`, `requester_accept_declined_replace`)
  and Held on tngqo...; 2 per bidder, liveCpus 16/16 on both; 19 records gone, no ComputeVM left
- `run` PASS 2026-11-13 21:41:08-21:44:43Z: fillers 14 vCPUs on each of 3 bidders, three 2-vCPU rentals Held in 73 s;
  rental 1 refused by lqkfi... then tngqo... (two 409s) and Held on zdt45... (the third); rental 2 refused once; 3
  refusals, liveCpus 16/16 on all three; 21 records gone
- `exhausted` PASS 2026-11-13 21:51:55-21:53:35Z: fillers 14 vCPUs on each of 2 bidders, three 2-vCPU rentals; two
  Held (one per bidder, liveCpus 16/16), rental 0 refused 409 by both, `requester_bids_exhausted_new_rfp`, its new RFP
  Failed at once with `requester_no_bidders` (both "denied by the contract"); 12 records gone (the old RFP too)
- Env failures on the way (not the code under test): four 8-vCPU guests at once on a loaded hat-8 missed the 2 min
  on-network window (hence the fillers); one `exhausted` try lost a 2-vCPU guest the same way (21:48Z)
- Found by the first runs (22c89875): a reservation whose ComputeVM was released before the bidder's next capacity
  check counted for its whole 10 min hold: bidder-2 refused 409 "hold 16 of its 16" with one live VM, a rental was
  refused by both bidders and failed (fixed in 91103758)

- port-wait-for-capacity (requester that waits for a slot), capt dev network (hat-8, 2 bidders, 2026-11-14), CLI and net core = branch 77029b12
  vs main 3c725c6f: `exhausted` PASS 2/2 (the unplaced rental waits, released by the test) and FAILS on main ("failed instead of waiting for
  capacity: requester_no_bidders"); `giveup` (both bidders filled by pinned fillers, rental `--expires-in 150s`) PASS 2/2 with "gave up
  waiting for capacity after 2 rounds" after one `requester_waiting_for_capacity` line, FAILS on main (Failed after one 30 s round, no
  "gave up"); `wait` (`--expires-in 15m`, a filler released after two waiting rounds) PASS 2/2: Held 77.1 s after the release (readings
  `heldAfterFreeS`, `waitRounds` 2, `waitSlot` 2-vCPU), FAILS on main (the rental fails after one 30 s round); `concurrent` and
  `backtoback` PASS on the fix, `run` needs 3 bidders (skipped on this 2-bidder net)
