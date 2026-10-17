# AGENTS.md - working rules for this repository

Read this file first. It is the one guide for agents in this repository. `CLAUDE.md` imports it.

This repository (socialweb-computer-kcp) is the monorepo of the socialweb-computer project. It is the
single source of truth for its Go code and for all of its TypeScript. The TypeScript of ten
repositories lives here with its git history, under `ts/<repository>/`. Those ten repositories are
frozen. Never edit them for a path that is under `ts/`. Make the change here.

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
| Which TypeScript entries ship | `deploy/ts-entries.yaml` |
| The TypeScript workspace, its tasks and versions | `deno.json`, `deno.lock` |
| Env the clusterprovider reads | `internal/providermain/config.go`; the gateway's defaults for it in `internal/stack/gatewaykcp/env.go` |
| Env the gateway reads | `internal/gatewaymain/config.go`, `deploy/socialweb-gateway.env.example` |
| How prod is installed and upgraded | `scripts/deploy.sh` (gateway: `install gateway`, then `upgrade`), `socialweb-computer install bidder` |
| What a host must provide | `socialweb-computer check-host -role gateway` or `-role bidder`, `docs/host-contract.md` |
| The market network | `deploy/market-network/README.md` |
| The operator handoff | `handoff.yaml` |
| What CI runs on a push, and its runners | `.github/workflows/ci.yml`: the gate, the guest image and the Spaces publish on GitHub-hosted runners; the release builds (`binaries`, `release-reproducible`) on the self-hosted runner of `scripts/gh-runner-install.sh` (account socialweb-ci), because the assets they stage are pinned by the bytes that host's toolchain produces |
| Where the integration suite runs | a runner of `scripts/ci-runner-install.sh` (`.gitlab-ci.yml`, account socialweb-ci, tag socialweb-computer-kcp): on hat-8, or on the droplet gl-runner-1 (tag dev-work, `scripts/dev-droplet.sh create` + `setup`, then the install script). It needs kvm, `systemd --user`, an `/etc/subuid` range, `/usr/bin/google-chrome` and `/usr/local/bin/kubectl` (the pinned k3s minor), which a hosted runner has not. The droplet is off the tailnet: on hat-10 the user unit `gl-runner-1-tunnel.service` of johnandersen777 runs `ssh -N -R 127.0.0.1:8443:gitlab.dorper-matrix.ts.net:8443 root@<droplet>` (key `~/.ssh/gl-runner-1-tunnel`, allowed on the droplet only with `restrict,port-forwarding,permitlisten="127.0.0.1:8443"`), and the droplet's `/etc/hosts` maps gitlab.dorper-matrix.ts.net to 127.0.0.1, so TLS and clone URLs are unchanged. Off Fedora 44 the job seeds the pinned downloads blob (`scripts/ci-seed-downloads.ts`) |
| A throwaway GitLab for integration runs | `scripts/gitlab-fixture.sh up` / `down`, `deploy/gitlab-fixture/template/` |

## Layout

Go:

| Directory | What it holds |
| --- | --- |
| `cmd/` | one directory per binary (`socialweb-computer`, `socialweb-gateway`, `socialweb-tsbundle`, ...) |
| `internal/` | the Go packages; `internal/tsbundle` finds and embeds the TypeScript |
| `api/` | the kcp API types; `go generate ./api/...` writes the schemas and the TypeScript wire types |
| `deploy/` | manifests, sets, scripts, `check.sh`, `ts-entries.yaml` |
| `docs/` | `ARCHITECTURE.md`, `plans/`, inventories, research notes |
| `test/` | repository-wide Go tests (the auth chain, the bidder launcher and deploy) |
| `rust/` | socialweb-meshd |

TypeScript:

| Directory | What it holds |
| --- | --- |
| `deno.json` | the one workspace: every package below, the shared npm and jsr versions, the tasks |
| `deno.lock` | the one lockfile for the whole workspace |
| `socialweb-computer-k8s/` | the socialweb-computer-k8s web app: `hono-socialweb-computer-k8s/` (CLI), `lib/`, `web/`, `test/` |
| `ts/<repository>/` | the vendored code of one original repository, at its original path |

The vendored repositories are atproto-market, atproto-relay, deno-worker-sandbox, did-key-associator
(hono-backend only), did-key-ingress-proxy, hono-compute-provider, hono-pds, policy-engine,
socialweb-computer-ssh and typescript-helpers. Only the packages that the entries in
`deploy/ts-entries.yaml` reach came in, plus the tests of those packages and the files they read at run
time. TypeScript that is not in that set stays in its own repository (digitalocean-bidder,
deno-macos-runner-desktop, the did-key-associator frontend, open-architecture).

