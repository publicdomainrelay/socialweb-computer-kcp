# Acceptance 0015: VM memory headroom

Proves a guest that uses nearly all of its RAM and writes to disk with fsync keeps its VM: the host does not OOM-kill
the VMM. A VM's cgroup leaves the guest its whole RAM again beside the VMM and any host page cache (`MemoryMax` = `MemoryHigh`
= 2 x guest, `MemorySwapMax` = guest, owner decision 2026-11-14: the host can always page to swap, so a VM is not
throttled or killed early), and its QEMU drives are `cache=none,aio=native` so guest I/O does not charge the host page
cache to the VM (firecracker has no such option: its rootfs page cache is charged, which the 2x headroom absorbs).

- Code: `internal/contract/host/host.go` (`VMLimits`, `VMCommitMiB`), the QEMU drive options (main 34a4aeaa);
  `internal/market/bidder/memory.go` (the host's `compute.events.vm.memoryPressure` / `vm.oomKilled` reports)
- Test: `tests/compositions/0015` (module `tests/modules/acc-0015`), entry `run`; prod and staging (it touches only its
  own VM)
- Last pass: see Evidence

## Preconditions

- An account that may rent (prod: bob; staging: a generated account)
- The VM's bidder runs it under a per-VM cgroup (a bidder account with its systemd user manager). A dev market network
  run as a login user without the kvm group in its user manager places VMs with the proc backend and no per-VM cgroup:
  there the run proves only the guest side

## Steps

```sh
cd tests/modules && cargo wasm && cd ..
cd compositions/0015 && deno run -A --no-lock compose.ts > composition.json && cd ../..
SWC_TARGET=$PWD/targets/prod.json cargo run -p swc-test-host -- compositions/0015 --account bob
```

Inputs (`--input '{...}'`): `cpus` (default "2"), `mem` ("2G"), `pct` (95), `seconds` (300).

1. `run request --hold --cpus 2 --mem 2G --expires-in 1h`; Held, the winner is the receipt's author
2. Over `run request ssh`: `/var/tmp/memfill.sh` detached. It records the boot id, holds `MemAvailable - (100 - pct)%
   of MemTotal` in a python3 process (os.urandom chunks, incompressible), and writes 64 MiB with `dd conv=fsync` in a
   loop for `seconds`, one status line per write (`/var/tmp/memfill.status`)
3. Every 30 s: the status (boot id now, lowest MemAvailable, fsyncs, the hog alive) and the receipt's `vm.terminated`
   (must be RecordNotFound)
4. Done: the winner's ComputeVM of the receipt (when the target names its kcp); the optional slot: the winner's
   `vm.memoryPressure` and `vm.oomKilled` records of this receipt (rkey `...-payload-<sha256("<nsid>:<tag>:<receipt>")
   [:16] hex>`, tag `high-n`, `max-n`, `oom-n`)
5. `run request release`; every record of the contract RecordNotFound

## Pass criteria

- The contract stays Held and `run request` stays up until the release; no `vm.terminated` during the fill
- The guest's boot id never changes; ssh answers (at most 3 misses in a row)
- The fill reached `pct - 3`% used (`peak_used_pct`), and at least one fsync'd write completed
- The ComputeVM is `Running` with no `lostReason` (never `Lost`/`OOMKilled`)
- Optional slot: no `vm.oomKilled` record of this receipt. `vm.memoryPressure` records are a reading (`high` is
  expected only past 2 x guest now; the VM is throttled, not killed); a bidder that writes none (older than
  ccda777a) passes
- After the release: every record of the contract RecordNotFound

Readings: `winner`, `peak_used_pct`, `fsyncs`, `guest_oom_lines` (the guest's own OOM killer, a reading: the guest may
kill the hog when its page cache needs room), `ssh_missed`, `computevm`, `memoryPressure`, `oomKilled`.

## Entry `pressure`: the memory events fire once per crossing and re-arm

Proves the bidder's edge-triggered `compute.events.vm.memoryPressure` (internal/market/bidder/memory.go: it polls the VM
cgroup's `memory.events` every 15 s; one record per crossing, at most one per kind per `MemoryTypeGap` 10 min, re-armed only
after `MemoryRearmQuiet` 5 min without a rise and usage under 90% of the limit) fires exactly once per crossing, not once per
poll, and not again inside the gap. Staging only (SKIP on prod): the test host lowers THIS run's own VM unit's `MemoryHigh`
(`vm.memory` host call: `systemctl --user set-property --runtime`, only for a ComputeVM of a receipt this run made, on a network
target) and restores it; a bidder without per-VM cgroups (proc backend) SKIPs with that reason.

