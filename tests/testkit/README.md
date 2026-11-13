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

## Next: tests as atlibs compositions

Owner-approved (2026-11-12): each runbook becomes a composition on atlibs' step loop (events `start`, `result`,
`message`/`closed`, `timer`; outputs `calls`, `send`, `cancel`, `timers`, `state`, `done`; `Out`/`Fan`/`On` in the abi
crate). A runbook's Rust test here stays until its composition passes on staging and prod.

Host handlers, Rust, on atlibs-host's transport seam (`com.fedfork.exec.<name>.*`; the module never holds a credential:
kubeconfigs, keys and creds files are host config, named by ref):

| NSID | call / event source | repo |
| --- | --- | --- |
| `com.fedfork.exec.kube.{get,list,apply,patch,delete}` | calls; results as `result` | atlibs-kube (new; kube-rs, transports via atlibs-iroh / atlibs-webrtc / direct / overlay) |
| `com.fedfork.exec.kube.watch` | `subscribe`: each watch event a `message`; `closed` at its end | atlibs-kube |
| `com.fedfork.exec.ssh.{exec,session}` | exec a call; session a `subscribe` (output lines as `message`, `send` = stdin, `cancel` = disconnect); the ssh gateway (`-F /dev/null -p 22`) and guests | atlibs-ssh (new; the system ssh, as atlibs-iroh does) |
| `computer.socialweb.test.requester.{start,stop,restart}` | `subscribe`: its phases and ready file as `message`s (release, sha or local build; sign-in by app password or QR) | this repo, `tests/host` |
| `com.fedfork.exec.identlink.walk` | a call: identlink natively (its async walk does not fit a step loop in wasm) | atlibs-host |
| firehose, Jetstream | `subscribe: true` XRPC / JSON-text streams | atlibs-host (exists) |
| PDS, PLC | XRPC with the run's session; `http.fetch` for PLC `log/last` | atlibs-host (exists) |

Cleanup: atlibs-host's `managed_records` ledger (0.5.1: deleted past the calls budget) grows to every resource a run
created: kube objects it applied, requester invocations it started (released), throwaway DIDs (tombstoned). Writes and
deletes outside the ledger are refused, so fault injection on prod reaches only the test's own VMs and contracts.

Modules (Rust -> wasm): `assert` (record present / RecordNotFound x3, PLC tombstone, walk roots, timing bounds,
distinct-node placement); one state machine per runbook (act, wait on `On` sources, assert, timeout, cleanup), its
composition built by `compose.ts` into a committed `composition.json` (`atlibs compose check`), run from local module
dirs while iterating, published pinned for CI. Budgets: each composition declares its calls and wall time.

Order: atlibs-kube + atlibs-ssh + `tests/host`, the `assert` module, 0004, then one runbook at a time; the staging
stand-up is itself a composition (knotless runs it).
