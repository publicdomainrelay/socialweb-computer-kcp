# Acceptance 0017: a release asked by another process while the owner holds an interactive ssh session

`run request --hold` with a terminal on stdin opens an interactive ssh session into its guest and holds the invocation: it
is the invocation's OWNER (it holds `owner.lock` and runs the invocation's kcp). `run request release <name>` from another
process asks the owner's kcp to release and waits. Two failures made that release wait out its whole timeout (27 minutes in
the 0004 and 0005 prod gate runs): the owner's re-read of the contract failed at the start of the release, so it stopped its
kcp 170 ms after the release began, and the release client waited on a kcp nobody served. Fixed on branch
`fix-owner-release` (`cmd/socialweb-computer/request.go`: `followRelease` falls back to the contract it saw; `releaseForOwner`
takes the invocation over when the owner exits before the release settled).

- Code: `cmd/socialweb-computer/request.go` (`carryOn`, `followRelease`, `releaseForOwner`, `runRequestRelease`),
  `internal/stack/request/owner.go`
- Test: `tests/compositions/0017` (module `tests/modules/acc-0017`), entries `run`, `killed`, `killedlate`, `half` and `devsup`; prod and staging (they
  touch only their own invocation and VM: one 1-vCPU VM each; the owner process is local)
- Last pass: see Evidence

## Preconditions

- An account that may rent (prod: bob; staging: a generated account); `socat` on the host running the test host
  (the owner runs on a pseudo-terminal)

## Steps

```sh
cd tests/modules && cargo wasm && cd ..
cd compositions/0017 && deno run -A --no-lock compose.ts > composition.json && cd ../..
SWC_TARGET=$PWD/targets/prod.json cargo run -p swc-test-host -- compositions/0017 --account bob --entry run
SWC_TARGET=$PWD/targets/prod.json cargo run -p swc-test-host -- compositions/0017 --account bob --entry killed
```

1. `request.start` with `pty: true` and `--hold --expires-in 1h`: the owner runs `run request` on a pseudo-terminal (stdin
   open), Held, then logs `ssh to the guest over ...` (not `no --exec and stdin is not a terminal`) and holds the session
2. 30 s after the ssh line (the session is up and still open), a SEPARATE process runs `run request release <name>`
   (`request.release`)
3. `run`: the release returns within 60 s (it fails at 90 s: the test's own cut of the old 27 minutes); the owner exits, and at its
   exit its summary (`requests/<name>/summary.json`) says the contract is Released and `kcpRunning` false (the owner stopped
   its kcp only after Released); the release process's summary says the same
4. `killed`: 100 ms after the release was asked, inside the release, the owner is SIGKILLed (`request.kill`: the `run request ... --name <name>`
   process of this invocation; its kcp child keeps running); the release client takes the invocation over (`TakeOwner`,
   restarts the kcp from its state, releases) and still returns within 60 s with the summary Released
   After the takeover release the summary says Released and `kcpRunning` false, a SECOND `run request release <name>` exits
   OK, and the invocation directory is gone
   `killedlate`: the same with the kill 2 s after the release was asked, while the owner stops its kcp after Released: the
   release returns OK, the summary's `kcpRunning` stays true (the dead owner's last word), the SECOND release must exit OK
   (the application is gone and the summary says Released: only the removal is left) and the directory is gone
5. Every record of the run is RecordNotFound afterwards
6. `half` (the guard of `restartLoaded`): `request.start` with `--hold --exec id`, a normal release (the application is removed,
   the summary says Released), then the test rewrites the phase of ITS OWN invocation's `summary.json` to Held
   (`request.setPhase`): the network application is gone but the state does not say the contract ended. `run request release
   <name>` must exit non-zero naming the phase (Held) and must leave the invocation directory (a VM may still be held);
   the test then removes its own directory (`request.removeState`). It touches only its own invocation: prod-safe

7. `devsup` (the restart race, no VM, no account): the test host runs `dev up --profile quick` (proc backend, a real devsup) in an
   instance of its own with the target's requester binary, then 12 times (delays 0.5, 1.0 and 2.0 s, 4 each) `dev down` SIGKILLed that
   long after it started (devsup keeps stopping bao, core, kcp, front, kine in order and exits) and `dev up` at once; 6 s later the kcp
   must answer `/readyz` 200. Every restart must succeed. It touches only its own instance on the test host (a local kcp, about 500 MB);
   it runs on prod and dev targets alike and skips when the binary has no `dev up`

## Evidence

capt dev network (hat-8, 3 bidders, 2026-11-14), CLI = main e13494b1 ("old") vs fix-owner-release 716f4ef4 ("fix"):

- `run`: old 6/6 pass, fix 6/6 pass (each with a 0014:concurrent load beside it). The owner's re-read failure that made prod
  gate 0004/0005 wait 27 minutes did not occur on the dev net: this entry passes on both and guards the scenario, it does not
  reproduce that failure there
- `killed` (the owner SIGKILLed 100 ms after the release was asked, which is inside the release): old fails twice (the release
  returns Released in 0.3 s through the dying owner, no summary is ever written: the invocation is left unsettled), fix passes
  twice (log `the owning process is gone before the release settled; taking the invocation over`, release 12.5 s and 14.8 s)

capt dev network (hat-8, 2 bidders, 2026-11-14), CLI = main a529874b ("old") vs fix-release-takeover-followups e18e82de ("fix"):

- `killedlate`: old fails 2/2 (the second release: `starting the invocation's kcp again from its state dir`, `the market network has
  no application <name>`, exit non-zero), fix passes 2/2 (second release OK in 0.1 s, directory gone)
- `killed` (a true takeover, the kill 100 ms into the release): old and fix both pass 2/2 with the strict assertions (summary
  Released, kcpRunning false, second release OK, directory gone): the takeover itself already worked on main
- `half`: fix passes; old fails (its release does not name the phase). The summary of the fixed release names `Held` and leaves the
  directory
- `run`: fix passes

- `devsup`, capt dev network (hat-8, 2026-11-14), the target's requester binary old = main a529874b vs fix = 7ebd228c: old FAILS 2/2
  (6 of 12 restarts each: `bootstrap: ... is Unknown (... is deactivating)` or a kcp not ready 6 s after its `dev up`), fix PASSES 2/2 (12 of 12
  ready, each `dev up` about 8 s: it waits for the old devsup to exit and starts a new one)
