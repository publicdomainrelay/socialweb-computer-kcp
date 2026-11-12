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
| `iroh` | a `dumbpipe connect-tcp` forward of the kcp front's ticket, TLS name kept |
| `webRtc` | `proxy`: a command piping stdin/stdout through a datachannel, one run per TCP connection (`kcp::stdio_bridge`); `Requester::webrtc_proxy(contract, tag)` builds it from the orchestrator's `run requester proxy` (the Go requester's WebRTC offerer, signalled through its kcp) for a kcp a guest tunnels as `tag` |

atlibs-webrtc's Rust crate is the answerer (`webrtc-peer`) only; its offerer is JavaScript (pages). The Go
requester's offerer is the one a native test drives; a Rust offerer (str0m) replaces it when the harness must
dial without a requester.

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
