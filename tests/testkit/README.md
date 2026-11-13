# testkit

The harness of the system tests (`tests/system`). Tests run against a target: staging (stood up fresh on rented VMs)
or prod (after a redeploy). No unit tests.

## Run

```sh
cd tests
SWC_TARGET=$PWD/targets/prod.json SWC_DRY_RUN=1 cargo test            # preflight only: target, creds files, requester binary
SWC_TARGET=$PWD/targets/prod.json cargo test --test acc_0004_vm_atirc_agent -- --nocapture
cargo run -p testkit --bin last-pass -- readings/0004-*.json          # passing runs onto docs/acceptance/README.md
```

| env | what |
| --- | --- |
| `SWC_TARGET` | the target.json (required) |
| `SWC_DRY_RUN=1` | stop after the preflight; touches no account |
| `SWC_SIGNIN=oauth-qr` | orchestrators sign in by OAuth QR (`scripts/oauth-signin.ts`, headless Chrome); default the app password |
| `SWC_READINGS` | readings dir (default `tests/readings`) |

## target.json

`targets/prod.json` is prod. Fields: `plc`, `pds` (where app-password sessions sign in), `relay`, `jetstream`, `atirc`,
`gateway` (`url`, `aud`, `ssh`, optional `kcp`), `bidders[]` (`name`, `did`, `operator`, optional `kcp`), `accounts`
(`handle`, `did`, `creds`, `oauthAccount`, `throwaway`), `agentConfig`, `requester`.

- `requester`: the orchestrator binary: `release` (newest `v*` tag), `release:<tag>`, `sha:<commit>` (CI's `main/<sha>`),
  `path:<file>` (a local build). Downloads are checked against `SHA256SUMS`, cached in `~/.tmp/swc-system-tests/bin`.
- `accounts[].creds`: a shell file exporting `*ATPROTO_HANDLE` and `*ATPROTO_PASSWORD`, mode 0600 (refused otherwise).
  Read in process, never printed; a log that holds a password fails the test. Staging accounts are throwaway
  accounts on the staging PDS (`throwaway: true`), tombstoned with the staging PLC at teardown.

## Pieces

| module | what |
| --- | --- |
| `lib.rs` | `run(runbook, test, accounts, body)`: per-account file locks (one driver per account), owner-only scratch dir, cleanups that always run (LIFO, also on failure or panic), readings JSON |
| `requester.rs` | the orchestrator: a Go requester (`run requester` / `run request`) signed in as an account; tests drive it through its kcp and CLI, never a market of their own |
| `kcp.rs` | `Kcp::connect(KcpEndpoint)`: a kube-rs client of any kcp (dynamic objects, server-side apply, waits) |
| `tenant.rs` | a tenant k3s (typed k8s-openapi): deployments, Ready pods per node, nodes, logs |
| `gateway.rs` | the gateway's cluster XRPC with the account's service auth |
| `atproto.rs` | atlibs-host: app-password sessions saved per account (no createSession per run), getRecord at computed keys (3 reads), PLC doc / `log/last` tombstones, rotation keys |
| `atirc.rs` | the owner mesh node: an `AtircNode` in the orchestrator's kcp, `atirc mesh pub/peers/sub` |
| `bidder.rs` | a bidder's own kcp: host-empty checks, pause / resume, fault injection |

## Reaching a kcp

kcp everywhere: a test talks to each kcp directly (the requester's, the gateway's, each bidder's), not only to public
endpoints, so it can inject faults (delete an in-flight VM object, pause a component, kill a guest) and assert the
recovery the runbooks describe (0007, 0006, 0001 section 7).

`KcpEndpoint { kubeconfig, workspace, via }`: the kubeconfig's credentials (client certificate, CA) over a transport.

| `via.kind` | transport |
| --- | --- |
| `direct` | the kubeconfig's server URL |
| `overlay` | the same port on a meshd overlay name (its DNS), TLS name kept |
| `ssh` | an `ssh -L` forward through `host` (an ssh alias) to the kubeconfig's host:port: a bidder's front on its loopback |
| `iroh` | atlibs-iroh: its ticket check, then its raw form `dumbpipe connect <ticket>` as the stream, one per TCP connection (`kcp::stdio_bridge`), TLS name kept |
| `webRtc` | `proxy`: a command piping stdin/stdout through a datachannel, one run per TCP connection (`kcp::stdio_bridge`); `Requester::webrtc_proxy(contract, tag)` builds it from the orchestrator's `run requester proxy` (the Go requester's WebRTC offerer, signalled through its kcp) for a kcp a guest tunnels as `tag` |

## What it reuses

| piece | from | for |
| --- | --- | --- |
| app-password sessions, service auth, PDS / PLC reads | atlibs-host v0.5.1 (`6e113df`, the rev atlibs-iroh pins: one copy) | `atproto.rs`, `gateway.rs` |
| iroh tickets, the raw `dumbpipe connect` stream | atlibs-iroh `e1cdf12` | `via iroh` |
| ident.link walks (verified `sync.getRecord`), the market's computed rkeys (`termination_rkey`, `release_rkey`) | identlink (atlibs-identlink `aa7cacc`, `native`) | `ctx.walk`, vm.terminated pins, the release's vm.delete |
| WebRTC offerer | the Go requester (`run requester proxy`) | `via webRtc` |
| the requester, its kcp, AtircNode, OpenBao | the release under test | the orchestrator |

Gaps, to close in atlibs rather than here:

- atlibs-iroh has no in-process QUIC: it shells out to dumbpipe (as js/index.js does). An in-process stream needs the
  `iroh` crate's endpoint dialing the dumbpipe ALPN in atlibs-iroh, returning an `AsyncRead + AsyncWrite`; kube-rs then
  takes it as a connector.
- atlibs-webrtc's Rust side is the answerer (`webrtc-peer`, str0m); the offerer is JavaScript (`WebrtcHost`,
  `recordSignals`). A native offerer needs, in that crate: str0m as offerer, the sealed offer record at
  `computer.socialweb.ingress.offer/<DID>.<tid>` and the answer read at its computed key (`recordSignals`), and a
  datachannel as `AsyncRead + AsyncWrite`.

