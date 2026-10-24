# AGENTS.md - working rules for this repository

Read this file first. It is the one guide for agents in this repository. `CLAUDE.md` imports it.

This repository (socialweb-computer-kcp) is the monorepo of the socialweb-computer project. It is the
single source of truth for its Go code. The only TypeScript left is `scripts/*.ts` and the policy
actions under `internal/policyengine/bundled-actions/tangy/`.

`docs/ARCHITECTURE.md` holds the decisions, the measurements and the invariants. This file points at it
and states the working rules. Open a section of `docs/ARCHITECTURE.md` by its heading when a change
needs the argument behind it. Do not read it end to end.

## Orientation: which file answers which question

| Question | File |
| --- | --- |
| What is this, in one paragraph? | `README.md` |
| The objective, stated once | `docs/ARCHITECTURE.md` section 1 |
| Where a concept lives; the package layout | `docs/ARCHITECTURE.md` section 12, and Layout below |
| Rules a change must not break | `docs/ARCHITECTURE.md` section 13 |
| Why a decision was made, and what it closed | `docs/ARCHITECTURE.md` section 15 |
| The plans and the status of each item | `docs/plans/*.yaml` |
| The live backlog (what is open now) | `NEXT.yaml` |
| The TypeScript that is left | `scripts/*.ts`, and the policy actions in `internal/policyengine/bundled-actions/tangy/` |
| Env the clusterprovider reads | `internal/providermain/config.go`; the gateway's defaults for it in `internal/stack/gatewaykcp/env.go` |
| Env the gateway reads | `internal/gatewaymain/config.go`, `deploy/socialweb-gateway.env.example` |
| How prod is installed and upgraded | `scripts/deploy.sh` (gateway: `install gateway`, then `upgrade`), `socialweb-computer install bidder` |
| What a host must provide | `socialweb-computer check-host -role gateway` or `-role bidder`, `docs/host-contract.md` |
| The market network | `deploy/market-network/README.md` |
| The operator handoff | `handoff.yaml` |
| What CI runs on a push, and its runners | `.github/workflows/ci.yml`: the gate, the guest image (fetched from `guest-images/<target>/<pinned image.json sha256>` in the bucket when it is there, built otherwise) and the Spaces publish on GitHub-hosted runners; the pin check (`pins`) and the release builds (`binaries`) on the self-hosted runner of `scripts/gh-runner-install.sh` (account socialweb-ci) |
| The integration scenarios (no Go tests until plan 0027) | `docs/spec/integration-catalog.yaml` |

## Layout

Go:

| Directory | What it holds |
| --- | --- |
| `cmd/` | one directory per binary (`socialweb-computer`, `socialweb-node`, `socialweb-vmexec`, `socialweb-guest-agent`, `socialweb-apigen`, the two OpenBao auth plugins) |
| `internal/` | the Go packages, in the plan 0026 layers (`docs/ARCHITECTURE.md` section 12) |
| `api/` | the kcp API types; `go generate ./api/...` writes the schemas, the APIExports and the generated Go |
| `deploy/` | manifests, sets, the generated schemas and exports, `check.sh` |
| `docs/` | `ARCHITECTURE.md`, `plans/`, inventories, research notes |
| `rust/` | socialweb-meshd |

TypeScript:

| Directory | What it holds |
| --- | --- |
| `scripts/*.ts` | one-off operator scripts (`oauth-signin.ts`, `associate-bidder.ts`, `publish-spaces.ts`), run with `deno run` |
| `internal/policyengine/bundled-actions/tangy/<action>/` | one policy action: `index.ts` and its built `dist/index.js`, embedded by the policy engine |

There is no deno workspace, no lockfile and no vendored TypeScript repository here any more.

## The TypeScript that is left

- `scripts/*.ts` are one-off operator scripts, run with `deno run -A`. No Go code runs them; the
  `publish` job of `.github/workflows/ci.yml` runs `scripts/publish-spaces.ts`.
- The policy actions under `internal/policyengine/bundled-actions/tangy/<action>/` are gha-lite assets
  that the policy engine embeds: `action.yml`, `index.ts` and its built `dist/index.js`. Edit them as
  such assets, with the workflows beside them in `internal/policyengine/bundled-actions/workflows/`.