One package name exists once. The packages that socialweb-computer-ssh and socialweb-computer-k8s
shared have one copy, in `socialweb-computer-k8s/lib/`.

## The deno workspace and the lockfile

- Every package is a member of the root `deno.json` workspace. A package keeps its own `deno.json`
  (name, version, `exports: "./mod.ts"`, its own imports). No `deno.json` under `ts/` has a
  `workspace` field.
- Import another package as `jsr:@publicdomainrelay/<name>@^0`, through a bare `@publicdomainrelay/<name>`
  in the package's `imports`. The workspace resolves it to the member. Never import a relative path
  into another package.
- A new package: add its directory to `workspace` in the root `deno.json`, in its layer group
  (common, abc, implementation, hono-factory, CLI).
- Change a dependency, then refresh the lock at the root (`deno install --node-modules-dir=none`) and
  commit `deno.lock` in the same commit. The release build of `cmd/socialweb-tsbundle` refuses a
  lockfile that the fill created or changed, and `deploy/check.sh` runs `deno install --frozen`.
- Run deno with `--node-modules-dir=none`, as `internal/denorun` does. The root tasks pass it.

## TypeScript rules

These rules come from the org-root CLAUDE.md. They apply to all TypeScript here.

- ABC layering. One concept has up to five packages, and the path tells the layer:
  `lib/common/<concept>-common` (types, constants, pure helpers, external imports only),
  `lib/abc/<concept>` (`-abc`: interfaces and pure state, no I/O),
  `lib/<concept>-<transport>` (the implementation: timers, crypto, fetch, sockets, `Deno.*`),
  `lib/hono-factory-<concept>-<transport>` (the Hono factory, composed and never subclassed),
  `hono-<concept>` (the thin CLI). Imports go one way: common, abc, implementation, factory, CLI.
- A new transport is a sibling package, never a flag or an if-branch in an existing one.
- No cross-concept imports. Shared code goes to a common package. Cross-repository helpers are in
  `ts/typescript-helpers/lib/`; look there before you write a new common package.
- One package, one `mod.ts` export. No sub-module exports. Never name a package bare `common`.
- CLI: one `new Command(...)` from `@publicdomainrelay/cli-args-env` with `cli-args-env.json` and an
  optional `config.json`. No `Deno.env.get()` in a CLI. No hard-coded port, host or path in a CLI.
- `createLogger` and `createServe` from typescript-helpers; no raw `Deno.serve()`. Read the bound port
  from `onListen`; never listen on port 0, read the port, close and listen again.
- Signal handlers only in a CLI, never in a library.
- No comments in code. ASCII only.

## The kcp paradigm, port 0, and the RFP flow

