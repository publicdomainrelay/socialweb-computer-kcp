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
- Prod (bob): waits until the release gate is done