- No comments in code. ASCII only.

## The kcp paradigm, port 0, and the RFP flow

- kcp is part of each application, not infrastructure. An application instance is a kcp of its own (or,
  in a market network, a workspace of the network's one kcp), a manifest set and a profile. State is kcp
  objects. Components (Pds, Plc, Relay, Bidder, Requester and the other component kinds) run in
  process in the core, in the component runtime of `internal/component`, and host processes are
  HostProcess units; both are reconciled from those objects. Only the virtual Kubernetes clusters
  project uses kcp in the traditional way.
- Which kcp, by profile: a prod profile (the gateway, `run bidder`, `run request` without `--network`)
  runs a strict kcp per application. The market network profile (dev, CI, soak) runs every application
  in one kcp, each in its own workspace `root:<app>` (components in `root:<app>:app`), from the same
  manifest set: the applier rebases the set's workspaces under the application's workspace. Either way
  applications talk only over the atproto market protocol; the shared kcp is composition and isolation,
  never a data channel. Write service names in manifests as `<name>.default.{{.ServiceSuffix}}`.
- Port 0 behind the edge: every listener binds port 0 and reports its bound address into the status of
  its kcp object. Peers find it by service name through kcp. Only the stable public entry points
  (caddy on 443, an application's front) keep a known address.
- The RFP flow is the only way a guest comes up: the requester posts a `compute.vm` record and a signed
  `market.rfp`, bidders bid, the requester accepts, and the winner boots the guest from cloud-init
  `user_data` (`BuildUserData` and the module registry in `internal/atproto/cloudinit`).
  The requester reaches the guest only through the guest transports below. Never start a container or VM by hand in
  code or in a test, and never install a guest agent outside cloud-init. A live run that needs a
  guest brings up its own market network,
  and `socialweb-computer run request` or a tenant cluster of the network's gateway rents the guest.
  `run request` composes a MarketRequester root (`internal/app/marketrequester`), and its Requester
  component (`internal/services/requester` over `internal/market/requester`) runs the RFP, the
  accept and the release; `run request list`, `run request release` and `run request resume` manage
  the invocations it recorded.

## Guest transports, guest events and the host-loopback fence

- Three guest transports, and no other. Never add one.
  - iroh is the default and required transport: every guest gets the iroh module, the requester's
    ssh and `status.contract.vmAddress` are iroh, and a contract is Held on the iroh address and the
    host keys alone.
  - opentunnel https, opt-in per entry of the ComputeContract's `spec.tunnels` with transport
    `opentunnel` (the default): one https URL per tag for the guest's `127.0.0.1:<port>`, back as a
    `vm.onNetwork` kind `https` in `status.contract.networks` (plans 0032, 0034).
  - webrtc, opt-in per entry with transport `webrtc`: socialweb-guest-agent (`cmd/socialweb-guest-agent`,
    `internal/guestagent`, pion and age) bridges each datachannel labelled with the tag to
    `127.0.0.1:<port>`; its DTLS fingerprint comes back as a `vm.onNetwork` kind `webrtc`. The CLI
    side is `run request proxy <name> --tag <tag>` (`internal/webrtcproxy`), and `run request ssh
    --transport webrtc` uses it as the ssh ProxyCommand; `run request --tunnel <tag>:<port>[:<transport>]`
    asks for tunnels. STUN is `stun:stun.cloudflare.com:3478` only; no TURN (plan 0034).
- The bidder installs the guest binaries, never the requester and never the internet. The requester's
  tunnel module carries config only; the bidder adds `setup-guest-binaries.service`
  (`internal/guestseed`), which fetches opentunnel or socialweb-guest-agent from its own
  GuestControlPlane `GET /v1/downloads/<sha256>/<name>` at `http://192.0.2.1:<port>` (a local
  Exposure), checks the sha256 and installs to `/usr/local/bin`. The binaries are the release asset
  `socialweb-guest-downloads` (`scripts/build-guest-downloads.sh`). A guest never fetches from GitHub or
  GHCR. The did:key ingress cannot carry binaries (JSON string bodies, a frame cap).
- Guest events live under `com.publicdomainrelay.temp.compute.events.guest.*` in the market.event
  envelope. The guest holds no repo key: it POSTs `{acceptUri, acceptCid, record}` to GuestControlPlane
  `POST /v1/events` (vm.onNetwork, guest.publicKey, guest.signalWebRTC) and the bidder signs and
  publishes them; requester events to the guest reach it through the bidder's per-ComputeVM queue
  and the XRPC subscription `com.publicdomainrelay.temp.compute.guest.subscribeEvents` (WebSocket,
  `cursor`, JSON text frames, an empty binary keepalive every 25 s). The guest bearer is the
  `Authorization` header only. The bidder routes by NSID prefix and never opens a sealed body.
- Signaling is sealed. Each side publishes a `guest.publicKey` (kind `age-x25519`) per tag; an offer
  is age ciphertext sealed to the guest's latest key, with plaintext `{sdp, replyTo}`; the answer is
  sealed to `replyTo`. A signal names no recipient: a reader trial-opens every signal for its tag.
  No private key, SDP, ICE candidate or IP address is ever in a record, a ContractEvent, a StateSync,
  kcp or a log. The requester side of each event is a cluster-scoped `ContractEvent`
  (`socialweb.computer/v1alpha1`, outbound ones made by the CLI or the browser, inbound ones by the
  requester), owned by its ComputeContract.
- The guest fence. socialweb-vmexec loads the nf_tables bridge table `swcguest` in the guest's
  namespace before the tap exists, and the VM does not start without it (`internal/guestnet`,
  `fence_linux.go`). On frames from the guest's tap it keeps, in order: TCP to the host's loopback at
  `192.0.2.1` only on the ports of the ComputeVM's `spec.hostPorts` (every other TCP connection to it
  is reset, the rest dropped); DNS (udp and tcp 53) to the pasta gateway 172.30.0.1; ICMPv6 router and
  neighbor discovery. It then drops everything to the bidder host's own addresses (`vmruntime`
  lists them in the host namespace at Prepare and writes them into the vmexec params) and to the
  guest link 172.30.0.0/30, and to 0/8, 10/8, 100.64/10, 127/8, 169.254/16, 172.16/12, 192.0.0/24,
  192.168/16, 198.18/15, 224/4, 255.255.255.255, ::1, fc00::/7, fe80::/10 and ff00::/8, and every
  ethertype but IPv4, IPv6 and ARP. TCP to the LAN is always closed. One allowance, on by default:
  UDP to 10/8, 172.16/12, 192.168/16 and fc00::/7 on destination ports 1024 and up, except 1900,
  5353 and 5355, so same-LAN WebRTC can use a LAN candidate; it never reaches the host's own
  addresses, 100.64/10, 169.254/16 or 127/8. It is the ComputeVM's and FirecrackerVM's
  `spec.guestNetwork.lanUDP` (unset: true), which the bidder writes from its own
  `spec.guestNetwork.lanUDP` (MarketBidder, Bidder, NetworkApp), the bidder operator's choice and
  never the requester's: `run bidder`, `install bidder` and `dev up` take `--no-guest-lan-udp`, the
  hono-bidder passthrough takes `--guest-lan-udp=false`. A second allowance, `hostUDP`, comes before
  the drop of the host's own addresses: UDP to those addresses (never loopback or link-local, never
  TCP) on the same ports, so WebRTC works when the requester runs on the bidder's own host (the
  router does not hairpin). It is the ComputeVM's and FirecrackerVM's `spec.guestNetwork.hostUDP`
  (unset: false), which the bidder always writes as its effective value: its own
  `spec.guestNetwork.hostUDP` (MarketBidder, Bidder, NetworkApp) when set, else on when its
  `spec.policy` is `bidder-only-me` and off otherwise; the Bidder's `status.guestNetwork` reports
  `{lanUDP, hostUDP, hostUDPReason}` (`spec`, `policy only-me`, `policy not only-me`). `run bidder`,
  `install bidder` and `dev up` take `--guest-host-udp true|false|auto`, the hono-bidder passthrough
  `--guest-host-udp`. A guest-facing local listener the bidder hands to a guest goes into
  `spec.hostPorts` (`GuestHostPorts`, `guestseed.HostPortsOf`); never widen the fence to all ports,
  and never open TCP to the LAN or to the host's own addresses.