- kcp is part of each application, not infrastructure. An application instance is a kcp of its own (or,
  in a market network, a workspace of the network's one kcp), a manifest set and a profile. State is kcp
  objects. Components are DenoPods and HostProcesses reconciled from those objects. Only the virtual
  Kubernetes clusters project uses kcp in the traditional way.
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
  `user_data` (`buildUserData` and the module registry in `ts/atproto-market/lib/common/cloud-init-common`).
  The requester reaches the guest only through the relay. Never start a container or VM by hand in
  code or in a test, never install a guest agent outside cloud-init, never add a transport that the
  RFP cloud-init does not install. A test that needs a live guest is an integration test in
  `test/integration`: its own market network, and `socialweb-computer run request` (which keeps
  `runComputeContract`) or a tenant cluster of the network's gateway rents the guest.

## Tests: no tests

Write no tests, except integration tests, and write those only once the system works in prod.

- No tests. Do not write unit tests, ratchets, allow-lists, inventories, golden files, snapshots,
  source-grep checks, size lines or any other check of an arbitrary rule. Do not work test-first.
  Ignore the `test_first` field of a plan item.
- Do not repair an old test. If a change breaks an existing test, delete that test in the same commit.
- Prod means: the new gateway started from the `socialweb-computer` binary, and the local
  bobvmbuilder bidder on hat-8 running the new bidder from that same binary
  (`socialweb-computer run bidder`).
- Integration tests come only at the end: after a tenant k3s cluster on that gateway scales to zero
  and gets nodes as market RFP VMs from that bidder. Only integration tests get written then.
- A manual live run is proof, not a test.
- Integration tests are Go tests in `test/integration`, build tag `integration`, plan 0010 G2. Each
  test brings up its own market network (`dev up`, a unique instance, port 0, VM slots of the VM
  account under its slot lock), drives it with the `socialweb-computer` verbs, ends at once on a
  failed network, child application, ComputeVM or NodeClaim, and runs `dev down` and checks that
  nothing is left, also on failure. Run them as an account that may `sudo -n -u test-0001`:
  `go test -tags integration -timeout 40m -v ./test/integration/`. `SOCIALWEB_IT_ACCOUNT` (the VM
  account), `SOCIALWEB_IT_GUEST_IMAGE` (default `file:///var/lib/socialweb-computer/guest-image`),
  `SOCIALWEB_IT_PUBLISH`, `SOCIALWEB_IT_SKIP_BUILD`, `SOCIALWEB_IT_RUN_ROOT` (default `/var/tmp/swcit`;
  keep it short, a tenant cluster's front socket path must fit in 107 bytes) and
  `SOCIALWEB_IT_ARTIFACTS` (the logs, default `~/.tmp/swc-it/<run>`) change the defaults. Each market
  network takes a host live slot first through `SOCIALWEB_IT_LIVE_SLOT` (default the lead's
  `~/.tmp/swc-lead/live-slot.ts` when it exists; `none` turns it off).

## Prod deployments

A prod deployment does not preserve anything.

- Losing sessions, VMs, clusters, claims, receipts or any other prod state during a deploy is
  acceptable. Do not plan migration, carry-over or rollback for them. Deploy fresh.
- The atproto credentials of the two prod accounts are on the deploy host, owner-only (mode 0600):
  `~/.aliceoa_bsky_social_source` (requester account alice; exports `ALICE_ATPROTO_HANDLE`,
  `ALICE_ATPROTO_PASSWORD`) and `~/.bobvmbuilder_bsky_social_source` (bidder account bobvmbuilder;
  exports `BOB_ATPROTO_HANDLE`, `BOB_ATPROTO_PASSWORD`). When a deploy loses a session, sign in again
  from these files. Source them into the one command that needs them; never print, log, commit or
  copy their values, and never put them in a kcp object, a DenoPod argument or a plan.
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
  `--url https://k8s.socialweb.computer/`.
- A bidder's guest image comes from `--guest-image-source` (a `file://` or `https://` base that
  `install bidder` writes to config.json), or, when that is unset, from the published artifacts:
  `run bidder` reads `releases/<version>` and then `main/<commit>` under
  `--guest-image-release-base`, the public Spaces bucket by default. On hat-8 the pinned image is in
  `/var/lib/socialweb-computer/guest-image` (root-owned; the bidder verifies every file by the sha256
  pinned in internal/versions/versions.json).
- Only the lead runs a prod deployment (plan 0006 M5-FRESH-DEPLOY). Task agents never touch prod.

## How to verify

- While iterating, build only what you touched: `go build ./internal/<pkg>/... ./cmd/socialweb-computer`
  and `deno check <the entry you changed>` (`deploy/ts-entries.yaml`). Do not run the full gate per
  change.
- The full gate runs ONCE, right before "ready": `go build ./...`, `go vet ./...`, `deno task check`,
  `deploy/check.sh --fast` and the release closure. GitHub CI runs the same gate on every push to
  `main` and `task/*` (`.github/workflows/ci.yml`); read the branch's run on GitHub.
  - `deploy/check.sh --fast`: gofmt, go build, apigen generate and its diff (generated code must
    match its source), go vet, `deno task check`, `deno install --frozen`, the cross-OS builds and the
    release builds. `--local` and `--full` run the live proofs this host can run.
  - `deno task check` type-checks every entry of `deploy/ts-entries.yaml` from the root workspace.
  - The release closure: `go run ./cmd/socialweb-tsbundle -release -deno ~/.deno/bin/deno -out <dir>`.
    It needs a committed, clean tree and a current `deno.lock`, and it starts every entry offline.
- The binary for live runs: `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/<work>`.
  One build of the working tree (uncommitted changes included), host platform, warm caches: about
  8 s after a one-line Go change or a one-line TypeScript change. It embeds every asset, so a live run
  needs no `SOCIALWEB_*_PATH` and no `--source-root`. `socialweb-computer version` says "dev build,
  not reproducible" with the commit and the dirty state. The binary runs the TypeScript live from
  `<publish>/src` (the mirror of the working tree that test-0001 can read; without `--publish`, the
  checkout itself). Output: `~/.tmp/socialweb-computer-build/dev/<worktree>/`. The first `--dev`
  build on a host stages every asset once (about 3.5 minutes); later builds stage only an asset whose
  `versions.json` entry changed.
- A TypeScript change in a running market network: `scripts/build-socialweb-computer.sh --dev
  --publish /var/tmp/<work> --sync-only` (mirrors the tree, about 1 s), then
  `socialweb-computer dev restart <instance> [<app>/][<workspace>:]<denopod>` (as the account that
  runs the network). It restarts that one DenoPod through its kcp object, waits until it is Running
  with its listeners reported, and prints the old and new pid and run directory (`stdout.txt` and
  `stderr.txt` are the pod's log). Then rerun `run request`. Do not `dev down`/`dev up` for a
  TypeScript change, and do not rebuild the binary for one. The PLC directory and the PDSes keep
  their state in memory: do not `dev restart` plc or a PDS (it empties them); `dev down`/`dev up` instead.
- The release binary (prod only; the lead builds it): `scripts/build-socialweb-computer.sh` without
  `--dev` builds one commit once per linux release platform from a clean checkout (every asset
  staged and checked against its pin, the closure, `-tags release`). Each step is keyed by its
  inputs and does not run again when they did not change: asset blobs by their pinned inputs (assets
  built here by their tar bytes), socialweb-meshd by rust/ and the toolchain, the closure by its
  TypeScript trees and the last commit that touched them, plus the go build cache and the cargo
  target dir. About 7 s when nothing changed and about 5 minutes with empty caches. Every phase prints
  a `timing` line. `--verify-reproducible` (0006 ACC6-REPRO; the `release-reproducible` CI job
  on main) builds the commit twice from two clean checkouts and publishes a platform only when both
  are byte-identical. `--no-cache` builds both from scratch, which proves the caches give the bytes
  of a clean build. Checkouts, caches and outputs stay under
  `~/.tmp/socialweb-computer-build`; `release/<commit>/` holds the binaries and `SHA256SUMS` that
  `install` and `upgrade --binary --sha256` take. Never build it in the inner loop.

## Where and how to work

- Work in a plain git worktree or branch of this repository. Do not put a worktree inside another
  worktree. The org-root `scripts/task-tree.ts` is for the polyrepo work that is left, not for this
  repository.
- Commit small logical changes and push after every commit. On `main`, push to origin main as work
  lands.
- Never use `/tmp`; it is tmpfs and spills to swap. Use `~/.tmp` (set `TMPDIR` under it).
- A one-off script is a `.ts` file under `$TMPDIR`, run with `deno run -A`, using `Deno.Command` for git
  and go. Keep each shell call to one plain command.
- Plans live in `docs/plans`. Flip an item's status only. Do not add evidence lists; the git history is
  the evidence.
- The original repositories in the org root are frozen. Never edit them for a path under `ts/`.
- Rebase on `origin/main` often (`git fetch && git rebase origin/main`). When a rebase stops on
  conflicts, run `scripts/regen-after-rebase.sh`: it takes one side of every conflicted generated
  file (the apigen outputs: schemas and exports under `deploy/`, the TypeScript wire types and
  fixtures; and `deno.lock`), reruns `go generate ./api/...` (and `deno install`), stages them and
  prints the hand-written conflicts left. Never hand-merge generated output. Resolve a conflict in
  `api/v1alpha1` first; the script says so. `deploy/apiresourceschema-revisions.txt` merges itself
  (`merge=union`); when two branches changed one schema under one revision, the script keeps the
  upstream line and the generator asks for a Revision bump.
- Live runs: your own market network only, run as the VM account (test-0001) from the binary and the
  mirror that `--dev --publish /var/tmp/<work>` wrote, with an instance name and bidder handle unique to
  your work. Keep the network up for the whole item; clean up only your own units and VMs.

## The fast loop for one item

- Compile and type-check: `go build ./cmd/socialweb-computer` (about 4 s in a new worktree, the Go caches
  are shared) and `deno check <entry>` or `deno task check` (about 4 s).
- Binary and mirror: `scripts/build-socialweb-computer.sh --dev --publish /var/tmp/<work>` (about 21 s the
  first time in a worktree, about 6.5 s after). Later TypeScript changes: `--sync-only`, then `dev restart`.
- One network for the whole item: `dev up` once (80 to 95 s, mostly waiting, about one core), then every
  proof is a `run request --network <instance>` (80 to 90 s each, 35 s of it the requester's own kcp).
  `dev down` costs 20 s. Do not `dev up` per proof.
- A held guest burns one host core for as long as it lives (an idle firecracker vCPU does not halt), so
  release a `--hold` invocation as soon as you have read it.
- Take a live slot before `dev up` and release it after `dev down`:
  `deno run -A ~/.tmp/swc-lead/live-slot.ts acquire|beat|release <name>`. The default is 5 slots
  (`SWC_LIVE_SLOTS`), and `acquire` also waits while MemAvailable is under 12000 MB
  (`SWC_LIVE_MIN_AVAIL_MB`) or cpu pressure avg10 is over 90 (`SWC_LIVE_MAX_CPU_PSI`).
- More live capacity: `scripts/dev-droplet.sh` makes a DigitalOcean droplet that has /dev/kvm (tag
  `dev-work`, at most 8, deleted with `down`): `create`, `setup`, `push <publish-dir>`, then
  `swc -- <args>` or `it <TestRegex> <publish-dir>` run as test-0001 there. A droplet needs no live slot.
  Measurements: `docs/ARCHITECTURE.md` section 15, "Live capacity and iteration time".
- The shared Linux dev VM `dev-linux-1` (c-48, 48 vCPU, 96 GB, build host and live host; Ubuntu 24.04; run
  all of this from the Mac or any host with doctl and the Johns-Air key; it exists while the lead keeps it).
  Use your own `<work>` name for everything (it names your worktree, publish dir and instance):
  1. Push your branch to GitHub, then `scripts/dev-droplet.sh sync dev-linux-1 origin/<branch> <work>`
     (a git worktree `~/w/<work>` of a clone of the public repository, owned by user `dev`).
  2. `scripts/dev-droplet.sh build dev-linux-1 <work>` writes the binary and the mirror to `/var/tmp/<work>`
     (`go build` and `deno task check` run there too: `scripts/dev-droplet.sh ssh dev-linux-1 "runuser -l dev -c 'cd w/<work> && <cmd>'"`).
  3. Live run, as test-0001: `export SOCIALWEB_PUBLISH_DIR=/var/tmp/<work>`, then `scripts/dev-droplet.sh swc dev-linux-1 -- dev up
     --instance <work> --profile market-network --bidders 1 --vm-host-backend systemd-user --state-dir /var/tmp/swcit/<work>
     --guest-image-source https://socialweb-computer-kcp.sfo3.digitaloceanspaces.com/main/latest/guest-image/firecracker-x86_64`
     (the binary fetches and verifies the pinned image itself; `main/latest` is the build that matches the pin).
  4. `scripts/dev-droplet.sh swc dev-linux-1 -- run request --network <work> --state-dir /var/tmp/swcit/<work> --exec '<cmd>'`
     per proof; `scripts/dev-droplet.sh ssh dev-linux-1 '<cmd>'` runs any command as root.
  5. End with `dev down --instance <work> --state-dir /var/tmp/swcit/<work>`. Never run `scripts/dev-droplet.sh down`
     (it deletes every dev-work droplet). Start parallel runs a few seconds apart; six at once fit.

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

## Standing constraints

- No comments in code except the `ponytail:` form naming a ceiling and its upgrade path, and
  `//go:` directives.
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
| the door | the socialweb-computer-k8s web app, `socialweb-computer-k8s/hono-socialweb-computer-k8s` |
| the ssh door | `ts/socialweb-computer-ssh/hono-socialweb-computer-ssh` |
| the core | the `socialweb-computer` binary process that runs kcp and supervises the DenoPods and HostProcesses |
| a fold, folded | one pass that merged a set of decisions into the plans |
| the spine | the RFP flow: requester, RFP, bid, accept, cloud-init `user_data` |
| the front | the stable entry point in front of a service: the kcp front before the kcp slots (0006 M2-KCP-SLOTS), the TLS front of the market network |
| the edge | caddy and the other stable public entry points that keep a known port |
| a stand-in | a small test executable used in place of k3s, kine or postgres |
| the soak | the last run against the real accounts (the bobvmbuilder bidder and alice), after the market network passes |
| a gate | a check that must pass before an item or a merge goes on (`deploy/check.sh`, a plan's done_when) |
| the seed | the first example that a model grows from |
