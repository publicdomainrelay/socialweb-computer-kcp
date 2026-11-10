# Acceptance 0013: Mixed prod (VM, Deno sandbox, agent VM on a user-configured bidder)

Proves [../../examples/infra/mixed-prod.yaml](../../examples/infra/mixed-prod.yaml) end to end against prod services: a
long-lived requester (`run requester`, then `install requester`) with a real atproto account, kubectl apply of one
EphemeralInfra pinned to a bidder the user configured, use each contract, restart the requester mid-run, delete. The
default policy runs on both sides; no file and no command names `open` ([../quickstart-prod.md](../quickstart-prod.md)
sections 2, 5 and 7).

- Paths: A `run requester` in the foreground (OAuth QR); B `install requester` (systemd user unit of its own account);
  C the gateway in the requester kcp (`run requester --with-gateway`, a tenant cluster)
- Plan: [0039](../plans/0039-long-lived-requester.yaml) items F-MIXED-PROD (this runbook) and ACC39-MIXED-PROD (its live pass)
- Last pass: 2026-11-01 on prod: A and C on rc159 (`ec0f4849`), B on the rc160 CLI (`840f0437`; on rc159 B failed its
  ssh step, Known Pitfalls). Prod readings below

```mermaid
sequenceDiagram
  participant U as user (kubectl, atirc)
  participant K as requester kcp (run requester)
  participant R as Requester app
  participant B as user-configured bidder
  participant I as irc.fedfork.com
  U->>K: apply vouch.yaml, mixed-prod.yaml
  K->>R: ComputeContract vm1, sbx1, agent1 (bidderDids pin)
  R->>B: RFP (requester-tangled-vouch attached)
  B->>B: bidder-tangled-vouch + attached policy: operator vouched requester?
  B->>R: bid
  R->>R: requester-tangled-vouch: requester vouched bidder?
  R->>B: accept
  B->>R: receipt, signed guest events at computed keys (onNetwork, hostKeys, vm.sandbox)
  R->>I: chat.atirc.mint (agent1, leased on receipt + vm.terminated)
  R->>K: ComputeContracts Held, EphemeralInfra Deployed
  U->>B: ssh vm1 (iroh, webrtc); read sbx1 stdout
  U->>K: agent-start ContractEvent -> guest.agentStart
  U->>I: JOIN of the agent nick, prompt, answer
  U->>K: Ctrl-C, restart: Held stays Held
  U->>K: delete EphemeralInfra -> revoke -> vm.delete -> vm.terminated
```

## Preconditions

| Item | Need |
| --- | --- |
| Bidder | a bidder the user configured: `install bidder` with the default policy (no `--bidder-policy`: `bidder-tangled-vouch`), [../bidder.md](../bidder.md); for sbx1 it must serve deno: `run bidder --compute-kinds vm,deno`, or `install bidder --compute-kinds vm,deno` (written to its `config.json`; on an already installed bidder, run the install again with the flag, which restarts the bidder unit); optional `--bidder-operator-signin <operator handle>` for the operator vouch of step A4. A `prod-qr` `install bidder` lists `https` in its offering `endpoints` only with the bidder option `market-https=true` (config.json `"bidder-options": "--market-https=true"`); without it the offering lists iroh and webrtc |
| Bidder DID | `MarketBidder app` `status.did` in the bidder kcp (the operator reads it); it replaces `REPLACE-BIDDER-DID` in every contract of `mixed-prod.yaml` |
| Requester account | a real atproto account (handle resolves to its DID, PDS public) signed in by OAuth QR; the creds file owner-only (mode 0600), read only by `scripts/oauth-signin.ts` |
| Vouches | both directions: requester -> bidder by [../../examples/infra/vouch.yaml](../../examples/infra/vouch.yaml) in the requester kcp (needs the optional scope `repo:sh.tangled.graph.vouch`) or `goat`; bidder operator -> requester by [../../examples/infra/vouch-bidder-operator.yaml](../../examples/infra/vouch-bidder-operator.yaml) in the bidder kcp (operator sign-in) or `goat` from the operator account |
| atirc | a token of your own (`atirc token <nick>`) to `atirc sub` the owner channel; never print it |
| Host | Linux, `kubectl`, `jq`, `ssh`, `dumbpipe` on PATH (the requester tries iroh first; a bidder without https, such as hat-9, is reached only over iroh or webrtc), the release or dev binary of the commit under test as `$SWC`; B: root on the requester host |
| Rules | [README.md](README.md) shared rules: announce on atirc first, one driver per account, release what you rent |