- After a change to `cmd/socialweb-vmexec` or `internal/guestnet`, run `make pins` and commit the
  `socialweb-vmexec` pin with the change, then `dev down` and `dev up` the network instead of a
  `dev restart ... core`.

## Tests: none until plan 0027

Plan 0026 deleted every Go test. Write no Go test of any kind until plan 0027.

- No tests. Do not write unit tests, ratchets, allow-lists, inventories, golden files, snapshots,
  source-grep checks, size lines or any other check of an arbitrary rule. Do not work test-first.
  Ignore the `test_first` field of a plan item. Do not restore a deleted test.
- Proof is the build gates plus live runs. A manual live run is proof, not a test.
- From plan 0027 on, tests are specification tests written from `docs/spec/integration-catalog.yaml`:
  each states what a component must do (actors, inputs, observable outcome), never how it does it.
  Only high-level integration tests are written.
- Rust tests under `rust/` are outside this rule and stay.

## Prod deployments

A prod deployment guarantees nothing; the routine path preserves most things.

- The routine deploy (`scripts/deploy.sh` on an installed gateway) upgrades in place: state dir, kcp
  objects, OpenBao contents, the requester's and bidder's DIDs, association records, web sessions and
  running HostProcess units, which the new core adopts by unit name and InvocationID, all survive
  (read on prod: web session across rc8 to rc9, DIDs and association
  across rc11 to rc21). A fresh install, or a lost or purged state dir, preserves nothing and needs
  one association per identity again.
