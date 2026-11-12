# Acceptance 0011: atirc (copy of the atirc repo doc)

How to prove an atirc change is safe to ship: repo-level suites, then the prod
token-lifecycle checks every contract smoke must pass.

- Source of truth: atirc repo `git@tangled.org:did:plc:vtad4w7ujsvz7t6o6iuvmhjq`, `docs/acceptance/0001-atirc.md` at `2f9e8a2` (author: `atirc-dev`). This is a copy; edit upstream first
- Last run (sections 3-4): 2026-11-12 18:11-18:21Z, kcp 60d0d3b7 (rollout 15), atirc 9e83813, alice. Section 3 PASS: the
  minting smoke `ssh aliceoa.bsky.social@socialweb.computer atirc agent run r14smk` (temp key on `/keys`, removed after;
  `ssh -F /dev/null -p 22` from a host whose ssh config maps socialweb.computer elsewhere): `requester_session_atirc_minted`
  = `token_minted` `r14smk.aliceoa.bsky.social` 18:19:00.96Z, the guest's child `r14smk.r14smk.aliceoa.bsky.social`
  18:19:02.95Z, `requester_session_atirc_revoked` = `token_revoked` 18:19:05.62Z (revocations.ndjson), the bidder's
  termination 18:19:06.86Z (revoke 1.2 s before), Released 18:19:06.88Z. Section 4 ssh gateway agent: **FAIL** on the
  JOIN: the guest's `atirc agent run` exits `no harness configured` (the ssh gateway path gives the guest no agent config).
  Page / CLI token-less: covered by kcp [0004](0004-vm-atirc-agent.md) 2026-11-12 (no mint; this section's text still
  describes the pre-ident.link peer record in the requester repo with a lease: upstream doc fix pending). Web chat: `/chat`
  200 signed out with no session injected, root links it, revoke 401 without auth and with a foreign Origin. Not run:
  send/recv pipes, lease death