## Steps

`ST` is an owner-only state dir (`ST=~/.tmp/acc13; mkdir -p "$ST"; chmod 700 "$ST"`). Write each vouch at least 5
minutes before the first apply when a contract was denied before (verdicts are cached, denials too).

### A. run requester (foreground, OAuth QR)

1. Start: `$SWC run requester --name mp --state-dir "$ST" -- --atproto-oauth-qr > "$ST/mp.log" 2>&1 &`. Sign in from the
   `pendingAction.url` in the log with `scripts/oauth-signin.ts --creds <file> --account <ACCT> --url "<url>"` (read
   the url right before, never print or paste it).
2. Wait for `$ST/requesters/mp/ready.json` (written atomically once the root is Ready); `eval "$($SWC run requester env mp
   --state-dir "$ST")"` sets `KUBECONFIG` to the requester workspace; `kubectl get marketrequesters.socialweb.computer app
   -o jsonpath='{.status.phase} {.status.did}'` is `Ready <did>`; the ready file's `identityMode` is `oauthQr`.
3. Requester -> bidder vouch: `sed -e "s/REPLACE-REQUESTER-REPO/repo-$(jq -r .did "$ST/requesters/mp/ready.json" | tr : -)/"
   -e "s/REPLACE-BIDDER-DID/<bidder did>/g" -e "s/2026-11-01T00:00:00Z/$(date -u +%Y-%m-%dT%H:%M:%SZ)/"
   examples/infra/vouch.yaml | kubectl apply -f -`; AtprotoRecord `vouch-bidder` Applied with
   `at://<requester did>/sh.tangled.graph.vouch/<bidder did>`. When the session lacks the vouch scope, write the same
   record with `goat record create` from the requester account instead.
4. Bidder operator -> requester vouch: in the bidder kcp, `vouch-bidder-operator.yaml` with `REPLACE-REQUESTER-DID` (the
   operator sign-in of [../bidder.md](../bidder.md)), or `goat` from the operator account; check with `goat get
   "at://<operator did>/sh.tangled.graph.vouch/<requester did>" | jq -r .kind` -> `vouch`. When the write needs the
   owner's account, it is owner-only: ask the owner to write it; an agent does not.
5. Apply: `sed -e "s|ssh-ed25519 AAAAREPLACE-WITH-YOUR-PUBLIC-KEY you@laptop|$(cat "$(jq -r .sshPublicKey
   "$ST/requesters/mp/ready.json")")|" -e "s/REPLACE-BIDDER-DID/<bidder did>/" examples/infra/mixed-prod.yaml | kubectl
   apply -f -`; `kubectl wait ephemeralinfras.socialweb.computer/mixedprod --for=jsonpath='{.status.phase}'=Deployed
   --timeout=5m`.
6. vm1: `$SWC run requester ssh mp --state-dir "$ST" --contract einfra-mixedprod-vm1 --exec 'cat /root/mixedprod'` prints
   `mixedprod-vm-ok`; the same with `--transport webrtc --exec 'uname -m'` prints `x86_64`.
7. sbx1: `kubectl get computecontracts.socialweb.computer einfra-mixedprod-sbx1 -o jsonpath='{.status.contract.sandbox}'`
   has `stdout` `mixedprod-deno-ok sum=10 on linux/x86_64` and `exitCode` 0.
8. agent1: the requester log has `requester_agent_bundle_placeholder` (no bao step) and, after the receipt,
   `requester_atirc_minted`; then `sed -e 's/quickstart-agent-agent1-start/mixedprod-agent1-start/' -e
   's/einfra-quickstart-agent-agent1/einfra-mixedprod-agent1/' examples/infra/agent-start.yaml | kubectl apply -f -`; in
   your `atirc sub` the agent nick `<label>-<rfp rkey>` JOINs; `atirc pub '<nick>: uname -m'` gets `x86_64` back.
9. Restart mid-run: Ctrl-C (`kill -INT` the `run requester`): the ready file is removed, the kcp stops, the state stays;
   start the same command again (the same options before `--`, such as `--network`; nothing after `--` is needed): same DID, ready file rewritten, every contract still
   Held, step 6 ssh works again, the agent still answers.
10. Delete: `kubectl delete ephemeralinfras.socialweb.computer mixedprod`: each contract releases first; agent1
    `status.atircRevokes` is Done before the bidder's `vm.terminated`; the EphemeralInfra and its ComputeContracts are
    gone; the bidder host is empty ([0008](0008-host-observer.md) watcher: no VM unit, no sandbox); the requester log has
    `requester_contract_records_deleted`, and a getRecord of each contract record (accept, rfp, compute.vm,
    contract-scoped market.association, com.fedproxy.rbac) at its key answers RecordNotFound (read 3 times); the two
    vouch records are still in the repos (`deletionPolicy: Retain`; `goat get` both).
