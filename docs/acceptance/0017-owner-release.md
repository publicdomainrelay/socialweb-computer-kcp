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
- Test: `tests/compositions/0017` (module `tests/modules/acc-0017`), entries `run` and `killed`; prod and staging (they
  touch only their own invocation and VM: one 1-vCPU VM each; the owner process is local)
- Last pass: see Evidence

## Preconditions

- An account that may rent (prod: bob; staging: a generated account); `script` (util-linux) on the host running the test host
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
4. `killed`: 2 s after the release was asked, the owner is SIGKILLed (`request.kill`: the `run request ... --name <name>`
   process of this invocation; its kcp child keeps running); the release client takes the invocation over (`TakeOwner`,
   restarts the kcp from its state, releases) and still returns within 60 s with the summary Released
5. Every record of the run is RecordNotFound afterwards

## Evidence

(not yet run)
