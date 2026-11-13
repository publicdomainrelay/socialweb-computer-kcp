# Staging

A self-contained socialweb.computer on one rented VM, for one run of the suite, then torn down.

The VM runs `socialweb-computer dev up --profile market-network`: its own PLC, relay, PDSes (`alice.test`,
`bob.test`), 3 bidders (nested VMs), the gateway, vouches between them, a per-network CA in `network.caBundle`.
Every service is on the VM's loopback, so anything that uses it runs on the VM (`run requester --network stg`).

## Run

1. `run request --hold --nested-virt --cpus 8 --mem 16G --expires-in 2h` as the target's account.
2. Install the binary under test on the VM: `<bucket>/{releases/<tag>|main/<commit>}/binaries/socialweb-computer-linux-amd64`,
   checked against `SHA256SUMS`.
3. `dev up --instance stg --state-dir ~/stg/s --profile market-network --bidders 3 --with-gateway --compute-kinds vm,deno --host-backend proc`.
4. `dev status --instance stg`.
5. Always: `dev down --instance stg`; the host releases the VM.

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