11. Stop: Ctrl-C the `run requester`.

### B. install requester (systemd user unit)

1. `sudo $SWC install requester --name mp --dry-run`, then `sudo $SWC install requester --name mp` (account
   `swc-requester`, identity `--atproto-oauth-qr` by default); the sign-in URL: `$SWC run requester signin mp --state-dir
   /var/lib/socialweb-computer/swc-requester` (as root or the account); sign in with `scripts/oauth-signin.ts`.
2. `R=/var/lib/socialweb-computer/swc-requester/requesters/mp/ready.json`; as the account, `kubectl --kubeconfig
   "$(jq -r .kubeconfig "$R")"` reads `MarketRequester app` Ready.
3. Steps A3 to A8 as the account, `$SWC run requester ssh mp --state-dir /var/lib/socialweb-computer/swc-requester`.
4. Restart mid-run: `systemctl --user restart socialweb-computer-requester@mp.service` as the account (and `upgrade` to
   the same binary): same DID, kcp and OpenBao units adopted (same InvocationID), contracts still Held, ssh again.
5. Step A10, then `sudo $SWC uninstall requester --account swc-requester` (keeps the state dir; `--purge` removes it).

### C. Gateway in the requester kcp (Cluster and k8s-cluster.yaml)

The gateway runs inside the requester's core (Gateway `spec.requesterRef`): its clusterprovider, k3s and kine are
HostProcesses of that core, and every node is a contract of the same requester. Needs a subuid/subgid range for the
user (`check-host` reports it).

1. `$SWC run requester --name mk --state-dir "$ST" --with-gateway -- --atproto-oauth-qr > "$ST/mk.log" 2>&1 &`; sign in
   as in A1; wait until the ready file names the gateway: `jq -r .gateway "$ST/requesters/mk/ready.json"` is not `null`.
2. `$SWC run requester cluster mk --cluster c1 --state-dir "$ST"`; then `TK=$($SWC run requester kubeconfig mk --cluster
   c1 --state-dir "$ST")` (retry until it prints a path: the Cluster is Ready).
3. `kubectl --kubeconfig "$TK" apply -f examples/infra/k8s-cluster.yaml`: a NodeClaim becomes an Infra in the requester
   kcp, its contract is bid and Held, the node VM fetches meshd by sha256 from the public bucket and joins k3s; poll
   until the `quickstart-k8s` pod is Ready (a `kubectl wait` before the pod exists returns no match); `kubectl logs
   deploy/quickstart-k8s` prints `quickstart-k8s-ok`.
4. Pin the node to a remote bidder by leaving only that bidder able to win (the run below suspended the local
   bidder: `kubectl patch marketbidders.socialweb.computer app --type merge -p '{"spec":{"suspend":true}}'` in its kcp,
   resumed after).
5. `kubectl --kubeconfig "$TK" delete deployment quickstart-k8s`: the node is reaped, its contract released, `kubectl
   get nodes` is empty (scale to zero); the bidder host is empty.
6. Ctrl-C the `run requester`.

## Expected Observations

| Event | Where | Typical |
| --- | --- | --- |
| ready file written, `identityMode` `oauthQr`, DID of the account | `requesters/mp/ready.json` | 11 s after the sign-in on an own network (quickstart-prod section 7) |
| `vouch-bidder` Applied, `at://<requester did>/sh.tangled.graph.vouch/<bidder did>`; operator record read back `vouch` | AtprotoRecord status, `goat get` | 1-4 s |
| RFP of each contract carries `requester-tangled-vouch`; the bidder's run allows (operator -> requester present) | bidder PolicyWorkflowRun, requester log | first run after the records |
| vm1 Deployed, ssh prints `mixedprod-vm-ok`, `x86_64` | EphemeralInfra `status.contracts[vm1]`, ssh | ~20 s after the apply (0012: 19 s) |
| sbx1 Held with ComputeVM backend `DenoSandbox`, `sandbox.stdout` `mixedprod-deno-ok sum=10 on linux/x86_64` | contract status | ~6 s (0012) |
| agent1: `requester_agent_bundle_placeholder`, `requester_service_auth` `atircSigning` `oauth`, `requester_atirc_minted`, nick JOIN after agent-start | requester log, atirc channel | Held ~20 s, JOIN within a minute of agent-start (0012 prod readings) |
| restart: same DID, ready file rewritten, contracts Held | ready file, ComputeContracts | ready again in seconds (quickstart-prod section 7: 1.4 s) |
| delete: `requester_atirc_revoked`, `status.atircRevokes` Done, then vm.delete, `vm.terminated`, contracts gone | requester log, bidder repo, kcp | under a minute |
| contract records deleted: `requester_contract_records_deleted`; accept, rfp, compute.vm, association, rbac RecordNotFound at their keys | requester log, getRecord in the requester repo | seconds after the release |
| bidder host empty, vouch records present | 0008 watcher, `goat get` | right after the delete |