- Losing sessions, VMs, clusters, claims, receipts or any other prod state during a deploy is
  acceptable. Do not plan migration, carry-over or rollback for a change of shape. Deploy fresh when
  the shape changed.
- The atproto credentials of the two prod accounts are on the deploy host, owner-only (mode 0600):
  `~/.aliceoa_bsky_social_source` (requester account alice; exports `ALICE_ATPROTO_HANDLE`,
  `ALICE_ATPROTO_PASSWORD`) and `~/.bobvmbuilder_bsky_social_source` (bidder account bobvmbuilder;
  exports `BOB_ATPROTO_HANDLE`, `BOB_ATPROTO_PASSWORD`). When a deploy loses a session, sign in again
  from these files. Source them into the one command that needs them; never print, log, commit or
  copy their values, and never put them in a kcp object, a HostProcess argument or a plan.
- OAuth sign-ins are scripted; never hand a sign-in link to the user. `scripts/oauth-signin.ts` drives
  a headless Chrome (`/usr/bin/google-chrome`, puppeteer-core) through the atproto OAuth flow with the
  handle and password from a credential file: the QR session-transfer page (qr.fedfork.com, a bidder's
  `prod-qr` MarketIdentity `pendingAction.url`) or the web app's sign-in page, the PDS's authorize
  page, then "Authorize". Example, for the hat-8 bidder:
  `deno run -A --no-lock scripts/oauth-signin.ts --creds ~/.bobvmbuilder_bsky_social_source --account BOB --url "<pendingAction.url>"`.
  It reads the file itself (values never on the command line or in logs) and writes step screenshots
  to `~/.tmp/oauth-signin/<ts>/` (mode 0700) for debugging. A QR link expires after 5 minutes; read the
  current one from the MarketIdentity status right before running it.
- After a fresh gateway deploy, the requester alice has no web app session: the cluster provider logs
  "no oauth session stored for did:plc:..." on every claim. Sign alice in with the same script and
  `--url https://k8s.socialweb.computer/ --start-api` (the gateway is API-only: `--start-api` POSTs
  `/oauth/start` with the handle and drives the returned authorization URL).
- A bidder's guest image comes from `--guest-image-source` (a `file://` or `https://` base that
  `install bidder` writes to config.json), or, when that is unset, from the published artifacts:
  `run bidder` reads `guest-images/<target>/<pinned image.json sha256>`, then `releases/<version>` and
  `main/<commit>` under `--guest-image-release-base`, the public Spaces bucket by default. CI uploads a
  guest image once, to that content-addressed key, and only when the key does not hold it yet. On hat-8 the pinned image is in
  `/var/lib/socialweb-computer/guest-image` (root-owned; the bidder verifies every file by the sha256
  pinned in internal/versions/versions.json).