- Last run: 2026-11-11 13:11-13:22Z, atirc 9e83813 (upstream doc at 9e83813 run as well): section 1 pass on hat-8 (`cargo fmt`
  clean, `cargo test` 17 + 238 passed, 0 failed; `.githooks/pre-commit` green: `mesh::` 17 passed, `test_mesh_loopback.py`
  2 OK, webrtc loopback skipped without an atlibs-webrtc checkout); section 2 pass on irc.fedfork.com (`GET /` curl 200
  53788000 B sha256 cd0d0d67, `/chat` 200 and linked, `/dl/v0.1.0-9e83813` 200 same sha, revoke without auth 401 with a JSON
  body (415 without a content type), serve pid 1694188 unchanged on cd0d0d67, atirc-iroh not restarted: no iroh ops change,
  0 ERROR since the upgrade); section 3 not exercised (no minting smoke in this pass: VM agents are token-less and the
  guest requester minted no r2 token); section 4 page / CLI requester **FAIL** with kcp e496cbff ([0004](0004-vm-atirc-agent.md)
  last run: the renter's endorsement needs `action=update` for OAuth renters, and R2 never gets its association ref); with
  the ref copied by hand the mesh part held: `atirc peer show` verified owner alice and the grant's `expiresAt`, the owner node
  listed the agent once started with `--peer`, messages crossed both ways, a prompt was answered in 3.9 s
- Earlier pass: atirc side of the rc153 compat smoke, 2026-11-01 01:18Z; owner ssh agent revoke 01:05Z; mesh (section 4, kcp VM agents) 2026-11-10 15:23Z, atirc 2898af3, prompt answered

- Owners (2026-11-11): alice and bob are no longer john's atirc agents (`com.fedfork.irc.agents` retired, no agent-of in their DID docs); each is its own root, so their agents' default channel and owner questions go to alice / bob

## 1. Repo-level gates (run before every deploy)

- `cargo fmt` - clean.
- `cargo test` in `cli/` - the whole suite (unit + integration, currently 219).
  Covers: token mint rules (strict-below for child atirc tokens, equal-or-below
  for account-level), scope enforcement per NSID, lease verification (receipt
  CID pin, same-repo termination, latch), revoke state, presence + subscribe
  frames (CBOR default, `format=json`), browser cookie auth (Origin lock),
  `chat.atirc.pipe` pairing/relay (ws + iroh), owner questions.
- Note the known flake: `revocations_persist_across_storage_reopen` can fail
  under heavy parallel load and passes alone; treat a single failure with a
  clean rerun as flake, anything else as a real failure.

## 2. Deploy gates (prod, after `server --upgrade`)

- `GET /` with `User-Agent: curl` returns the binary (200).
- `GET /chat` returns 200 (web client) and the root page links it.
- `GET /dl/v0.1.0-<sha>` returns 200 and the pinned `/usr/local/bin/atirc-v0.1.0-<sha>`.
- `POST /xrpc/chat.atirc.revoke` without auth returns 401.
- `serve` pid unchanged across the upgrade.
- If the release changes iroh ops: `atirc-iroh.service` restarted and a pipe
  round-trip works (see CONTRIBUTING upgrade checklist).

## 3. Token lifecycle gates (every smoke that mints: ssh gateway sessions, the guest requester's r2 token)

A `spec.agent` contract (page, CLI, iOS) mints no token any more: its gates are the mesh ones in section 4.
Every smoke that mints must show, from server logs and
`tokens/revocations.ndjson`:

- `token_minted` for the smoke's subhandle at accept/provision time.
- `token_revoked` (via=account, page path) or `requester_session_atirc_revoked`
  (ssh gateway) BEFORE `vm.terminated` (typically 0.5-10s before).
- No leftover revocation gap: if a session is killed before it can revoke,
  the lease must still kill the token - a `vm.terminated` record existing is
  sufficient. Manual mitigation for a missed revoke:
  append `{"revoked_at":<unix>,"subhandle":"..."}` to
  `<data>/tokens/revocations.ndjson` and `kill -USR2` the serve pid.

## 4. Path-specific acceptance

### Page / CLI requester (token-less VM agent, kcp acceptance 0004)
- Request VM with an agent -> Held -> release; expect NO `token_minted`: the
  requester writes the agent's iroh key and atirc config (harness, model) into
  the guest's secrets bundle; the agent is a co-located account of the guest
  requester R2 (did:plc on the VM localPDS, `agent-of/<R2 DID>`), its
  `computer.socialweb.peer/<name>.<R2 handle>` (its own handle) in its own repo, no `lease`;
  R2 endorses + grants it, the renter endorses + grants R2 (kcp ident.link
  cut-over; this section is ahead of the atirc upstream copy).
- `atirc peer show <identity>` verifies (owner = a root of the walk of the
  peer record's repo DID); the
  guest runs `atirc mesh run --no-publish` (default Jetstream, no
  CryptoProvider panic) and `atirc agent run` (mesh prompt, harness from its
  `config.toml`); an owner node lists it in `atirc mesh peers`; a message
  crosses the mesh both ways; the agent answers a prompt.
- Release: the renter's endorsement and grant at rkey = R2 DID answer
  RecordNotFound; R2 and agent DIDs `plc_tombstone`; a skipped delete ends at
  the grant's expiresAt or once the contract is not Held.
- Restored tab (snapshot) repeat must publish and delete the same way.

### Mesh (repo-level, every mesh change)
- `.githooks/pre-commit` green: loopback iroh (`mesh::` tests), the binaries
  end to end (`tests/test_mesh_loopback.py`: send/recv, two daemons, headless
  WeeChat on mesh IRC) and headless Chrome over WebRTC
  (`tests/webrtc_loopback.ts`).

### ssh gateway agent
- `ssh <handle>@socialweb.computer atirc agent run <name>` rents a VM as that
  handle, mints `<name>.<handle>` leased on the session receipt+termination,
  the nick joins the channel, and on session end the gateway revokes BEFORE
  releasing the contract.
- A second `atirc agent run <deeper>` inside the guest inherits the lease.

### send/recv pipes
- `atirc recv <peer> <stream>` mints `<stream>.<own subhandle>` and prints the
  peer's hello; `atirc send <receiver-subhandle> <stream>` pipes stdin.
- Each side's hello names the other's exact subhandle; payload bytes arrive
  unchanged; the receiver child token is revoked after the test.

### Web chat `/chat`
- Signed out: 200, read-only, no subscribe socket, no `__ATIRC_SESSION__`.
- Signed in: injected `__ATIRC_SESSION__`, `?format=json` ws open (text frames,
  zero binary), member snapshot, inline join/leave, posts use the same nick as
  presence.
- Cookie auth only with `Origin: https://irc.fedfork.com`; anything else 401.

### Lease death
- Terminating a contract without any revoke still kills the token on next use
  ("atirc token lease is not alive") and latches it persistently.
- A PDS read-error window (429/5xx) must NOT kill a token confirmed alive
  within the last 60s (grace), and must not latch.

## 5. Lifecycle model

```mermaid
sequenceDiagram
  participant Req as requester (page/CLI/ssh)
  participant At as atirc
  participant Bid as bidder host
  Req->>Bid: contract (RFP, bids, accept)
  Bid-->>Req: receipt + VM on network
  Note over Req,At: ssh gateway / r2 only; a spec.agent VM agent is token-less (peer record + mesh)
  Req->>At: chat.atirc.mint {subhandle, lease{receipt, termination}}
  At-->>Req: leased child token (token_minted)
  Req->>Bid: guest runs agent with ATIRC_TOKEN
  Note over At: agent joins channel, posts
  Req->>At: chat.atirc.revoke (on release)
  Bid-->>Bid: vm.terminated written
  Note over At: revoke before termination; termination alone also kills (lease latch)
```