## Prod Readings (2026-11-01, rc159 `ec0f4849`, account alice)

| time (Z) | path, step | reading |
| --- | --- | --- |
| 10:37:20 | A2 | ready file, `identityMode` `oauthQr`, alice's DID |
| 10:37:55 | A5 | mixed-prod Deployed in 35 s on the local bidder (`install bidder --compute-kinds vm,deno`): vm1, sbx1, agent1 Held |
| 10:38 | A6, A7 | vm1 ssh iroh and webrtc exit 0; sbx1 stdout `mixedprod-deno-ok sum=10 on linux/x86_64`, exit 0 |
| 10:38:31 | A8 | placeholder bundle, mint, agent JOIN, answered `x86_64` |
| 10:39 | A9 | restart: same DID, all Held, ssh again |
| 10:40:16.805 | A10 | agent1 revoked, then the terminations at 10:40:17; 0 contracts in 5 s; host empty |
| 11:19:31 | B3 (rc159) | `run requester ssh` as the account: did not answer within 1m (the ProxyCommand ran under the account's `nologin` shell) |
| 11:46:44 | B1, B2 (rc160) | `install requester` (account `swc-requester`, `socialweb.slice`) printed the journal and `run requester signin` hints; the URL from `run requester signin mp`; Ready |
| 11:47:14 | B3 | `vm.yaml` pinned to hat-7; ssh as the account: `quickstart-vm-ok`, `x86_64`, exit 0 |
| 11:47:15 | B4 | `systemctl --user restart`: same DID, contract Held, ssh `quickstart-vm-ok` again |
| 11:47:35 | B5 | delete: 0 contracts; `uninstall --purge` removed the account; hat-7 empty |
| 11:31:52 | C1 | `--with-gateway`, ready file names the gateway |
| 11:32:03 | C2 | cluster `c1` kubeconfig |
| 11:33:04 | C3, C4 | node `2wjeqtgx-c1-node1` on hat-7 (remote, local bidder suspended), k3s v1.36.4+k3s1, pod Ready, `quickstart-k8s-ok` |
| 11:35:41 | C5 | node reaped (hat-7 terminate 11:35:36, released 11:35:38), 0 nodes, hat-7 empty |

No plaintext token, key or sign-in URL in any log (check 0 on every file).

## Local Readings (2026-11-01, own market network `mxp`, main 6497fe61 dev build)

`dev up --bidders 1 --compute-kinds vm,deno` (default `bidder-tangled-vouch`, network-composed vouches), then
`run requester --name mp --network mxp` (localRepo, handle `root-mp.requester.local`), ready in 11.5 s.

| time (Z) | step | reading |
| --- | --- | --- |
| 08:38 | `kubectl apply --dry-run=server`, the file as committed and filled in | `ephemeralinfra.socialweb.computer/mixedprod created (server dry run)`, exit 0; no `policy` in any contract |
| 08:38:38 | apply, `bidderDids` = the network bidder | agent1 Failed at once, `refused: ... root-mp.requester.local is a .local handle ... --atproto-handle ...`, placeholder bundle written; vm1 and sbx1 Held on the pinned bidder (winner did) by 08:38:58 |
| 08:38:43 | sbx1 | `sandbox` `{"exitCode":0,"stdout":"mixedprod-deno-ok sum=10 on linux/x86_64\n"}` |
| 08:39:05 | `run requester ssh mp --contract einfra-mixedprod-vm1` | iroh: `mixedprod-vm-ok`, `x86_64` (0.87 s); webrtc: `x86_64` (0.68 s) |
| 08:39:20 | Ctrl-C | `requester_contract_left_for_restart` vm1, sbx1; kcp stopped, state kept |
| 08:39:30 | restart without `--network` | refused: "created with other options (network "mxp" ...)" (the network is part of the same command) |
| 08:39:39 | restart with `--network mxp` | ready in 1.6 s, same DID; 08:39:44 vm1 and sbx1 Failed `the held contract is not resumed: the policy run for the recorded winner ... the policy engine is not running`, then `infra_ephemeral_complete`, both released, the EphemeralInfra deleted |

On today's main a `run request release` of an invocation that Failed before a VM was held, or is Released, only removes
it; no second release is needed.

## Pass Criteria

- `kubectl apply --dry-run=server -f examples/infra/mixed-prod.yaml` passes against the requester (A and B)
- no file and no command names a policy: `requester-tangled-vouch` and `bidder-tangled-vouch` run, and the contracts are
  bid only after both vouch records exist
- every contract lands on the pinned bidder (`status.contract.winner.did` is the DID put in for `REPLACE-BIDDER-DID`)
- vm1 ssh over iroh and webrtc, sbx1 stdout, agent1 JOIN and answer, all on the same requester
- a Ctrl-C (A) or unit restart (B) mid-run keeps the DID and every Held contract Held
- delete: every agent token revoked before `vm.terminated` (0004 pass criteria), the bidder host empty, every contract
  record of the requester RecordNotFound at its key, both vouch records still present
- no token, password, key, cookie or sign-in URL on any terminal or in any log (plaintext check 0 on every log file)

## Cleanup

- `kubectl delete ephemeralinfras.socialweb.computer mixedprod` (it ends itself at `endWhen: Complete` or the 1 h ttl)
- A: Ctrl-C the `run requester`; remove `$ST` only when the DID is not needed again
- B: `sudo $SWC uninstall requester --account swc-requester` (`--purge` for a throwaway account)
- the vouch records stay: remove a vouch only from the account itself (`goat`), never by a kcp delete
- post done on atirc; stop your `atirc sub`

## Known Pitfalls

| Symptom | Cause / fix |
| --- | --- |
| after a restart every Held contract is Failed `the held contract is not resumed: ... the policy engine is not running` | main 6497fe61 and rc157: fixed in rc158 (resume waits for the policy engine) |
| B: no sign-in URL in `journalctl --user` of the account | the user units of a system account log to the system journal, which the account cannot read: `run requester signin mp --state-dir /var/lib/socialweb-computer/swc-requester` (rc160), or as root `journalctl _UID=<uid> _SYSTEMD_USER_UNIT=socialweb-computer-requester@mp.service` |
| B: `run requester ssh` as the account does not answer within 1m | rc159: ssh ran its ProxyCommand with the account's `nologin` shell; rc160 runs it under `/bin/sh` |
| C: `run requester cluster` says the requester runs no gateway | rc159: the ready file was first written before the gateway was Ready; rc160 writes it once, with `.gateway` |
| sbx1 `no bids received within 30s`, stays without a bid | the pinned bidder does not serve deno: `--compute-kinds vm,deno` on `run bidder`, or `install bidder --compute-kinds vm,deno` again for an installed bidder |
| every contract `no bids`; bidder `policy rejected ... not vouched` | the operator -> requester record is missing (step A4); denials are cached 5 minutes |
| agent1 Failed before its RFP, `refused: the atirc agent needs a requester handle that resolves to <did> ... --atproto-handle ...` | the requester handle is `.local` (a localRepo identity, the default without an identity option) or does not resolve; use the OAuth account, or a local PDS on plc.directory with `--atproto-handle <a domain you control>` ([../quickstart-prod.md](../quickstart-prod.md) section 5) |
| `vouch-bidder` (or the operator record) not Applied, the MarketIdentity has `pendingAction` `signInAgain` `ScopeUpgrade` | the QR page (qr.fedfork.com) granted a session without `repo:sh.tangled.graph.vouch`; the scope is optional, so the identity stays Ready: write the record with `goat`, or sign in again once the QR page asks for it |
| agent1 Failed `secrets need the requester ingress (getSecrets) or secretsFallback: Sealed` | `--comms-mode Firehose` has no inbound endpoint: add `--secrets-fallback Sealed` after `--`, or keep the default comms `Both` |
| a restart is refused | the options after `--` differ from `requester.json` of the first start: give the same ones or none, or start a new name |
| contracts land on another bidder | `REPLACE-BIDDER-DID` left in one contract or a wrong DID: every contract of the file carries its own `bidderDids` |
| Held never reached after the accept | the winner's offering `endpoints` hold nothing the requester can use: no `dumbpipe` on PATH for iroh, and no https endpoint |
| HTTP 400 on `market.event` at the bidder | the event is unsigned: the requester is an old build |
