# Staging

A self-contained socialweb.computer on one rented VM, for one run of the suite, then torn down. The smallest step of
the staging plan (CI VM, associated requester R2 renting staging VMs, `dev up --parent` onto them, one meshd overlay).

The VM runs `socialweb-computer dev up --profile market-network`: its own PLC, relay, PDSes (`alice.test`,
`bob.test`), 3 bidders (nested VMs), the gateway, vouches between them, a per-network CA in `network.caBundle`.
Every service is on the VM's loopback, so anything that uses it runs on the VM (`run requester --network stg`).

## Run

1. `run request --hold --nested-virt --cpus 16 --mem 32G --expires-in 2h` as the target's account (input `cpus`, `mem`).
2. As root on the VM: the account `stg` (group `kvm`, one subordinate id range above the image's, `uidmap`, linger), and
   the binary under test in `/home/stg/stg`: `<bucket>/{releases/<tag>|main/<commit>}/binaries/socialweb-computer-linux-amd64`,
   checked against `SHA256SUMS`. `dev` refuses root.
3. As `stg`, detached from the ssh session and polled (`/home/stg/up.rc`): `dev up --instance stg --state-dir /home/stg/stg/s
   --profile market-network --bidders 3 --with-gateway --compute-kinds vm,deno --host-backend proc`.
4. `dev status --instance stg --state-dir /home/stg/stg/s`.
5. Always: `dev down` (detached, polled); the host releases the VM.

The target's `requester` picks the binary: `release:<tag>` or `sha:<commit>` (a bare `release` is refused).

```sh
cd tests/modules && cargo wasm && cd ..
cd compositions/staging && deno run -A --no-lock compose.ts > composition.json && cd ../..
SWC_TARGET=$PWD/targets/prod.json cargo run -p swc-test-host -- compositions/staging --dry-run
```

Live (rents one VM; `requester` set to a published build):

```sh
jq '.requester = "sha:<commit>"' targets/prod.json > ~/.tmp/target-staging.json
SWC_TARGET=~/.tmp/target-staging.json cargo run -p swc-test-host -- compositions/staging
```

## Multi-VM (`--input '{"layout": "multi"}'`)

Four small VMs on one meshd overlay (10.44.0.1-4), joined by the atproto VM's network value:

| VM | size | runs |
| --- | --- | --- |
| atproto | 2/4 | `dev up --profile market-network --bidders 0 --network-bind 10.44.0.1` (PLC, relay, PDSes) |
| bidder-1, bidder-2 | 4/8, nested virt | `run bidder --market-network-file net.json --vmm qemu --bidder-policy open` |
| gateway | 4/8 | the overlay only, a PLC probe (its layer: later) |

1. Rent all four (`--nested-virt` only for the bidders), install each (above), put `socialweb-meshd` (`request.put` what
   `meshd`, the host's `SWC_MESHD`: a static x86_64 build of `rust/build.sh`).
2. `socialweb-meshd identity` on each, one roster file of the four endpoints, `socialweb-meshd run --roster` as root.
3. atproto: `dev up --network-bind`: the PLC, relay, PDSes, ingress and TLS front listen on 10.44.0.1, and
   `status.network` names that address. `dev status | jq .network.status` is the network file.
4. Bidders: `run bidder --market-network-file`; their MarketBidder `spec.identity.association: Skip` (no operator; a
   merge patch on the bidder kcp's admin token); a bidder forwards its `127.0.0.1:<port>` to each remote network host, so
   its guests' `relay.localhost` path (`192.0.2.1` -> the host's loopback) reaches the atproto VM.
5. Two `run request --network stg -- --policy open` on the atproto VM (the network vouches only for its own children;
   the joined bidders run `--bidder-policy open`), each on a nested bidder.
6. `dev down` on atproto; the host releases the four VMs.

```sh
SWC_MESHD=<static socialweb-meshd x86_64> SWC_TARGET=~/.tmp/target-staging.json \
  cargo run -p swc-test-host -- compositions/staging --input '{"layout": "multi"}'
```

## Later

- The suite against it: test steps that start requesters run them on the VM (`run requester --network stg`).
- Multi-VM: the suite from a requester VM, the gateway layer on the gateway VM, layers placed through another kcp
  (`dev up --parent`), the roster as a `computer.socialweb.mesh.roster` record with TXT and wildcard names.