Prod bidder kcps (`targets/prod.json`): owner-only copies of each bidder's admin kubeconfig under
`~/.config/swc-system-tests/kcp/<bidder>.kubeconfig` on the runner (refused unless 0600). Fault injection on prod
touches only VMs and contracts the test itself created (`BidderKcp::own_vm` matches the test's own receipt).

A requester elsewhere is driven the same way: the ssh gateway's per-user requester is a kcp (its endpoint in the
target); the browser requester (shell.socialweb.computer) is the atlibs requester wasm module in the page, driven by
headless Chrome and its WebRTC session.

## Rules

- Integration only: a test proves what a runbook says, on a real target. No unit tests, no mocks.
- Discovery: requests name no bidders list.
- Cleanups release VMs, delete records and check tombstones even when the test failed.
- One file per runbook in `tests/system/tests/acc_NNNN_*.rs`; each test names the runbook section it implements.

## Tests as atlibs compositions

Owner-approved (2026-11-12): each runbook becomes a composition on atlibs' step loop (events `start`, `result`,
`message`/`closed`, `timer`; outputs `calls`, `send`, `cancel`, `timers`, `state`, `done`). A runbook's Rust test here
stays until its composition passes on staging and prod.

| composition | entries (`--entry`) | live |
| --- | --- | --- |
| 0001 | `run` (sections 3-6), `s2` (staging: a temporary oauthQr bidder) | not yet |
| 0002 | `run` | not yet |
| 0003 (0001 section 9) | `run` (`--per-host`: each host in turn) | not yet |
| 0004 | `run` | not yet |
| 0005 | `run` (A), `b`, `r2fail` (a failed R2 leaves no live DID at PLC: known gap, fails until rc166) | not yet |
| 0006 | `run` (bidders), `compat` (`SWC_COMPAT_REQUESTER`) | not yet |
| 0007, 0008, 0009, 0012 | `run` | not yet |
| 0011 | `run` (gates), `token` | gates: pass 2026-11-13 (signed out) |
| 0013 | `run` (A), `c`, `b` (staging) | not yet |

0010 (iOS) is paused. Follow-ups: the CI freeze on the knotless orchestrator VM (a frozen run's check names its attach
route); `freeze::attach` ends when stdin closes (with the next atlibs-host change).

```sh
cd tests/modules && cargo wasm && cd ..                                     # the modules (wasm32)
cd compositions/0004 && deno run -A --no-lock compose.ts > composition.json && cd ../..
SWC_TARGET=$PWD/targets/prod.json cargo run -p swc-test-host -- compositions/0004 --dry-run
SWC_TARGET=$PWD/targets/prod.json cargo run -p swc-test-host -- compositions/0004 --account alice
SWC_TARGET=$PWD/targets/prod.json cargo run -p swc-test-host -- compositions/0004 --instance assert --entry tombstoned --input '{"dids":["did:plc:lpfuqerea3deuoyrn7ojser4"]}'
```