- The did:key ingress relay at xrpc.fedproxy.com is the Go relay `socialweb-computer internal
  ingress-relay` (`internal/atproto/ingress`), behind caddy on a unix socket as the
  `30-go.conf` drop-in of `xrpc-relay-server.service`. Deploy it only with
  `scripts/deploy-xrpc-relay.sh` (build, ship, restart, health check, rollback on a failed check);
  `--rollback` removes the drop-in and goes back to the deno relay, `--status` only reads. Never run
  `atproto-reverse-proxy/scripts/deploy.sh`: it prints `CF_API_TOKEN`. A change to the ingress
  protocol (`internal/atproto/ingress`) ships to both ends: the core and this relay.
- Only the lead runs a prod deployment (plan 0006 M5-FRESH-DEPLOY). Task agents never touch prod.

## How to verify

- While iterating, build only what you touched: `go build ./internal/<pkg>/... ./cmd/socialweb-computer`.
  Do not run the full gate per change.
- The full gate runs ONCE, right before "ready": `go build ./...`, `go vet ./...` and
  `deploy/check.sh --fast`. GitHub CI runs the same three on every push to `main` and `task/**`
  (`.github/workflows/ci.yml`); read the branch's run on GitHub.
  - `deploy/check.sh --fast`, in order: `gofmt -l` of `api`, `internal`, `cmd`, `hack` and `lexicons`;
    `go build ./...`; apigen into a scratch directory and a diff against every file
    `go run ./cmd/socialweb-apigen -root . -list` names (generated code must match its source);
    `go generate ./...` must leave `api`, `internal/atproto/market` and `deploy` unchanged;
    `go vet ./...`; `scripts/check-architecture.sh`; `go build` and `go vet` of each Go asset module
    in `internal/versions/versions.json`; `go build` and `go vet` for darwin/arm64, darwin/amd64,
    windows/amd64, windows/arm64 and linux/arm64, each with a `-tags release` build of
    `cmd/socialweb-computer`; and the `-tags release` build of every release platform.
  - `deploy/check.sh --local` adds `cargo fmt --check` and `cargo test` of `rust/`, and
    `socialweb-computer dev verify-assets` on a release build when the host's blobs are staged; it
    lists what it skipped. `deploy/check.sh --full` makes a skip a failure.
- The binary for live runs: `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/<work>`
  (`make dev` is the same with `/var/tmp/swc`). It builds one socialweb-computer of the working tree
  (uncommitted changes included) for the host platform, or for each `--platform`. It embeds every
  pinned asset, staged once per `versions.json` entry; the first `--dev` build on a host stages every
  asset (about 3.5 minutes), later builds only an asset whose entry changed. `socialweb-computer
  version` says "dev build, not reproducible" with the commit and the dirty state. `--publish`
  copies the host binary to `<publish>/bin/socialweb-computer` and mirrors the working tree to
  `<publish>/src`, both readable by test-0001. Output: `~/.tmp/socialweb-computer-build/dev/<worktree>/`.
- A Go change in a running dev instance or market network: rebuild with `--dev --publish`, then
  `socialweb-computer dev restart <instance> core --binary /var/tmp/<work>/bin/socialweb-computer`
  (as the account that runs the instance). It stops the core unit, places it again with the new
  binary, waits until it is ready, prints a JSON report and fails unless every unit was adopted
  unchanged. A child application of a market network takes `dev restart <network> <app>/core` (or
  `dev restart <network>-<app> core`); the network's supervisor places that core through its
  HostProcess object. Then rerun `run request`. Do not `dev down`/`dev up` for a code change.
- One component: `socialweb-computer dev restart <instance> [<app>/][<workspace>:][<kind>/]<component>`.
  It writes `spec.restartedAt` on that component's kcp object and waits until the component reports
  the restart. The PolicyEngine restarts only with `core`. A kcp roll is `dev roll kcp`; on an installed host,
  `units roll kcp`, `units roll cluster` and `units roll link`.
