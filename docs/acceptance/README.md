# Acceptance runbooks

Runnable end-to-end checks against prod. Each doc: purpose, preconditions, steps,
expected observations, pass criteria, cleanup, pitfalls. They change prod state
(VMs, contracts, tokens): run only with the lead's go, announce on atirc first,
release everything you rent.

| id | name | what it proves | hosts | how long | last pass |
| --- | --- | --- | --- | --- | --- |
| [0001](0001-auth-modes.md) | k8s: account modes, pause, one VM per host | localRepo + oauthQr bidders, per-component suspend, tenant k3s 0 -> 3 -> 0 nodes, one VM per host, typed settings, ACC34 | gateway, local + a temporary oauthQr bidder (hat-8), hat-9, hat-7 | ~45 min | e496cbff, 2026-11-11 (all nine sections; alice + bob oauthQr; 4 bidders, ACC34 on all 4, no bidders list); c770a887, 2026-11-10 (alice + bob; 4 nodes on 4 bidders, ACC34 on all 4); gateway re-check 5e6c4e3d, 2026-11-11 (sections 3-6, 0 -> 3 -> 0 one step at a time on hat-7, hat-8, hat-9) |
| [0002](0002-ssh-gateway.md) | ssh gateway, per-user OAuth | `ssh <handle>@socialweb.computer atirc agent run <name>` rents as the user, agent joins, revoke before release on disconnect / cap, restart survival | gateway (ssh gw), any bidder | ~3 min + session | 2026-11-10 transient command, alice (5b8682fd); rc158 2026-11-01 agent run |
| [0003](0003-cli-webrtc-ssh.md) | CLI ssh over WebRTC (ACC34) | CLI requester rents per host over the bidder's offering endpoints, ssh over WebRTC fresh + restarted kcp, no plaintext signaling | local, hat-9, hat-7 | ~3 min / host | 2026-11-10 hat-9 + hat-7 (69aa8b0f), alice |
| [0004](0004-vm-atirc-agent.md) | atirc agent in a VM (plan 0035, p2p) | no mint; iroh key + agent config (harness, model) in the bundle; the agent a co-located did:plc of the in-VM R2 (ident.link pair + grant agent -> R2 -> renter, peer record in the agent's repo, no `lease`), `guest.agentStart` starts `atirc mesh run` + the harness, messages both ways, the agent answers, release deletes the renter's grant + endorsement and tombstones the DIDs | any bidder, an owner mesh node | ~5 min | FAIL e496cbff, 2026-11-11 (OAuth renter cannot endorse R2: putRecord needs `action=update`; R2 never gets its association ref without `r2-atirc.env`; 24 min release); 2026-11-10 (alice, rollout 5, no workarounds, prompt answered) |
| [0005](0005-guest-requester-association.md) | guest requester R2, association (plan 0036) | contract-scoped grant + `link.ident.endorsement` (rkey = R2 DID, after the nonce check) lets R2 rent a nested VM, grandchild agent, outer release cascades, all tokens revoked, records deleted; co-located accounts on the guest requester's PDS, `computer.socialweb.peer` records without `lease`, the identlink walk (agent-of, `maxDepth`), PLC tombstone on release | 2 bidders, atirc, relay.mini-cloud-0002, plc.directory | ~10 min | FAIL e496cbff, 2026-11-11 (association ref not mirrored; R2 `--agent` refused: R2 handle does not resolve); 2026-11-10 |
| [0006](0006-rollout-compat.md) | rc rollout checks + compat smoke | every component on rcN, Ready / SignedIn / 0 ERROR / host empty, unchanged page + CLI still work | all | ~5-15 min | rollout 8 (5e6c4e3d) every component, 2026-11-10 (component rows; compat smoke last on rollout 6) |
| [0007](0007-bidder-termination.md) | bidder-side VM termination (ops) | operator delete ends an orphan cleanly: released, `vm.terminated` pinned, leased tokens dead, host empty | one bidder | ~1 min | e496cbff, 2026-11-11 (hat-8, delete + expiry); rollout 5 438ca0fa, 2026-11-10 (hat-7) |
| [0008](0008-host-observer.md) | host observer verification | per-host watcher protocol: CONFIRMED after rollout, VM timeline, `vm.terminated` pin, orphan check | each bidder | continuous | rollout 5 438ca0fa, 2026-11-10 (hat-7, hat-9) |
| [0009](0009-browser-page.md) | browser requester page | A1-A3, A6, A7: unit, OAuth sign-in, VM lifecycle over the bidder's WebRTC endpoint (no https), compat, `/chat` | browser, bidders | ~5 min / test | 2026-11-10 prod page vs hat-9 (438ca0fa), alice |
| [0010](0010-ios-app.md) | iOS app, embedded kcp | A1-A6: market sign-in + persistence, launch / agentStart / join, revoke before terminated, cross-restart release, presence, perf | simulator / device, bidders | ~5 min / test | simulator, 2026-10-31 (not re-run on rc158/rc159) |
| [0011](0011-atirc.md) | atirc | repo + deploy gates, token lifecycle gates for the minting paths (ssh gateway, r2), token-less mesh gates for VM agents, pipes, `/chat`, lease death | irc.fedfork.com, an owner mesh node | ~10 min | 9e83813, 2026-11-11 (sections 1, 2; section 4 FAIL with kcp e496cbff, see 0004); 2026-11-01 (atirc v0.1.0-5a8dfa2); mesh 2026-11-10 (atirc 2898af3) |
| [0012](0012-infra-quickstart.md) | Infra quickstart | kubectl apply of every `examples/infra` file: VM ssh, tenant k8s node + pod, Deno sandbox stdout, agent VM refused before its RFP locally (`.local` handle; join on prod, 0013), release | own market network (+ prod for the agent join) | ~15 min | main ec97f78e local, 2026-11-01; needs a re-run on 2026-11-10 main |
| [0013](0013-mixed-prod.md) | Mixed prod on a user-configured bidder | `run requester` / `install requester` with a real account, `mixed-prod.yaml` pinned to one bidder under the default policies and both vouches: VM ssh, Deno stdout, agent join, restart keeps Held, delete revokes before `vm.terminated`; the gateway part waits on plan 0039 C | a user-configured bidder, irc.fedfork.com | ~20 min | rc159 (A, C) and rc160 CLI (B), 2026-11-01; needs a re-run on 2026-11-10 main |

0011 is a copy; the source is the atirc repo (`git@tangled.org:did:plc:vtad4w7ujsvz7t6o6iuvmhjq`,
`docs/acceptance/0001-atirc.md` @ `2f9e8a2`).

## Shared rules

- Announce before, post done after: `<nick> -> all: <test> now; hold VM tests` / `... done; tests may resume`
- One driver per account at a time (alice, bob, john); owner-held VMs are never touched
- Policies: `bidder-tangled-vouch` on every prod bidder, `requester-tangled-vouch` on requesters; never `open` unless the owner sets it
- Evidence comes from three sides: requester log, host watcher ([0008](0008-host-observer.md)), atirc-dev (`revocations.ndjson`); plus requester-repo getRecord at computed keys (RecordNotFound after release or termination)
- Token order: revoke strictly before `vm.terminated`; the lease kills it anyway once termination exists
- No secrets anywhere: no tokens, passwords, cookies, session secrets or creds file contents in any log, terminal or post; never paste a sign-in URL anywhere (the CLI's own log prints the short-lived QR sign-in URL by design); creds files stay mode 0600 and are read only inside the one signing command
- A workaround is reported as one; the run is re-done without it before the item counts as passed

## Which runbook for which change

| Change | Run |
| --- | --- |
| any rc to prod | 0006, 0008 (watchers), then the runbooks of the changed area |
| bidder market / policy code | 0003 on all hosts, 0006 compat, 0001 before a milestone |
| ssh gateway / per-user requester | 0002 (side port first, then 22) |
| requester mint / revoke / lease, agent peer records | 0004, 0005, 0011 section 3, 0009 A3 |
| offering endpoints (iroh / webrtc / https), signed market.event, requester record deletion | 0003 on every host (hat-9 is iroh and webrtc only), 0005, 0013 |
| association / guest requester | 0005 |
| page build | requester repo `scripts/deploy-page.sh`, then 0009 A1-A3 on prod |
| Infra / EphemeralInfra, examples, quickstart-local / quickstart-prod | 0012 |
| run requester / install requester, agent identity, bidder compute kinds or operator sign-in | 0013 |
| iOS app / pkg/embedded | 0010 |
| OAuth role scope | re-sign every OAuth identity first (0001 section 9 note, 0006), then 0003 hat-7 |