| piece | where | what |
| --- | --- | --- |
| `com.fedfork.exec.kube.{get,list,apply,patch,delete}`, `kube.watch` (subscription) | atlibs-kube (knotless) | any kcp or cluster the host names; writes only to what the run created, or what the host's guard allows; `cleanup()` deletes them and restores patched specs |
| `com.fedfork.exec.ssh.{exec,session}` | atlibs-ssh (knotless) | the system ssh to destinations the host names (`-F /dev/null`, `SetEnv`) |
| `computer.socialweb.test.{requester,request,account,atirc}.*` | `tests/host` (`swc-test-host`) | the requester under test as the account: `run requester` (its kcp becomes cluster `requester:<name>`), `run request` (its log events a subscription), the owner's mesh node; released and stopped after the run |
| `com.fedfork.exec.identlink.{walk,plc}` | `tests/host` (to move into atlibs-host) | identlink's verified walk, PLC log/last and documents of the target's PLC |
| `assert` | `tests/modules/assert` | records gone (x3), tombstones, an agent VM's chain; each asks again every 10 s for up to `within` s (180) |
| `acc-0004` | `tests/modules/acc-0004`, `tests/compositions/0004` | the runbook as a state machine |

### Freeze, inspect, replay

A failure (pass false, a run error, the module's deadline) freezes the run when `--on-fail freeze` (the default
outside CI; `CI` set: cleanup): no more events reach the module, every VM, requester and kube object stays, and the
run dir holds the dump. `--freeze-at <step|phase>` (repeatable) is a breakpoint; `kill -USR1 <pid>` freezes a live
run. A freeze ends at `continue` / `cleanup` or its TTL (`--freeze-ttl`, default 7200 s), then cleanup runs.

A run has two dirs: its dump (`$SWC_RUNS/<run>`, default `~/.tmp/swc-system-tests/runs/<run>`: the files below, no
secret; a CI workflow sets `SWC_RUNS=runs` and declares `artifacts: [runs/]`) and its work dir
(`~/.tmp/swc-system-tests/work/<run>`: requester state, kubeconfigs, keys, the store; never uploaded).

| file in the dump | what |
| --- | --- |
| `ticks.ndjson.zst` | every tick of every instance (atlibs-host `ticks`): seq, times, what woke it (a call's NSID and latency), the event, the output, the state's hash and diff, full snapshots; secret-named keys redacted; zstd, rotated past 64 MiB |
| `state.json`, `failure.json`, `freeze.ndjson` | the root's state; the failure with the last ticks; freezes and thaws |
| `resources.json` | requesters (state dir, pid, cluster), invocations (receipt, winner, logs, ssh command), kube objects, clusters (kubeconfig paths), temporary keys, observer nicks: paths only, no secret |
| `run.json`, `control.sock` | what ran; the control socket (MCP, 0600) while the run lives |

```sh
swc-test-host attach <run dir>     # stdio MCP: state, ticks, state_at, failure, freeze, step, continue, inject_event,
                                   # resources, kube_get, kube_list, kube_logs, ssh_exec, requester_ssh,
                                   # record_get, run_assert, cleanup, thaw, abort (ends with stdin)
swc-test-host replay <run dir> [--to <seq>] [--modules <dir>]   # the whole composition again from its ticks, no network:
                                   # {ticks, same, firstDiverge}; --to prints every run's state there; --modules: other wasm
swc-test-host cleanup <run dir>    # from resources.json, when the host died
```

Checked 2026-11-13 (signed out): the 0004 composition's `assert` instance, 7 ticks, replayed 7/7, `--to 3` gave its
state, a changed `assert` (one more result field) diverged at seq 7 (the tick of its result), attach answered `ticks`
and `state_at` while frozen at step 2 and ended with its stdin; the 0011 gates composition, 5 ticks, replayed 5/5.
Size: 1.4 KB for the 7-tick run, 27 KB for 0011's (HTTP bodies); a 0004 run (some 500 ticks: the invocation's log
events, mesh lines, calls) about 0.5 MB, estimated.

The host passes the guard: a test deletes only VMs of its own receipts, patches a bidder root only to have it restored.
Budgets: calls, events and wall time unlimited per run (each module bounds itself with a deadline timer).
