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

## Later

- The suite against it: test steps that start requesters run them on the VM (`run requester --network stg`).
- Multi-VM (both-sides-NAT'd firehose test): layers placed through another kcp (`dev up --parent`), one meshd overlay
  (`computer.socialweb.mesh.roster`, TXT and wildcard names).