1. `run request --hold --cpus 2 --mem 2G`; in the guest a python3 process holds 55% of RAM (os.urandom chunks) and, while
   `/var/tmp/memhold.go` exists, reads every page of it again and again
2. Crossing n: `MemoryHigh` to half the VM's `memory.current` (256 MiB at least), the guest touches its memory for 60 s
   (memory.events `high` must rise at least 3 times), then `MemoryHigh` restored and the touching stopped
3. Crossing 1: exactly one `vm.memoryPressure` record of this receipt (kind `high`, `observed` > 0), still one 45 s after it
   appeared (the polls that follow add none); crossing 2 at record 1 + 6.5 min (past the 5 min re-arm, inside the 10 min gap):
   still exactly one 75 s later; crossing 3 at record 1 + 10.75 min (past the gap): exactly two
4. Never a kind `max` record or a `vm.oomKilled` one; the ComputeVM `Running`, the guest's boot id unchanged, the contract
   never left Held, no `vm.terminated`; release; every record gone

Readings: `limits` (unit, high, max, swapMax, memory.events before), `crossings` (lowered, highEventsRose, restored limit),
`record1`, `records`, `oomKilledRecords`. Run time about 15 min (the gap makes it so: a second record cannot appear sooner;
a test-only knob for the gap would make it shorter but would not test the product constant).

```sh
SWC_TARGET=<dev or staging target.json> cargo run -p swc-test-host -- compositions/0015 --account alice --entry pressure
```

## Known pitfalls

| Symptom | Cause / fix |
| --- | --- |
| "the fill reached N% used" | the guest's OOM killer took the hog early (`guest_oom_lines`); lower `pct` |
| ssh misses while full | the guest is slow under the fill; four in a row fail the run |
| `memoryPressure.others` large | other contracts' reports on the bidder; only this receipt's are asserted (listRecords reads 100) |

## Evidence

- Dev market network `capt` on hat-8, binary 22c89875, account alice: PASS 2026-11-13 20:58:17-21:04:49Z. Winner
  hm7ge..., Held 29.5 s, peak 95% used (lowest MemAvailable 109 MB of 2.0 GB), 1118 fsync'd 64 MiB writes in 349 s,
  boot id unchanged, ComputeVM `fc-65f2xp54oloh-a6ff487f` Running, no lostReason, no memory reports (the network's
  bidders run VMs with the proc backend: no per-VM cgroup, so this proves the guest side only), 3 records gone
- `pressure`, dev network `captt` (test-0001, systemd-user VMs, `dev up` inside `systemd-run --user --scope` so the bidder core runs
  under user@UID.service, bidder binary = main e13494b1 + fix-vm-cgroup-walk b8f323b5): PASS 2026-11-14 03:28-03:42Z, 799.9 s.
  Crossing 1 (03:28:50, `MemoryHigh` 1.04 GB, memory.events high rose 24442 times): one record at 03:29:15Z (kind high, observed
  1048645632, threshold 1044066304); crossing 2 (6.5 min later, 28277 rises): still one record; crossing 3 (record 1 + 10.75 min,
  27460 rises): second record at 03:40:00Z (10 min 45 s after the first), `payload-2dedf4f6...`; no max record, no oomKilled,
  ComputeVM Running, boot id unchanged, no vm.terminated, 3 records gone. Without b8f323b5 the same run fails at crossing 1
  ("0 memoryPressure records 4 min after the window") because `vmCgroupDir` never found the VM cgroup
- Prod (bob): waits until the release gate is done
