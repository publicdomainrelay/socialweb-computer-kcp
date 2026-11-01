# Acceptance runbooks

Runnable end-to-end checks against prod. Each doc: purpose, preconditions, steps,
expected observations, pass criteria, cleanup, pitfalls. They change prod state
(VMs, contracts, tokens): run only with the lead's go, announce on atirc first,
release everything you rent.

| id | name | what it proves | hosts | how long | last pass |
| --- | --- | --- | --- | --- | --- |
| [0001](0001-auth-modes.md) | k8s: account modes, pause, one VM per host | localRepo + oauthQr bidders, per-component suspend, tenant k3s 0 -> 3 -> 0 nodes, one VM per host, typed settings, ACC34 | gateway, local, hat-9, hat-7 | ~45 min | rc154, 2026-11-01 (all 9 sections) |
| [0002](0002-ssh-gateway.md) | ssh gateway, per-user OAuth | `ssh <handle>@socialweb.computer atirc agent run <name>` rents as the user, agent joins, revoke before release on disconnect / cap, restart survival | gateway (ssh gw), any bidder | ~3 min + session | rc152, 2026-11-01 (owner, port 22) |
| [0003](0003-cli-webrtc-ssh.md) | CLI ssh over WebRTC (ACC34) | CLI requester rents per host, ssh over WebRTC fresh + restarted kcp, no plaintext signaling | local, hat-9, hat-7 | ~3 min / host | rc154, 2026-11-01 (exit 0, 2.2-4.9 s) |
| [0004](0004-vm-atirc-agent.md) | atirc agent in a VM (plan 0035) | leased mint at RFP, `guest.agentStart` starts the unit, nick joins, revoke before `vm.terminated` | any bidder, atirc | ~3 min | rc135 page prod / rc132 CLI, 2026-10-31 |
| [0005](0005-guest-requester-association.md) | guest requester R2, association (plan 0036) | contract-scoped association lets R2 rent a nested VM, grandchild agent, outer release cascades, all tokens revoked, record deleted | 2 bidders, atirc | ~10 min | rc128 cascade, 2026-10-30 |
| [0006](0006-rollout-compat.md) | rc rollout checks + compat smoke | every component on rcN, Ready / SignedIn / 0 ERROR / host empty, unchanged page + CLI still work | all | ~5-15 min | rc154, 2026-11-01 |
| [0007](0007-bidder-termination.md) | bidder-side VM termination (ops) | operator delete ends an orphan cleanly: released, `vm.terminated` pinned, leased tokens dead, host empty | one bidder | ~1 min | 2026-10-31 (hat-7) |
| [0008](0008-host-observer.md) | host observer verification | per-host watcher protocol: CONFIRMED after rollout, VM timeline, `vm.terminated` pin, orphan check | each bidder | continuous | rc154, 2026-11-01 (hat-7) |
| [0009](0009-browser-page.md) | wasm browser page | A1-A7: suite, boot + sign-in, VM lifecycle, Held-reload restore, upgrade load, compat smoke, `/chat` | browser, bidders | ~5 min / test | page-rc141 vs rc153, 2026-11-01 |
| [0010](0010-ios-app.md) | iOS app, embedded kcp | A1-A6: market sign-in + persistence, launch / agentStart / join, revoke before terminated, cross-restart release, presence, perf | simulator / device, bidders | ~5 min / test | simulator, 2026-10-31 |
| [0011](0011-atirc.md) | atirc | repo + deploy gates, token lifecycle gates for every path, pipes, `/chat`, lease death | irc.fedfork.com | ~10 min | 2026-11-01 (atirc side of compat smoke) |

0011 is a copy; the source is the atirc repo (`git@tangled.org:did:plc:vtad4w7ujsvz7t6o6iuvmhjq`,
`docs/acceptance/0001-atirc.md` @ `ec0d088`).

## Shared rules

- Announce before, post done after: `<nick> -> all: <test> now; hold VM tests` / `... done; tests may resume`
- One driver per account at a time (alice, hat, bob); owner-held VMs are never touched
- Policies: `bidder-tangled-vouch` on every prod bidder, `requester-tangled-vouch` on requesters; never `open` unless the owner sets it
- Evidence comes from three sides: requester log, host watcher ([0008](0008-host-observer.md)), atirc-dev (`revocations.ndjson`)
- Token order: revoke strictly before `vm.terminated`; the lease kills it anyway once termination exists
- No secrets anywhere: no tokens, passwords, cookies, sign-in URLs, creds file contents; creds files stay mode 0600 and are read only inside the one signing command
- A workaround is reported as one; the run is re-done without it before the item counts as passed

## Which runbook for which change

| Change | Run |
| --- | --- |
| any rc to prod | 0006, 0008 (watchers), then the runbooks of the changed area |
| bidder market / policy code | 0003 on all hosts, 0006 compat, 0001 before a milestone |
| ssh gateway / per-user requester | 0002 (side port first, then 22) |
| requester mint / revoke / lease | 0004, 0011 section 3, 0009 A3 |
| association / guest requester | 0005 |
| page build | 0009 A1-A6 on dev, then A3 + A6 on prod |
| iOS app / pkg/embedded | 0010 |
| OAuth role scope | re-sign every OAuth identity first (0001 section 9 note, 0006), then 0003 hat-7 |