- The release binary (prod only; the lead builds it): `scripts/build-socialweb-computer.sh` without
  `--dev` builds one commit once per release platform of `versions.json` from a clean checkout:
  every pinned asset staged by `scripts/stage-assets.sh` and checked against its pin, the embedded
  Deno of the policy engine, then `go build -tags "release embed_deno"` with `CGO_ENABLED=0`.
  Each step is keyed by its inputs and does not run again when they did not change: asset blobs by
  their pinned inputs (assets built here by their tar bytes), socialweb-meshd by rust/ and the
  toolchain, plus the go build cache and the cargo target dir. Tens of seconds warm; minutes with
  empty caches. Every phase prints a `timing` line. `make` builds with `--update-pins`: assets built
  from this repository or from pinned upstream source are rebuilt and their new pins written to
  `internal/versions/versions.json`; downloaded assets still verify their pins and never move.
  `make pins` re-stages and re-pins only the owned assets. Commit the pin change with the source
  change. The `pins` CI job runs `make check-pins`, which runs `make pins` and fails when
  `versions.json` differs from HEAD. Checkouts, caches and outputs stay under
  `~/.tmp/socialweb-computer-build`; `release/<commit>/` holds the binaries and `SHA256SUMS` that
  `install` and `upgrade --binary --sha256` take. Never build it in the inner loop.

## Where and how to work

- Work in a plain git worktree or branch of this repository. Do not put a worktree inside another
  worktree.
- Commit small logical commits and push as you go.
- Before you mark a goal complete cut a release candidate
  - Babysit CI every time you tag a release candidate. You are only allowed to
    tag rc's, never full releases.
  - Cut more rc's until CI is green and you have validated that they install and
    work on all bidder hosts and prod gateway.
- Never use `/tmp`; it is tmpfs and spills to swap. Use `~/.tmp` (set `TMPDIR` under it).
- A one-off script is a `.ts` file under `$TMPDIR`, run with `deno run -A`, using `Deno.Command` for git
  and go. Keep each shell call to one plain command.
- Plans live in `docs/plans`. Flip an item's status only. Do not add evidence lists; the git history is
  the evidence.
- Rebase on `origin/main` often (`git fetch && git rebase origin/main`). When a rebase stops on
  conflicts, run `scripts/regen-after-rebase.sh` (no arguments). It stops first when a generator
  input is conflicted (`api/*.go`, `internal/apigen`, `cmd/socialweb-apigen`, `hack/codegen.sh`):
  resolve those by hand, `git add` them and run it again. Otherwise it drops the conflicted
  `api/**/zz_generated.*.go` files, reruns `hack/codegen.sh`, takes one side of every conflicted file
  that `go run ./cmd/socialweb-apigen -root . -list` names (the schemas and exports under `deploy/`,
  the zz_generated register and conditions files), reruns `go generate ./api/...`, stages the
  outputs and prints the hand-written conflicts left, or the command to continue. Never hand-merge
  generated output. `deploy/apiresourceschema-revisions.txt` merges itself (`merge=union`); when it
  names one schema twice, the script keeps the HEAD line, and the generator may ask for a Revision
  bump in `api/v1alpha1/resources.go`.
- Live runs: your own market network only, run as the VM account (test-0001) from the binary and the
  mirror that `--dev --publish /var/tmp/<work>` wrote, with an instance name and bidder handle unique to
  your work. Keep the network up for the whole item; clean up only your own units and VMs.

## The fast loop for one item

- Compile: `go build ./cmd/socialweb-computer` (about 4 s in a new worktree, the Go caches are
  shared).
- Binary and mirror: `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/<work>` (about 21 s the
  first time in a worktree, about 6.5 s after). Later changes: build again, then
  `dev restart <instance> core --binary /var/tmp/<work>/bin/socialweb-computer`.
- One network for the whole item: `dev up` once (80 to 95 s, mostly waiting, about one core), then every
  proof is a `run request --network <instance>` (80 to 90 s each, 35 s of it the requester's own kcp).
  `dev down` costs 70 to 95 s (one-kcp network with a gateway). Do not `dev up` per proof.
- A held guest burns one host core for as long as it lives (an idle firecracker vCPU does not halt), so
  release a `--hold` invocation as soon as you have read it.
- Take a live slot before `dev up` and release it after `dev down`:
  `deno run -A ~/.tmp/swc-lead/live-slot.ts acquire|beat|release <name>`. The default is 5 slots
  (`SWC_LIVE_SLOTS`), and `acquire` also waits while MemAvailable is under 12000 MB
  (`SWC_LIVE_MIN_AVAIL_MB`) or cpu pressure avg10 is over 90 (`SWC_LIVE_MAX_CPU_PSI`).

## The loop

- There is no container. Read the state of an instance with `socialweb-computer units list` and its
  units' journals (`journalctl --user -u <unit>`); the processes already log a status line.
- NEVER write a sleep-poll loop such as `for i in $(seq ...); do sleep ...; journalctl ...; done`.
  It blocks a turn for minutes and cannot fail. Watch the log with the Monitor tool instead (below).

## Turns and background work

- Never end a turn with a build, test or live run in flight. Background a long command and
  keep working, or wait for it, but do not hand back a turn that says "still running".
- Never block the foreground on a long run. Start a live run (`dev up`, `run bidder`, `run request`)
  or a build with run_in_background and wait for its completion notification, or watch its log with
  the Monitor tool (filter for the success line AND the failure lines). Foreground `until` loops
  only for waits under about a minute; no `sleep N; tail` chains. Do other work (docs, the next
  commit) while it runs.
- Parallelize reads, greps and edits. Live runs go in parallel too: every listener binds port 0, and
  the VM account test-0001 has 16 VM slots that vmruntime hands out under a per-account lock (no
  whole-run lock; test-0001 linger stays on). Keep heavy builds few: 16 cores.
- Launch subagents for parallel work, then verify the artefact (the file, the exit code, the log
  line) rather than the summary.
- Nested subagents hit a depth limit. Do not spawn a subagent from inside a subagent; do that
  reading inline.

## Edit discipline

- Read the exact bytes before editing. The edit tool matches text, and a guessed string is a
  failed edit.
- A block that repeats: use `replaceAll`, not one edit per copy.
- One failed match is fixed by re-reading and retrying. Do not guess the same string again.
- Do not revert another agent's work. Run `git status` first; the tree is shared.

## Architecture rules (plan 0026)

- Layout: the TypeScript ABC layers in Go. Imports point down this list only;
  `scripts/check-architecture.sh` enforces it:
  - `api/<group>/v1alpha1` (common): types, contract status, refs.
  - `internal/contract/<concept>` (abc): small Go interfaces and pure types; api and stdlib only.
  - `internal/kit/<name>` (common helpers): no concept; api, contract, kit and third-party only.
  - `internal/controllers/runtime/<pkg>` (transport): the kcp transport (`kube` typed client and
    awaits, `kcpapply`, `manifests`); the only home of the dynamic client and unstructured objects.
  - `internal/<concept>/<impl>` (impl): one sibling package per backend, the real work, no kcp.
  - Adapters: `internal/controllers/<kind>` turns kcp objects into contract calls (one reconciler per
    kind on `internal/controllers/runtime`); `internal/services/<svc>` turns XRPC/HTTP routes into
    contract calls and holds no role logic.
  - `internal/app/<root>` (factory): one composition controller per app root; wires impls into the
    bus and picks a backend by the `backendRef` kind, never by a flag. Composition code
    (`internal/stack/**`, `internal/devcore/**`, `internal/<x>main`) is this layer too.
  - `cmd/` (CLI); tools (`hack/`, `internal/apigen`, `internal/lexgen`) rank with it.
- Gate. `scripts/check-architecture.sh` (`go run ./hack/archcheck`) fails on any violation of: the
  layer order; backend deps; a ticker or periodic requeue site missing from `deploy/obligations.txt`;
  a System caller missing from `deploy/bus-bootstrap-callers.txt`; a dynamic client or unstructured
  import outside the transport and apigen; a GroupVersionResource literal outside `api/` (foreign
  groups are vars in `api/kcpgvr`); an annotation read as a signal (kcp and kubelet metadata
  conventions are listed in the checker); an endpoint field in spec without `apigen:"userEndpoint"`;
  an impl package without a compile-time assertion against an `internal/contract` interface.
- Go idiom. No factory types: `New(cfg, deps)` takes explicit dependencies and returns a concrete
  struct; callers accept the contract interface. Every impl states its contracts at compile time
  (`var _ compute.Provider = (*Provider)(nil)`). One package, one concept; no util package outside
  `internal/kit`. kcp is a transport like XRPC or the CLI: an adapter translates it.
- No polling. Controllers are driven by watches on their own kind and every kind they ref. Deadlines
  use delayed requeue; errors use the runtime's rate-limited backoff. A ticker is allowed only for an
  outbound obligation and must be listed in `deploy/obligations.txt` with its owner and period.
  systemd units are followed through D-Bus signals, processes through wait.
- Bus. Components and controllers get typed contract handles with `bus.Resolve[T]`. The caller is the
  reconciled object or the component instance, never a controller and never System; System is allowed
  only for the callers in `deploy/bus-bootstrap-callers.txt`. The callee holds credentials; a handle
  never exposes a token. A call with no accepted ComponentLink fails with `ErrLinkDenied`.
- Testable backends. A backend package (`internal/<concept>/<impl>` and the host libraries it drives)
  runs without kcp, the bus or a controller runtime: it takes and returns `internal/contract` and api
  value types, and host effects (systemd, processes, filesystem root, clock) come in through the small
  interfaces of `internal/contract/host`. The controller translates objects to contract values.
  `scripts/check-architecture.sh` reports a backend whose deps reach kcp.
- Writers. Every write is server-side apply with field manager `socialweb.computer/<controller>`. A
  spec is written only by its composer; a status only by the controller of its kind; a field a second
  controller reports is its own status subfield with its own field manager. Every child and intent
  object has an ownerReference to its composer or creator. No annotation carries a control signal.

## Standing constraints

- No explanatory comments in code. The code explains itself through its structure: names, layers,
  contracts and types. Two kinds of comment are allowed: the `ponytail:` form naming a ceiling and its
  upgrade path, and functional comments that a tool reads, such as `//go:` directives, generated-code
  headers and code generator markers (`// +k8s:deepcopy-gen=package`, `// +groupName=`,
  `// +k8s:deepcopy-gen:interfaces=...`). A functional comment carries no prose.
- ASCII only, everywhere, including this file and `docs/ARCHITECTURE.md`.
- Decisions, measurements and invariants live in `docs/ARCHITECTURE.md`, never in code comments.
  An item opens in section 14 and moves to section 15, with its before/after readings, when it
  closes.
- The invariants in section 13 are load-bearing; a change must not break one.
- Runtime runs as USER 1000:1000. Nothing on the L0 host needs root.

## Names: use the real name

Write the real name of a thing: the repository, directory, package, binary, command or plan item id.
Do not make up a short name. Write plain, short sentences, with one meaning for each word. The plans
and older notes use the short names below; read them as the real names.

| Short name in the plans | Real name |
| --- | --- |
| the door | the gateway's web app and tenant API: the GatewayAPI component (`internal/services/gatewayapi`) and `internal/gatewayhttp`, served by `socialweb-computer serve gateway xrpc` |
| the ssh door | the SshServer component (`internal/services/sshserver`) of the SshGateway root (`internal/app/sshgateway`), run by `socialweb-computer run gateway ssh` |
| the core | the `socialweb-computer` process of an instance that runs the controllers (`internal/stack/componentcore`) and supervises the components and HostProcesses; `internal dev-core` in a dev instance |
| a fold, folded | one pass that merged a set of decisions into the plans |
| the spine | the RFP flow: requester, RFP, bid, accept, cloud-init `user_data` |
| the front | the stable entry point in front of a service: the kcp front before the kcp slots (0006 M2-KCP-SLOTS), the TLS front of the market network |
| the edge | caddy and the other stable public entry points that keep a known port |
| a stand-in | a small test executable used in place of k3s, kine or postgres |
| the soak | the last run against the real accounts (the bobvmbuilder bidder and alice), after the market network passes |
| a gate | a check that must pass before an item or a merge goes on (`deploy/check.sh`, a plan's done_when) |
| the seed | the first example that a model grows from |
